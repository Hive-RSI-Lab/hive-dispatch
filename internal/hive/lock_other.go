//go:build !unix

package hive

import "os"

// waitLock takes no lock where flock isn't: handovers are counted, unguarded.
func waitLock(path string) (*os.File, error) { return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) }
