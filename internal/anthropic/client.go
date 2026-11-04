// Package anthropic talks to the three Claude subscription endpoints infinitic needs:
// token refresh, identity (profile), and the no-spend usage read.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stevejkang/infinitic/internal/creds"
)

// Claude Code's public OAuth client id.
const clientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// ErrDeadGrant means the refresh token was rejected (invalid_grant): the account must log in again.
// Any other refresh error is transient and worth retrying.
var ErrDeadGrant = errors.New("refresh token rejected (invalid_grant)")

// ErrUnauthorized means the API rejected an access token (HTTP 401) before its recorded expiry,
// e.g. after a refresh elsewhere revoked it. A refresh may still revive the grant.
var ErrUnauthorized = errors.New("access token rejected (HTTP 401)")

// RateLimitedError is an HTTP 429. RetryAfter is the server's hint, or a default when it sent none.
// Endpoint is empty when the caller already names it.
type RateLimitedError struct {
	Endpoint   string
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	msg := fmt.Sprintf("rate limited (HTTP 429, retry after %s)", e.RetryAfter)
	if e.Endpoint == "" {
		return msg
	}
	return e.Endpoint + ": " + msg
}

const defaultRetryAfter = time.Minute

func rateLimited(endpoint string, h http.Header) *RateLimitedError {
	return &RateLimitedError{Endpoint: endpoint, RetryAfter: parseRetryAfter(h.Get("Retry-After"), time.Now())}
}

// parseRetryAfter reads delta-seconds or an HTTP-date (RFC 9110).
func parseRetryAfter(v string, now time.Time) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return defaultRetryAfter
}

const (
	defaultTokenLifetime = time.Hour
	refreshTimeout       = 30 * time.Second
)

type Client struct {
	APIBase   string // https://api.anthropic.com
	TokenURL  string // https://claude.ai/v1/oauth/token
	UserAgent func() string
	HTTP      *http.Client
}

type Identity struct {
	AccountUUID string
	Email       string
	OrgUUID     string
	Plan        string // from the organization type, e.g. "Max"; "" when unknown
	SeatTier    string // a team seat's tier, e.g. "team_tier_1"; "" for individual plans
}

var orgPlans = map[string]string{
	"claude_max":        "Max",
	"claude_pro":        "Pro",
	"claude_team":       "Team",
	"claude_enterprise": "Enterprise",
}

// Refresh exchanges the refresh token for a new token pair. The request is not cancelled with ctx:
// once sent, the server may already have rotated the pair, and abandoning the response would lose
// the only valid grant.
func (c *Client) Refresh(ctx context.Context, o creds.OAuth) (creds.OAuth, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {o.RefreshToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return creds.OAuth{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.UserAgent())

	status, header, body, err := c.do(req)
	if err != nil {
		return creds.OAuth{}, fmt.Errorf("token refresh: %w", err)
	}
	if status == http.StatusTooManyRequests {
		return creds.OAuth{}, rateLimited("token refresh", header)
	}
	if status != http.StatusOK {
		code, detail := oauthErrorDetail(body)
		msg := fmt.Sprintf("token refresh: HTTP %d %s", status, detail)
		if (status == http.StatusBadRequest || status == http.StatusUnauthorized) && code == "invalid_grant" {
			return creds.OAuth{}, fmt.Errorf("%w: %s", ErrDeadGrant, msg)
		}
		return creds.OAuth{}, errors.New(msg)
	}
	var t struct {
		AccessToken      string      `json:"access_token"`
		RefreshToken     string      `json:"refresh_token"`
		ExpiresIn        int64       `json:"expires_in"`
		RefreshExpiresIn json.Number `json:"refresh_token_expires_in"`
	}
	if err := json.Unmarshal([]byte(body), &t); err != nil || t.AccessToken == "" {
		return creds.OAuth{}, fmt.Errorf("token refresh: unreadable response")
	}
	if t.RefreshToken == "" {
		t.RefreshToken = o.RefreshToken
	}
	lifetime := time.Duration(t.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = defaultTokenLifetime
	}
	fresh := o.WithTokens(t.AccessToken, t.RefreshToken, time.Now().Add(lifetime))
	// The login's own expiry is kept unless the server states a new one, as Claude Code does.
	if secs, err := t.RefreshExpiresIn.Int64(); err == nil && secs > 0 {
		fresh = fresh.WithLoginExpiry(time.Now().Add(time.Duration(secs) * time.Second))
	}
	return fresh, nil
}

// Profile returns which account a token belongs to.
func (c *Client) Profile(ctx context.Context, accessToken string) (Identity, error) {
	var p struct {
		Account struct {
			UUID  string `json:"uuid"`
			Email string `json:"email"`
		} `json:"account"`
		Organization struct {
			UUID     string `json:"uuid"`
			Type     string `json:"organization_type"`
			SeatTier string `json:"seat_tier"`
		} `json:"organization"`
	}
	if err := c.getJSON(ctx, "/api/oauth/profile", accessToken, &p); err != nil {
		return Identity{}, fmt.Errorf("profile: %w", err)
	}
	if p.Account.UUID == "" {
		return Identity{}, errors.New("profile: response has no account")
	}
	return Identity{AccountUUID: p.Account.UUID, Email: p.Account.Email, OrgUUID: p.Organization.UUID,
		Plan: orgPlans[p.Organization.Type], SeatTier: p.Organization.SeatTier}, nil
}

func (c *Client) getJSON(ctx context.Context, path, accessToken string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", c.UserAgent())

	status, header, body, err := c.do(req)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusTooManyRequests:
		return rateLimited("", header)
	default:
		return fmt.Errorf("HTTP %d", status)
	}
	return json.Unmarshal([]byte(body), into)
}

func (c *Client) do(req *http.Request) (int, http.Header, string, error) {
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, nil, "", err
	}
	return res.StatusCode, res.Header, string(body), nil
}

// oauthErrorDetail returns the error code and description of an OAuth error body. Anything else
// is withheld: an unrecognized body could echo request data, tokens included.
func oauthErrorDetail(body string) (code, detail string) {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal([]byte(body), &e) != nil || e.Error == "" {
		return "", fmt.Sprintf("(unrecognized error body, %d bytes, withheld)", len(body))
	}
	detail = e.Error
	if e.Description != "" {
		detail += ": " + e.Description
	}
	if len(detail) > 240 {
		detail = detail[:240] + "…"
	}
	return e.Error, detail
}
