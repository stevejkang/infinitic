package quota

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// Size is a plan's limits relative to Pro, per window: how much more usage one percent of each
// window stands for.
type Size struct{ Session, Weekly float64 }

// Sizes of the plans infinitic names. Ultra has no 5-hour limit; when the response still reports a
// 5-hour window, it is sized like the weekly one.
var sizes = map[string]Size{
	"Pro":           {1, 1},
	"Max":           {5, 5},
	"Ultra":         {10, 10},
	"Team Premium":  {6.25, 6.25},
	"Team Standard": {1.25, 1.25},
}

// PlanSize is the size of the named plan. Unknown plans count as the smallest one, which gives
// them the widest safety margin.
func PlanSize(plan string) Size {
	if s, ok := sizes[plan]; ok {
		return s
	}
	return Size{1, 1}
}

// Candidate is an account a switch may move to.
type Candidate struct {
	ID    string
	Size  Size
	Usage *Usage
}

// Params tune Rank. Spike is the extra usage right after a switch (the new account's prompt
// cache is cold), per window, in percent of a Pro account.
type Params struct {
	K       float64       // safety factor on the spike
	Tie     time.Duration // 7d resets this close count as simultaneous
	Horizon time.Duration // 7d quota resetting sooner is used first; beyond it, load is spread
	Spike   map[string]float64
}

// DefaultParams were chosen with select_sim_test.go and ten days of real watch logs.
var DefaultParams = Params{K: 1.5, Tie: time.Hour, Horizon: 72 * time.Hour,
	Spike: map[string]float64{SessionLabel: 15, WeeklyLabel: 3}}

var windows = [2]string{SessionLabel, WeeklyLabel}

type scored struct {
	c        Candidate
	size     [2]float64 // the plan's size, per window
	headroom [2]float64 // percent of this account, per window; +Inf for a window the plan lacks
	reset    [2]float64 // hours until reset, +Inf when unknown, -Inf for a window the plan lacks
	margin   float64    // smallest headroom / required spike margin over both windows; >= 1 fits
	capped   bool       // a narrower cap (e.g. a per-model weekly limit) is at the weekly bar
	group    int        // index of the cluster of simultaneous 7d resets
}

// Rank returns the accounts a switch may move to, best first: the quota that expires soonest is
// used first. Candidates must keep a margin below the bars for the post-switch spike; when none
// does, Rank falls back to every candidate below the bars, closest to having that margin first,
// and reports relaxed.
func Rank(cands []Candidate, bars Bars, p Params, now time.Time) (ranked []Candidate, relaxed bool) {
	var strict, loose []scored
	for _, c := range cands {
		s, ok := score(c, bars, p, now)
		if !ok || s.headroom[0] <= 0 || s.headroom[1] <= 0 {
			continue
		}
		loose = append(loose, s)
		if s.margin >= 1 {
			strict = append(strict, s)
		}
	}
	pick := strict
	if len(pick) == 0 && len(loose) > 0 {
		pick, relaxed = loose, true
		slices.SortStableFunc(pick, func(x, y scored) int {
			return cmp.Or(compareBool(x.capped, y.capped), cmp.Compare(y.margin, x.margin))
		})
	} else {
		order(pick, p)
	}
	for _, s := range pick {
		ranked = append(ranked, s.c)
	}
	return ranked, relaxed
}

func score(c Candidate, bars Bars, p Params, now time.Time) (scored, bool) {
	s := scored{c: c, margin: math.Inf(1)}
	s.size = [2]float64{max(c.Size.Session, 1), max(c.Size.Weekly, 1)} // guards hand-built candidates
	if !Known(c.Usage) {
		return s, false
	}
	for i, label := range windows {
		w := c.Usage.Window(label)
		if w == nil { // no 5-hour limit: never in the way, and no spike to keep room for
			s.headroom[i], s.reset[i] = math.Inf(1), math.Inf(-1)
			continue
		}
		pct, hours := w.Percent, math.Inf(1)
		if w.ResetsAt != nil {
			if d := w.ResetsAt.Sub(now); d > 0 {
				hours = d.Hours()
			} else {
				pct = 0 // the sample predates the reset
			}
		}
		s.headroom[i], s.reset[i] = bars.of(label)-pct, hours
		s.margin = min(s.margin, s.headroom[i]/(p.K*p.Spike[label]/s.size[i]))
	}
	for _, w := range c.Usage.Others() {
		s.capped = s.capped || w.Percent >= bars.Weekly
	}
	return s, true
}

// order uses the quota resetting within p.Horizon first: earliest 7d reset first (resets within
// p.Tie of a cluster's first one tie), then no full narrower cap, then the sooner 5h reset (no 5h
// limit first), then the most 7d quota. Beyond the horizon: started 7d windows before untouched ones (using one
// starts its clock), then no full narrower cap, then the most room in percent. Percent, not
// absolute room: equal percentages mean equal time to a bar whatever the plan, which keeps more
// accounts usable on a heavy day (absolute room was worse in select_sim_test.go).
func order(ss []scored, p Params) {
	slices.SortStableFunc(ss, func(x, y scored) int { return cmp.Compare(x.reset[1], y.reset[1]) })
	for i := range ss {
		switch {
		case i == 0:
		case ss[i].reset[1]-ss[firstOf(ss, i-1)].reset[1] <= p.Tie.Hours():
			ss[i].group = ss[i-1].group
		default:
			ss[i].group = ss[i-1].group + 1
		}
	}
	horizon := p.Horizon.Hours()
	slices.SortStableFunc(ss, func(x, y scored) int {
		xIn, yIn := x.reset[1] <= horizon, y.reset[1] <= horizon
		if c := compareBool(!xIn, !yIn); c != 0 || !xIn {
			return cmp.Or(c,
				compareBool(math.IsInf(x.reset[1], 1), math.IsInf(y.reset[1], 1)),
				compareBool(x.capped, y.capped),
				cmp.Compare(min(y.headroom[0], y.headroom[1]), min(x.headroom[0], x.headroom[1])))
		}
		return cmp.Or(
			cmp.Compare(x.group, y.group),
			compareBool(x.capped, y.capped),
			cmp.Compare(x.reset[0], y.reset[0]),
			cmp.Compare(y.headroom[1]*y.size[1], x.headroom[1]*x.size[1]),
		)
	})
}

// firstOf is the index of the first member of the cluster holding ss[i].
func firstOf(ss []scored, i int) int {
	for i > 0 && ss[i-1].group == ss[i].group {
		i--
	}
	return i
}

func compareBool(x, y bool) int {
	switch {
	case x == y:
		return 0
	case x:
		return 1
	default:
		return -1
	}
}

func (b Bars) of(label string) float64 {
	if label == SessionLabel {
		return b.Session
	}
	return b.Weekly
}
