package rod_test

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

func TestForkWaitEventContextTimeout(t *testing.T) {
	g := setup(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := g.page.WaitEventContext(ctx, &proto.PageJavascriptDialogOpening{})()
	g.True(errors.Is(err, context.DeadlineExceeded))
	g.Lt(time.Since(start), 5*time.Second)
}

func TestForkWaitEventContextPageClosed(t *testing.T) {
	g := setup(t)

	// Chrome 157 drops a Target.closeTarget sent while a navigation commits.
	p := g.browser.MustPage(g.blank()).MustWaitLoad()
	wait := p.WaitEventContext(context.Background(), &proto.PageJavascriptDialogOpening{})

	done := make(chan error, 1)
	go func() { done <- wait() }()

	g.E(proto.TargetCloseTarget{TargetID: p.TargetID}.Call(g.browser))

	select {
	case err := <-done:
		g.True(errors.Is(err, rod.ErrTargetClosed))
	case <-time.After(10 * time.Second):
		g.Fatal("wait did not end after the target closed")
	}
}

func TestForkWaitEventContextDelivers(t *testing.T) {
	g := setup(t)

	e := &proto.PageFrameNavigated{}
	wait := g.page.WaitEventContext(g.Context(), e)
	g.page.MustNavigate(g.blank())
	g.E(wait())
	g.NotZero(e.Frame)
}

func TestForkPageErrAfterClose(t *testing.T) {
	g := setup(t)

	// Chrome 157 drops a Target.closeTarget sent while a navigation commits.
	p := g.browser.MustPage(g.blank()).MustWaitLoad()
	g.Nil(p.Err())

	g.E(proto.TargetCloseTarget{TargetID: p.TargetID}.Call(g.browser))

	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		g.Fatal("page session did not end")
	}

	g.True(errors.Is(p.Err(), rod.ErrTargetClosed))

	_, err := p.Eval(`() => 1`)
	g.True(errors.Is(err, rod.ErrTargetClosed))
	g.True(errors.Is(err, context.Canceled))
}

func TestForkAutoDialog(t *testing.T) {
	g := setup(t)

	u := g.Serve().Route("/", ".html", `<html><body><button onclick="window.res = confirm('x')">b</button></body></html>`).URL()
	p := g.newPage(u)
	stop := p.AutoDialog(true, "")
	defer stop()

	p.MustElement("button").MustClick()
	g.Eq(p.Timeout(10*time.Second).MustEval(`() => window.res`).Bool(), true)
}

