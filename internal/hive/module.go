package hive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Add registers a runner. An ssh:// runner needs its host key, read on that
// machine (`hive-dispatch runner setup` prints it), which is pinned so the
// link only ever opens to that machine.
func (h Home) Add(r Runner, hostKey string) error {
	if !validName.MatchString(r.Name) {
		return fmt.Errorf("a runner's name is lower-case letters, digits, '.', '_' and '-' (got %q)", r.Name)
	}
	if _, err := h.Argv(r); err != nil {
		return err
	}
	ssh := strings.HasPrefix(r.Address, "ssh://")
	if ssh {
		f := strings.Fields(hostKey)
		if len(f) < 2 || !strings.HasPrefix(f[0], "ssh-") && !strings.HasPrefix(f[0], "ecdsa-") {
			return fmt.Errorf("an ssh runner needs --host-key \"<type> <key>\", the runner's host key (run `hive-dispatch runner setup` on it)")
		}
		if err := h.pin(r.Name, f[0]+" "+f[1]); err != nil {
			return err
		}
	}
	rs, err := h.Runners()
	if err != nil {
		return err
	}
	out := rs[:0]
	for _, x := range rs {
		if x.Name != r.Name {
			out = append(out, x)
		}
	}
	return h.Save(append(out, r))
}

// Remove unregisters a runner and unpins its host key.
func (h Home) Remove(name string) error {
	rs, err := h.Runners()
	if err != nil {
		return err
	}
	out := rs[:0]
	found := false
	for _, x := range rs {
		if x.Name == name {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		return fmt.Errorf("no runner %q", name)
	}
	h.pin(name, "")
	os.Remove(h.path("status", name+".json"))
	return h.Save(out)
}

// pin sets (or, with key "", drops) a runner's line in known_hosts.
func (h Home) pin(name, key string) error {
	raw, _ := os.ReadFile(h.KnownHosts())
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] != name {
			lines = append(lines, l)
		}
	}
	if key != "" {
		lines = append(lines, name+" "+key)
	}
	return writeAtomic(h.KnownHosts(), []byte(strings.Join(lines, "\n")+"\n"))
}

// PublicKey is the Hive's half of every link, made the first time.
func (h Home) PublicKey() (string, error) {
	if _, err := os.Stat(h.Key()); err != nil {
		os.MkdirAll(string(h), 0o700)
		out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "hive-dispatch", "-f", h.Key()).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("ssh-keygen: %v %s", err, out)
		}
	}
	raw, err := os.ReadFile(h.Key() + ".pub")
	return strings.TrimSpace(string(raw)), err
}

// Board is the runners and the latest turns, as one line each, for the
// dashboard and `dispatch list`.
func (h Home) Board(turns int) []string {
	rs, _ := h.Runners()
	var out []string
	for _, r := range rs {
		s, ok := h.Seen(r.Name)
		line := r.Name + " (" + r.Address + "): "
		switch {
		case !ok:
			line += "not read yet"
		case s.Reach != "":
			line += "unreachable: " + s.Reach
		default:
			line += fmt.Sprintf("%d/%d turns, load %.1f on %d cores, %d of %d MB free, sandbox %s", s.Running, s.Max, s.Load1, s.Cores, s.MemFreeMB, s.MemTotalMB, s.Sandbox)
			if w := r.Room(s); w != "" {
				line += "; no room: " + w
			}
		}
		if ok {
			line += " (read " + s.At.Format("15:04:05") + ")"
		}
		out = append(out, line)
	}
	ps := h.Turns()
	for i := max(0, len(ps)-turns); i < len(ps); i++ {
		out = append(out, ps[i].String())
	}
	return out
}

func (p Placed) String() string {
	s := p.At.Format("01-02 15:04") + " " + p.Run + " " + p.Turn + " on " + p.Where
	switch {
	case !p.Ended:
		s += ": started"
	default:
		s += fmt.Sprintf(": exit %d after %.0fs", p.Exit, p.Secs)
	}
	if p.Why != "" {
		s += " (here because " + p.Why + ")"
	}
	if p.Error != "" {
		s += " — " + clip(p.Error, 200)
	}
	return s
}

// Turns reads turns.jsonl, keeping only each turn's latest line.
func (h Home) Turns() []Placed {
	f, err := os.Open(h.path("turns.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []Placed
	at := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var p Placed
		if json.Unmarshal(sc.Bytes(), &p) != nil {
			continue
		}
		k := p.Run + "/" + p.Turn
		if i, ok := at[k]; ok {
			all[i] = p
			continue
		}
		at[k] = len(all)
		all = append(all, p)
	}
	return all
}

// Serve is the module's program under the Hive's module runner: it reads
// each runner's room every 20 seconds, so a turn's choice is quick, and
// reports the board and every placed turn to the Hive's journal
// (module.reported), where the dashboard and the record see them.
func (h Home) Serve(api, token string) error {
	report := func(body map[string]any) error {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", api+"/api/module/report", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("report: %s", resp.Status)
		}
		return nil
	}
	sent := h.reported()
	last := ""
	for misses := 0; misses < 10; time.Sleep(20 * time.Second) {
		rs, _ := h.Runners()
		for _, r := range rs {
			if !r.Off {
				h.Probe(r)
			}
		}
		board := strings.Join(h.Board(0), "\n")
		if board != last {
			if err := report(map[string]any{"kind": "board", "text": "dispatch runners:\n" + board}); err != nil {
				misses++ // the daemon is gone; the module runner starts this again when it is back
				continue
			}
			last = board
		}
		misses = 0
		ps := h.Turns()
		for _, p := range ps {
			k := p.Run + "/" + p.Turn + "/" + strconv.FormatBool(p.Ended)
			if sent[k] {
				continue
			}
			if report(map[string]any{"kind": "turn", "run": p.Run, "turn": p.Turn, "where": p.Where, "exit": p.Exit, "ended": p.Ended, "text": p.String()}) == nil {
				sent[k] = true
				h.markReported(k)
			}
		}
	}
	return fmt.Errorf("the Hive's API stopped answering")
}

func (h Home) reported() map[string]bool {
	raw, _ := os.ReadFile(h.path("reported"))
	m := map[string]bool{}
	for _, l := range strings.Fields(string(raw)) {
		m[l] = true
	}
	return m
}

func (h Home) markReported(k string) {
	f, err := os.OpenFile(h.path("reported"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintln(f, k)
		f.Close()
	}
}

// Score is the project's score: how many turns ran on a runner and ended
// with exit 0 in the day before now, by when they ended (At is when they
// started).
func (h Home) Score(now time.Time) int {
	n := 0
	for _, p := range h.Turns() {
		end := p.At.Add(time.Duration(p.Secs * float64(time.Second)))
		if p.Where != "local" && p.Where != "" && p.Ended && p.Exit == 0 &&
			!end.After(now) && now.Sub(end) <= 24*time.Hour {
			n++
		}
	}
	return n
}
