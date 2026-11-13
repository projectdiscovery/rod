package rod_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// getWithin fails the test if Get blocks past d. A slot that was never freed
// shows up here instead of as a hung test run.
func getWithin(t *testing.T, pp *rod.HealthPagePool) *rod.Page {
	t.Helper()
	d := 15 * time.Second
	type result struct {
		p   *rod.Page
		err error
	}
	ch := make(chan result, 1)
	go func() {
		p, err := pp.Get()
		ch <- result{p, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.p
	case <-time.After(d):
		t.Fatal("Get blocked: a slot was never freed")
		return nil
	}
}

func TestHealthPagePoolNeverHandsOutClosed(t *testing.T) {
	g := setup(t)

	pp := rod.NewHealthPagePool(g.browser, 1)
	defer pp.Cleanup()

	// A page closed while idle in the pool must not come back out.
	p1 := getWithin(t, pp)
	id1 := p1.TargetID
	pp.Put(p1)
	g.E(p1.Close())

	p2 := getWithin(t, pp)
	g.True(p2.TargetID != id1)
	_, err := p2.Info()
	g.E(err)

	// A page that dies while checked out is dropped on Put, and its slot
	// is free for the next Get.
	g.E(p2.Close())
	pp.Put(p2)

	p3 := getWithin(t, pp)
	g.True(p3.TargetID != id1 && p3.TargetID != p2.TargetID)
	_, err = p3.Info()
	g.E(err)
	pp.Put(p3)
}

func TestHealthPagePoolResetsToBlank(t *testing.T) {
	g := setup(t)

	s := g.Serve()
	s.Route("/dirty", ".html", `<p>dirty</p>`)

	pp := rod.NewHealthPagePool(g.browser, 1)
	defer pp.Cleanup()

	p := getWithin(t, pp)
	p.MustNavigate(s.URL("/dirty")).MustWaitLoad()
	pp.Put(p)

	p = getWithin(t, pp)
	info, err := p.Info()
	g.E(err)
	g.Eq(info.URL, "about:blank")
	pp.Put(p)
}

func TestHealthPagePoolClosesPopups(t *testing.T) {
	g := setup(t)

	s := g.Serve()
	s.Mux.HandleFunc("/popup", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<p>popup</p>`))
	})

	pp := rod.NewHealthPagePool(g.browser, 1)
	defer pp.Cleanup()

	p := getWithin(t, pp)
	p.MustNavigate(s.URL("/")).MustWaitLoad()
	// Return nothing: window.open returns a Window that cannot be serialized.
	p.MustEval(`(u) => { window.open(u) }`, s.URL("/popup"))

	popupOf := func() bool {
		res, err := proto.TargetGetTargets{}.Call(g.browser)
		g.E(err)
		for _, info := range res.TargetInfos {
			if info.OpenerID == p.TargetID && info.Type == proto.TargetTargetInfoTypePage {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(10 * time.Second)
	for !popupOf() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	g.True(popupOf())

	pp.Put(p)
	g.True(!popupOf())
}
