package rod

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/projectdiscovery/rod/lib/proto"
)

func testMsg(session, method string) *Message {
	return &Message{SessionID: proto.TargetSessionID(session), Method: method, lock: &sync.Mutex{}}
}

func recvOne(t *testing.T, ch <-chan *Message) *Message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for message")
		return nil
	}
}

func expectNone(t *testing.T, ch <-chan *Message) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected message %s/%s", m.SessionID, m.Method)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRouterSessionAndMethod(t *testing.T) {
	r := newRouter(16)
	ctx := t.Context()

	all := r.subscribe(ctx, "", nil)
	load := r.subscribe(ctx, "", map[string]struct{}{"Page.loadEventFired": {}})
	s1 := r.subscribe(ctx, "s1", nil)
	s1load := r.subscribe(ctx, "s1", map[string]struct{}{"Page.loadEventFired": {}})

	r.publish(testMsg("s2", "Page.loadEventFired"))
	if m := recvOne(t, all); m.SessionID != "s2" {
		t.Fatal(m.SessionID)
	}
	if m := recvOne(t, load); m.SessionID != "s2" {
		t.Fatal(m.SessionID)
	}
	expectNone(t, s1)
	expectNone(t, s1load)

	r.publish(testMsg("s1", "Network.requestWillBeSent"))
	recvOne(t, all)
	if m := recvOne(t, s1); m.Method != "Network.requestWillBeSent" {
		t.Fatal(m.Method)
	}
	expectNone(t, load)
	expectNone(t, s1load)

	r.publish(testMsg("s1", "Page.loadEventFired"))
	recvOne(t, all)
	recvOne(t, load)
	recvOne(t, s1)
	recvOne(t, s1load)
}

func TestRouterOrder(t *testing.T) {
	r := newRouter(-1)
	ctx := t.Context()
	ch := r.subscribe(ctx, "s", nil)
	for i := range 1000 {
		r.publish(testMsg("s", fmt.Sprintf("M.%d", i)))
	}
	for i := range 1000 {
		if m := recvOne(t, ch); m.Method != fmt.Sprintf("M.%d", i) {
			t.Fatalf("got %s at %d", m.Method, i)
		}
	}
}

func TestRouterStalledConsumerDoesNotBlock(t *testing.T) {
	r := newRouter(8)
	ctx := t.Context()

	_ = r.subscribe(ctx, "stalled", nil) // never read
	live := r.subscribe(ctx, "live", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10000 {
			r.publish(testMsg("stalled", "Network.dataReceived"))
			r.publish(testMsg("stalled", "Fetch.requestPaused"))
		}
		r.publish(testMsg("live", "Page.loadEventFired"))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on a stalled subscriber")
	}
	recvOne(t, live)
	if r.dropped.Load() == 0 {
		t.Fatal("expected non-critical drops")
	}
}

func TestRouterKeepsCritical(t *testing.T) {
	r := newRouter(4)
	ctx := t.Context()
	ch := r.subscribe(ctx, "s", nil)

	for range 100 {
		r.publish(testMsg("s", "Network.dataReceived"))
		r.publish(testMsg("s", "Fetch.requestPaused"))
	}

	// Give the pump time to park on the unread channel.
	time.Sleep(50 * time.Millisecond)

	critical, other := 0, 0
	for {
		select {
		case m := <-ch:
			if m.Method == "Fetch.requestPaused" {
				critical++
			} else {
				other++
			}
			continue
		case <-time.After(100 * time.Millisecond):
		}
		break
	}
	if critical != 100 {
		t.Fatalf("critical events lost: %d", critical)
	}
	if other > 5 {
		t.Fatalf("queue bound ignored: %d", other)
	}
}

func TestRouterUnsubscribe(t *testing.T) {
	before := runtime.NumGoroutine()
	r := newRouter(16)
	ctx, cancel := context.WithCancel(context.Background())
	chans := []<-chan *Message{
		r.subscribe(ctx, "", nil),
		r.subscribe(ctx, "", map[string]struct{}{"A.b": {}, "C.d": {}}),
		r.subscribe(ctx, "s", nil),
	}
	r.publish(testMsg("s", "A.b"))
	cancel()
	for _, ch := range chans {
		for range ch {
		}
	}

	r.mu.RLock()
	n := len(r.all) + len(r.byMethod) + len(r.bySession)
	r.mu.RUnlock()
	if n != 0 {
		t.Fatalf("router kept %d index entries", n)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > before {
		t.Fatalf("goroutines leaked: %d > %d", g, before)
	}
}

func BenchmarkRouterPublish(b *testing.B) {
	for _, pages := range []int{1, 50, 500} {
		b.Run(fmt.Sprintf("pages=%d", pages), func(b *testing.B) {
			r := newRouter(-1)
			ctx := b.Context()
			for i := range pages {
				ch := r.subscribe(ctx, proto.TargetSessionID(fmt.Sprint(i)), nil)
				go func() {
					for range ch {
					}
				}()
			}
			msg := testMsg("0", "Network.dataReceived")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.publish(msg)
			}
		})
	}
}
