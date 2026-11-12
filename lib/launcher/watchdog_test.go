//go:build !linux && !windows

package launcher

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAfterStartWithoutProcess(t *testing.T) {
	cmd := exec.Command("true")
	stop := afterStart(cmd)
	stop()
}

func TestAfterStartPipeError(t *testing.T) {
	old := osPipe
	t.Cleanup(func() { osPipe = old })
	osPipe = func() (*os.File, *os.File, error) {
		return nil, nil, errors.New("no pipe")
	}
	// A real process is needed for the pid; use this test binary.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := afterStart(cmd)
	stop()
	_ = cmd.Wait()
}

func TestAfterStartWatchdogStartError(t *testing.T) {
	old := execCommand
	t.Cleanup(func() { execCommand = old })
	execCommand = func(string, ...string) *exec.Cmd {
		return exec.Command(filepath.Join(t.TempDir(), "missing"))
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := afterStart(cmd)
	stop()
	_ = cmd.Wait()
}
