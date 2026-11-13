//go:build !windows

package launcher

import (
	"os/exec"
	"testing"
)

func TestDropPrivilegesCredential(t *testing.T) {
	l := New().DropPrivileges(1, 2)
	cmd := exec.CommandContext(t.Context(), "true")
	l.osSetupCmd(cmd)
	cred := cmd.SysProcAttr.Credential
	if cred == nil || cred.Uid != 1 || cred.Gid != 2 {
		t.Fatalf("%+v", cred)
	}
}
