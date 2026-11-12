package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// ChromeForTestingVersionsURL lists the latest Chrome for Testing version per channel.
var ChromeForTestingVersionsURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions.json"

// chromeForTestingStorage is the base URL of the Chrome for Testing archives.
var chromeForTestingStorage = "https://storage.googleapis.com/chrome-for-testing-public"

// ChromeForTestingPins maps "product/platform/version" to the SHA-256 hex
// digest of that archive. product is "chrome" or "chrome-headless-shell",
// platform is linux64, linux-arm64, mac-arm64, mac-x64, win64 or win32. A
// listed version is refused when the download does not match. Chrome for
// Testing does not publish these hashes, so latest stable is not listed: the
// hash of the first completed download is stored beside the cache and
// required for every later download of that same version.
var ChromeForTestingPins = map[string]string{}

const stableChannel = "Stable"

// latestTTL is how long a resolved Stable version is reused before the
// versions file is fetched again. latestTimeout bounds one fetch.
var (
	latestTTL     = time.Hour
	latestTimeout = 30 * time.Second
)

type chromeForTesting struct {
	version       string
	headlessShell bool
}

func cftPlatform() (string, error) {
	return cftPlatformFor(runtime.GOOS, runtime.GOARCH)
}

func cftPlatformFor(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "linux64", nil
	case "linux/arm64":
		return "linux-arm64", nil
	case "darwin/arm64":
		return "mac-arm64", nil
	case "darwin/amd64":
		return "mac-x64", nil
	case "windows/amd64":
		return "win64", nil
	case "windows/386":
		return "win32", nil
	}
	return "", fmt.Errorf("chrome for testing has no build for %s/%s", goos, goarch)
}

func (c *chromeForTesting) product() string {
	if c.headlessShell {
		return "chrome-headless-shell"
	}
	return "chrome"
}

func (c *chromeForTesting) dirName() string {
	return c.product() + "-" + c.version
}

func (c *chromeForTesting) binName() string {
	if c.headlessShell {
		if runtime.GOOS == "windows" {
			return "chrome-headless-shell.exe"
		}
		return "chrome-headless-shell"
	}
	switch runtime.GOOS {
	case "darwin":
		return "Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
	case "windows":
		return "chrome.exe"
	}
	return "chrome"
}

// HostChromeForTesting returns the download URL of a Chrome for Testing
// build. The revision argument of [Host] is ignored.
func HostChromeForTesting(version string, headlessShell bool) Host {
	c := &chromeForTesting{version: version, headlessShell: headlessShell}
	return func(int) string {
		platform, err := cftPlatform()
		if err != nil {
			return ""
		}
		return fmt.Sprintf("%s/%s/%s/%s-%s.zip",
			chromeForTestingStorage, version, platform, c.product(), platform)
	}
}

// NewChromeForTesting returns a [Browser] that downloads the given Chrome for
// Testing version. With headlessShell the smaller chrome-headless-shell build
// is used, which only runs headless and starts faster.
func NewChromeForTesting(version string, headlessShell bool) *Browser {
	b := NewBrowser()
	b.cft = &chromeForTesting{version: version, headlessShell: headlessShell}
	return b
}

// LatestChromeForTesting returns the current version of a Chrome for Testing
// channel: "Stable", "Beta", "Dev", or "Canary".
func LatestChromeForTesting(ctx context.Context, client *http.Client, channel string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ChromeForTestingVersionsURL, nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chrome for testing versions: %s", res.Status)
	}
	var data struct {
		Channels map[string]struct {
			Version string `json:"version"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&data); err != nil {
		return "", err
	}
	ch, ok := data.Channels[channel]
	if !ok || ch.Version == "" {
		return "", fmt.Errorf("unknown chrome for testing channel %q", channel)
	}
	return ch.Version, nil
}

// latestVersions caches the Stable version per versions URL. Failures are
// not cached.
var latestVersions = struct {
	sync.Mutex
	m map[string]latestEntry
}{m: map[string]latestEntry{}}

type latestEntry struct {
	version string
	at      time.Time
}

// latestStable returns the current Stable Chrome for Testing version, reusing
// a recent answer so that each launch does not fetch the versions file.
func latestStable(ctx context.Context, client *http.Client) (string, error) {
	latestVersions.Lock()
	defer latestVersions.Unlock()

	key := ChromeForTestingVersionsURL
	if e, ok := latestVersions.m[key]; ok && time.Since(e.at) < latestTTL {
		return e.version, nil
	}
	ctx, cancel := context.WithTimeout(ctx, latestTimeout)
	defer cancel()
	version, err := LatestChromeForTesting(ctx, client, stableChannel)
	if err != nil {
		return "", err
	}
	latestVersions.m[key] = latestEntry{version: version, at: time.Now()}
	return version, nil
}

// newestInstalled returns the newest version of the product of c that is
// already extracted under root and has its executable.
func newestInstalled(root string, c *chromeForTesting) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	best := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		prefix, ver, ok := browserVersion(e.Name())
		if !ok || prefix != c.product()+"-" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), filepath.FromSlash(c.binName()))); err != nil {
			continue
		}
		if best == "" {
			best = ver
			continue
		}
		if cmp, ok := compareVersion(ver, best); ok && cmp > 0 {
			best = ver
		}
	}
	return best, best != ""
}

// InstallLatestChromeForTesting downloads the current Stable Chrome for
// Testing build and removes older cached builds of the same product.
// With headlessShell the smaller chrome-headless-shell build is used.
// The returned path is the browser executable.
func InstallLatestChromeForTesting(ctx context.Context, headlessShell bool) (bin string, removed int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	version, err := LatestChromeForTesting(ctx, nil, stableChannel)
	if err != nil {
		return "", 0, err
	}
	b := NewChromeForTesting(version, headlessShell)
	b.Context = ctx
	bin, err = b.Get()
	if err != nil {
		return "", 0, err
	}
	removed, err = PruneOlderBrowsers(b.RootDir, b.Dir())
	return bin, removed, err
}
