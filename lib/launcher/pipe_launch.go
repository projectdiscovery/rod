package launcher

import (
	"context"
	"os"
	"os/exec"
	"runtime"

	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher/flags"
)

// PipeSupported reports whether LaunchPipe can run on this platform. Go does
// not pass ExtraFiles to child processes on Windows, so there the browser has
// to be launched on a debugging port instead.
func PipeSupported() bool {
	return runtime.GOOS != goosWindows
}

// LaunchPipe starts the browser with --remote-debugging-pipe and returns a
// CDP client on that pipe. No debugging port is opened. Closing the client
// closes the pipe, which makes Chrome exit.
func (l *Launcher) LaunchPipe() (*cdp.Client, error) {
	if l.hasLaunched() {
		return nil, ErrAlreadyLaunched
	}
	l.applyCompliant()
	defer l.ctxCancel()

	bin, err := l.getBin()
	if err != nil {
		return nil, err
	}
	l.Delete(flags.RemoteDebuggingPort)

	pair, err := cdp.NewPipePair()
	if err != nil {
		return nil, err
	}

	l.setupUserPreferences()
	err = l.preparePrivileges()
	if err != nil {
		return nil, err
	}
	args := l.FormatArgs()
	args = append(args, "--remote-debugging-pipe")

	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.ExtraFiles = []*os.File{pair.ChromeRead, pair.ChromeWrite}
	l.setupCmd(cmd)

	err = startLocked(l, cmd)
	if err != nil {
		err = l.annotate(err)
		_ = pair.ChromeRead.Close()
		_ = pair.ChromeWrite.Close()
		_ = pair.ParentRead.Close()
		_ = pair.ParentWrite.Close()
		return nil, err
	}
	_ = pair.ChromeRead.Close()
	_ = pair.ChromeWrite.Close()

	return cdp.New().Start(cdp.NewPipe(pair.ParentRead, pair.ParentWrite)), nil
}
