package wire

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/spec"
)

// The conversation, the Hive speaking first:
//
//	Hive    J Hello{op: "status"}         runner  J Status
//	Hive    J Hello{op: "turn"}, J Turn   runner  J Manifests (of its cached shared mounts)
//	Hive    if Manifests.NeedHive: T..., E (the hive binary Turn.Hive names)
//	Hive    per run or shared mount: J Tree, T..., E
//	runner  J Started, then O/R output and M mailbox requests as they come;
//	Hive    I mailbox responses and S (stop) as they come
//	runner  X ExitCode, then per RW mount that isn't Throwaway: J Tree, T..., E; then Z
//
// Any J message over MaxFrame goes as B... J: its pieces in Big frames, the
// last in the J frame.

// Hello opens a session.
type Hello struct {
	Version int    `json:"version"`
	Op      string `json:"op"` // status | turn
}

// Status is a runner's room.
type Status struct {
	Name       string  `json:"name"`
	Version    int     `json:"version"`
	Cores      int     `json:"cores"`
	MemTotalMB int     `json:"mem_total_mb"`
	MemFreeMB  int     `json:"mem_free_mb"`
	Load1      float64 `json:"load1"`
	Running    int     `json:"running"`
	Young      int     `json:"young,omitempty"` // of those, how many started too lately to show in the load and memory yet
	Max        int     `json:"max"`
	Sandbox    string  `json:"sandbox"` // what `hive sandbox-probe` said, or why it failed
	Error      string  `json:"error,omitempty"`
}

// Mount kinds.
const (
	KindRun    = "run"    // this turn's own: sent whole, RW ones sent back, wiped after
	KindShared = "shared" // read-only and common to turns (kb, modules, tools): cached, sent as changes
	KindEmpty  = "empty"  // not carried (other projects, the Hive's records): an empty folder
)

// MountPlan is how one of the spec's mounts goes across, by index.
type MountPlan struct {
	Inside string `json:"inside"`
	RW     bool   `json:"rw"`
	Hide   bool   `json:"hide,omitempty"`
	Kind   string `json:"kind"`
	Key    string `json:"key,omitempty"` // a shared mount's cache name
	// Throwaway is an RW run mount whose files don't come back: a check's
	// checkout and home, which the core deletes as soon as it ends. The
	// runner wipes it with the rest of the turn.
	Throwaway bool `json:"throwaway,omitempty"`
}

// Turn is what the runner needs to run a turn. Spec's host paths are blank;
// the runner fills its own.
type Turn struct {
	ID     string      `json:"id"`
	Label  string      `json:"label"` // the turn's output file name
	Spec   spec.Spec   `json:"spec"`
	Mounts []MountPlan `json:"mounts"`
	Stdin  []byte      `json:"stdin"`
	Tools  []string    `json:"tools"` // base names of the tools to expose
	// Hive is the hive binary the core named among the turn's tools. It is
	// the turn's `hive` inside the sandbox there too, so its `hive agent`
	// ops are the core's own CLI, not whatever release the runner has
	// installed. The runner's own hive still runs sandbox-exec.
	Hive *HiveBin `json:"hive,omitempty"`
}

// HiveBin names a hive binary by its content.
type HiveBin struct {
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Machine string `json:"machine"` // its ELF machine (EM_X86_64): a runner of another kind can't run it
}

// Manifests is the runner's cached state of the shared mounts, by index.
type Manifests struct {
	Mounts map[int]Manifest `json:"mounts"`
	// NeedHive asks for the hive binary Turn.Hive names: the runner hasn't
	// it cached yet.
	NeedHive bool   `json:"need_hive,omitempty"`
	Error    string `json:"error,omitempty"` // the runner won't take the turn, and why
}

// Tree heads a tar stream for one mount.
type Tree struct {
	Mount int      `json:"mount"`
	Gone  []string `json:"gone,omitempty"`
}

// Started says the turn is running, or why it isn't.
type Started struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ExitCode is how the turn ended.
type ExitCode struct {
	Code   int    `json:"code"`
	Error  string `json:"error,omitempty"`
	Egress []byte `json:"egress,omitempty"` // the egress proxy's refusals, for the Hive to read as its own
}

// Mail is one mailbox file.
type Mail struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// MailSkip leaves a box's outbox and inbox out of its trees: they are relayed
// live as Mail instead.
func MailSkip(rel string) bool { return rel == "outbox" || rel == "inbox" }

var putMu sync.Mutex

// PutMail writes a mailbox file as the core does: a dot file, renamed into
// place, so a reader never sees half of it. Names that aren't a plain
// "<id>.json" are dropped.
func PutMail(dir string, m Mail) {
	if strings.ContainsAny(m.Name, `/\\`) || strings.HasPrefix(m.Name, ".") || !strings.HasSuffix(m.Name, ".json") {
		return
	}
	putMu.Lock()
	defer putMu.Unlock()
	tmp := filepath.Join(dir, "."+m.Name+".tmp")
	os.Remove(tmp)
	if err := os.WriteFile(tmp, m.Data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, filepath.Join(dir, m.Name))
}

// TakeMail calls send with each finished mailbox file in dir, removing it
// first; it stops at send's first error.
func TakeMail(dir string, send func(Mail) error) error {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) > MaxFrame/2 {
			continue
		}
		os.Remove(p)
		if err := send(Mail{Name: e.Name(), Data: raw}); err != nil {
			return err
		}
	}
	return nil
}

// NoTool starts a runner's refusal of a turn whose tools it can't find; the
// names follow, comma-separated, then " (" and the rest of the why.
const NoTool = "no such tool here: "
