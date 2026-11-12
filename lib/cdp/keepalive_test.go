package cdp_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher"
)

func TestKeepAliveChrome(t *testing.T) {
	u := launcher.New().MustLaunch()
	ws := &cdp.WebSocket{KeepAlive: 100 * time.Millisecond}
	if err := ws.Connect(context.Background(), u, nil); err != nil {
		t.Fatal(err)
	}
	c := cdp.New().Start(ws)
	defer func() {
		_, _ = c.Call(context.Background(), "", "Browser.close", nil)
	}()

	time.Sleep(time.Second)

	if err := c.Err(); err != nil {
		t.Fatalf("idle connection dropped: %v", err)
	}
	if _, err := c.Call(context.Background(), "", "Browser.getVersion", nil); err != nil {
		t.Fatal(err)
	}
}

func TestShortDeadlineKeepsConnection(t *testing.T) {
	u := launcher.New().MustLaunch()
	c := cdp.MustStartWithURL(context.Background(), u, nil)
	defer func() {
		_, _ = c.Call(context.Background(), "", "Browser.close", nil)
	}()

	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i)*time.Microsecond)
		_, _ = c.Call(ctx, "", "Browser.getVersion", nil)
		cancel()
	}
	if err := c.Err(); err != nil {
		t.Fatalf("short deadlines closed the connection: %v", err)
	}
	if _, err := c.Call(context.Background(), "", "Browser.getVersion", nil); err != nil {
		t.Fatal(err)
	}
}
