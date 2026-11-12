//go:generate go run ./lib/utils/setup
//go:generate go run ./lib/proto/generate
//go:generate go run ./lib/js/generate
//go:generate go run ./lib/assets/generate
//go:generate go run ./lib/utils/lint

// Package rod is a high-level driver directly based on DevTools Protocol.
package rod

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/defaults"
	"github.com/go-rod/rod/lib/devices"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/rod/lib/utils"
)

// Browser implements these interfaces.
var (
	_ proto.Client      = &Browser{}
	_ proto.Contextable = &Browser{}
)

// Browser represents the browser.
// It doesn't depends on file system, it should work with remote browser seamlessly.
// To check the env var you can use to quickly enable options from CLI, check here:
// https://pkg.go.dev/github.com/go-rod/rod/lib/defaults
type Browser struct {
	// BrowserContextID is the id for incognito window
	BrowserContextID proto.BrowserBrowserContextID

	e eFunc

	ctx context.Context

	sleeper func() utils.Sleeper

	logger utils.Logger

	slowMotion time.Duration // see defaults.slow
	trace      bool          // see defaults.Trace
	monitor    string

	defaultDevice devices.Device

	// compliant skips the default device and aligns the user agent with the
	// browser. The launcher must also be started with [launcher.Launcher.Compliant].
	compliant bool
	profile   *compliantProfile

	controlURL  string
	client      CDPClient
	launcher    *launcher.Launcher // set when Connect started the browser itself
	router      *router            // all the browser events from cdp client
	eventQueue  int
	targetsLock *sync.Mutex
	domainMu    *sync.Mutex
	domains     map[stateKey]int
	autoAttach  *atomic.Bool

	// stores all the previous cdp call of same type. Browser doesn't have enough API
	// for us to retrieve all its internal states. This is an workaround to map them to local.
	// For example you can't use cdp API to get the current position of mouse.
	states *sync.Map
}

// New creates a controller.
// DefaultDevice to emulate is set to [devices.LaptopWithMDPIScreen].Landscape(), it will change the default
// user-agent and can make the actual view area smaller than the browser window on headful mode,
// you can use [Browser.NoDefaultDevice] to disable it.
func New() *Browser {
	return (&Browser{
		ctx:           context.Background(),
		sleeper:       DefaultSleeper,
		controlURL:    defaults.URL,
		slowMotion:    defaults.Slow,
		trace:         defaults.Trace,
		monitor:       defaults.Monitor,
		logger:        DefaultLogger,
		defaultDevice: devices.LaptopWithMDPIScreen.Landscape(),
		targetsLock:   &sync.Mutex{},
		domainMu:      &sync.Mutex{},
		domains:       map[stateKey]int{},
		autoAttach:    &atomic.Bool{},
		states:        &sync.Map{},
		profile:       &compliantProfile{},
	}).WithPanic(utils.Panic)
}

// Incognito creates a new incognito browser.
func (b *Browser) Incognito() (*Browser, error) {
	return b.IncognitoContext(b.ctx, BrowserContextOptions{})
}

// BrowserContextOptions configures an incognito browser context.
type BrowserContextOptions struct {
	ProxyServer     string
	ProxyBypassList string
	DisposeOnDetach bool
}

// IncognitoContext creates an incognito browser context. The call honours ctx.
func (b *Browser) IncognitoContext(ctx context.Context, opts BrowserContextOptions) (*Browser, error) {
	res, err := proto.TargetCreateBrowserContext{
		ProxyServer:     opts.ProxyServer,
		ProxyBypassList: opts.ProxyBypassList,
		DisposeOnDetach: opts.DisposeOnDetach,
	}.Call(b.Context(ctx))
	if err != nil {
		return nil, err
	}

	incognito := *b
	incognito.BrowserContextID = res.BrowserContextID
	return &incognito, nil
}

// Alive reports whether the browser still answers CDP.
func (b *Browser) Alive() error {
	_, err := proto.BrowserGetVersion{}.Call(b)
	return err
}

// AutoAttach turns on flattened auto-attach so cross-origin iframes and
// workers get their own sessions. New targets are paused until
// Runtime.runIfWaitingForDebugger, which initEvents sends for attached targets
// when this was enabled.
func (b *Browser) AutoAttach() error {
	b.autoAttach.Store(true)
	return proto.TargetSetAutoAttach{
		AutoAttach:             true,
		WaitForDebuggerOnStart: true,
		Flatten:                true,
	}.Call(b)
}

