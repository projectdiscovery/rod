//go:build linux

package launcher

import "os/exec"

// afterStart has nothing to do on Linux, where Pdeathsig kills the browser.
func afterStart(*exec.Cmd) (stop func()) { return func() {} }
