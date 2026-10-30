package runner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A tool named by its absolute path is a folder that must be here; it isn't
// one of the sandbox's tools.
func TestToolTreeMustBeHere(t *testing.T) {
	dir := t.TempDir()
	tree, file := filepath.Join(dir, "eda-toolchain"), filepath.Join(dir, "not-a-folder")
	os.MkdirAll(tree, 0o755)
	os.WriteFile(file, []byte("x"), 0o755)
	gone := filepath.Join(dir, "gone")
	tools, missing := findTools(Config{Hive: "/x/hive"}, []string{"hive", tree, gone, file}, "/x/hive")
	if !slices.Equal(tools, []string{"/x/hive"}) {
		t.Errorf("tools = %v", tools)
	}
	if !slices.Equal(missing, []string{gone, file}) {
		t.Errorf("missing = %v", missing)
	}
}
