//go:build !linux && !windows

package launcher

import "syscall"

func setPdeathsig(attr *syscall.SysProcAttr) {
	// Pdeathsig only exists on Linux; keep the parameter used so the empty
	// body still has a statement for coverage.
	_ = attr
}
