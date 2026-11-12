//go:build !windows

package rod

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
)

// A default local Connect must talk CDP over the debugging pipe and open no
// debugging port, and Close must take the browser down with it.
func TestConnectDefaultsToPipe(t *testing.T) {
	if !launcher.PipeSupported() {
		t.Skip("no pipe on this platform")
	}

	b := New()
	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}

	pid := b.launcher.PID()
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	args := string(out)
	if !strings.Contains(args, "--remote-debugging-pipe") {
		t.Fatalf("browser not launched over pipe: %s", args)
	}
	if strings.Contains(args, "--remote-debugging-port") {
		t.Fatalf("browser opened a debugging port: %s", args)
	}

	if _, err := b.Version(); err != nil {
		t.Fatal(err)
	}

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("browser %d still running after Close", pid)
}
