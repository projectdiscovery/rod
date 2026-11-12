//go:build linux

package launcher

import "syscall"

func setPdeathsig(attr *syscall.SysProcAttr) {
	if attr == nil {
		return
	}
	attr.Pdeathsig = syscall.SIGKILL
}
