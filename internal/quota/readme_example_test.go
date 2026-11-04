package quota

import (
	"strings"
	"testing"
	"time"
)

// TestReadmeExample keeps the example table in README.md true to Rank.
func TestReadmeExample(t *testing.T) {
	now := time.Now()
	in := func(h float64) *time.Time { r := now.Add(time.Duration(h * float64(time.Hour))); return &r }
	acct := func(id string, fiveH float64, fiveReset *time.Time, sevenD float64, reset *time.Time) Candidate {
		return Candidate{ID: id, Size: PlanSize("Team Premium"), Usage: &Usage{At: now, Windows: []Window{
			{Label: SessionLabel, Percent: fiveH, ResetsAt: fiveReset},
			{Label: WeeklyLabel, Percent: sevenD, ResetsAt: reset},
		}}}
	}
	cands := []Candidate{
		acct("A", 20, in(0.5), 70, in(30)),
		acct("B", 10, in(4), 60, in(30.5)),
		acct("C", 30, in(2), 50, in(100)),
		acct("D", 0, nil, 0, nil),
		acct("E", 5, in(1), 97.5, in(10)),
	}
	ranked, relaxed := Rank(cands, DefaultBars, DefaultParams, now)
	var ids []string
	for _, c := range ranked {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, ""); got != "ABCD" || relaxed {
		t.Fatalf("order %s relaxed=%v, want ABCD (E skipped) as README says", got, relaxed)
	}
	// Alone, E still has room in L and is used, relaxed.
	ranked, relaxed = Rank([]Candidate{acct("E", 5, in(1), 97.5, in(10))}, DefaultBars, DefaultParams, now)
	if len(ranked) != 1 || !relaxed {
		t.Fatalf("E alone: %v relaxed=%v, want E with the margin relaxed", ranked, relaxed)
	}
}
