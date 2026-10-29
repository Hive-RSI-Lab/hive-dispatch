package hive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/spec"
	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// bins builds the dispatch binary (the runner side) and the fake hive binary
// that stands in for `hive sandbox-exec` once per test run.
var built struct{ dispatch, fakehive string }

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dispatch-bin-")
	if err != nil {
		panic(err)
	}
	built.dispatch, built.fakehive = filepath.Join(dir, "hive-dispatch"), filepath.Join(dir, "hive")
	for out, pkg := range map[string]string{built.dispatch: "../../cmd/dispatch", built.fakehive: "./testdata/fakehive"} {
		if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			panic(string(b))
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// testHostKey stands in for a runner's host key: a test link runs no ssh.
const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest"

// localRunner is an ssh:// address that the tests' links reach through cmd
// on this machine instead (testLinks), as a runner in runners.json can't.
func localRunner(cmd string) string {
	addr := fmt.Sprintf("ssh://test@link-%d.invalid", len(testLinks))
	testLinks[addr] = strings.Fields(cmd)
	return addr
}

type world struct {
	t                                                         *testing.T
	home                                                      Home
	remote                                                    string // the fake runner's folder
	work, ro, box, rhome, kb, mods, state, tools, logins, out string
	spec                                                      spec.Spec
}

// newWorld lays out one run's folders as the core does and a spec over them
// as the core's sandboxSpec builds it, and registers a runner that is the
// dispatch binary on this machine, reached through a local command instead
// of SSH.
func newWorld(t *testing.T) *world {
	root := t.TempDir()
	w := &world{t: t, home: Home(filepath.Join(root, "module")), remote: filepath.Join(root, "remote")}
	p := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	w.work, w.ro, w.box, w.rhome = p("checkout"), p("runs", "r1", "ro"), p("runs", "r1", "box"), p("private", "r1", "home")
	w.kb, w.mods, w.state, w.tools, w.logins = p("kb"), p("modules"), p("state"), p("tools"), p("state", "logins")
	host := p("runs", "r1", "host")
	for _, d := range []string{w.work, w.ro, filepath.Join(w.box, "outbox"), filepath.Join(w.box, "inbox"), w.rhome, w.kb, w.mods, w.logins, w.tools, host, filepath.Join(w.work, ".git")} {
		os.MkdirAll(d, 0o755)
	}
	write := func(path, s string) { os.WriteFile(path, []byte(s), 0o644) }
	write(filepath.Join(w.work, "file.txt"), "line one\n")
	write(filepath.Join(w.work, "gone.txt"), "delete me\n")
	write(filepath.Join(w.work, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(filepath.Join(w.ro, "brief.md"), "the brief\n")
	write(filepath.Join(w.kb, "note.md"), "kb note\n")
	write(filepath.Join(w.state, "events.jsonl"), "SECRET-STATE\n")
	write(filepath.Join(w.logins, "token"), "SECRET-LOGIN\n")
	write(filepath.Join(host, "turn-001.prompt"), "hello")
	w.out = filepath.Join(host, "turn-001.jsonl")
	w.spec = spec.Spec{
		ID: "w-test", Argv: []string{"claude", "-p"}, Env: []string{"HIVE_ID=w-test", "CLAUDE_CODE_OAUTH_TOKEN=short-lived"}, Cwd: "/work",
		Mounts: []spec.Mount{
			{Host: w.work, Inside: "/work", RW: true}, {Host: w.ro, Inside: "/hive/run"}, {Host: w.box, Inside: "/hive/box", RW: true},
			{Host: w.rhome, Inside: "/hive/home", RW: true}, {Host: w.kb, Inside: "/hive/kb"}, {Host: w.mods, Inside: "/hive/modules"},
			{Host: p("projects"), Inside: "/hive/projects"}, {Host: w.state, Inside: "/hive/state"},
			{Host: w.logins, Inside: "/hive/state/logins", Hide: true}, {Host: w.tools, Inside: "/hive/tools"},
		},
		Tools: []string{built.fakehive, "claude"}, Stdin: filepath.Join(host, "turn-001.prompt"), Stdout: w.out,
		StopFile: filepath.Join(host, "stop"), Network: true,
	}
	addr := localRunner(built.dispatch + " runner serve --config /nonexistent --name fake --max 2 --dir " + w.remote + " --hive " + built.fakehive)
	if err := w.home.Add(Runner{Name: "fake", Address: addr, MinFreeMB: 1, MaxLoad: 1000}, testHostKey); err != nil {
		t.Fatal(err)
	}
	return w
}

// core answers mailbox requests as the core's Drain does, while the turn runs.
func (w *world) core(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(50 * time.Millisecond):
		}
		wire.TakeMail(filepath.Join(w.box, "outbox"), func(m wire.Mail) error {
			wire.PutMail(filepath.Join(w.box, "inbox"), wire.Mail{Name: m.Name, Data: []byte(`{"id":"r1","ok":true,"result":"noted"}`)})
			return nil
		})
	}
}

func (w *world) delegate() int {
	f := filepath.Join(w.t.TempDir(), "spec.json")
	if err := spec.Write(f, w.spec); err != nil {
		w.t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go w.core(stop)
	code := w.home.Delegate(f, "")
	if _, err := os.Stat(f); err == nil {
		w.t.Error("the spec file, with its token, was left on disk")
	}
	return code
}

func (w *world) read(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		w.t.Errorf("read %s: %v", path, err)
	}
	return string(raw)
}

func (w *world) checkTurnRan() {
	t := w.t
	out := w.read(w.out)
	for _, want := range []string{"got: hello", "HIVE_ID=w-test", "done"} {
		if !strings.Contains(out, want) {
			t.Errorf("turn output lacks %q:\n%s", want, out)
		}
	}
	if got := w.read(filepath.Join(w.work, "file.txt")); got != "line one\nremote edit\n" {
		t.Errorf("file.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(w.work, "gone.txt")); err == nil {
		t.Error("gone.txt was deleted there but not here")
	}
	if got := w.read(filepath.Join(w.work, "new", "dir", "x.txt")); got != "x" {
		t.Errorf("new file = %q", got)
	}
	if got := w.read(filepath.Join(w.work, "kb-seen.txt")); got != "kb note\n" {
		t.Errorf("the turn saw kb %q", got)
	}
	if got := w.read(filepath.Join(w.work, "reply.txt")); !strings.Contains(got, "noted") {
		t.Errorf("the mailbox reply = %q", got)
	}
	if got := w.read(filepath.Join(w.work, ".git", "HEAD")); !strings.Contains(got, "main") {
		t.Errorf(".git/HEAD = %q", got)
	}
	if got := w.read(filepath.Join(w.box, "notes.md")); got != "notes from the turn\n" {
		t.Errorf("notes.md = %q", got)
	}
	if got := w.read(filepath.Join(w.rhome, "session.jsonl")); got != "session\n" {
		t.Errorf("home session = %q", got)
	}
	if got := w.read(w.out + ".err"); !strings.Contains(got, "a line on stderr") {
		t.Errorf("stderr = %q", got)
	}
	if got := w.read(w.out + ".egress"); !strings.Contains(got, "example.com") {
		t.Errorf("egress refusals = %q", got)
	}
}

func TestTurnRunsOnRunner(t *testing.T) {
	w := newWorld(t)
	if code := w.delegate(); code != 3 {
		t.Fatalf("exit %d, want the turn's own 3; err: %s", code, w.read(w.out+".err"))
	}
	w.checkTurnRan()
	if got := w.read(filepath.Join(w.work, "state-entries.txt")); got != "0" {
		t.Errorf("the Hive's records went across: %s entries", got)
	}
	ps := w.home.Turns()
	if len(ps) != 1 || ps[0].Where != "fake" || !ps[0].Ended || ps[0].Exit != 3 || ps[0].Sent == 0 || ps[0].Back == 0 {
		t.Fatalf("turns = %+v", ps)
	}
	// Nothing of the turn stays there, and nothing secret ever went.
	if ents, _ := os.ReadDir(filepath.Join(w.remote, "turns")); len(ents) != 0 {
		t.Errorf("the turn's folder stayed on the runner: %v", ents)
	}
	filepath.WalkDir(w.remote, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			raw, _ := os.ReadFile(p)
			for _, s := range []string{"SECRET", "short-lived"} {
				if bytes.Contains(raw, []byte(s)) {
					t.Errorf("%s holds %q", p, s)
				}
			}
		}
		return nil
	})
	if got := w.read(filepath.Join(w.remote, "shared", sharedKey(t, w)) + "/note.md"); got != "kb note\n" {
		t.Errorf("the kb cache = %q", got)
	}

	// A second turn runs on the cache the first left.
	os.WriteFile(w.out, nil, 0o644)
	if code := w.delegate(); code != 3 {
		t.Fatalf("second turn: exit %d: %s %+v", code, w.read(w.out+".err"), w.home.Turns())
	}
}