// ControlURL set the url to remote control browser.
func (b *Browser) ControlURL(url string) *Browser {
	b.controlURL = url
	return b
}

// SlowMotion set the delay for each control action, such as the simulation of the human inputs.
func (b *Browser) SlowMotion(delay time.Duration) *Browser {
	b.slowMotion = delay
	return b
}

// Trace enables/disables the visual tracing of the input actions on the page.
func (b *Browser) Trace(enable bool) *Browser {
	b.trace = enable
	return b
}

// Monitor address to listen if not empty. Shortcut for [Browser.ServeMonitor].
func (b *Browser) Monitor(url string) *Browser {
	b.monitor = url
	return b
}

// Logger overrides the default log functions for tracing.
func (b *Browser) Logger(l utils.Logger) *Browser {
	b.logger = l
	return b
}

// Client set the cdp client.
func (b *Browser) Client(c CDPClient) *Browser {
	b.client = c
	return b
}

// DefaultDevice sets the default device for new page to emulate in the future.
// Default is [devices.LaptopWithMDPIScreen].
// Set it to [devices.Clear] to disable it.
func (b *Browser) DefaultDevice(d devices.Device) *Browser {
	b.defaultDevice = d
	return b
}

// NoDefaultDevice is the same as [Browser.DefaultDevice](devices.Clear).
func (b *Browser) NoDefaultDevice() *Browser {
	return b.DefaultDevice(devices.Clear)
}

// Compliant makes pages report this browser's own user agent and client hints,
// with the HeadlessChrome token removed. It also skips the default device,
// which would force a Chrome 114 user agent. The browser has to be launched
// with [launcher.Launcher.Compliant], which turns navigator.webdriver off.
// Without that launch, the user agent is aligned and webdriver stays true.
func (b *Browser) Compliant(enable bool) *Browser {
	b.compliant = enable
	return b
}

// pipeSupported and newLauncher are variables so tests can cover the
// port-launch fallback on any platform.
var (
	pipeSupported = launcher.PipeSupported
	newLauncher   = launcher.New
)

// Connect to the browser and start to control it.
// If fails to connect, try to launch a local browser, if local browser not found try to download one.
//
// A local launch talks CDP over --remote-debugging-pipe, so no debugging port
// is opened and other local processes cannot reach the browser. Where pipes
// are not available (Windows), the browser is launched on a port instead.
func (b *Browser) Connect() error {
	if b.client == nil {
		// Pipe launches do not wait on the context, so check it here.
		if err := b.ctx.Err(); err != nil {
			return err
		}
		u := b.controlURL
		var l *launcher.Launcher
		if u == "" {
			l = newLauncher().Context(b.ctx)
			if b.compliant {
				l.Compliant(true)
			}
		}

		var c CDPClient
		if l != nil && pipeSupported() {
			pc, err := l.LaunchPipe()
			if err != nil {
				return err
			}
			c = pc
		} else {
			if l != nil {
				var err error
				u, err = l.Launch()
				if err != nil {
					return err
				}
			}
			cc, err := cdp.StartWithURL(b.ctx, u, nil)
			if err != nil {
				if l != nil {
					l.Kill()
					l.Cleanup()
				}
				return err
			}
			c = cc
		}
		b.launcher = l
		b.client = c
	} else if b.controlURL != "" {
		return errors.New("Browser.Client and Browser.ControlURL can't be set at the same time")
	}

	b.initEvents()

	if b.monitor != "" {
		launcher.Open(b.ServeMonitor(b.monitor))
	}

	if err := (proto.TargetSetDiscoverTargets{Discover: true}).Call(b); err != nil {
		if b.launcher != nil {
			b.abandonLaunch()
		}
		return err
	}
	return nil
}

// abandonLaunch tears down a browser that Connect started but could not
// finish connecting to. Closing the client also ends the event loop.
func (b *Browser) abandonLaunch() {
	if c, ok := b.client.(io.Closer); ok && c != nil {
		_ = c.Close()
	}
	b.launcher.Kill()
	b.launcher.Cleanup()
	b.client = nil
	b.launcher = nil
}

