package rod

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

// fakeClient is a CDP client for the branches that a real browser cannot
// easily reach.
type fakeClient struct {
	events chan *cdp.Event
	call   func(ctx context.Context, sessionID, method string, params interface{}) ([]byte, error)
	err    error
	closed bool
}

func newFakeClient() *fakeClient {
	return &fakeClient{events: make(chan *cdp.Event, 16)}
}

func (c *fakeClient) Event() <-chan *cdp.Event { return c.events }

func (c *fakeClient) Call(ctx context.Context, sessionID, method string, params interface{}) ([]byte, error) {
	if c.call != nil {
		return c.call(ctx, sessionID, method, params)
	}
	return []byte("{}"), nil
}

func (c *fakeClient) Close() error {
	c.closed = true
	close(c.events)
	return nil
}

func (c *fakeClient) Done() <-chan struct{} { return nil }

func (c *fakeClient) Err() error { return c.err }

// reply is a fake call that answers the given methods with raw JSON.
func reply(m map[string]string) func(context.Context, string, string, interface{}) ([]byte, error) {
	return func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if s, ok := m[method]; ok {
			return []byte(s), nil
		}
		return []byte("{}"), nil
	}
}

func TestFakeConnect(t *testing.T) {
	c := newFakeClient()
	c.call = reply(map[string]string{
		proto.BrowserGetVersion{}.ProtoReq(): `{"product":"x"}`,
	})
	b := New().Client(c)
	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := b.Alive(); err != nil {
		t.Fatal(err)
	}
	if err := b.AutoAttach(); err != nil {
		t.Fatal(err)
	}
	if b.DroppedEvents() != 0 {
		t.Fatal("dropped events")
	}
	if b.EventQueue(-1) != b {
		t.Fatal("EventQueue must return the browser")
	}
	if b.Disconnected() != nil {
		t.Fatal("Disconnected must be nil for this client")
	}
	if b.Err() != nil {
		t.Fatal(b.Err())
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if !c.closed {
		t.Fatal("client not closed")
	}
}

func TestResumeAttached(t *testing.T) {
	called := make(chan string, 1)
	c := newFakeClient()
	c.call = func(_ context.Context, sessionID, method string, _ interface{}) ([]byte, error) {
		if method == "Runtime.runIfWaitingForDebugger" {
			called <- sessionID + "/" + method
		}
		return []byte("{}"), nil
	}
	b := New().Client(c)

	// Every early return.
	b.resumeAttached([]byte(`{`))
	b.resumeAttached([]byte(`{"sessionId":"s"}`))
	b.resumeAttached([]byte(`{"waitingForDebugger":true}`))
	select {
	case got := <-called:
		t.Fatalf("called %s", got)
	default:
	}

	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}
	b.autoAttach.Store(true)
	c.events <- &cdp.Event{
		Method: "Target.attachedToTarget",
		Params: []byte(`{"sessionId":"s1","waitingForDebugger":true}`),
	}
	select {
	case got := <-called:
		if got != "s1/Runtime.runIfWaitingForDebugger" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not called")
	}
	_ = b.Close()
}

func TestFramePage(t *testing.T) {
	c := newFakeClient()
	b := New().Client(c)

	// Not an iframe target.
	c.call = reply(map[string]string{
		proto.TargetGetTargetInfo{}.ProtoReq(): `{"targetInfo":{"type":"page"}}`,
	})
	if p, err := b.framePage("t"); p != nil || err != nil {
		t.Fatalf("got %v %v", p, err)
	}

	// A cached page is returned without attaching.
	cached := &Page{TargetID: "t"}
	b.states.Store(proto.TargetTargetID("t"), cached)
	c.call = reply(map[string]string{
		proto.TargetGetTargetInfo{}.ProtoReq(): `{"targetInfo":{"type":"iframe"}}`,
	})
	if p, err := b.framePage("t"); p != cached || err != nil {
		t.Fatalf("got %v %v", p, err)
	}
	b.states.Delete(proto.TargetTargetID("t"))

	// A new iframe target is attached.
	c.call = reply(map[string]string{
		proto.TargetGetTargetInfo{}.ProtoReq():  `{"targetInfo":{"type":"iframe"}}`,
		proto.TargetAttachToTarget{}.ProtoReq(): `{"sessionId":"s"}`,
	})
	p, err := b.framePage("t2")
	if err != nil {
		t.Fatal(err)
	}
	if p.SessionID != "s" || p.TargetID != "t2" {
		t.Fatalf("page %+v", p)
	}

	// The info call can fail.
	c.call = func(context.Context, string, string, interface{}) ([]byte, error) {
		return nil, errors.New("boom")
	}
	if _, err := b.framePage("t3"); err != nil {
		t.Fatal(err)
	}

	// Attaching can fail.
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.TargetGetTargetInfo{}).ProtoReq() {
			return []byte(`{"targetInfo":{"type":"iframe"}}`), nil
		}
		return nil, errors.New("attach failed")
	}
	if _, err := b.framePage("t4"); err == nil {
		t.Fatal("expected the attach error")
	}
}

