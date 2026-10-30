// Package runner is the side of the link on the other machine: it answers
// how much room it has, and runs a turn the Hive sends in the same sandbox the
// Hive uses (`hive sandbox-exec`, bubblewrap or the hive binary's own
// namespaces), with the turn's files unpacked into a folder of its own that is
// wiped when the turn ends.
//
// It is started once per session, on the stdin and stdout of an SSH session
// whose key may run nothing else (a forced command), and listens on nothing.
package runner

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/spec"
	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// Config is the runner's own settings, set by the person on its machine.
type Config struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`  // where turns and the shared cache live
	Hive string `json:"hive"` // the hive binary that runs sandbox-exec
	// The runner takes a turn while its load per core is under MaxLoad and it
	// has MinFreeMB of memory free, counting turns that started in the last
	// two minutes as a core and a GB each (the load average lags). Max, when
	// set, is a fixed cap on turns at once besides; 0 is none.
	MaxLoad   float64           `json:"max_load,omitempty"`
	MinFreeMB int               `json:"min_free_mb,omitempty"`
	Max       int               `json:"max_turns,omitempty"`
	Tools     map[string]string `json:"tools"` // tool name -> path on this machine, where it isn't on PATH
	// NoUpdate keeps this runner on the binary the person installed: the
	// Hive's own isn't taken (Update) and none it sent before is served.
	NoUpdate bool `json:"no_update,omitempty"`
}

// Defaults for a runner's room, the same the Hive holds it to.
const (
	DefaultMaxLoad   = 0.75
	DefaultMinFreeMB = 4096
)

// A turn that started within Young counts as YoungLoad of load and YoungMB
// of memory, until the load average and the memory reading have caught up.
const (
	Young     = 2 * time.Minute
	YoungLoad = 1.0
	YoungMB   = 1024
)

// Defaults fills what the person left out.
func (c *Config) Defaults() {
	if c.Name == "" {
		c.Name, _ = os.Hostname()
	}
	if c.Dir == "" {
		home, _ := os.UserHomeDir()
		c.Dir = filepath.Join(home, ".local", "share", "hive-dispatch")
	}
	if c.Hive == "" {
		c.Hive = "hive"
		if p, err := exec.LookPath("hive"); err == nil {
			c.Hive = p
		}
	}
	if c.MaxLoad <= 0 {
		c.MaxLoad = DefaultMaxLoad
	}
	if c.MinFreeMB <= 0 {
		c.MinFreeMB = DefaultMinFreeMB
	}
}

// Serve speaks one session on r and w.
func Serve(cfg Config, r io.Reader, w io.Writer) error {
	cfg.Defaults()
	c := wire.New(r, w)
	var h wire.Hello
	if err := c.Expect(wire.JSON, &h); err != nil {
		return err
	}
	if h.Op == "update" {
		return update(cfg, c)
	}
	if h.Version != wire.Version {
		_, me, _ := Self()
		return c.SendJSON(wire.JSON, wire.Status{Name: cfg.Name, Version: wire.Version, Binary: me.SHA256, Error: fmt.Sprintf("protocol %d, this runner speaks %d", h.Version, wire.Version)})
	}
	switch h.Op {
	case "status":
		return c.SendJSON(wire.JSON, Room(cfg))
	case "turn":
		return turn(cfg, c)
	}
	return fmt.Errorf("unknown op %q", h.Op)
}

// Room reads this machine's room.
func Room(cfg Config) wire.Status {
	cfg.Defaults()
	st := wire.Status{Name: cfg.Name, Version: wire.Version, Cores: runtime.NumCPU(), Max: cfg.Max}
	st.MemTotalMB = int(memField("MemTotal:") / 1024)
	st.MemFreeMB, st.Load1 = readRoom()
	st.Running, st.Young = running(cfg)
	st.Sandbox = probe(cfg)
	_, me, _ := Self()
	st.Binary = me.SHA256
	return st
}

// readRoom reads the memory free and the load now.
func readRoom() (freeMB int, load1 float64) {
	freeMB = int(memField("MemAvailable:") / 1024)
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 0 {
			load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	return freeMB, load1
}

// full says why one more turn doesn't fit: n turns run, young of them
// started within Young, on a machine with freeMB free and load1 on cores.
// "" is room.
func full(cfg Config, n, young, freeMB int, load1 float64, cores int) string {
	load := load1 + float64(young)*YoungLoad
	free := freeMB - young*YoungMB
	switch {
	case cfg.Max > 0 && n >= cfg.Max:
		return fmt.Sprintf("full (%d of %d turns running)", n, cfg.Max)
	case cores > 0 && load/float64(cores) > cfg.MaxLoad:
		return fmt.Sprintf("loaded (%.1f on %d cores, %d turns running, %d just started)", load, cores, n, young)
	case free < cfg.MinFreeMB:
		return fmt.Sprintf("low on memory (%d MB free, %d turns running, %d just started)", free, n, young)
	}
	return ""
}

func memField(key string) float64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	_, rest, ok := strings.Cut(string(raw), key)
	if !ok {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.Fields(rest)[0], 64)
	return f
}

// probe asks the hive binary whether its sandbox runs here, and keeps the
// answer five minutes.
func probe(cfg Config) string {
	cache := filepath.Join(cfg.Dir, "probe")
	if fi, err := os.Stat(cache); err == nil && time.Since(fi.ModTime()) < 5*time.Minute {
		raw, _ := os.ReadFile(cache)
		return string(raw)
	}
	out, err := exec.Command(cfg.Hive, "sandbox-probe").CombinedOutput()
	got := strings.TrimSpace(string(out))
	if err != nil {
		got = "unavailable: " + clip(got+" "+err.Error(), 300)
	}
	os.MkdirAll(cfg.Dir, 0o700)
	os.WriteFile(cache, []byte(got), 0o600)
	return got
}

// running counts turns whose session still holds its lock, and wipes the
// folders of any that ended without cleaning up (a dropped link, a reboot),
// and how many of them started within Young.
func running(cfg Config) (n, young int) {
	os.MkdirAll(filepath.Join(cfg.Dir, "turns"), 0o700)
	all, err := waitLock(filepath.Join(cfg.Dir, "turns.lock"))
	if err != nil {
		return 0, 0
	}
	defer all.Close()
	return count(cfg)
}

// claim takes a folder and a place for one more turn, if there is room.
func claim(cfg Config, name string) (string, *os.File, error) {
	os.MkdirAll(filepath.Join(cfg.Dir, "turns"), 0o700)
	all, err := waitLock(filepath.Join(cfg.Dir, "turns.lock"))
	if err != nil {
		return "", nil, err
	}
	defer all.Close()
	n, young := count(cfg)
	freeMB, load1 := readRoom()
	if why := full(cfg, n, young, freeMB, load1, runtime.NumCPU()); why != "" {
		return "", nil, errors.New(why)
	}
	dir, err := os.MkdirTemp(filepath.Join(cfg.Dir, "turns"), name+"-")
	if err != nil {
		return "", nil, err
	}
	l, err := tryLock(filepath.Join(dir, "lock"))
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, l, nil
}

// count is running's count, under the turns lock, and how many of those
// started within Young.
func count(cfg Config) (n, young int) {
	dirs, _ := filepath.Glob(filepath.Join(cfg.Dir, "turns", "*"))
	for _, d := range dirs {
		l, err := tryLock(filepath.Join(d, "lock"))
		if err != nil {
			n++
			if fi, err := os.Stat(filepath.Join(d, "lock")); err == nil && time.Since(fi.ModTime()) < Young {
				young++
			}
			continue
		}
		os.RemoveAll(d)
		l.Close()
	}
	return n, young
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func turn(cfg Config, c *wire.Conn) error {
	var t wire.Turn
	if err := c.Expect(wire.JSON, &t); err != nil {
		return err
	}
	refuse := func(why string) error { return c.SendJSON(wire.JSON, wire.Manifests{Error: why}) }
	if sb := probe(cfg); !strings.HasPrefix(sb, "ok") {
		return refuse("no sandbox here: " + sb)
	}
	hive, needHive, why := hiveFor(cfg, t)
	if why != "" {
		return refuse(why)
	}
	tools, missing := findTools(cfg, t.Tools, hive)
	if len(missing) > 0 {
		return refuse(wire.NoTool + strings.Join(missing, ", ") + " (put a program on PATH or name its path under \"tools\" in runner.json; a folder must be at the same path here)")
	}
	dir, lock, err := claim(cfg, unsafeName.ReplaceAllString(t.ID+"-"+t.Label, "_"))
	if err != nil {
		return refuse(err.Error())
	}
	// Everything the turn brought (its home, its prompt, the spec with its
	// token) goes when it ends.
	defer func() { os.RemoveAll(dir); lock.Close() }()
	empty := filepath.Join(dir, "empty")
	os.MkdirAll(empty, 0o755)

	hosts := make([]string, len(t.Mounts))
	man := wire.Manifests{Mounts: map[int]wire.Manifest{}, NeedHive: needHive}
	for i, m := range t.Mounts {
		switch {
		case m.Hide || m.Kind == wire.KindEmpty:
			hosts[i] = empty
		case m.Kind == wire.KindShared:
			hosts[i] = filepath.Join(cfg.Dir, "shared", unsafeName.ReplaceAllString(m.Key, "_"))
			os.MkdirAll(hosts[i], 0o755)
			if man.Mounts[i], err = wire.Scan(hosts[i], nil); err != nil {
				return refuse(err.Error())
			}
		default:
			hosts[i] = filepath.Join(dir, "m"+strconv.Itoa(i))
		}
	}
	if err := c.SendJSON(wire.JSON, man); err != nil {
		return err
	}
	if needHive {
		if err := takeHive(hive, *t.Hive, c.TarReader()); err != nil {
			return err
		}
	}
	for range sent(t.Mounts) {
		var tr wire.Tree
		if err := c.Expect(wire.JSON, &tr); err != nil {
			return err
		}
		if tr.Mount < 0 || tr.Mount >= len(t.Mounts) || hosts[tr.Mount] == empty {
			return fmt.Errorf("a tree for mount %d, which takes none", tr.Mount)
		}
		if err := applyShared(hosts[tr.Mount], tr, c.TarReader(), t.Mounts[tr.Mount].Kind == wire.KindShared); err != nil {
			return err
		}
	}
	box := ""
	for i, m := range t.Mounts {
		if m.Inside == "/hive/box" {
			box = hosts[i]
			os.MkdirAll(filepath.Join(box, "outbox"), 0o755)
			os.MkdirAll(filepath.Join(box, "inbox"), 0o755)
		}
		// A hidden path needs a folder to be mounted on, in the mount it covers.
		if m.Hide {
			for j, p := range t.Mounts {
				if rel, ok := under(m.Inside, p.Inside); ok && !p.Hide && j != i && hosts[j] != empty {
					os.MkdirAll(filepath.Join(hosts[j], rel), 0o755)
				}
			}
		}
	}
	base := map[int]wire.Manifest{}
	for i, m := range t.Mounts {
		if back(m) {
			if base[i], err = wire.Scan(hosts[i], skipFor(m)); err != nil {
				return err
			}
		}
	}

	s := t.Spec
	s.Mounts = make([]spec.Mount, len(t.Mounts))
	for i, m := range t.Mounts {
		s.Mounts[i] = spec.Mount{Host: hosts[i], Inside: m.Inside, RW: m.RW && !m.Hide && m.Kind == wire.KindRun, Hide: m.Hide}
	}
	s.Tools = tools
	s.Stdin = filepath.Join(dir, "stdin")
	s.Stdout = filepath.Join(dir, "out")
	s.StopFile = filepath.Join(dir, "stop")
	s.Local = nil // the main machine's local services aren't here
	s.Guard = []string{cfg.Dir}
	s.UID, s.GID = os.Getuid(), os.Getgid()
	if err := os.WriteFile(s.Stdin, t.Stdin, 0o600); err != nil {
		return err
	}
	for _, f := range []string{s.Stdout, s.Stdout + ".err"} {
		os.WriteFile(f, nil, 0o600)
	}
	specPath := filepath.Join(dir, "spec.json")
	if err := spec.Write(specPath, s); err != nil {
		return err
	}
	cmd := exec.Command(cfg.Hive, "sandbox-exec", "--spec", specPath)
	log, _ := os.Create(filepath.Join(dir, "exec.log"))
	cmd.Stdout, cmd.Stderr = log, log
	setGroup(cmd)
	if err := cmd.Start(); err != nil {
		log.Close()
		return c.SendJSON(wire.JSON, wire.Started{Error: err.Error()})
	}
	if err := c.SendJSON(wire.JSON, wire.Started{OK: true}); err != nil {
		killGroup(cmd)
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); log.Close() }()

	// While it runs: output and mailbox requests out, responses and stop in.
	// A link that drops stops the turn: nobody is left to take its work.
	linkErr := make(chan error, 1)
	go func() {
		for {
			typ, p, err := c.Recv()
			if err != nil {
				linkErr <- err
				return
			}
			switch typ {
			case wire.Stop:
				os.WriteFile(s.StopFile, nil, 0o600)
			case wire.Inbox:
				var m wire.Mail
				if json.Unmarshal(p, &m) == nil && box != "" {
					wire.PutMail(filepath.Join(box, "inbox"), m)
				}
			}
		}
	}()
	tails := []*tail{{path: s.Stdout, typ: wire.Out}, {path: s.Stdout + ".err", typ: wire.Err}}
	pump := func() error {
		for _, tl := range tails {
			if err := tl.send(c); err != nil {
				return err
			}
		}
		if box != "" {
			return wire.TakeMail(filepath.Join(box, "outbox"), func(m wire.Mail) error { return c.SendJSON(wire.Outbox, m) })
		}
		return nil
	}
	var waitErr error
loop:
	for tick := time.NewTicker(200 * time.Millisecond); ; {
		select {
		case waitErr = <-exited:
			break loop
		case err := <-linkErr:
			os.WriteFile(s.StopFile, nil, 0o600)
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				killGroup(cmd)
				<-exited
			}
			return fmt.Errorf("link dropped mid-turn, turn stopped: %w", err)
		case <-tick.C:
			if err := pump(); err != nil {
				killGroup(cmd)
				return err
			}
		}
	}
	if err := pump(); err != nil {
		return err
	}
	ec := wire.ExitCode{}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		ec.Code = ee.ExitCode()
	} else if waitErr != nil {
		ec.Code, ec.Error = 125, waitErr.Error()
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "exec.log")); err == nil && ec.Code != 0 {
		ec.Error = strings.TrimSpace(ec.Error + " " + clip(string(raw), 2000))
	}
	ec.Egress, _ = os.ReadFile(s.Stdout + ".egress")
	if err := c.SendJSON(wire.Exit, ec); err != nil {
		return err
	}
	for i, m := range t.Mounts {
		if !back(m) {
			continue
		}
		cur, err := wire.Scan(hosts[i], skipFor(m))
		if err != nil {
			return err
		}
		if err := sendTree(c, i, hosts[i], base[i], cur); err != nil {
			return err
		}
	}
	return c.Send(wire.Done, nil)
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hiveFor is where the turn's hive binary (the core's own, Turn.Hive) is
// kept here, and whether the Hive must send it first; why, when not "", is
// why the turn can't run here. A turn that names hive among its tools runs
// the core's hive as `hive` inside the sandbox, never this machine's
// installed one: that one is whatever release the person put here, and an
// older hive CLI parses the core's newer ops wrong (10-29: `hive agent
// heavy` read as a generic --key op on iiks1). The binary sent runs only
// inside the sandbox; sandbox-exec is still cfg.Hive.
func hiveFor(cfg Config, t wire.Turn) (path string, need bool, why string) {
	if !slices.Contains(t.Tools, "hive") {
		return "", false, ""
	}
	if t.Hive == nil || !sha256Hex.MatchString(t.Hive.SHA256) || t.Hive.Size <= 0 || t.Hive.Size > wire.MaxHive {
		return "", false, "the turn's hive binary wasn't named (a Hive from before protocol 2)"
	}
	if own, err := wire.ELFMachine(cfg.Hive); err != nil || own != t.Hive.Machine {
		return "", false, fmt.Sprintf("the Hive's hive binary is built for %s, this machine's hive for %s", t.Hive.Machine, cmp.Or(own, fmt.Sprint(err)))
	}
	dir := filepath.Join(cfg.Dir, "hive")
	os.MkdirAll(dir, 0o700)
	path = filepath.Join(dir, t.Hive.SHA256)
	if fi, err := os.Stat(path); err == nil && fi.Size() == t.Hive.Size {
		now := time.Now()
		os.Chtimes(path, now, now) // in use: pruneHive keeps it
		return path, false, ""
	}
	return path, true, ""
}

