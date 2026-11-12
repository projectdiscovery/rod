package rod

import (
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

func countStates(b *Browser) int {
	n := 0
	b.states.Range(func(_, _ interface{}) bool {
		n++
		return true
	})
	return n
}

func TestClosedPagesLeaveNoStates(t *testing.T) {
	b := New().MustConnect()
	defer b.MustClose()

	base := countStates(b)
	for i := 0; i < 50; i++ {
		p := b.MustPage("about:blank").MustWaitLoad()
		p.MustEval(`() => 1`)
		p.MustSetViewport(800, 600, 1, false)
		p.EnableDomain(proto.NetworkEnable{})
		if countStates(b) == base {
			t.Fatal("page created no states, the test checks nothing")
		}
		p.MustClose()
	}

	deadline := time.Now().Add(5 * time.Second)
	for countStates(b) > base {
		if time.Now().After(deadline) {
			t.Fatalf("%d states left after closing every page, want %d", countStates(b), base)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
