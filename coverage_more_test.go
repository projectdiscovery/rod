package rod

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

// bareClient is a CDP client that cannot report its connection state.
type bareClient struct {
	events chan *cdp.Event
}

func (c *bareClient) Event() <-chan *cdp.Event { return c.events }

func (c *bareClient) Call(context.Context, string, string, interface{}) ([]byte, error) {
	return []byte("{}"), nil
}

// fakeChrome writes a shell script that reports a DevTools address and exits.
func fakeChrome(t *testing.T, devtools string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "chrome")
	script := "#!/bin/sh\necho '" + devtools + "'\nsleep 0.5\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// versionServer serves the CDP version endpoint the launcher resolves.
func versionServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/x"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBrowserConnectPipeError(t *testing.T) {
	oldLauncher, oldPipe := newLauncher, pipeSupported
	defer func() { newLauncher, pipeSupported = oldLauncher, oldPipe }()

	newLauncher = func() *launcher.Launcher {
		return launcher.New().Bin(filepath.Join(t.TempDir(), "missing"))
	}
	pipeSupported = func() bool { return true }

	b := New().Compliant(true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b.ctx = ctx
	if err := b.Connect(); err == nil {
		t.Fatal("expected the launch pipe error")
	}
}

func TestBrowserConnectPortFallback(t *testing.T) {
	oldLauncher, oldPipe := newLauncher, pipeSupported
	defer func() { newLauncher, pipeSupported = oldLauncher, oldPipe }()

	srv := versionServer(t)
	devtools := "DevTools listening on ws://" + srv.Listener.Addr().String() + "/devtools/browser/x"
	newLauncher = func() *launcher.Launcher {
		return launcher.New().Bin(fakeChrome(t, devtools))
	}
	pipeSupported = func() bool { return false }

	b := New()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b.ctx = ctx
	// The printed websocket address is unreachable, so Connect fails after
	// the port launch and must tear the browser down.
	if err := b.Connect(); err == nil {
		t.Fatal("expected the connect error")
	}
}

func TestBrowserConnectAbandonLaunch(t *testing.T) {
	srv := versionServer(t)
	devtools := "DevTools listening on ws://" + srv.Listener.Addr().String() + "/devtools/browser/x"
	l := launcher.New().Bin(fakeChrome(t, devtools))
	if _, err := l.Launch(); err != nil {
		t.Fatal(err)
	}
	l.Cleanup() // let the fake browser exit so Cleanup returns at once

	c := newFakeClient()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.TargetSetDiscoverTargets{}).ProtoReq() {
			return nil, errors.New("discover")
		}
		return []byte("{}"), nil
	}
	b := New().Client(c)
	b.launcher = l

	if err := b.Connect(); err == nil {
		t.Fatal("expected the discover error")
	}
	if b.client != nil || b.launcher != nil {
		t.Fatal("the launch was not abandoned")
	}
	if !c.closed {
		t.Fatal("the client was not closed")
	}
}

func TestInitEventsNilFields(t *testing.T) {
	c := newFakeClient()
	b := &Browser{ctx: context.Background(), states: &sync.Map{}, client: c}
	b.initEvents()
	if b.domains == nil || b.domainMu == nil || b.autoAttach == nil {
		t.Fatal("initEvents did not fill the nil fields")
	}
	close(c.events)
	time.Sleep(20 * time.Millisecond)
}

func TestBrowserWaitEvent(t *testing.T) {
	c := newFakeClient()
	b := New().Client(c)
	b.initEvents()
	defer func() {
		close(c.events)
		time.Sleep(20 * time.Millisecond)
	}()

	wait := b.WaitEvent(&proto.TargetTargetDestroyed{})
	c.events <- &cdp.Event{
		Method: (proto.TargetTargetDestroyed{}).ProtoEvent(),
		Params: []byte(`{"targetId":"t"}`),
	}
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitEvent did not return")
	}
}

