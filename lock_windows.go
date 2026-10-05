//go:build windows

package agentsafe

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// LockFileEx / UnlockFileEx from kernel32, called directly so the core keeps zero dependencies
// (golang.org/x/sys/windows would wrap the same two calls). Windows releases the lock when the handle is
// closed, including when the process is terminated.
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errLockViolation        = syscall.Errno(33)
)

func tryLock(f *os.File) error {
	var ol syscall.Overlapped
	r1, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0,
		uintptr(unsafe.Pointer(&ol))) //nolint:gosec // required by the Win32 API
	if r1 == 0 {
		if errors.Is(err, errLockViolation) {
			return lockedErr(f.Name())
		}
		return err
	}
	return nil
}

func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	r1, _, err := procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol))) //nolint:gosec // Win32 API
	if r1 == 0 {
		return err
	}
	return nil
}
