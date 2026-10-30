package hive

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/spec"
	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// The bench is the project's score: dispatch's own scenarios (a turn sent to
// a runner and brought back, the cache, a stop, each way a turn falls back or
// is given back, a check never run, the core's hive sent once, room on one
// reading) played against a runner that is this commit's dispatch binary on
// this machine, reached through a local command instead of ssh, with a fake
// sandbox. It reads the commit, not whether iiks1 was up today.

// benchHostKey stands in for a runner's host key: a bench link runs no ssh.
const benchHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIbench"

// localRunner is an ssh:// address that links reach through cmd on this
// machine instead (testLinks), as a runner in runners.json can't.
func localRunner(cmd string) string {
	linksMu.Lock()
	defer linksMu.Unlock()
	addr := fmt.Sprintf("ssh://test@link-%d.invalid", len(testLinks))
	testLinks[addr] = strings.Fields(cmd)
	return addr
}

// scene is one run's folders laid out as the core does, a spec over them as
// the core's sandboxSpec builds it, and a runner ("fake") that is the
// dispatch binary on this machine.
type scene struct {
	home                                                      Home
	dispatch, fakehive                                        string
	remote                                                    string // the fake runner's folder
	work, ro, box, rhome, kb, mods, state, tools, logins, out string
	root                                                      string
	spec                                                      spec.Spec
}

func newScene(root, dispatch, fakehive string) (*scene, error) {
	s := &scene{root: root, home: Home(filepath.Join(root, "module")), dispatch: dispatch, fakehive: fakehive, remote: filepath.Join(root, "remote")}
	p := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	s.work, s.ro, s.box, s.rhome = p("checkout"), p("runs", "r1", "ro"), p("runs", "r1", "box"), p("private", "r1", "home")
	s.kb, s.mods, s.state, s.tools, s.logins = p("kb"), p("modules"), p("state"), p("tools"), p("state", "logins")
	host := p("runs", "r1", "host")
	for _, d := range []string{s.work, s.ro, filepath.Join(s.box, "outbox"), filepath.Join(s.box, "inbox"), s.rhome, s.kb, s.mods, s.logins, s.tools, host, filepath.Join(s.work, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	for path, text := range map[string]string{
		filepath.Join(s.work, "file.txt"):      "line one\n",
		filepath.Join(s.work, "gone.txt"):      "delete me\n",
		filepath.Join(s.work, ".git", "HEAD"):  "ref: refs/heads/main\n",
		filepath.Join(s.ro, "brief.md"):        "the brief\n",
		filepath.Join(s.kb, "note.md"):         "kb note\n",
		filepath.Join(s.state, "events.jsonl"): "SECRET-STATE\n",
		filepath.Join(s.logins, "token"):       "SECRET-LOGIN\n",
		filepath.Join(host, "turn-001.prompt"): "hello",
	} {
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			return nil, err
		}
	}
	s.out = filepath.Join(host, "turn-001.jsonl")
	s.spec = spec.Spec{
		ID: "w-test", Argv: []string{"claude", "-p"}, Env: []string{"HIVE_ID=w-test", "CLAUDE_CODE_OAUTH_TOKEN=short-lived"}, Cwd: "/work",
		Mounts: []spec.Mount{
			{Host: s.work, Inside: "/work", RW: true}, {Host: s.ro, Inside: "/hive/run"}, {Host: s.box, Inside: "/hive/box", RW: true},
			{Host: s.rhome, Inside: "/hive/home", RW: true}, {Host: s.kb, Inside: "/hive/kb"}, {Host: s.mods, Inside: "/hive/modules"},
			{Host: p("projects"), Inside: "/hive/projects"}, {Host: s.state, Inside: "/hive/state"},
			{Host: s.logins, Inside: "/hive/state/logins", Hide: true}, {Host: s.tools, Inside: "/hive/tools"},
		},
		Tools: []string{fakehive, "claude"}, Stdin: filepath.Join(host, "turn-001.prompt"), Stdout: s.out,
		StopFile: filepath.Join(host, "stop"), Network: true,
	}
	if err := s.setRunner("--name fake --max 2 --hive " + fakehive); err != nil {
		return nil, err
	}
	return s, nil
}

// setRunner makes "fake" a runner serving with these flags.
func (s *scene) setRunner(flags string) error {
	addr := localRunner(s.dispatch + " runner serve --config /nonexistent --max-load 1000 --min-free-mb 1 --dir " + s.remote + " " + flags)
	return s.home.Add(Runner{Name: "fake", Address: addr, MinFreeMB: 1, MaxLoad: 1000}, benchHostKey)
}

// unreachable makes "fake" a runner whose link can't start.
func (s *scene) unreachable() error {
	return s.home.Add(Runner{Name: "fake", Address: localRunner("/nonexistent/runner")}, benchHostKey)
}

// core answers mailbox requests as the core's Drain does, while the turn runs.
func (s *scene) core(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(50 * time.Millisecond):
		}
		wire.TakeMail(filepath.Join(s.box, "outbox"), func(m wire.Mail) error {
			wire.PutMail(filepath.Join(s.box, "inbox"), wire.Mail{Name: m.Name, Data: []byte(`{"id":"r1","ok":true,"result":"noted"}`)})
			return nil
		})
	}
}

