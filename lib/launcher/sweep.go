package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ownerFile records the "hostname-pid" of the browser the launcher started in
// its profile. chrome-headless-shell does not create a SingletonLock.
const ownerFile = "rod.lock"

// SweepUserDataDirs removes browser profile directories under root that no
// running browser owns, such as the ones left behind when this process was
// killed before [Launcher.Cleanup] ran. A directory is kept while its
// SingletonLock or the launcher's own lock names a live process, or while it
// is younger than minAge so a browser that is still starting is not swept. An
// empty root means [DefaultUserDataDirPrefix].
func SweepUserDataDirs(root string, minAge time.Duration) (removed int, err error) {
	if root == "" {
		root = DefaultUserDataDirPrefix
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < minAge {
			continue
		}
		if profileInUse(dir) {
			continue
		}
		if os.RemoveAll(dir) == nil {
			removed++
		}
	}
	return removed, nil
}

// writeOwner records pid as the owner of the profile dir.
func writeOwner(dir string, pid int) {
	if dir == "" {
		return
	}
	host, _ := os.Hostname()
	if os.MkdirAll(dir, 0o700) == nil {
		_ = os.WriteFile(filepath.Join(dir, ownerFile), []byte(fmt.Sprintf("%s-%d", host, pid)), 0o600)
	}
}

// profileInUse reads Chrome's SingletonLock, a symlink to "hostname-pid", and
// falls back to the launcher's own lock.
func profileInUse(dir string) bool {
	target, err := os.Readlink(filepath.Join(dir, "SingletonLock"))
	if err != nil {
		// No Chrome lock: either never started, exited cleanly, or a browser
		// that does not create one. On Windows the lock is a regular file that
		// RemoveAll cannot delete while held.
		b, err := os.ReadFile(filepath.Join(dir, ownerFile))
		if err != nil {
			return false
		}
		target = string(b)
	}
	return ownerAlive(target)
}

func ownerAlive(target string) bool {
	i := strings.LastIndex(target, "-")
	if i < 0 {
		return true
	}
	host, _ := os.Hostname()
	if target[:i] != host {
		// Owned by another host sharing this directory; leave it alone.
		return true
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return true
	}
	return processAlive(pid)
}
