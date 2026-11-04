package quota

import (
	"fmt"
	"strings"
)

// Bars are the usage thresholds (percent) at which an account counts as exhausted.
type Bars struct {
	Session float64 // 5-hour window
	Weekly  float64 // 7-day window
}

var DefaultBars = Bars{Session: 90, Weekly: 98}

// Known reports whether the usage has been sampled. Only the weekly window is required: a plan
// without a 5-hour limit has no session window.
func Known(u *Usage) bool {
	return u.Window(WeeklyLabel) != nil
}

// NoSessionLimit reports whether sampled usage has no 5-hour window.
func NoSessionLimit(u *Usage) bool {
	return Known(u) && u.Window(SessionLabel) == nil
}

// Over reports whether usage reached either bar. Unknown usage is never "over".
func (b Bars) Over(u *Usage) bool {
	if !Known(u) {
		return false
	}
	if s := u.Window(SessionLabel); s != nil && s.Percent >= b.Session {
		return true
	}
	return u.Window(WeeklyLabel).Percent >= b.Weekly
}

func (b Bars) Describe(u *Usage) string {
	if u == nil || len(u.Windows) == 0 {
		return "5h ?  7d ?"
	}
	parts := make([]string, 0, len(u.Windows)+1)
	if NoSessionLimit(u) {
		parts = append(parts, SessionLabel+" none")
	}
	for _, w := range u.Windows {
		parts = append(parts, fmt.Sprintf("%s %3.0f%%", w.Label, w.Percent))
	}
	return strings.Join(parts, "  ")
}
