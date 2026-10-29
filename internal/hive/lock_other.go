//go:build !unix

package hive

import (
	"errors"
	"os"
)

var errUnix = errors.New("needs a unix system")

func tryLock(string) (*os.File, error)  { return nil, errUnix }
func waitLock(string) (*os.File, error) { return nil, errUnix }
