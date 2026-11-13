//go:build windows

package launcher

import (
	"os"
	"syscall"
	"unsafe"
)

// lockfileExclusiveLock is LOCKFILE_EXCLUSIVE_LOCK. Without
// LOCKFILE_FAIL_IMMEDIATELY the call blocks until the lock is granted.
const lockfileExclusiveLock = 0x2

var (
	modKernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modKernel32.NewProc("LockFileEx")
	procUnlockFileEx = modKernel32.NewProc("UnlockFileEx")
)

// lockFile takes an exclusive lock on the first byte of f. Parallel test
// binaries and processes share the browser directory, so a no-op here lets
// two installs race on the same directory.
func lockFile(f *os.File) error {
	ol := new(syscall.Overlapped)
	r1, _, err := procLockFileEx.Call(
		f.Fd(), lockfileExclusiveLock, 0, 1, 0, uintptr(unsafe.Pointer(ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func unlockFile(f *os.File) error {
	ol := new(syscall.Overlapped)
	r1, _, err := procUnlockFileEx.Call(
		f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}
