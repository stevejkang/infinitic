package snapshot

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/quota"
	"github.com/stevejkang/infinitic/internal/service"
	"github.com/stevejkang/infinitic/internal/state"
)

func TestBuild(t *testing.T) {
	sampled := time.Date(2026, 10, 18, 12, 0, 0, 0, time.FixedZone("KST", 9*3600))
	reset := sampled.Add(2 * time.Hour)
	st := &state.State{
		ActiveUUID: "aaaa",
		Accounts: []*state.Account{
			{UUID: "aaaa", Email: "a@example.com", Plan: "Team Premium", Usage: &quota.Usage{At: sampled, Windows: []quota.Window{
				{Label: "5h", Percent: 95, ResetsAt: &reset},
				{Label: "7d", Percent: 40},
				{Label: "Fable", Percent: 38, ResetsAt: &reset}}},
				OAuthAccount: json.RawMessage(`{"fullName":"Private Person"}`)},
			{UUID: "bbbb", Email: "b@example.com", LastError: "usage: HTTP 503", FailStreak: 2, RetryAt: &reset},
			{UUID: "cccc", Email: "c@example.com", NeedsReauth: "refresh token rejected (invalid_grant)", Usage: &quota.Usage{At: sampled, Windows: []quota.Window{
				{Label: "5h", Percent: 10}, {Label: "7d"}}}},
		},
	}
	now := sampled.Add(time.Minute)
	snap := Build(st, quota.DefaultBars, SourceCached, &service.Status{Installed: true, Running: true, PID: 42}, now)

	if snap.SchemaVersion != 1 || snap.Source != SourceCached || !snap.ObservedAt.Equal(now) || snap.ObservedAt.Location() != time.UTC {
		t.Fatalf("header %+v", snap)
	}
	if snap.Active == nil || snap.Active.Email != "a@example.com" {
		t.Fatalf("active %+v", snap.Active)
	}
	if snap.Service == nil || !snap.Service.Running || snap.Service.PID != 42 {
		t.Fatalf("service %+v", snap.Service)
	}

	a, b, c := snap.Accounts[0], snap.Accounts[1], snap.Accounts[2]
	if !a.Active || a.Plan != "Team Premium" || a.RetryAt != nil || a.Health != HealthOK || !a.OverBar || a.Usage.Windows[0].Percent != 95 || !a.Usage.Windows[0].ResetsAt.Equal(reset) {
		t.Errorf("a %+v", a)
	}
	if w := a.Usage.Windows; len(w) != 3 || w[2].Label != "Fable" || w[2].Percent != 38 || w[2].ResetsAt.Location() != time.UTC || w[1].ResetsAt != nil {
		t.Errorf("windows %+v", w)
	}
	if b.Active || b.RetryAt == nil || !b.RetryAt.Equal(reset) || b.Health != HealthDegraded || b.Problem != "usage: HTTP 503" || b.Usage != nil || b.OverBar {
		t.Errorf("b %+v", b)
	}
	if c.Health != HealthNeedsReauth || !strings.Contains(c.Problem, "invalid_grant") {
		t.Errorf("c %+v", c)
	}

	out, _ := json.Marshal(snap)
	if strings.Contains(string(out), "Private Person") {
		t.Error("the snapshot leaked ~/.claude.json personal data")
	}
}

func TestBuildWithoutPrimaryOrService(t *testing.T) {
	snap := Build(&state.State{}, quota.DefaultBars, SourceLive, nil, time.Now())
	out, _ := json.Marshal(snap)
	for _, want := range []string{`"active":null`, `"accounts":[]`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(string(out), `"service"`) {
		t.Errorf("unknown service status should be omitted: %s", out)
	}
}

func TestRateLimitingIsNotReportedAsAProblem(t *testing.T) {
	st := &state.State{Accounts: []*state.Account{{UUID: "aaaa", LastError: "usage: rate limited (HTTP 429)", RateLimited: true}}}
	got := Build(st, quota.DefaultBars, SourceCached, nil, time.Now()).Accounts[0]
	if got.Health != HealthOK || got.Problem != "" {
		t.Fatalf("health %q problem %q, want ok and none for routine rate limiting", got.Health, got.Problem)
	}
}

func TestLoginExpiryIsReported(t *testing.T) {
	at := time.Date(2026, 11, 16, 2, 29, 52, 0, time.FixedZone("KST", 9*3600))
	st := &state.State{Accounts: []*state.Account{{UUID: "aaaa", LoginExpiresAt: &at}}}
	got := Build(st, quota.DefaultBars, SourceCached, nil, time.Now()).Accounts[0]
	if got.LoginExpiresAt == nil || got.LoginExpiresAt.Location() != time.UTC || !got.LoginExpiresAt.Equal(at) {
		t.Fatalf("loginExpiresAt %v", got.LoginExpiresAt)
	}
}

func TestTimesAreUTCWholeSeconds(t *testing.T) {
	at := time.Date(2026, 11, 5, 14, 0, 0, 515_000_000, time.FixedZone("KST", 9*3600))
	st := &state.State{Accounts: []*state.Account{{UUID: "aaaa", LoginExpiresAt: &at, RetryAt: &at,
		Usage: &quota.Usage{At: at, Windows: []quota.Window{{Label: "7d", ResetsAt: &at}}}}}}
	out, _ := json.Marshal(Build(st, quota.DefaultBars, SourceCached, nil, at))
	if got := strings.Count(string(out), `"2026-11-05T05:00:00Z"`); got != 5 {
		t.Fatalf("want 5 times as 2026-11-05T05:00:00Z, got %d in %s", got, out)
	}
}

// The published schema and the documents must name the same URL, or a document points at a
// schema that does not describe it.
func TestPublishedSchemaMatchesSchemaURL(t *testing.T) {
	data, err := os.ReadFile("../../public/schemas/status/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		ID         string `json:"$id"`
		Properties struct {
			Schema        struct{ Const string } `json:"$schema"`
			SchemaVersion struct{ Const int }    `json:"schemaVersion"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != SchemaURL || schema.Properties.Schema.Const != SchemaURL || schema.Properties.SchemaVersion.Const != SchemaVersion {
		t.Fatalf("schema $id %q, $schema const %q, schemaVersion const %d; want %q and %d",
			schema.ID, schema.Properties.Schema.Const, schema.Properties.SchemaVersion.Const, SchemaURL, SchemaVersion)
	}
}
