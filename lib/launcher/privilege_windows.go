//go:build windows

package launcher

import "errors"

func (l *Launcher) preparePrivileges() error {
	if l.dropPriv {
		return errors.New("launcher: DropPrivileges is not supported on windows")
	}
	return nil
}
