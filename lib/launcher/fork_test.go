//go:build !windows

package launcher_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

func TestLaunchTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	bin := filepath.Join(t.TempDir(), "fake-chrome")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	l := launcher.New().Bin(bin).LaunchTimeout(300 * time.Millisecond)
	start := time.Now()
	_, err := l.Launch()
	if !errors.Is(err, launcher.ErrLaunchTimeout) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(l.PID(), 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("fake browser still running after timeout")
}

func TestLaunchErrorIncludesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	bin := filepath.Join(t.TempDir(), "fake-chrome")
	script := "#!/bin/sh\necho boom-from-chrome >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := launcher.New().Bin(bin).LaunchTimeout(2 * time.Second).Launch()
	if err == nil || !strings.Contains(err.Error(), "boom-from-chrome") {
		t.Fatalf("got %v", err)
	}
}

func TestHardenedDefaults(t *testing.T) {
	l := launcher.New().HardenedDefaults()
	if !l.Has("disable-extensions") || !l.Has("disable-dev-shm-usage") {
		t.Fatal("preset flags missing")
	}
	if l.Has("disable-web-security") {
		t.Fatal("preset disables web security")
	}
	if l.Has("disable-ipc-flooding-protection") || l.Has("use-mock-keychain") {
		t.Fatal("preset kept flags the headless launchers remove")
	}
	args := strings.Join(l.FormatArgs(), " ")
	if !strings.Contains(args, "--disable-features=") || !strings.Contains(args, "HttpsUpgrades") {
		t.Fatalf("features %s", args)
	}
}

func TestLaunchPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ExtraFiles is not supported on windows")
	}
	l := launcher.New()
	defer l.Cleanup()
	client, err := l.LaunchPipe()
	if err != nil {
		t.Fatal(err)
	}

	b := rod.New().Client(client)
	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}
	p := b.MustPage("about:blank")
	if v := p.MustEval(`() => 1 + 1`).Int(); v != 2 {
		t.Fatalf("eval %d", v)
	}

	_ = client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if syscall.Kill(l.PID(), 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("browser did not exit after the pipe closed")
}

func TestChromeForTestingHeadlessShell(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads a browser")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	version, err := launcher.LatestChromeForTesting(ctx, nil, "Stable")
	if err != nil {
		t.Skipf("chrome for testing unreachable: %v", err)
	}

	b := launcher.NewChromeForTesting(version, true)
	b.Context = ctx
	b.RootDir = t.TempDir()
	bin, err := b.Get()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}

	l := launcher.New().Bin(bin)
	defer l.Kill()
	browser := rod.New().ControlURL(l.MustLaunch()).MustConnect()
	defer browser.MustClose()

	if v := browser.MustVersion().Product; !strings.Contains(v, version) {
		t.Fatalf("product %s, want %s", v, version)
	}
	if v := browser.MustPage("about:blank").MustEval(`() => 1 + 1`).Int(); v != 2 {
		t.Fatalf("eval %d", v)
	}
}

func TestSweepUserDataDirs(t *testing.T) {
	root := t.TempDir()
	host, _ := os.Hostname()
	mkWith := func(name, lock, owner string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if lock != "" {
			if err := os.Symlink(lock, filepath.Join(dir, "SingletonLock")); err != nil {
				t.Fatal(err)
			}
		}
		if owner != "" {
			if err := os.WriteFile(filepath.Join(dir, "rod.lock"), []byte(owner), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(dir, old, old)
		return dir
	}
	mk := func(name, lock string) string { return mkWith(name, lock, "") }
	stale := mk("stale", host+"-999999")
	noLock := mk("nolock", "")
	live := mk("live", host+"-"+strconv.Itoa(os.Getpid()))
	other := mk("other", "some-other-host-1")
	// chrome-headless-shell profiles only carry the launcher's own lock.
	shellLive := mkWith("shell-live", "", host+"-"+strconv.Itoa(os.Getpid()))
	shellStale := mkWith("shell-stale", "", host+"-999999")
	fresh := filepath.Join(root, "fresh")
	_ = os.MkdirAll(fresh, 0o755)

	n, err := launcher.SweepUserDataDirs(root, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("removed %d", n)
	}
	for _, d := range []string{stale, noLock, shellStale} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("%s not removed", d)
		}
	}
	for _, d := range []string{live, other, fresh, shellLive} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s removed", d)
		}
	}
}

func TestSweepKeepsRunningBrowser(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p")
	l := launcher.New().UserDataDir(dir)
	defer l.Cleanup()
	defer l.Kill()
	l.MustLaunch()

	n, err := launcher.SweepUserDataDirs(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("swept a running browser profile")
	}
	l.Kill()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := launcher.SweepUserDataDirs(root, 0); n == 1 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("dead browser profile not swept")
}