func TestBrowserConnectionState(t *testing.T) {
	b := New().Client(&bareClient{events: make(chan *cdp.Event)})
	if b.Disconnected() != nil {
		t.Fatal("a bare client has no connection state")
	}
	if b.Err() != nil {
		t.Fatal(b.Err())
	}
}

func TestSessionErrCustomCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("custom")
	cancel(cause)
	if sessionErr(ctx) != cause {
		t.Fatal("the custom cause must be kept")
	}
	if !errors.Is(waitResult(context.Background()), ErrTargetClosed) {
		t.Fatal("a live context means the target closed")
	}
}

func TestHandleDialogContextCanceled(t *testing.T) {
	c := newFakeClient()
	b := New().Client(c)
	b.initEvents()
	defer func() {
		close(c.events)
		time.Sleep(20 * time.Millisecond)
	}()

	p := b.PageFromSession("s")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wait, handle := p.HandleDialogContext(ctx)
	if _, err := wait(); err == nil {
		t.Fatal("expected the wait error")
	}
	if err := handle(&proto.PageHandleJavaScriptDialog{}); err != nil {
		t.Fatal(err)
	}
}

func TestPageWithReferrer(t *testing.T) {
	p := &Page{}
	clone := p.WithReferrer("https://example.com")
	if clone.referrer != "https://example.com" || clone == p {
		t.Fatal("WithReferrer must return a clone with the referrer")
	}
}

func newCloseBrowser() (*Browser, *fakeClient) {
	c := newFakeClient()
	b := &Browser{
		ctx:        context.Background(),
		client:     c,
		states:     &sync.Map{},
		domainMu:   &sync.Mutex{},
		domains:    map[stateKey]int{},
		autoAttach: new(atomic.Bool),
	}
	b.e = func(...interface{}) {}
	return b, c
}

func TestPageForceCloseDefaultTimeout(t *testing.T) {
	b, _ := newCloseBrowser()
	p := &Page{browser: b, ctx: context.Background(), TargetID: "t", SessionID: "s", helpersLock: &sync.Mutex{}}
	if err := p.ForceClose(0); err != nil {
		t.Fatal(err)
	}
}