func sharedKey(t *testing.T, w *world) string {
	for _, m := range planFor(w.spec).mounts {
		if m.Inside == "/hive/kb" {
			return m.Key
		}
	}
	t.Fatal("no kb mount")
	return ""
}

func TestStopReachesRunner(t *testing.T) {
	w := newWorld(t)
	os.WriteFile(w.spec.Stdin, []byte("please stop"), 0o644)
	done := make(chan int)
	go func() { done <- w.delegate() }()
	deadline := time.Now().Add(20 * time.Second)
	for raw, _ := os.ReadFile(w.out); !strings.Contains(string(raw), "waiting for stop"); raw, _ = os.ReadFile(w.out) {
		if time.Now().After(deadline) {
			t.Fatal("the turn never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	os.WriteFile(w.spec.StopFile, nil, 0o644)
	select {
	case code := <-done:
		if code != 143 {
			t.Fatalf("exit %d, want 143", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the stop file never reached the runner")
	}
}

func TestFallsBackHere(t *testing.T) {
	for name, edit := range map[string]func(w *world){
		"granted a local service": func(w *world) { w.spec.Local = []string{"127.0.0.1:11434"} },
		"runner unreachable": func(w *world) {
			w.home.Add(Runner{Name: "fake", Address: localRunner("/nonexistent/runner")}, testHostKey)
		},
		"runner has no sandbox": func(w *world) {
			w.home.Add(Runner{Name: "fake", Address: localRunner(built.dispatch + " runner serve --config /nonexistent --max 1 --dir " + w.remote + " --hive /nonexistent/hive"), MinFreeMB: 1, MaxLoad: 1000}, testHostKey)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			edit(w)
			if code := w.delegate(); code != 3 {
				t.Fatalf("exit %d; err: %s", code, w.read(w.out+".err"))
			}
			w.checkTurnRan()
			ps := w.home.Turns()
			if len(ps) != 1 || ps[0].Where != "local" || ps[0].Why == "" || !ps[0].Ended {
				t.Fatalf("turns = %+v", ps)
			}
			// The spec, token and all, went to sandbox-exec from the module's
			// own HOME, readable by it alone, and didn't stay there.
			tmp := filepath.Join(string(w.home), "tmp")
			if want := "spec-file: " + tmp + string(filepath.Separator); !strings.Contains(w.read(w.out), want) || !strings.Contains(w.read(w.out), " 600\n") {
				t.Errorf("the spec wasn't written under HOME as 0600:\n%s", w.read(w.out))
			}
			if fi, err := os.Stat(tmp); err != nil || fi.Mode().Perm() != 0o700 {
				t.Errorf("tmp/ = %v %v", fi, err)
			}
			if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
				t.Errorf("a spec stayed in tmp/: %v", ents)
			}
		})
	}
}

func TestRoom(t *testing.T) {
	h := Home(t.TempDir())
	ok := wire.Status{Cores: 6, MemFreeMB: 25000, Max: 3, Sandbox: "ok bwrap"}
	for _, c := range []struct {
		name string
		edit func(*Seen)
		want string
	}{
		{"room", func(*Seen) {}, ""},
		{"full", func(s *Seen) { s.Running = 3 }, "full"},
		{"loaded", func(s *Seen) { s.Load1 = 5 }, "loaded"},
		{"low memory", func(s *Seen) { s.MemFreeMB = 1000 }, "memory"},
		{"no sandbox", func(s *Seen) { s.Sandbox = "unavailable: no bwrap" }, "sandbox"},
		{"unreachable", func(s *Seen) { s.Reach = "connection refused" }, "unreachable"},
	} {
		s := Seen{Status: ok, At: time.Now()}
		c.edit(&s)
		got := Runner{Name: "x"}.Room(s)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("%s: Room = %q, want %q", c.name, got, c.want)
		}
	}
	h.Save([]Runner{{Name: "busy", Address: "ssh://u@x"}, {Name: "idle", Address: "ssh://u@x"}, {Name: "off", Address: "ssh://u@x", Off: true}})
	busy, idle := ok, ok
	busy.Running = 2
	h.See("busy", Seen{Status: busy, At: time.Now()})
	h.See("idle", Seen{Status: idle, At: time.Now()})
	if r, why, err := h.Pick(); err != nil || r.Name != "idle" {
		t.Fatalf("Pick = %q (%s, %v), want idle", r.Name, why, err)
	}
	idle.Running = 3
	busy.Running = 3
	h.See("busy", Seen{Status: busy, At: time.Now()})
	h.See("idle", Seen{Status: idle, At: time.Now()})
	if r, why, _ := h.Pick(); r.Name != "" || !strings.Contains(why, "full") || !strings.Contains(why, "off: turned off") {
		t.Fatalf("Pick = %q, why %q", r.Name, why)
	}
}

func TestPlanCarriesNoHiddenOrHiveRecords(t *testing.T) {
	w := newWorld(t)
	p := planFor(w.spec)
	kinds := map[string]string{}
	for _, m := range p.mounts {
		kinds[m.Inside] = m.Kind
	}
	want := map[string]string{"/work": "run", "/hive/run": "run", "/hive/box": "run", "/hive/home": "run", "/hive/kb": "shared",
		"/hive/modules": "shared", "/hive/tools": "shared", "/hive/projects": "empty", "/hive/state": "empty"}
	for in, k := range want {
		if kinds[in] != k {
			t.Errorf("%s goes as %q, want %q", in, kinds[in], k)
		}
	}
	if _, ok := kinds["/hive/state/logins"]; ok {
		t.Error("a hide under an empty mount was kept")
	}
	// A hide under a mount that is sent keeps its contents here.
	w.spec.Mounts = append(w.spec.Mounts, spec.Mount{Host: w.logins, Inside: "/work/secret", Hide: true})
	os.MkdirAll(filepath.Join(w.work, "secret"), 0o755)
	os.WriteFile(filepath.Join(w.work, "secret", "k"), []byte("SECRET"), 0o600)
	p = planFor(w.spec)
	m, _ := wire.Scan(w.work, p.skips[0])
	for k := range m {
		if strings.HasPrefix(k, "secret") {
			t.Errorf("%s would be sent", k)
		}
	}
	raw, _ := json.Marshal(p.mounts)
	if !strings.Contains(string(raw), `"/work/secret","rw":false,"hide":true`) {
		t.Errorf("the hide isn't in the plan: %s", raw)
	}
}

func TestDispatchable(t *testing.T) {
	s := spec.Spec{Cwd: "/home/x/checkout", Mounts: []spec.Mount{{Host: "/home/x/checkout", Inside: "/work"}}, Stdout: "o"}
	if Dispatchable(s) == "" {
		t.Error("a bare turn (host paths) was dispatchable")
	}
	s.Cwd = "/work"
	if why := Dispatchable(s); why != "" {
		t.Errorf("a sandboxed turn wasn't: %s", why)
	}
}

// A turn that runs here runs only the hive binary the core named among its
// tools, by its absolute path.
func TestLocalRunsOnlyTheHiveBinary(t *testing.T) {
	for _, tools := range [][]string{nil, {"claude"}, {"hive"}, {"/bin/sh"}, {"/usr/bin/hivemind"}, {"/nonexistent/hive"}} {
		if got, err := hiveTool(spec.Spec{Tools: tools}); err == nil {
			t.Errorf("tools %q: ran %q", tools, got)
		}
	}
	if got, err := hiveTool(spec.Spec{Tools: []string{"claude", built.fakehive}}); err != nil || got != built.fakehive {
		t.Errorf("hiveTool = %q, %v", got, err)
	}
}

// A check the core hands over brings back its output and exit code, and
// nothing of its checkout or home: the core deletes those as it ends.
func TestCheckBringsBackOnlyOutput(t *testing.T) {
	w := newWorld(t)
	w.spec.ID = "gate-test"
	w.spec.Mounts = []spec.Mount{{Host: w.work, Inside: "/work", RW: true}, {Host: w.rhome, Inside: "/hive/home", RW: true}, {Host: w.tools, Inside: "/hive/tools"}}
	if code := w.delegate(); code != 1 {
		t.Fatalf("exit %d, want the check's own 1; err: %s", code, w.read(w.out+".err"))
	}
	if out := w.read(w.out); !strings.Contains(out, "got: hello") || !strings.HasSuffix(out, "done\n\nhive-check-exit gate-test 1\n") {
		t.Errorf("check output = %q, want it to end with the core's exit marker unchanged", out)
	}
	if got := w.read(filepath.Join(w.work, "file.txt")); got != "line one\n" {
		t.Errorf("the checkout came back: file.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(w.work, "gone.txt")); err != nil {
		t.Error("a deletion there came back here")
	}
	for _, p := range []string{filepath.Join(w.work, "new"), filepath.Join(w.rhome, "session.jsonl")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s came back", p)
		}
	}
	ps := w.home.Turns()
	if len(ps) != 1 || ps[0].Where != "fake" || ps[0].Exit != 1 || ps[0].Sent == 0 || ps[0].Back != 0 {
		t.Fatalf("turns = %+v", ps)
	}
	if ents, _ := os.ReadDir(filepath.Join(w.remote, "turns")); len(ents) != 0 {
		t.Errorf("the check's folder stayed on the runner: %v", ents)
	}
}

// A check that can't go is never run here: it would run outside the core's
// check line. It exits without output, so the core finds no exit marker and
// puts it back in its line.
func TestCheckNeverRunsHere(t *testing.T) {
	for name, edit := range map[string]func(w *world){
		"granted a local service": func(w *world) { w.spec.Local = []string{"127.0.0.1:11434"} },
		"runner unreachable": func(w *world) {
			w.home.Add(Runner{Name: "fake", Address: localRunner("/nonexistent/runner")}, testHostKey)
		},
		"runner has no sandbox": func(w *world) {
			w.home.Add(Runner{Name: "fake", Address: localRunner(built.dispatch + " runner serve --config /nonexistent --max 1 --dir " + w.remote + " --hive /nonexistent/hive"), MinFreeMB: 1, MaxLoad: 1000}, testHostKey)
		},
		"no runner": func(w *world) { w.home.Remove("fake") },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.spec.ID = "gate-test"
			w.spec.Mounts = []spec.Mount{{Host: w.work, Inside: "/work", RW: true}, {Host: w.rhome, Inside: "/hive/home", RW: true}, {Host: w.tools, Inside: "/hive/tools"}}
			edit(w)
			if code := w.delegate(); code != 125 {
				t.Fatalf("exit %d, want 125", code)
			}
			if raw, _ := os.ReadFile(w.out); strings.Contains(string(raw), "hive-check-exit") || strings.Contains(string(raw), "got:") {
				t.Errorf("the check ran: %q", raw)
			}
			if got := w.read(w.out + ".err"); !strings.Contains(got, "not run") {
				t.Errorf(".err = %q", got)
			}
			ps := w.home.Turns()
			if len(ps) != 1 || ps[0].Where != "nowhere" || ps[0].Exit != 125 || !ps[0].Ended {
				t.Fatalf("turns = %+v", ps)
			}
			if got := w.home.Score(time.Now()); got != 0 {
				t.Errorf("score = %d", got)
			}
		})
	}
}

