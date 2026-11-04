// Package rotator moves Claude subscription grants between the primary login and the pool.
//
// Grant rule: a grant lives in exactly one slot. The active grant lives in the primary slot,
// where claude and other tools using its login refresh it; a parked grant lives only in its pool slot,
// where only infinitic refreshes it. A switch moves grants, it never copies them: refresh tokens
// are single-use, so a duplicated grant breaks as soon as either copy is refreshed.
//
// A refresh is the one step that cannot be undone: once it succeeds, the old pair is revoked and
// the new pair is the account's only grant. Every refresh result is therefore stored somewhere,
// even when the slot it came from changed meanwhile.
package rotator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
	"github.com/stevejkang/infinitic/internal/creds"
	"github.com/stevejkang/infinitic/internal/credstore"
	"github.com/stevejkang/infinitic/internal/quota"
	"github.com/stevejkang/infinitic/internal/state"
)

const (
	refreshAhead = 2 * time.Minute // refresh access tokens this close to expiry
	usageTTL     = 5 * time.Minute // reuse a usage sample this fresh, except right before a switch
)

// ErrPrimaryChanged means someone rewrote the primary login mid-switch; the next check retries.
var ErrPrimaryChanged = errors.New("the primary login changed during the switch - retry")

var errNoParked = errors.New("no parked credential; renew it with infinitic reauth")

// DeadPrimaryError means the primary login's grant was rejected (invalid_grant).
// It wraps anthropic.ErrDeadGrant.
type DeadPrimaryError struct {
	grant creds.Blob
	err   error
}

func (e *DeadPrimaryError) Error() string { return e.err.Error() }
func (e *DeadPrimaryError) Unwrap() error { return e.err }

// Reporter receives progress messages; fields are slog-style key/value pairs.
type Reporter interface {
	Info(msg string, fields ...any)
	Warn(msg string, fields ...any)
	Success(msg string, fields ...any)
}

type Rotator struct {
	Store    credstore.Store
	API      *anthropic.Client
	State    *state.State
	Identity IdentityFile
	Report   Reporter
}

// Active is the account currently in the primary slot.
type Active struct {
	Account *state.Account
	OAuth   creds.OAuth
}

// SyncActive identifies the primary login and registers it if it is new.
// It returns (nil, nil) when there is no primary login, and a *DeadPrimaryError when the
// primary's grant is dead.
func (r *Rotator) SyncActive(ctx context.Context) (*Active, error) {
	blob, ok, err := r.readPrimary(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		r.State.ActiveUUID = ""
		return nil, nil
	}
	account, err := r.identifyPrimary(ctx, &blob)
	if err != nil || account == nil {
		return nil, err
	}
	noteGrant(account, blob.OAuth)
	if oa := r.Identity.OAuthAccountFor(account.UUID); oa != nil {
		account.OAuthAccount = oa
	}
	account.NeedsReauth = ""
	r.State.ActiveUUID = account.UUID
	r.State.ActiveGrant = fingerprint(blob.OAuth.AccessToken)
	r.dropStrayCopy(account)
	return &Active{Account: account, OAuth: blob.OAuth}, nil
}

