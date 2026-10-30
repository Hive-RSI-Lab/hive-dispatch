package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// The Hive keeps the runner on its own dispatch binary: when a status shows
// another one serving (Status.Binary), it sends its own (the "update" op),
// which the runner keeps under its Dir as bin/dispatch-<sha256> and names in
// bin/current. Every later `runner serve` the forced command starts execs
// that one (Hop), so the authorized_keys line and the binary the person
// installed stay as they are, and nothing outside the runner's Dir is
// written. Without this, each protocol change left the runner unreachable
// until the person rebuilt and copied it by hand.

// Hopped is set in the environment of a serve that Hop exec'd, so it never
// hops again.
const Hopped = "HIVE_DISPATCH_HOPPED"

var self struct {
	once sync.Once
	path string
	bin  wire.HiveBin
	err  error
}

// Self is the dispatch binary running, by path and content.
func Self() (string, wire.HiveBin, error) {
	self.once.Do(func() {
		p, err := os.Executable()
		if err == nil {
			p, err = filepath.EvalSymlinks(p)
		}
		if err != nil {
			self.err = err
			return
		}
		self.path = p
		self.bin, self.err = wire.DescribeHive(p)
	})
	return self.path, self.bin, self.err
}

func binDir(cfg Config) string { return filepath.Join(cfg.Dir, "bin") }

// Hop is the dispatch binary the Hive sent last, for serve to exec in place
// of this one, or "" to serve as this one: updates are off, none was sent,
// this is it, or this one was installed after it (a hand install wins until
// the Hive sends its own again).
func Hop(cfg Config) string {
	if cfg.NoUpdate || os.Getenv(Hopped) != "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(binDir(cfg), "current"))
	sum := strings.TrimSpace(string(raw))
	if err != nil || !sha256Hex.MatchString(sum) {
		return ""
	}
	next := filepath.Join(binDir(cfg), "dispatch-"+sum)
	nfi, err := os.Stat(next)
	if err != nil || !nfi.Mode().IsRegular() || nfi.Mode().Perm()&0o100 == 0 {
		return ""
	}
	me, err := os.Executable()
	if err == nil {
		me, err = filepath.EvalSymlinks(me)
	}
	if err != nil || me == next {
		return ""
	}
	if mfi, err := os.Stat(me); err == nil && mfi.ModTime().After(nfi.ModTime()) {
		return ""
	}
	return next
}

// update takes the Hive's dispatch binary, when it is one this machine can
// run and it says it speaks the Hive's protocol, and makes it the one later
// sessions serve from.
func update(cfg Config, c *wire.Conn) error {
	var u wire.Update
	if err := c.Expect(wire.JSON, &u); err != nil {
		return err
	}
	refuse := func(why string) error { return c.SendJSON(wire.JSON, wire.UpdateAsk{Error: why}) }
	if cfg.NoUpdate {
		return refuse(`updates are off here ("no_update" in runner.json)`)
	}
	if !sha256Hex.MatchString(u.Bin.SHA256) || u.Bin.Size <= 0 || u.Bin.Size > wire.MaxHive {
		return refuse("the dispatch binary offered wasn't named")
	}
	_, me, err := Self()
	if err != nil {
		return refuse("can't read this runner's own binary: " + err.Error())
	}
	if me.Machine != u.Bin.Machine {
		return refuse(fmt.Sprintf("the Hive's dispatch binary is built for %s, this runner for %s", u.Bin.Machine, me.Machine))
	}
	dir := binDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return refuse(err.Error())
	}
	path := filepath.Join(dir, "dispatch-"+u.Bin.SHA256)
	fi, err := os.Stat(path)
	have := err == nil && fi.Size() == u.Bin.Size
	if have {
		// Newer than a hand install again, which Hop would otherwise prefer.
		now := time.Now()
		os.Chtimes(path, now, now)
	}
	if err := c.SendJSON(wire.JSON, wire.UpdateAsk{Need: !have}); err != nil {
		return err
	}
	done := func(err error) error {
		var d wire.UpdateDone
		if err != nil {
			d.Error = err.Error()
		}
		return c.SendJSON(wire.JSON, d)
	}
	if !have {
		if err := takeBinary(path, u, c.TarReader()); err != nil {
			return done(err)
		}
	}
	if err := writeAtomic(filepath.Join(dir, "current"), []byte(u.Bin.SHA256+"\n")); err != nil {
		return done(err)
	}
	pruneBin(dir, path)
	return done(nil)
}

// takeBinary reads the binary into path, checking it is the one named and
// that it runs here and speaks the protocol it was offered for. Nothing
// replaces the binary serving until then.
func takeBinary(path string, u wire.Update, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dispatch-*")
	if err != nil {
		wire.Drain(r)
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, wire.MaxHive+1))
	tmp.Close()
	if err != nil {
		return err
	}
	if err := wire.Drain(r); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != u.Bin.Size || got != u.Bin.SHA256 {
		return fmt.Errorf("the dispatch binary sent (%d bytes, sha256 %s) isn't the one named (%d bytes, %s)", n, got, u.Bin.Size, u.Bin.SHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, tmp.Name(), "runner", "protocol").Output()
	if err != nil {
		return fmt.Errorf("the dispatch binary sent doesn't run here: %w", err)
	}
	if v, _ := strconv.Atoi(strings.TrimSpace(string(out))); v != u.Version {
		return fmt.Errorf("the dispatch binary sent says it speaks protocol %q, not %d", clip(strings.TrimSpace(string(out)), 40), u.Version)
	}
	return os.Rename(tmp.Name(), path)
}

// pruneBin removes the dispatch binaries in dir besides keep that are a day
// old. A session serving from one keeps it open.
func pruneBin(dir, keep string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if !strings.HasPrefix(e.Name(), "dispatch-") || p == keep {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
			os.Remove(p)
		}
	}
}

func writeAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
