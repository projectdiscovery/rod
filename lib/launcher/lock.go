package launcher

import (
	"os"
	"path/filepath"
)

func lockDownload(dir string) (func(), error) {
	if dir == "" {
		dir = DefaultBrowserDir
	}
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".download.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	err = lockFile(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
