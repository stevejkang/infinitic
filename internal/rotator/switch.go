package rotator

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
	"github.com/stevejkang/infinitic/internal/creds"
	"github.com/stevejkang/infinitic/internal/quota"
	"github.com/stevejkang/infinitic/internal/state"
)

type SwitchOptions struct {
	// ReplaceDead allows overwriting the dead primary grant it carries without parking it,
	// but only while the primary still holds exactly that grant.
	ReplaceDead *DeadPrimaryError
}

// TargetError means the switch target's parked grant is unusable. The primary was not touched.
type TargetError struct {
	Account *state.Account
	Err     error
}

func (e *TargetError) Error() string { return e.Account.Label() + ": " + e.Err.Error() }
func (e *TargetError) Unwrap() error { return e.Err }

// Switch moves target's grant into the primary slot and parks the grant it replaces.
func (r *Rotator) Switch(ctx context.Context, target *state.Account, opts SwitchOptions) error {
	targetOAuth, err := r.provenParked(ctx, target)
	if err != nil {
		return &TargetError{Account: target, Err: err}
	}
	from := "none"
	if previous := r.State.ByUUID(r.State.ActiveUUID); previous != nil {
		from = previous.Label()
	}
	if err := r.replacePrimary(ctx, target, targetOAuth, opts.ReplaceDead); err != nil {
		return err
	}
	r.State.ActiveUUID = target.UUID
	if err := r.Store.Delete(r.Store.Pool(target.UUID)); err != nil {
		r.Report.Warn("could not remove the parked copy of " + target.Label() + " (removed on the next check): " + err.Error())
	}
	if err := r.Identity.SetOAuthAccount(target.OAuthAccount); err != nil {
		r.Report.Warn("could not update ~/.claude.json identity: " + err.Error())
	}
	fields := append([]any{"event", "switch", "from", from, "to", target.Label()}, usageFields("to_", target.Usage)...)
	r.Report.Success(fmt.Sprintf("switched primary login -> %s", target.Label()), fields...)
	return nil
}

// SwitchToBest switches to the first candidate whose parked grant works, trying the next one
// when a target turns out unusable. It returns the new active account, or nil if none worked.
func (r *Rotator) SwitchToBest(ctx context.Context, bars quota.Bars, opts SwitchOptions) (*state.Account, error) {
	candidates, relaxed := r.Candidates(bars, time.Now())
	if relaxed {
		r.Report.Warn("No account has room for the post-switch spike. Taking the one closest to it.",
			"event", "candidate_margin_relaxed")
	}
	for _, a := range candidates {
		err := r.Switch(ctx, a, opts)
		var targetErr *TargetError
		if !errors.As(err, &targetErr) {
			return a, err
		}
		r.Report.Warn(fmt.Sprintf("Can't switch to %v. Trying the next account.", err), "event", "switch_target_unusable", "to", a.Label(), "error", targetErr.Err)
	}
	return nil, nil
}

// provenParked returns target's parked grant after proving it belongs to target and works.
func (r *Rotator) provenParked(ctx context.Context, a *state.Account) (creds.OAuth, error) {
	oauth, ok, err := r.readParked(ctx, a, false)
	if err != nil || !ok {
		r.record(a, ok, err)
		return creds.OAuth{}, cmp.Or(err, errNoParked)
	}
	id, err := r.API.Profile(ctx, oauth.AccessToken)
	if errors.Is(err, anthropic.ErrUnauthorized) {
		if oauth, _, err = r.readParked(ctx, a, true); err == nil {
			id, err = r.API.Profile(ctx, oauth.AccessToken)
		}
	}
	if err != nil {
		r.record(a, true, err)
		return creds.OAuth{}, err
	}
	noteGrant(a, oauth)
	if id.AccountUUID != a.UUID {
		a.NeedsReauth = "its parked credential belongs to " + id.Email
		return creds.OAuth{}, errors.New(a.NeedsReauth)
	}
	return oauth, nil
}

// replacePrimary parks the current primary grant (unless it is the dead one being replaced) and
// installs the target's. If the primary changes along the way it undoes the parking and returns
// ErrPrimaryChanged, so no grant ends up in two slots and no new login is overwritten.
func (r *Rotator) replacePrimary(ctx context.Context, target *state.Account, targetOAuth creds.OAuth, dead *DeadPrimaryError) error {
	current, ok, err := r.loadPrimary()
	if err != nil {
		return err
	}
	undo := func() {}
	switch {
	case dead != nil:
		if !ok || !current.SameGrant(dead.grant) {
			return ErrPrimaryChanged // someone logged in since the grant was found dead
		}
	case ok:
		// Never overwrite a login we cannot identify: that would destroy its grant.
		id, err := r.API.Profile(ctx, current.OAuth.AccessToken)
		if err != nil {
			return fmt.Errorf("cannot identify the current primary login, refusing to overwrite it: %w", err)
		}
		if id.AccountUUID == target.UUID {
			return nil
		}
		parked, _ := r.State.Upsert(id)
		if oa := r.Identity.OAuthAccountFor(parked.UUID); oa != nil {
			parked.OAuthAccount = oa
		}
		if err := r.Park(parked, current.OAuth); err != nil {
			return err
		}
		undo = func() { _ = r.Store.Delete(r.Store.Pool(parked.UUID)) }
	}

	latest, latestOK, err := r.loadPrimary()
	if err != nil || latestOK != ok || !latest.SameGrant(current) {
		undo()
		return cmp.Or(err, ErrPrimaryChanged)
	}
	if err := r.Store.Write(r.Store.Primary(), latest.WithOAuth(targetOAuth).String()); err != nil {
		undo()
		return err
	}
	return nil
}
