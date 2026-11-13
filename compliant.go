package rod

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/projectdiscovery/rod/lib/proto"
	"github.com/ysmood/gson"
)

const compliantHintJS = `(() => {
	const ua = navigator.userAgent
	if (!navigator.userAgentData) return JSON.stringify({ua})
	return navigator.userAgentData.getHighEntropyValues([
		"brands", "fullVersionList", "platform", "platformVersion",
		"architecture", "bitness", "model", "wow64", "formFactors"
	]).then(hints => JSON.stringify({ua, hints}))
})()`

// compliantProfile is the browser's own client hints, read once.
type compliantProfile struct {
	once sync.Once
	ua   string
	meta *proto.EmulationUserAgentMetadata
	err  error
}

// ensureCompliant reads this browser's own client hints once.
func (b *Browser) ensureCompliant() error {
	if b.profile == nil {
		b.profile = &compliantProfile{}
	}
	b.profile.once.Do(func() {
		b.profile.ua, b.profile.meta, b.profile.err = b.readCompliantProfile()
	})
	return b.profile.err
}

func (p *Page) applyCompliant() error {
	if p.browser.profile == nil || p.browser.profile.ua == "" || p.browser.profile.meta == nil {
		if p.browser.profile != nil && p.browser.profile.err != nil {
			return p.browser.profile.err
		}
		return fmt.Errorf("compliant profile was not read")
	}
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
	probe := &Page{
		ctx:       b.ctx,
		session:   b.ctx,
		browser:   b,
		SessionID: session.SessionID,
	}
	_ = proto.PageEnable{}.Call(probe)

	var last error
	for range 50 {
		res, err := proto.RuntimeEvaluate{
			Expression:    compliantHintJS,
			AwaitPromise:  true,
			ReturnByValue: true,
		}.Call(probe)
		switch {
		case err != nil:
			last = err
		case res.ExceptionDetails != nil:
			last = fmt.Errorf("compliant profile: %s", res.ExceptionDetails.Text)
		default:
			ua, meta, err := compliantFromValue(res.Result.Value)
			switch {
			case err != nil:
				last = fmt.Errorf("compliant profile: %w (%s)", err, res.Result.Value.Str())
			case meta != nil:
				return ua, meta, nil
			default:
				last = fmt.Errorf("compliant profile: client hints not ready (%s)", res.Result.Value.Str())
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", nil, last
}

type compliantHints struct {
	UA    string `json:"ua"`
	Hints *struct {
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
	if parsed.Hints == nil || len(parsed.Hints.Brands) == 0 || parsed.UA == "" {
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
