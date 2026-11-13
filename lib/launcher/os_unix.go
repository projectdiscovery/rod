//go:build !windows

package launcher

import (
	"context"
	"os/exec"
	"syscall"

	"github.com/go-rod/rod/lib/launcher/flags"
)

func killGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func (l *Launcher) osSetupCmd(cmd *exec.Cmd) {
	if flags, has := l.GetFlags(flags.XVFB); has {
		var command []string
		// flags must append before cmd.Args
		command = append(command, flags...)
		command = append(command, cmd.Args...)

		*cmd = *exec.CommandContext(context.Background(), "xvfb-run", command...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setPdeathsig(cmd.SysProcAttr)
	if l.dropPriv {
		cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid: uint32(l.uid),
			Gid: uint32(l.gid),
		}
	}
}
