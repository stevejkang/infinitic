// Package state is infinitic's non-secret pool index (~/.config/infinitic/state.json).
// Tokens never live here; they live in the credential store.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
	"github.com/stevejkang/infinitic/internal/atomicfile"
	"github.com/stevejkang/infinitic/internal/quota"
)

type Account struct {
	UUID         string          `json:"uuid"`
	Email        string          `json:"email,omitempty"`
	Org          string          `json:"org,omitempty"`
	OAuthAccount json.RawMessage `json:"oauthAccount,omitempty"` // claude's ~/.claude.json identity, restored on switch
	Usage        *quota.Usage    `json:"usage,omitempty"`

	// NeedsReauth is set when the grant is dead (invalid_grant); the account is skipped until re-added.
	NeedsReauth string `json:"needsReauth,omitempty"`
	// LastError is a transient failure (network, 429); retried after RetryAt, cleared on success.
	LastError string `json:"lastError,omitempty"`
	// FailStreak counts consecutive transient failures; RetryAt is when the account may be
	// contacted again. Together they back off from an endpoint that is rate limiting us.
	FailStreak int        `json:"failStreak,omitempty"`
	RetryAt    *time.Time `json:"retryAt,omitempty"`
	// RateLimited marks LastError as an HTTP 429: routine throttling, backed off but not shown.
	RateLimited bool `json:"rateLimited,omitempty"`
	// Plan is the subscription, e.g. "Ultra" or "Team Premium".
	Plan string `json:"plan,omitempty"`
	// SeatTier is a team seat's tier as the profile reported it at the last login.
	SeatTier string `json:"seatTier,omitempty"`
	// LoginExpiresAt is when the account's login (its refresh token) expires, as the grant says.
	LoginExpiresAt *time.Time `json:"loginExpiresAt,omitempty"`
	// LoginWarnedAt is when watch last warned that the login is about to expire.
	LoginWarnedAt *time.Time `json:"loginWarnedAt,omitempty"`
}

// LoginWarning is how long before its login expires an account is flagged; Claude Code warns as
// early for its own login.
const LoginWarning = 3 * 24 * time.Hour

// LoginNotice says when the login expires once that is within LoginWarning, else "".
func (a *Account) LoginNotice(now time.Time) string {
	if a.LoginExpiresAt == nil {
		return ""
	}
	left := a.LoginExpiresAt.Sub(now)
	switch {
	case left > LoginWarning:
		return ""
	case left <= 0:
		return "login expired"
	case left <= 24*time.Hour:
		return "login expires within a day"
	default:
		return fmt.Sprintf("login expires in %d days", int((left+24*time.Hour-1)/(24*time.Hour)))
	}
}

// Label is how the account is shown to people.
func (a *Account) Label() string {
	if a.Email != "" {
		return a.Email
	}
	return a.UUID[:min(8, len(a.UUID))]
}

// BackingOff reports whether checks of the account are paused after failures.
func (a *Account) BackingOff(now time.Time) bool {
	return a.RetryAt != nil && now.Before(*a.RetryAt)
}

// Problem is the failure to show people: the last error, unless it is routine rate limiting.
func (a *Account) Problem() string {
	if a.RateLimited {
		return ""
	}
	return a.LastError
}

// Healthy reports whether the account may be switched to.
func (a *Account) Healthy() bool { return a.NeedsReauth == "" }

type State struct {
	Accounts   []*Account `json:"accounts"`
	ActiveUUID string     `json:"activeUuid,omitempty"`
	// ActiveGrant fingerprints the primary's access token when ActiveUUID was identified, so an
	// unchanged primary is not re-identified over the network on every check.
	ActiveGrant string `json:"activeGrant,omitempty"`

	path string
}

// Load reads the index, returning an empty one if it does not exist yet.
func Load(path string) (*State, error) {
	s := &State{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Save writes the index atomically.
func (s *State) Save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, data, 0o600)
}

func (s *State) ByUUID(uuid string) *Account {
	for _, a := range s.Accounts {
		if a.UUID == uuid {
			return a
		}
	}
	return nil
}

// NotFoundError is a selector that names no account exactly. Suggestion is the one account the
// selector starts (or, in any case, spells) when there is exactly one, else "".
type NotFoundError struct{ Selector, Suggestion string }

func (e *NotFoundError) Error() string {
	if e.Suggestion != "" {
		return fmt.Sprintf("no pooled account is %q (did you mean %s?)", e.Selector, e.Suggestion)
	}
	return fmt.Sprintf("no pooled account is %q", e.Selector)
}

// Resolve returns the account selector names exactly: its email or its UUID. A prefix or another
// case never selects an account to act on, since it can come to name a different one as the pool
// grows; the error suggests the account it would have matched.
func (s *State) Resolve(selector string) (*Account, error) {
	if selector == "" {
		return nil, errors.New("empty account selector")
	}
	var near []*Account
	sel := strings.ToLower(selector)
	for _, a := range s.Accounts {
		if a.Email == selector || a.UUID == selector {
			return a, nil
		}
		if strings.HasPrefix(strings.ToLower(a.UUID), sel) || strings.HasPrefix(strings.ToLower(a.Email), sel) {
			near = append(near, a)
		}
	}
	e := &NotFoundError{Selector: selector}
	if len(near) == 1 {
		e.Suggestion = near[0].Label()
	}
	return nil, e
}

// Upsert returns the account for id, adding it if new. The bool reports whether it was added.
func (s *State) Upsert(id anthropic.Identity) (*Account, bool) {
	if a := s.ByUUID(id.AccountUUID); a != nil {
		if a.Email == "" {
			a.Email = id.Email
		}
		if a.Plan == "" {
			a.Plan = id.Plan
		}
		if id.SeatTier != "" {
			a.SeatTier = id.SeatTier
		}
		return a, false
	}
	a := &Account{UUID: id.AccountUUID, Email: id.Email, Org: id.OrgUUID, Plan: id.Plan, SeatTier: id.SeatTier}
	s.Accounts = append(s.Accounts, a)
	return a, true
}

func (s *State) Remove(a *Account) {
	kept := s.Accounts[:0]
	for _, x := range s.Accounts {
		if x != a {
			kept = append(kept, x)
		}
	}
	s.Accounts = kept
	if s.ActiveUUID == a.UUID {
		s.ActiveUUID = ""
	}
}
