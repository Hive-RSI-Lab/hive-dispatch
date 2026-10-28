package main

import (
	"path/filepath"
	"testing"
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
