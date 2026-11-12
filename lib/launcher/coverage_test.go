package launcher

import (
	"archive/zip"
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/utils"
)

func TestOutputBufferNil(t *testing.T) {
	var b *limitBuf
	if n, err := b.Write([]byte("x")); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if b.Len() != 0 || b.String() != "" {
		t.Fatal("a nil buffer reports content")
	}
}

func TestOutputBufferLimit(t *testing.T) {
	b := &limitBuf{max: 3}
	if n, err := b.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := b.Write([]byte("def")); n != 3 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if b.String() != "abc" {
		t.Fatalf("got %q", b.String())
	}
}

func TestAnnotate(t *testing.T) {
	err := errors.New("boom")
	l := &Launcher{output: &limitBuf{max: 64}}
	if got := l.annotate(nil); got != nil {
		t.Fatal(got)
	}
	if got := l.annotate(err); got != err {
		t.Fatal("annotated an empty output")
	}
	l.output.Write([]byte("chrome said so"))
	got := l.annotate(err)
	if !errors.Is(got, err) || !strings.Contains(got.Error(), "chrome said so") {
		t.Fatalf("got %v", got)
	}
}

func TestCheckFileSHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	// sha256("abc")
	sum := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if err := CheckFileSHA256(p, "  "+strings.ToUpper(sum)+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := CheckFileSHA256(p, "00"); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("got %v", err)
	}
	if err := CheckFileSHA256(filepath.Join(t.TempDir(), "missing"), sum); err == nil {
		t.Fatal("expected the open error")
	}
}

func TestLockDownload(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockDownload(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	// An empty dir uses DefaultBrowserDir.
	old := DefaultBrowserDir
	root := t.TempDir()
	DefaultBrowserDir = filepath.Join(root, "browsers")
	t.Cleanup(func() { DefaultBrowserDir = old })
	unlock, err = lockDownload("")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err := os.Stat(filepath.Join(DefaultBrowserDir, ".download.lock")); err != nil {
		t.Fatal(err)
	}

	// A file where the directory should be makes MkdirAll fail.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := lockDownload(f); err == nil {
		t.Fatal("expected the mkdir error")
	}
}

func TestPreparePrivileges(t *testing.T) {
	l := New()
	if err := l.preparePrivileges(); err != nil {
		t.Fatal(err)
	}

	l.DropPrivileges(os.Getuid(), os.Getgid())
	l.UserDataDir("")
	if err := l.preparePrivileges(); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "profile")
	l.UserDataDir(dir)
	if err := l.preparePrivileges(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}

	// A file where the profile dir should be makes MkdirAll fail.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l.UserDataDir(f)
	if err := l.preparePrivileges(); err == nil {
		t.Fatal("expected the mkdir error")
	}
}

func TestMinimalEnv(t *testing.T) {
	env := minimalEnv()
	for _, e := range env {
		if !strings.Contains(e, "=") {
			t.Fatalf("bad env entry %q", e)
		}
	}
	old := os.Getenv("HOME")
	if old != "" && !strings.Contains(strings.Join(env, "\n"), "HOME=") {
		t.Fatal("HOME missing")
	}
}

func TestNoInheritEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	script := "#!/bin/sh\necho DevTools listening on ws://127.0.0.1:1/devtools/browser/x\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	l := New().Bin(bin).NoInheritEnv(true)
	if !l.noInheritEnv {
		t.Fatal("flag not set")
	}
	if _, err := l.Launch(); err != nil {
		// The printed URL is unreachable, so only the launch itself matters.
		t.Logf("launch: %v", err)
	}
}

func TestWindowSizeAndPosition(t *testing.T) {
	l := New().WindowSize(800, 600).WindowPosition(10, 20)
	if l.Get(flags.WindowSize) != "800,600" {
		t.Fatal(l.Get(flags.WindowSize))
	}
	if l.Get(flags.WindowPosition) != "10,20" {
		t.Fatal(l.Get(flags.WindowPosition))
	}
}

func TestCompliantFlag(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	// Chrome answers the compliance probe on the debug port.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := New().Bin(bin)
	l.Compliant(true)
	if !l.compliant {
		t.Fatal("compliant flag not set")
	}
	if err := l.preparePrivileges(); err != nil {
		t.Fatal(err)
	}
}