func TestPageCloseWithDeadline(t *testing.T) {
	b, _ := newCloseBrowser()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	p := &Page{browser: b, ctx: ctx, TargetID: "t", SessionID: "s", helpersLock: &sync.Mutex{}}
	if err := p.closeWith(2 * time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestPageCloseWithExpiredContext(t *testing.T) {
	b, _ := newCloseBrowser()
	p := &Page{browser: b, ctx: context.Background(), TargetID: "t", SessionID: "s", helpersLock: &sync.Mutex{}}
	if err := p.closeWith(-1); err == nil {
		t.Fatal("expected the context error")
	}
}

func TestPageCloseNotAttached(t *testing.T) {
	b, c := newCloseBrowser()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.PageClose{}).ProtoReq() {
			return nil, cdp.ErrNotAttachedToActivePage
		}
		return []byte("{}"), nil
	}
	p := &Page{browser: b, ctx: context.Background(), TargetID: "t", SessionID: "s", helpersLock: &sync.Mutex{}}
	err := p.closeWith(50 * time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestPageCloseStillAttached(t *testing.T) {
	b, _ := newCloseBrowser()
	p := &Page{browser: b, ctx: context.Background(), TargetID: "t", SessionID: "s", helpersLock: &sync.Mutex{}}
	b.cachePage(p)
	// No target destroyed event, so the still cached session is reported.
	if err := p.closeWith(time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestPageWaitRepaintNoRoot(t *testing.T) {
	c := newFakeClient()
	b := New().Client(c)
	b.initEvents()
	defer func() {
		close(c.events)
		time.Sleep(20 * time.Millisecond)
	}()
	c.call = reply(map[string]string{
		(proto.RuntimeEvaluate{}).ProtoReq():       `{"result":{"objectId":"w"}}`,
		(proto.RuntimeCallFunctionOn{}).ProtoReq(): `{"result":{"objectId":"o"}}`,
	})

	p := b.PageFromSession("s")
	p.root = nil
	if err := p.WaitRepaint(); err != nil {
		t.Fatal(err)
	}
}

func TestPageWaitLoadRetryCanceled(t *testing.T) {
	c := newFakeClient()
	c.call = func(context.Context, string, string, interface{}) ([]byte, error) {
		return nil, cdp.ErrCtxDestroyed
	}
	b := New().Client(c)
	p := b.PageFromSession("s")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.ctx = ctx
	// The retry loop re-checks the page context before the next attempt.
	err := p.WaitLoad()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestPageWaitLoadRetryExhausted(t *testing.T) {
	c := newFakeClient()
	c.call = func(context.Context, string, string, interface{}) ([]byte, error) {
		return nil, cdp.ErrCtxDestroyed
	}
	b := New().Client(c)
	p := b.PageFromSession("s")
	err := p.WaitLoad()
	if !errors.Is(err, cdp.ErrCtxDestroyed) {
		t.Fatalf("got %v", err)
	}
}

func TestPageWaitRequestIdleDone(t *testing.T) {
	c := newFakeClient()
	b := New().Client(c)
	b.initEvents()

	p := b.PageFromSession("s")
	wait := p.WaitRequestIdle(300*time.Millisecond, nil, nil, nil)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		time.Sleep(20 * time.Millisecond)
		c.events <- &cdp.Event{
			SessionID: "s",
			Method:    (proto.NetworkRequestWillBeSent{}).ProtoEvent(),
			Params:    []byte(`{"requestId":"1","request":{"url":"http://x/"},"type":"Document"}`),
		}
		c.events <- &cdp.Event{
			SessionID: "s",
			Method:    (proto.NetworkLoadingFinished{}).ProtoEvent(),
			Params:    []byte(`{"requestId":"1"}`),
		}
		c.events <- &cdp.Event{
			SessionID: "s",
			Method:    (proto.NetworkRequestWillBeSent{}).ProtoEvent(),
			Params:    []byte(`{"requestId":"2","request":{"url":"http://x/2"},"type":"Document"}`),
		}
		c.events <- &cdp.Event{
			SessionID: "s",
			Method:    (proto.NetworkLoadingFailed{}).ProtoEvent(),
			Params:    []byte(`{"requestId":"2"}`),
		}
	}()

	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitRequestIdle did not finish")
	}
	<-sent
	close(c.events)
	time.Sleep(20 * time.Millisecond)
}

func TestGetHelperNilMap(t *testing.T) {
	p := &Page{helpersLock: &sync.Mutex{}}
	if _, ok := p.getHelper("ctx", "fn"); ok {
		t.Fatal("empty cache")
	}
	// The map and the per-context map are created on the first lookup.
	if p.helpers == nil || p.helpers["ctx"] == nil {
		t.Fatal("helpers not initialized")
	}
}

func TestJSCtxIDCrossOriginFrame(t *testing.T) {
	c := newFakeClient()
	c.call = reply(map[string]string{
		(proto.DOMDescribeNode{}).ProtoReq(): `{"node":{}}`,
	})
	b := New().Client(c)
	p := &Page{
		ctx:         context.Background(),
		browser:     b,
		jsCtxLock:   &sync.Mutex{},
		jsCtxID:     new(proto.RuntimeRemoteObjectID),
		helpersLock: &sync.Mutex{},
	}
	p.element = &Element{page: p, Object: &proto.RuntimeRemoteObject{}}
	_, err := p.getJSCtxID()
	if err == nil {
		t.Fatal("expected the cross-origin frame error")
	}
}

func TestElementFrameAttachError(t *testing.T) {
	c := newFakeClient()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		switch method {
		case (proto.DOMDescribeNode{}).ProtoReq():
			return []byte(`{"node":{"frameId":"f"}}`), nil
		case (proto.TargetGetTargetInfo{}).ProtoReq():
			return []byte(`{"targetInfo":{"type":"iframe"}}`), nil
		default:
			return nil, errors.New("attach")
		}
	}
	b := New().Client(c)
	p := b.PageFromSession("s")
	el := &Element{page: p, ctx: context.Background(), Object: &proto.RuntimeRemoteObject{}}
	if _, err := el.Frame(); err == nil {
		t.Fatal("expected the attach error")
	}
}

func TestCompliantFromValueNonString(t *testing.T) {
	ua, meta, err := compliantFromValue(gson.New(map[string]interface{}{"ua": "x"}))
	if err != nil || meta != nil || ua != "" {
		t.Fatalf("got %q %v %v", ua, meta, err)
	}
}

func TestReadCompliantProfileValueError(t *testing.T) {
	evals := []string{
		`{"result":{"value":"{"}}`,
		`{"result":{"value":"{\"ua\":\"Mozilla Chrome\",\"hints\":{\"brands\":[{\"brand\":\"Chrome\",\"version\":\"1\"}]}}"}}`,
	}
	var i int
	c := newFakeClient()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		switch method {
		case (proto.TargetCreateTarget{}).ProtoReq():
			return []byte(`{"targetId":"t"}`), nil
		case (proto.TargetAttachToTarget{}).ProtoReq():
			return []byte(`{"sessionId":"s"}`), nil
		case (proto.RuntimeEvaluate{}).ProtoReq():
			i++
			if i > len(evals) {
				return []byte(evals[len(evals)-1]), nil
			}
			return []byte(evals[i-1]), nil
		}
		return []byte("{}"), nil
	}
	b := New().Client(c)
	old := compliantRetryDelay
	compliantRetryDelay = 0
	defer func() { compliantRetryDelay = old }()
	if _, _, err := b.readCompliantProfile(); err != nil {
		t.Fatal(err)
	}
}

// hijackTestRouter builds a router whose client is a fake.
func hijackTestRouter(c *fakeClient) *HijackRouter {
	return &HijackRouter{
		enable:  &proto.FetchEnable{},
		browser: New().Client(c),
		client:  c,
	}
}

func TestHijackHandlePanic(t *testing.T) {
	c := newFakeClient()
	r := hijackTestRouter(c)
	r.handlers = []*hijackHandler{{
		regexp:  regexp.MustCompile(`\A.*\z`),
		handler: func(*Hijack) { panic("boom") },
	}}
	e := &proto.FetchRequestPaused{RequestID: "1", Request: &proto.NetworkRequest{URL: "http://x/"}}
	r.handle(context.Background(), e)
}

func TestHijackHandleSkips(t *testing.T) {
	c := newFakeClient()
	r := hijackTestRouter(c)
	r.handlers = []*hijackHandler{{
		regexp:   regexp.MustCompile(`\A.*\z`),
		resource: proto.NetworkResourceTypeImage,
		handler:  func(*Hijack) { t.Fatal("handler must not run") },
	}}
	e := &proto.FetchRequestPaused{
		RequestID:    "1",
		Request:      &proto.NetworkRequest{URL: "http://x/"},
		ResourceType: proto.NetworkResourceTypeXHR,
	}
	r.handle(context.Background(), e)
}

func TestHijackApplyWithoutPatterns(t *testing.T) {
	c := newFakeClient()
	r := hijackTestRouter(c)
	if err := r.apply(); err != nil {
		t.Fatal(err)
	}
}

func TestHijackAddInvalidPattern(t *testing.T) {
	old := compilePattern
	compilePattern = func(string) (*regexp.Regexp, error) { return nil, errors.New("bad pattern") }
	defer func() { compilePattern = old }()

	c := newFakeClient()
	r := hijackTestRouter(c)
	if err := r.Add("*", "", func(*Hijack) {}); err == nil {
		t.Fatal("expected the pattern error")
	}
	if err := r.AddResponse("*", "", func(*HijackResponseStage) {}); err == nil {
		t.Fatal("expected the pattern error")
	}
}

func TestHijackRemoveKeepsOtherResponse(t *testing.T) {
	c := newFakeClient()
	r := hijackTestRouter(c)
	r.handlers = []*hijackHandler{
		{pattern: "a", regexp: regexp.MustCompile(`\Aa\z`), handler: func(*Hijack) {}},
		{pattern: "b", regexp: regexp.MustCompile(`\Ab\z`), handler: func(*Hijack) {}},
	}
	r.response = []*hijackResponseHandler{
		{pattern: "a", regexp: regexp.MustCompile(`\Aa\z`), handler: func(*HijackResponseStage) {}},
		{pattern: "b", regexp: regexp.MustCompile(`\Ab\z`), handler: func(*HijackResponseStage) {}},
	}
	if err := r.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if len(r.handlers) != 1 || r.handlers[0].pattern != "b" {
		t.Fatalf("handlers %+v", r.handlers)
	}
	if len(r.response) != 1 || r.response[0].pattern != "b" {
		t.Fatalf("response %+v", r.response)
	}
}

func TestHijackRequestGetBody(t *testing.T) {
	c := newFakeClient()
	r := hijackTestRouter(c)
	e := &proto.FetchRequestPaused{
		RequestID: "1",
		Request:   &proto.NetworkRequest{URL: "http://x/", PostData: "hi"},
	}
	ctx := r.new(context.Background(), e)
	rc, err := ctx.Request.req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if err != nil || string(b) != "hi" {
		t.Fatalf("got %q %v", b, err)
	}
	_ = rc.Close()
}

func TestHijackLoadResponseBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer srv.Close()

	c := newFakeClient()
	r := hijackTestRouter(c)
	e := &proto.FetchRequestPaused{RequestID: "1", Request: &proto.NetworkRequest{URL: srv.URL}}
	ctx := r.new(context.Background(), e)
	ctx.maxBody = 3
	if err := ctx.LoadResponse(srv.Client(), true); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("got %v", err)
	}
}

