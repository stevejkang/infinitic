package quota

import (
	"slices"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// none as a reset means the endpoint reported no reset time.
const none = -1

// acct builds a candidate; resets are hours from now.
func acct(id string, mult, u5, reset5h, u7, reset7h float64) Candidate {
	at := func(h float64) *time.Time {
		if h == none {
			return nil
		}
		t := now.Add(time.Duration(h * float64(time.Hour)))
		return &t
	}
	return Candidate{ID: id, Size: Size{mult, mult}, Usage: &Usage{At: now, Windows: []Window{
		{Label: SessionLabel, Percent: u5, ResetsAt: at(reset5h)},
		{Label: WeeklyLabel, Percent: u7, ResetsAt: at(reset7h)},
	}}}
}

func withCap(c Candidate, pct float64) Candidate {
	c.Usage.Windows = append(c.Usage.Windows, Window{Label: "Fable", Percent: pct})
	return c
}

func ids(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func TestRankScenarios(t *testing.T) {
	h := func(d, hr, m float64) float64 { return d*24 + hr + m/60 }

	cases := []struct {
		name    string
		why     string
		spike   map[string]float64 // overrides DefaultParams.Spike
		cands   []Candidate
		want    []string
		relaxed bool
	}{
		{
			name: "golden: real pool, first pick",
			why:  "B's 37% expires in 20h; A has 43h; C is over the 5h bar",
			cands: []Candidate{
				acct("A", 5, 13, h(0, 4, 42), 83, h(1, 19, 0)),
				acct("B", 5, 6, h(0, 3, 42), 61, h(0, 19, 42)),
				acct("C", 5, 92, h(0, 0, 32), 25, h(5, 5, 0)),
			},
			want: []string{"B", "A"},
		},
		{
			name: "golden: after B is spent and C's 5h reset",
			why:  "A's 15% expires in 43h, C has 5 days: use A before C",
			cands: []Candidate{
				acct("A", 5, 13, h(0, 4, 42), 83, h(1, 19, 0)),
				acct("B", 5, 40, h(0, 3, 0), 98, h(0, 19, 0)),
				acct("C", 5, 0, none, 25, h(5, 5, 0)),
			},
			want: []string{"A", "C"},
		},
		{
			name:  "expiring quota beats bigger headroom",
			why:   "B's 48% disappears in 2h; A's 68% lasts 6 days",
			cands: []Candidate{acct("A", 5, 10, 3, 30, 6*24), acct("B", 5, 10, 3, 50, 2)},
			want:  []string{"B", "A"},
		},
		{
			name: "small plan needs a wider spike margin",
			why:  "a cache-miss spike is ~15 Pro-points of 5h: fatal to Pro at 80%, noise to a 20x account",
			cands: []Candidate{
				acct("pro", 1, 80, 3, 50, 48),
				acct("max20", 20, 80, 3, 50, 72),
			},
			want: []string{"max20"},
		},
		{
			name:    "only margin-failing accounts: relax rather than stay on an exhausted one",
			cands:   []Candidate{acct("pro", 1, 80, 3, 50, 48)},
			want:    []string{"pro"},
			relaxed: true,
		},
		{
			name:    "relaxed fallback lands where there is the most room, not the earliest deadline",
			why:     "both fail the margin; EDF would pick 'tight' (7d sooner) with the least 5h room left",
			cands:   []Candidate{acct("tight", 1, 88, 3, 50, 24), acct("roomy", 1, 80, 3, 50, 96)},
			want:    []string{"roomy", "tight"},
			relaxed: true,
		},
		{
			name:    "relaxed fallback weighs room by plan size",
			why:     "both fail the margin; Pro's 10% is under half of the 22.5% it needs, the 20x account's 1% is 89% of its 1.1%",
			cands:   []Candidate{acct("pro", 1, 80, 3, 50, 48), acct("max20", 20, 89, 3, 50, 48)},
			want:    []string{"max20", "pro"},
			relaxed: true,
		},
		{
			name:    "relaxed fallback puts a full per-model cap last",
			cands:   []Candidate{withCap(acct("capped", 5, 89, 3, 50, 48), 100), acct("free", 5, 89.5, 3, 50, 48)},
			want:    []string{"free", "capped"},
			relaxed: true,
		},
		{
			name: "beyond the horizon, room is compared in percent",
			why:  "60% free outlasts 50% free at the same pace whatever the plan; absolute room lost in the simulation",
			cands: []Candidate{
				acct("pro", 1, 30, 3, 38, 100),
				acct("max20", 20, 40, 3, 48, 110),
			},
			want: []string{"pro", "max20"},
		},
		{
			name: "a full per-model cap demotes an account among equals",
			why:  "same deadlines, but the user's model is blocked on 'capped'",
			cands: []Candidate{
				withCap(acct("capped", 5, 10, 3, 50, 48), 100),
				acct("free", 5, 10, 3, 50, 48),
			},
			want: []string{"free", "capped"},
		},
		{
			name:  "everyone over a bar: nobody",
			cands: []Candidate{acct("a", 5, 95, 1, 10, 48), acct("b", 5, 10, 1, 99, 48)},
			want:  nil,
		},
		{
			name: "unknown usage is skipped",
			cands: []Candidate{
				{ID: "blank", Size: Size{5, 5}, Usage: &Usage{Windows: []Window{{Label: SessionLabel, Percent: 0}}}},
				acct("ok", 5, 10, 3, 50, 48),
			},
			want: []string{"ok"},
		},
		{
			name:  "no 7d reset time sorts after known ones",
			cands: []Candidate{acct("noreset", 5, 10, 3, 10, none), acct("known", 5, 10, 3, 50, 100)},
			want:  []string{"known", "noreset"},
		},
		{
			name:  "a 7d reset that already passed counts as fresh",
			why:   "the cached 99% predates the reset",
			cands: []Candidate{acct("stale", 5, 10, 3, 99, -0.5)},
			want:  []string{"stale"},
		},
		{
			name: "7d resets 20 minutes apart are a tie; the sooner 5h reset wins",
			why:  "19h50m vs 20h10m is noise; B's 5h refills in 20m so its 5h quota is free to burn",
			cands: []Candidate{
				acct("A", 5, 50, 4, 50, h(0, 19, 50)),
				acct("B", 5, 50, h(0, 0, 20), 50, h(0, 20, 10)),
			},
			want: []string{"B", "A"},
		},
		{
			name: "a little quota left is still worth a switch when it would expire",
			why:  "the switch costs less than the quota that would expire (see the simulation)",
			cands: []Candidate{
				acct("A", 20, 87, 4, 50, 10),
				acct("C", 20, 10, 4, 50, 30),
			},
			want: []string{"A", "C"},
		},
		{
			name: "beyond the horizon nothing expires soon: spread to the most room",
			why:  "A resets in 4 days with 18% left, B in 5 days with 58% left: keep both alive for heavy days",
			cands: []Candidate{
				acct("A", 5, 10, 3, 80, 96),
				acct("B", 5, 10, 3, 40, 120),
			},
			want: []string{"B", "A"},
		},
		{
			name:  "a stricter margin from a larger spike",
			why:   "a 5h spike of 60 Pro-points needs 18% of a 5x account; 15% is not enough",
			spike: map[string]float64{SessionLabel: 60, WeeklyLabel: 3},
			cands: []Candidate{acct("A", 5, 85, 3, 50, 10), acct("B", 5, 70, 3, 50, 20)},
			want:  []string{"B"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultParams
			if tc.spike != nil {
				p.Spike = tc.spike
			}
			got, relaxed := Rank(tc.cands, DefaultBars, p, now)
			if !slices.Equal(ids(got), tc.want) || relaxed != tc.relaxed {
				t.Errorf("got %v relaxed=%v, want %v relaxed=%v (%s)", ids(got), relaxed, tc.want, tc.relaxed, tc.why)
			}
		})
	}
}

func TestPlanSize(t *testing.T) {
	for plan, want := range map[string]Size{"Pro": {1, 1}, "Max": {5, 5}, "Ultra": {10, 10}, "Team Premium": {6.25, 6.25},
		"Team Standard": {1.25, 1.25}, "Team": {1, 1}, "": {1, 1}, "Max 20x": {1, 1}} {
		if got := PlanSize(plan); got != want {
			t.Errorf("PlanSize(%q) = %v, want %v", plan, got, want)
		}
	}
}

func TestAnAccountWithoutASessionLimit(t *testing.T) {
	ultra := func(id string, u7, reset7h float64) Candidate {
		r := now.Add(time.Duration(reset7h * float64(time.Hour)))
		return Candidate{ID: id, Size: PlanSize("Ultra"), Usage: &Usage{At: now, Windows: []Window{{Label: WeeklyLabel, Percent: u7, ResetsAt: &r}}}}
	}
	// A spent 5h window does not apply to it, and it comes before a 5h reset in the same 7d cluster.
	ranked, relaxed := Rank([]Candidate{acct("max", 5, 10, 1, 50, 30), ultra("ultra", 50, 30.5)}, DefaultBars, DefaultParams, now)
	if got := ids(ranked); relaxed || !slices.Equal(got, []string{"ultra", "max"}) {
		t.Fatalf("order %v relaxed=%v", got, relaxed)
	}
	// Only its weekly window can put it over a bar.
	if over := DefaultBars.Over(ultra("u", 97, 10).Usage); over {
		t.Fatal("97% weekly is below the 98% bar")
	}
	if over := DefaultBars.Over(ultra("u", 98, 10).Usage); !over {
		t.Fatal("98% weekly is at the bar")
	}
	if ranked, _ := Rank([]Candidate{ultra("full", 98, 10)}, DefaultBars, DefaultParams, now); len(ranked) != 0 {
		t.Fatalf("an account at its weekly bar was ranked: %v", ids(ranked))
	}
	if got := DefaultBars.Describe(ultra("u", 12, 10).Usage); got != "5h none  7d  12%" {
		t.Fatalf("describe %q", got)
	}
}
