package launcher

import (
	"testing"
	"time"
)

// TestLockDownloadExcludes checks that a second lock on the same directory
// waits until the first is released. On Windows this fails if the lock is a no-op.
func TestLockDownloadExcludes(t *testing.T) {
	dir := t.TempDir()

	unlock, err := lockDownload(dir)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan error, 1)
	go func() {
		u, err := lockDownload(dir)
		if err == nil {
			u()
		}
		acquired <- err
	}()

	select {
	case <-acquired:
		unlock()
		t.Fatal("second lock acquired while the first was held")
	case <-time.After(200 * time.Millisecond):
	}

	unlock()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second lock not acquired after release")
	}
}
