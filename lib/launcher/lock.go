package launcher

import (
	"os"
	"path/filepath"
)

// lockFileFn is a variable so tests can simulate a failing lock.
var lockFileFn = lockFile

func lockDownload(dir string) (func(), error) {
	if dir == "" {
		dir = DefaultBrowserDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".download.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFileFn(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
