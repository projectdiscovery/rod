package cdp

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// A peer that accepts the TCP connection but never answers the upgrade must
// not hang Connect when the caller has no deadline.
func TestConnectBoundsSilentHandshake(t *testing.T) {
	old := DefaultHandshakeTimeout
	DefaultHandshakeTimeout = 300 * time.Millisecond
	defer func() { DefaultHandshakeTimeout = old }()

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	defer func() { _ = l.Close() }()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		<-release
	}()

	ws := &WebSocket{}
	done := make(chan error, 1)
	go func() {
		done <- ws.Connect(context.Background(), "ws://"+l.Addr().String()+"/devtools/browser", http.Header{})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected handshake error from silent peer")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Connect hung on a silent handshake")
	}
}
