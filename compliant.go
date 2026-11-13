package rod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/projectdiscovery/rod/lib/proto"
	"github.com/ysmood/gson"
)

// errCompliantPending means the probe still shows its first document. The user
// agent may already be known. If the navigation never commits, the caller
// uses the user agent it already saw.
var errCompliantPending = errors.New("compliant profile pending")

const compliantHintJS = `(() => {
	const ua = navigator.userAgent
	// The first document is about:blank and has no userAgentData. Reading it
	// then would stick the browser with no client hints. Wait until the
	// navigation commits. Full Chrome commits chrome://version. Headless shell
	// has no such page and commits chrome-error://chromewebdata, which never
	// becomes chrome://version. In both cases userAgentData is then final.
	if (location.protocol === "about:" || document.readyState === "loading") {
		return JSON.stringify({ua, pending: true})
	}
	if (!navigator.userAgentData) return JSON.stringify({ua})
	return navigator.userAgentData.getHighEntropyValues([
		"brands", "fullVersionList", "platform", "platformVersion",
		"architecture", "bitness", "model", "wow64", "formFactors"
	]).then(hints => JSON.stringify({ua, hints}))
})()`

// compliantProfile is the browser's own client hints, read once.
type compliantProfile struct {
	// lock holds one token while a read runs. A channel lets a waiter give up
	// when its context ends.
	lock chan struct{}
	done bool
	ua   string
	meta *proto.EmulationUserAgentMetadata
	err  error
}

func newCompliantProfile() *compliantProfile {
	return &compliantProfile{lock: make(chan struct{}, 1)}
}

// ensureCompliant reads this browser's own client hints once. A read that
// stops because the caller's context ended is not kept. The next caller with
// a live context reads again. A caller that waits for another read stops
// when its own context ends.
func (b *Browser) ensureCompliant() error {
	if b.profile == nil {
		b.profile = newCompliantProfile()
	}
	p := b.profile
	select {
	case p.lock <- struct{}{}:
	case <-b.ctx.Done():
		return b.ctx.Err()
	}
	defer func() { <-p.lock }()
	if p.done {
		return p.err
	}
	ua, meta, err := b.readCompliantProfile()
	if err != nil && b.ctx.Err() != nil {
		return err
	}
	p.ua, p.meta, p.err = ua, meta, err
	p.done = true
	return err
}

func (p *Page) applyCompliant() error {
	if p.browser.profile == nil || p.browser.profile.ua == "" {
		if p.browser.profile != nil && p.browser.profile.err != nil {
			return p.browser.profile.err
		}
		return fmt.Errorf("compliant profile was not read")
	}
	// Some builds, such as chrome-headless-shell, expose no client hints. The
	// user agent alone is still aligned, and the metadata stays unset.
	return proto.EmulationSetUserAgentOverride{
		UserAgent:         p.browser.profile.ua,
		UserAgentMetadata: p.browser.profile.meta,
	}.Call(p)
}