func TestTargetGoneBranches(t *testing.T) {
	b := New()

	b.targetGone("Target.detachedFromTarget", []byte(`{`))
	b.targetGone("Target.detachedFromTarget", []byte(`{"sessionId":""}`))
	b.targetGone("Target.targetDestroyed", []byte(`{`))
	b.targetGone("Other.event", nil)

	canceled := false
	p := &Page{TargetID: "t", SessionID: "s", browser: b, sessionCancel: func() { canceled = true }}
	b.cachePage(p)
	b.targetGone("Target.targetDestroyed", []byte(`{"targetId":"t"}`))
	if !canceled {
		t.Fatal("the page was not canceled")
	}

	// Nothing cached for the session.
	b.targetGone("Target.detachedFromTarget", []byte(`{"sessionId":"nope"}`))
}

func TestEnableDomainNilDomains(t *testing.T) {
	b := New().Client(newFakeClient())
	b.domains = nil
	restore := b.EnableDomain("", proto.PageEnable{})
	// Calling restore more often than enable must not go negative.
	restore()
	restore()
	if n := b.domains[b.key("", (proto.PageEnable{}).ProtoReq())]; n != 0 {
		t.Fatalf("count %d", n)
	}
}

func TestListenNilRouter(t *testing.T) {
	b := New()
	ch := b.Event()
	if _, ok := <-ch; ok {
		t.Fatal("expected a closed channel")
	}
	if b.DroppedEvents() != 0 {
		t.Fatal("dropped events")
	}
}

func TestTargetClosedErrorText(t *testing.T) {
	e := &targetClosedError{}
	if e.Error() != ErrTargetClosed.Error() {
		t.Fatalf("got %q", e.Error())
	}
	e = &targetClosedError{cause: errors.New("boom")}
	if !strings.Contains(e.Error(), "boom") {
		t.Fatalf("got %q", e.Error())
	}
	if len(e.Unwrap()) != 3 {
		t.Fatal("unwrap")
	}
}

func TestPageNilSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Page{ctx: ctx}
	cancel()
	select {
	case <-p.Done():
	default:
		t.Fatal("Done must follow ctx")
	}
	if !errors.Is(p.Err(), context.Canceled) {
		t.Fatalf("got %v", p.Err())
	}
	err := errors.New("x")
	if p.translate(err) != err {
		t.Fatal("translate must keep the error")
	}
}

func TestTranslateEndsWithSession(t *testing.T) {
	session, cancel := context.WithCancelCause(context.Background())
	p := &Page{ctx: context.Background(), session: session}
	cancel(ErrTargetClosed)
	err := p.translate(context.Canceled)
	if !errors.Is(err, ErrTargetClosed) {
		t.Fatalf("got %v", err)
	}
	// An already typed error is kept.
	if p.translate(err) != err {
		t.Fatal("typed error replaced")
	}
	// A non-cancel error is kept.
	if p.translate(errors.New("x")) == nil {
		t.Fatal("nil")
	}
}

func TestMergeContextSessionErr(t *testing.T) {
	session, cancelSession := context.WithCancelCause(context.Background())
	pageCtx, cancelPage := context.WithCancelCause(context.Background())
	p := &Page{ctx: pageCtx, session: session}
	merged, release := p.mergeContext(context.Background())
	defer release()
	cancelSession(ErrTargetClosed)
	cancelPage(nil)
	select {
	case <-merged.Done():
	case <-time.After(time.Second):
		t.Fatal("merged context did not end")
	}
	if !errors.Is(context.Cause(merged), ErrTargetClosed) {
		t.Fatalf("got %v", context.Cause(merged))
	}
}

func TestMergeContextPageEnds(t *testing.T) {
	pageCtx, cancelPage := context.WithCancelCause(context.Background())
	p := &Page{ctx: pageCtx, session: context.Background()}
	merged, release := p.mergeContext(context.Background())
	defer release()
	cancelPage(ErrTargetClosed)
	select {
	case <-merged.Done():
	case <-time.After(time.Second):
		t.Fatal("merged context did not end")
	}
	if !errors.Is(context.Cause(merged), ErrTargetClosed) {
		t.Fatalf("got %v", context.Cause(merged))
	}
}

