package launcher

import "github.com/go-rod/rod/lib/launcher/flags"

// hardenedFlags is the flag list shared by the ProjectDiscovery headless
// launchers. --no-sandbox and --disable-web-security are not included:
// sandboxing stays under the caller's control, and disabling web security
// is a separate decision.
var hardenedFlags = []struct {
	name   flags.Flag
	values []string
}{
	{"disable-field-trial-config", nil},
	{"disable-background-networking", nil},
	{"enable-features", []string{"NetworkService", "NetworkServiceInProcess"}},
	{"disable-background-timer-throttling", nil},
	{"disable-backgrounding-occluded-windows", nil},
	{"disable-back-forward-cache", nil},
	{"disable-breakpad", nil},
	{"disable-crash-reporter", nil},
	{"disable-client-side-phishing-detection", nil},
	{"disable-component-extensions-with-background-pages", nil},
	{"disable-component-update", nil},
	{"no-default-browser-check", nil},
	{"disable-default-apps", nil},
	{"disable-extensions", nil},
	{"disable-features", []string{
		"ImprovedCookieControls", "LazyFrameLoading", "GlobalMediaControls",
		"DestroyProfileOnBrowserClose", "MediaRouter", "DialMediaRouteProvider",
		"AcceptCHFrame", "AutoExpandDetailsElement", "CertificateTransparencyComponentUpdater",
		"AvoidUnnecessaryBeforeUnloadCheckSync", "Translate", "HttpsUpgrades", "PaintHolding",
	}},
	{"allow-pre-commit-input", nil},
	{"disable-hang-monitor", nil},
	{"disable-popup-blocking", nil},
	{"disable-prompt-on-repost", nil},
	{"disable-renderer-backgrounding", nil},
	{"force-color-profile", []string{"srgb"}},
	{"metrics-recording-only", nil},
	{"no-first-run", nil},
	{"enable-automation", nil},
	{"password-store", []string{"basic"}},
	{"no-service-autorun", nil},
	{"export-tagged-pdf", nil},
	{"disable-search-engine-choice-screen", nil},
	{"unsafely-disable-devtools-self-xss-warnings", nil},
	{"disable-canvas-aa", nil},
	{"disable-2d-canvas-clip-aa", nil},
	{"disable-gl-drawing-for-tests", nil},
	{"disable-infobars", nil},
	{"disable-plugins", nil},
	{"deny-permission-prompts", nil},
	{"disable-gpu", nil},
	{"ignore-certificate-errors", nil},
	{"disable-notifications", nil},
	{"hide-scrollbars", nil},
	{"mute-audio", nil},
	{"incognito", nil},
	{"disable-sync", nil},
	{"no-pings", nil},
	{"safebrowsing-disable-auto-update", nil},
	{"disable-domain-reliability", nil},
	{"disable-gpu-sandbox", nil},
	{"disable-software-rasterizer", nil},
	{"disable-dev-shm-usage", nil},
}

// HardenedDefaults applies the shared headless flag preset. It does not
// change --no-sandbox, and it does not set --disable-web-security.
// disable-ipc-flooding-protection is removed because it makes some Chrome
// builds loop.
func (l *Launcher) HardenedDefaults() *Launcher {
	l.Delete("disable-ipc-flooding-protection")
	l.Delete("use-mock-keychain")
	for _, f := range hardenedFlags {
		l.Set(f.name, f.values...)
	}
	return l
}