// identifyPrimary returns the account the primary grant belongs to. The answer is reused while
// the access token is unchanged, so watch does not call the profile endpoint on every check.
func (r *Rotator) identifyPrimary(ctx context.Context, blob *creds.Blob) (*state.Account, error) {
	if r.State.ActiveGrant == fingerprint(blob.OAuth.AccessToken) {
		if a := r.State.ByUUID(r.State.ActiveUUID); a != nil {
			return a, nil
		}
	}
	id, err := r.API.Profile(ctx, blob.OAuth.AccessToken)
	if errors.Is(err, anthropic.ErrUnauthorized) {
		// Revoked before its recorded expiry: refreshing is the only way to tell dead from stale.
		fresh, ok, err := r.refreshPrimary(ctx, *blob)
		if err != nil || !ok {
			return nil, err
		}
		*blob = fresh
		id, err = r.API.Profile(ctx, blob.OAuth.AccessToken)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	account, added := r.State.Upsert(id)
	if added {
		r.Report.Info("registered the current login " + account.Label())
	}
	return account, nil
}

// fingerprint identifies a token without storing it.
func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// noteGrant records what a grant says about its account: the plan and when the login expires.
func noteGrant(a *state.Account, o creds.OAuth) {
	if plan := o.Plan(a.SeatTier); plan != "" {
		a.Plan = plan
	}
	if t, ok := o.LoginExpiresAt(); ok {
		a.LoginExpiresAt = &t
	}
}

// loadPrimary reads the primary blob. A logged-out blob reads as absent but keeps its sibling
// entries, so installing a grant into it does not drop them.
func (r *Rotator) loadPrimary() (creds.Blob, bool, error) {
	slot := r.Store.Primary()
	raw, ok, err := r.Store.Read(slot)
	if err != nil || !ok {
		return creds.Blob{}, false, err
	}
	blob, err := creds.Parse(raw)
	if errors.Is(err, creds.ErrNoGrant) {
		return blob, false, nil
	}
	if err != nil {
		return creds.Blob{}, false, fmt.Errorf("%s: %w", slot.Label, err)
	}
	return blob, true, nil
}

// readPrimary reads the primary blob, refreshing it in place when its token is about to expire.
func (r *Rotator) readPrimary(ctx context.Context) (creds.Blob, bool, error) {
	blob, ok, err := r.loadPrimary()
	if err != nil || !ok || !blob.OAuth.ExpiresWithin(refreshAhead, time.Now()) {
		return blob, ok, err
	}
	return r.refreshPrimary(ctx, blob)
}

// refreshPrimary refreshes the primary grant read as seen. claude and other tools using its login refresh
// the same slot, so the outcome is reconciled with whatever the slot holds afterwards.
func (r *Rotator) refreshPrimary(ctx context.Context, seen creds.Blob) (creds.Blob, bool, error) {
	fresh, refreshErr := r.API.Refresh(ctx, seen.OAuth)
	current, ok, err := r.loadPrimary()
	if err != nil {
		if refreshErr == nil {
			r.parkOrphan(ctx, fresh)
		}
		return creds.Blob{}, false, err
	}
	replaced := !ok || !current.SameGrant(seen)

	switch {
	case refreshErr == nil && replaced:
		// Someone installed another grant meanwhile (e.g. /login). Theirs stays; ours is parked.
		r.parkOrphan(ctx, fresh)
		return current, ok, nil
	case refreshErr == nil:
		// Keep sibling entries written meanwhile; only the token pair is ours to change.
		updated := current.WithOAuth(fresh)
		if err := r.Store.Write(r.Store.Primary(), updated.String()); err != nil {
			r.parkOrphan(ctx, fresh)
			return creds.Blob{}, false, err
		}
		return updated, true, nil
	case replaced:
		// claude or the plugin rotated the token first; that is why ours was rejected.
		return current, ok, nil
	case errors.Is(refreshErr, anthropic.ErrDeadGrant):
		return creds.Blob{}, false, &DeadPrimaryError{grant: seen, err: refreshErr}
	default:
		return creds.Blob{}, false, refreshErr
	}
}

// parkOrphan stores a freshly refreshed grant whose slot was taken meanwhile, so it is not lost.
func (r *Rotator) parkOrphan(ctx context.Context, o creds.OAuth) {
	id, err := r.API.Profile(ctx, o.AccessToken)
	if err == nil {
		account, _ := r.State.Upsert(id)
		if err = r.Park(account, o); err == nil {
			r.Report.Warn("The primary login changed during a token refresh. Parked the refreshed grant of " + account.Label() + ".")
			return
		}
	}
	r.Report.Warn("a refreshed grant could not be stored and is lost (" + err.Error() + "); its account may need `infinitic reauth`")
}

// dropStrayCopy removes a pool copy of the active account. One is left behind when a switch is
// interrupted after installing the grant; the primary is the authoritative copy.
func (r *Rotator) dropStrayCopy(a *state.Account) {
	slot := r.Store.Pool(a.UUID)
	if _, ok, err := r.Store.Read(slot); err != nil || !ok {
		return
	}
	if err := r.Store.Delete(slot); err == nil {
		r.Report.Info("removed a stale parked copy of the active login " + a.Label())
	}
}

// Park stores a grant in a's pool slot.
func (r *Rotator) Park(a *state.Account, o creds.OAuth) error {
	return r.Store.Write(r.Store.Pool(a.UUID), creds.New(o).String())
}

// Forget deletes a's parked grant and drops the account. A primary login is left alone.
func (r *Rotator) Forget(a *state.Account) error {
	if err := r.Store.Delete(r.Store.Pool(a.UUID)); err != nil {
		return err
	}
	r.State.Remove(a)
	return nil
}

// readParked returns a parked grant, refreshing it in its pool slot when needed (or when forced).
func (r *Rotator) readParked(ctx context.Context, a *state.Account, force bool) (creds.OAuth, bool, error) {
	slot := r.Store.Pool(a.UUID)
	raw, ok, err := r.Store.Read(slot)
	if err != nil || !ok {
		return creds.OAuth{}, false, err
	}
	blob, err := creds.Parse(raw)
	if errors.Is(err, creds.ErrNoGrant) {
		return creds.OAuth{}, false, nil
	}
	if err != nil {
		return creds.OAuth{}, false, fmt.Errorf("%s: %w", slot.Label, err)
	}
	if !force && !blob.OAuth.ExpiresWithin(refreshAhead, time.Now()) {
		return blob.OAuth, true, nil
	}
	fresh, err := r.API.Refresh(ctx, blob.OAuth)
	if err != nil {
		return creds.OAuth{}, false, err
	}
	if err := r.Store.Write(slot, blob.WithOAuth(fresh).String()); err != nil {
		return creds.OAuth{}, false, fmt.Errorf("storing the refreshed grant of %s failed, it may need `infinitic reauth`: %w", a.Label(), err)
	}
	return fresh, true, nil
}

// CheckParked refreshes (if needed) and samples a parked account, recording failures as
// dead (NeedsReauth) or transient (LastError).
func (r *Rotator) CheckParked(ctx context.Context, a *state.Account, force bool) {
	if a.BackingOff(time.Now()) {
		return
	}
	oauth, ok, err := r.readParked(ctx, a, false)
	if err == nil && ok {
		noteGrant(a, oauth)
		err = r.sample(ctx, a, oauth, force)
		if errors.Is(err, anthropic.ErrUnauthorized) {
			if oauth, ok, err = r.readParked(ctx, a, true); err == nil && ok {
				err = r.sample(ctx, a, oauth, true)
			}
		}
	}
	r.record(a, ok, err)
}

// CheckActive samples the active account's usage.
func (r *Rotator) CheckActive(ctx context.Context, active *Active, force bool) {
	if active.Account.BackingOff(time.Now()) {
		return
	}
	err := r.sample(ctx, active.Account, active.OAuth, force)
	if errors.Is(err, anthropic.ErrUnauthorized) {
		r.State.ActiveGrant = "" // re-identify next check: that refreshes the grant or finds it dead
	}
	r.record(active.Account, true, err)
}

func (r *Rotator) record(a *state.Account, ok bool, err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The check was stopped, not refused: the account is not degraded and must not back off.
	case errors.Is(err, anthropic.ErrDeadGrant):
		a.NeedsReauth = err.Error()
	case err != nil:
		a.FailStreak++
		retryAt := time.Now().Add(backoffFor(err, a.FailStreak))
		var limited *anthropic.RateLimitedError
		a.LastError, a.RetryAt, a.RateLimited = err.Error(), &retryAt, errors.As(err, &limited)
		r.Report.Warn(fmt.Sprintf("checking %s failed: %v (keeping cached figures, next try %s)", a.Label(), err, retryAt.Format("15:04")),
			"event", "check_failed", "account", a.Label(), "error", err, "fail_streak", a.FailStreak, "retry_at", retryAt)
	case !ok:
		a.NeedsReauth = "no parked credential"
	default:
		a.NeedsReauth, a.LastError, a.FailStreak, a.RetryAt, a.RateLimited = "", "", 0, nil, false
	}
}

