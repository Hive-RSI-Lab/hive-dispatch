package runner

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/spec"
	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// TreeKeep is how long a kept tool tree no turn has used stays.
const TreeKeep = 7 * 24 * time.Hour

// treesFor are where the tool trees the Hive sent names (Turn.Trees) are
// kept here, by the turn's path, and the paths of those not kept yet, which
// the Hive sends first. The Hive's copy is used even where this machine has
// a tree at that path: it is the version the turn's checkout was written
// against (/opt/chip-flow is a pinned commit). A tree named oddly is left
// out, and findTools looks for it here as before.
func treesFor(cfg Config, t wire.Turn) (kept map[string]string, need []string) {
	kept = map[string]string{}
	dir := filepath.Join(cfg.Dir, "trees")
	for _, tt := range t.Trees {
		p := tt.Path
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || filepath.Dir(p) == "/" || !slices.Contains(t.Tools, p) || !sha256Hex.MatchString(tt.SHA256) || tt.Size > wire.MaxToolTree {
			continue
		}
		if _, dup := kept[p]; dup {
			continue
		}
		os.MkdirAll(dir, 0o700)
		at := filepath.Join(dir, tt.SHA256)
		kept[p] = at
		if fi, err := os.Lstat(at); err == nil && fi.IsDir() {
			now := time.Now()
			os.Chtimes(at, now, now) // in use: pruneTrees keeps it
			continue
		}
		need = append(need, p)
	}
	return kept, need
}

// treeNamed is the tree at path p among t.Trees.
func treeNamed(t wire.Turn, p string) wire.ToolTree {
	for _, tt := range t.Trees {
		if tt.Path == p {
			return tt
		}
	}
	return wire.ToolTree{}
}

// takeTree reads the tool tree the Hive sends into at, checking it is the
// one named, and drops the trees no turn has used within TreeKeep.
func takeTree(at string, want wire.ToolTree, r io.Reader) error {
	tmp, err := os.MkdirTemp(filepath.Dir(at), ".take-")
	if err != nil {
		wire.Drain(r)
		return err
	}
	defer os.RemoveAll(tmp)
	if err := wire.Apply(tmp, nil, io.LimitReader(r, wire.MaxToolTree+int64(wire.MaxToolTreeFiles)*4096)); err != nil {
		wire.Drain(r)
		return fmt.Errorf("taking %s: %w", want.Path, err)
	}
	if err := wire.Drain(r); err != nil {
		return err
	}
	got, _, err := wire.DescribeTree(tmp)
	if err != nil {
		return fmt.Errorf("taking %s: %w", want.Path, err)
	}
	if got.SHA256 != want.SHA256 {
		return fmt.Errorf("the tool tree sent for %s (sha256 %s) isn't the one named (%s)", want.Path, got.SHA256, want.SHA256)
	}
	os.Chmod(tmp, 0o755)
	if err := os.Rename(tmp, at); err != nil {
		// Another turn took the same tree meanwhile: its copy is this one.
		if fi, serr := os.Lstat(at); serr != nil || !fi.IsDir() {
			return err
		}
	}
	pruneTrees(filepath.Dir(at), at)
	return nil
}

// pruneTrees removes the kept trees in dir besides keep that no turn has
// used within TreeKeep, and takes a dead session left half-done.
func pruneTrees(dir, keep string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if fi, err := e.Info(); err == nil && p != keep && time.Since(fi.ModTime()) > TreeKeep {
			os.RemoveAll(p) // Apply left each folder writable to this user
		}
	}
}

// treeMounts are the mounts that put the kept trees at their paths inside
// the sandbox. The sandbox binds a tree's parent (/opt) read-only from this
// machine, where nothing may be at that path to mount on, so the parent is
// covered by a folder of the turn's own (over) holding a place for each of
// the machine's entries there, each bound back from the machine (a symlink
// copied as one), and for each tree, bound from where it is kept.
func treeMounts(over string, kept map[string]string) ([]spec.Mount, error) {
	byParent := map[string][]string{}
	for p := range kept {
		byParent[filepath.Dir(p)] = append(byParent[filepath.Dir(p)], p)
	}
	parents := make([]string, 0, len(byParent))
	for d := range byParent {
		parents = append(parents, d)
	}
	sort.Strings(parents)
	var out []spec.Mount
	for i, parent := range parents {
		paths := byParent[parent]
		sort.Strings(paths)
		at := filepath.Join(over, fmt.Sprint(i))
		if err := os.MkdirAll(at, 0o755); err != nil {
			return nil, err
		}
		mounts := []spec.Mount{{Host: at, Inside: parent}}
		ents, err := os.ReadDir(parent)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, e := range ents {
			host := filepath.Join(parent, e.Name())
			if slices.Contains(paths, host) {
				continue
			}
			fi, err := os.Lstat(host)
			if err != nil {
				continue
			}
			place := filepath.Join(at, e.Name())
			switch {
			case fi.Mode()&fs.ModeSymlink != 0:
				if target, err := os.Readlink(host); err == nil {
					os.Symlink(target, place)
				}
				continue
			case fi.IsDir():
				err = os.Mkdir(place, 0o755)
			case fi.Mode().IsRegular():
				err = os.WriteFile(place, nil, 0o644)
			default:
				continue
			}
			if err != nil {
				return nil, err
			}
			mounts = append(mounts, spec.Mount{Host: host, Inside: host})
		}
		for _, p := range paths {
			if err := os.Mkdir(filepath.Join(at, filepath.Base(p)), 0o755); err != nil {
				return nil, err
			}
			mounts = append(mounts, spec.Mount{Host: kept[p], Inside: p})
		}
		out = append(out, mounts...)
	}
	return out, nil
}