// delegate hands the scene's spec over as the core does and says how the
// turn exited; an error if the spec, with its token, stayed on disk.
func (s *scene) delegate() (int, error) {
	dir, err := os.MkdirTemp("", "dispatch-spec-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "spec.json")
	if err := spec.Write(f, s.spec); err != nil {
		return 0, err
	}
	// Each turn starts from the same checkout and an empty output.
	os.WriteFile(filepath.Join(s.work, "file.txt"), []byte("line one\n"), 0o644)
	os.WriteFile(filepath.Join(s.work, "gone.txt"), []byte("delete me\n"), 0o644)
	os.WriteFile(s.out, nil, 0o644)
	stop := make(chan struct{})
	defer close(stop)
	go s.core(stop)
	code := s.home.Delegate(f, "")
	if _, err := os.Stat(f); err == nil {
		return code, errors.New("the spec file, with its token, was left on disk")
	}
	return code, nil
}

func (s *scene) read(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

// ran says what of the scripted turn (testdata/fakehive) didn't come back.
func (s *scene) ran() []string {
	var bad []string
	out := s.read(s.out)
	for _, want := range []string{"got: hello", "HIVE_ID=w-test", "done"} {
		if !strings.Contains(out, want) {
			bad = append(bad, fmt.Sprintf("turn output lacks %q:\n%s", want, out))
		}
	}
	check := func(what, got string, ok bool) {
		if !ok {
			bad = append(bad, fmt.Sprintf("%s = %q", what, got))
		}
	}
	got := s.read(filepath.Join(s.work, "file.txt"))
	check("file.txt", got, got == "line one\nremote edit\n")
	if _, err := os.Stat(filepath.Join(s.work, "gone.txt")); err == nil {
		bad = append(bad, "gone.txt was deleted there but not here")
	}
	got = s.read(filepath.Join(s.work, "new", "dir", "x.txt"))
	check("new file", got, got == "x")
	got = s.read(filepath.Join(s.work, "kb-seen.txt"))
	check("the kb the turn saw", got, got == "kb note\n")
	got = s.read(filepath.Join(s.work, "reply.txt"))
	check("the mailbox reply", got, strings.Contains(got, "noted"))
	got = s.read(filepath.Join(s.work, ".git", "HEAD"))
	check(".git/HEAD", got, strings.Contains(got, "main"))
	got = s.read(filepath.Join(s.box, "notes.md"))
	check("notes.md", got, got == "notes from the turn\n")
	got = s.read(filepath.Join(s.rhome, "session.jsonl"))
	check("home session", got, got == "session\n")
	got = s.read(s.out + ".err")
	check("stderr", got, strings.Contains(got, "a line on stderr"))
	got = s.read(s.out + ".egress")
	check("egress refusals", got, strings.Contains(got, "example.com"))
	return bad
}

// latest is the turn last recorded.
func (s *scene) latest() Placed {
	if ps := s.home.Turns(); len(ps) > 0 {
		return ps[0]
	}
	return Placed{}
}

// A Scenario is one thing dispatch must do, played on a fresh scene. One
// that GivesBack is played with the core taking turns back (GiveBackEnv).
type Scenario struct {
	Name      string
	GivesBack bool
	Play      func(s *scene) error
}

// ranHere is a turn that couldn't go and ran here, whole.
func ranHere(s *scene) error {
	if code, err := s.delegate(); code != 3 || err != nil {
		return fmt.Errorf("exit %d (%v): %s", code, err, s.read(s.out+".err"))
	}
	if bad := s.ran(); len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	if p := s.latest(); p.Where != "local" || p.Why == "" || !p.Ended {
		return fmt.Errorf("recorded %+v, want it run here with why", p)
	}
	if ents, _ := os.ReadDir(filepath.Join(string(s.home), "tmp")); len(ents) != 0 {
		return fmt.Errorf("a spec stayed in tmp/: %v", ents)
	}
	return nil
}

// givenBack is a turn that couldn't go, given back unstarted.
func givenBack(s *scene) error {
	if code, err := s.delegate(); code != 125 || err != nil {
		return fmt.Errorf("exit %d (%v), want 125", code, err)
	}
	if strings.Contains(s.read(s.out), "got:") {
		return errors.New("the turn ran")
	}
	if !strings.Contains(s.read(s.out+".err"), "dispatch: not run: ") {
		return fmt.Errorf(".err = %q", s.read(s.out+".err"))
	}
	if p := s.latest(); p.Where != "nowhere" || p.Exit != 125 || !p.Ended {
		return fmt.Errorf("recorded %+v", p)
	}
	return nil
}

// ranThere is a turn run on the runner and brought back whole.
func ranThere(s *scene) error {
	if code, err := s.delegate(); code != 3 || err != nil {
		return fmt.Errorf("exit %d (%v), want the turn's own 3: %s", code, err, s.read(s.out+".err"))
	}
	if bad := s.ran(); len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	if p := s.latest(); p.Where != "fake" || !p.Ended || p.Exit != 3 || p.Sent == 0 || p.Back == 0 {
		return fmt.Errorf("recorded %+v", p)
	}
	return nil
}

// Scenarios are the bench's, in the order they run.
var Scenarios = []Scenario{
	{"a turn runs on the runner and comes back whole, nothing secret left there", false, func(s *scene) error {
		if err := ranThere(s); err != nil {
			return err
		}
		if got := s.read(filepath.Join(s.work, "state-entries.txt")); got != "0" {
			return fmt.Errorf("the Hive's records went across: %s entries", got)
		}
		if ents, _ := os.ReadDir(filepath.Join(s.remote, "turns")); len(ents) != 0 {
			return fmt.Errorf("the turn's folder stayed on the runner: %v", ents)
		}
		var leak error
		filepath.WalkDir(s.remote, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				raw, _ := os.ReadFile(p)
				for _, secret := range []string{"SECRET", "short-lived"} {
					if bytes.Contains(raw, []byte(secret)) {
						leak = fmt.Errorf("%s holds %q", p, secret)
					}
				}
			}
			return nil
		})
		return leak
	}},
	{"a second turn runs on the cache the first left, sending less", false, func(s *scene) error {
		if err := ranThere(s); err != nil {
			return err
		}
		first := s.latest().Sent
		if err := ranThere(s); err != nil {
			return fmt.Errorf("second turn: %w", err)
		}
		if again := s.latest().Sent; again >= first {
			return fmt.Errorf("the second turn sent %d bytes, the first %d", again, first)
		}
		return nil
	}},
	{"a stop reaches the runner", false, func(s *scene) error {
		os.WriteFile(s.spec.Stdin, []byte("please stop"), 0o644)
		done := make(chan int, 1)
		go func() { code, _ := s.delegate(); done <- code }()
		deadline := time.Now().Add(2 * time.Minute)
		for !strings.Contains(s.read(s.out), "waiting for stop") {
			if time.Now().After(deadline) {
				return errors.New("the turn never started")
			}
			time.Sleep(50 * time.Millisecond)
		}
		os.WriteFile(s.spec.StopFile, nil, 0o644)
		select {
		case code := <-done:
			if code != 143 {
				return fmt.Errorf("exit %d, want 143", code)
			}
			return nil
		case <-time.After(time.Minute):
			return errors.New("the stop file never reached the runner")
		}
	}},
	{"an unreachable runner: the turn runs here", false, func(s *scene) error {
		if err := s.unreachable(); err != nil {
			return err
		}
		return ranHere(s)
	}},
	{"a runner with no sandbox: the turn runs here", false, func(s *scene) error {
		if err := s.setRunner("--max 1 --hive /nonexistent/hive"); err != nil {
			return err
		}
		return ranHere(s)
	}},
	{"a turn granted a local service runs here", false, func(s *scene) error {
		s.spec.Local = []string{"127.0.0.1:11434"}
		return ranHere(s)
	}},
	{"no runner, the core takes it back: given back unstarted", true, func(s *scene) error {
		if err := s.home.Remove("fake"); err != nil {
			return err
		}
		return givenBack(s)
	}},
	{"an unreachable runner, the core takes it back: given back unstarted", true, func(s *scene) error {
		if err := s.unreachable(); err != nil {
			return err
		}
		return givenBack(s)
	}},
	{"a check is never run, even with a runner that has room", false, func(s *scene) error {
		s.spec.ID = "gate-test"
		s.spec.Mounts = []spec.Mount{{Host: s.work, Inside: "/work", RW: true}, {Host: s.rhome, Inside: "/hive/home", RW: true}, {Host: s.tools, Inside: "/hive/tools"}}
		if code, err := s.delegate(); code != 125 || err != nil {
			return fmt.Errorf("exit %d (%v), want 125", code, err)
		}
		if strings.Contains(s.read(s.out), "got:") {
			return errors.New("the check ran")
		}
		if p := s.latest(); p.Where != "nowhere" || p.Exit != 125 {
			return fmt.Errorf("recorded %+v", p)
		}
		return nil
	}},
	{"a turn whose harness the runner hasn't runs here, and the next isn't sent", false, func(s *scene) error {
		s.spec.Tools = []string{s.fakehive, "nowhere-harness"}
		if err := ranHere(s); err != nil {
			return err
		}
		if !strings.Contains(s.latest().Why, "nowhere-harness") {
			return fmt.Errorf("why = %q", s.latest().Why)
		}
		if err := ranHere(s); err != nil {
			return fmt.Errorf("second: %w", err)
		}
		if why := s.latest().Why; !strings.Contains(why, "fake: hasn't nowhere-harness") {
			return fmt.Errorf("second went to the runner again: why %q", why)
		}
		s.spec.Tools = []string{s.fakehive, "claude"}
		if err := ranThere(s); err != nil {
			return fmt.Errorf("a turn that doesn't need it: %w", err)
		}
		return nil
	}},
	{"a turn there runs the core's hive, sent once and kept", false, func(s *scene) error {
		raw, err := os.ReadFile(s.fakehive)
		if err != nil {
			return err
		}
		core := filepath.Join(s.root, "core", "hive") // a hive other than the runner's
		os.MkdirAll(filepath.Dir(core), 0o755)
		os.WriteFile(core, append(raw, "newer release"...), 0o755)
		s.spec.Tools = []string{core, "claude"}
		hb, err := wire.DescribeHive(core)
		if err != nil {
			return err
		}
		kept := filepath.Join(s.remote, "hive", hb.SHA256)
		for i := 1; i <= 2; i++ {
			if code, err := s.delegate(); code != 3 || err != nil {
				return fmt.Errorf("turn %d: exit %d (%v): %s", i, code, err, s.read(s.out+".err"))
			}
			if !strings.Contains(s.read(s.out), "tools: "+kept+" ") {
				return fmt.Errorf("turn %d's hive isn't the core's, kept at %s", i, kept)
			}
			if sent := s.latest().Sent; i == 1 && sent < hb.Size || i == 2 && sent >= hb.Size {
				return fmt.Errorf("turn %d sent %d bytes; the hive binary is %d", i, sent, hb.Size)
			}
		}
		return nil
	}},
	{"a burst on one reading stops at the runner's room", false, func(s *scene) error {
		h := s.home
		h.Save([]Runner{{Name: "iiks1", Address: "ssh://u@x"}})
		h.See("iiks1", Seen{Status: wire.Status{Cores: 6, MemFreeMB: 25000, Load1: 0.5, Sandbox: "ok bwrap"}, At: time.Now()})
		n := 0
		for i := 0; i < 20; i++ {
			if r, _, _ := h.Pick(); r.Name == "iiks1" {
				n++
			}
		}
		if n != 5 {
			return fmt.Errorf("room said yes %d times on one reading, want 5", n)
		}
		return nil
	}},
}