func TestHandleAuthRestoresPrevious(t *testing.T) {
	c := newFakeClient()
	b := New().Client(c)
	b.initEvents()
	defer func() {
		close(c.events)
		time.Sleep(20 * time.Millisecond)
	}()
	b.states.Store(b.key("", (proto.FetchEnable{}).ProtoReq()), &proto.FetchEnable{
		Patterns: []*proto.FetchRequestPattern{{URLPattern: "x"}},
	})

	wait := b.HandleAuth("u", "p")
	done := make(chan error, 1)
	go func() { done <- wait() }()

	c.events <- &cdp.Event{
		Method: (proto.FetchAuthRequired{}).ProtoEvent(),
		Params: []byte(`{"requestId":"1","challenge":{"origin":"x","scheme":"basic","realm":"r"},"authChallengeResponse":{"response":"Default"}}`),
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleAuth did not answer")
	}
}

func TestHandleAuthEnableError(t *testing.T) {
	c := newFakeClient()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.FetchEnable{}).ProtoReq() {
			return nil, errors.New("enable")
		}
		return []byte("{}"), nil
	}
	b := New().Client(c)
	b.initEvents()
	defer func() {
		close(c.events)
		time.Sleep(20 * time.Millisecond)
	}()

	if err := b.HandleAuth("u", "p")(); err == nil {
		t.Fatal("expected the enable error")
	}
}

