package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/creds"
	"github.com/stevejkang/infinitic/internal/quota"
)

func client(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{APIBase: srv.URL, TokenURL: srv.URL + "/token", UserAgent: func() string { return "test" }, HTTP: srv.Client()}
}

func grant(t *testing.T) creds.OAuth {
	t.Helper()
	var o creds.OAuth
	if err := json.Unmarshal([]byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"scopes":["x"]}`), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestRefreshClassifiesFailures(t *testing.T) {
	cases := []struct {
		status int
		body   string
		dead   bool
	}{
		{http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		{http.StatusUnauthorized, `{"error":"invalid_grant"}`, true},
		{http.StatusBadRequest, `{"error":"invalid_request"}`, false},
		{http.StatusTooManyRequests, `{"error":"invalid_grant"}`, false},
		{http.StatusServiceUnavailable, ``, false},
	}
	for _, tc := range cases {
		c := client(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := c.Refresh(context.Background(), grant(t))
		if err == nil || errors.Is(err, ErrDeadGrant) != tc.dead {
			t.Errorf("HTTP %d %s: got %v, dead=%v", tc.status, tc.body, err, tc.dead)
		}
	}
}

func TestRefreshErrorsWithholdUnrecognizedBodies(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>refresh_token=rt-secret</html>`))
	})
	_, err := c.Refresh(context.Background(), grant(t))
	if err == nil || strings.Contains(err.Error(), "rt-secret") || !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("got %v", err)
	}
}

func TestRateLimitsCarryRetryAfter(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	var limited *RateLimitedError
	if _, err := c.Usage(context.Background(), "at"); !errors.As(err, &limited) || limited.RetryAfter != 90*time.Second {
		t.Fatalf("usage: got %v", err)
	}
	if _, err := c.Refresh(context.Background(), grant(t)); !errors.As(err, &limited) || errors.Is(err, ErrDeadGrant) {
		t.Fatalf("refresh: got %v, want a rate limit, not a dead grant", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"120":                           2 * time.Minute,
		"Sun, 18 Oct 2026 12:05:00 GMT": 5 * time.Minute,
		"":                              defaultRetryAfter,
		"-3":                            defaultRetryAfter,
		"Sun, 18 Oct 2026 11:00:00 GMT": defaultRetryAfter,
	}
	for v, want := range cases {
		if got := parseRetryAfter(v, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", v, got, want)
		}
	}
}

func TestProfileReportsThePlanFamily(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"account":{"uuid":"u","email":"e","has_claude_max":false},"organization":{"uuid":"o","organization_type":"claude_team","seat_tier":"team_tier_1"}}`))
	})
	id, err := c.Profile(context.Background(), "at")
	if err != nil || id.Plan != "Team" || id.SeatTier != "team_tier_1" {
		t.Fatalf("got %+v %v", id, err)
	}
}

func TestRefreshKeepsExtrasAndDefaultsMissingFields(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at2"}`))
	})
	fresh, err := c.Refresh(context.Background(), grant(t))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.AccessToken != "at2" || fresh.RefreshToken != "rt" {
		t.Fatalf("tokens %q/%q, want at2 and the old refresh token", fresh.AccessToken, fresh.RefreshToken)
	}
	if fresh.ExpiresWithin(30*time.Minute, time.Now()) {
		t.Fatal("a response without expires_in produced an already-expiring token")
	}
	out, _ := json.Marshal(fresh)
	if !json.Valid(out) || !strings.Contains(string(out), `"scopes":["x"]`) {
		t.Fatalf("extra fields dropped: %s", out)
	}
}

func TestRefreshIsNotAbandonedWhenTheCallerCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := client(t, func(w http.ResponseWriter, _ *http.Request) {
		cancel() // e.g. Ctrl-C while the server is already rotating the pair
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"access_token":"at2","refresh_token":"rt2","expires_in":3600}`))
	})
	fresh, err := c.Refresh(ctx, grant(t))
	if err != nil || fresh.RefreshToken != "rt2" {
		t.Fatalf("the rotated pair was lost: %v", err)
	}
}

func TestA401IsReportedAsUnauthorized(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	if _, err := c.Usage(context.Background(), "at"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("usage: got %v", err)
	}
	if _, err := c.Profile(context.Background(), "at"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("profile: got %v", err)
	}
}

func TestUsageParsesWindows(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at" || r.Header.Get("User-Agent") != "test" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":42.5,"resets_at":"2026-10-18T13:00:00Z"},"seven_day":{"utilization":7,"resets_at":null}}`))
	})
	u, err := c.Usage(context.Background(), "at")
	if err != nil {
		t.Fatal(err)
	}
	s, w := u.Window(quota.SessionLabel), u.Window(quota.WeeklyLabel)
	if s.Percent != 42.5 || w.Percent != 7 || s.ResetsAt == nil || w.ResetsAt != nil || s.Label != "5h" || w.Label != "7d" {
		t.Fatalf("usage %+v", u)
	}
}

func TestRefreshUpdatesTheLoginExpiryOnlyWhenTheServerStatesOne(t *testing.T) {
	var o creds.OAuth
	_ = json.Unmarshal([]byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"refreshTokenExpiresAt":1790000000000}`), &o)
	for body, want := range map[string]time.Duration{
		`{"access_token":"at2"}`:                                      -1,
		`{"access_token":"at2","refresh_token_expires_in":2419200}`:   28 * 24 * time.Hour,
		`{"access_token":"at2","refresh_token_expires_in":"2419200"}`: 28 * 24 * time.Hour,
		`{"access_token":"at2","refresh_token_expires_in":0}`:         -1,
	} {
		c := client(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		fresh, err := c.Refresh(context.Background(), o)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		got, ok := fresh.LoginExpiresAt()
		switch {
		case !ok:
			t.Fatalf("%s: login expiry lost", body)
		case want < 0 && !got.Equal(time.UnixMilli(1790000000000)):
			t.Fatalf("%s: login expiry %v, want it kept", body, got)
		case want > 0 && (got.Sub(time.Now().Add(want)).Abs() > time.Minute):
			t.Fatalf("%s: login expiry %v, want about %v from now", body, got, want)
		}
	}
	if got, _ := o.LoginExpiresAt(); !got.Equal(time.UnixMilli(1790000000000)) {
		t.Fatal("the refreshed copy changed the original grant")
	}
}
