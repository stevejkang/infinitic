package creds

import (
	"encoding/json"
	"testing"
	"time"
)

const stored = `{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt","expiresAt":1792340174841,` +
	`"scopes":["user:inference"],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"},` +
	`"mcpOAuth":{"server":{"token":"x"}}}`

func TestRoundTripPreservesUnknownFields(t *testing.T) {
	b, err := Parse(stored)
	if err != nil {
		t.Fatal(err)
	}
	if b.OAuth.AccessToken != "at" || b.OAuth.RefreshToken != "rt" || b.OAuth.ExpiresAt.UnixMilli() != 1792340174841 {
		t.Fatalf("parsed %+v", b.OAuth)
	}
	assertSameJSON(t, b.String(), stored)
}

func TestWithTokensKeepsEverythingElse(t *testing.T) {
	b, _ := Parse(stored)
	exp := time.UnixMilli(1800000000000)
	out := b.WithOAuth(b.OAuth.WithTokens("at2", "rt2", exp)).String()
	assertSameJSON(t, out, `{"claudeAiOauth":{"accessToken":"at2","refreshToken":"rt2","expiresAt":1800000000000,`+
		`"scopes":["user:inference"],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"},`+
		`"mcpOAuth":{"server":{"token":"x"}}}`)
}

func TestParseAcceptsABareGrant(t *testing.T) {
	b, err := Parse(`{"accessToken":"at","refreshToken":"rt","expiresAt":1}`)
	if err != nil || b.OAuth.AccessToken != "at" {
		t.Fatalf("bare grant: %v %+v", err, b.OAuth)
	}
}

func TestParseRejectsADeadCredential(t *testing.T) {
	for name, raw := range map[string]string{
		"empty refresh": `{"claudeAiOauth":{"accessToken":"at","refreshToken":"","expiresAt":1}}`,
		"no expiry":     `{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt"}}`,
		"not json":      `nope`,
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

func TestExpiresWithin(t *testing.T) {
	now := time.Now()
	o := OAuth{ExpiresAt: now.Add(time.Minute)}
	if !o.ExpiresWithin(2*time.Minute, now) || o.ExpiresWithin(30*time.Second, now) {
		t.Fatal("ExpiresWithin misjudged a token expiring in one minute")
	}
}

func assertSameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(want), &w)
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("got  %s\nwant %s", gb, wb)
	}
}

func TestPlan(t *testing.T) {
	cases := []struct{ fields, seat, want string }{
		{`"subscriptionType":"pro","rateLimitTier":"default_claude_ai"`, "", "Pro"},
		{`"subscriptionType":"max","rateLimitTier":"default_claude_max_5x"`, "", "Max"},
		{`"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"`, "", "Ultra"},
		{`"subscriptionType":"ultra","rateLimitTier":"default_claude_ultra"`, "", "Ultra"},
		{`"subscriptionType":"team","rateLimitTier":"default_claude_max_5x"`, "team_tier_1", "Team Premium"},
		{`"subscriptionType":"team","rateLimitTier":"default_claude_max_5x"`, "", "Team Premium"},
		{`"subscriptionType":"team","rateLimitTier":"default_raven"`, "team_standard", "Team Standard"},
		{`"subscriptionType":"team","rateLimitTier":"default_raven"`, "", "Team Standard"},
		{`"subscriptionType":"team","rateLimitTier":"default_claude_ai"`, "", "Team"},
		{`"subscriptionType":"enterprise","rateLimitTier":"default_raven"`, "", "Enterprise"},
		{`"rateLimitTier":"default_claude_max_5x"`, "", ""},
		{`"scopes":[]`, "", ""},
	}
	for _, c := range cases {
		raw := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1,` + c.fields + `}}`
		b, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := b.OAuth.Plan(c.seat); got != c.want {
			t.Errorf("%s seat %q: plan %q, want %q", c.fields, c.seat, got, c.want)
		}
	}
}
