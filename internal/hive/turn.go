package hive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/spec"
	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// Turn plans and the host folders behind them, side by side.
type plan struct {
	mounts []wire.MountPlan
	hosts  []string
	skips  []wire.Skip
}

// Dispatchable says why s can't run on another machine, or "".
func Dispatchable(s spec.Spec) string {
	work := false
	for _, m := range s.Mounts {
		if m.Inside == "/work" && m.Host != "" {
			work = true
		}
	}
	switch {
	case !work || s.Cwd != "/work":
		return "it isn't a sandboxed turn (no /work mount it starts in)"
	case len(s.Local) > 0:
		return "it was granted a service on this machine (" + strings.Join(s.Local, ", ") + ")"
	case s.Stdout == "":
		return "it has no output file"
	case readsRecords(s):
		return "it reads the Hive's records (/hive/state), which stay on this machine"
	}
	return ""
}

// readsRecords says s mounts the Hive's records and is a run whose work is
// reading them: any run but a worker's. The records are far too big and too
// live to send (10-29: the state folder's cache alone was 6.8 GB, the journal
// near a GB), so there they are an empty folder. A reviewer checks goals.md
// quotes against $HIVE_STATE/chats, so reviews run here too. A worker's brief
// and $HIVE_RUN carry what it needs, and the knowledge base (sent) holds the
// record's story, so workers go.
func readsRecords(s spec.Spec) bool {
	for _, m := range s.Mounts {
		if m.Inside == "/hive/state" && !m.Hide {
			return !strings.HasPrefix(s.ID, "w-")
		}
	}
	return false
}

// IsCheck says s is a check the core hands over (its ID is "gate-..."), not
// an agent's turn.
func IsCheck(s spec.Spec) bool { return strings.HasPrefix(s.ID, "gate-") }

// planFor decides how each mount goes across. The turn's own folders go
// whole; the knowledge base, modules and tools are cached there and sent as
// changes; so are the other projects, but only each one's git-tracked files
// (trackedOnly); the Hive's records stay here, an empty folder there.
// Anything a Hide mount covers is never sent.
func planFor(s spec.Spec) plan {
	var p plan
	kind := func(m spec.Mount) string {
		switch {
		case m.Inside == "/hive/state":
			return wire.KindEmpty
		case !m.RW && (m.Inside == "/hive/kb" || m.Inside == "/hive/projects" || m.Inside == "/hive/modules" || m.Inside == "/hive/tools" || strings.HasPrefix(m.Inside, "/hive/modules/")):
			return wire.KindShared
		}
		return wire.KindRun
	}
	parent := func(inside string) (spec.Mount, bool) {
		var best spec.Mount
		found := false
		for _, m := range s.Mounts {
			if !m.Hide && strings.HasPrefix(inside, strings.TrimSuffix(m.Inside, "/")+"/") && len(m.Inside) > len(best.Inside) {
				best, found = m, true
			}
		}
		return best, found
	}
	for _, m := range s.Mounts {
		if m.Hide {
			if par, ok := parent(m.Inside); !ok || kind(par) == wire.KindEmpty {
				continue // nothing is sent there to hide
			}
			p.mounts = append(p.mounts, wire.MountPlan{Inside: m.Inside, Hide: true, Kind: wire.KindEmpty})
			p.hosts, p.skips = append(p.hosts, ""), append(p.skips, nil)
			continue
		}
		mp := wire.MountPlan{Inside: m.Inside, RW: m.RW, Kind: kind(m)}
		if mp.Kind == wire.KindShared {
			sum := sha256.Sum256([]byte(m.Host))
			mp.Key = strings.Trim(strings.ReplaceAll(m.Inside, "/", "_"), "_") + "-" + hex.EncodeToString(sum[:6])
		}
		// Skip what a Hide mount covers inside this one, and a box's mail.
		var covered []string
		for _, o := range s.Mounts {
			if rel, ok := strings.CutPrefix(o.Inside, strings.TrimSuffix(m.Inside, "/")+"/"); ok && o.Hide {
				covered = append(covered, rel)
			}
		}
		box := m.Inside == "/hive/box"
		var tracked map[string]bool
		if m.Inside == "/hive/projects" && mp.Kind == wire.KindShared {
			tracked = trackedOnly(m.Host)
		}
		skip := func(rel string) bool {
			if tracked != nil && !tracked[rel] {
				return true
			}
			for _, c := range covered {
				if rel == c || strings.HasPrefix(rel, c+"/") {
					return true
				}
			}
			return box && wire.MailSkip(rel)
		}
		p.mounts = append(p.mounts, mp)
		p.hosts, p.skips = append(p.hosts, m.Host), append(p.skips, skip)
	}
	return p
}

