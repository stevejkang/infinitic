package rotator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
	"github.com/stevejkang/infinitic/internal/creds"
	"github.com/stevejkang/infinitic/internal/quota"
)

func identity(account string) anthropic.Identity {
	return anthropic.Identity{AccountUUID: uuidOf(account), Email: account + "@example.com"}
}

func TestStoreLoginRenewsWhereTheAccountLives(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	// The primary login carries a sibling entry that a renewal must keep.
	o, _ := json.Marshal(h.fake.issue("a", time.Hour))
	withSibling, _ := creds.Parse(`{"mcpOAuth":{"server":"kept"},"claudeAiOauth":` + string(o) + `}`)
	h.put(h.store.Primary(), withSibling)
	h.account("b").NeedsReauth = "invalid_grant"
	expiry := time.Now().Add(28 * 24 * time.Hour).Truncate(time.Millisecond)

	for _, tc := range []struct {
		account string
		want    LoginTarget
	}{{"b", LoginParked}, {"b", LoginParked}, {"a", LoginPrimary}, {"c", LoginAdded}} {
		fresh := h.fake.issue(tc.account, time.Hour).WithLoginExpiry(expiry)
		a, target, err := h.rot.StoreLogin(identity(tc.account), fresh, true)
		if err != nil || target != tc.want {
			t.Fatalf("%s: target %v err %v, want %v", tc.account, target, err, tc.want)
		}
		slot := h.store.Pool(uuidOf(tc.account))
		if target == LoginPrimary {
			slot = h.store.Primary()
		}
		raw, _, _ := h.store.Read(slot)
		if !strings.Contains(raw, fresh.AccessToken) {
			t.Fatalf("%s: %s does not hold the new grant", tc.account, slot.Label)
		}
		if a.NeedsReauth != "" || a.LoginExpiresAt == nil || !a.LoginExpiresAt.Equal(expiry) {
			t.Fatalf("%s: needsReauth %q loginExpiresAt %v", tc.account, a.NeedsReauth, a.LoginExpiresAt)
		}
	}
	raw, _, _ := h.store.Read(h.store.Primary())
	if !strings.Contains(raw, `"server":"kept"`) {
		t.Fatalf("renewing the active login dropped its sibling entries: %s", raw)
	}
	if active, err := h.rot.SyncActive(context.Background()); err != nil || active.Account.UUID != uuidOf("a") {
		t.Fatalf("primary after renewal: %v %v", active, err)
	}
	if h.ownerOf(h.store.Pool(uuidOf("a"))) != "" {
		t.Fatal("renewing the active account parked a copy of it")
	}
}

func TestStoreLoginRefusesWhenThePrimaryIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	before, _, _ := h.store.Read(h.store.Primary())
	if _, _, err := h.rot.StoreLogin(identity("a"), h.fake.issue("a", time.Hour), false); !errors.Is(err, ErrPrimaryUnknown) {
		t.Fatalf("err %v, want ErrPrimaryUnknown", err)
	}
	if after, _, _ := h.store.Read(h.store.Primary()); after != before {
		t.Fatal("the primary login changed")
	}
	// A parked account does not depend on the primary.
	if _, target, err := h.rot.StoreLogin(identity("b"), h.fake.issue("b", time.Hour), false); err != nil || target != LoginParked {
		t.Fatalf("parked: %v %v", target, err)
	}
}

func TestCheckingAParkedAccountRecordsWhenItsLoginExpires(t *testing.T) {
	h := newHarness(t)
	h.pool("a")
	expiry := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Millisecond)
	h.put(h.store.Pool(uuidOf("b")), creds.New(h.fake.issue("b", time.Hour).WithLoginExpiry(expiry)))
	h.rot.State.Upsert(identity("b"))
	h.rot.CheckAllParked(context.Background(), true)
	if got := h.account("b").LoginExpiresAt; got == nil || !got.Equal(expiry) {
		t.Fatalf("loginExpiresAt %v, want %v", got, expiry)
	}
}

func TestWatchWarnsOnceADayAboutAnExpiringLogin(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	soon := time.Now().Add(47 * time.Hour)
	h.account("b").LoginExpiresAt = &soon
	warnings := func() int {
		n := 0
		for _, l := range h.log.lines {
			if strings.Contains(l, "event=login_expiring") {
				n++
				if !strings.Contains(l, "login expires in 2 days") || !strings.Contains(l, "infinitic reauth b@example.com") {
					t.Fatalf("warning %q", l)
				}
			}
		}
		return n
	}
	for range 2 {
		if err := h.rot.Tick(context.Background(), quota.DefaultBars); err != nil {
			t.Fatal(err)
		}
	}
	if n := warnings(); n != 1 {
		t.Fatalf("%d warnings after two checks, want 1", n)
	}
	dayAgo := time.Now().Add(-25 * time.Hour)
	h.account("b").LoginWarnedAt = &dayAgo
	if err := h.rot.Tick(context.Background(), quota.DefaultBars); err != nil {
		t.Fatal(err)
	}
	if n := warnings(); n != 2 {
		t.Fatalf("%d warnings a day later, want 2", n)
	}
	later := time.Now().Add(28 * 24 * time.Hour)
	h.account("b").LoginExpiresAt = &later // renewed
	if err := h.rot.Tick(context.Background(), quota.DefaultBars); err != nil {
		t.Fatal(err)
	}
	if h.account("b").LoginWarnedAt != nil || warnings() != 2 {
		t.Fatal("a renewed login is still warned about")
	}
}

func TestStoreLoginDoesNotKeepAnAccountItCouldNotPark(t *testing.T) {
	h := newHarness(t)
	h.pool("a")
	// A file where the pool directory should be: parking fails after the account was upserted.
	if err := os.WriteFile(h.store.PoolDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := h.rot.StoreLogin(identity("c"), h.fake.issue("c", time.Hour), true)
	if err == nil {
		t.Fatal("parking into a file succeeded")
	}
	if _, err := h.rot.State.Resolve("c@example.com"); err == nil {
		t.Fatal("an account whose login could not be parked stayed in the pool")
	}
}