func TestHandleAuthContextEnds(t *testing.T) {
	c := newFakeClient()
	b := New().Client(c)
	ctx, cancel := context.WithCancel(context.Background())
	b.ctx = ctx
	b.initEvents()
	defer func() {
		close(c.events)
		time.Sleep(20 * time.Millisecond)
	}()

	wait := b.HandleAuth("u", "p")
	done := make(chan error, 1)
	go func() { done <- wait() }()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a context error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleAuth did not end")
	}
}

func TestHijackResponseStage(t *testing.T) {
	c := newFakeClient()
	e := &proto.FetchRequestPaused{
		RequestID: "1",
		Request:   &proto.NetworkRequest{URL: "http://x/"},
	}
	s := &HijackResponseStage{Event: e, client: c}
	if s.URL() != "http://x/" {
		t.Fatal(s.URL())
	}
	if s.StatusCode() != 0 {
		t.Fatal("a nil status code is zero")
	}
	code := 204
	s.Event.ResponseStatusCode = &code
	if s.StatusCode() != 204 {
		t.Fatal(s.StatusCode())
	}
	if s.Header() == nil {
		t.Fatal("header")
	}

	// Body without base64.
	c.call = reply(map[string]string{
		(proto.FetchGetResponseBody{}).ProtoReq(): `{"body":"abc"}`,
	})
	s.maxBody = 10
	if b, err := s.Body(); err != nil || string(b) != "abc" {
		t.Fatalf("got %q %v", b, err)
	}
	s.maxBody = 2
	if _, err := s.Body(); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("got %v", err)
	}

	// Body with base64.
	c.call = reply(map[string]string{
		(proto.FetchGetResponseBody{}).ProtoReq(): `{"body":"MDEyMzQ1Njc4OQ==","base64Encoded":true}`,
	})
	s.maxBody = 2
	if _, err := s.Body(); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("got %v", err)
	}
	s.maxBody = 64
	if b, err := s.Body(); err != nil || string(b) != "0123456789" {
		t.Fatalf("got %q %v", b, err)
	}

	// A failing body call.
	c.call = func(context.Context, string, string, interface{}) ([]byte, error) {
		return nil, errors.New("body")
	}
	if _, err := s.Body(); err == nil {
		t.Fatal("expected the body error")
	}

	// Continue.
	c.call = reply(nil)
	if err := s.Continue(); err != nil || !s.done {
		t.Fatalf("got %v %v", err, s.done)
	}

	// Fulfill with a replacement header.
	s.done = false
	header := http.Header{"X-A": []string{"1", "2"}}
	if err := s.Fulfill(200, header, []byte("x")); err != nil || !s.done {
		t.Fatalf("got %v %v", err, s.done)
	}

	// Fail.
	s.done = false
	if err := s.Fail(proto.NetworkErrorReasonAborted); err != nil || !s.done {
		t.Fatalf("got %v %v", err, s.done)
	}
}

