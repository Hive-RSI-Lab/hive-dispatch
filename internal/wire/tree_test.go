package wire

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTreeRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "b", "f"), []byte("one"), 0o640)
	os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o755)
	os.Symlink("a/b/f", filepath.Join(src, "link"))
	cur, err := Scan(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Pack(&buf, src, nil, cur); err != nil {
		t.Fatal(err)
	}
	if err := Apply(dst, nil, &buf); err != nil {
		t.Fatal(err)
	}
	got, _ := Scan(dst, nil)
	for k, v := range cur {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}

	// Only the change goes the second time, and a deletion goes as a name.
	os.WriteFile(filepath.Join(src, "x"), []byte("changed"), 0o755)
	os.Remove(filepath.Join(src, "a", "b", "f"))
	next, _ := Scan(src, nil)
	buf.Reset()
	if err := Pack(&buf, src, got, next); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "one") {
		t.Error("an unchanged or deleted file was sent again")
	}
	gone := Gone(got, next)
	if len(gone) != 1 || gone[0] != "a/b/f" {
		t.Fatalf("gone = %v", gone)
	}
	if err := Apply(dst, gone, &buf); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dst, "x")); string(raw) != "changed" {
		t.Errorf("x = %q", raw)
	}
	if _, err := os.Stat(filepath.Join(dst, "a", "b", "f")); err == nil {
		t.Error("a/b/f wasn't deleted")
	}
}

func TestApplyRefusesEscapes(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	os.Symlink(outside, filepath.Join(root, "out"))
	for _, rel := range []string{"../x", "/etc/x", "out/x", "a/../../x"} {
		if _, err := SafeJoin(root, rel); err == nil {
			t.Errorf("SafeJoin(%q) was allowed", rel)
		}
	}
	// A tree that plants a link and then writes through it.
	evil := t.TempDir()
	os.Symlink(outside, filepath.Join(evil, "l"))
	os.MkdirAll(filepath.Join(evil, "d"), 0o755)
	os.WriteFile(filepath.Join(evil, "d", "f"), []byte("x"), 0o644)
	m, _ := Scan(evil, nil)
	m["l/f"] = m["d/f"]
	delete(m, "d/f")
	var buf bytes.Buffer
	// Pack reads l/f through the link on this side, which is fine; Apply on
	// the other must refuse it.
	os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o644)
	Pack(&buf, evil, nil, m)
	os.Remove(filepath.Join(outside, "f"))
	dst := t.TempDir()
	if err := Apply(dst, nil, &buf); err == nil {
		t.Error("Apply wrote through a symlink it planted")
	}
	if _, err := os.Stat(filepath.Join(outside, "f")); err == nil {
		t.Error("a file landed outside the tree")
	}
	if err := Apply(dst, []string{"../escape"}, bytes.NewReader(nil)); err == nil {
		t.Error("a deletion outside the tree was allowed")
	}
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	c := New(&buf, &buf)
	c.SendJSON(JSON, Hello{Version: Version, Op: "status"})
	w := c.TarWriter()
	w.Write(bytes.Repeat([]byte("a"), 3<<20))
	w.Close()
	var h Hello
	if err := c.Expect(JSON, &h); err != nil || h.Op != "status" {
		t.Fatalf("%v %+v", err, h)
	}
	var got bytes.Buffer
	got.ReadFrom(c.TarReader())
	if got.Len() != 3<<20 {
		t.Fatalf("tar stream of %d bytes, want %d", got.Len(), 3<<20)
	}
}
