package hive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/runner"
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

// Room says why r has no room for one more turn, or "": the runner's room as
// s read it, with sent more turns the Hive handed it since. It holds the turn
// to what the runner will (runner.full): its own limits, as it reports them,
// where they're stricter than the Hive's, and every turn started within
// runner.Young counted as runner.YoungLoad of load and runner.YoungMB of
// memory, since the load average lags. A turn sent since s was read is one
// it doesn't show at all, so it counts as running and just started.
func (r Runner) Room(s Seen, sent int) string {
	maxLoad, minFree := r.MaxLoad, r.MinFreeMB
	if maxLoad <= 0 {
		maxLoad = DefaultMaxLoad
	}
	if minFree <= 0 {
		minFree = DefaultMinFreeMB
	}
	if s.MaxLoad > 0 && s.MaxLoad < maxLoad {
		maxLoad = s.MaxLoad
	}
	minFree = max(minFree, s.MinFreeMB)
	n, young := s.Running+sent, s.Young+sent
	load := s.Load1 + float64(young)*runner.YoungLoad
	free := s.MemFreeMB - young*runner.YoungMB
	switch {
	case r.Off:
		return "turned off"
	case s.Reach != "":
		return "unreachable: " + s.Reach
	case s.Error != "":
		return s.Error
	case !strings.HasPrefix(s.Sandbox, "ok"):
		return "no sandbox there: " + s.Sandbox
	case s.Max > 0 && n >= s.Max:
		return fmt.Sprintf("full (%d of %d turns%s)", n, s.Max, justSent(sent))
	case s.Cores > 0 && load/float64(s.Cores) > maxLoad:
		return fmt.Sprintf("loaded (%.1f on %d cores, %d turns running, %d just started%s)", load, s.Cores, n, young, justSent(sent))
	case free < minFree:
		return fmt.Sprintf("low on memory (%d MB free, %d turns running, %d just started%s)", free, n, young, justSent(sent))
	}
	return ""
}

func justSent(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d of them sent since its status", n)
}

// Pick is the runner with the most room, reading any whose last status is
// stale; why says what each of the others lacked when none has room.
func (h Home) Pick() (Runner, string, error) {
	l, _ := waitLock(h.path("claims.lock"))
	if l != nil {
		defer l.Close()
	}
	return h.pick(nil, "")
}

// PickFor is Pick for a turn that needs the tools named: a runner that
// lacked one of them within LackFor is passed over without being asked, so
// its turns aren't sent across only to be refused.
func (h Home) PickFor(tools []string) (Runner, string, error) {
	l, _ := waitLock(h.path("claims.lock"))
	if l != nil {
		defer l.Close()
	}
	return h.pick(tools, "")
}

// A Claim is the Hive's hold on a place for one turn on a runner, from when
// it is picked until the turn ends: while it is held, the runner's room
// counts the turn even before the runner's status shows it, so a burst of
// turns isn't all sent on one status that had room for one.
type Claim struct {
	Runner Runner
	f      *os.File
}

// Release gives the place back.
func (c *Claim) Release() {
	if c == nil || c.f == nil {
		return
	}
	os.Remove(c.f.Name())
	c.f.Close()
	c.f = nil
}

// Take picks a runner for a turn that needs the tools named, prefer when it
// has room (the one `room` named), and claims a place on it. With no runner
// with room it returns a nil claim and why.
func (h Home) Take(tools []string, prefer string) (*Claim, string, error) {
	l, _ := waitLock(h.path("claims.lock"))
	if l != nil {
		defer l.Close()
	}
	r, why, err := h.pick(tools, prefer)
	if err != nil || r.Name == "" {
		return nil, why, err
	}
	c := &Claim{Runner: r}
	dir := h.path("claims", r.Name)
	os.MkdirAll(dir, 0o700)
	if f, err := os.CreateTemp(dir, "turn-"); err == nil {
		f.Close()
		if c.f, err = tryLock(f.Name()); err != nil {
			os.Remove(f.Name())
		}
	}
	return c, "", nil
}

// Sent is sent for whoever isn't picking a runner (the board).
func (h Home) Sent(name string, since time.Time) int {
	l, _ := waitLock(h.path("claims.lock"))
	if l != nil {
		defer l.Close()
	}
	return h.sent(name, since)
}

// sent counts the turns this Hive has claimed places for on the runner
// named since since and that are still going, and clears the claims of any
// that ended without giving theirs back; the caller holds claims.lock.
func (h Home) sent(name string, since time.Time) int {
	n := 0
	files, _ := filepath.Glob(filepath.Join(h.path("claims", name), "turn-*"))
	for _, f := range files {
		l, err := tryLock(f)
		if err == nil {
			os.Remove(f)
			l.Close()
			continue
		}
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(since) {
			n++
		}
	}
	return n
}

// pick is the runner with the most room, prefer when it has room; the
// caller holds claims.lock.
func (h Home) pick(tools []string, prefer string) (Runner, string, error) {
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
		sent := h.sent(r.Name, s.At)
		if w := r.Room(s, sent); w != "" {
			why = append(why, r.Name+": "+w)
			continue
		}
		if r.Name == prefer {
			return r, "", nil
		}
		young := s.Young + sent
		ok = append(ok, cand{r, (s.Load1 + float64(young)*runner.YoungLoad) / float64(max(1, s.Cores)), s.MemFreeMB - young*runner.YoungMB})
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