// Once a runner caches a mount as big as the Hive's tools (the whole Go SDK),
// its manifest of them is over one frame; the next turn still runs there.
func TestBigCachedMount(t *testing.T) {
	if testing.Short() {
		t.Skip("lays out 45000 files")
	}
	w := newWorld(t)
	for d := 0; d < 450; d++ {
		dir := filepath.Join(w.tools, "go", "src", fmt.Sprintf("a-rather-long-package-path-%03d", d), "internal", "subpackage")
		os.MkdirAll(dir, 0o755)
		for f := 0; f < 100; f++ {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("source_file_with_a_long_name_%03d.go", f)), nil, 0o644)
		}
	}
	if man, _ := wire.Scan(w.tools, nil); len(must(json.Marshal(man))) <= wire.MaxFrame {
		t.Fatal("the tools manifest fits one frame; the test needs it over")
	}
	for i := 1; i <= 2; i++ {
		os.WriteFile(w.out, nil, 0o644)
		if code := w.delegate(); code != 3 {
			t.Fatalf("turn %d: exit %d: %s %+v", i, code, w.read(w.out+".err"), w.home.Turns())
		}
	}
	if ps := w.home.Turns(); len(ps) == 0 || ps[len(ps)-1].Where != "fake" {
		t.Fatalf("turns = %+v", ps)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