func TestSetupUserPreferences(t *testing.T) {
	dir := t.TempDir()
	l := New().UserDataDir(dir).Preferences(`{"a":1}`)
	l.setupUserPreferences()
	p := filepath.Join(dir, "Default", "Preferences")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"a":1}` {
		t.Fatalf("got %s", b)
	}

	l.ProfileDir("P2")
	l.setupUserPreferences()
	if _, err := os.Stat(filepath.Join(dir, "P2", "Preferences")); err != nil {
		t.Fatal(err)
	}

	l.UserDataDir("")
	l.setupUserPreferences()

	l.UserDataDir(dir).Preferences("")
	l.setupUserPreferences()
}

func TestCleanupRemovesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	l := New().UserDataDir(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	close(l.exit)
	l.Cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("profile not removed")
	}

	removeAllRetry("")
}

func TestGetURLErrors(t *testing.T) {
	l := New()
	l.ctxCancel()
	if _, err := l.getURL(); err == nil {
		t.Fatal("expected the context error")
	}

	l = New()
	close(l.exit)
	if _, err := l.getURL(); err == nil {
		t.Fatal("expected the parser error")
	}
}

func TestResolveURLErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json/version") {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()

	if _, err := ResolveURL(s.URL); err == nil || !strings.Contains(err.Error(), "cdp version endpoint") {
		t.Fatalf("got %v", err)
	}

	// A body that is not JSON yields an empty websocket URL.
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer s2.Close()
	if _, err := ResolveURL(s2.URL); err != nil {
		t.Fatal(err)
	}
}

func TestURLParserWriteBadURL(t *testing.T) {
	p := NewURLParser()
	// A url that fails to parse leaves the buffer and drops the write.
	n, err := p.Write([]byte("ws://a b/\n"))
	if n != 10 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if p.done {
		t.Fatal("parser marked done")
	}
}

func TestURLParserContextCancelled(t *testing.T) {
	p := NewURLParser()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Context(ctx)
	done := make(chan struct{})
	go func() {
		_, _ = p.Write([]byte("ws://127.0.0.1:1/devtools/browser/x\n"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("write blocked after the context ended")
	}
	if !p.done {
		t.Fatal("parser not marked done")
	}
}

func TestPTYEnabled(t *testing.T) {
	if !PipeSupported() {
		t.Skip("pipes are only supported outside windows")
	}
}

func TestPruneEdgeCases(t *testing.T) {
	// An unknown keep name is a no-op.
	if n, err := PruneOlderBrowsers(t.TempDir(), "unknown"); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	// A missing root is a no-op.
	if n, err := PruneOlderBrowsers(filepath.Join(t.TempDir(), "missing"), "chrome-1"); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}

	root := t.TempDir()
	// A regular file is skipped.
	if err := os.WriteFile(filepath.Join(root, "chrome-0.1.0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory with a non-numeric version is skipped by compareVersion.
	if err := os.Mkdir(filepath.Join(root, "chrome-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory with the same version is not older.
	if err := os.Mkdir(filepath.Join(root, "chrome-1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	n, err := PruneOlderBrowsers(root, filepath.Join(root, "chrome-1.0.0"))
	if err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestCompareVersionInvalid(t *testing.T) {
	if _, ok := compareVersion("a", "1"); ok {
		t.Fatal("a is not a number")
	}
	if _, ok := compareVersion("1", "b"); ok {
		t.Fatal("b is not a number")
	}
	if cmp, ok := compareVersion("1.2", "1.2.0"); !ok || cmp != 0 {
		t.Fatalf("%d %v", cmp, ok)
	}
	if cmp, ok := compareVersion("2", "1"); !ok || cmp != 1 {
		t.Fatalf("%d %v", cmp, ok)
	}
	if _, _, ok := browserVersion("chrome-"); ok {
		t.Fatal("an empty version must not match")
	}
}

func TestSweepRemovesUnreadableRoot(t *testing.T) {
	// A file as root makes ReadDir fail.
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepUserDataDirs(f, 0); err == nil {
		t.Fatal("expected the read error")
	}
	if n, err := SweepUserDataDirs(filepath.Join(t.TempDir(), "missing"), 0); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := SweepUserDataDirs("", 0); err != nil {
		t.Fatalf("%d %v", n, err)
	}

	// A regular file entry is skipped.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := SweepUserDataDirs(root, 0); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
}

func TestOwnerAlive(t *testing.T) {
	if !ownerAlive("no-dash") {
		t.Fatal("an unknown owner is assumed alive")
	}
	host, _ := os.Hostname()
	if !ownerAlive(host + "-notanumber") {
		t.Fatal("a bad pid is assumed alive")
	}
	if !ownerAlive(host + "-0") {
		t.Fatal("a zero pid is assumed alive")
	}
	if !ownerAlive("other-host-1") {
		t.Fatal("another host is assumed alive")
	}
}

func TestElfUsesMuslMissingFile(t *testing.T) {
	if elfUsesMusl(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a missing file is not musl")
	}
}

func TestElfUsesMuslNoInterp(t *testing.T) {
	// A tiny file is not an ELF and reports false.
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("not elf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if elfUsesMusl(p) {
		t.Fatal("a non-ELF file is not musl")
	}
}

func TestCFTPlatformErrors(t *testing.T) {
	if _, err := cftPlatformFor("plan9", "mips"); err == nil {
		t.Fatal("expected an unknown platform error")
	}
}

func TestCFTNames(t *testing.T) {
	c := &chromeForTesting{version: "1", headlessShell: true}
	if c.product() != "chrome-headless-shell" || c.dirName() != "chrome-headless-shell-1" {
		t.Fatalf("%s %s", c.product(), c.dirName())
	}
	c = &chromeForTesting{version: "1"}
	if c.product() != "chrome" || c.dirName() != "chrome-1" {
		t.Fatalf("%s %s", c.product(), c.dirName())
	}
}

func TestHostChromeForTestingUnknownPlatform(t *testing.T) {
	// An unsupported platform makes the host report an empty url.
	old := cftPlatform
	t.Cleanup(func() { cftPlatform = old })
	cftPlatform = func() (string, error) { return "", errors.New("no platform") }

	host := HostChromeForTesting("1.2.3", false)
	if u := host(0); u != "" {
		t.Fatalf("got %q", u)
	}
}

func TestLatestChromeForTestingErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer s.Close()
	old := ChromeForTestingVersionsURL
	ChromeForTestingVersionsURL = s.URL
	t.Cleanup(func() { ChromeForTestingVersionsURL = old })

	if _, err := LatestChromeForTesting(context.Background(), nil, stableChannel); err == nil ||
		!strings.Contains(err.Error(), "versions") {
		t.Fatalf("got %v", err)
	}

	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"channels":{}}`))
	}))
	defer s2.Close()
	ChromeForTestingVersionsURL = s2.URL
	if _, err := LatestChromeForTesting(context.Background(), nil, "Nope"); err == nil ||
		!strings.Contains(err.Error(), "unknown") {
		t.Fatalf("got %v", err)
	}

	ChromeForTestingVersionsURL = s2.URL + "\n"
	if _, err := LatestChromeForTesting(context.Background(), nil, stableChannel); err == nil {
		t.Fatal("expected the request error")
	}
}

