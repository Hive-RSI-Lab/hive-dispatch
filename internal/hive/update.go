package hive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// SelfBinary is the dispatch binary the Hive keeps its runners on: the one
// running here. A test sets it.
var SelfBinary = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// UpdateRetry is how long a runner that didn't take an update is left
// before it is offered the same binary again.
const UpdateRetry = 10 * time.Minute

// Updated is the last update offered to a runner.
type Updated struct {
	SHA256 string    `json:"sha256"`
	At     time.Time `json:"at"`
	Error  string    `json:"error,omitempty"`
}

// LastUpdate reads the last update offered to a runner.
func (h Home) LastUpdate(name string) (Updated, bool) {
	var u Updated
	raw, err := os.ReadFile(h.path("status", name+".update.json"))
	if err != nil || json.Unmarshal(raw, &u) != nil {
		return u, false
	}
	return u, true
}

// behind is the Hive's binary to send r, whose status read st, when the
// runner serves another one and wasn't offered it within UpdateRetry. A
// runner that names no binary predates updates and can't take one.
func (h Home) behind(r Runner, st wire.Status) (string, wire.HiveBin, bool) {
	if st.Binary == "" {
		return "", wire.HiveBin{}, false
	}
	path, err := SelfBinary()
	if err != nil {
		return "", wire.HiveBin{}, false
	}
	own, err := wire.DescribeHive(path)
	if err != nil || own.SHA256 == st.Binary {
		return "", wire.HiveBin{}, false
	}
	if u, ok := h.LastUpdate(r.Name); ok && u.SHA256 == own.SHA256 && time.Since(u.At) < UpdateRetry {
		return "", wire.HiveBin{}, false
	}
	return path, own, true
}

// Update sends r the Hive's own dispatch binary at path, which r serves its
// sessions from after this one, and records how that went.
func (h Home) Update(r Runner, path string, bin wire.HiveBin) error {
	err := h.update(r, path, bin)
	u := Updated{SHA256: bin.SHA256, At: time.Now()}
	if err != nil {
		u.Error = err.Error()
	}
	raw, _ := json.Marshal(u)
	os.MkdirAll(h.path("status"), 0o700)
	writeAtomic(h.path("status", r.Name+".update.json"), raw)
	return err
}

func (h Home) update(r Runner, path string, bin wire.HiveBin) error {
	l, err := h.Dial(r)
	if err != nil {
		return err
	}
	defer l.Close()
	got := make(chan error, 1)
	go func() {
		if err := l.SendJSON(wire.JSON, wire.Hello{Version: wire.Version, Op: "update"}); err != nil {
			got <- err
			return
		}
		if err := l.SendJSON(wire.JSON, wire.Update{Bin: bin, Version: wire.Version}); err != nil {
			got <- err
			return
		}
		var ask wire.UpdateAsk
		if err := l.Expect(wire.JSON, &ask); err != nil {
			got <- err
			return
		}
		if ask.Error != "" {
			got <- errors.New(ask.Error)
			return
		}
		if ask.Need {
			if _, err := sendFile(l, path, bin.Size); err != nil {
				got <- err
				return
			}
		}
		var done wire.UpdateDone
		if err := l.Expect(wire.JSON, &done); err != nil {
			got <- err
			return
		}
		if done.Error != "" {
			got <- errors.New(done.Error)
			return
		}
		got <- nil
	}()
	select {
	case err = <-got:
	case <-time.After(5 * time.Minute):
		err = errors.New("no answer in 5m")
	}
	if err != nil {
		return fmt.Errorf("update %s: %w", r.Name, l.Why(err))
	}
	return nil
}
