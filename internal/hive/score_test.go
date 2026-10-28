package hive

import (
	"testing"
	"time"
)

func TestScoreCountsRunnerTurnsEndedOK(t *testing.T) {
	h := Home(t.TempDir())
	now := time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC)
	if n := h.Score(now); n != 0 {
		t.Fatalf("no records: score %d, want 0", n)
	}
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, p := range []Placed{
		{At: ago(time.Hour), Run: "r1", Turn: "t1", Where: "iiks1", Ended: true, Secs: 60},                      // counts
		{At: ago(2 * time.Hour), Run: "r2", Turn: "t1", Where: "iiks1", Ended: true, Exit: 1, Secs: 60},         // failed
		{At: ago(3 * time.Hour), Run: "r3", Turn: "t1", Where: "local", Ended: true, Secs: 60},                  // ran here
		{At: ago(25 * time.Hour), Run: "r4", Turn: "t1", Where: "iiks1", Ended: true, Secs: 60},                 // too old
		{At: ago(24*time.Hour + 30*time.Second), Run: "r5", Turn: "t1", Where: "iiks1", Ended: true, Secs: 600}, // ended inside the day
		{At: ago(time.Hour), Run: "r6", Turn: "t1", Where: "iiks1"},                                             // still running
		{At: ago(time.Hour), Run: "r7", Turn: "t1", Where: "iiks1"},                                             // started ...
		{At: ago(time.Hour), Run: "r7", Turn: "t1", Where: "iiks1", Ended: true, Secs: 30},                      // ... and ended ok: one turn
	} {
		if err := h.Record(p); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.Score(now); n != 3 {
		t.Fatalf("score %d, want 3", n)
	}
}