// Close the browser and, when this instance owns the connection, the CDP transport.
// A browser that Connect launched itself is also waited for and its profile
// directory removed.
func (b *Browser) Close() error {
	var err error
	if b.BrowserContextID == "" {
		err = proto.BrowserClose{}.Call(b)
		if c, ok := b.client.(io.Closer); ok && c != nil {
			_ = c.Close()
		}
		if b.launcher != nil {
			reapBrowser(b.launcher, BrowserExitTimeout)
		}
		return err
	}
	return proto.TargetDisposeBrowserContext{BrowserContextID: b.BrowserContextID}.Call(b)
}

// BrowserExitTimeout is how long Close waits for a browser that Connect
// launched to exit on its own before killing it.
var BrowserExitTimeout = 10 * time.Second

func reapBrowser(l *launcher.Launcher, timeout time.Duration) {
	reap(l.Cleanup, l.Kill, timeout)
}

// reap waits for cleanup, then kills and waits again after timeout.
func reap(cleanup, kill func(), timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		cleanup()
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		kill()
		<-done
	}
}

// Page creates a new browser tab. If opts.URL is empty, the default target will be "about:blank".
func (b *Browser) Page(opts proto.TargetCreateTarget) (p *Page, err error) {
	req := opts
	req.BrowserContextID = b.BrowserContextID
	req.URL = "about:blank"

	target, err := req.Call(b)
	if err != nil {
		return nil, err
	}
	defer func() {
		// If Navigate or PageFromTarget fails we should close the target to prevent leak.
		// Use a fresh timeout so a cancelled caller context still closes the target.
		if err != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = proto.TargetCloseTarget{TargetID: target.TargetID}.Call(b.Context(closeCtx))
		}
	}()

	p, err = b.PageFromTarget(target.TargetID)
	if err != nil {
		return
	}

	if opts.URL == "" {
		return
	}

	err = p.Navigate(opts.URL)

	return
}

// Pages retrieves all visible pages.
func (b *Browser) Pages() (Pages, error) {
	list, err := proto.TargetGetTargets{}.Call(b)
	if err != nil {
		return nil, err
	}

	pageList := Pages{}
	for _, target := range list.TargetInfos {
		if target.Type != proto.TargetTargetInfoTypePage {
			continue
		}

		page, err := b.PageFromTarget(target.TargetID)
		if err != nil {
			return nil, err
		}
		pageList = append(pageList, page)
	}

	return pageList, nil
}

// Call implements the [proto.Client] to call raw cdp interface directly.
func (b *Browser) Call(ctx context.Context, sessionID, methodName string, params interface{}) (res []byte, err error) {
	res, err = b.client.Call(ctx, sessionID, methodName, params)
	if err != nil {
		return nil, err
	}

	b.set(proto.TargetSessionID(sessionID), methodName, params)
	return
}

// PageFromSession is used for low-level debugging.
func (b *Browser) PageFromSession(sessionID proto.TargetSessionID) *Page {
	sessionCtx, cancel, cancelCause := b.sessionContext()
	page := &Page{
		e:             b.e,
		ctx:           sessionCtx,
		session:       sessionCtx,
		sessionCancel: cancel,
		cancelCause:   cancelCause,
		sleeper:       b.sleeper,
		browser:       b,
		SessionID:     sessionID,
		jsCtxLock:     &sync.Mutex{},
		jsCtxID:       new(proto.RuntimeRemoteObjectID),
		helpersLock:   &sync.Mutex{},
	}
	page.root = page
	page.newKeyboard().newMouse().newTouch()
	return page
}

