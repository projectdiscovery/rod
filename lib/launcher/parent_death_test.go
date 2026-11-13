//go:build !windows

package launcher_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
)

//nolint:gocognit // the test runs as a subprocess and then checks the browser is gone
func TestBrowserDiesWithParent(t *testing.T) {
	if os.Getenv("ROD_TEST_PARENT") == "1" {
		l := launcher.New().NoSandbox(true)
		l.MustLaunch()
		_, _ = fmt.Fprintf(os.Stdout, "browser-pid=%d\n", l.PID())
		select {}
	}

	for _, sig := range []syscall.Signal{syscall.SIGKILL, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestBrowserDiesWithParent$")
			cmd.Env = append(os.Environ(), "ROD_TEST_PARENT=1")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			err = cmd.Start()
			if err != nil {
				t.Fatal(err)
			}

			pid := 0
			scanner := bufio.NewScanner(out)
			for scanner.Scan() {
				if v, ok := strings.CutPrefix(scanner.Text(), "browser-pid="); ok {
					pid, _ = strconv.Atoi(v)
					break
				}
			}
			if pid == 0 {
				_ = cmd.Process.Kill()
				t.Fatal("child did not launch a browser")
			}

			// SIGINT goes to the whole group, like Ctrl-C in a terminal.
			if sig == syscall.SIGINT {
				_ = syscall.Kill(-cmd.Process.Pid, sig)
			} else {
				_ = cmd.Process.Signal(sig)
			}
			_ = cmd.Wait()

			deadline := time.Now().Add(10 * time.Second)
			for running(pid) {
				if time.Now().After(deadline) {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					t.Fatal("browser outlived its parent")
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

// running treats a zombie as gone: without an init that reaps, such as in a
// container, a killed orphan stays a zombie.
func running(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err == nil {
		_, state, _ := strings.Cut(string(b), ") ")
		return !strings.HasPrefix(state, "Z")
	}
	out, err := exec.CommandContext(context.Background(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
