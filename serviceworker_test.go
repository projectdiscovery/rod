package rod_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectdiscovery/rod"
	"github.com/projectdiscovery/rod/lib/proto"
)

// A service worker issues its own fetch. That request belongs to the worker
// target, not the page. This checks which watch sees it.
func TestServiceWorkerRequestWatch(t *testing.T) {
	g := setup(t)

	s := g.Serve()
	var beacons atomic.Int32
	s.Mux.HandleFunc("/sw.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(`self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', e => e.waitUntil(self.clients.claim().then(() => fetch('/beacon'))));`))
	})
	s.Mux.HandleFunc("/beacon", func(w http.ResponseWriter, _ *http.Request) {
		beacons.Add(1)
		_, _ = w.Write([]byte("ok"))
	})
	s.Mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<p>sw</p><script>navigator.serviceWorker.register('/sw.js')</script>`))
	})

	run := func(name string, mustSee bool, watch func(*rod.Browser, *rod.Page) *rod.HijackRouter) {
		t.Run(name, func(t *testing.T) {
			beacons.Store(0)
			// Own browser per subtest: the shared one's cleanup closes every target
			// it can list, and a worker that is still shutting down breaks that.
			b := rod.New().MustConnect()
			defer b.MustClose()
			p := b.MustPage()

			router := watch(b, p)
			var seen atomic.Int32
			router.MustAdd("*/beacon", func(h *rod.Hijack) {
				seen.Add(1)
				h.ContinueRequest(&proto.FetchContinueRequest{})
			})
			go router.Run()
			defer router.MustStop()

			p.MustNavigate(s.URL("/page"))
			deadline := time.Now().Add(10 * time.Second)
			for beacons.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
			}
			time.Sleep(500 * time.Millisecond)
			t.Logf("%s: server saw %d beacon(s), router saw %d", name, beacons.Load(), seen.Load())
			if beacons.Load() == 0 {
				t.Fatal("service worker never fetched; setup is wrong")
			}
			if mustSee && seen.Load() == 0 {
				t.Fatal("router missed the service worker request")
			}
		})
	}

	// A page-scoped router does not see the requests its service worker
	// makes, so that case is only logged. The browser-wide router must see it.
	run("page-only", false, func(_ *rod.Browser, p *rod.Page) *rod.HijackRouter { return p.HijackRequests() })
	run("browser-wide", true, func(b *rod.Browser, _ *rod.Page) *rod.HijackRouter { return b.HijackRequests() })
}