// PageFromTarget gets or creates a Page instance.
func (b *Browser) PageFromTarget(targetID proto.TargetTargetID) (*Page, error) {
	if b.compliant {
		if err := b.ensureCompliant(); err != nil {
			return nil, err
		}
	}

	b.targetsLock.Lock()
	defer b.targetsLock.Unlock()

	page := b.loadCachedPage(targetID)
	if page != nil {
		return page, nil
	}

	session, err := proto.TargetAttachToTarget{
		TargetID: targetID,
		Flatten:  true, // if it's not set no response will return
	}.Call(b)
	if err != nil {
		return nil, err
	}

	sessionCtx, cancel, cancelCause := b.sessionContext()

	page = &Page{
		e:             b.e,
		ctx:           sessionCtx,
		session:       sessionCtx,
		sessionCancel: cancel,
		cancelCause:   cancelCause,
		sleeper:       b.sleeper,
		browser:       b,
		TargetID:      targetID,
		SessionID:     session.SessionID,
		FrameID:       proto.PageFrameID(targetID),
		jsCtxLock:     &sync.Mutex{},
		jsCtxID:       new(proto.RuntimeRemoteObjectID),
		helpersLock:   &sync.Mutex{},
	}

	page.root = page
	page.newKeyboard().newMouse().newTouch()

	if b.compliant {
		err = page.applyCompliant()
		if err != nil {
			return nil, err
		}
	} else if !b.defaultDevice.IsClear() {
		err = page.Emulate(b.defaultDevice)
		if err != nil {
			return nil, err
		}
	}

	b.cachePage(page)

	// If we don't enable it, it will cause a lot of unexpected browser behavior.
	// Such as proto.PageAddScriptToEvaluateOnNewDocument won't work.
	page.EnableDomain(&proto.PageEnable{})

	return page, nil
}

// EachEvent is similar to [Page.EachEvent], but catches events of the entire browser.
func (b *Browser) EachEvent(callbacks ...interface{}) (wait func()) {
	return b.eachEvent("", callbacks...)
}

// WaitEvent waits for the next event for one time. It will also load the data into the event object.
func (b *Browser) WaitEvent(e proto.Event) (wait func()) {
	return b.waitEvent("", e)
}

// waits for the next event for one time. It will also load the data into the event object.
func (b *Browser) waitEvent(sessionID proto.TargetSessionID, e proto.Event) (wait func()) {
	return b.eachEvent(sessionID, eventLoader(e))
}

// If the any callback returns true the event loop will stop.
// It will enable the related domains if not enabled, and restore them after wait ends.
func (b *Browser) eachEvent(sessionID proto.TargetSessionID, callbacks ...interface{}) (wait func()) {
	w := b.eachEventStop(sessionID, callbacks...)
	return func() { w() }
}

// eachEventStop is eachEvent whose wait reports whether a callback stopped it.
func (b *Browser) eachEventStop(sessionID proto.TargetSessionID, callbacks ...interface{}) (wait func() bool) {
	cbMap := map[string]reflect.Value{}
	restores := []func(){}

	for _, cb := range callbacks {
		cbVal := reflect.ValueOf(cb)
		eType := cbVal.Type().In(0)
		name := reflect.New(eType.Elem()).Interface().(proto.Event).ProtoEvent() //nolint: forcetypeassert
		cbMap[name] = cbVal

		// Only enabled domains will emit events to cdp client.
		// We enable the domains for the event types if it's not enabled.
		// We restore the domains to their previous states after the wait ends.
		domain, _ := proto.ParseMethodName(name)
		if req := proto.GetType(domain + ".enable"); req != nil {
			enable := reflect.New(req).Interface().(proto.Request) //nolint: forcetypeassert
			restores = append(restores, b.EnableDomain(sessionID, enable))
		}
	}

	methods := make(map[string]struct{}, len(cbMap))
	for name := range cbMap {
		methods[name] = struct{}{}
	}

	b, cancel := b.WithCancel()
	messages := b.listen(b.ctx, sessionID, methods)

	return func() bool {
		if messages == nil {
			return false
		}

		defer func() {
			cancel()
			messages = nil
			for _, restore := range restores {
				restore()
			}
		}()

		for msg := range messages {
			if cbVal, has := cbMap[msg.Method]; has {
				e := reflect.New(proto.GetType(msg.Method))
				msg.Load(e.Interface().(proto.Event)) //nolint: forcetypeassert
				args := []reflect.Value{e}
				if cbVal.Type().NumIn() == 2 {
					args = append(args, reflect.ValueOf(msg.SessionID))
				}
				res := cbVal.Call(args)
				if len(res) > 0 {
					if res[0].Bool() {
						return true
					}
				}
			}
		}
		return false
	}
}

// Event of the browser.
func (b *Browser) Event() <-chan *Message {
	return b.listen(b.ctx, "", nil)
}

// listen returns a channel of events. An empty sessionID means every session.
// When methods is non-nil only those methods are delivered. The channel is
// closed when ctx ends.
func (b *Browser) listen(ctx context.Context, sessionID proto.TargetSessionID, methods map[string]struct{}) <-chan *Message {
	if b.router == nil {
		ch := make(chan *Message)
		close(ch)
		return ch
	}
	return b.router.subscribe(ctx, sessionID, methods)
}

