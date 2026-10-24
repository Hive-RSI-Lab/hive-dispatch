//go:build unix

package runner

import (
	"os"
	"os/exec"
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

// setGroup starts cmd in a process group of its own, so the whole sandbox
// can be killed together.
func setGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
