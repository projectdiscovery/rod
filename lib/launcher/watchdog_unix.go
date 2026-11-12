//go:build !linux && !windows

package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// osPipe and execCommand are variables so tests can simulate a failing
// watchdog start.
var (
	osPipe      = os.Pipe
	execCommand = exec.Command
)

// afterStart starts a /bin/sh watchdog that SIGKILLs the browser's process
// group once it reads EOF on a pipe only this process holds open, which
// happens when this process dies, even by SIGKILL. The watchdog has its own
// process group and ignores INT, TERM and HUP, so a Ctrl-C that kills this
// process does not take the watchdog with it. stop must run after the browser
// exits, so the watchdog never signals a reused process group.
func afterStart(cmd *exec.Cmd) (stop func()) {
	stop = func() {}
	if cmd.Process == nil {
		return
	}
	r, w, err := osPipe()
	if err != nil {
		return
	}
	script := fmt.Sprintf("trap '' INT TERM HUP; read x <&3; kill -9 -%d 2>/dev/null", cmd.Process.Pid)
	wd := execCommand("/bin/sh", "-c", script)
	wd.ExtraFiles = []*os.File{r}
	wd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = wd.Start()
	_ = r.Close()
	if err != nil {
		_ = w.Close()
		return
	}
	return func() {
		_ = wd.Process.Kill()
		_ = wd.Wait()
		_ = w.Close()
	}
}
