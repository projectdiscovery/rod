package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-rod/rod/lib/defaults"
	"github.com/go-rod/rod/lib/utils"
)

// Host formats a revision number to a downloadable URL for the browser.
type Host func(revision int) string

var hostConf = map[string]struct {
	urlPrefix string
	zipName   string
}{
	"darwin_amd64":  {"Mac", "chrome-mac.zip"},
	"darwin_arm64":  {"Mac_Arm", "chrome-mac.zip"},
	"linux_amd64":   {"Linux_x64", "chrome-linux.zip"},
	"windows_386":   {"Win", "chrome-win.zip"},
	"windows_amd64": {"Win_x64", "chrome-win.zip"},
}[runtime.GOOS+"_"+runtime.GOARCH]

// HostGoogle to download browser.
func HostGoogle(revision int) string {
	return fmt.Sprintf(
		"https://storage.googleapis.com/chromium-browser-snapshots/%s/%d/%s",
		hostConf.urlPrefix,
		revision,
		hostConf.zipName,
	)
}

// HostNPM to download browser.
func HostNPM(revision int) string {
	return fmt.Sprintf(
		"https://registry.npmmirror.com/-/binary/chromium-browser-snapshots/%s/%d/%s",
		hostConf.urlPrefix,
		revision,
		hostConf.zipName,
	)
}

// HostPlaywright to download browser.
func HostPlaywright(revision int) string {
	rev := RevisionPlaywright
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "arm64") {
		rev = revision
	}
	return fmt.Sprintf(
		"https://playwright.azureedge.net/builds/chromium/%d/chromium-linux-arm64.zip",
		rev,
	)
}

// DefaultBrowserDir for downloaded browser. For unix is "$HOME/.cache/rod/browser",
// for Windows it's "%APPDATA%\rod\browser".
var DefaultBrowserDir = filepath.Join(map[string]string{
	"windows": os.Getenv("APPDATA"),
	"darwin":  filepath.Join(os.Getenv("HOME"), ".cache"),
	"linux":   filepath.Join(os.Getenv("HOME"), ".cache"),
}[runtime.GOOS], "rod", "browser")

// Browser is a helper to download browser smartly.
//
// By default it downloads the latest stable full Chrome from Chrome for
// Testing. Set HeadlessShell for the smaller chrome-headless-shell build, or
// Revision for a Chromium snapshot.
type Browser struct {
	Context context.Context

	// Hosts are the candidates to download the browser.
	// Such as [HostGoogle] or [HostNPM]. When nil, the default hosts for the
	// selected browser are used. An empty non-nil list downloads nothing.
	Hosts []Host

	// Revision of a Chromium snapshot to download, such as [RevisionDefault].
	// When zero, the latest stable Chrome for Testing build is downloaded.
	Revision int

	// HeadlessShell downloads chrome-headless-shell instead of full Chrome.
	// The shell cannot show a window, so [Browser.Headful] wins. Only applies
	// when Revision is zero and the browser was not created by
	// [NewChromeForTesting].
	HeadlessShell bool

	// Headful downloads the full Chrome build, because chrome-headless-shell
	// cannot show a window. Only applies when Revision is zero and the browser
	// was not created by [NewChromeForTesting].
	Headful bool

	// RootDir to download different browser versions.
	RootDir string

	// Log to print output
	Logger utils.Logger

	// LockPort a tcp port to prevent race downloading. Default is 2968 .
	LockPort int

	// HTTPClient to download the browser
	HTTPClient *http.Client

	// SHA256 of the downloaded archive. When set, a mismatching download is
	// rejected before extraction.
	SHA256 string

	cft *chromeForTesting
}

// NewBrowser with default values. The default is the latest stable full
// Chrome from Chrome for Testing.
func NewBrowser() *Browser {
	return &Browser{
		Context:  context.Background(),
		RootDir:  DefaultBrowserDir,
		Logger:   log.New(os.Stdout, "[launcher.Browser]", log.LstdFlags),
		LockPort: defaults.LockPort,
	}
}

