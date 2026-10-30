package wire

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A tree is named by its content: a copy taken across names the same, a
// touched file doesn't change it, a changed byte or mode does, and a tree
// too big to send is refused.
func TestDescribeTree(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "f"), []byte("one"), 0o644)
	os.Symlink("/elsewhere", filepath.Join(src, "l"))
	tt, man, err := DescribeTree(src)
	if err != nil || tt.Files != 1 || tt.Size != 3 {
		t.Fatalf("%+v %v", tt, err)
	}
	var buf bytes.Buffer
	if err := Pack(&buf, src, nil, man); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := Apply(dst, nil, &buf); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := DescribeTree(dst); got.SHA256 != tt.SHA256 {
		t.Errorf("the copy names %s, the tree %s", got.SHA256, tt.SHA256)
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(src, "a", "f"), later, later)
	if got, _, _ := DescribeTree(src); got.SHA256 != tt.SHA256 {
		t.Error("a touched file changed the tree's name")
	}
	os.Chmod(filepath.Join(src, "a", "f"), 0o755)
	if got, _, _ := DescribeTree(src); got.SHA256 == tt.SHA256 {
		t.Error("a changed mode kept the tree's name")
	}
	os.WriteFile(filepath.Join(src, "a", "f"), []byte("two"), 0o755)
	if got, _, _ := DescribeTree(dst); got.SHA256 != tt.SHA256 {
		t.Error("the copy changed")
	}
	f, _ := os.Create(filepath.Join(src, "big"))
	f.Truncate(MaxToolTree + 1)
	f.Close()
	if _, _, err := DescribeTree(src); err == nil {
		t.Error("a tree over MaxToolTree was named")
	}
}
