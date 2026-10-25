package hive

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// Local runs s in this machine's sandbox, as the core would have.
func (h Home) Local(s spec.Spec, why string) int {
	at := time.Now()
	p := Placed{At: at, Run: s.ID, Turn: filepath.Base(s.Stdout), Where: "local", Why: why}
	h.Record(p)
	hive := ""
	for _, t := range s.Tools {
		if strings.HasPrefix(filepath.Base(t), "hive") {
			hive = t
			break
		}
	}
	code := 125
	err := errors.New("no hive binary among the turn's tools")
	if hive != "" {
		code, err = sandboxExec(hive, s)
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

func sandboxExec(hive string, s spec.Spec) (int, error) {
	f, err := os.CreateTemp("", "hive-spec-*.json")
	if err != nil {
		return 125, err
	}
	specPath := f.Name()
	f.Close()
	if err := spec.Write(specPath, s); err != nil {
		os.Remove(specPath)
		return 125, err
	}
	cmd := exec.Command(hive, "sandbox-exec", "--spec", specPath)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Remove(specPath)
		return ee.ExitCode(), nil
	}
	if err != nil {
		os.Remove(specPath)
		return 125, err
	}
	os.Remove(specPath)
	return 0, nil
}
