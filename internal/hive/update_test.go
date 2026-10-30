package hive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

func sha(t *testing.T, path string) string {
	b, err := wire.DescribeHive(path)
	if err != nil {
		t.Fatal(err)
	}
	return b.SHA256
}

func hiveBinary(t *testing.T, path string) {
	t.Cleanup(func() { SelfBinary = func() (string, error) { return built.dispatch, nil } })
	SelfBinary = func() (string, error) { return path, nil }
}

// A Hive whose dispatch moved ahead brings the runner onto its own binary
// through the forced command alone: the next session serves from it.
func TestRunnerTakesTheHivesBinary(t *testing.T) {
	w := newWorld(t)
	r, _ := w.home.Get("fake")
	if s := w.home.Probe(r); s.Binary != sha(t, built.dispatch) {
		t.Fatalf("the runner serves %q, want the installed %s (reach %q)", s.Binary, sha(t, built.dispatch), s.Reach)
	}
	hiveBinary(t, built.next)
	s := w.home.Probe(r)
	if s.Reach != "" || s.Binary != sha(t, built.next) {
		u, _ := w.home.LastUpdate("fake")
		t.Fatalf("after the update the runner serves %q, want %s (reach %q, update %+v)", s.Binary, sha(t, built.next), s.Reach, u)
	}
	if got, _ := os.ReadFile(filepath.Join(w.remote, "bin", "current")); strings.TrimSpace(string(got)) != sha(t, built.next) {
		t.Errorf("bin/current = %q", got)
	}
	// It runs turns as the new binary.
	if code := w.delegate(); code != 3 {
		t.Fatalf("exit %d after the update: %s", code, w.read(w.out+".err"))
	}
	w.checkTurnRan()
	// And goes back when the Hive does.
	hiveBinary(t, built.dispatch)
	if s := w.home.Probe(r); s.Binary != sha(t, built.dispatch) {
		t.Errorf("after the Hive went back the runner serves %q", s.Binary)
	}
}

func TestRunnerWithUpdatesOffKeepsItsBinary(t *testing.T) {
	w := newWorld(t)
	cfg := filepath.Join(t.TempDir(), "runner.json")
	os.WriteFile(cfg, []byte(`{"no_update": true}`), 0o600)
	addr := localRunner(built.dispatch + " runner serve --config " + cfg + " --name off --max-load 1000 --min-free-mb 1 --dir " + w.remote + " --hive " + built.fakehive)
	if err := w.home.Add(Runner{Name: "off", Address: addr, MinFreeMB: 1, MaxLoad: 1000}, testHostKey); err != nil {
		t.Fatal(err)
	}
	r, _ := w.home.Get("off")
	hiveBinary(t, built.next)
	if s := w.home.Probe(r); s.Binary != sha(t, built.dispatch) || s.Reach != "" {
		t.Fatalf("the runner serves %q (reach %q), want its own", s.Binary, s.Reach)
	}
	if u, _ := w.home.LastUpdate("off"); !strings.Contains(u.Error, "no_update") {
		t.Errorf("last update = %+v", u)
	}
	if b := strings.Join(w.home.Board(0), "\n"); !strings.Contains(b, "didn't take the Hive's dispatch binary") {
		t.Errorf("the board doesn't say so:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(w.remote, "bin")); err == nil {
		t.Error("a runner with updates off kept a binary")
	}
	// Not offered again at every read.
	u1, _ := w.home.LastUpdate("off")
	w.home.Probe(r)
	if u2, _ := w.home.LastUpdate("off"); !u2.At.Equal(u1.At) {
		t.Error("offered again within UpdateRetry")
	}
}

// A binary that doesn't say it speaks the Hive's protocol never serves.
func TestRunnerRefusesABinaryThatDoesntRun(t *testing.T) {
	w := newWorld(t)
	r, _ := w.home.Get("fake")
	hiveBinary(t, built.fakehive)
	s := w.home.Probe(r)
	if s.Reach != "" || s.Binary != sha(t, built.dispatch) {
		t.Fatalf("the runner serves %q (reach %q)", s.Binary, s.Reach)
	}
	if u, _ := w.home.LastUpdate("fake"); !strings.Contains(u.Error, "doesn't run here") && !strings.Contains(u.Error, "protocol") {
		t.Errorf("last update = %+v", u)
	}
	if _, err := os.Stat(filepath.Join(w.remote, "bin", "current")); err == nil {
		t.Error("bin/current names it")
	}
}
