package hive

import (
	"os"
	"testing"
)

// module.json execs ./run, and the core execs `run turn --spec` for each
// delegated turn: it must stay executable.
func TestRunIsExecutable(t *testing.T) {
	fi, err := os.Stat("../../run")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 != 0o111 {
		t.Fatalf("run is %v; it must be executable (git update-index --chmod=+x run)", fi.Mode().Perm())
	}
}