func TestLatestChromeForTestingBadJSON(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{"))
	}))
	defer s.Close()
	old := ChromeForTestingVersionsURL
	ChromeForTestingVersionsURL = s.URL
	t.Cleanup(func() { ChromeForTestingVersionsURL = old })
	if _, err := LatestChromeForTesting(context.Background(), nil, stableChannel); err == nil {
		t.Fatal("expected the decode error")
	}
}

func TestLatestStableCaches(t *testing.T) {
	var hits int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"channels":{"Stable":{"version":"9.9.9"}}}`))
	}))
	defer s.Close()
	old := ChromeForTestingVersionsURL
	ChromeForTestingVersionsURL = s.URL
	latestVersions.Lock()
	latestVersions.m = map[string]latestEntry{}
	latestVersions.Unlock()
	t.Cleanup(func() {
		ChromeForTestingVersionsURL = old
		latestVersions.Lock()
		latestVersions.m = map[string]latestEntry{}
		latestVersions.Unlock()
	})

	for i := 0; i < 3; i++ {
		v, err := latestStable(context.Background(), nil)
		if err != nil || v != "9.9.9" {
			t.Fatalf("%q %v", v, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("fetched %d times", got)
	}
}

func TestNewestInstalled(t *testing.T) {
	root := t.TempDir()
	c := &chromeForTesting{version: "1"}
	// A missing root reports nothing.
	if _, ok := newestInstalled(filepath.Join(root, "missing"), c); ok {
		t.Fatal("found a browser in a missing root")
	}
	// A directory without the executable is skipped.
	if err := os.Mkdir(filepath.Join(root, "chrome-1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file entry is skipped.
	if err := os.WriteFile(filepath.Join(root, "chrome-2.0.0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory with the executable is the best candidate.
	dir := filepath.Join(root, "chrome-3.0.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, filepath.FromSlash(c.binName()))
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	ver, ok := newestInstalled(root, c)
	if !ok || ver != "3.0.0" {
		t.Fatalf("%q %v", ver, ok)
	}
}

func TestBrowserDirAndBinFallbacks(t *testing.T) {
	b := NewBrowser()
	b.RootDir = t.TempDir()
	b.Revision = 5
	if !strings.HasSuffix(b.Dir(), "chromium-5") {
		t.Fatalf("dir %s", b.Dir())
	}
	if !strings.Contains(filepath.ToSlash(b.BinPath()), "chromium-5/") {
		t.Fatalf("bin %s", b.BinPath())
	}
}

func TestBrowserResolvedFallback(t *testing.T) {
	b := NewBrowser()
	b.RootDir = t.TempDir()
	b.HeadlessShell = true
	b.Headful = true
	if b.useShell() {
		t.Fatal("a headful launch must use full chrome")
	}
}

func TestBrowserDownloadErrors(t *testing.T) {
	b := NewBrowser()
	b.RootDir = filepath.Join(t.TempDir(), "missing-parent")
	// A file in place of the root makes MkdirAll fail.
	f := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b.RootDir = f
	b.Revision = 3
	b.Hosts = []Host{func(int) string { return "http://127.0.0.1:1/x" }}
	b.Logger = utils.LoggerQuiet
	if err := b.Download(); err == nil {
		t.Fatal("expected a download error")
	}
}

func TestBrowserDownloadEmptyHosts(t *testing.T) {
	b := NewBrowser()
	b.RootDir = t.TempDir()
	b.Revision = 3
	b.Hosts = []Host{}
	if err := b.Download(); err == nil || !strings.Contains(err.Error(), "no download urls") {
		t.Fatalf("got %v", err)
	}
}

func TestBrowserTargetNoContext(t *testing.T) {
	b := NewChromeForTesting("1.0.0", false)
	b.Context = nil
	b.RootDir = t.TempDir()
	if b.Dir() == "" {
		t.Fatal("no dir")
	}
}

func TestBrowserGetWithRevision(t *testing.T) {
	// The download fails fast on a bad host, and the returned error is
	// wrapped with the compatibility hint.
	b := NewBrowser()
	b.RootDir = t.TempDir()
	b.Revision = 4
	b.Hosts = []Host{func(int) string { return "http://127.0.0.1:1/x" }}
	b.Logger = utils.LoggerQuiet
	if _, err := b.Get(); err == nil || !strings.Contains(err.Error(), "can't find a browser binary") {
		t.Fatalf("got %v", err)
	}
}

func TestBrowserPinKey(t *testing.T) {
	b := NewBrowser()
	b.Revision = 7
	if k := b.pinKey(nil); k != "chromium/7" {
		t.Fatalf("key %s", k)
	}
	c := &chromeForTesting{version: "1.2.3"}
	if k := b.pinKey(c); !strings.HasPrefix(k, "chrome/") || !strings.HasSuffix(k, "/1.2.3") {
		t.Fatalf("key %s", k)
	}
}

func TestFetchArchiveErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "b")
	// No urls.
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", nil); err == nil {
		t.Fatal("expected the no urls error")
	}
	// Every url fails.
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{"http://127.0.0.1:1/x"}); err == nil {
		t.Fatal("expected the fetch error")
	}

	// A non-200 answer is an error.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer s.Close()
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{s.URL}); err == nil {
		t.Fatal("expected the status error")
	}

	// A body larger than the limit is rejected.
	old := MaxDownloadSize
	MaxDownloadSize = 4
	t.Cleanup(func() { MaxDownloadSize = old })
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer s2.Close()
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{s2.URL}); err == nil ||
		!strings.Contains(err.Error(), "larger than") {
		t.Fatalf("got %v", err)
	}

	// A sum mismatch is rejected.
	MaxDownloadSize = old
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buildZip(t, zipEntry{name: "a/chrome", body: "x"}))
	}))
	defer s3.Close()
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "00", []string{s3.URL}); err == nil ||
		!strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("got %v", err)
	}
}

func TestFastestLoserErrors(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer fast.Close()

	res, _, err := fastest(context.Background(), http.DefaultClient, []string{fast.URL, slow.URL})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body %q", body)
	}
	time.Sleep(300 * time.Millisecond) // let the draining goroutine cancel the loser
}

func TestFastestBadURL(t *testing.T) {
	if _, _, err := fastest(context.Background(), http.DefaultClient, []string{"://"}); err == nil {
		t.Fatal("expected the request error")
	}
}

func TestUnzipSafeErrors(t *testing.T) {
	dir := t.TempDir()
	// A zip with an absolute entry escapes the target.
	buf := bytes.NewBuffer(nil)
	z := zip.NewWriter(buf)
	w, err := z.Create("/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("x"))
	_ = z.Close()
	if err := unzipSafe(bytes.NewReader(buf.Bytes()), int64(buf.Len()), dir); err == nil ||
		!errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("got %v", err)
	}

	// A bad zip fails to open.
	if err := unzipSafe(bytes.NewReader([]byte("not a zip")), 9, dir); err == nil {
		t.Fatal("expected the zip error")
	}

	// A zip with too much content is rejected.
	old := MaxDownloadSize
	MaxDownloadSize = 1
	t.Cleanup(func() { MaxDownloadSize = old })
	if err := unzipSafe(bytes.NewReader(buildZip(t, zipEntry{name: "a", body: "abc"})), int64(len(buildZip(t, zipEntry{name: "a", body: "abc"}))), dir); err == nil ||
		!strings.Contains(err.Error(), "larger than") {
		t.Fatalf("got %v", err)
	}
}

func TestUnzipSafeDirsAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	buf := bytes.NewBuffer(nil)
	z := zip.NewWriter(buf)
	// A directory entry.
	if _, err := z.Create("a/"); err != nil {
		t.Fatal(err)
	}
	// A relative symlink inside the root is allowed.
	h := &zip.FileHeader{Name: "a/link", Method: zip.Deflate}
	h.SetMode(os.ModeSymlink | 0o777)
	w, err := z.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("target"))
	_ = z.Close()
	if err := unzipSafe(bytes.NewReader(buf.Bytes()), int64(buf.Len()), dir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(dir, "a", "link")); err != nil || got != "target" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestExtractSymlinkUnsafe(t *testing.T) {
	dir := t.TempDir()
	buf := bytes.NewBuffer(nil)
	z := zip.NewWriter(buf)
	for _, target := range []string{"/etc/passwd", "../../escape"} {
		h := &zip.FileHeader{Name: "link" + strings.ReplaceAll(target, "/", "_"), Method: zip.Deflate}
		h.SetMode(os.ModeSymlink | 0o777)
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(target))
	}
	_ = z.Close()
	if err := unzipSafe(bytes.NewReader(buf.Bytes()), int64(buf.Len()), dir); err == nil ||
		!errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("got %v", err)
	}
}

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	if _, err := safeJoin(root, "C:\\a"); err != nil {
		t.Fatalf("a backslash name is valid: %v", err)
	}
	if _, err := safeJoin(root, "../x"); err == nil {
		t.Fatal("expected the escape error")
	}
}

func TestStripSingleDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "only"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := stripSingleDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "only") {
		t.Fatalf("got %s", got)
	}
	if _, err := stripSingleDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected the read error")
	}
}

func TestInstallLatestChromeForTestingNoNetwork(t *testing.T) {
	// A bad versions URL makes the first request fail.
	old := ChromeForTestingVersionsURL
	ChromeForTestingVersionsURL = "http://127.0.0.1:1/x"
	t.Cleanup(func() { ChromeForTestingVersionsURL = old })
	if _, _, err := InstallLatestChromeForTesting(context.Background(), false); err == nil {
		t.Fatal("expected the fetch error")
	}
}

func TestInstallLatestChromeForTestingNilContext(t *testing.T) {
	old := ChromeForTestingVersionsURL
	ChromeForTestingVersionsURL = "http://127.0.0.1:1/x"
	t.Cleanup(func() { ChromeForTestingVersionsURL = old })
	//nolint: staticcheck // a nil context is part of the contract
	if _, _, err := InstallLatestChromeForTesting(nil, true); err == nil { //nolint: staticcheck
		t.Fatal("expected the fetch error")
	}
}

func TestPipeLaunchAlreadyLaunched(t *testing.T) {
	l := New()
	l.hasLaunched()
	if _, err := l.LaunchPipe(); !errors.Is(err, ErrAlreadyLaunched) {
		t.Fatalf("got %v", err)
	}
}

func TestPipeLaunchErrors(t *testing.T) {
	// A missing binary fails before the pipe is used.
	l := New().Bin(filepath.Join(t.TempDir(), "missing"))
	if _, err := l.LaunchPipe(); err == nil {
		t.Fatal("expected the start error")
	}

	// A binary that exits immediately leaves the pipe unusable but must
	// still close the browser and every pipe end.
	l = New().Bin("echo")
	if _, err := l.LaunchPipe(); err != nil {
		t.Fatal(err)
	}
}

func TestPipeLaunchBadPipe(t *testing.T) {
	// A failing pipe pair is reported.
	old := newPipePair
	t.Cleanup(func() { newPipePair = old })
	newPipePair = func() (*cdp.PipePair, error) { return nil, errors.New("pipe failed") }

	l := New().Bin("true")
	if _, err := l.LaunchPipe(); err == nil || !strings.Contains(err.Error(), "pipe failed") {
		t.Fatalf("got %v", err)
	}
}

func TestManagerFlagDenied(t *testing.T) {
	g := &managerFixture{t: t}
	res := g.request("--renderer-cmd-prefix=/tmp/x")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("code %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "flag not allowed") {
		t.Fatalf("body %s", body)
	}
}

func TestManagerPathDenied(t *testing.T) {
	g := &managerFixture{t: t}
	res := g.request("--user-data-dir=/tmp/x")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("code %d", res.StatusCode)
	}
}

func TestDeniedLauncherFlag(t *testing.T) {
	if !deniedLauncherFlag("--gpu-launcher") {
		t.Fatal("gpu-launcher is denied")
	}
	if deniedLauncherFlag("--headless") {
		t.Fatal("headless is allowed")
	}
}

func TestMinimalEnvKeepsTMPDIR(t *testing.T) {
	old, had := os.LookupEnv("TMPDIR")
	t.Setenv("TMPDIR", "/tmp/x")
	t.Cleanup(func() {
		if !had {
			os.Unsetenv("TMPDIR")
		} else {
			os.Setenv("TMPDIR", old)
		}
	})
	env := minimalEnv()
	if !strings.Contains(strings.Join(env, "\n"), "TMPDIR=/tmp/x") {
		t.Fatal("TMPDIR missing")
	}
}

// managerFixture drives a Manager over HTTP.
type managerFixture struct {
	t *testing.T
}

func (g *managerFixture) request(flag string) *http.Response {
	g.t.Helper()
	m := NewManager()
	name, value, _ := strings.Cut(strings.TrimLeft(flag, "-"), "=")
	l := New().Set(flags.Flag(name), value)
	m.Defaults = func(http.ResponseWriter, *http.Request) *Launcher { return l }
	s := httptest.NewServer(m)
	g.t.Cleanup(s.Close)

	req, err := http.NewRequest(http.MethodGet, s.URL, nil)
	if err != nil {
		g.t.Fatal(err)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set(HeaderName, string(l.JSON()))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func TestManagerDefaultsBody(t *testing.T) {
	m := NewManager()
	s := httptest.NewServer(m)
	defer s.Close()
	res, err := http.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	if !bytes.Contains(b, []byte(`"flags"`)) {
		t.Fatalf("body %s", b)
	}
}

func TestLaunchPipeStartErrorClosesPipes(t *testing.T) {
	// A script that is not executable makes cmd.Start fail.
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	if err := os.WriteFile(bin, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := New().Bin(bin)
	if _, err := l.LaunchPipe(); err == nil {
		t.Fatal("expected the start error")
	}
}

func TestLockFileSecondHandle(t *testing.T) {
	// A second handle on the same lock file can lock it once the first is
	// gone, and unlocking a free file is not an error.
	f, err := os.OpenFile(filepath.Join(t.TempDir(), ".download.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := lockFile(f); err != nil {
		t.Fatal(err)
	}
	if err := unlockFile(f); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonlessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("the test process must be alive")
	}
	if processAlive(1 << 22) {
		t.Fatal("an unused pid must be dead")
	}
}

func TestNetAddr(t *testing.T) {
	// toHTTP and toWS are covered elsewhere; keep the helpers referenced.
	_ = toHTTP(url.URL{Scheme: "wss"})
	_ = toWS(url.URL{Scheme: "https"})
}

func TestOwnerAliveWithoutDash(t *testing.T) {
	if !ownerAlive("nodash") {
		t.Fatal("a lock without a pid is assumed alive")
	}
}

func TestElfProgs(t *testing.T) {
	makeElf := func(types ...elf.ProgType) string {
		data := []byte("/lib/ld-musl-x86_64.so.1\x00")
		hdr := elf.Header64{
			Type:      uint16(elf.ET_EXEC),
			Machine:   uint16(elf.EM_X86_64),
			Version:   uint32(elf.EV_CURRENT),
			Phoff:     64,
			Ehsize:    64,
			Phentsize: 56,
			Phnum:     uint16(len(types)),
			Shentsize: 64,
		}
		copy(hdr.Ident[:], elf.ELFMAG)
		hdr.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
		hdr.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
		hdr.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
		buf := bytes.NewBuffer(nil)
		if err := binary.Write(buf, binary.LittleEndian, hdr); err != nil {
			t.Fatal(err)
		}
		for _, typ := range types {
			prog := elf.Prog64{Type: uint32(typ), Flags: uint32(elf.PF_R), Align: 1}
			if typ == elf.PT_INTERP {
				prog.Off = uint64(64 + 56*len(types))
				prog.Filesz = uint64(len(data))
				prog.Memsz = uint64(len(data))
			}
			if err := binary.Write(buf, binary.LittleEndian, prog); err != nil {
				t.Fatal(err)
			}
		}
		buf.Write(data)
		p := filepath.Join(t.TempDir(), "sh")
		if err := os.WriteFile(p, buf.Bytes(), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if elfUsesMusl(makeElf(elf.PT_LOAD)) {
		t.Fatal("an ELF without a loader is not musl")
	}
	if !elfUsesMusl(makeElf(elf.PT_LOAD, elf.PT_INTERP)) {
		t.Fatal("a loader after another program was not found")
	}
}

func TestCheckFileSHA256Dir(t *testing.T) {
	if err := CheckFileSHA256(t.TempDir(), "00"); err == nil {
		t.Fatal("expected the read error")
	}
}

func TestLockDownloadLockError(t *testing.T) {
	old := lockFileFn
	t.Cleanup(func() { lockFileFn = old })
	lockFileFn = func(*os.File) error { return errors.New("locked") }
	if _, err := lockDownload(t.TempDir()); err == nil || err.Error() != "locked" {
		t.Fatalf("got %v", err)
	}
}

func TestLockDownloadOpenError(t *testing.T) {
	// A directory named like the lock file makes OpenFile fail.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".download.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := lockDownload(dir); err == nil {
		t.Fatal("expected the open error")
	}
}

func TestResolveURLBodyError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer s.Close()
	if _, err := ResolveURL(s.URL); err == nil {
		t.Fatal("expected the body read error")
	}
}

func TestCompliantKeepsFeature(t *testing.T) {
	l := New().Compliant(true)
	l.Set("disable-blink-features", "SomethingElse")
	l.applyCompliant()
	got := l.Get("disable-blink-features")
	if !strings.Contains(got, "SomethingElse") || !strings.Contains(got, "AutomationControlled") {
		t.Fatalf("got %q", got)
	}
	l.applyCompliant()
	if strings.Count(l.Get("disable-blink-features"), "AutomationControlled") != 1 {
		t.Fatalf("got %q", l.Get("disable-blink-features"))
	}
}

func TestLaunchPrivilegeError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l := New().Bin("true").DropPrivileges(os.Getuid(), os.Getgid()).UserDataDir(f)
	if _, err := l.Launch(); err == nil {
		t.Fatal("expected the profile error")
	}
}

func TestStartLockedOnLinux(t *testing.T) {
	old := lockOSThread
	t.Cleanup(func() { lockOSThread = old })
	lockOSThread = func() bool { return true }

	l := New()
	if err := startLocked(l, exec.Command("true")); err != nil {
		t.Fatal(err)
	}
	<-l.exit
}

func TestRemoveAllRetryLoop(t *testing.T) {
	old := removeAll
	t.Cleanup(func() { removeAll = old })
	var calls int32
	removeAll = func(string) error {
		if atomic.AddInt32(&calls, 1) < 2 {
			return errors.New("busy")
		}
		return nil
	}
	removeAllRetry("x")
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("calls %d", calls)
	}
}

func TestNewestInstalledSkipsOddEntries(t *testing.T) {
	root := t.TempDir()
	c := &chromeForTesting{version: "1"}
	for _, name := range []string{"no-version", "chromium-9.9.9"} {
		dir := filepath.Join(root, name)
		bin := filepath.Join(dir, filepath.FromSlash(c.binName()))
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if ver, ok := newestInstalled(root, c); ok {
		t.Fatalf("found %q", ver)
	}
}

func TestBrowserPlatformErrors(t *testing.T) {
	old := cftPlatform
	t.Cleanup(func() { cftPlatform = old })
	cftPlatform = func() (string, error) { return "", errors.New("no platform") }

	b := NewBrowser()
	b.RootDir = t.TempDir()
	if err := b.Download(); err == nil {
		t.Fatal("expected the platform error")
	}
	if b.Dir() == "" {
		t.Fatal("no fallback dir")
	}
	if k := b.pinKey(&chromeForTesting{version: "1"}); k != "" {
		t.Fatalf("key %q", k)
	}
}

func TestBrowserTargetNilContext(t *testing.T) {
	old := ChromeForTestingVersionsURL
	ChromeForTestingVersionsURL = "http://127.0.0.1:1/x"
	t.Cleanup(func() {
		ChromeForTestingVersionsURL = old
		latestVersions.Lock()
		latestVersions.m = map[string]latestEntry{}
		latestVersions.Unlock()
	})

	// A version that is already installed is used when the lookup fails.
	b := NewBrowser()
	b.Context = nil
	b.RootDir = t.TempDir()
	installed := filepath.Join(b.RootDir, "chrome-1.2.3")
	bin := filepath.Join(installed, filepath.FromSlash((&chromeForTesting{}).binName()))
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if b.Dir() != installed {
		t.Fatalf("dir %s", b.Dir())
	}
}

func TestBrowserDownloadNilContextAndLogger(t *testing.T) {
	b := NewBrowser()
	b.Context = nil
	b.Logger = nil
	b.RootDir = t.TempDir()
	b.Hosts = []Host{}
	if err := b.Download(); err == nil {
		t.Fatal("expected the no urls error")
	}
}

func TestBrowserPinWriteError(t *testing.T) {
	data := buildZip(t, zipEntry{name: "top/chrome", body: "x"})
	s := serveZip(t, data)
	b := NewBrowser()
	b.RootDir = t.TempDir()
	b.Revision = 3
	b.Hosts = []Host{func(int) string { return s.URL }}
	b.Logger = utils.LoggerQuiet
	if err := os.Mkdir(b.Dir()+".sha256", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Download(); err == nil {
		t.Fatal("expected the pin write error")
	}
}

func TestBrowserGetErrors(t *testing.T) {
	old := lookPath
	t.Cleanup(func() { lookPath = old })
	oldMusl := hostUsesMusl
	t.Cleanup(func() { hostUsesMusl = oldMusl })
	hostUsesMusl = func() bool { return true }
	lookPath = func() (string, bool) { return "", false }
	b := NewBrowser()
	b.RootDir = t.TempDir()
	if _, err := b.Get(); !errors.Is(err, ErrMuslNoBrowser) {
		t.Fatalf("got %v", err)
	}

	hostUsesMusl = func() bool { return false }
	// A file where the root should be makes the lock fail.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b.RootDir = f
	if _, err := b.Get(); err == nil {
		t.Fatal("expected the lock error")
	}
}

func TestOpenWithoutBrowser(t *testing.T) {
	old := lookPath
	t.Cleanup(func() { lookPath = old })
	lookPath = func() (string, bool) { return "", false }
	Open("http://[::]/x")
}

func TestPipeGetBinError(t *testing.T) {
	l := New()
	l.browser.RootDir = t.TempDir()
	l.browser.Hosts = []Host{}
	l.browser.Logger = utils.LoggerQuiet
	if _, err := l.LaunchPipe(); err == nil {
		t.Fatal("expected the download error")
	}
}

func TestPipePrivilegeError(t *testing.T) {
	if !PipeSupported() {
		t.Skip("pipes are not supported on windows")
	}
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l := New().Bin("true").DropPrivileges(os.Getuid(), os.Getgid()).UserDataDir(f)
	if _, err := l.LaunchPipe(); err == nil {
		t.Fatal("expected the profile error")
	}
}

func TestPruneReadDirError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneOlderBrowsers(f, filepath.Join(f, "chrome-1.0.0")); err == nil {
		t.Fatal("expected the read error")
	}
}

func TestPruneRemoveError(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "chrome-2.0.0")
	old := filepath.Join(root, "chrome-1.0.0")
	if err := os.Mkdir(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	n, err := PruneOlderBrowsers(root, keep)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("removed %d", n)
	}
}

func TestFetchArchiveFileErrors(t *testing.T) {
	data := buildZip(t, zipEntry{name: "top/chrome", body: "x"})
	s := serveZip(t, data)

	// A file where the parent dir should be makes MkdirAll fail.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, filepath.Join(f, "b"), "", []string{s.URL}); err == nil {
		t.Fatal("expected the mkdir error")
	}

	// A read-only parent makes the temp file fail.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, filepath.Join(parent, "b"), "", []string{s.URL}); err == nil {
		t.Fatal("expected the temp file error")
	}
}

func TestFetchArchiveBodyError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer s.Close()
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, filepath.Join(t.TempDir(), "b"), "", []string{s.URL}); err == nil {
		t.Fatal("expected the copy error")
	}
}

func TestFetchArchiveStagingErrors(t *testing.T) {
	data := buildZip(t, zipEntry{name: "top/chrome", body: "x"})
	s := serveZip(t, data)
	dir := filepath.Join(t.TempDir(), "b")

	oldTemp := osMkdirTemp
	t.Cleanup(func() { osMkdirTemp = oldTemp })
	osMkdirTemp = func(string, string) (string, error) { return "", errors.New("no temp") }
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{s.URL}); err == nil ||
		!strings.Contains(err.Error(), "no temp") {
		t.Fatalf("got %v", err)
	}

	// An empty archive leaves the staging dir empty, so a missing staging
	// dir is what makes stripSingleDir fail.
	empty := serveZip(t, buildZip(t))
	osMkdirTemp = func(parent, _ string) (string, error) {
		return filepath.Join(parent, "missing"), nil
	}
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{empty.URL}); err == nil {
		t.Fatal("expected the read error")
	}

	oldRename := osRename
	t.Cleanup(func() { osRename = oldRename })
	osRename = func(string, string) error { return errors.New("no rename") }
	if _, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{s.URL}); err == nil ||
		!strings.Contains(err.Error(), "no rename") {
		t.Fatalf("got %v", err)
	}
}

func TestFastestDrainsLoser(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer slow.Close()

	res, _, err := fastest(context.Background(), http.DefaultClient, []string{fast.URL, slow.URL})
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	time.Sleep(300 * time.Millisecond)
}

func TestUnzipEntryErrors(t *testing.T) {
	// A dir entry below a file.
	if err := unzipSafe(bytes.NewReader(buildZip(t,
		zipEntry{name: "a", body: "x"},
		zipEntry{name: "a/b/", body: ""},
	)), int64(len(buildZip(t, zipEntry{name: "a", body: "x"}, zipEntry{name: "a/b/", body: ""}))), t.TempDir()); err == nil {
		t.Fatal("expected the dir error")
	}

	// A file entry below a file.
	data := buildZip(t, zipEntry{name: "a", body: "x"}, zipEntry{name: "a/b", body: "y"})
	if err := unzipSafe(bytes.NewReader(data), int64(len(data)), t.TempDir()); err == nil {
		t.Fatal("expected the file error")
	}

	// A symlink below a file.
	data = buildZip(t,
		zipEntry{name: "a", body: "x"},
		zipEntry{name: "a/link", body: "target", mode: os.ModeSymlink | 0o777},
	)
	if err := unzipSafe(bytes.NewReader(data), int64(len(data)), t.TempDir()); err == nil {
		t.Fatal("expected the symlink error")
	}
}

// unknownMethod patches every compression method in a zip to one the
// standard library cannot decompress.
func unknownMethod(data []byte) []byte {
	const unsupported = 99
	binary.LittleEndian.PutUint16(data[8:], unsupported)
	for i := 0; i+4 <= len(data); i++ {
		if bytes.Equal(data[i:i+4], []byte("PK\x01\x02")) {
			binary.LittleEndian.PutUint16(data[i+10:], unsupported)
		}
	}
	return data
}

func TestUnzipUnknownMethod(t *testing.T) {
	data := unknownMethod(buildZip(t, zipEntry{name: "a/chrome", body: "x"}))
	if err := unzipSafe(bytes.NewReader(data), int64(len(data)), t.TempDir()); err == nil {
		t.Fatal("expected the file open error")
	}

	data = unknownMethod(buildZip(t,
		zipEntry{name: "a/link", body: "target", mode: os.ModeSymlink | 0o777}))
	if err := unzipSafe(bytes.NewReader(data), int64(len(data)), t.TempDir()); err == nil {
		t.Fatal("expected the symlink open error")
	}
}

func TestUnzipOpenTargetError(t *testing.T) {
	// A file entry that names an existing directory fails to open.
	buf := bytes.NewBuffer(nil)
	z := zip.NewWriter(buf)
	if _, err := z.Create("a/"); err != nil {
		t.Fatal(err)
	}
	h := &zip.FileHeader{Name: "a", Method: zip.Deflate}
	w, err := z.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("x"))
	_ = z.Close()
	if err := unzipSafe(bytes.NewReader(buf.Bytes()), int64(buf.Len()), t.TempDir()); err == nil {
		t.Fatal("expected the target open error")
	}
}

func TestUnzipBadChecksum(t *testing.T) {
	// A stored symlink whose body is corrupted makes the read fail.
	buf := bytes.NewBuffer(nil)
	z := zip.NewWriter(buf)
	h := &zip.FileHeader{Name: "a/link", Method: zip.Store}
	h.SetMode(os.ModeSymlink | 0o777)
	w, err := z.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("target"))
	_ = z.Close()
	data := buf.Bytes()
	i := bytes.Index(data, []byte("target"))
	if i < 0 {
		t.Fatal("body not found")
	}
	data[i] = 'T'
	if err := unzipSafe(bytes.NewReader(data), int64(len(data)), t.TempDir()); err == nil {
		t.Fatal("expected the checksum error")
	}
}

func TestWithinRelError(t *testing.T) {
	if within("rel", "/abs") {
		t.Fatal("a relative root cannot contain an absolute path")
	}
}

func TestStripSingleDirFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := stripSingleDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("got %s", got)
	}
}

func TestInstallLatestChromeForTesting(t *testing.T) {
	name := "top/" + filepath.ToSlash((&chromeForTesting{}).binName())
	data := buildZip(t, zipEntry{name: name, body: "x", mode: 0o755})
	storage := serveZip(t, data)
	versions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"channels":{"Stable":{"version":"9.9.9"}}}`))
	}))
	defer versions.Close()

	oldURL, oldStorage := ChromeForTestingVersionsURL, chromeForTestingStorage
	oldDir := DefaultBrowserDir
	ChromeForTestingVersionsURL, chromeForTestingStorage = versions.URL, storage.URL
	DefaultBrowserDir = t.TempDir()
	latestVersions.Lock()
	latestVersions.m = map[string]latestEntry{}
	latestVersions.Unlock()
	t.Cleanup(func() {
		ChromeForTestingVersionsURL, chromeForTestingStorage = oldURL, oldStorage
		DefaultBrowserDir = oldDir
		latestVersions.Lock()
		latestVersions.m = map[string]latestEntry{}
		latestVersions.Unlock()
	})

	bin, removed, err := InstallLatestChromeForTesting(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed %d", removed)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	}

	// A bad storage url makes the download fail.
	chromeForTestingStorage = "http://127.0.0.1:1/x"
	if _, _, err := InstallLatestChromeForTesting(context.Background(), false); err == nil {
		t.Fatal("expected the download error")
	}
}

func TestChromeForTestingBinNames(t *testing.T) {
	c := &chromeForTesting{version: "1", headlessShell: true}
	if got := c.binNameFor("windows"); got != "chrome-headless-shell.exe" {
		t.Fatalf("got %q", got)
	}
	if got := c.binNameFor("linux"); got != "chrome-headless-shell" {
		t.Fatalf("got %q", got)
	}

	c.headlessShell = false
	if got := c.binNameFor("darwin"); !strings.Contains(got, "Google Chrome for Testing.app") {
		t.Fatalf("got %q", got)
	}
	if got := c.binNameFor("windows"); got != "chrome.exe" {
		t.Fatalf("got %q", got)
	}
	if got := c.binNameFor("linux"); got != "chrome" {
		t.Fatalf("got %q", got)
	}
	if got := c.binName(); got == "" {
		t.Fatal("no bin name")
	}
}