// takeHive reads the hive binary the Hive sends into path, checking it is
// the one Turn.Hive names, and drops the copies no turn has used in a day.
func takeHive(path string, want wire.HiveBin, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hive-*")
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
	if got := hex.EncodeToString(h.Sum(nil)); n != want.Size || got != want.SHA256 {
		return fmt.Errorf("the hive binary sent (%d bytes, sha256 %s) isn't the one named (%d bytes, %s)", n, got, want.Size, want.SHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	pruneHive(filepath.Dir(path), path)
	return nil
}

// pruneHive removes the hive binaries in dir besides keep that no turn has
// used in a day. One that a running turn has mounted stays open to it.
func pruneHive(dir, keep string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if fi, err := e.Info(); err == nil && p != keep && time.Since(fi.ModTime()) > 24*time.Hour {
			os.Remove(p)
		}
	}
}

// findTools gives each tool the turn names its path here: the hive binary
// the Hive sent (hive), the path runner.json names, or where sandbox-exec would find it (PATH,
// ~/.local/bin, /usr/local/bin). missing are the names found nowhere: a turn
// whose harness isn't here is refused before it takes a place, so it runs on
// the Hive's machine instead of ending 'tool "claude" not found' (exit 125).
// A name that is an absolute path is a tool tree the sandbox reads in place
// (/opt/eda-toolchain): it must be a folder at that path here, and it isn't
// one of the sandbox's tools.
func findTools(cfg Config, names []string, hive string) (tools, missing []string) {
	home, _ := os.UserHomeDir()
	for _, name := range names {
		if filepath.IsAbs(name) {
			if fi, err := os.Stat(name); err != nil || !fi.IsDir() {
				missing = append(missing, name)
			}
			continue
		}
		switch p, ok := cfg.Tools[name]; {
		case name == "hive" && hive != "":
			tools = append(tools, hive)
			continue
		case ok:
			tools = append(tools, p)
			continue
		}
		found := ""
		if p, err := exec.LookPath(name); err == nil {
			found, _ = filepath.Abs(p)
		}
		for _, d := range []string{filepath.Join(home, ".local", "bin"), "/usr/local/bin"} {
			if p, err := exec.LookPath(filepath.Join(d, name)); found == "" && filepath.IsAbs(d) && err == nil {
				found = p
			}
		}
		if found == "" {
			missing = append(missing, name)
			continue
		}
		tools = append(tools, found)
	}
	return tools, missing
}

// back says the mount's files go back to the Hive when the turn ends.
func back(m wire.MountPlan) bool { return m.RW && m.Kind == wire.KindRun && !m.Throwaway }

// sent lists the mounts the Hive sends a tree for, in order.
func sent(ms []wire.MountPlan) []int {
	var out []int
	for i, m := range ms {
		if !m.Hide && m.Kind != wire.KindEmpty {
			out = append(out, i)
		}
	}
	return out
}

// applyShared applies a tree; a shared cache is locked while it changes, as
// two sessions may bring it up to date at once.
func applyShared(dir string, tr wire.Tree, r io.Reader, shared bool) error {
	if shared {
		l, err := waitLock(dir + ".lock")
		if err != nil {
			return err
		}
		defer l.Close()
	}
	if err := wire.Apply(dir, tr.Gone, r); err != nil {
		wire.Drain(r)
		return err
	}
	return wire.Drain(r)
}

func sendTree(c *wire.Conn, i int, root string, base, cur wire.Manifest) error {
	if err := c.SendJSON(wire.JSON, wire.Tree{Mount: i, Gone: wire.Gone(base, cur)}); err != nil {
		return err
	}
	w := c.TarWriter()
	if err := wire.Pack(w, root, base, cur); err != nil {
		return err
	}
	return w.Close()
}

func skipFor(m wire.MountPlan) wire.Skip {
	if m.Inside == "/hive/box" {
		return wire.MailSkip
	}
	return nil
}

// under says whether inside path p is below root, and where.
func under(p, root string) (string, bool) {
	rel, ok := strings.CutPrefix(p, strings.TrimSuffix(root, "/")+"/")
	return rel, ok && rel != ""
}

type tail struct {
	path string
	typ  byte
	off  int64
}

func (t *tail) send(c *wire.Conn) error {
	f, err := os.Open(t.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return nil
	}
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if err := c.Send(t.typ, buf[:n]); err != nil {
				return err
			}
			t.off += int64(n)
		}
		if err != nil {
			return nil
		}
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
