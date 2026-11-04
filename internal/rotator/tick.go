package rotator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stevejkang/infinitic/internal/quota"
)

// Tick is one watch iteration: switch away from the active account when it is over a bar,
// or when its grant is dead.
func (r *Rotator) Tick(ctx context.Context, bars quota.Bars) error {
	var opts SwitchOptions
	active, err := r.SyncActive(ctx)
	var dead *DeadPrimaryError
	if errors.As(err, &dead) {
		opts.ReplaceDead = dead
		r.flagDeadActive(dead)
		r.CheckAllParked(ctx, true)
		return r.switchOrStay(ctx, bars, opts, "no healthy pooled account to move to")
	}
	if err != nil {
		return err
	}
	if active == nil {
		r.CheckAllParked(ctx, true)
		return r.switchOrStay(ctx, bars, opts, "no primary login and no healthy pooled account to install")
	}

	r.CheckActive(ctx, active, true)
	r.warnExpiringLogins(time.Now())
	account := active.Account
	line := account.Label() + " " + bars.Describe(account.Usage)
	over := bars.Over(account.Usage)
	source := "live"
	if account.LastError != "" || account.NeedsReauth != "" {
		source = "cached" // the read failed: these are the last stored figures
	}
	fields := append([]any{"event", "check", "account", account.Label(), "over_bar", over, "source", source}, usageFields("", account.Usage)...)
	if !over {
		r.Report.Info(line, fields...)
		return nil
	}
	r.Report.Warn(line+"  over its bar, looking for a fresher account", fields...)
	r.CheckAllParked(ctx, true)
	return r.switchOrStay(ctx, bars, opts, "no pooled account is below its bars - staying put")
}

// usageFields are log fields for every window's percent and the 5h/7d reset times, keys prefixed.
func usageFields(prefix string, u *quota.Usage) []any {
	if u == nil {
		return nil
	}
	fields := make([]any, 0, 2*len(u.Windows)+6)
	if quota.NoSessionLimit(u) {
		fields = append(fields, prefix+"usage."+quota.SessionLabel, "none") // the plan has no 5-hour limit
	}
	for _, w := range u.Windows {
		fields = append(fields, prefix+"usage."+strings.ReplaceAll(w.Label, " ", "_"), w.Percent)
	}
	for _, label := range []string{quota.SessionLabel, quota.WeeklyLabel} {
		if w := u.Window(label); w != nil && w.ResetsAt != nil {
			fields = append(fields, prefix+"reset."+label, w.ResetsAt.UTC().Format(time.RFC3339))
		}
	}
	return fields
}

func (r *Rotator) flagDeadActive(dead *DeadPrimaryError) {
	if a := r.State.ByUUID(r.State.ActiveUUID); a != nil {
		a.NeedsReauth = dead.Error()
		r.Report.Warn(fmt.Sprintf("The active login %s is dead (%v). Moving to a healthy account.", a.Label(), dead),
			"event", "active_dead", "account", a.Label(), "error", dead)
		return
	}
	r.Report.Warn(fmt.Sprintf("The active login is dead (%v). Moving to a healthy account.", dead),
		"event", "active_dead", "error", dead)
}

func (r *Rotator) switchOrStay(ctx context.Context, bars quota.Bars, opts SwitchOptions, noneMsg string) error {
	switched, err := r.SwitchToBest(ctx, bars, opts)
	if err == nil && switched == nil {
		skipped := 0
		for _, a := range r.State.Accounts {
			if a.UUID != r.State.ActiveUUID && a.Healthy() && a.LastError != "" {
				skipped++
			}
		}
		if skipped > 0 {
			noneMsg += fmt.Sprintf(" (%d skipped: their last check failed or is rate limited)", skipped)
		}
		r.Report.Warn(noneMsg, "event", "no_candidate", "skipped", skipped)
	}
	return err
}
