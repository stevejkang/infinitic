// Package creds models the credential blob Claude Code stores: {"claudeAiOauth": {...}, ...}.
// Unknown fields are preserved verbatim so a round trip never drops data Claude Code relies on.
package creds

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

const oauthKey = "claudeAiOauth"

// OAuth is one Claude subscription grant.
type OAuth struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time

	extra map[string]json.RawMessage // scopes, subscriptionType, rateLimitTier, ...
}

// ExpiresWithin reports whether the access token is expired or expires within d of now.
func (o OAuth) ExpiresWithin(d time.Duration, now time.Time) bool {
	return !o.ExpiresAt.After(now.Add(d))
}

// Plan names the subscription from the grant and the seat tier the profile reported at login
// ("" when unknown): "Pro", "Max", "Ultra", "Team Premium" or "Team Standard". Any other plan is
// its subscription type as recorded. It is "" when the grant carries no subscription type.
func (o OAuth) Plan(seatTier string) string {
	var subscription, tier string
	_ = json.Unmarshal(o.extra["subscriptionType"], &subscription)
	_ = json.Unmarshal(o.extra["rateLimitTier"], &tier)
	switch {
	case seatTier == "team_tier_1":
		return "Team Premium"
	case seatTier == "team_standard":
		return "Team Standard"
	case subscription == "":
		return ""
	case subscription == "pro":
		return "Pro"
	case subscription == "ultra": // not seen yet: new Ultra subscriptions may be recorded this way
		return "Ultra"
	case subscription == "max" && strings.Contains(tier, "max_20x"): // Max 20x became Ultra
		return "Ultra"
	case subscription == "max":
		return "Max"
	case subscription == "team" && tier == "default_claude_max_5x":
		return "Team Premium"
	case subscription == "team" && tier == "default_raven": // only with a team subscription: org tiers use it too
		return "Team Standard"
	}
	return strings.ToUpper(subscription[:1]) + subscription[1:]
}

// WithTokens returns a copy carrying a freshly issued token pair.
func (o OAuth) WithTokens(access, refresh string, expiresAt time.Time) OAuth {
	o.AccessToken, o.RefreshToken, o.ExpiresAt = access, refresh, expiresAt
	return o
}

const loginExpiresKey = "refreshTokenExpiresAt"

// LoginExpiresAt is when the refresh token, and with it the login, expires. false when the grant
// does not say.
func (o OAuth) LoginExpiresAt() (time.Time, bool) {
	var ms float64
	if json.Unmarshal(o.extra[loginExpiresKey], &ms) != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(ms)), true
}

// WithLoginExpiry returns a copy whose login expires at t, in the field Claude Code reads.
func (o OAuth) WithLoginExpiry(t time.Time) OAuth {
	extra := maps.Clone(o.extra)
	if extra == nil {
		extra = map[string]json.RawMessage{}
	}
	extra[loginExpiresKey], _ = json.Marshal(t.UnixMilli())
	o.extra = extra
	return o
}

func (o OAuth) MarshalJSON() ([]byte, error) {
	fields := make(map[string]any, len(o.extra)+3)
	for k, v := range o.extra {
		fields[k] = v
	}
	fields["accessToken"] = o.AccessToken
	fields["refreshToken"] = o.RefreshToken
	fields["expiresAt"] = o.ExpiresAt.UnixMilli()
	return json.Marshal(fields)
}

func (o *OAuth) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var expiresAtMs float64
	if err := decodeField(fields, "accessToken", &o.AccessToken); err != nil {
		return err
	}
	if err := decodeField(fields, "refreshToken", &o.RefreshToken); err != nil {
		return err
	}
	if err := decodeField(fields, "expiresAt", &expiresAtMs); err != nil {
		return err
	}
	o.ExpiresAt = time.UnixMilli(int64(expiresAtMs))
	delete(fields, "accessToken")
	delete(fields, "refreshToken")
	delete(fields, "expiresAt")
	o.extra = fields
	return nil
}

func decodeField(fields map[string]json.RawMessage, key string, into any) error {
	raw, ok := fields[key]
	if !ok {
		return fmt.Errorf("credential has no %s", key)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("credential field %s: %w", key, err)
	}
	return nil
}

// Blob is the whole stored credential: the OAuth grant plus any sibling entries (e.g. mcpOAuth).
type Blob struct {
	OAuth OAuth
	rest  map[string]json.RawMessage
}

// New wraps a bare grant.
func New(o OAuth) Blob { return Blob{OAuth: o} }

// ErrNoGrant means the blob holds no Claude grant, e.g. after `claude /logout`, which keeps
// sibling entries such as mcpOAuth. Parse still returns those siblings alongside it.
var ErrNoGrant = errors.New("credential holds no Claude login")

// Parse reads a stored credential. It accepts the wrapped form and a bare OAuth object.
func Parse(raw string) (Blob, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return Blob{}, fmt.Errorf("credential is not JSON: %w", err)
	}
	inner, wrapped := fields[oauthKey]
	if !wrapped {
		if _, bare := fields["accessToken"]; !bare {
			return Blob{rest: fields}, ErrNoGrant
		}
		inner, fields = []byte(raw), nil
	}
	delete(fields, oauthKey)
	if string(inner) == "null" {
		return Blob{rest: fields}, ErrNoGrant
	}
	var o OAuth
	if err := json.Unmarshal(inner, &o); err != nil {
		return Blob{}, err
	}
	if o.AccessToken == "" || o.RefreshToken == "" {
		return Blob{rest: fields}, ErrNoGrant
	}
	return Blob{OAuth: o, rest: fields}, nil
}

// SameGrant reports whether two blobs carry the same token pair, ignoring sibling entries.
func (b Blob) SameGrant(other Blob) bool {
	return b.OAuth.AccessToken == other.OAuth.AccessToken && b.OAuth.RefreshToken == other.OAuth.RefreshToken
}

// WithOAuth returns a copy carrying a different grant, keeping the sibling entries.
func (b Blob) WithOAuth(o OAuth) Blob {
	b.OAuth = o
	return b
}

// String serializes the blob in Claude Code's wrapped form.
func (b Blob) String() string {
	fields := make(map[string]any, len(b.rest)+1)
	for k, v := range b.rest {
		fields[k] = v
	}
	fields[oauthKey] = b.OAuth
	out, err := json.Marshal(fields)
	if err != nil {
		panic(fmt.Sprintf("credential blob does not serialize: %v", err)) // only RawMessage and strings inside
	}
	return string(out)
}
