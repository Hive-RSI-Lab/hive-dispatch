package hive

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/spec"
)

// Delegate is the turn the core hands this module instead of starting it in
// its own sandbox: `dispatch turn --spec <file>`, as the core runs
// `hive sandbox-exec --spec <file>`. It runs the turn on a runner with room,
// or here, in the Hive's own sandbox, when it can't go (no runner has room,
// the turn needs this machine, or the runner refused it before it started).
// Its exit code is the turn's.
func (h Home) Delegate(specPath, runner string) int {
	s, err := spec.Read(specPath)
	os.Remove(specPath) // it carries the turn's token
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatch:", err)
		return 125
	}
	if why := Dispatchable(s); why != "" {
		return h.Local(s, why)
	}
	var r Runner
	if runner != "" {
		r, err = h.Get(runner)
		if err != nil {
			return h.Local(s, err.Error())
		}
	} else {
		var why string
		if r, why, err = h.Pick(); err != nil || r.Name == "" {
			if err != nil {
				why = err.Error()
			}
			return h.Local(s, why)
		}
	}
	res := h.Send(r, s)
	if res.Err != nil && !res.Started {
		return h.Local(s, res.Err.Error())
	}
	h.Record(res.Placed)
	return res.Code
}

// Local runs s in this machine's sandbox, as the core would have: the core
// has handed the turn over and can't take it back, so a turn that can't go
// runs here, through the hive binary the core named among the turn's tools
// (`hive sandbox-exec --spec <file>`), and through nothing else. module.json
// declares it.
func (h Home) Local(s spec.Spec, why string) int {
	at := time.Now()
	p := Placed{At: at, Run: s.ID, Turn: filepath.Base(s.Stdout), Where: "local", Why: why}
	h.Record(p)
	code := 125
	hive, err := hiveTool(s)
	if err == nil {
		code, err = h.sandboxExec(hive, s)
	}
	if err != nil {
		if f, e := appendFile(s.Stdout + ".err"); e == nil {
			fmt.Fprintln(f, "dispatch: running it here:", err)
			f.Close()
		}
	}
	p.Exit, p.Ended, p.Secs = code, true, time.Since(at).Seconds()
	if err != nil {
		p.Error = err.Error()
	}
	h.Record(p)
	return code
}

// hiveTool is the hive binary among the turn's tools: an absolute path to an
// executable named hive (hive.exe on Windows).
func hiveTool(s spec.Spec) (string, error) {
	for _, t := range s.Tools {
		if b := filepath.Base(t); b != "hive" && b != "hive.exe" {
			continue
		}
		if !filepath.IsAbs(t) {
			return "", fmt.Errorf("the turn's hive binary %q isn't an absolute path", t)
		}
		if fi, err := os.Stat(t); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("the turn's hive binary %q isn't an executable file", t)
		}
		return t, nil
	}
	return "", errors.New("no hive binary among the turn's tools")
}

// sandboxExec runs `hive sandbox-exec` on s. The spec carries the turn's
// token, so it is written under the module's HOME (tmp/, 0700), as 0600,
// and removed when the turn ends (sandbox-exec removes it once read).
func (h Home) sandboxExec(hive string, s spec.Spec) (int, error) {
	dir := h.path("tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 125, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return 125, fmt.Errorf("making %s 0700 for the turn's spec: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "spec-*.json")
	if err != nil {
		return 125, err
	}
	specPath := f.Name()
	f.Close()
	defer os.Remove(specPath)
	if err := spec.Write(specPath, s); err != nil {
		return 125, err
	}
	cmd := exec.Command(hive, "sandbox-exec", "--spec", specPath)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 125, err
	}
	return 0, nil
}