// trackedOnly is what of the projects folder at root goes across: each
// project's git-tracked files and the folders holding them, not its .git or
// build outputs. 10-29: the projects held about 280k files in several GB
// (pll 1.1 GB, hifi-receiver-2 1 GB), their tracked files 6.6k entries in
// 199 MB. A folder git can't list (not a repo, a symlink, a repo git calls
// of dubious ownership) sends nothing, and says so on stderr.
func trackedOnly(root string) map[string]bool {
	keep := map[string]bool{}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			fmt.Fprintf(os.Stderr, "dispatch: /hive/projects/%s isn't a folder; nothing of it is sent\n", e.Name())
			continue
		}
		out, err := exec.Command("git", "-C", filepath.Join(root, e.Name()), "ls-files", "-z").Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dispatch: git can't list /hive/projects/%s (%v); nothing of it is sent\n", e.Name(), err)
			continue
		}
		keep[e.Name()] = true
		for _, f := range strings.Split(string(out), "\x00") {
			for rel := path.Join(e.Name(), f); f != "" && !keep[rel]; rel = path.Dir(rel) {
				keep[rel] = true
			}
		}
	}
	return keep
}

// Result is how a dispatched turn ended.
type Result struct {
	Code int
	// Started is false when the runner never started the turn: nothing ran
	// there, so the turn can run here instead.
	Started bool
	Err     error
	Placed  Placed
	// Lacks are the turn's tools the runner hasn't: it refused the turn for
	// them, or (a runner from before it checked) its sandbox did.
	Lacks []string
}

// ToolNames are the names a turn's tools go across by: each path's base
// name, and the hive binary (whatever it is called here) as "hive".
func ToolNames(tools []string) []string {
	var out []string
	for _, tool := range tools {
		name := filepath.Base(tool)
		if strings.HasPrefix(name, "hive") {
			name = "hive"
		}
		out = append(out, name)
	}
	return out
}

// notFound is how sandbox-exec refuses a tool it can't find.
var notFound = regexp.MustCompile(`tool "([^"]+)" not found on this machine`)

