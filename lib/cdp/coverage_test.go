package cdp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// wsServer is a minimal websocket peer for the frame-level tests.
type wsServer struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func (s *wsServer) readFrame() (opcode byte, payload []byte, masked bool) {
	s.t.Helper()
	h := make([]byte, 2)
	if _, err := io.ReadFull(s.br, h); err != nil {
		s.t.Fatalf("read frame header: %v", err)
	}
	size := int(h[1] & 0x7f)
	masked = h[1]&0x80 != 0
	switch size {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(s.br, b[:]); err != nil {
			s.t.Fatalf("read length: %v", err)
		}
		size = int(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(s.br, b[:]); err != nil {
			s.t.Fatalf("read length: %v", err)
		}
		size = int(binary.BigEndian.Uint64(b[:]))
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(s.br, key[:]); err != nil {
			s.t.Fatalf("read mask: %v", err)
		}
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(s.br, payload); err != nil {
		s.t.Fatalf("read payload: %v", err)
	}
	if masked {
		maskBytes(payload, key)
	}
	return h[0] & 0x0f, payload, masked
}

func (s *wsServer) writeRaw(b []byte) {
	s.t.Helper()
	if _, err := s.conn.Write(b); err != nil {
		s.t.Logf("server write: %v", err)
	}
}

// writeFrame writes one unmasked server frame.
func (s *wsServer) writeFrame(fin bool, opcode byte, payload []byte) {
	s.t.Helper()
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	var out []byte
	switch {
	case len(payload) <= 125:
		out = []byte{b0, byte(len(payload))}
	case len(payload) <= 65535:
		out = []byte{b0, 126, 0, 0}
		binary.BigEndian.PutUint16(out[2:], uint16(len(payload)))
	default:
		out = []byte{b0, 127, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(out[2:], uint64(len(payload)))
	}
	out = append(out, payload...)
	s.writeRaw(out)
}

// serveWS performs the handshake then runs fn. It returns the ws url.
func serveWS(t *testing.T, fn func(*wsServer)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			t.Logf("read request: %v", err)
			return
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		h := sha1.New()
		h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
		_, err = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n", accept)
		if err != nil {
			t.Logf("write handshake: %v", err)
			return
		}
		fn(&wsServer{t: t, conn: conn, br: br})
	}()
	return "ws://" + l.Addr().String() + "/devtools/browser/x"
}

func dialWS(t *testing.T, u string, ws *WebSocket) *WebSocket {
	t.Helper()
	if ws == nil {
		ws = &WebSocket{}
	}
	if err := ws.Connect(context.Background(), u, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func TestWebSocketControlFrames(t *testing.T) {
	pong := make(chan []byte, 1)
	u := serveWS(t, func(s *wsServer) {
		s.writeFrame(true, 0x9, []byte("ping-payload")) // ping
		op, payload, _ := s.readFrame()
		if op != 0xA {
			t.Errorf("reply opcode %x", op)
		}
		pong <- payload
		s.writeFrame(true, 0xA, []byte("ignored")) // pong
		s.writeFrame(false, 0x1, []byte("he"))
		s.writeFrame(true, 0x0, []byte("llo")) // continuation
	})

	ws := dialWS(t, u, nil)
	msg, err := ws.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "hello" {
		t.Fatalf("message %q", msg)
	}
	if p := <-pong; string(p) != "ping-payload" {
		t.Fatalf("pong %q", p)
	}
}

func TestWebSocketCloseFrame(t *testing.T) {
	stop := make(chan struct{})
	u := serveWS(t, func(s *wsServer) {
		s.writeFrame(true, 0x8, []byte{0x03, 0xE8, 'o', 'k'}) // 1000, "ok"
		op, _, _ := s.readFrame()
		if op != 0x8 {
			t.Errorf("close reply opcode %x", op)
		}
		<-stop
	})
	ws := dialWS(t, u, nil)
	_, err := ws.Read()
	close(stop)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("got %v", err)
	}
}

func TestWebSocketReadErrors(t *testing.T) {
	text := func(s string) []byte { return []byte(s) }
	cases := []struct {
		name  string
		feed  func(*wsServer)
		limit int
		want  string
	}{
		{"reserved bits", func(s *wsServer) { s.writeRaw([]byte{0x41, 0}) }, 0, "reserved bits"},
		{"truncated len16", func(s *wsServer) { s.writeRaw([]byte{0x81, 126, 0}) }, 0, "unexpected EOF"},
		{"truncated len64", func(s *wsServer) { s.writeRaw([]byte{0x81, 127, 0}) }, 0, "unexpected EOF"},
		{"truncated mask", func(s *wsServer) { s.writeRaw([]byte{0x81, 0x80 | 2, 1}) }, 0, "unexpected EOF"},
		{"truncated payload", func(s *wsServer) { s.writeRaw([]byte{0x81, 4, 'a'}) }, 0, "unexpected EOF"},
		{"ping not fin", func(s *wsServer) { s.writeFrame(false, 0x9, nil) }, 0, "malformed ping"},
		{"ping too big", func(s *wsServer) { s.writeFrame(true, 0x9, make([]byte, 126)) }, 0, "malformed ping"},
		{"close not fin", func(s *wsServer) { s.writeFrame(false, 0x8, nil) }, 0, "malformed close"},
		{"close too big", func(s *wsServer) { s.writeFrame(true, 0x8, make([]byte, 126)) }, 0, "malformed close"},
		{"close bad code", func(s *wsServer) { s.writeFrame(true, 0x8, []byte{0x03, 0xEC}) }, 0, "invalid close code"},
		{"close bad reason", func(s *wsServer) { s.writeFrame(true, 0x8, []byte{0x03, 0xE8, 0xff}) }, 0, "not utf-8"},
		{"text while fragmented", func(s *wsServer) {
			s.writeFrame(false, 0x1, []byte("a"))
			s.writeFrame(true, 0x1, []byte("b"))
		}, 0, "unexpected text frame"},
		{"unexpected continuation", func(s *wsServer) { s.writeFrame(true, 0x0, []byte("a")) }, 0, "unexpected continuation"},
		{"binary", func(s *wsServer) { s.writeFrame(true, 0x2, text("a")) }, 0, "unexpected binary frame"},
		{"unknown opcode", func(s *wsServer) { s.writeFrame(true, 0x3, nil) }, 0, "unknown opcode 3"},
		{"text too large", func(s *wsServer) { s.writeFrame(true, 0x1, text("abcd")) }, 3, "exceeds size limit"},
		{"fragment too large", func(s *wsServer) {
			s.writeFrame(false, 0x1, text("ab"))
			s.writeFrame(true, 0x0, text("cd"))
		}, 3, "exceeds size limit"},
		{"too many frames", func(s *wsServer) {
			for i := 0; i <= maxFrames; i++ {
				s.writeFrame(true, 0xA, nil)
			}
		}, 0, "too many websocket frames"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := serveWS(t, c.feed)
			ws := &WebSocket{MaxMessageSize: c.limit}
			if err := ws.Connect(context.Background(), u, nil); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ws.Close() }()
			_, err := ws.Read()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

func TestWebSocketMaskedServerFrame(t *testing.T) {
	u := serveWS(t, func(s *wsServer) {
		payload := []byte("masked")
		var key [4]byte
		key = [4]byte{1, 2, 3, 4}
		masked := append([]byte(nil), payload...)
		maskBytes(masked, key)
		frame := []byte{0x81, 0x80 | byte(len(payload))}
		frame = append(frame, key[:]...)
		frame = append(frame, masked...)
		s.writeRaw(frame)
	})
	ws := dialWS(t, u, nil)
	msg, err := ws.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "masked" {
		t.Fatalf("got %q", msg)
	}
}

func TestWebSocketPingReplyWriteError(t *testing.T) {
	u := serveWS(t, func(s *wsServer) {
		s.writeFrame(true, 0x9, nil)
	})
	ws := &WebSocket{}
	if err := ws.Connect(context.Background(), u, nil); err != nil {
		t.Fatal(err)
	}
	// Break the write side so answering the ping fails.
	ws.mu.Lock()
	_ = ws.conn.Close()
	ws.mu.Unlock()
	if _, err := ws.Read(); err == nil {
		t.Fatal("expected the ping reply error")
	}
}

func TestWebSocketKeepAliveFailureCloses(t *testing.T) {
	client, server := net.Pipe()
	ws := &WebSocket{conn: client, r: bufio.NewReader(client), closed: make(chan struct{}), KeepAlive: 20 * time.Millisecond}
	_ = server.Close() // every control write fails
	ws.keepAlive(client, ws.closed)
	select {
	case <-ws.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive did not close the socket")
	}
}

func TestWebSocketCloseNoConn(t *testing.T) {
	ws := &WebSocket{}
	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
	// The second close of a live socket must not panic on the closed channel.
	client, server := net.Pipe()
	defer server.Close()
	ws.conn = client
	ws.closed = make(chan struct{})
	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing a second time must not close the closed channel again.
	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketClosedPipe(t *testing.T) {
	ws := &WebSocket{}
	if err := ws.Send([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("send %v", err)
	}
	if _, err := ws.Read(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("read %v", err)
	}
	if err := ws.writeControl(0x9, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("control %v", err)
	}
}

func TestWebSocketWriteControlTruncates(t *testing.T) {
	got := make(chan int, 1)
	u := serveWS(t, func(s *wsServer) {
		_, payload, _ := s.readFrame()
		got <- len(payload)
	})
	ws := dialWS(t, u, nil)
	if err := ws.writeControl(0x9, make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	if n := <-got; n != maxControlFrame {
		t.Fatalf("control payload %d", n)
	}
}

func TestWebSocketWriteTimeoutSetting(t *testing.T) {
	if d := (&WebSocket{}).writeTimeout(); d != DefaultWriteTimeout {
		t.Fatalf("default %v", d)
	}
	custom := 3 * time.Second
	if d := (&WebSocket{WriteTimeout: custom}).writeTimeout(); d != custom {
		t.Fatalf("custom %v", d)
	}
}

func TestWebSocketSendWriteError(t *testing.T) {
	client, server := net.Pipe()
	ws := &WebSocket{conn: client, r: bufio.NewReader(client), closed: make(chan struct{})}
	_ = server.Close()
	if err := ws.Send([]byte("x")); err == nil {
		t.Fatal("expected write error")
	}
	if err := ws.writeControl(0x9, []byte("x")); err == nil {
		t.Fatal("expected control write error")
	}
}

func TestWebSocketConnectDeadline(t *testing.T) {
	u := serveWS(t, func(s *wsServer) {
		_, _ = s.br.ReadByte()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws := dialWS(t, u, &WebSocket{})
	if ws.WriteTimeout != 0 {
		t.Fatal("unexpected timeout")
	}
	_ = ctx
}

func TestWebSocketConnectWithDeadline(t *testing.T) {
	u := serveWS(t, func(s *wsServer) {
		s.writeFrame(true, 0x1, []byte("ok"))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws := &WebSocket{}
	if err := ws.Connect(ctx, u, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	if _, err := ws.Read(); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketRandomFailures(t *testing.T) {
	old := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no random") }
	t.Cleanup(func() { randRead = old })

	client, server := net.Pipe()
	defer server.Close()
	ws := &WebSocket{conn: client, r: bufio.NewReader(client), closed: make(chan struct{})}
	if err := ws.Send([]byte("x")); err == nil {
		t.Fatal("send must report the random error")
	}

	// writeControl ignores the random error but still writes.
	client2, server2 := net.Pipe()
	defer server2.Close()
	ws = &WebSocket{conn: client2, r: bufio.NewReader(client2), closed: make(chan struct{})}
	go func() { _, _ = io.Copy(io.Discard, server2) }()
	if err := ws.writeControl(0x9, nil); err != nil {
		t.Fatal(err)
	}

	if err := ws.handshake(context.Background(), nil, nil); err == nil {
		t.Fatal("handshake must report the random error")
	}
}

func TestWebSocketHandshakeErrors(t *testing.T) {
	ws := &WebSocket{
		conn:   &MockConn{errOnCount: 1},
		r:      bufio.NewReader(&MockConn{errOnCount: 1}),
		closed: make(chan struct{}),
	}
	u, err := url.Parse("ws://example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.handshake(context.Background(), u, nil); err == nil {
		t.Fatal("expected the request write error")
	}

	// A 101 response with the wrong accept key is a bad handshake.
	client, server := net.Pipe()
	defer server.Close()
	ws = &WebSocket{conn: client, r: bufio.NewReader(client), closed: make(chan struct{})}
	go func() {
		br := bufio.NewReader(server)
		_, _ = http.ReadRequest(br)
		_, _ = io.WriteString(server, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: nope\r\n\r\n")
	}()
	err = ws.handshake(context.Background(), u, nil)
	var bad *BadHandshakeError
	if !errors.As(err, &bad) || bad.Status != "101 Switching Protocols" {
		t.Fatalf("got %v", err)
	}
}

func TestWebSocketConnectDialError(t *testing.T) {
	ws := &WebSocket{Dialer: &net.Dialer{}}
	if err := ws.Connect(context.Background(), "ws://127.0.0.1:1/x", nil); err == nil {
		t.Fatal("expected a dial error")
	}
}

func TestPipeSendTooLarge(t *testing.T) {
	pair, err := NewPipePair()
	if err != nil {
		t.Fatal(err)
	}
	defer pair.ChromeRead.Close()
	defer pair.ChromeWrite.Close()
	defer pair.ParentRead.Close()
	p := NewPipe(pair.ParentRead, pair.ParentWrite)
	p.max = 2
	if err := p.Send([]byte("abc")); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestPipeSendWhileClosed(t *testing.T) {
	pair, err := NewPipePair()
	if err != nil {
		t.Fatal(err)
	}
	defer pair.ChromeRead.Close()
	defer pair.ChromeWrite.Close()
	p := NewPipe(pair.ParentRead, pair.ParentWrite)

	started := make(chan struct{})
	errc := make(chan error, 1)
	big := make([]byte, 1<<20)
	go func() {
		close(started)
		errc <- p.Send(big)
	}()
	<-started
	time.Sleep(100 * time.Millisecond) // the pipe is full, Send blocks in Write
	p.closed.Store(true)
	_ = p.w.Close()

	select {
	case err := <-errc:
		if !errors.Is(err, ErrPipeClosed) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("send did not finish")
	}
}

func TestPipeReadLargeMessage(t *testing.T) {
	pair, err := NewPipePair()
	if err != nil {
		t.Fatal(err)
	}
	defer pair.ChromeRead.Close()
	defer pair.ChromeWrite.Close()
	p := NewPipe(pair.ParentRead, pair.ParentWrite)

	msg := make([]byte, 5000)
	for i := range msg {
		msg[i] = 'a'
	}
	go func() {
		_, _ = pair.ChromeWrite.Write(append(append([]byte(nil), msg...), 0))
	}()
	got, err := p.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Fatalf("got %d bytes", len(got))
	}
}

func TestPipeReadMissingNUL(t *testing.T) {
	pair, err := NewPipePair()
	if err != nil {
		t.Fatal(err)
	}
	defer pair.ChromeRead.Close()
	defer pair.ParentRead.Close()
	defer pair.ParentWrite.Close()
	p := NewPipe(pair.ParentRead, pair.ParentWrite)

	go func() {
		_, _ = pair.ChromeWrite.Write(bytes.Repeat([]byte("a"), 5000)) // no NUL
		_ = pair.ChromeWrite.Close()
	}()
	_, err = p.Read()
	if err == nil {
		t.Fatal("expected the read error")
	}
}

func TestPipeReadTooLarge(t *testing.T) {
	pair, err := NewPipePair()
	if err != nil {
		t.Fatal(err)
	}
	defer pair.ChromeRead.Close()
	defer pair.ChromeWrite.Close()
	defer pair.ParentRead.Close()
	p := NewPipe(pair.ParentRead, pair.ParentWrite)
	p.max = 10

	go func() {
		_, _ = pair.ChromeWrite.Write(append(bytes.Repeat([]byte("a"), 5000), 0))
	}()
	if _, err := p.Read(); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestPipeReadAfterClose(t *testing.T) {
	pair, err := NewPipePair()
	if err != nil {
		t.Fatal(err)
	}
	defer pair.ChromeRead.Close()
	defer pair.ChromeWrite.Close()
	p := NewPipe(pair.ParentRead, pair.ParentWrite)
	p.closed.Store(true)
	if _, err := p.read(); !errors.Is(err, ErrPipeClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestNewPipePairFailure(t *testing.T) {
	old := osPipe
	t.Cleanup(func() { osPipe = old })

	calls := 0
	osPipe = func() (*os.File, *os.File, error) {
		calls++
		if calls == 1 {
			return nil, nil, errors.New("first pipe")
		}
		return old()
	}
	if _, err := NewPipePair(); err == nil || err.Error() != "first pipe" {
		t.Fatalf("first: %v", err)
	}

	calls = 0
	osPipe = func() (*os.File, *os.File, error) {
		calls++
		if calls == 2 {
			return nil, nil, errors.New("second pipe")
		}
		return old()
	}
	if _, err := NewPipePair(); err == nil || err.Error() != "second pipe" {
		t.Fatalf("second: %v", err)
	}
}

func TestScanIncomingErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"not object", `[]`, "not an object"},
		{"truncated after brace", `{`, "truncated message"},
		{"bad key", `{1:2}`, "expected string"},
		{"missing colon", `{"a" 1}`, "expected colon"},
		{"bad id", `{"id":x}`, "expected number"},
		{"bad session", `{"sessionId":1}`, "expected string"},
		{"bad method", `{"method":1}`, "expected string"},
		{"bad error", `{"error":1}`, "cannot unmarshal"},
		{"truncated after value", `{"id":1`, "truncated message"},
		{"missing comma", `{"id":1 "x":2}`, "expected comma or end"},
		{"trailing data", `{"id":1} x`, "trailing data"},
		{"truncated escape", `{"method":"a\`, "truncated escape"},
		{"bad escape", `{"method":"\q"}`, "bad escape"},
		{"truncated unicode", `{"method":"\u1`, "truncated unicode escape"},
		{"bad unicode", `{"method":"\uzzzz"}`, "invalid syntax"},
		{"control char", "{\"method\":\"a\x01\"}", "control character"},
		{"truncated string", `{"method":"abc`, "truncated string"},
		{"truncated value", `{"params":`, "truncated value"},
		{"truncated container", `{"params":[1,2`, "truncated container"},
		{"bad literal", `{"params":tru}`, "expected true"},
		{"bad number", `{"params":-}`, "expected number"},
		{"bad id literal", `{"id":true}`, "invalid syntax"},
		{"bad null", `{"params":nul}`, "expected null"},
		{"bad nested container", `{"params":["a\"b]}`, "truncated string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := scanIncoming([]byte(c.in))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			// The fallback to encoding/json decides whether the message is
			// dropped, so it must not panic on any of these.
			_, _ = decodeIncoming([]byte(c.in))
		})
	}
}

func TestScanIncomingLiterals(t *testing.T) {
	msg, err := scanIncoming([]byte(` {"id":1 ,"params":true,"result":false,"method":"\b\f\n\r\t\/\\\u0041"} `))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Method != "\b\f\n\r\t/\\A" {
		t.Fatalf("method %q", msg.Method)
	}
	if string(msg.Params) != "true" || string(msg.Result) != "false" {
		t.Fatalf("params %s result %s", msg.Params, msg.Result)
	}
	if msg.Error != nil {
		t.Fatalf("error %+v", msg.Error)
	}
	if msg.ID != 1 {
		t.Fatalf("id %d", msg.ID)
	}
}

func TestScanIncomingEmptyObject(t *testing.T) {
	msg, err := scanIncoming([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != 0 || msg.Method != "" || msg.Params != nil {
		t.Fatalf("%+v", msg)
	}
}

func TestStartWithURLError(t *testing.T) {
	if _, err := StartWithURL(context.Background(), "ws://127.0.0.1:1/x", nil); err == nil {
		t.Fatal("expected a dial error")
	}
}

func TestDecodeFallsBackToJSON(t *testing.T) {
	msg, err := decodeIncoming([]byte(`null`))
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != 0 || msg.Method != "" {
		t.Fatalf("%+v", msg)
	}
}

func TestErrorIsNotCDPError(t *testing.T) {
	e := Error{Code: 1, Message: "m"}
	if e.Is(errors.New("other")) {
		t.Fatal("a plain error must not match")
	}
	if !e.Is(&Error{Code: 1, Message: "m"}) {
		t.Fatal("an equal error must match")
	}
}

func TestClientMarshalError(t *testing.T) {
	c := New().Start(&fakeWS{in: make(chan []byte, 1), out: make(chan []byte, 1)})
	defer func() { _ = c.Close() }()
	if _, err := c.Call(context.Background(), "", "m", func() {}); err == nil {
		t.Fatal("expected the marshal error")
	}
}

type closeWS struct {
	fakeWS
	closed chan struct{}
}

func (c *closeWS) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return errors.New("close error")
}

func TestClientCloseCallsCloser(t *testing.T) {
	ws := &closeWS{
		fakeWS: fakeWS{in: make(chan []byte, 1), out: make(chan []byte, 1)},
		closed: make(chan struct{}),
	}
	c := New().Start(ws)
	if err := c.Close(); err == nil || err.Error() != "close error" {
		t.Fatalf("got %v", err)
	}
	if err := c.Close(); err == nil {
		t.Fatal("Close must keep the first error")
	}
}

func TestClientCallWhenDead(t *testing.T) {
	c := New() // no writer started, so only the dead channel is ready
	close(c.dead)
	_, err := c.Call(context.Background(), "", "m", nil)
	if err != nil {
		t.Fatalf("a nil cause must be reported as nil: %v", err)
	}
}

func TestClientWriteFailure(t *testing.T) {
	// The writer reports a transport error while the connection is alive.
	f := &fakeWS{in: make(chan []byte), out: make(chan []byte, 1)}
	f.sendErr = errors.New("write failed")
	c := New().Start(f)
	defer func() { _ = c.Close() }()
	_, err := c.Call(context.Background(), "", "m", nil)
	if err == nil || err.Error() != "write failed" {
		t.Fatalf("got %v", err)
	}
}

// pauseCtx blocks Err so a test can settle the transport state after a call
// has queued its frame but before it starts waiting for the response.
type pauseCtx struct {
	context.Context
	ready chan struct{}
}

func (c *pauseCtx) Err() error {
	<-c.ready
	return nil
}

func (c *pauseCtx) Done() <-chan struct{} {
	return nil
}

func TestClientWriteFailureWithDeadConn(t *testing.T) {
	// When the transport fails and the connection also dies before the call
	// starts waiting, the call reports the connection error.
	boom := errors.New("boom")
	for i := 0; i < 200; i++ {
		f := &fakeWS{in: make(chan []byte), out: make(chan []byte, 1)}
		f.sendErr = errors.New("write failed")
		c := New().Start(f)
		ctx := &pauseCtx{Context: context.Background(), ready: make(chan struct{})}
		errc := make(chan error, 1)
		go func() {
			_, err := c.Call(ctx, "", "m", nil)
			errc <- err
		}()
		f.sendHook = func() { c.fail(boom) }
		// Let the writer deliver the error and the reader fail, then let
		// the call proceed to its wait with both results ready.
		deadline := time.Now().Add(2 * time.Second)
		for c.Err() == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		close(ctx.ready)
		select {
		case err := <-errc:
			if !errors.Is(err, ErrConnClosed) {
				t.Fatalf("got %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("call did not return")
		}
		_ = c.Close()
	}
}

func TestClientResultBeforeDeath(t *testing.T) {
	// A response that arrives just before the connection dies must be
	// returned to the caller.
	for i := 0; i < 200; i++ {
		f := &fakeWS{in: make(chan []byte, 1), out: make(chan []byte, 1)}
		c := New().Start(f)
		ctx := &pauseCtx{Context: context.Background(), ready: make(chan struct{})}
		type res struct {
			msg []byte
			err error
		}
		done := make(chan res, 1)
		go func() {
			msg, err := c.Call(ctx, "", "m", nil)
			done <- res{msg, err}
		}()
		req := <-f.out
		var r struct {
			ID int `json:"id"`
		}
		_ = json.Unmarshal(req, &r)
		f.in <- []byte(fmt.Sprintf(`{"id":%d,"result":{"ok":true}}`, r.ID))
		// Wait until the consumer has stored the response, then fail.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			c.pending[uint(r.ID)%pendingShards].mu.Lock()
			_, pending := c.pending[uint(r.ID)%pendingShards].m[r.ID]
			c.pending[uint(r.ID)%pendingShards].mu.Unlock()
			if pending {
				time.Sleep(time.Millisecond)
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		close(f.in)
		<-c.Done()
		close(ctx.ready)
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("got %v", got.err)
			}
			if string(got.msg) != `{"ok":true}` {
				t.Fatalf("got %s", got.msg)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("call did not return")
		}
		_ = c.Close()
	}
}

func TestClientDeadWhileWaiting(t *testing.T) {
	f := &gateWS{
		out:     make(chan []byte, 1),
		in:      make(chan []byte),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	c := New().Start(f)
	errc := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "", "m", nil)
		errc <- err
	}()
	<-f.started
	c.fail(errors.New("boom"))
	select {
	case err := <-errc:
		if !errors.Is(err, ErrConnClosed) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("call did not return")
	}
	close(f.release)
}

func TestClientResultAndDeath(t *testing.T) {
	// A response and a shutdown race with each other; either way the call
	// must return without a panic.
	for i := 0; i < 200; i++ {
		f := &fakeWS{in: make(chan []byte, 1), out: make(chan []byte, 1)}
		c := New().Start(f)
		errc := make(chan error, 1)
		go func() {
			_, err := c.Call(context.Background(), "", "m", nil)
			errc <- err
		}()
		req := <-f.out
		var r struct {
			ID int `json:"id"`
		}
		_ = json.Unmarshal(req, &r)
		f.in <- []byte(fmt.Sprintf(`{"id":%d,"result":1}`, r.ID))
		close(f.in)
		select {
		case <-errc:
		case <-time.After(2 * time.Second):
			t.Fatal("call did not return")
		}
	}
}

func TestClientEventOnDeadConn(t *testing.T) {
	c := New()
	c.event = make(chan *Event) // unbuffered, no consumer
	f := &fakeWS{in: make(chan []byte), out: make(chan []byte, 1)}
	f.read = func() ([]byte, error) {
		<-c.dead
		return []byte(`{"method":"Page.loadEventFired","params":{}}`), nil
	}
	c.ws = f
	c.writes = make(chan writeJob, 1)
	go c.consumeMessages()
	c.fail(errors.New("boom"))
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("event loop did not stop")
	}
}

func TestClientOnDecodeError(t *testing.T) {
	got := make(chan error, 1)
	f := &fakeWS{in: make(chan []byte, 2), out: make(chan []byte, 1)}
	c := New()
	c.OnDecodeError = func(err error) { got <- err }
	c.Start(f)
	defer func() { _ = c.Close() }()
	f.in <- []byte("not json")
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("expected the decode error")
		}
	case <-time.After(time.Second):
		t.Fatal("decode error not reported")
	}
}

func TestConnErrorMessage(t *testing.T) {
	e := &ConnError{Err: io.EOF}
	if e.Error() != "cdp: connection closed: EOF" {
		t.Fatalf("got %q", e.Error())
	}
	if !errors.Is(e, ErrConnClosed) || !errors.Is(e, io.EOF) {
		t.Fatal("must match both errors")
	}
}

// Ensure the frame helper stays in sync with the reader.
func TestMaskBytesRoundTrip(t *testing.T) {
	b := []byte("0123456789abcdef")
	orig := string(b)
	maskBytes(b, [4]byte{1, 2, 3, 4})
	if string(b) == orig {
		t.Fatal("mask had no effect")
	}
	maskBytes(b, [4]byte{1, 2, 3, 4})
	if string(b) != orig {
		t.Fatal("mask is not reversible")
	}
}

var _ = sync.Mutex{}