// target is the Chrome for Testing build this browser uses. It is nil when a
// Chromium snapshot of Revision is selected. The latest stable version is
// looked up once per [latestTTL]. When the lookup fails, the newest build
// already in RootDir is used, so an offline machine keeps working.
func (lc *Browser) target() (*chromeForTesting, error) {
	if lc.Revision != 0 {
		return nil, nil
	}
	if lc.cft != nil {
		return lc.cft, nil
	}
	if _, err := cftPlatform(); err != nil {
		return nil, err
	}

	ctx := lc.Context
	if ctx == nil {
		ctx = context.Background()
	}
	t := &chromeForTesting{headlessShell: lc.useShell()}
	version, err := latestStable(ctx, lc.HTTPClient)
	if err == nil {
		t.version = version
		return t, nil
	}
	installed, ok := newestInstalled(lc.RootDir, t)
	if !ok {
		return nil, err
	}
	t.version = installed
	return t, nil
}

// resolved is target for the path helpers, which cannot return an error.
// When the target can't be resolved, the path has an empty version and the
// caller fails in [Browser.Get] or [Browser.Download] with the real error.
func (lc *Browser) resolved() *chromeForTesting {
	t, err := lc.target()
	if err != nil {
		return &chromeForTesting{headlessShell: lc.useShell()}
	}
	return t
}

// useShell reports whether the download is chrome-headless-shell. A headful
// launch always gets full Chrome, because the shell cannot show a window.
func (lc *Browser) useShell() bool {
	return lc.HeadlessShell && !lc.Headful
}

func (lc *Browser) dirFor(t *chromeForTesting) string {
	if t != nil {
		return filepath.Join(lc.RootDir, t.dirName())
	}
	return filepath.Join(lc.RootDir, fmt.Sprintf("chromium-%d", lc.Revision))
}

func (lc *Browser) binFor(t *chromeForTesting) string {
	if t != nil {
		return filepath.Join(lc.dirFor(t), filepath.FromSlash(t.binName()))
	}
	bin := map[string]string{
		"darwin":  "Chromium.app/Contents/MacOS/Chromium",
		"linux":   "chrome",
		"windows": "chrome.exe",
	}[runtime.GOOS]

	return filepath.Join(lc.dirFor(t), filepath.FromSlash(bin))
}

// Dir to download the browser.
func (lc *Browser) Dir() string {
	return lc.dirFor(lc.resolved())
}

// BinPath to download the browser executable.
func (lc *Browser) BinPath() string {
	return lc.binFor(lc.resolved())
}

// defaultHosts are the download candidates for the target t.
func defaultHosts(t *chromeForTesting) []Host {
	if t == nil {
		return []Host{HostGoogle, HostNPM, HostPlaywright}
	}
	return []Host{HostChromeForTesting(t.version, t.headlessShell)}
}

// Download browser from the fastest host.
// It will race downloading a TCP packet from each host and use the fastest host.
func (lc *Browser) Download() error {
	t, err := lc.target()
	if err != nil {
		return err
	}
	return lc.download(t)
}

func (lc *Browser) download(t *chromeForTesting) error {
	hosts := lc.Hosts
	if hosts == nil {
		hosts = defaultHosts(t)
	}
	us := []string{}
	for _, host := range hosts {
		us = append(us, host(lc.Revision))
	}

	ctx := lc.Context
	if ctx == nil {
		ctx = context.Background()
	}
	logger := lc.Logger
	if logger == nil {
		logger = utils.LoggerQuiet
	}

	dir := lc.dirFor(t)
	sum := lc.SHA256
	if sum == "" {
		if b, err := os.ReadFile(dir + ".sha256"); err == nil {
			sum = strings.TrimSpace(string(b))
		}
	}
	if sum == "" {
		sum = ChromeForTestingPins[lc.pinKey(t)]
	}

	got, err := fetchArchive(ctx, lc.HTTPClient, logger, dir, sum, us)
	if err != nil {
		return fmt.Errorf("can't find a browser binary for your OS, the doc might help https://go-rod.github.io/#/compatibility?id=os : %w", err) //nolint: lll
	}
	if err := os.WriteFile(dir+".sha256", []byte(got+"\n"), 0o644); err != nil {
		return err
	}

	// Only the latest-stable path keeps one build per product. An explicit
	// version from NewChromeForTesting is never pruned.
	if t != nil && lc.cft == nil {
		if n, err := PruneOlderBrowsers(lc.RootDir, dir); err == nil && n > 0 {
			logger.Println("Removed older browsers:", n)
		}
	}
	return nil
}