func TestForkHandleAuthKeepsPageHijack(t *testing.T) {
	g := setup(t)

	s := g.Serve()
	s.Mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "a" || p != "b" {
			w.Header().Add("WWW-Authenticate", `Basic realm="web"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		g.HandleHTTP(".html", `<p>ok</p>`)(w, r)
	})
	s.Route("/plain", ".html", `<p>origin</p>`)

	hijacked := g.newPage()
	var fulfilled atomic.Int32
	router := hijacked.HijackRequests()
	router.MustAdd("*/plain", func(h *rod.Hijack) {
		h.Response.SetBody(`<p>hijacked</p>`)
		fulfilled.Add(1)
	})
	go router.Run()
	defer router.MustStop()

	wait := g.browser.HandleAuth("a", "b")
	errc := make(chan error, 1)
	go func() { errc <- wait() }()

	// HandleAuth must leave requests paused by the page router alone, so the
	// router can still fulfill them.
	for i := 0; i < 5; i++ {
		hijacked.MustNavigate(s.URL("/plain"))
		hijacked.Timeout(10*time.Second).MustElementR("p", "hijacked")
	}
	g.Eq(fulfilled.Load(), int32(5))

	authed := g.newPage()
	authed.MustNavigate(s.URL("/a"))
	authed.Timeout(10*time.Second).MustElementR("p", "ok")

	select {
	case err := <-errc:
		g.E(err)
	case <-time.After(10 * time.Second):
		g.Fatal("auth not answered")
	}
}

func TestForkStalledSubscriberDoesNotBlock(t *testing.T) {
	g := setup(t)

	stalled := g.newPage(g.blank())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = stalled.Context(ctx).Event() // never read

	for i := 0; i < 20; i++ {
		stalled.MustNavigate(g.blank())
	}

	other := g.newPage()
	done := make(chan struct{})
	go func() {
		defer close(done)
		other.MustNavigate(g.blank()).MustWaitLoad()
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		g.Fatal("a stalled subscriber blocked another page")
	}
}

func TestForkBrowserDisconnect(t *testing.T) {
	g := setup(t)

	l := launcher.New()
	defer l.Kill()
	b := rod.New().ControlURL(l.MustLaunch()).MustConnect()
	p := b.MustPage()

	g.Nil(b.Err())
	l.Kill()

	select {
	case <-b.Disconnected():
	case <-time.After(10 * time.Second):
		g.Fatal("disconnect not reported")
	}
	g.True(errors.Is(b.Err(), cdp.ErrConnClosed))

	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		g.Fatal("page not cancelled on disconnect")
	}
	g.True(errors.Is(p.Err(), cdp.ErrConnClosed))
}

func TestForkHijackResponseStage(t *testing.T) {
	g := setup(t)

	s := g.Serve()
	s.Mux.HandleFunc("/r", func(w http.ResponseWriter, r *http.Request) {
		// Path keeps the cookie off other tests. The jar is per host, not per port.
		http.SetCookie(w, &http.Cookie{Name: "seen", Path: "/r", Value: r.Header.Get("User-Agent")})
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("X-Origin", "yes")
		_, _ = w.Write([]byte(`<p>origin</p>`))
	})

	p := g.newPage()
	router := p.HijackRequests()
	var status atomic.Int32
	var header atomic.Value
	g.E(router.AddResponse("*/r", "", func(h *rod.HijackResponseStage) {
		status.Store(int32(h.StatusCode()))
		header.Store(h.Header().Get("X-Origin"))
		body, err := h.Body()
		if err != nil {
			_ = h.Fail(proto.NetworkErrorReasonFailed)
			return
		}
		_ = h.Fulfill(200, nil, append(body, []byte(`<p>patched</p>`)...))
	}))
	go router.Run()
	defer router.MustStop()

	p.MustNavigate(s.URL("/r"))
	p.Timeout(10*time.Second).MustElementR("p", "patched")
	g.Has(p.MustElement("body").MustText(), "origin")
	g.Eq(status.Load(), int32(200))
	g.Eq(header.Load(), "yes")
}

func TestForkBrowserRouterLeavesPageRouter(t *testing.T) {
	g := setup(t)

	s := g.Serve()
	s.Route("/x", ".html", `<p>origin</p>`)

	p := g.newPage()
	pageRouter := p.HijackRequests()
	pageRouter.MustAdd("*/x", func(h *rod.Hijack) {
		h.Response.SetBody(`<p>page</p>`)
	})
	go pageRouter.Run()
	defer pageRouter.MustStop()

	var browserSaw atomic.Int32
	browserRouter := g.browser.HijackRequests()
	browserRouter.MustAdd("*/nothing-matches", func(h *rod.Hijack) {
		browserSaw.Add(1)
	})
	go browserRouter.Run()
	defer browserRouter.MustStop()

	for i := 0; i < 5; i++ {
		p.MustNavigate(s.URL("/x"))
		p.Timeout(10*time.Second).MustElementR("p", "page")
	}
	g.Eq(browserSaw.Load(), int32(0))
}

func TestForkWaitWritableReadOnly(t *testing.T) {
	g := setup(t)

	u := g.Serve().Route("/", ".html", `<html><body><input readonly>
<script>setTimeout(() => document.querySelector('input').readOnly = false, 500)</script></body></html>`).URL()
	p := g.newPage(u)
	start := time.Now()
	el := p.MustElement("input")
	g.E(el.Timeout(10 * time.Second).WaitWritable())
	g.Gt(time.Since(start), 300*time.Millisecond)
	g.False(el.MustEval(`() => this.readOnly`).Bool())
}

func TestForkScreenshotDeviceScale(t *testing.T) {
	g := setup(t)

	u := g.Serve().Route("/", ".html", `<html><body style="margin:0">
<div id="box" style="position:absolute;left:30px;top:40px;width:50px;height:20px;background:red"></div></body></html>`).URL()
	p := g.newPage(u)
	p.MustSetViewport(400, 300, 2, false)
	p.MustWaitLoad()

	bin, err := p.MustElement("#box").Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
	g.E(err)
	img, err := png.Decode(bytes.NewReader(bin))
	g.E(err)
	g.Eq(img.Bounds().Dx(), 100)
	g.Eq(img.Bounds().Dy(), 40)
	r, _, _, _ := img.At(50, 20).RGBA()
	g.Gt(r, uint32(0xf000))
}

func TestForkWaitDOMIdle(t *testing.T) {
	g := setup(t)

	u := g.Serve().Route("/", ".html", `<html><body><div id="c"></div><script>
let n = 0
const t = setInterval(() => {
	document.getElementById('c').textContent = n++
	if (n > 10) clearInterval(t)
}, 50)
</script></body></html>`).URL()
	p := g.newPage(u)
	g.E(p.Timeout(10 * time.Second).WaitDOMIdle(200 * time.Millisecond))
	g.Eq(p.MustElement("#c").MustText(), "10")
}

func TestForkWaitDOMIdleTimeout(t *testing.T) {
	g := setup(t)

	u := g.Serve().Route("/", ".html", `<html><body><div id="c"></div><script>
setInterval(() => document.getElementById('c').textContent = Math.random(), 20)
</script></body></html>`).URL()
	p := g.newPage(u)
	err := p.Timeout(500 * time.Millisecond).WaitDOMIdle(200 * time.Millisecond)
	g.True(errors.Is(err, context.DeadlineExceeded))
}

func TestForkCrossSiteFrame(t *testing.T) {
	g := setup(t)

	s := g.Serve()
	s.Route("/inner", ".html", `<html><body><h1>inner</h1></body></html>`)
	inner := strings.Replace(s.URL("/inner"), "127.0.0.1", "localhost", 1)
	s.Route("/outer", ".html", `<html><body><iframe src="`+inner+`"></iframe></body></html>`)

	// The default launcher disables site isolation; enable it so the frame
	// is an out-of-process iframe with its own target.
	l := launcher.New().
		Delete("disable-features").
		Delete("disable-site-isolation-trials").
		Set("site-per-process").
		Set("proxy-bypass-list", "<-loopback>")
	defer l.Kill()
	b := rod.New().ControlURL(l.MustLaunch()).MustConnect()
	defer b.MustClose()

	p := b.MustPage(s.URL("/outer"))
	p.MustWaitLoad()
	frame, err := p.Timeout(10 * time.Second).MustElement("iframe").Frame()
	g.E(err)
	g.Neq(frame.TargetID, p.TargetID)
	g.Eq(frame.Timeout(10*time.Second).MustElement("h1").MustText(), "inner")
	g.Has(frame.MustInfo().URL, "localhost")
}

func TestForkSameSiteFrame(t *testing.T) {
	g := setup(t)

	s := g.Serve()
	s.Route("/inner", ".html", `<html><body><h1>same</h1></body></html>`)
	s.Route("/outer", ".html", `<html><body><iframe src="/inner"></iframe></body></html>`)

	p := g.newPage(s.URL("/outer"))
	p.MustWaitLoad()
	frame := p.MustElement("iframe").MustFrame()
	g.Eq(frame.Timeout(10*time.Second).MustElement("h1").MustText(), "same")
	g.True(frame.IsIframe())
}

func TestForkCloseDuringNavigation(t *testing.T) {
	g := setup(t)

	// Page.navigate returns before the navigation commits. Chrome 157
	// acknowledges a close sent then without closing the target, so Close
	// must retry.
	p := g.browser.MustPage(g.blank()).MustWaitLoad()
	_, err := proto.PageNavigate{URL: g.blank()}.Call(p)
	g.E(err)
	start := time.Now()
	g.E(p.ForceClose(10 * time.Second))
	g.Lt(time.Since(start), 5*time.Second)
	g.True(errors.Is(p.Err(), rod.ErrTargetClosed))
}

func TestForkPageNavigateFailClosesTarget(t *testing.T) {
	g := setup(t)

	hang := make(chan struct{})
	defer close(hang)
	s := g.Serve()
	s.Mux.HandleFunc("/hang", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})

	b := g.browser.MustIncognito()
	defer b.MustClose()

	_, err := b.Timeout(time.Second).Page(proto.TargetCreateTarget{URL: s.URL("/hang")})
	g.Is(err, context.DeadlineExceeded)

	open := func() bool {
		list, err := proto.TargetGetTargets{}.Call(b)
		g.E(err)
		for _, info := range list.TargetInfos {
			if info.BrowserContextID == b.BrowserContextID {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	for open() {
		if time.Now().After(deadline) {
			t.Fatal("target left open after failed navigation")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestForkWaitIdleAfterClick(t *testing.T) {
	g := setup(t)

	l := launcher.New().NoSandbox(true)
	defer l.Cleanup()
	defer l.Kill()
	b := rod.New().ControlURL(l.MustLaunch()).MustConnect()
	defer b.MustClose()

	p := b.MustPage(g.srcFile("fixtures/click.html")).MustWaitLoad()
	p.MustElement("button").MustClick()

	// requestIdleCallback may wait out its timeout on a busy runner.
	g.E(p.WaitIdle(10 * time.Second))
}

func TestForkCompliantMode(t *testing.T) {
	setup(t)
	bin, ok := launcher.LookPath()
	if !ok {
		t.Skip("no browser installed")
	}
	l := launcher.New().Bin(bin).NoSandbox(true).Compliant(true)
	defer l.Cleanup()
	defer l.Kill()
	b := rod.New().ControlURL(l.MustLaunch()).Compliant(true).MustConnect()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>ok</body></html>"))
	}))
	defer s.Close()

	const hint = `() => navigator.userAgentData.getHighEntropyValues(["brands","fullVersionList","architecture","platform","platformVersion"]).then(h => ({
		ua: navigator.userAgent,
		webdriver: navigator.webdriver,
		brands: h.brands.map(b => b.brand).join(","),
		full: h.fullVersionList.map(b => b.version).join(","),
		architecture: h.architecture,
		platform: h.platform,
		platformVersion: h.platformVersion
	}))`
	check := func(p *rod.Page) {
		got := p.MustEval(hint)
		ua := got.Get("ua").Str()
		if strings.Contains(ua, "Headless") || strings.Contains(ua, "Chrome/114") {
			t.Fatalf("ua %s", ua)
		}
		if got.Get("webdriver").Bool() {
			t.Fatal("navigator.webdriver")
		}
		brands := got.Get("brands").Str()
		if strings.Contains(brands, "Headless") || !strings.Contains(brands, "Chromium") {
			t.Fatalf("brands %s", brands)
		}
		if got.Get("architecture").Str() == "" || got.Get("platform").Str() == "" {
			t.Fatalf("client hints cleared: %s", got)
		}
		if !strings.Contains(got.Get("full").Str(), ".") {
			t.Fatalf("full version %s", got.Get("full").Str())
		}
	}
	check(b.MustPage(s.URL).MustWaitLoad())
	check(b.MustPage(s.URL).MustWaitLoad())
}

func TestForkHijackEventExposed(t *testing.T) {
	g := setup(t)

	s := g.Serve()
	s.Route("/event", ".html", `<p>ok</p>`)

	p := g.newPage()
	router := p.HijackRequests()
	var seen atomic.Int32
	router.MustAdd("*/event", func(h *rod.Hijack) {
		ev := h.Request.Event()
		g.True(ev.RequestID != "")
		g.Eq(ev.ResourceType, proto.NetworkResourceTypeDocument)
		g.True(ev.Request.URL != "")
		g.Nil(ev.ResponseStatusCode)
		seen.Add(1)
		h.ContinueRequest(&proto.FetchContinueRequest{})
	})
	go router.Run()
	defer router.MustStop()

	p.MustNavigate(s.URL("/event"))
	g.Eq(seen.Load(), int32(1))
}
