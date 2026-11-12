package rod

import (
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// DefaultHealthPagePoolTimeout bounds each health check and reset of a pooled page.
var DefaultHealthPagePoolTimeout = 5 * time.Second

// HealthPagePool hands out up to a fixed number of pages from one browser and takes
// them back for reuse.
//
// A page is only handed out after it passes a health check: the browser still
// answers, the target still exists, and its renderer still runs script. Pages
// that fail are closed and their slot is freed, so the next Get creates a
// fresh page. A returned page is navigated to about:blank and any popups it
// opened are closed before it goes back into the pool.
//
// Reset clears the page's document and its popups, not the browser profile.
// Cookies and storage of the origin the page visited are kept. Use a separate
// browser context if that state must not carry over.
type HealthPagePool struct {
	browser *Browser
	slots   Pool[Page]
	timeout time.Duration
}

// NewHealthPagePool creates a pool of at most size pages on browser b. Pages are
// created on the first Get that needs them, not up front.
func NewHealthPagePool(b *Browser, size int) *HealthPagePool {
	return &HealthPagePool{
		browser: b,
		slots:   NewPool[Page](size),
		timeout: DefaultHealthPagePoolTimeout,
	}
}

// Get returns a healthy page at about:blank. It blocks while every slot is in
// use. The page must be returned with Put.
func (pp *HealthPagePool) Get() (*Page, error) {
	for {
		p, err := pp.slots.Get(pp.create)
		if err != nil {
			// The slot was consumed by the failed create. Give it back.
			pp.slots.Put(nil)
			return nil, err
		}
		if pp.healthy(p) {
			return p, nil
		}
		pp.discard(p)
	}
}

// Put returns a page obtained from Get. The page is reset and checked before
// it is reused. A page that cannot be reset is closed instead. Put with nil
// releases the slot without a page.
func (pp *HealthPagePool) Put(p *Page) {
	if p == nil {
		pp.slots.Put(nil)
		return
	}
	if !pp.reset(p) {
		pp.discard(p)
		return
	}
	pp.slots.Put(p)
}

// Cleanup closes every idle page in the pool. Pages that are currently handed
// out are not touched.
func (pp *HealthPagePool) Cleanup() {
	pp.slots.Cleanup(func(p *Page) {
		_ = p.ForceClose(pp.timeout)
	})
}

func (pp *HealthPagePool) create() (*Page, error) {
	return pp.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
}

// discard closes a page that failed a check and frees its slot.
func (pp *HealthPagePool) discard(p *Page) {
	_ = p.ForceClose(pp.timeout)
	pp.slots.Put(nil)
}

// healthy reports whether p can be handed out right now.
func (pp *HealthPagePool) healthy(p *Page) bool {
	if _, err := pp.browser.Timeout(pp.timeout).Version(); err != nil {
		return false
	}
	info, err := p.Timeout(pp.timeout).Info()
	if err != nil || info.Type != proto.TargetTargetInfoTypePage {
		return false
	}
	_, err = p.Timeout(pp.timeout).Eval(`() => 1`)
	return err == nil
}

// reset navigates p back to about:blank and closes popups it opened. It
// reports whether p is usable afterwards.
func (pp *HealthPagePool) reset(p *Page) bool {
	if !pp.healthy(p) {
		return false
	}

	if err := pp.closePopups(p); err != nil {
		return false
	}

	t := p.Timeout(pp.timeout)
	if err := t.Navigate("about:blank"); err != nil {
		return false
	}
	if err := t.WaitLoad(); err != nil {
		return false
	}
	info, err := t.Info()
	if err != nil || info.URL != "about:blank" {
		return false
	}
	return pp.healthy(p)
}

// closePopups closes pages whose opener is p.
func (pp *HealthPagePool) closePopups(p *Page) error {
	res, err := proto.TargetGetTargets{}.Call(pp.browser.Timeout(pp.timeout))
	if err != nil {
		return err
	}
	for _, info := range res.TargetInfos {
		if info.OpenerID != p.TargetID || info.Type != proto.TargetTargetInfoTypePage {
			continue
		}
		if _, err := (proto.TargetCloseTarget{TargetID: info.TargetID}).Call(pp.browser.Timeout(pp.timeout)); err != nil {
			return err
		}
	}
	return nil
}
