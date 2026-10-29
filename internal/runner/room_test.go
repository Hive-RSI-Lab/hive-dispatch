package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A runner takes turns by its load and memory, not up to a count: iiks1 at
// 18:17Z on 10-28 read 3 turns, load 0.4 on 6 cores, 29 GB free.
func TestFullByLoadAndMemory(t *testing.T) {
	cfg := Config{Name: "x", Dir: t.TempDir(), Hive: "hive"}
	cfg.Defaults()
	for _, c := range []struct {
		name               string
		max, n, young, mem int
		load               float64
		want               string
	}{
		{"idle with many turns", 0, 3, 0, 29000, 0.4, ""},
		{"loaded", 0, 3, 0, 29000, 5, "loaded"},
		{"low memory", 0, 3, 0, 3000, 0.4, "memory"},
		{"just-started turns count before the load shows them", 0, 4, 4, 29000, 0.6, "loaded"},
		{"just-started turns count against memory", 0, 2, 2, 5000, 0.4, "memory"},
		{"a fixed cap the person set", 3, 3, 0, 29000, 0.4, "full"},
	} {
		cfg.Max = c.max
		got := full(cfg, c.n, c.young, c.mem, c.load, 6)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("%s: full = %q, want %q", c.name, got, c.want)
		}
	}
}

// Setup wrote the old default "max": half the cores into runner.json; that
// key no longer caps a runner.
func TestOldMaxKeyIsNoCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runner.json")
	os.WriteFile(p, []byte(`{"name":"iiks1","max":3}`), 0o600)
	cfg, err := Load(p)
	if err != nil || cfg.Max != 0 || cfg.MaxLoad != DefaultMaxLoad || cfg.MinFreeMB != DefaultMinFreeMB {
		t.Fatalf("Load = %+v, %v", cfg, err)
	}
}

// A turn's age is its lock's, not its folder's, which changes as files land.
func TestCountYoung(t *testing.T) {
	cfg := Config{Dir: t.TempDir()}
	os.MkdirAll(filepath.Join(cfg.Dir, "turns"), 0o700)
	var held []*os.File
	for _, age := range []time.Duration{0, time.Hour} {
		d, _ := os.MkdirTemp(filepath.Join(cfg.Dir, "turns"), "t-")
		l, err := tryLock(filepath.Join(d, "lock"))
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, l)
		old := time.Now().Add(-age)
		os.Chtimes(filepath.Join(d, "lock"), old, old)
	}
	defer func() {
		for _, l := range held {
			l.Close()
		}
	}()
	if n, young := count(cfg); n != 2 || young != 1 {
		t.Fatalf("count = %d, %d young; want 2, 1", n, young)
	}
}
