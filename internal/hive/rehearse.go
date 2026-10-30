package hive

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/spec"
	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// A rehearsal is the project's score: it plays the turns dispatch exists to
// carry against a runner on this machine (the dispatch binary itself, reached
// through a local command, as the tests' links are) and a stand-in for
// `hive sandbox-exec` (testdata/fakehive), in folders of its own. What it
// reads is the commit: not whether iiks1 is up today or how many turns the
// Hive had to give it (that is `dispatch ran`, a board line).

// Scene is one rehearsed case: its name and why it failed ("" passed).
type Scene struct{ Name, Fail string }

// Rehearse plays every scene in a fresh folder under dir, with dispatch as
// the runner's binary and fakeHive as the hive binary on both sides.
func Rehearse(dir, dispatch, fakeHive string) []Scene {
	var out []Scene
	for i, sc := range scenes {
		r := &rehearsal{dir: filepath.Join(dir, fmt.Sprint(i)), dispatch: dispatch, hive: fakeHive}
		fail := func() (fail string) {
			defer func() {
				if p := recover(); p != nil {
					fail = fmt.Sprint("panic: ", p)
				}
			}()
			if err := r.lay(); err != nil {
				return err.Error()
			}
			return sc.play(r)
		}()
		out = append(out, Scene{sc.name, fail})
	}
	return out
}

// Percent is the share of scenes that passed, 0 to 100.
func Percent(ss []Scene) int {
	if len(ss) == 0 {
		return 0
	}
	n := 0
	for _, s := range ss {
		if s.Fail == "" {
			n++
		}
	}
	return 100 * n / len(ss)
}

type rehearsal struct {
	dir, dispatch, hive    string
	home                   Home
	remote, work, box, out string
	kb, rhome              string
	spec                   spec.Spec
}

// link registers an ssh:// address that reaches cmd on this machine. The
// .invalid host resolves nowhere, and only this process's links map holds
// it: a runner in runners.json still opens ssh and nothing else.
func link(argv ...string) string {
	addr := fmt.Sprintf("ssh://rehearsal@link-%d.invalid", len(testLinks))
	testLinks[addr] = argv
	return addr
}

func (r *rehearsal) runner(hive string) string {
	return link(r.dispatch, "runner", "serve", "--config", "/nonexistent", "--name", "rehearsal", "--max", "2",
		"--max-load", "1000", "--min-free-mb", "1", "--dir", r.remote, "--hive", hive)
}