// pinKey is the ChromeForTestingPins key for the target t.
func (lc *Browser) pinKey(t *chromeForTesting) string {
	if t == nil {
		return fmt.Sprintf("chromium/%d", lc.Revision)
	}
	platform, err := cftPlatform()
	if err != nil {
		return ""
	}
	return t.product() + "/" + platform + "/" + t.version
}

// Get is a smart helper to get the browser executable path.
// If [Browser.BinPath] is not valid it will auto download the browser to [Browser.BinPath].
// On a musl host it returns the browser found by [LookPath] instead, or
// [ErrMuslNoBrowser].
func (lc *Browser) Get() (string, error) {
	if hostUsesMusl() {
		if bin, has := LookPath(); has {
			return bin, nil
		}
		return "", ErrMuslNoBrowser
	}

	unlock, err := lockDownload(lc.RootDir)
	if err != nil {
		return "", err
	}
	defer unlock()

	t, err := lc.target()
	if err != nil {
		return "", err
	}
	bin := lc.binFor(t)
	if validateBin(bin) == nil {
		return bin, nil
	}

	// Try to cleanup before downloading
	_ = os.RemoveAll(lc.dirFor(t))

	return bin, lc.download(t)
}

// MustGet is similar with Get.
func (lc *Browser) MustGet() string {
	p, err := lc.Get()
	utils.E(err)
	return p
}

// Validate returns nil if the browser executable is valid.
// If the executable is malformed it will return error.
func (lc *Browser) Validate() error {
	return validateBin(lc.BinPath())
}

func validateBin(bin string) error {
	_, err := os.Stat(bin)
	if err != nil {
		return err
	}

	cmd := exec.Command(bin, "--headless", "--no-sandbox",
		"--use-mock-keychain", "--disable-dev-shm-usage",
		"--disable-gpu", "--dump-dom", "about:blank")
	b, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(b), "error while loading shared libraries") {
			// When the os is missing some dependencies for chromium we treat it as valid binary.
			return nil
		}

		return fmt.Errorf("failed to run the browser: %w\n%s", err, b)
	}
	if !bytes.Contains(b, []byte(`<html><head></head><body></body></html>`)) {
		return errors.New("the browser executable doesn't support headless mode")
	}

	return nil
}

// LookPath searches for the browser executable from often used paths on current operating system.
func LookPath() (found string, has bool) {
	list := map[string][]string{
		"darwin": {
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/google-chrome",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
		},
		"linux": {
			"chrome",
			"google-chrome",
			"/usr/bin/google-chrome",
			"microsoft-edge",
			"/usr/bin/microsoft-edge",
			"chromium",
			"chromium-browser",
			"google-chrome-stable",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
			"/data/data/com.termux/files/usr/bin/chromium-browser",
		},
		"openbsd": {
			"chrome",
			"chromium",
		},
		"windows": append([]string{"chrome", "edge"}, expandWindowsExePaths(
			`Google\Chrome\Application\chrome.exe`,
			`Chromium\Application\chrome.exe`,
			`Microsoft\Edge\Application\msedge.exe`,
		)...),
	}[runtime.GOOS]

	for _, path := range list {
		var err error
		found, err = exec.LookPath(path)
		has = err == nil
		if has {
			break
		}
	}

	return
}

// interface for testing.
var openExec = exec.Command

// Open tries to open the url via system's default browser.
func Open(url string) {
	// Windows doesn't support format [::]
	url = strings.Replace(url, "[::]", "[::1]", 1)

	if bin, has := LookPath(); has {
		p := openExec(bin, url)
		_ = p.Start()
		_ = p.Process.Release()
	}
}

func expandWindowsExePaths(list ...string) []string {
	newList := []string{}
	for _, p := range list {
		newList = append(
			newList,
			filepath.Join(os.Getenv("ProgramFiles"), p),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), p),
			filepath.Join(os.Getenv("LocalAppData"), p),
		)
	}

	return newList
}
