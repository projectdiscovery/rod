package launcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectdiscovery/rod/lib/launcher/flags"
	"github.com/projectdiscovery/rod/lib/utils"
)

const fakeDumpDOSScript = "#!/bin/sh\necho '<html><head></head><body></body></html>'\n"

// fakeCFT serves a Stable versions file and fake Chrome for Testing archives
// for the current platform, both headless-shell and full Chrome. It points the
// package at the server for the duration of the test.
func fakeCFT(t *testing.T) {
	t.Helper()
	version := "157.0.1.0"
	if runtime.GOOS == "windows" {
		t.Skip("the fake browser is a shell script")
	}
	platform, err := cftPlatform()
	if err != nil {
		t.Skip(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/versions.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"channels":{"Stable":{"version":%q}}}`, version)
	})
	for _, headless := range []bool{true, false} {
		c := &chromeForTesting{version: version, headlessShell: headless}
		body := buildZip(t, zipEntry{
			name: c.product() + "-" + platform + "/" + c.binName(),
			body: fakeDumpDOSScript,
			mode: 0o755,
		})
		path := fmt.Sprintf("/cft/%s/%s/%s-%s.zip", version, platform, c.product(), platform)
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body)
		})
	}
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)

	oldVersions, oldStorage := ChromeForTestingVersionsURL, chromeForTestingStorage
	ChromeForTestingVersionsURL = s.URL + "/versions.json"
	chromeForTestingStorage = s.URL + "/cft"
	t.Cleanup(func() {
		ChromeForTestingVersionsURL = oldVersions
		chromeForTestingStorage = oldStorage
	})
}

// installFake places an executable fake browser under root, as a previous
// download would have.
func installFake(t *testing.T, root string, c *chromeForTesting) string {
	t.Helper()
	bin := filepath.Join(root, c.dirName(), filepath.FromSlash(c.binName()))
	err := os.MkdirAll(filepath.Dir(bin), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(bin, []byte(fakeDumpDOSScript), 0o755)
	if err != nil { //nolint: gosec
		t.Fatal(err)
	}
	return bin
}

func TestCFTPlatformFor(t *testing.T) {
	cases := map[[2]string]string{
		{"linux", "amd64"}:   "linux64",
		{"linux", "arm64"}:   "linux-arm64",
		{"darwin", "arm64"}:  "mac-arm64",
		{"darwin", "amd64"}:  "mac-x64",
		{"windows", "amd64"}: "win64",
		{"windows", "386"}:   "win32",
	}
	for k, want := range cases {
		got, err := cftPlatformFor(k[0], k[1])
		if err != nil || got != want {
			t.Fatalf("%v: got %q %v, want %q", k, got, err, want)
		}
	}
	_, err := cftPlatformFor("linux", "386")
	if err == nil {
		t.Fatal("linux/386 has no build")
	}
}

func TestDefaultDownloadsLatestChrome(t *testing.T) {
	fakeCFT(t)

	b := NewBrowser()
	b.RootDir = t.TempDir()
	b.Logger = utils.LoggerQuiet

	bin, err := b.Get()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(b.RootDir, "chrome-157.0.1.0",
		filepath.FromSlash((&chromeForTesting{}).binName()))
	if bin != want {
		t.Fatalf("bin %s, want %s", bin, want)
	}
	err = b.Validate()
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(b.Dir() + ".sha256")
	if err != nil {
		t.Fatal("pin not recorded", err)
	}
}

func TestDownloadSuccessIgnoresLegacyChromium(t *testing.T) {
	fakeCFT(t)

	root := t.TempDir()
	legacy := installLegacy(t, root, 1321438, fakeDumpDOSScript)

	b := NewBrowser()
	b.RootDir = root
	b.Logger = utils.LoggerQuiet
	bin, err := b.Get()
	if err != nil {
		t.Fatal(err)
	}
	if bin == legacy || !strings.Contains(bin, "157.0.1.0") {
		t.Fatalf("bin %s, want the downloaded latest Chrome for Testing", bin)
	}
}

func TestHeadfulDownloadsFullChrome(t *testing.T) {
	fakeCFT(t)

	b := NewBrowser()
	b.RootDir = t.TempDir()
	b.Logger = utils.LoggerQuiet
	b.Headful = true

	bin, err := b.Get()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bin, "chrome-157.0.1.0") || strings.Contains(bin, "headless-shell") {
		t.Fatalf("headful should get the full build, got %s", bin)
	}
}

func TestLaunchPicksBuildByHeadless(t *testing.T) {
	fakeCFT(t)

	headless := New()
	headless.browser.RootDir = t.TempDir()
	headless.browser.Logger = utils.LoggerQuiet
	bin, err := headless.getBin()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bin, "chrome-157.0.1.0") || strings.Contains(bin, "headless-shell") {
		t.Fatalf("headless launch should get full Chrome, got %s", bin)
	}

	shell := New().HeadlessShell(true)
	shell.browser.RootDir = t.TempDir()
	shell.browser.Logger = utils.LoggerQuiet
	bin, err = shell.getBin()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bin, "chrome-headless-shell-157.0.1.0") {
		t.Fatalf("HeadlessShell should get the shell, got %s", bin)
	}

	// The shell cannot show a window, so the flag is ignored.
	window := New().Headless(false).HeadlessShell(true)
	window.browser.RootDir = t.TempDir()
	window.browser.Logger = utils.LoggerQuiet
	bin, err = window.getBin()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bin, "chrome-157.0.1.0") || strings.Contains(bin, "headless-shell") {
		t.Fatalf("headful HeadlessShell should get full Chrome, got %s", bin)
	}

	headful := New().Headless(false)
	headful.browser.RootDir = t.TempDir()
	headful.browser.Logger = utils.LoggerQuiet
	if headful.Has(flags.Headless) {
		t.Fatal("headless flag should be removed")
	}
	bin, err = headful.getBin()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bin, "chrome-157.0.1.0") || strings.Contains(bin, "headless-shell") {
		t.Fatalf("headful launch should get the full build, got %s", bin)
	}
}

func TestLatestPrunesOlderVersion(t *testing.T) {
	fakeCFT(t)

	root := t.TempDir()
	old := installFake(t, root, &chromeForTesting{version: "156.0.1.0", headlessShell: false})
	shell := installFake(t, root, &chromeForTesting{version: "156.0.1.0", headlessShell: true})

	b := NewBrowser()
	b.RootDir = root
	b.Logger = utils.LoggerQuiet
	_, err := b.Get()
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(filepath.Dir(old))
	if !os.IsNotExist(err) {
		t.Fatal("older full chrome still cached", err)
	}
	_, err = os.Stat(filepath.Dir(shell))
	if err != nil {
		t.Fatal("older headless-shell of another product must stay", err)
	}
}

func TestLatestFallsBackToInstalledWhenOffline(t *testing.T) {
	// Nothing listens on port 1, so the versions file cannot be fetched.
	oldVersions := ChromeForTestingVersionsURL
	ChromeForTestingVersionsURL = "http://127.0.0.1:1/versions.json"
	t.Cleanup(func() { ChromeForTestingVersionsURL = oldVersions })

	root := t.TempDir()
	older := installFake(t, root, &chromeForTesting{version: "155.0.1.0", headlessShell: false})
	newest := installFake(t, root, &chromeForTesting{version: "156.0.1.0", headlessShell: false})

	b := NewBrowser()
	b.RootDir = root
	b.Logger = utils.LoggerQuiet
	bin, err := b.Get()
	if err != nil {
		t.Fatal(err)
	}
	if bin != newest {
		t.Fatalf("bin %s, want the newest installed %s (not %s)", bin, newest, older)
	}
}

func TestOfflineWithNothingInstalledFails(t *testing.T) {
	oldVersions := ChromeForTestingVersionsURL
	ChromeForTestingVersionsURL = "http://127.0.0.1:1/versions.json"
	t.Cleanup(func() { ChromeForTestingVersionsURL = oldVersions })

	b := NewBrowser()
	b.RootDir = t.TempDir()
	b.Logger = utils.LoggerQuiet
	_, err := b.Get()
	if err == nil {
		t.Fatal("expected an error without network or cache")
	}
}

// installLegacy places a Chromium snapshot under root in the layout that
// go-rod used for its default download, such as chromium-1321438.
func installLegacy(t *testing.T, root string, rev int, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake browser is a shell script")
	}
	bin := filepath.Join(root, fmt.Sprintf("chromium-%d", rev), filepath.FromSlash(chromiumBin()))
	err := os.MkdirAll(filepath.Dir(bin), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(bin, []byte(script), 0o755)
	if err != nil { //nolint: gosec
		t.Fatal(err)
	}
	return bin
}

func offlineCFT(t *testing.T) {
	t.Helper()
	// Nothing listens on port 1, so the versions file cannot be fetched.
	oldVersions := ChromeForTestingVersionsURL
	ChromeForTestingVersionsURL = "http://127.0.0.1:1/versions.json"
	t.Cleanup(func() { ChromeForTestingVersionsURL = oldVersions })
}

func TestOfflineFallsBackToLegacyChromium(t *testing.T) {
	offlineCFT(t)

	root := t.TempDir()
	installLegacy(t, root, 1131657, fakeDumpDOSScript)
	want := installLegacy(t, root, 1321438, fakeDumpDOSScript)
	installLegacy(t, root, 1400000, "#!/bin/sh\nexit 1\n")
	for _, dir := range []string{"chromium-latest", "chromium-1500000"} {
		err := os.MkdirAll(filepath.Join(root, dir), 0o755)
		if err != nil {
			t.Fatal(err)
		}
	}
	layout := map[string]string{
		"darwin":  "Chromium.app/Contents/MacOS/Chromium",
		"linux":   "chrome",
		"windows": "chrome.exe",
	}[runtime.GOOS]
	if want != filepath.Join(root, "chromium-1321438", filepath.FromSlash(layout)) {
		t.Fatalf("fixture %s is not the go-rod cache layout", want)
	}

	b := NewBrowser()
	b.RootDir = root
	b.Logger = utils.LoggerQuiet
	bin, err := b.Get()
	if err != nil {
		t.Fatal(err)
	}
	if bin != want {
		t.Fatalf("bin %s, want the newest valid snapshot %s", bin, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.Context = ctx
	_, err = b.Get()
	if err == nil {
		t.Fatal("a canceled context must not fall back")
	}
}

func TestOfflinePrefersChromeForTestingOverLegacy(t *testing.T) {
	offlineCFT(t)

	root := t.TempDir()
	installLegacy(t, root, 1321438, fakeDumpDOSScript)
	want := installFake(t, root, &chromeForTesting{version: "156.0.1.0", headlessShell: false})

	b := NewBrowser()
	b.RootDir = root
	b.Logger = utils.LoggerQuiet
	bin, err := b.Get()
	if err != nil {
		t.Fatal(err)
	}
	if bin != want {
		t.Fatalf("bin %s, want the installed Chrome for Testing %s", bin, want)
	}
}

func TestDownloadFailureFallsBackToLegacyChromium(t *testing.T) {
	var downloads atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/versions.json" {
			_, _ = fmt.Fprint(w, `{"channels":{"Stable":{"version":"157.0.1.0"}}}`)
			return
		}
		downloads.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	oldVersions, oldStorage := ChromeForTestingVersionsURL, chromeForTestingStorage
	ChromeForTestingVersionsURL = s.URL + "/versions.json"
	chromeForTestingStorage = s.URL + "/cft"
	t.Cleanup(func() {
		ChromeForTestingVersionsURL = oldVersions
		chromeForTestingStorage = oldStorage
	})

	root := t.TempDir()
	want := installLegacy(t, root, 1321438, fakeDumpDOSScript)

	// An explicit Chromium revision does not fall back.
	snap := NewBrowser()
	snap.RootDir = root
	snap.Revision = 1400000
	snap.Hosts = []Host{func(int) string { return s.URL + "/snapshot.zip" }}
	snap.Logger = utils.LoggerQuiet
	_, err := snap.Get()
	if err == nil {
		t.Fatal("an explicit revision must fail when its download fails")
	}

	b := NewBrowser()
	b.RootDir = root
	b.Logger = utils.LoggerQuiet
	bin, err := b.Get()
	if err != nil {
		t.Fatal(err)
	}
	if bin != want {
		t.Fatalf("bin %s, want the cached snapshot %s", bin, want)
	}

	// An older cached Chrome for Testing build wins over the snapshot.
	cft := installFake(t, root, &chromeForTesting{version: "156.0.1.0", headlessShell: false})
	bin, err = b.Get()
	if err != nil {
		t.Fatal(err)
	}
	if bin != cft {
		t.Fatalf("bin %s, want the cached Chrome for Testing %s", bin, cft)
	}
	if downloads.Load() == 0 {
		t.Fatal("the Chrome for Testing download was not tried")
	}

	// Without a cached snapshot the download error is returned.
	empty := NewBrowser()
	empty.RootDir = t.TempDir()
	empty.Logger = utils.LoggerQuiet
	_, err = empty.Get()
	if err == nil {
		t.Fatal("a failed download without a cached snapshot must fail")
	}

	// An explicit Chrome for Testing version must not fall back.
	pinned := NewChromeForTesting("157.0.1.0", false)
	pinned.RootDir = root
	pinned.Logger = utils.LoggerQuiet
	_, err = pinned.Get()
	if err == nil {
		t.Fatal("an explicit version must fail when its download fails")
	}
}

func TestRevisionSelectsChromium(t *testing.T) {
	b := NewBrowser()
	b.RootDir = "/cache"
	b.Revision = 7
	if got, want := b.Dir(), filepath.Join("/cache", "chromium-7"); got != want {
		t.Fatalf("dir %s, want %s", got, want)
	}
	if len(defaultHosts(nil)) != 3 {
		t.Fatal("chromium snapshots keep the google, npm and playwright hosts")
	}
}

func TestCompliantLaunchFlags(t *testing.T) {
	l := New().HeadlessShell(true).Compliant(true)
	l.applyCompliant()
	if l.browser.HeadlessShell {
		t.Fatal("compliant launch uses full Chrome")
	}
	if l.Has("enable-automation") {
		t.Fatal("automation switch still set")
	}
	if !strings.Contains(l.Get("disable-blink-features"), "AutomationControlled") {
		t.Fatalf("blink features %q", l.Get("disable-blink-features"))
	}

	again := New().Set("disable-blink-features", "AutomationControlled")
	again.compliant = true
	again.applyCompliant()
	if again.Get("disable-blink-features") != "AutomationControlled" {
		t.Fatalf("feature duplicated: %s", again.Get("disable-blink-features"))
	}
}

// A download host that accepts the connection and sends no headers must not
// block Get. The download fails in time and the cached browser is used.
func TestSilentDownloadFallsBackToCachedBrowser(t *testing.T) {
	old := DownloadHeaderTimeout
	DownloadHeaderTimeout = 300 * time.Millisecond
	t.Cleanup(func() { DownloadHeaderTimeout = old })

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"channels":{"Stable":{"version":"157.0.1.0"}}}`)
	}))
	t.Cleanup(s.Close)
	oldVersions, oldStorage := ChromeForTestingVersionsURL, chromeForTestingStorage
	ChromeForTestingVersionsURL = s.URL + "/versions.json"
	storage, accepted := silentHost(t)
	chromeForTestingStorage = storage
	t.Cleanup(func() {
		ChromeForTestingVersionsURL = oldVersions
		chromeForTestingStorage = oldStorage
	})
	latestVersions.Lock()
	delete(latestVersions.m, ChromeForTestingVersionsURL)
	latestVersions.Unlock()

	root := t.TempDir()
	want := installFake(t, root, &chromeForTesting{version: "156.0.1.0", headlessShell: false})

	b := NewBrowser()
	b.RootDir = root
	b.Logger = utils.LoggerQuiet
	type result struct {
		bin string
		err error
	}
	done := make(chan result, 1)
	go func() {
		bin, err := b.Get()
		done <- result{bin, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.bin != want {
			t.Fatalf("bin %s, want the cached Chrome for Testing %s", r.bin, want)
		}
		if accepted.Load() == 0 {
			t.Fatal("the Chrome for Testing download was not tried")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Get hung on a download host that sends no headers")
	}
}