// lay sets out one run's folders as the core does and a worker turn's spec
// over them, and registers a runner with room.
func (r *rehearsal) lay() error {
	p := func(parts ...string) string { return filepath.Join(append([]string{r.dir}, parts...)...) }
	r.home, r.remote = Home(p("module")), p("remote")
	r.work, r.box, r.rhome, r.kb = p("checkout"), p("runs", "r1", "box"), p("private", "r1", "home"), p("kb")
	ro, state, logins, tools, host := p("runs", "r1", "ro"), p("state"), p("state", "logins"), p("tools"), p("runs", "r1", "host")
	for _, d := range []string{r.work, ro, filepath.Join(r.box, "outbox"), filepath.Join(r.box, "inbox"), r.rhome, r.kb, p("modules"), p("projects"), logins, tools, host, filepath.Join(r.work, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for f, s := range map[string]string{
		filepath.Join(r.work, "file.txt"): "line one\n", filepath.Join(r.work, "gone.txt"): "delete me\n",
		filepath.Join(r.work, ".git", "HEAD"): "ref: refs/heads/main\n", filepath.Join(ro, "brief.md"): "the brief\n",
		filepath.Join(r.kb, "note.md"): "kb note\n", filepath.Join(state, "events.jsonl"): "SECRET-STATE\n",
		filepath.Join(logins, "token"): "SECRET-LOGIN\n", filepath.Join(host, "turn-001.prompt"): "hello",
	} {
		if err := os.WriteFile(f, []byte(s), 0o644); err != nil {
			return err
		}
	}
	r.out = filepath.Join(host, "turn-001.jsonl")
	r.spec = spec.Spec{
		ID: "w-rehearsal", Argv: []string{"hive", "-p"}, Env: []string{"HIVE_ID=w-rehearsal", "CLAUDE_CODE_OAUTH_TOKEN=short-lived"}, Cwd: "/work",
		Mounts: []spec.Mount{
			{Host: r.work, Inside: "/work", RW: true}, {Host: ro, Inside: "/hive/run"}, {Host: r.box, Inside: "/hive/box", RW: true},
			{Host: r.rhome, Inside: "/hive/home", RW: true}, {Host: r.kb, Inside: "/hive/kb"}, {Host: p("modules"), Inside: "/hive/modules"},
			{Host: p("projects"), Inside: "/hive/projects"}, {Host: state, Inside: "/hive/state"},
			{Host: logins, Inside: "/hive/state/logins", Hide: true}, {Host: tools, Inside: "/hive/tools"},
		},
		Tools: []string{r.hive}, Stdin: filepath.Join(host, "turn-001.prompt"), Stdout: r.out,
		StopFile: filepath.Join(host, "stop"), Network: true,
	}
	return r.home.Add(Runner{Name: "rehearsal", Address: r.runner(r.hive), MinFreeMB: 1, MaxLoad: 1000}, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIrehearsal")
}

// delegate hands the turn over as the core does, answering its mailbox
// while it runs.
func (r *rehearsal) delegate() int {
	f := filepath.Join(r.dir, "spec.json")
	if err := spec.Write(f, r.spec); err != nil {
		return -1
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			wire.TakeMail(filepath.Join(r.box, "outbox"), func(m wire.Mail) error {
				wire.PutMail(filepath.Join(r.box, "inbox"), wire.Mail{Name: m.Name, Data: []byte(`{"id":"r1","ok":true,"result":"noted"}`)})
				return nil
			})
		}
	}()
	return r.home.Delegate(f, "")
}

func (r *rehearsal) read(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

// ran says what of the turn's work didn't come back, or "".
func (r *rehearsal) ran(code int) string {
	if code != 3 {
		return fmt.Sprintf("exit %d, want the turn's own 3: %s", code, clip(r.read(r.out+".err"), 300))
	}
	out := r.read(r.out)
	for _, want := range []string{"got: hello", "HIVE_ID=w-rehearsal", "done"} {
		if !strings.Contains(out, want) {
			return fmt.Sprintf("the turn's output lacks %q", want)
		}
	}
	for f, want := range map[string]string{
		filepath.Join(r.work, "file.txt"): "line one\nremote edit\n", filepath.Join(r.work, "new", "dir", "x.txt"): "x",
		filepath.Join(r.work, "kb-seen.txt"): "kb note\n", filepath.Join(r.box, "notes.md"): "notes from the turn\n",
		filepath.Join(r.rhome, "session.jsonl"): "session\n",
	} {
		if got := r.read(f); got != want {
			return fmt.Sprintf("%s = %q, want %q", filepath.Base(f), got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(r.work, "gone.txt")); err == nil {
		return "a file the turn deleted is still here"
	}
	if !strings.Contains(r.read(filepath.Join(r.work, "reply.txt")), "noted") {
		return "the turn's mailbox request got no answer"
	}
	if !strings.Contains(r.read(r.out+".err"), "a line on stderr") {
		return "the turn's stderr didn't come back"
	}
	return ""
}

// placed says the turn's record isn't one ended turn where, or "".
func (r *rehearsal) placed(where string, exit int) string {
	ps := r.home.Turns()
	if len(ps) != 1 || ps[0].Where != where || !ps[0].Ended || ps[0].Exit != exit {
		return fmt.Sprintf("turns.jsonl reads %+v, want one ended on %s with exit %d", ps, where, exit)
	}
	return ""
}

func (r *rehearsal) unreachable() {
	r.home.Add(Runner{Name: "rehearsal", Address: link("/nonexistent/runner")}, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIrehearsal")
}

func first(fails ...func() string) string {
	for _, f := range fails {
		if s := f(); s != "" {
			return s
		}
	}
	return ""
}

var scenes = []struct {
	name string
	play func(r *rehearsal) string
}{
	{"a worker turn runs on the runner and its work comes back", func(r *rehearsal) string {
		code := r.delegate()
		return first(func() string { return r.ran(code) }, func() string { return r.placed("rehearsal", 3) },
			func() string {
				if r.read(filepath.Join(r.work, "state-entries.txt")) != "0" {
					return "the Hive's records went across"
				}
				return ""
			},
			func() string {
				if ents, _ := os.ReadDir(filepath.Join(r.remote, "turns")); len(ents) != 0 {
					return "the turn's folder stayed on the runner"
				}
				bad := ""
				filepath.WalkDir(r.remote, func(p string, d fs.DirEntry, err error) error {
					if err == nil && d.Type().IsRegular() {
						raw, _ := os.ReadFile(p)
						for _, s := range []string{"SECRET", "short-lived"} {
							if bytes.Contains(raw, []byte(s)) {
								bad = p + " on the runner holds " + s
							}
						}
					}
					return nil
				})
				return bad
			})
	}},
	{"a second turn runs on the cache the first left", func(r *rehearsal) string {
		if code := r.delegate(); code != 3 {
			return fmt.Sprintf("first turn: exit %d", code)
		}
		os.WriteFile(r.out, nil, 0o644)
		code := r.delegate()
		if code != 3 || !strings.Contains(r.read(r.out), "done") {
			return fmt.Sprintf("second turn: exit %d: %s", code, clip(r.read(r.out+".err"), 300))
		}
		return ""
	}},
	{"the stop file reaches a turn on the runner", func(r *rehearsal) string {
		os.WriteFile(r.spec.Stdin, []byte("please stop"), 0o644)
		done := make(chan int, 1)
		go func() { done <- r.delegate() }()
		for deadline := time.Now().Add(60 * time.Second); !strings.Contains(r.read(r.out), "waiting for stop"); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				return "the turn never started"
			}
		}
		os.WriteFile(r.spec.StopFile, nil, 0o644)
		select {
		case code := <-done:
			if code != 143 {
				return fmt.Sprintf("exit %d, want 143", code)
			}
			return r.placed("rehearsal", 143)
		case <-time.After(60 * time.Second):
			return "the stop never reached the runner"
		}
	}},
	{"a turn whose runner is unreachable runs here", func(r *rehearsal) string {
		r.unreachable()
		code := r.delegate()
		return first(func() string { return r.ran(code) }, func() string { return r.placed("local", 3) })
	}},
	{"a turn whose runner has no sandbox runs here", func(r *rehearsal) string {
		r.home.Add(Runner{Name: "rehearsal", Address: r.runner("/nonexistent/hive"), MinFreeMB: 1, MaxLoad: 1000}, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIrehearsal")
		code := r.delegate()
		return first(func() string { return r.ran(code) }, func() string { return r.placed("local", 3) })
	}},
	{"a turn that can't go is given back when the core takes it", func(r *rehearsal) string {
		r.unreachable()
		os.Setenv(GiveBackEnv, "1")
		code := r.delegate()
		os.Unsetenv(GiveBackEnv)
		if code != 125 || strings.Contains(r.read(r.out), "got:") || !strings.Contains(r.read(r.out+".err"), "dispatch: not run:") {
			return fmt.Sprintf("exit %d, output %q: want 125, nothing run and a not-run line", code, clip(r.read(r.out), 200))
		}
		return r.placed("nowhere", 125)
	}},
	{"a run that reads the Hive's records runs here", func(r *rehearsal) string {
		r.spec.ID = "review-rehearsal"
		r.spec.Env = []string{"HIVE_ID=w-rehearsal"}
		code := r.delegate()
		return first(func() string { return r.ran(code) }, func() string { return r.placed("local", 3) })
	}},
	{"a check handed over is never run", func(r *rehearsal) string {
		r.spec.ID = "gate-rehearsal"
		code := r.delegate()
		if code != 125 || strings.Contains(r.read(r.out), "got:") {
			return fmt.Sprintf("exit %d, output %q: want 125 and nothing run", code, clip(r.read(r.out), 200))
		}
		return r.placed("nowhere", 125)
	}},
}
