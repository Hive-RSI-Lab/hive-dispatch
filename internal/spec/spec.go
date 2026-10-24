// Package spec is the Hive's sandbox spec as it is written to disk for
// `hive sandbox-exec --spec`: the same JSON the core's internal/sandbox Spec
// marshals to. It is copied here, not imported, because the core's packages
// are internal; the fields keep the core's tags so a spec round-trips.
package spec

import (
	"encoding/json"
	"os"
)

// Mount exposes a host path inside the sandbox; a Hide mount covers Inside so
// Host can't be read there.
type Mount struct {
	Host   string `json:"host"`
	Inside string `json:"inside"`
	RW     bool   `json:"rw"`
	Hide   bool   `json:"hide,omitempty"`
}

// Spec is one sandboxed command.
type Spec struct {
	ID       string   `json:"id"`
	Argv     []string `json:"argv"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
	Mounts   []Mount  `json:"mounts"`
	Tools    []string `json:"tools"`
	Stdin    string   `json:"stdin"`
	Stdout   string   `json:"stdout"`
	StopFile string   `json:"stop_file"`
	Network  bool     `json:"network"`
	Allow    []string `json:"allow"`
	Local    []string `json:"local"`
	Guard    []string `json:"guard,omitempty"`
	UID      int      `json:"uid"`
	GID      int      `json:"gid"`
}

// Read loads a spec.
func Read(path string) (Spec, error) {
	var s Spec
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(raw, &s)
}

// Write saves a spec readable only by this user: it carries a short-lived token.
func Write(path string, s Spec) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
