//go:build !linux && !windows

package launcher

import "syscall"

func setPdeathsig(_ *syscall.SysProcAttr) {}
