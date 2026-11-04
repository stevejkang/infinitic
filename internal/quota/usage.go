package quota

import "time"

// Labels of the two account-wide windows the bars govern. Every other window carries the label
// its provider gives it, such as the model a weekly cap is scoped to ("Fable").
const (
	SessionLabel = "5h"
	WeeklyLabel  = "7d"
)

// Usage is every usage window an account reported, the session and weekly windows first.
type Usage struct {
	Windows []Window  `json:"windows"`
	At      time.Time `json:"at"`
}

type Window struct {
	Label    string     `json:"label"`
	Percent  float64    `json:"percent"`
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
}

// Window returns the window with the given label, or nil.
func (u *Usage) Window(label string) *Window {
	if u == nil {
		return nil
	}
	for i := range u.Windows {
		if u.Windows[i].Label == label {
			return &u.Windows[i]
		}
	}
	return nil
}

// Others are the windows beyond the session and weekly ones.
func (u *Usage) Others() []Window {
	if u == nil {
		return nil
	}
	var out []Window
	for _, w := range u.Windows {
		if w.Label != SessionLabel && w.Label != WeeklyLabel {
			out = append(out, w)
		}
	}
	return out
}