func TestCompliantErrors(t *testing.T) {
	b := New()
	b.profile = nil
	c := newFakeClient()
	c.call = func(context.Context, string, string, interface{}) ([]byte, error) {
		return nil, errors.New("boom")
	}
	b.Client(c)
	if err := b.ensureCompliant(); err == nil {
		t.Fatal("expected the profile error")
	}
	if err := b.profile.err; err == nil {
		t.Fatal("error not cached")
	}

	// The page reports the cached error.
	p := &Page{browser: b}
	if err := p.applyCompliant(); err == nil {
		t.Fatal("expected the cached error")
	}

	// A profile that was never read.
	p.browser = &Browser{profile: &compliantProfile{}}
	if err := p.applyCompliant(); err == nil || !strings.Contains(err.Error(), "not read") {
		t.Fatalf("got %v", err)
	}

	// A profile without a pointer is read on demand.
	p.browser = &Browser{}
	if err := p.applyCompliant(); err == nil {
		t.Fatal("expected the read error")
	}
}

func TestApplyCompliantSuccess(t *testing.T) {
	c := newFakeClient()
	var called bool
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.EmulationSetUserAgentOverride{}).ProtoReq() {
			called = true
		}
		return []byte("{}"), nil
	}
	b := New().Client(c)
	b.profile = &compliantProfile{
		ua:   "ua",
		meta: &proto.EmulationUserAgentMetadata{},
	}
	p := &Page{browser: b}
	if err := p.applyCompliant(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("no user agent override")
	}
}

func TestCompliantFromValueErrors(t *testing.T) {
	if _, _, err := compliantFromValue(gson.New(make(chan int))); err == nil {
		t.Fatal("expected the marshal error")
	}
	if _, _, err := compliantFromEval("{"); err == nil {
		t.Fatal("expected the unmarshal error")
	}
	if ua, meta, err := compliantFromEval(`{"ua":""}`); err != nil || meta != nil || ua != "" {
		t.Fatalf("got %q %v %v", ua, meta, err)
	}
}

func TestReadCompliantProfileRetries(t *testing.T) {
	evals := []string{
		``, // call error
		`{"result":{"value":"x"},"exceptionDetails":{"text":"bad"}}`,
		`{"result":{"value":"{\"ua\":\"x\"}"}}`,
		`{"result":{"value":"{\"ua\":\"Mozilla HeadlessChrome\",\"hints\":{\"brands\":[{\"brand\":\"HeadlessChrome\",\"version\":\"1\"}]}}"}}`,
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
			if evals[i-1] == "" {
				return nil, errors.New("eval")
			}
			return []byte(evals[i-1]), nil
		}
		return []byte("{}"), nil
	}
	b := New().Client(c)
	b.profile = nil
	if err := b.ensureCompliant(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.profile.ua, "Chrome") || strings.Contains(b.profile.ua, "Headless") {
		t.Fatalf("ua %q", b.profile.ua)
	}
}

func TestReadCompliantProfileCreateError(t *testing.T) {
	c := newFakeClient()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		if method == (proto.TargetCreateTarget{}).ProtoReq() {
			return nil, errors.New("create failed")
		}
		return []byte("{}"), nil
	}
	b := New().Client(c)
	if _, _, err := b.readCompliantProfile(); err == nil {
		t.Fatal("expected the create error")
	}
}

func TestReadCompliantProfileAttachError(t *testing.T) {
	c := newFakeClient()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		switch method {
		case (proto.TargetCreateTarget{}).ProtoReq():
			return []byte(`{"targetId":"t"}`), nil
		case (proto.TargetAttachToTarget{}).ProtoReq():
			return nil, errors.New("attach failed")
		}
		return []byte("{}"), nil
	}
	b := New().Client(c)
	if _, _, err := b.readCompliantProfile(); err == nil {
		t.Fatal("expected the attach error")
	}
}

func TestReadCompliantProfileGivesUp(t *testing.T) {
	c := newFakeClient()
	c.call = func(_ context.Context, _, method string, _ interface{}) ([]byte, error) {
		switch method {
		case (proto.TargetCreateTarget{}).ProtoReq():
			return []byte(`{"targetId":"t"}`), nil
		case (proto.TargetAttachToTarget{}).ProtoReq():
			return []byte(`{"sessionId":"s"}`), nil
		}
		return nil, errors.New("eval")
	}
	b := New().Client(c)
	old := compliantRetryDelay
	compliantRetryDelay = 0
	defer func() { compliantRetryDelay = old }()
	if _, _, err := b.readCompliantProfile(); err == nil {
		t.Fatal("expected the retry error")
	}
}

func TestReapTimeout(t *testing.T) {
	release := make(chan struct{})
	var killed bool
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	reap(func() { <-release }, func() { killed = true }, 10*time.Millisecond)
	if !killed {
		t.Fatal("the cleanup was not killed after the timeout")
	}
}

func TestReapWithoutTimeout(t *testing.T) {
	reap(func() {}, func() { t.Fatal("killed") }, time.Second)
}
