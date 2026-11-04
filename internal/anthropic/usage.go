package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stevejkang/infinitic/internal/quota"
)

// Kinds of the two account-wide entries in the `limits` array.
const (
	kindSession = "session"
	kindWeekly  = "weekly_all"
)

// The response carries a `limits` array beside older per-window keys. `limits` is the current
// shape and lists every cap, so its entries win; the window keys are the fallback for responses
// without them. Both are
// read leniently: an entry this code cannot parse is skipped instead of failing the whole read,
// because the server adds kinds and fields without notice.
type window struct {
	Utilization float64    `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

type limitEntry struct {
	Kind     string     `json:"kind"`
	Group    string     `json:"group"`
	Percent  *float64   `json:"percent"`
	ResetsAt *time.Time `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// legacyScoped are the per-window keys that predate `limits`, with their labels.
var legacyScoped = []struct{ key, label string }{
	{"seven_day_opus", "Opus"},
	{"seven_day_sonnet", "Sonnet"},
	{"seven_day_oauth_apps", "Apps"},
	{"seven_day_cowork", "Cowork"},
}

// Usage reads the account's caps without spending tokens.
func (c *Client) Usage(ctx context.Context, accessToken string) (quota.Usage, error) {
	var raw map[string]json.RawMessage
	if err := c.getJSON(ctx, "/api/oauth/usage?at_wall=1&skip_spend=1", accessToken, &raw); err != nil {
		return quota.Usage{}, fmt.Errorf("usage: %w", err)
	}
	u, err := parseUsage(raw)
	if err != nil {
		return quota.Usage{}, fmt.Errorf("usage: %w", err)
	}
	u.At = time.Now()
	return u, nil
}

// parseUsage maps the response onto our windows. Claude's kinds stay in this package: the two
// account-wide windows become the session and weekly windows, every other cap is labelled by the
// model it is scoped to, or by its kind when it has none. Only the weekly window is required: a
// plan without a 5-hour limit reports no session window, and the usage then has none.
func parseUsage(raw map[string]json.RawMessage) (quota.Usage, error) {
	limits := parseLimits(raw["limits"])
	seven, ok := windowOf(raw["seven_day"], limits, kindWeekly)
	if !ok {
		return quota.Usage{}, errors.New("response has no 7-day window")
	}
	var u quota.Usage
	if five, ok := windowOf(raw["five_hour"], limits, kindSession); ok {
		u.Windows = append(u.Windows, quota.Window{Label: quota.SessionLabel, Percent: five.Utilization, ResetsAt: five.ResetsAt})
	}
	u.Windows = append(u.Windows, quota.Window{Label: quota.WeeklyLabel, Percent: seven.Utilization, ResetsAt: seven.ResetsAt})
	for _, e := range limits {
		if w, ok := scopedWindow(e); ok {
			u.Windows = appendUnique(u.Windows, w)
		}
	}
	if len(limits) == 0 {
		for _, k := range legacyScoped {
			var w *window
			if json.Unmarshal(raw[k.key], &w) == nil && w != nil {
				u.Windows = appendUnique(u.Windows, quota.Window{Label: k.label, Percent: w.Utilization, ResetsAt: w.ResetsAt})
			}
		}
	}
	return u, nil
}

// parseLimits decodes each entry on its own; a `limits` that is not an array yields none.
func parseLimits(raw json.RawMessage) []limitEntry {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]limitEntry, 0, len(items))
	for _, item := range items {
		var e limitEntry
		if json.Unmarshal(item, &e) == nil && e.Kind != "" && e.Percent != nil {
			out = append(out, e)
		}
	}
	return out
}

// windowOf reads an account-wide window from its limits entry, or from the legacy key when the
// limits list has none. A null legacy key is no window.
func windowOf(raw json.RawMessage, limits []limitEntry, kind string) (window, bool) {
	for _, e := range limits {
		if e.Kind == kind && e.Scope == nil {
			return window{Utilization: *e.Percent, ResetsAt: e.ResetsAt}, true
		}
	}
	var w *window
	if json.Unmarshal(raw, &w) == nil && w != nil {
		return *w, true
	}
	return window{}, false
}

// scopedWindow turns a limits entry into a narrower window. The account-wide entries are already the
// two main windows, and a scope naming every model is the weekly window under another label.
func scopedWindow(e limitEntry) (quota.Window, bool) {
	label := e.Kind
	if e.Scope != nil && e.Scope.Model != nil && strings.TrimSpace(e.Scope.Model.DisplayName) != "" {
		label = strings.TrimSpace(e.Scope.Model.DisplayName)
	}
	if (e.Scope == nil && (e.Kind == kindSession || e.Kind == kindWeekly)) || isAllModels(label) || isMainLabel(label) {
		return quota.Window{}, false
	}
	return quota.Window{Label: label, Percent: min(max(*e.Percent, 0), 100), ResetsAt: e.ResetsAt}, true
}

// isMainLabel keeps a cap the server happens to name "5h" or "7d" from shadowing the main windows.
func isMainLabel(label string) bool {
	return label == quota.SessionLabel || label == quota.WeeklyLabel
}

func isAllModels(label string) bool {
	normalized := strings.Join(strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(label))), " ")
	return normalized == "all models"
}

// appendUnique keeps the first reading of each label.
func appendUnique(ws []quota.Window, w quota.Window) []quota.Window {
	for _, x := range ws {
		if strings.EqualFold(x.Label, w.Label) {
			return ws
		}
	}
	return append(ws, w)
}
