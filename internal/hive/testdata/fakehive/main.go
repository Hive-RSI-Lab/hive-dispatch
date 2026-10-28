// Command fakehive stands in for the hive binary in tests: `sandbox-probe`
// says ok, and `sandbox-exec --spec F` plays a scripted agent turn against
// the spec's mounts by their host paths, as a sandbox would show them inside.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type mount struct {
	Host, Inside string
	RW, Hide     bool
}

type spec struct {
	ID       string
	Env      []string
	Mounts   []mount
	Stdin    string
	Stdout   string
	StopFile string `json:"stop_file"`
	Local    []string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-probe" {
		fmt.Println("ok fake")
		return
	}
	if len(os.Args) != 4 || os.Args[1] != "sandbox-exec" {
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[3])
	if err != nil {
		os.Exit(125)
	}
	specAt := "spec-file: gone\n"
	if fi, err := os.Stat(os.Args[3]); err == nil {
		specAt = fmt.Sprintf("spec-file: %s %o\n", os.Args[3], fi.Mode().Perm())
	}
	os.Remove(os.Args[3])
	var s spec
	json.Unmarshal(raw, &s)
	at := map[string]string{}
	for _, m := range s.Mounts {
		if !m.Hide {
			at[m.Inside] = m.Host
		}
	}
	out, _ := os.OpenFile(s.Stdout, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	errf, _ := os.OpenFile(s.Stdout+".err", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	prompt, _ := os.ReadFile(s.Stdin)
	fmt.Fprintf(out, "got: %s\n", prompt)
	out.WriteString(specAt)
	for _, e := range s.Env {
		if strings.HasPrefix(e, "HIVE_ID=") {
			fmt.Fprintln(out, e)
		}
	}
	work, box := at["/work"], at["/hive/box"]
	if strings.Contains(string(prompt), "stop") {
		fmt.Fprintln(out, "waiting for stop")
		for {
			if _, err := os.Stat(s.StopFile); err == nil {
				os.Exit(143)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	f, _ := os.OpenFile(filepath.Join(work, "file.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("remote edit\n")
	f.Close()
	os.Remove(filepath.Join(work, "gone.txt"))
	os.MkdirAll(filepath.Join(work, "new", "dir"), 0o755)
	os.WriteFile(filepath.Join(work, "new", "dir", "x.txt"), []byte("x"), 0o644)
	kb, _ := os.ReadFile(filepath.Join(at["/hive/kb"], "note.md"))
	os.WriteFile(filepath.Join(work, "kb-seen.txt"), kb, 0o644)
	state, _ := os.ReadDir(at["/hive/state"])
	os.WriteFile(filepath.Join(work, "state-entries.txt"), []byte(fmt.Sprint(len(state))), 0o644)
	os.WriteFile(filepath.Join(at["/hive/home"], "session.jsonl"), []byte("session\n"), 0o644)
	if box == "" { // a check: no box, no mailbox
		fmt.Fprintln(out, "done")
		os.Exit(1)
	}
	os.WriteFile(filepath.Join(box, "notes.md"), []byte("notes from the turn\n"), 0o644)

	// A mailbox request, answered by the core.
	os.WriteFile(filepath.Join(box, "outbox", ".r1.tmp"), []byte(`{"id":"r1","op":"progress"}`), 0o644)
	os.Rename(filepath.Join(box, "outbox", ".r1.tmp"), filepath.Join(box, "outbox", "r1.json"))
	for i := 0; i < 200; i++ {
		if raw, err := os.ReadFile(filepath.Join(box, "inbox", "r1.json")); err == nil {
			os.WriteFile(filepath.Join(work, "reply.txt"), raw, 0o644)
			os.Remove(filepath.Join(box, "inbox", "r1.json"))
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintln(errf, "a line on stderr")
	os.WriteFile(s.Stdout+".egress", []byte("example.com:443 unlisted\n"), 0o600)
	fmt.Fprintln(out, "done")
	os.Exit(3)
}
