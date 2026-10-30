package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A binary installed by hand after the Hive's last one serves, until the
// Hive sends its own again.
func TestHopPrefersTheNewer(t *testing.T) {
	cfg := Config{Dir: t.TempDir()}
	sum := strings.Repeat("a", 64)
	next := filepath.Join(cfg.Dir, "bin", "dispatch-"+sum)
	os.MkdirAll(filepath.Dir(next), 0o700)
	os.WriteFile(next, []byte("x"), 0o700)
	os.WriteFile(filepath.Join(cfg.Dir, "bin", "current"), []byte(sum+"\n"), 0o600)
	old := time.Now().Add(-1000 * time.Hour)
	os.Chtimes(next, old, old)
	if got := Hop(cfg); got != "" {
		t.Errorf("hopped to the older %s", got)
	}
	soon := time.Now().Add(time.Hour)
	os.Chtimes(next, soon, soon)
	if got := Hop(cfg); got != next {
		t.Errorf("Hop = %q, want %s", got, next)
	}
	cfg.NoUpdate = true
	if got := Hop(cfg); got != "" {
		t.Errorf("hopped with updates off")
	}
	cfg.NoUpdate = false
	t.Setenv(Hopped, "1")
	if got := Hop(cfg); got != "" {
		t.Errorf("hopped twice")
	}
}
