package hive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveSandbox runs a turn through a runner on this machine whose sandbox
// is the real hive binary's (`hive sandbox-exec`: bubblewrap or the binary's
// own namespaces). It needs a machine where that sandbox runs, so it is
// opted in: DISPATCH_LIVE_HIVE=$(command -v hive) go test ./internal/hive -run Live
func TestLiveSandbox(t *testing.T) {
	hiveBin := os.Getenv("DISPATCH_LIVE_HIVE")
	if hiveBin == "" {
		t.Skip("set DISPATCH_LIVE_HIVE to a hive binary to run a turn in its real sandbox")
	}
	w := newWorld(t)
	addr := localRunner(built.dispatch + " runner serve --config /nonexistent --name live --max 2 --max-load 1000 --min-free-mb 1 --dir " + w.remote + " --hive " + hiveBin)
	if err := w.home.Add(Runner{Name: "fake", Address: addr, MinFreeMB: 1, MaxLoad: 1000}, testHostKey); err != nil {
		t.Fatal(err)
	}
	w.spec.Argv = []string{"sh", "-c", `cat; echo; echo inside > /work/live.txt; ls /hive/state | wc -l; cat /hive/kb/note.md; exit 4`}
	w.spec.Env = []string{"PATH=/usr/bin:/bin", "HOME=/hive/home"}
	w.spec.Tools = []string{hiveBin}
	w.spec.Network = false
	if code := w.delegate(); code != 4 {
		t.Fatalf("exit %d: %s", code, w.read(w.out+".err"))
	}
	if got := w.read(filepath.Join(w.work, "live.txt")); got != "inside\n" {
		t.Errorf("live.txt = %q", got)
	}
	out := w.read(w.out)
	for _, want := range []string{"hello", "kb note"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if ps := w.home.Turns(); len(ps) != 1 || ps[0].Where != "fake" {
		t.Errorf("turns = %+v", ps)
	}
}