func TestHijackHandleResponse(t *testing.T) {
	c := newFakeClient()
	var continues int
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.FetchContinueResponse{}).ProtoReq() {
			continues++
		}
		return []byte("{}"), nil
	}

	var called int
	r := &HijackRouter{client: c, response: []*hijackResponseHandler{{
		pattern:  "a",
		regexp:   regexp.MustCompile(`\Aa\z`),
		resource: proto.NetworkResourceTypeImage,
		handler:  func(*HijackResponseStage) { called++ },
	}}}

	// The URL does not match.
	r.handleResponse(&proto.FetchRequestPaused{
		RequestID: "1",
		Request:   &proto.NetworkRequest{URL: "http://x/"},
	})
	// The resource type does not match.
	r.handleResponse(&proto.FetchRequestPaused{
		RequestID:    "2",
		Request:      &proto.NetworkRequest{URL: "a"},
		ResourceType: proto.NetworkResourceTypeXHR,
	})
	if called != 0 || continues != 2 {
		t.Fatalf("called %d continues %d", called, continues)
	}

	// The handler does not finish the stage, so the response is continued.
	r.response[0].resource = ""
	r.handleResponse(&proto.FetchRequestPaused{
		RequestID: "3",
		Request:   &proto.NetworkRequest{URL: "a"},
	})
	if called != 1 || continues != 3 {
		t.Fatalf("called %d continues %d", called, continues)
	}
}

func TestPoolCleanupEmpty(t *testing.T) {
	p := NewPool[int](3)
	<-p // leave one empty slot
	var seen int
	p.Cleanup(func(*int) { seen++ })
	if seen != 0 {
		t.Fatalf("seen %d", seen)
	}
}

