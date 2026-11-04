package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
)

func pool() *State {
	s := &State{}
	s.Upsert(anthropic.Identity{AccountUUID: "1111aaaa-0001", Email: "anna@example.com"})
	s.Upsert(anthropic.Identity{AccountUUID: "1111bbbb-0002", Email: "ann@example.com"})
	s.Upsert(anthropic.Identity{AccountUUID: "2222cccc-0003", Email: "bob@example.com"})
	return s
}

func TestResolveTakesOnlyAnExactEmailOrUUID(t *testing.T) {
	s := pool()
	if a, err := s.Resolve("ann@example.com"); err != nil || a.UUID != "1111bbbb-0002" {
		t.Fatalf("exact email: %v %v", a, err)
	}
	if a, err := s.Resolve("2222cccc-0003"); err != nil || a.Email != "bob@example.com" {
		t.Fatalf("exact uuid: %v %v", a, err)
	}
	for sel, hint := range map[string]string{"2222": "bob@example.com", "ANN@Example.com": "ann@example.com", "BOB": "bob@example.com", "1111": "", "ann": "", "nobody": ""} {
		a, err := s.Resolve(sel)
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("%q resolved to %v (%v), want NotFoundError", sel, a, err)
			continue
		}
		if nf.Suggestion != hint {
			t.Errorf("%q: suggestion %q, want %q", sel, nf.Suggestion, hint)
		}
	}
	if _, err := s.Resolve(""); err == nil {
		t.Error("empty selector resolved")
	}
}

func TestSaveLoadRoundTripIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Upsert(anthropic.Identity{AccountUUID: "u1", Email: "a@x"})
	s.ActiveUUID = "u1"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil || back.ActiveUUID != "u1" || back.ByUUID("u1") == nil {
		t.Fatalf("round trip %+v %v", back, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode %v", info.Mode().Perm())
	}
}

func TestLoginNotice(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for left, want := range map[time.Duration]string{
		4 * 24 * time.Hour: "",
		3 * 24 * time.Hour: "login expires in 3 days",
		47 * time.Hour:     "login expires in 2 days",
		5 * time.Hour:      "login expires within a day",
		-time.Minute:       "login expired",
	} {
		at := now.Add(left)
		if got := (&Account{LoginExpiresAt: &at}).LoginNotice(now); got != want {
			t.Errorf("%v left: %q, want %q", left, got, want)
		}
	}
	if got := (&Account{}).LoginNotice(now); got != "" {
		t.Errorf("unknown expiry: %q", got)
	}
}