func (r *Rotator) sample(ctx context.Context, a *state.Account, oauth creds.OAuth, force bool) error {
	if !force && quota.Known(a.Usage) && time.Since(a.Usage.At) < usageTTL {
		return nil
	}
	u, err := r.API.Usage(ctx, oauth.AccessToken)
	if err != nil {
		return err
	}
	a.Usage = &u
	return nil
}

// CheckAllParked samples every account except the active one.
func (r *Rotator) CheckAllParked(ctx context.Context, force bool) {
	for _, a := range r.State.Accounts {
		if a.UUID != r.State.ActiveUUID {
			r.CheckParked(ctx, a, force)
		}
	}
}

// Candidates are the parked accounts a switch may move to, best first (see Rank).
func (r *Rotator) Candidates(bars quota.Bars, now time.Time) (accounts []*state.Account, relaxed bool) {
	return Rank(r.State, bars, now)
}

// Rank orders the parked accounts a switch may move to, best first, by quota.Rank. Accounts
// whose last check failed, including rate-limited ones, are left out: their cached usage is not
// current. The prompts rank with this too, so "recommended" is what a switch would pick.
func Rank(st *state.State, bars quota.Bars, now time.Time) (accounts []*state.Account, relaxed bool) {
	byID := make(map[string]*state.Account)
	var cands []quota.Candidate
	for _, a := range st.Accounts {
		if a.UUID != st.ActiveUUID && a.Healthy() && a.LastError == "" {
			byID[a.UUID] = a
			cands = append(cands, quota.Candidate{ID: a.UUID, Size: quota.PlanSize(a.Plan), Usage: a.Usage})
		}
	}
	ranked, relaxed := quota.Rank(cands, bars, quota.DefaultParams, now)
	for _, c := range ranked {
		accounts = append(accounts, byID[c.ID])
	}
	return accounts, relaxed
}