func hpFake(t *testing.T) (*Browser, *fakeClient) {
	t.Helper()
	c := newFakeClient()
	c.call = reply(map[string]string{
		(proto.BrowserGetVersion{}).ProtoReq():     `{"product":"x"}`,
		(proto.TargetGetTargetInfo{}).ProtoReq():   `{"targetInfo":{"type":"page","url":"about:blank"}}`,
		(proto.TargetGetTargets{}).ProtoReq():      `{"targetInfos":[]}`,
		(proto.RuntimeEvaluate{}).ProtoReq():       `{"result":{"objectId":"w"}}`,
		(proto.RuntimeCallFunctionOn{}).ProtoReq(): `{"result":{"objectId":"o","value":1}}`,
		(proto.PageNavigate{}).ProtoReq():          `{}`,
	})
	b := New().Client(c)
	b.initEvents()
	t.Cleanup(func() {
		close(c.events)
		time.Sleep(20 * time.Millisecond)
	})
	return b, c
}

func hpPage(b *Browser) *Page {
	p := b.PageFromSession("s")
	p.TargetID = "t"
	return p
}

func TestHealthPoolPutNil(t *testing.T) {
	b, _ := hpFake(t)
	pp := NewHealthPagePool(b, 1)
	<-pp.slots // take the idle slot, as a handed-out Get would
	pp.Put(nil)
	if _, err := pp.slots.Get(func() (*Page, error) { return &Page{}, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestHealthPoolCreateError(t *testing.T) {
	c := newFakeClient()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.TargetCreateTarget{}).ProtoReq() {
			return nil, errors.New("create")
		}
		return []byte("{}"), nil
	}
	b := New().Client(c)
	pp := NewHealthPagePool(b, 1)
	if _, err := pp.Get(); err == nil {
		t.Fatal("expected the create error")
	}
}

func TestHealthPoolHealthyVersionError(t *testing.T) {
	b, c := hpFake(t)
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.BrowserGetVersion{}).ProtoReq() {
			return nil, errors.New("version")
		}
		return []byte("{}"), nil
	}
	pp := NewHealthPagePool(b, 1)
	if pp.healthy(hpPage(b)) {
		t.Fatal("an unreachable browser is not healthy")
	}
}

func TestHealthPoolClosePopupsErrors(t *testing.T) {
	b, c := hpFake(t)
	pp := NewHealthPagePool(b, 1)
	p := hpPage(b)

	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.TargetGetTargets{}).ProtoReq() {
			return nil, errors.New("targets")
		}
		return []byte("{}"), nil
	}
	if err := pp.closePopups(p); err == nil {
		t.Fatal("expected the targets error")
	}

	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		switch method {
		case (proto.TargetGetTargets{}).ProtoReq():
			return []byte(`{"targetInfos":[{"targetId":"popup","openerId":"t","type":"page"}]}`), nil
		case (proto.TargetCloseTarget{}).ProtoReq():
			return nil, errors.New("close")
		}
		return []byte("{}"), nil
	}
	if err := pp.closePopups(p); err == nil {
		t.Fatal("expected the close error")
	}
}

func TestHealthPoolResetBranches(t *testing.T) {
	b, c := hpFake(t)
	pp := NewHealthPagePool(b, 1)

	// The full reset succeeds.
	if !pp.reset(hpPage(b)) {
		t.Fatal("the reset must succeed")
	}

	// Navigation fails.
	saved := c.call
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.PageNavigate{}).ProtoReq() {
			return nil, errors.New("navigate")
		}
		return saved(nil, "", method, nil)
	}
	if pp.reset(hpPage(b)) {
		t.Fatal("a failed navigation is not a reset")
	}

	// The load wait fails.
	var evalCalls int
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.RuntimeCallFunctionOn{}).ProtoReq() {
			evalCalls++
			if evalCalls >= 2 {
				return nil, errors.New("wait load")
			}
		}
		return saved(nil, "", method, nil)
	}
	if pp.reset(hpPage(b)) {
		t.Fatal("a failed load wait is not a reset")
	}

	// The final info call fails.
	var infos int
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.TargetGetTargetInfo{}).ProtoReq() {
			infos++
			if infos >= 2 {
				return nil, errors.New("info")
			}
		}
		return saved(nil, "", method, nil)
	}
	if pp.reset(hpPage(b)) {
		t.Fatal("a failed info call is not a reset")
	}

	// The final info reports another URL.
	infos = 0
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.TargetGetTargetInfo{}).ProtoReq() {
			infos++
			if infos >= 2 {
				return []byte(`{"targetInfo":{"type":"page","url":"other"}}`), nil
			}
		}
		return saved(nil, "", method, nil)
	}
	if pp.reset(hpPage(b)) {
		t.Fatal("another URL is not a reset")
	}
}

