package runner

import (
	"os"
	"strings"
	"testing"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// A turn naming hive goes only with the core's hive binary, built for this
// machine's kind.
func TestHiveForRefuses(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	own, err := wire.ELFMachine(self)
	if err != nil {
		t.Skip("not an ELF machine:", err)
	}
	cfg := Config{Dir: t.TempDir(), Hive: self}
	sha := strings.Repeat("a", 64)
	for name, c := range map[string]struct {
		t    wire.Turn
		want string
	}{
		"unnamed":       {wire.Turn{Tools: []string{"hive", "claude"}}, "wasn't named"},
		"bad sha":       {wire.Turn{Tools: []string{"hive"}, Hive: &wire.HiveBin{SHA256: "../x", Size: 1, Machine: own}}, "wasn't named"},
		"other machine": {wire.Turn{Tools: []string{"hive"}, Hive: &wire.HiveBin{SHA256: sha, Size: 1, Machine: "EM_NONE"}}, "built for EM_NONE"},
	} {
		if _, _, why := hiveFor(cfg, c.t); !strings.Contains(why, c.want) {
			t.Errorf("%s: why = %q, want %q", name, why, c.want)
		}
	}
	if p, need, why := hiveFor(cfg, wire.Turn{Tools: []string{"claude"}}); p != "" || need || why != "" {
		t.Errorf("a turn without hive: %q %v %q", p, need, why)
	}
	p, need, why := hiveFor(cfg, wire.Turn{Tools: []string{"hive"}, Hive: &wire.HiveBin{SHA256: sha, Size: 1, Machine: own}})
	if why != "" || !need || !strings.HasSuffix(p, "/hive/"+sha) {
		t.Errorf("a new hive: %q %v %q", p, need, why)
	}
}
