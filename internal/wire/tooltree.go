package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A tool tree under /opt goes across when it is this small: chip-flow's
// and pdf-material-builder's (7.7 and 5.1 MB, 10-30), not the EDA
// toolchain's many GB, which a runner must have of its own.
const (
	MaxToolTree      = 64 << 20
	MaxToolTreeFiles = 20000
)

// ToolTree names a tool tree the turn needs (Path, as the turn sees it) by
// its content, so a runner keeps one copy of each and takes it once.
type ToolTree struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Files  int    `json:"files"`
}

// DescribeTree names the tree at root by its content: each entry's path,
// kind, file mode and bytes, or link target. No symlink under root is
// followed. A tree over MaxToolTree or MaxToolTreeFiles is refused before
// any file is read. man is root's scan, for Pack.
func DescribeTree(root string) (tt ToolTree, man Manifest, err error) {
	man, err = Scan(root, nil)
	if err != nil {
		return tt, nil, err
	}
	paths := make([]string, 0, len(man))
	for p, e := range man {
		paths = append(paths, p)
		if strings.HasPrefix(e, "f:") {
			var size int64
			fmt.Sscanf(e, "f:%d:", &size)
			tt.Size += size
			tt.Files++
		}
	}
	if tt.Size > MaxToolTree || len(man) > MaxToolTreeFiles {
		return tt, nil, fmt.Errorf("%s is %d bytes in %d entries, over the %d bytes and %d entries a tool tree sent may be", root, tt.Size, len(man), MaxToolTree, MaxToolTreeFiles)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		e := man[p]
		kind, rest, _ := strings.Cut(e, ":")
		switch kind {
		case "d":
			fmt.Fprintf(h, "d %s\x00", p)
		case "l":
			fmt.Fprintf(h, "l %s\x00%s\x00", p, rest)
		case "f":
			sum, err := fileSum(filepath.Join(root, filepath.FromSlash(p)))
			if err != nil {
				return tt, nil, err
			}
			perm := rest[strings.LastIndex(rest, ":")+1:]
			fmt.Fprintf(h, "f %s\x00%s %s\x00", p, perm, sum)
		}
	}
	tt.SHA256 = hex.EncodeToString(h.Sum(nil))
	return tt, man, nil
}

// fileSum is the sha256 of the regular file at p, not followed if it has
// since become a symlink.
func fileSum(p string) (string, error) {
	f, err := openPlain(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openPlain(p string) (*os.File, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a regular file", p)
	}
	return os.Open(p)
}
