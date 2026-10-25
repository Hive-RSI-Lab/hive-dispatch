package wire

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Manifest is a tree's state: relative slash path to "f:size:mtime-ns:perm",
// "d:perm" or "l:target". Two equal entries are taken to be the same file.
type Manifest map[string]string

// Skip says whether a relative path (and, for a folder, all under it) is left
// out of a scan.
type Skip func(rel string) bool

// Scan reads the tree at root without following symlinks. A missing root is
// an empty tree.
func Scan(root string, skip Skip) (Manifest, error) {
	m := Manifest{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			m[rel] = "l:" + t
		case fi.IsDir():
			m[rel] = fmt.Sprintf("d:%o", fi.Mode().Perm())
		case fi.Mode().IsRegular():
			m[rel] = fmt.Sprintf("f:%d:%d:%o", fi.Size(), fi.ModTime().UnixNano(), fi.Mode().Perm())
		}
		return nil
	})
	return m, err
}

// Gone is what base has that cur doesn't, for the other side to delete.
func Gone(base, cur Manifest) []string {
	var gone []string
	for p := range base {
		if _, ok := cur[p]; !ok {
			gone = append(gone, p)
		}
	}
	sort.Strings(gone)
	return gone
}

// Pack writes to w a tar of what in cur (a scan of root) differs from base
// (the other side's copy).
func Pack(w io.Writer, root string, base, cur Manifest) error {
	tw := tar.NewWriter(w)
	var paths []string
	for p, e := range cur {
		if base[p] != e {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, rel := range paths {
		e := cur[rel]
		host := filepath.Join(root, filepath.FromSlash(rel))
		kind, rest, _ := strings.Cut(e, ":")
		switch kind {
		case "d":
			perm, _ := strconv.ParseInt(rest, 8, 32)
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: perm}); err != nil {
				return err
			}
		case "l":
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: rel, Linkname: rest, Mode: 0o777}); err != nil {
				return err
			}
		case "f":
			if err := packFile(tw, host, rel); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

func packFile(tw *tar.Writer, host, rel string) error {
	f, err := os.Open(host)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // gone since the scan
		}
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	h := &tar.Header{Typeflag: tar.TypeReg, Name: rel, Size: fi.Size(), Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime(), Format: tar.FormatPAX}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	// A file that grew since its stat is cut to the size in its header.
	_, err = io.CopyN(tw, f, fi.Size())
	return err
}

// Apply deletes gone under root, then writes the tar in r there. No path may
// leave root: one that is absolute, climbs out with "..", or passes through
// a symlink is refused, so a tree can't write through a link it planted.
func Apply(root string, gone []string, r io.Reader) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(gone)))
	for _, rel := range gone {
		p, err := SafeJoin(root, rel)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := SafeJoin(root, strings.TrimSuffix(h.Name, "/"))
		if err != nil {
			return err
		}
		perm := fs.FileMode(h.Mode).Perm()
		fi, statErr := os.Lstat(p)
		switch h.Typeflag {
		case tar.TypeDir:
			if statErr == nil && !fi.IsDir() {
				os.RemoveAll(p)
			}
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			os.Chmod(p, perm|0o700)
		case tar.TypeSymlink:
			if statErr == nil {
				os.RemoveAll(p)
			}
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeReg:
			if statErr == nil {
				os.RemoveAll(p)
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
			os.Chmod(p, perm)
			mt := h.ModTime
			if mt.IsZero() {
				mt = time.Now()
			}
			if err := os.Chtimes(p, mt, mt); err != nil {
				return err
			}
		}
	}
}

// SafeJoin is root/rel, refused when rel would leave root or pass through a
// symlink on the way. Parents that don't exist are made as folders.
func SafeJoin(root, rel string) (string, error) {
	clean := path.Clean("/" + filepath.ToSlash(rel))[1:]
	if clean == "" || rel != clean && rel != clean+"/" || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("unsafe path %q", rel)
	}
	parts := strings.Split(clean, "/")
	p := root
	for _, part := range parts[:len(parts)-1] {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(p, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("unsafe path %q: %s is not a real folder", rel, part)
		}
	}
	return filepath.Join(p, parts[len(parts)-1]), nil
}