func TestBrowserConnectLaunchError(t *testing.T) {
	oldLauncher, oldPipe := newLauncher, pipeSupported
	defer func() { newLauncher, pipeSupported = oldLauncher, oldPipe }()

	newLauncher = func() *launcher.Launcher {
		return launcher.New().Bin(filepath.Join(t.TempDir(), "missing"))
	}
	pipeSupported = func() bool { return false }

	b := New()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b.ctx = ctx
	if err := b.Connect(); err == nil {
		t.Fatal("expected the launch error")
	}
}

func TestPageFromTargetCompliantError(t *testing.T) {
	c := newFakeClient()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		switch method {
		case (proto.TargetCreateTarget{}).ProtoReq():
			return []byte(`{"targetId":"t"}`), nil
		case (proto.TargetAttachToTarget{}).ProtoReq():
			return []byte(`{"sessionId":"s"}`), nil
		}
		return nil, errors.New("boom")
	}
	old := compliantRetryDelay
	compliantRetryDelay = 0
	defer func() { compliantRetryDelay = old }()

	b := New().Client(c)
	b.compliant = true
	if _, err := b.PageFromTarget("t"); err == nil {
		t.Fatal("expected the profile error")
	}

	// The user agent override fails.
	b = New().Client(c)
	b.compliant = true
	b.profile = &compliantProfile{ua: "ua", meta: &proto.EmulationUserAgentMetadata{}}
	b.profile.once.Do(func() {}) // the profile was already read
	if _, err := b.PageFromTarget("t"); err == nil {
		t.Fatal("expected the override error")
	}
}

func TestSetHelperNilMap(t *testing.T) {
	p := &Page{helpersLock: &sync.Mutex{}}
	p.setHelper("ctx", "fn", "id")
	if p.helpers["ctx"]["fn"] != "id" {
		t.Fatal("helper not cached")
	}
}

func TestHandleDialogContextSuccess(t *testing.T) {
	c := newFakeClient()
	b := New().Client(c)
	b.initEvents()
	defer func() {
		close(c.events)
		time.Sleep(20 * time.Millisecond)
	}()

	p := b.PageFromSession("s")
	wait, handle := p.HandleDialogContext(context.Background())
	opened := make(chan *proto.PageJavascriptDialogOpening, 1)
	go func() {
		e, err := wait()
		if err != nil {
			opened <- nil
			return
		}
		opened <- e
	}()
	c.events <- &cdp.Event{
		SessionID: "s",
		Method:    (proto.PageJavascriptDialogOpening{}).ProtoEvent(),
		Params:    []byte(`{"message":"hi","type":"alert"}`),
	}
	select {
	case e := <-opened:
		if e == nil || e.Message != "hi" {
			t.Fatalf("got %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the dialog event was not delivered")
	}
	if err := handle(&proto.PageHandleJavaScriptDialog{Accept: true}); err != nil {
		t.Fatal(err)
	}
}

func TestHealthPoolResetClosePopupsError(t *testing.T) {
	b, c := hpFake(t)
	pp := NewHealthPagePool(b, 1)
	saved := c.call
	c.call = func(ctx context.Context, session, method string, params interface{}) ([]byte, error) {
		if method == (proto.TargetGetTargets{}).ProtoReq() {
			return nil, errors.New("targets")
		}
		return saved(ctx, session, method, params)
	}
	if pp.reset(hpPage(b)) {
		t.Fatal("a failed popup close is not a reset")
	}
}
