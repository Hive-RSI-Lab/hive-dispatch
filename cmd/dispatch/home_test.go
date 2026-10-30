package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The core runs `room` and `turn` with HOME set to the module's folder and
// every HIVE_ variable scrubbed (the core's runner.Env); home must find the
// module's folder from HOME alone, or `room` fails and no turn ever leaves.
func TestHomeFromTheCoresScrubbedEnv(t *testing.T) {
	mod := filepath.Join(t.TempDir(), "hive", ".hive", "modules", "dispatch")
	env := map[string]string{"HOME": mod}
	h, err := homeFrom(func(k string) string { return env[k] })
	if err != nil || string(h) != mod {
		t.Fatalf("home = %q, %v; want %q", h, err, mod)
	}
	// DISPATCH_HOME still wins, and the long-lived program's HIVE_MODULE too.
	env["DISPATCH_HOME"] = "/elsewhere"
	if h, _ := homeFrom(func(k string) string { return env[k] }); h != "/elsewhere" {
		t.Fatalf("home = %q with DISPATCH_HOME", h)
	}
	other := t.TempDir()
	env = map[string]string{"HOME": other, "HIVE_MODULE": "dispatch"}
	if h, _ := homeFrom(func(k string) string { return env[k] }); string(h) != other {
		t.Fatalf("home = %q with HIVE_MODULE", h)
	}
}

// The Hive's score check runs with an empty HOME of its own and HIVE_HOME at
// a read-only copy of the module's records (the core's moduleRecords), the
// copy cut to its last bytes, so its first line may be half a line.
func TestRanReadsTheScoreChecksRecords(t *testing.T) {
	root := t.TempDir()
	mod := filepath.Join(root, ".hive", "modules", "dispatch")
	os.MkdirAll(mod, 0o755)
	end := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	lines := `ere":"iiks1","exit":0,"ended":true}
{"at":"` + end + `","run":"w-1","turn":"t1","where":"iiks1","exit":0,"secs":60,"ended":true}
{"at":"` + end + `","run":"w-2","turn":"t1","where":"iiks1","exit":1,"secs":60,"ended":true}
{"at":"` + end + `","run":"w-3","turn":"t1","where":"local","exit":0,"secs":60,"ended":true}
{"at":"` + end + `","run":"gate-ab-test-1","turn":"g.log","where":"iiks1","exit":0,"secs":60,"ended":true}
`
	if err := os.WriteFile(filepath.Join(mod, "turns.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": t.TempDir(), "HIVE_HOME": root}
	h, err := homeFrom(func(k string) string { return env[k] })
	if err != nil || string(h) != mod {
		t.Fatalf("home = %q, %v; want %q", h, err, mod)
	}
	if n := h.Ran(time.Now()); n != 1 {
		t.Fatalf("ran %d, want 1 (only w-1)", n)
	}
}
