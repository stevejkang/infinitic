package quota

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// The simulator replays two weeks of a single user on a random pool. Work is measured in
// "units": one unit is 1% of a Pro 7d window. An account of multiplier m holds 100m units per
// 7d window and 100m/ratio per 5h window, where ratio is the unpublished 7d:5h capacity ratio,
// so a unit costs 1/m percent of 7d and ratio/m percent of 5h. A 5h window opens at the first
// use after it lapsed; 7d windows reset on a fixed weekly schedule. Every switch costs a
// cache-miss spike.

const simSpike = 2.0 // units burned right after a switch

type simAcct struct {
	id     string
	m      float64
	ratio  float64
	u5, u7 float64
	r5     *time.Time
	r7     time.Time
}

func (a *simAcct) candidate() Candidate {
	r7 := a.r7
	return Candidate{ID: a.id, Size: Size{a.m, a.m}, Usage: &Usage{Windows: []Window{
		{Label: SessionLabel, Percent: a.u5, ResetsAt: a.r5},
		{Label: WeeklyLabel, Percent: a.u7, ResetsAt: &r7},
	}}}
}

// consume burns up to x units, stopping at the hard 100% caps; it returns what was burned.
func (a *simAcct) consume(x float64, now time.Time) float64 {
	got := min(x, (100-a.u5)*a.m/a.ratio, (100-a.u7)*a.m)
	if got <= 0 {
		return 0
	}
	if a.r5 == nil {
		r := now.Add(5 * time.Hour)
		a.r5 = &r
	}
	a.u5 += got * a.ratio / a.m
	a.u7 += got / a.m
	return got
}

type simResult struct {
	served, unmet, expired, spikes float64
	switches                       int
}

type selector func(cands []Candidate, now time.Time) []Candidate

func legacy(bars Bars) selector {
	return func(cands []Candidate, _ time.Time) []Candidate {
		var out []Candidate
		for _, c := range cands {
			if Known(c.Usage) && !bars.Over(c.Usage) {
				out = append(out, c)
			}
		}
		headroom := func(u *Usage) float64 {
			return min(bars.Session-u.Window(SessionLabel).Percent, bars.Weekly-u.Window(WeeklyLabel).Percent)
		}
		slices.SortStableFunc(out, func(x, y Candidate) int { return cmp.Compare(headroom(y.Usage), headroom(x.Usage)) })
		return out
	}
}

func ranked(bars Bars, p Params) selector {
	return func(cands []Candidate, now time.Time) []Candidate {
		out, _ := Rank(cands, bars, p, now)
		return out
	}
}

type simPool struct {
	accts  []*simAcct
	demand float64   // mean units per working hour
	daily  []float64 // per-day demand factor: real use is bursty (mean 1)
	start  time.Time
}

func newPool(seed uint64, ratio float64) simPool {
	rng := rand.New(rand.NewPCG(seed, 7))
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	plans := []float64{1, 5, 5, 20}
	p := simPool{start: start}
	capacity := 0.0
	for i := range 3 + rng.IntN(2) {
		m := plans[rng.IntN(len(plans))]
		capacity += 100 * m
		p.accts = append(p.accts, &simAcct{
			id: string(rune('a' + i)), m: m, ratio: ratio,
			u7: rng.Float64() * 60,
			r7: start.Add(time.Duration(rng.Float64() * float64(7*24*time.Hour))),
		})
	}
	// Demand between 70% and 150% of the pool's weekly capacity, spread over 12h workdays.
	p.demand = (0.7 + rng.Float64()*0.8) * capacity / 84
	for range 14 {
		p.daily = append(p.daily, rng.ExpFloat64()) // many light days, a few very heavy ones
	}
	return p
}

func (p simPool) clone() []*simAcct {
	out := make([]*simAcct, len(p.accts))
	for i, a := range p.accts {
		c := *a
		out[i] = &c
	}
	return out
}

func simulate(p simPool, pick selector, bars Bars) simResult {
	var res simResult
	accts := p.clone()
	active := accts[0]
	for t := p.start; t.Before(p.start.Add(14 * 24 * time.Hour)); t = t.Add(time.Minute) {
		for _, a := range accts {
			if a.r5 != nil && !t.Before(*a.r5) {
				a.u5, a.r5 = 0, nil
			}
			if !t.Before(a.r7) {
				res.expired += (100 - a.u7) * a.m
				a.u7, a.r7 = 0, a.r7.Add(7*24*time.Hour)
			}
		}
		if active.u5 >= bars.Session || active.u7 >= bars.Weekly {
			var cands []Candidate
			for _, a := range accts {
				if a != active {
					cands = append(cands, a.candidate())
				}
			}
			if next := pick(cands, t); len(next) > 0 {
				for _, a := range accts {
					if a.id == next[0].ID {
						active = a
					}
				}
				res.switches++
				res.spikes += active.consume(simSpike, t)
			}
		}
		if h := t.Hour(); h >= 9 && h < 21 {
			need := p.demand * p.daily[int(t.Sub(p.start).Hours())/24] / 60
			got := active.consume(need, t)
			res.served += got
			res.unmet += need - got
		}
	}
	return res
}

// The 5h:7d capacity ratio is not published, so the comparison runs at three of them: with 7d
// binding (5), 5h binding (20) and in between. Rank must cut expired quota and must not leave
// more demand unmet than legacy beyond noise.
func TestSimulateAgainstLegacy(t *testing.T) {
	bars := DefaultBars
	add := func(sum *simResult, r simResult) {
		sum.served += r.served
		sum.unmet += r.unmet
		sum.expired += r.expired
		sum.spikes += r.spikes
		sum.switches += r.switches
	}
	for _, ratio := range []float64{5, 10, 20} {
		exact := DefaultParams
		exact.Spike = map[string]float64{SessionLabel: simSpike * ratio, WeeklyLabel: simSpike}
		var old, cold, known simResult
		for seed := range uint64(300) {
			p := newPool(seed, ratio)
			add(&old, simulate(p, legacy(bars), bars))
			add(&cold, simulate(p, ranked(bars, DefaultParams), bars))
			add(&known, simulate(p, ranked(bars, exact), bars))
		}
		t.Logf("7d = %v x 5h", ratio)
		for _, row := range []struct {
			name string
			r    simResult
		}{{"legacy", old}, {"rank (default)", cold}, {"rank (exact spike)", known}} {
			t.Logf("  %-18s unmet %7.0f (%+.2f%%)  expired %7.0f (%+.2f%%)  switches %5d", row.name,
				row.r.unmet, 100*(row.r.unmet/old.unmet-1), row.r.expired, 100*(row.r.expired/old.expired-1), row.r.switches)
			if row.r.unmet > old.unmet*1.005 || row.r.expired > old.expired {
				t.Errorf("ratio %v: %s is worse than legacy", ratio, row.name)
			}
		}
	}
}
