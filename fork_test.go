package rod

import (
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/projectdiscovery/rod/lib/launcher/flags"
	"github.com/projectdiscovery/rod/lib/proto"
)

func TestMessageLoadBadJSON(t *testing.T) {
	msg := &Message{
		Method: "Network.requestWillBeSent",
		lock:   &sync.Mutex{},
		data:   json.RawMessage(`{"timestamp":"nope"}`),
	}
	ev := &proto.NetworkRequestWillBeSent{}
	if msg.Load(ev) {
		t.Fatal("schema mismatch must not succeed or panic")
	}
}

func TestMessageLoadByValueDoesNotDecode(t *testing.T) {
	msg := &Message{
		Method: "Target.targetDestroyed",
		lock:   &sync.Mutex{},
		data:   json.RawMessage(`{"targetId":"abc"}`),
	}
	ev := proto.TargetTargetDestroyed{}
	if !msg.Load(ev) {
		t.Fatal("non-pointer still reports a type match")
	}
	if ev.TargetID != "" {
		t.Fatal("value receiver must not be filled")
	}
	if !msg.Load(&ev) {
		t.Fatal("pointer load failed")
	}
	if ev.TargetID != "abc" {
		t.Fatalf("target %s", ev.TargetID)
	}
}

func TestDOMDiff(t *testing.T) {
	cases := []struct {
		xs, ys []string
		want   float64
	}{
		{nil, nil, 0},
		{[]string{"a"}, nil, 1},
		{[]string{"a", "b"}, []string{"a", "b"}, 0},
		{[]string{"a", "b"}, []string{"b", "a"}, 0},
		{[]string{"a"}, []string{"a", "a"}, 0.5},
		{[]string{"a", "b", "c", "d"}, []string{"a", "b", "c", "x"}, 0.25},
	}
	for _, c := range cases {
		if got := domDiff(c.xs, c.ys); got != c.want {
			t.Fatalf("domDiff(%v, %v) = %v, want %v", c.xs, c.ys, got, c.want)
		}
	}
}

func TestKeepStateSkipsFulfillBody(t *testing.T) {
	if keepState("Fetch.fulfillRequest") {
		t.Fatal("fulfill bodies must not be retained")
	}
	if !keepState("Network.enable") {
		t.Fatal("domain enable must be retained")
	}
}

func TestCloseReapsLaunchedBrowser(t *testing.T) {
	b := New()
	err := b.Connect()
	if err != nil {
		t.Fatal(err)
	}
	dir := b.launcher.Get(flags.UserDataDir)
	_, err = os.Stat(dir)
	if err != nil {
		t.Fatalf("profile dir missing: %v", err)
	}
	err = b.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(dir)
	if !os.IsNotExist(err) {
		t.Fatalf("profile dir left behind: %v", err)
	}
}
