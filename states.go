package rod

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/go-rod/rod/lib/proto"
)

type stateKey struct {
	browserContextID proto.BrowserBrowserContextID
	sessionID        proto.TargetSessionID
	methodName       string
}

func (b *Browser) key(sessionID proto.TargetSessionID, methodName string) stateKey {
	return stateKey{
		browserContextID: b.BrowserContextID,
		sessionID:        sessionID,
		methodName:       methodName,
	}
}

func keepState(methodName string) bool {
	switch {
	case strings.HasSuffix(methodName, ".enable"), strings.HasSuffix(methodName, ".disable"):
		return true
	case strings.Contains(methodName, "setDeviceMetricsOverride"),
		strings.Contains(methodName, "setDownloadBehavior"),
		strings.Contains(methodName, "setExtraHTTPHeaders"),
		strings.Contains(methodName, "setUserAgentOverride"),
		strings.Contains(methodName, "setGeolocationOverride"),
		strings.Contains(methodName, "setTouchEmulationEnabled"),
		strings.Contains(methodName, "Emulation.clear"):
		return true
	default:
		return false
	}
}

func (b *Browser) set(sessionID proto.TargetSessionID, methodName string, params any) {
	if !keepState(methodName) {
		return
	}
	b.states.Store(b.key(sessionID, methodName), params)

	key := ""
	switch methodName {
	case (proto.EmulationClearDeviceMetricsOverride{}).ProtoReq():
		key = (proto.EmulationSetDeviceMetricsOverride{}).ProtoReq()
	case (proto.EmulationClearGeolocationOverride{}).ProtoReq():
		key = (proto.EmulationSetGeolocationOverride{}).ProtoReq()
	default:
		domain, name := proto.ParseMethodName(methodName)
		if name == "disable" {
			key = domain + ".enable"
		}
	}
	if key != "" {
		b.states.Delete(b.key(sessionID, key))
	}
}

// LoadState into the method, sessionID can be empty.
func (b *Browser) LoadState(sessionID proto.TargetSessionID, method proto.Request) (has bool) {
	data, has := b.states.Load(b.key(sessionID, method.ProtoReq()))
	if has {
		reflect.Indirect(reflect.ValueOf(method)).Set(
			reflect.Indirect(reflect.ValueOf(data)),
		)
	}
	return
}

// RemoveState a state.
func (b *Browser) RemoveState(key any) {
	b.states.Delete(key)
}

// EnableDomain and returns a restore function to restore previous state.
func (b *Browser) EnableDomain(sessionID proto.TargetSessionID, req proto.Request) (restore func()) {
	key := b.key(sessionID, req.ProtoReq())

	b.domainMu.Lock()
	if b.domains == nil {
		b.domains = map[stateKey]int{}
	}
	n := b.domains[key]
	if n == 0 {
		if _, enabled := b.states.Load(key); enabled {
			n = 1
		}
	}
	n++
	b.domains[key] = n
	shouldEnable := n == 1
	b.domainMu.Unlock()

	if shouldEnable {
		_, _ = b.Call(b.ctx, string(sessionID), req.ProtoReq(), req)
	}

	return func() {
		b.domainMu.Lock()
		n := max(b.domains[key]-1, 0)
		b.domains[key] = n
		shouldDisable := n == 0
		b.domainMu.Unlock()
		if shouldDisable {
			domain, _ := proto.ParseMethodName(req.ProtoReq())
			_, _ = b.Call(b.ctx, string(sessionID), domain+".disable", nil)
		}
	}
}

// DisableDomain and returns a restore function to restore previous state.
func (b *Browser) DisableDomain(sessionID proto.TargetSessionID, req proto.Request) (restore func()) {
	_, enabled := b.states.Load(b.key(sessionID, req.ProtoReq()))
	domain, _ := proto.ParseMethodName(req.ProtoReq())

	if enabled {
		_, _ = b.Call(b.ctx, string(sessionID), domain+".disable", nil)
	}

	return func() {
		if enabled {
			_, _ = b.Call(b.ctx, string(sessionID), req.ProtoReq(), req)
		}
	}
}

type sessionPageKey proto.TargetSessionID

func (b *Browser) cachePage(page *Page) {
	b.states.Store(page.TargetID, page)
	b.states.Store(sessionPageKey(page.SessionID), page)
}

// targetGone cancels the page owning a detached session or destroyed target
// and drops its cached states.
func (b *Browser) targetGone(method string, params []byte) {
	var key any
	switch method {
	case "Target.detachedFromTarget":
		var e proto.TargetDetachedFromTarget
		if json.Unmarshal(params, &e) != nil || e.SessionID == "" {
			return
		}
		key = sessionPageKey(e.SessionID)
	case "Target.targetDestroyed":
		var e proto.TargetTargetDestroyed
		if json.Unmarshal(params, &e) != nil {
			return
		}
		key = e.TargetID
	default:
		return
	}
	cache, ok := b.states.Load(key)
	if !ok {
		return
	}
	page := cache.(*Page) //nolint: forcetypeassert
	if page.sessionCancel != nil {
		page.sessionCancel()
	}
	page.cleanupStates()
}

func (b *Browser) loadCachedPage(id proto.TargetTargetID) *Page {
	if cache, ok := b.states.Load(id); ok {
		return cache.(*Page) //nolint: forcetypeassert
	}
	return nil
}

// LoadState into the method.
func (p *Page) LoadState(method proto.Request) (has bool) {
	return p.browser.LoadState(p.SessionID, method)
}

// EnableDomain and returns a restore function to restore previous state.
func (p *Page) EnableDomain(method proto.Request) (restore func()) {
	return p.browser.Context(p.ctx).EnableDomain(p.SessionID, method)
}

// DisableDomain and returns a restore function to restore previous state.
func (p *Page) DisableDomain(method proto.Request) (restore func()) {
	return p.browser.Context(p.ctx).DisableDomain(p.SessionID, method)
}

func (p *Page) cleanupStates() {
	cache, ok := p.browser.states.Load(p.TargetID)
	if ok && cache.(*Page).SessionID == p.SessionID { //nolint: forcetypeassert
		p.browser.RemoveState(p.TargetID)
	}
	p.browser.RemoveState(sessionPageKey(p.SessionID))
	p.browser.purgeSession(p.SessionID)
}

func (b *Browser) purgeSession(sessionID proto.TargetSessionID) {
	b.states.Range(func(k, _ any) bool {
		key, ok := k.(stateKey)
		if ok && key.sessionID == sessionID {
			b.states.Delete(k)
		}
		return true
	})
	b.domainMu.Lock()
	for key := range b.domains {
		if key.sessionID == sessionID {
			delete(b.domains, key)
		}
	}
	b.domainMu.Unlock()
}