// Send runs the turn in s on r. The prompt, the turn's folders and its
// environment (with the turn's short-lived model token, as a local sandbox
// gets it) go across; the output and the mailbox come back as it runs, and
// the turn's writable folders when it ends.
func (h Home) Send(r Runner, s spec.Spec) Result {
	start := time.Now()
	res := Result{Placed: Placed{At: start, Run: s.ID, Turn: filepath.Base(s.Stdout), Where: r.Name}}
	fail := func(err error) Result {
		res.Err, res.Code = err, 125
		res.Placed.Error, res.Placed.Exit, res.Placed.Ended = err.Error(), 125, true
		res.Placed.Secs = time.Since(start).Seconds()
		return res
	}
	p := planFor(s)
	stdin, err := os.ReadFile(s.Stdin)
	if err != nil && s.Stdin != "" {
		return fail(err)
	}
	t := wire.Turn{ID: s.ID, Label: strings.TrimSuffix(filepath.Base(s.Stdout), filepath.Ext(s.Stdout)), Mounts: p.mounts, Stdin: stdin}
	t.Spec = s
	t.Spec.Mounts, t.Spec.Tools, t.Spec.Stdin, t.Spec.Stdout, t.Spec.StopFile, t.Spec.Guard, t.Spec.Local = nil, nil, "", "", "", nil, nil
	t.Tools = ToolNames(s.Tools)

	l, err := h.Dial(r)
	if err != nil {
		return fail(err)
	}
	defer l.Close()
	if err := l.SendJSON(wire.JSON, wire.Hello{Version: wire.Version, Op: "turn"}); err != nil {
		return fail(l.Why(err))
	}
	if err := l.SendJSON(wire.JSON, t); err != nil {
		return fail(l.Why(err))
	}
	var man wire.Manifests
	if err := l.Expect(wire.JSON, &man); err != nil {
		return fail(l.Why(err))
	}
	if man.Error != "" {
		if names, ok := strings.CutPrefix(man.Error, wire.NoTool); ok {
			names, _, _ = strings.Cut(names, " (")
			res.Lacks = strings.Split(names, ", ")
		}
		return fail(fmt.Errorf("%s won't take it: %s", r.Name, man.Error))
	}
	for i, m := range p.mounts {
		if m.Hide || m.Kind == wire.KindEmpty {
			continue
		}
		cur, err := wire.Scan(p.hosts[i], p.skips[i])
		if err != nil {
			return fail(err)
		}
		base := man.Mounts[i]
		if err := l.SendJSON(wire.JSON, wire.Tree{Mount: i, Gone: wire.Gone(base, cur)}); err != nil {
			return fail(l.Why(err))
		}
		cw := &counter{w: l.TarWriter()}
		if err := wire.Pack(cw, p.hosts[i], base, cur); err != nil {
			return fail(err)
		}
		if err := cw.w.Close(); err != nil {
			return fail(l.Why(err))
		}
		res.Placed.Sent += cw.n
	}
	var st wire.Started
	if err := l.Expect(wire.JSON, &st); err != nil {
		return fail(l.Why(err))
	}
	if !st.OK {
		return fail(fmt.Errorf("%s couldn't start it: %s", r.Name, st.Error))
	}
	res.Started = true
	h.Handed(r.Name, s.ID+"/"+filepath.Base(s.Stdout), true)
	h.Record(res.Placed) // started there: the record says so before it ends

	box := ""
	for i, m := range p.mounts {
		if m.Inside == "/hive/box" {
			box = p.hosts[i]
		}
	}
	stop := make(chan struct{})
	defer close(stop)
	go relay(l, s.StopFile, box, stop)

	out, err := appendFile(s.Stdout)
	if err != nil {
		return fail(err)
	}
	defer out.Close()
	errf, err := appendFile(s.Stdout + ".err")
	if err != nil {
		return fail(err)
	}
	defer errf.Close()
	var ec *wire.ExitCode
	var errHead []byte // the start of the turn's stderr there, to read a sandbox's refusal in
	for {
		typ, payload, err := l.Recv()
		if err != nil {
			fmt.Fprintf(errf, "dispatch: the link to %s dropped mid-turn: %v\n", r.Name, l.Why(err))
			return fail(l.Why(fmt.Errorf("link dropped mid-turn: %w", err)))
		}
		switch typ {
		case wire.Out:
			out.Write(payload)
		case wire.Err:
			errf.Write(payload)
			if len(errHead) < 4096 {
				errHead = append(errHead, payload[:min(len(payload), 4096-len(errHead))]...)
			}
		case wire.Outbox:
			var m wire.Mail
			if json.Unmarshal(payload, &m) == nil && box != "" {
				wire.PutMail(filepath.Join(box, "outbox"), m)
			}
		case wire.Exit:
			ec = &wire.ExitCode{}
			if err := json.Unmarshal(payload, ec); err != nil {
				return fail(err)
			}
			if len(ec.Egress) > 0 {
				if f, err := appendFile(s.Stdout + ".egress"); err == nil {
					f.Write(ec.Egress)
					f.Close()
				}
			}
			if ec.Error != "" {
				fmt.Fprintf(errf, "dispatch: on %s: %s\n", r.Name, ec.Error)
			}
		case wire.JSON:
			var tr wire.Tree
			if err := json.Unmarshal(payload, &tr); err != nil {
				return fail(err)
			}
			if ec == nil || tr.Mount < 0 || tr.Mount >= len(p.mounts) || !p.mounts[tr.Mount].RW || p.mounts[tr.Mount].Kind != wire.KindRun {
				return fail(fmt.Errorf("%s sent files for mount %d, which can't take them", r.Name, tr.Mount))
			}
			cr := &countReader{r: l.TarReader()}
			if err := wire.Apply(p.hosts[tr.Mount], tr.Gone, cr); err != nil {
				return fail(fmt.Errorf("bringing back %s: %w", p.mounts[tr.Mount].Inside, err))
			}
			if err := wire.Drain(cr); err != nil {
				return fail(err)
			}
			res.Placed.Back += cr.n
		case wire.Done:
			if ec == nil {
				return fail(errors.New("the runner ended without an exit code"))
			}
			res.Code = ec.Code
			res.Placed.Exit, res.Placed.Ended, res.Placed.Secs = ec.Code, true, time.Since(start).Seconds()
			// A runner from before it checked tools started a turn whose
			// harness isn't there: the sandbox refused it before anything ran,
			// so it is as if the runner had refused it, and it can run here.
			if m := notFound.FindStringSubmatch(string(errHead) + "\n" + ec.Error); ec.Code == 125 && m != nil {
				res.Started, res.Lacks = false, []string{m[1]}
				res.Err = fmt.Errorf("%s hasn't the turn's tool %q", r.Name, m[1])
				res.Placed.Error = res.Err.Error()
				fmt.Fprintf(errf, "dispatch: %v; nothing ran there, so it runs here\n", res.Err)
			}
			return res
		default:
			return fail(fmt.Errorf("unexpected frame %q", typ))
		}
	}
}

// relay carries the stop file and the mailbox's responses to the runner
// while the turn runs.
func relay(l *Link, stopFile, box string, done <-chan struct{}) {
	stopped := false
	for t := time.NewTicker(200 * time.Millisecond); ; {
		select {
		case <-done:
			t.Stop()
			return
		case <-t.C:
		}
		if _, err := os.Stat(stopFile); err == nil && stopFile != "" && !stopped {
			stopped = l.Send(wire.Stop, nil) == nil
		}
		if box != "" {
			wire.TakeMail(filepath.Join(box, "inbox"), func(m wire.Mail) error { return l.SendJSON(wire.Inbox, m) })
		}
	}
}

func appendFile(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

type counter struct {
	w io.WriteCloser
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
