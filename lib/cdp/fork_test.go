package cdp

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeWS struct {
	in       chan []byte
	out      chan []byte
	sendErr  error
	sendHook func()
	read     func() ([]byte, error)
}

func (f *fakeWS) Send(b []byte) error {
	if f.sendHook != nil {
		f.sendHook()
	}
	if f.sendErr != nil {
		return f.sendErr
	}
	select {
	case f.out <- append([]byte(nil), b...):
		return nil
	default:
		return io.ErrClosedPipe
	}
}

func (f *fakeWS) Read() ([]byte, error) {
	if f.read != nil {
		return f.read()
	}
	b, ok := <-f.in
	if !ok {
		return nil, io.EOF
	}
	return b, nil
}

func TestClientIgnoresBadJSON(t *testing.T) {
	f := &fakeWS{in: make(chan []byte, 4), out: make(chan []byte, 4)}
	c := New().Start(f)
	f.in <- []byte("not-json")
	f.in <- []byte(`{"method":"Network.requestWillBeSent","params":{"x":1}}`)
	select {
	case ev := <-c.Event():
		if ev.Method != "Network.requestWillBeSent" {
			t.Fatalf("method %s", ev.Method)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientCallContext(t *testing.T) {
	f := &fakeWS{in: make(chan []byte, 1), out: make(chan []byte, 1)}
	c := New().Start(f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Call(ctx, "", "Browser.getVersion", nil)
	if err == nil {
		t.Fatal("expected deadline")
	}
	_ = c.Close()
}

func TestClientCallDoneContextStillSends(t *testing.T) {
	f := &fakeWS{in: make(chan []byte, 1), out: make(chan []byte, 1)}
	c := New().Start(f)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Call(ctx, "", "Network.disable", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	select {
	case b := <-f.out:
		if !strings.Contains(string(b), `"Network.disable"`) {
			t.Fatalf("sent %s", b)
		}
	case <-time.After(time.Second):
		t.Fatal("call with a done context was not sent")
	}
}

type gateWS struct {
	out     chan []byte
	in      chan []byte
	started chan struct{}
	release chan struct{}
}

func (g *gateWS) Send(b []byte) error {
	g.started <- struct{}{}
	<-g.release
	g.out <- append([]byte(nil), b...)
	return nil
}

func (g *gateWS) Read() ([]byte, error) {
	b, ok := <-g.in
	if !ok {
		return nil, io.EOF
	}
	return b, nil
}

func TestCallReturnsWhileWriteBlocks(t *testing.T) {
	g := &gateWS{
		out:     make(chan []byte, 1),
		in:      make(chan []byte),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	c := New().Start(g)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, "", "Network.disable", nil)
		errc <- err
	}()
	select {
	case <-g.started:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("call blocked on the socket write")
	}
	close(g.release)
	select {
	case b := <-g.out:
		if !strings.Contains(string(b), `"Network.disable"`) {
			t.Fatalf("sent %s", b)
		}
	case <-time.After(time.Second):
		t.Fatal("frame was not written after the call returned")
	}
}

func TestPipeRoundTrip(t *testing.T) {
	pair, err := NewPipePair()
	if err != nil {
		t.Fatal(err)
	}
	defer pair.ChromeRead.Close()
	defer pair.ChromeWrite.Close()
	p := NewPipe(pair.ParentRead, pair.ParentWrite)
	go func() {
		buf := make([]byte, 64)
		n, _ := pair.ChromeRead.Read(buf)
		_, _ = pair.ChromeWrite.Write(append(buf[:n], 0))
	}()
	msg := []byte(`{"id":1}`)
	if err := p.Send(msg); err != nil {
		t.Fatal(err)
	}
	got, err := p.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Fatalf("got %s", got)
	}
}

func TestWebSocketRejectsHugeLength(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ws := &WebSocket{conn: client, r: bufio.NewReader(client), closed: make(chan struct{}), MaxMessageSize: 32}
	go func() {
		// FIN+text, 64-bit length with the high bit set.
		_, _ = server.Write([]byte{0x81, 127, 0x80, 0, 0, 0, 0, 0, 0, 1})
	}()
	_, err := ws.Read()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestClientConnError(t *testing.T) {
	f := &fakeWS{in: make(chan []byte), out: make(chan []byte, 4)}
	c := New().Start(f)

	errc := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "", "Browser.getVersion", nil)
		errc <- err
	}()
	<-f.out
	close(f.in)

	select {
	case err := <-errc:
		if !errors.Is(err, ErrConnClosed) || !errors.Is(err, io.EOF) {
			t.Fatalf("pending call: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call not failed")
	}

	<-c.Done()
	if !errors.Is(c.Err(), io.EOF) {
		t.Fatalf("cause %v", c.Err())
	}

	_, err := c.Call(context.Background(), "", "Browser.getVersion", nil)
	if !errors.Is(err, ErrConnClosed) {
		t.Fatalf("later call: %v", err)
	}
}

func TestClientCloseFailsPending(t *testing.T) {
	f := &fakeWS{in: make(chan []byte), out: make(chan []byte, 4)}
	c := New().Start(f)
	errc := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "", "Browser.getVersion", nil)
		errc <- err
	}()
	<-f.out
	_ = c.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrConnClosed) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call not failed")
	}
	close(f.in)
}

func TestClientPendingCleanup(t *testing.T) {
	f := &fakeWS{in: make(chan []byte, 1), out: make(chan []byte, 1)}
	c := New().Start(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 100; i++ {
		_, _ = c.Call(ctx, "", "X.y", nil)
		select {
		case <-f.out:
		default:
		}
	}
	n := 0
	for i := range c.pending {
		c.pending[i].mu.Lock()
		n += len(c.pending[i].m)
		c.pending[i].mu.Unlock()
	}
	if n != 0 {
		t.Fatalf("pending leaked: %d", n)
	}
	_ = c.Close()
	close(f.in)
}

func TestWebSocketBasicAuthFromURL(t *testing.T) {
	got := make(chan string, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer s.Close()

	u := strings.Replace(s.URL, "http://", "ws://user:pa%3Ass@", 1)
	err := (&WebSocket{}).Connect(context.Background(), u, nil)
	var bad *BadHandshakeError
	if !errors.As(err, &bad) {
		t.Fatalf("got %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pa:ss"))
	if h := <-got; h != want {
		t.Fatalf("authorization %q", h)
	}
}

func TestWebSocketKeepAliveDetectsDeadPeer(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ws := &WebSocket{conn: client, r: bufio.NewReader(client), closed: make(chan struct{}), KeepAlive: 30 * time.Millisecond}
	go func() {
		// Swallow pings without ever answering.
		_, _ = io.Copy(io.Discard, server)
	}()
	go ws.keepAlive(client, ws.closed)

	start := time.Now()
	_, err := ws.Read()
	if err == nil {
		t.Fatal("expected deadline error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestPipeCloseUnblocksRead(t *testing.T) {
	pair, err := NewPipePair()
	if err != nil {
		t.Fatal(err)
	}
	defer pair.ChromeRead.Close()
	defer pair.ChromeWrite.Close()
	p := NewPipe(pair.ParentRead, pair.ParentWrite)

	errc := make(chan error, 1)
	go func() {
		_, err := p.Read()
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_ = p.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a pending Read")
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrPipeClosed) {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read not unblocked")
	}
	if err := p.Send([]byte("x")); !errors.Is(err, ErrPipeClosed) {
		t.Fatalf("send: %v", err)
	}
}
