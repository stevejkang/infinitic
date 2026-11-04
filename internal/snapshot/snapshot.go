// Package snapshot is the machine-readable view of infinitic's state (`infinitic status --json`).
//
// It is a public contract for other programs: fields are only ever added within a schema
// version. It never contains tokens or the personal data in ~/.claude.json.
package snapshot

import (
	"time"

	"github.com/stevejkang/infinitic/internal/quota"
	"github.com/stevejkang/infinitic/internal/service"
	"github.com/stevejkang/infinitic/internal/state"
)

const SchemaVersion = 1

// SchemaURL is the JSON Schema for SchemaVersion, emitted as the document's "$schema".
const SchemaURL = "https://raw.githubusercontent.com/stevejkang/infinitic/main/public/schemas/status/v1.json"

// Source tells whether usage was just read from the API or comes from the last stored sample.
type Source string

const (
	SourceLive   Source = "live"
	SourceCached Source = "cached"
)

// Health of an account's grant.
type Health string

const (
	HealthOK          Health = "ok"           // usable
	HealthDegraded    Health = "degraded"     // the last refresh or usage read failed transiently; rate limiting (HTTP 429) is not reported, see retryAt
	HealthNeedsReauth Health = "needs_reauth" // the grant is dead; run `infinitic reauth` for it
)

type Snapshot struct {
	Schema        string    `json:"$schema"`
	SchemaVersion int       `json:"schemaVersion"`
	ObservedAt    time.Time `json:"observedAt"`
	Source        Source    `json:"source"`
	Bars          Bars      `json:"bars"`
	Active        *Ref      `json:"active"` // null when there is no primary login
	Service       *Service  `json:"service,omitempty"`
	Accounts      []Account `json:"accounts"`
}

type Bars struct {
	Session float64 `json:"session"`
	Weekly  float64 `json:"weekly"`
}

type Ref struct {
	UUID  string `json:"uuid"`
	Email string `json:"email,omitempty"`
}

type Service struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	Note      string `json:"note,omitempty"`
}

type Account struct {
	UUID    string `json:"uuid"`
	Email   string `json:"email,omitempty"`
	Plan    string `json:"plan,omitempty"` // e.g. "Ultra", "Team Premium"; omitted until known
	Active  bool   `json:"active"`
	Health  Health `json:"health"`
	Problem string `json:"problem,omitempty"` // the error behind a non-ok health
	OverBar bool   `json:"overBar"`           // at or past a bar (only meaningful when usage is known)
	// RetryAt is when infinitic contacts the account again after failures (rate limiting).
	RetryAt *time.Time `json:"retryAt,omitempty"`
	// LoginExpiresAt is when the account's login expires and it needs `infinitic reauth`.
	LoginExpiresAt *time.Time `json:"loginExpiresAt,omitempty"`
	Usage          *Usage     `json:"usage"` // null until the account was sampled once
}

type Usage struct {
	SampledAt time.Time `json:"sampledAt"`
	// Windows lists every cap the usage endpoint reported: "5h" and "7d" first, then narrower
	// ones such as a per-model weekly limit. New kinds appear without a schema change. A plan
	// without a 5-hour limit has "7d" but no "5h".
	Windows []Window `json:"windows"`
}

type Window struct {
	Label    string     `json:"label"` // "5h", "7d", or e.g. "Fable"
	Percent  float64    `json:"percent"`
	ResetsAt *time.Time `json:"resetsAt"`
}

// Build assembles a snapshot. svc may be nil when the service status is unknown.
func Build(st *state.State, bars quota.Bars, source Source, svc *service.Status, now time.Time) Snapshot {
	snap := Snapshot{
		Schema:        SchemaURL,
		SchemaVersion: SchemaVersion,
		ObservedAt:    stamp(now),
		Source:        source,
		Bars:          Bars{Session: bars.Session, Weekly: bars.Weekly},
		Accounts:      make([]Account, 0, len(st.Accounts)),
	}
	if svc != nil {
		snap.Service = &Service{Installed: svc.Installed, Running: svc.Running, PID: svc.PID, Note: svc.Note}
	}
	for _, a := range st.Accounts {
		acct := account(a, bars)
		acct.Active = a.UUID == st.ActiveUUID
		if acct.Active {
			snap.Active = &Ref{UUID: a.UUID, Email: a.Email}
		}
		snap.Accounts = append(snap.Accounts, acct)
	}
	return snap
}

func account(a *state.Account, bars quota.Bars) Account {
	out := Account{UUID: a.UUID, Email: a.Email, Plan: a.Plan, Health: HealthOK, OverBar: bars.Over(a.Usage), RetryAt: utc(a.RetryAt), LoginExpiresAt: utc(a.LoginExpiresAt)}
	switch {
	case a.NeedsReauth != "":
		out.Health, out.Problem = HealthNeedsReauth, a.NeedsReauth
	case a.Problem() != "":
		out.Health, out.Problem = HealthDegraded, a.Problem()
	}
	if u := a.Usage; u != nil {
		out.Usage = &Usage{SampledAt: stamp(u.At), Windows: make([]Window, 0, len(u.Windows))}
		for _, w := range u.Windows {
			out.Usage.Windows = append(out.Usage.Windows, Window{Label: w.Label, Percent: w.Percent, ResetsAt: utc(w.ResetsAt)})
		}
	}
	return out
}

// stamp is how every time in the document is written: UTC, whole seconds, so it always
// serializes as RFC 3339 "2006-01-02T15:04:05Z".
func stamp(t time.Time) time.Time {
	return t.UTC().Truncate(time.Second)
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := stamp(*t)
	return &u
}
