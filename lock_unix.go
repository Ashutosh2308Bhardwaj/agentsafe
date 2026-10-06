//go:build unix

package agentsafe

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes a non-blocking exclusive flock. flock locks belong to the open file description, so two
// FileLogs on the same path exclude each other even inside one process, and the kernel drops the lock when
// the descriptor is closed, including when the process is killed.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return lockedErr(f.Name())
	}
	return err
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// waitLock takes an exclusive flock, waiting for it.
func waitLock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) { // a signal interrupted the wait: wait again
			return err
		}
	}
}
