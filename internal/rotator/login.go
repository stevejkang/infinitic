package rotator

import (
	"errors"
	"fmt"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
	"github.com/stevejkang/infinitic/internal/creds"
	"github.com/stevejkang/infinitic/internal/state"
)

// LoginTarget is where StoreLogin put the grant.
type LoginTarget int

const (
	LoginAdded   LoginTarget = iota // new to the pool, parked
	LoginParked                     // replaced the account's parked grant
	LoginPrimary                    // replaced the grant in the primary login
)

// ErrPrimaryUnknown means the primary login could not be identified, so a login of what may be
// the active account cannot be stored safely.
var ErrPrimaryUnknown = errors.New("cannot tell which account the primary login holds")

// StoreLogin stores the grant of a fresh login of id where the account lives: in the primary
// login when it is the active account, keeping the slot's other entries, otherwise in its pool
// slot. primaryKnown reports whether SyncActive identified the primary login (a dead grant counts).
// Running it again for the same account replaces the grant again; nothing else changes.
func (r *Rotator) StoreLogin(id anthropic.Identity, o creds.OAuth, primaryKnown bool) (*state.Account, LoginTarget, error) {
	account, added := r.State.Upsert(id)
	target := LoginParked
	switch {
	case added:
		target = LoginAdded
	case account.UUID == r.State.ActiveUUID && !primaryKnown:
		return account, target, ErrPrimaryUnknown
	case account.UUID == r.State.ActiveUUID:
		target = LoginPrimary
	}
	if target == LoginPrimary {
		current, ok, err := r.loadPrimary()
		if err != nil {
			return account, target, err
		}
		if !ok {
			return account, target, ErrPrimaryChanged
		}
		if err := r.Store.Write(r.Store.Primary(), current.WithOAuth(o).String()); err != nil {
			return account, target, err
		}
		r.State.ActiveGrant = fingerprint(o.AccessToken)
	} else if err := r.Park(account, o); err != nil {
		if added {
			r.State.Remove(account) // locked() saves state even on error; a slot-less account must not survive
		}
		return account, target, err
	}
	account.SeatTier = id.SeatTier // a login is when the seat tier is read; it may have gone away
	noteGrant(account, o)
	account.NeedsReauth, account.LastError, account.FailStreak, account.RetryAt, account.RateLimited = "", "", 0, nil, false
	account.LoginWarnedAt = nil
	return account, target, nil
}

// warnExpiringLogins warns, at most once a day per account, about logins about to expire.
func (r *Rotator) warnExpiringLogins(now time.Time) {
	for _, a := range r.State.Accounts {
		notice := a.LoginNotice(now)
		if notice == "" || a.NeedsReauth != "" {
			a.LoginWarnedAt = nil
			continue
		}
		if a.LoginWarnedAt != nil && now.Sub(*a.LoginWarnedAt) < 24*time.Hour {
			continue
		}
		warned := now
		a.LoginWarnedAt = &warned
		r.Report.Warn(fmt.Sprintf("%s: %s (%s). Renew it with: infinitic reauth %s",
			a.Label(), notice, a.LoginExpiresAt.Local().Format("Jan 2 15:04"), a.Label()),
			"event", "login_expiring", "account", a.Label(), "expires_at", a.LoginExpiresAt.UTC().Format(time.RFC3339))
	}
}
