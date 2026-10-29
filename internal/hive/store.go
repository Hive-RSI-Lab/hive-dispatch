// Package hive is the Hive's side of dispatch: the runners the person
// registered, how much room each has, the turn that goes to one of them, and
// the record of where every turn ran.
//
// Everything it keeps is in the module's HOME, which the Hive gives the
// module and covers from every agent: the runner list, the SSH key that is the
// Hive's half of each link, the pinned host keys that are the runners' half,
// the last status read from each runner, and turns.jsonl.
package hive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// Runner is one registered machine.
type Runner struct {
	Name    string `json:"name"`
	Address string `json:"address"` // ssh://user@host[:port][/path/to/hive-dispatch]
	// A runner has room while its load per core is under MaxLoad and it has
	// MinFreeMB of memory free; 0 takes the defaults.
	MaxLoad   float64 `json:"max_load,omitempty"`
	MinFreeMB int     `json:"min_free_mb,omitempty"`
	Off       bool    `json:"off,omitempty"` // registered but offered nothing
}

// Defaults for a runner's room.
const (
	DefaultMaxLoad   = 0.75
	DefaultMinFreeMB = 4096
)

// Home is the module's folder.
type Home string

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func (h Home) path(parts ...string) string {
	return filepath.Join(append([]string{string(h)}, parts...)...)
}

// Key is the private key of the Hive's half of every link.
func (h Home) Key() string { return h.path("id_ed25519") }

// KnownHosts holds each runner's pinned host key.
func (h Home) KnownHosts() string { return h.path("known_hosts") }

// Runners lists the registered runners by name.
func (h Home) Runners() ([]Runner, error) {
	raw, err := os.ReadFile(h.path("runners.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rs []Runner
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, fmt.Errorf("runners.json: %w", err)
	}
	return rs, nil
}

// Save writes the runner list.
func (h Home) Save(rs []Runner) error {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	raw, _ := json.MarshalIndent(rs, "", "  ")
	return writeAtomic(h.path("runners.json"), raw)
}

// Get finds a runner by name.
func (h Home) Get(name string) (Runner, error) {
	rs, err := h.Runners()
	if err != nil {
		return Runner{}, err
	}
	for _, r := range rs {
		if r.Name == name {
			return r, nil
		}
	}
	return Runner{}, fmt.Errorf("no runner %q (dispatch list shows them)", name)
}

// Seen is a runner's status as last read, and when.
type Seen struct {
	wire.Status
	At    time.Time `json:"at"`
	Reach string    `json:"reach,omitempty"` // why it couldn't be read
}

// Seen reads a runner's last status.
func (h Home) Seen(name string) (Seen, bool) {
	var s Seen
	raw, err := os.ReadFile(h.path("status", name+".json"))
	if err != nil || json.Unmarshal(raw, &s) != nil {
		return s, false
	}
	return s, true
}

// See records a runner's status.
func (h Home) See(name string, s Seen) error {
	os.MkdirAll(h.path("status"), 0o700)
	raw, _ := json.Marshal(s)
	return writeAtomic(h.path("status", name+".json"), raw)
}

// Placed is the record of one turn: where it ran and how it ended.
type Placed struct {
	At    time.Time `json:"at"`
	Run   string    `json:"run"`
	Turn  string    `json:"turn"`
	Where string    `json:"where"`         // a runner's name, "local", or "nowhere" (a check not run)
	Why   string    `json:"why,omitempty"` // why it ran locally after all
	Exit  int       `json:"exit"`
	Secs  float64   `json:"secs"`
	Error string    `json:"error,omitempty"`
	Sent  int64     `json:"sent,omitempty"` // bytes of files sent
	Back  int64     `json:"back,omitempty"` // bytes of files brought back
	Ended bool      `json:"ended"`          // false: the turn has started there
}

// Record appends to turns.jsonl.
func (h Home) Record(p Placed) error {
	f, err := os.OpenFile(h.path("turns.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, _ := json.Marshal(p)
	_, err = f.Write(append(raw, '\n'))
	return err
}

func writeAtomic(path string, raw []byte) error {
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0o700)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// LackFor is how long a runner that lacked a turn's tool is passed over for
// turns that need it, before one is tried there again (the person may have
// installed it since).
const LackFor = time.Hour

// Lack records that runner hasn't the tools named.
func (h Home) Lack(runner string, tools []string) error {
	have := h.lacks(runner)
	for _, t := range tools {
		have[t] = time.Now()
	}
	raw, _ := json.Marshal(have)
	return writeAtomic(h.path("lacks", runner+".json"), raw)
}

// Lacks is the tools runner lacked within LackFor, by name, sorted.
func (h Home) Lacks(runner string) []string {
	var out []string
	for t, at := range h.lacks(runner) {
		if time.Since(at) < LackFor {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func (h Home) lacks(runner string) map[string]time.Time {
	m := map[string]time.Time{}
	if raw, err := os.ReadFile(h.path("lacks", runner+".json")); err == nil {
		json.Unmarshal(raw, &m)
	}
	return m
}

// LackForChecks records that a check of project failed on runner for a tool
// it hasn't, so that project's checks stay here for LackFor.
func (h Home) LackForChecks(runner, project, tool string) error {
	have := h.checkLacks(runner)
	have[project] = checkLack{Tool: tool, At: time.Now()}
	raw, _ := json.Marshal(have)
	return writeAtomic(h.path("lacks", runner+".checks.json"), raw)
}

// ChecksLack is the projects whose checks lacked a tool on runner within
// LackFor, with the tool each lacked.
func (h Home) ChecksLack(runner string) map[string]string {
	out := map[string]string{}
	for p, l := range h.checkLacks(runner) {
		if time.Since(l.At) < LackFor {
			out[p] = l.Tool
		}
	}
	return out
}

type checkLack struct {
	Tool string    `json:"tool"`
	At   time.Time `json:"at"`
}

func (h Home) checkLacks(runner string) map[string]checkLack {
	m := map[string]checkLack{}
	if raw, err := os.ReadFile(h.path("lacks", runner+".checks.json")); err == nil {
		json.Unmarshal(raw, &m)
	}
	return m
}
