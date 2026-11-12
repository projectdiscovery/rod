//go:build !windows

package launcher

import (
	"os"

	"github.com/go-rod/rod/lib/launcher/flags"
)

func (l *Launcher) preparePrivileges() error {
	if !l.dropPriv {
		return nil
	}
	dir := l.Get(flags.UserDataDir)
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Chown(dir, l.uid, l.gid)
}
