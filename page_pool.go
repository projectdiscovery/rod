package rod

import (
	"context"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// HealthyPages is a fixed set of pages on one browser. Get checks that a
// page still answers and resets it to about:blank before handing it out.
// A page that fails the check is closed and replaced.
type HealthyPages struct {
	browser *Browser
	ch      chan *Page
}

// NewHealthyPages opens n pages. n must be greater than zero.
func (b *Browser) NewHealthyPages(n int) (*HealthyPages, error) {
	if n < 1 {
		n = 1
	}
	h := &HealthyPages{browser: b, ch: make(chan *Page, n)}
	for i := 0; i < n; i++ {
		p, err := b.Page(proto.TargetCreateTarget{URL: "about:blank"})
		if err != nil {
			h.Close()
			return nil, err
		}
		h.ch <- p
	}
	return h, nil
}

// Get returns a page that answered a short liveness check.
func (h *HealthyPages) Get(ctx context.Context) (*Page, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case p := <-h.ch:
		if err := p.Context(ctx).Navigate("about:blank"); err != nil {
			_ = p.ForceClose(2 * time.Second)
			np, nerr := h.browser.Context(ctx).Page(proto.TargetCreateTarget{URL: "about:blank"})
			if nerr != nil {
				return nil, nerr
			}
			return np, nil
		}
		return p, nil
	}
}

// Put returns a page to the pool. A nil page refills the slot.
func (h *HealthyPages) Put(p *Page) {
	if p == nil {
		np, err := h.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
		if err != nil {
			return
		}
		p = np
	}
	h.ch <- p
}

// Close closes every page still in the pool.
func (h *HealthyPages) Close() {
	close(h.ch)
	for p := range h.ch {
		if p != nil {
			_ = p.ForceClose(2 * time.Second)
		}
	}
}