// readCompliantProfile opens chrome://version, which is a secure context, so
// the browser's own client hints are available. about:blank does not expose them.
func (b *Browser) readCompliantProfile() (string, *proto.EmulationUserAgentMetadata, error) {
	target, err := proto.TargetCreateTarget{URL: "chrome://version"}.Call(b)
	if err != nil {
		return "", nil, err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = proto.TargetCloseTarget{TargetID: target.TargetID}.Call(b.Context(ctx))
	}()

	session, err := proto.TargetAttachToTarget{TargetID: target.TargetID, Flatten: true}.Call(b)
	if err != nil {
		return "", nil, err
	}
	// The probe session must not outlive the read. Its enable state would
	// otherwise stay in the browser's state table.
	defer func() {
		b.purgeSession(session.SessionID)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proto.TargetDetachFromTarget{SessionID: session.SessionID}.Call(b.Context(ctx))
	}()
	probe := &Page{
		ctx:       b.ctx,
		session:   b.ctx,
		browser:   b,
		SessionID: session.SessionID,
	}
	_ = proto.PageEnable{}.Call(probe)

	var last error
	var seenUA string
	for range 50 {
		res, err := proto.RuntimeEvaluate{
			Expression:    compliantHintJS,
			AwaitPromise:  true,
			ReturnByValue: true,
		}.Call(probe)
		switch {
		case err != nil:
			if ctxErr := b.ctx.Err(); ctxErr != nil {
				return "", nil, ctxErr
			}
			last = err
		case res.ExceptionDetails != nil:
			last = fmt.Errorf("compliant profile: %s", res.ExceptionDetails.Text)
		default:
			ua, meta, err := compliantFromValue(res.Result.Value)
			switch {
			case errors.Is(err, errCompliantPending):
				if ua != "" {
					seenUA = ua
				}
				last = err
			case err != nil:
				last = fmt.Errorf("compliant profile: %w (%s)", err, res.Result.Value.Str())
			case meta != nil:
				return ua, meta, nil
			case ua != "":
				// The navigation has committed and this page has no client hints.
				return ua, nil, nil
			default:
				last = fmt.Errorf("compliant profile: client hints not ready (%s)", res.Result.Value.Str())
			}
		}
		select {
		case <-b.ctx.Done():
			return "", nil, b.ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	// The navigation did not commit. Keep the user agent the first document
	// reported and leave the metadata unset.
	if seenUA != "" {
		return seenUA, nil, nil
	}
	return "", nil, last
}

type compliantHints struct {
	UA      string `json:"ua"`
	Pending bool   `json:"pending"`
	Hints   *struct {
		Architecture    string                                 `json:"architecture"`
		Bitness         string                                 `json:"bitness"`
		Brands          []proto.EmulationUserAgentBrandVersion `json:"brands"`
		FullVersionList []proto.EmulationUserAgentBrandVersion `json:"fullVersionList"`
		Mobile          bool                                   `json:"mobile"`
		Model           string                                 `json:"model"`
		Platform        string                                 `json:"platform"`
		PlatformVersion string                                 `json:"platformVersion"`
		Wow64           bool                                   `json:"wow64"`
		FormFactors     []string                               `json:"formFactors"`
	} `json:"hints"`
}

// compliantFromValue turns the browser's own hints into the override. The user
// agent and brand lose the HeadlessChrome token. The rest is copied, so the
// platform and versions stay the ones this browser already reports.
func compliantFromValue(v gson.JSON) (string, *proto.EmulationUserAgentMetadata, error) {
	raw := v.Val()
	if s, ok := raw.(string); ok {
		return compliantFromEval(s)
	}
	bin, err := json.Marshal(raw)
	if err != nil {
		return "", nil, err
	}
	return compliantFromEval(string(bin))
}

// compliantFromEval parses the JSON form of [compliantFromValue].
func compliantFromEval(raw string) (string, *proto.EmulationUserAgentMetadata, error) {
	var parsed compliantHints
	err := json.Unmarshal([]byte(raw), &parsed)
	if err != nil {
		return "", nil, err
	}
	if parsed.Pending {
		return chromeUA(parsed.UA), nil, errCompliantPending
	}
	if parsed.UA == "" {
		return "", nil, nil
	}
	if parsed.Hints == nil {
		return chromeUA(parsed.UA), nil, nil
	}
	if len(parsed.Hints.Brands) == 0 {
		return "", nil, nil
	}
	brands := make([]*proto.EmulationUserAgentBrandVersion, len(parsed.Hints.Brands))
	for i, brand := range parsed.Hints.Brands {
		copied := brand
		copied.Brand = chromeBrand(copied.Brand)
		brands[i] = &copied
	}
	full := make([]*proto.EmulationUserAgentBrandVersion, len(parsed.Hints.FullVersionList))
	for i, brand := range parsed.Hints.FullVersionList {
		copied := brand
		copied.Brand = chromeBrand(copied.Brand)
		full[i] = &copied
	}
	return chromeUA(parsed.UA), &proto.EmulationUserAgentMetadata{
		Brands:          brands,
		FullVersionList: full,
		Platform:        parsed.Hints.Platform,
		PlatformVersion: parsed.Hints.PlatformVersion,
		Architecture:    parsed.Hints.Architecture,
		Model:           parsed.Hints.Model,
		Mobile:          parsed.Hints.Mobile,
		Bitness:         parsed.Hints.Bitness,
		Wow64:           parsed.Hints.Wow64,
		FormFactors:     parsed.Hints.FormFactors,
	}, nil
}

func chromeUA(ua string) string {
	return strings.ReplaceAll(ua, "HeadlessChrome", "Chrome")
}

func chromeBrand(brand string) string {
	if brand == "HeadlessChrome" {
		return "Google Chrome"
	}
	return brand
}
