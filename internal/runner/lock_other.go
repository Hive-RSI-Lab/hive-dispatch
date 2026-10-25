//go:build !unix

package runner

import (
	"errors"
	"os"
	"os/exec"
)

var errUnix = errors.New("the runner needs Linux")

func tryLock(string) (*os.File, error)  { return nil, errUnix }
func waitLock(string) (*os.File, error) { return nil, errUnix }
func setGroup(*exec.Cmd)                {}
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
