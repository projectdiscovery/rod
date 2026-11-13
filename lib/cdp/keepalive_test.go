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
	err := ws.Connect(context.Background(), u, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := cdp.New().Start(ws)
	defer func() {
		_, _ = c.Call(context.Background(), "", "Browser.close", nil)
	}()

	time.Sleep(time.Second)

	err = c.Err()
	if err != nil {
		t.Fatalf("idle connection dropped: %v", err)
	}
	_, err = c.Call(context.Background(), "", "Browser.getVersion", nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestShortDeadlineKeepsConnection(t *testing.T) {
	u := launcher.New().MustLaunch()
	c := cdp.MustStartWithURL(context.Background(), u, nil)
	defer func() {
		_, _ = c.Call(context.Background(), "", "Browser.close", nil)
	}()

	for i := range 50 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i)*time.Microsecond)
		_, _ = c.Call(ctx, "", "Browser.getVersion", nil)
		cancel()
	}
	err := c.Err()
	if err != nil {
		t.Fatalf("short deadlines closed the connection: %v", err)
	}
	_, err = c.Call(context.Background(), "", "Browser.getVersion", nil)
	if err != nil {
		t.Fatal(err)
	}
}
