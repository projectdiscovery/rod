//go:build !windows

package launcher

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	switch os.Getenv("ROD_HOLD_STDOUT") {
	case "child":
		_, _ = syscall.Setsid()
		time.Sleep(20 * time.Second)
		os.Exit(0)
	case "parent":
		// Stdout nil would be /dev/null. The grandchild has to inherit the
		// pipe this process was given, then outlive it from a new session.
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "ROD_HOLD_STDOUT=child")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Start()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestCleanupWhenChildHoldsStdout(t *testing.T) {
	l := New()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "ROD_HOLD_STDOUT=parent")
	l.setupCmd(cmd)
	if err := startLocked(l, cmd); err != nil {
		t.Fatal(err)
	}
	// Wait until the parent has exited and the grandchild has called setsid,
	// so Kill cannot reach the process that still holds the pipe.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(l.PID(), 0); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	started := time.Now()
	l.Kill()
	l.Cleanup()
	elapsed := time.Since(started)
	if elapsed < 2*time.Second || elapsed > 12*time.Second {
		t.Fatalf("cleanup took %s", elapsed)
	}
}
