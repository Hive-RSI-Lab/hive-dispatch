package hive

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// Fresh is how long a status read stands for the runner's room.
const Fresh = time.Minute

// Probe reads a runner's status over a fresh link, records it, and returns it.
func (h Home) Probe(r Runner) Seen {
	s := Seen{At: time.Now()}
	st, err := h.status(r)
	if err != nil {
		s.Reach = err.Error()
	} else {
		s.Status = st
	}
	h.See(r.Name, s)
	return s
}

func (h Home) status(r Runner) (wire.Status, error) {
	var st wire.Status
	l, err := h.Dial(r)
	if err != nil {
		return st, err
	}
	defer l.Close()
	got := make(chan error, 1)
	go func() {
		if err := l.SendJSON(wire.JSON, wire.Hello{Version: wire.Version, Op: "status"}); err != nil {
			got <- err
			return
		}
		got <- l.Expect(wire.JSON, &st)
	}()
	select {
	case err = <-got:
	case <-time.After(30 * time.Second):
		err = errors.New("no answer in 30s")
	}
	if err != nil {
		return st, l.Why(err)
	}
	if st.Error != "" {
		return st, errors.New(st.Error)
	}
	return st, nil
}

// Room says why r has no room for a turn, or "".
func (r Runner) Room(s Seen) string {
	maxLoad, minFree := r.MaxLoad, r.MinFreeMB
	if maxLoad <= 0 {
		maxLoad = DefaultMaxLoad
	}
	if minFree <= 0 {
		minFree = DefaultMinFreeMB
	}
	switch {
	case r.Off:
		return "turned off"
	case s.Reach != "":
		return "unreachable: " + s.Reach
	case s.Error != "":
		return s.Error
	case !strings.HasPrefix(s.Sandbox, "ok"):
		return "no sandbox there: " + s.Sandbox
	case s.Max > 0 && s.Running >= s.Max:
		return fmt.Sprintf("full (%d of %d turns)", s.Running, s.Max)
	case s.Cores > 0 && s.Load1/float64(s.Cores) > maxLoad:
		return fmt.Sprintf("loaded (%.1f on %d cores)", s.Load1, s.Cores)
	case s.MemFreeMB < minFree:
		return fmt.Sprintf("low on memory (%d MB free)", s.MemFreeMB)
	}
	return ""
}

// Pick is the runner with the most room, reading any whose last status is
// stale; why says what each of the others lacked when none has room.
func (h Home) Pick() (Runner, string, error) { return h.PickFor(nil) }

// PickFor is Pick for a turn that needs the tools named: a runner that
// lacked one of them within LackFor is passed over without being asked, so
// its turns aren't sent across only to be refused there.
func (h Home) PickFor(tools []string) (Runner, string, error) {
	rs, err := h.Runners()
	if err != nil {
		return Runner{}, "", err
	}
	if len(rs) == 0 {
		return Runner{}, "no runners registered", nil
	}
	type cand struct {
		r    Runner
		load float64
		free int
	}
	var ok []cand
	var why []string
	for _, r := range rs {
		if lack := missing(h.Lacks(r.Name), tools); len(lack) > 0 && !r.Off {
			why = append(why, r.Name+": hasn't "+strings.Join(lack, ", "))
			continue
		}
		s, have := h.Seen(r.Name)
		if !have || time.Since(s.At) > Fresh {
			if r.Off {
				why = append(why, r.Name+": turned off")
				continue
			}
			s = h.Probe(r)
		}
		if w := r.Room(s); w != "" {
			why = append(why, r.Name+": "+w)
			continue
		}
		ok = append(ok, cand{r, s.Load1 / float64(max(1, s.Cores)), s.MemFreeMB})
	}
	if len(ok) == 0 {
		return Runner{}, strings.Join(why, "; "), nil
	}
	sort.SliceStable(ok, func(i, j int) bool {
		if ok[i].load != ok[j].load {
			return ok[i].load < ok[j].load
		}
		return ok[i].free > ok[j].free
	})
	return ok[0].r, "", nil
}

// missing are the tools in want that are in lacks.
func missing(lacks, want []string) []string {
	var out []string
	for _, t := range want {
		if slices.Contains(lacks, t) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}
