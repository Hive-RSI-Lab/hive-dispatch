package hive

import (
	"encoding/json"
	"os"
	"time"
)

// A strained Hive asks `room` and hands over turns faster than a runner's
// reading changes: the reading is cached for Fresh, and a turn shows in it
// only once it has started there (and in its load later still). Judged on
// that one reading, every turn of a burst found room, the runner refused all
// but its first few, and the rest ran here after all (10-29: 61 turns in
// seven minutes, with this machine's memory pressure at 92%). So the Hive
// keeps its own count of what it has handed each runner that the reading
// can't show yet, and judges room on the reading plus those.
//
// A hand is one of them: a `room` answer the core hasn't handed a turn on
// yet (no Turn, for AskFor), or a turn on its way there (not Started) or
// started after the reading.
type hand struct {
	Turn    string    `json:"turn,omitempty"`
	At      time.Time `json:"at"`
	Started time.Time `json:"started,omitempty"`
}

// How long a hand counts at most: a `room` answer the core never handed a
// turn on, a turn still going across (its files, the first time, can take
// a while), and a turn that started there (by then a reading shows it).
const (
	AskFor     = 30 * time.Second
	SendingFor = 15 * time.Minute
	StartedFor = Young + Fresh
)

// Young is how long the runner counts a turn that started there as a core
// and a GB of its own (the load average lags), as the runner does.
const (
	Young     = 2 * time.Minute
	YoungLoad = 1.0
	YoungMB   = 1024
)

type hands map[string][]hand // by runner

func (h Home) lockHands() (func(), hands) {
	os.MkdirAll(string(h), 0o700)
	l, err := waitLock(h.path("handed.lock"))
	hs := hands{}
	if raw, err := os.ReadFile(h.path("handed.json")); err == nil {
		json.Unmarshal(raw, &hs)
	}
	now := time.Now()
	for r, list := range hs {
		kept := list[:0]
		for _, x := range list {
			switch {
			case x.Turn == "" && now.Sub(x.At) > AskFor:
			case x.Turn != "" && x.Started.IsZero() && now.Sub(x.At) > SendingFor:
			case !x.Started.IsZero() && now.Sub(x.Started) > StartedFor:
			default:
				kept = append(kept, x)
			}
		}
		hs[r] = kept
		if len(kept) == 0 {
			delete(hs, r)
		}
	}
	return func() {
		if raw, err := json.Marshal(hs); err == nil {
			writeAtomic(h.path("handed.json"), raw)
		}
		if err == nil {
			l.Close()
		}
	}, hs
}

// pending is how many of runner's hands the reading s doesn't show.
func (hs hands) pending(runner string, s Seen) int {
	n := 0
	for _, x := range hs[runner] {
		if x.Started.IsZero() || x.Started.After(s.At) {
			n++
		}
	}
	return n
}

// asked says runner has a `room` answer no turn has taken yet.
func (hs hands) asked(runner string) bool {
	for _, x := range hs[runner] {
		if x.Turn == "" {
			return true
		}
	}
	return false
}

// hand records a `room` answer (turn "") or a turn going to runner; a turn
// takes the place of the oldest answer not yet taken.
func (hs hands) hand(runner, turn string) {
	if turn != "" {
		for i, x := range hs[runner] {
			if x.Turn == "" {
				hs[runner][i] = hand{Turn: turn, At: time.Now()}
				return
			}
		}
	}
	hs[runner] = append(hs[runner], hand{Turn: turn, At: time.Now()})
}

// Handed marks turn as started on runner (started) or drops it (it never
// started there, or it has ended).
func (h Home) Handed(runner, turn string, started bool) {
	done, hs := h.lockHands()
	defer done()
	list := hs[runner]
	for i, x := range list {
		if x.Turn != turn {
			continue
		}
		if started {
			list[i].Started = time.Now()
		} else {
			hs[runner] = append(list[:i], list[i+1:]...)
		}
		return
	}
}

// with is s as it will be with n more turns handed over: n more running,
// n more just started.
func with(s Seen, n int) Seen {
	s.Running += n
	s.Young += n
	return s
}