// Played is how one scenario went.
type Played struct {
	Name string
	Err  error
	Took time.Duration
}

// Bench plays every scenario, each on a fresh scene under root, against a
// runner that is dispatch (this commit's binary) with fakehive as its
// sandbox: those that don't give back all at once, then those that do, as
// the core's environment says which. A scenario not done within limit
// fails.
func Bench(root, dispatch, fakehive string, limit time.Duration) []Played {
	old := SelfBinary
	SelfBinary = func() (string, error) { return dispatch, nil }
	defer func() { SelfBinary = old }()
	// The harness the turns name, on the PATH the runner (this machine) sees.
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o755)
	path := os.Getenv("PATH")
	os.Setenv("PATH", bin+string(os.PathListSeparator)+path)
	defer os.Setenv("PATH", path)
	rs := make([]Played, len(Scenarios))
	play := func(i int) {
		sc, start := Scenarios[i], time.Now()
		s, err := newScene(filepath.Join(root, fmt.Sprint(i)), dispatch, fakehive)
		if err == nil {
			done := make(chan error, 1)
			go func() { done <- sc.Play(s) }()
			select {
			case err = <-done:
			case <-time.After(limit):
				err = fmt.Errorf("not done in %s", limit)
			}
		}
		rs[i] = Played{Name: sc.Name, Err: err, Took: time.Since(start)}
	}
	for _, giveBack := range []bool{false, true} {
		if giveBack {
			os.Setenv(GiveBackEnv, "1")
			defer os.Unsetenv(GiveBackEnv)
		}
		var wg sync.WaitGroup
		for i, sc := range Scenarios {
			if sc.GivesBack == giveBack {
				wg.Add(1)
				go func(i int) { defer wg.Done(); play(i) }(i)
			}
		}
		wg.Wait()
	}
	return rs
}

// BenchScore is the share of scenarios that passed, out of 100.
func BenchScore(rs []Played) int {
	if len(Scenarios) == 0 {
		return 0
	}
	n := 0
	for _, r := range rs {
		if r.Err == nil {
			n++
		}
	}
	return n * 100 / len(Scenarios)
}
