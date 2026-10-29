//go:build unix

package hive

import (
	"os"
	"syscall"
)

// tryLock takes an exclusive lock on path, or fails at once if it is held.
func tryLock(path string) (*os.File, error) { return lock(path, syscall.LOCK_EX|syscall.LOCK_NB) }

// waitLock takes an exclusive lock on path, waiting for it.
func waitLock(path string) (*os.File, error) { return lock(path, syscall.LOCK_EX) }

func lock(path string, how int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