func (b *Browser) initEvents() {
	queue := b.eventQueue
	if queue == 0 {
		queue = DefaultEventQueue
	}
	b.router = newRouter(queue)
	if b.domains == nil {
		b.domains = map[stateKey]int{}
	}
	if b.domainMu == nil {
		b.domainMu = &sync.Mutex{}
	}
	if b.autoAttach == nil {
		b.autoAttach = &atomic.Bool{}
	}
	client := b.client
	event := client.Event()

	go func() {
		for e := range event {
			msg := &Message{
				SessionID: proto.TargetSessionID(e.SessionID),
				Method:    e.Method,
				lock:      &sync.Mutex{},
				data:      e.Params,
			}
			switch e.Method {
			case "Target.attachedToTarget":
				if b.autoAttach.Load() {
					go b.resumeAttached(e.Params)
				}
			case "Target.detachedFromTarget", "Target.targetDestroyed":
				b.targetGone(e.Method, e.Params)
			}
			b.router.publish(msg)
		}
		cause := errOf(client)
		if cause == nil {
			cause = cdp.ErrConnClosed
		}
		b.cancelPages(cause)
	}()
}

func (b *Browser) pageInfo(id proto.TargetTargetID) (*proto.TargetTargetInfo, error) {
	res, err := proto.TargetGetTargetInfo{TargetID: id}.Call(b)
	if err != nil {
		return nil, err
	}
	return res.TargetInfo, nil
}

func (b *Browser) isHeadless() (enabled bool) {
	res, _ := proto.BrowserGetBrowserCommandLine{}.Call(b)
	for _, v := range res.Arguments {
		if strings.Contains(v, "headless") {
			return true
		}
	}
	return false
}

// IgnoreCertErrors switch. If enabled, all certificate errors will be ignored.
func (b *Browser) IgnoreCertErrors(enable bool) error {
	return proto.SecuritySetIgnoreCertificateErrors{Ignore: enable}.Call(b)
}

// GetCookies from the browser.
func (b *Browser) GetCookies() ([]*proto.NetworkCookie, error) {
	res, err := proto.StorageGetCookies{BrowserContextID: b.BrowserContextID}.Call(b)
	if err != nil {
		return nil, err
	}
	return res.Cookies, nil
}

// SetCookies to the browser. If the cookies is nil it will clear all the cookies.
func (b *Browser) SetCookies(cookies []*proto.NetworkCookieParam) error {
	if cookies == nil {
		return proto.StorageClearCookies{BrowserContextID: b.BrowserContextID}.Call(b)
	}

	return proto.StorageSetCookies{
		Cookies:          cookies,
		BrowserContextID: b.BrowserContextID,
	}.Call(b)
}

// WaitDownload returns a helper to get the next download file.
// The file path will be:
//
//	filepath.Join(dir, info.GUID)
func (b *Browser) WaitDownload(dir string) func() (info *proto.PageDownloadWillBegin) {
	var oldDownloadBehavior proto.BrowserSetDownloadBehavior
	has := b.LoadState("", &oldDownloadBehavior)

	_ = proto.BrowserSetDownloadBehavior{
		Behavior:         proto.BrowserSetDownloadBehaviorBehaviorAllowAndName,
		BrowserContextID: b.BrowserContextID,
		DownloadPath:     dir,
	}.Call(b)

	var start *proto.PageDownloadWillBegin

	waitProgress := b.EachEvent(func(e *proto.PageDownloadWillBegin) {
		start = e
	}, func(e *proto.PageDownloadProgress) bool {
		return start != nil && start.GUID == e.GUID && e.State == proto.PageDownloadProgressStateCompleted
	})

	return func() *proto.PageDownloadWillBegin {
		defer func() {
			if has {
				_ = oldDownloadBehavior.Call(b)
			} else {
				_ = proto.BrowserSetDownloadBehavior{
					Behavior:         proto.BrowserSetDownloadBehaviorBehaviorDefault,
					BrowserContextID: b.BrowserContextID,
				}.Call(b)
			}
		}()

		waitProgress()

		return start
	}
}

// Version info of the browser.
func (b *Browser) Version() (*proto.BrowserGetVersionResult, error) {
	return proto.BrowserGetVersion{}.Call(b)
}
