package rotator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
	"github.com/stevejkang/infinitic/internal/quota"
)

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	transient := errors.New("HTTP 503")
	limited := &anthropic.RateLimitedError{RetryAfter: 2 * time.Minute}
	huge := &anthropic.RateLimitedError{RetryAfter: 24 * time.Hour}
	cases := []struct {
		err    error
		streak int
		want   time.Duration
	}{
		{transient, 1, time.Minute},
		{transient, 3, 4 * time.Minute},
		{transient, 50, 15 * time.Minute},
		{limited, 1, 2 * time.Minute},
		{limited, 3, 8 * time.Minute},
		{huge, 1, 15 * time.Minute},
		{huge, 2, 30 * time.Minute},
		{huge, 9, time.Hour},
	}
	for _, tc := range cases {
		if got := backoffFor(tc.err, tc.streak); got != tc.want {
			t.Errorf("backoffFor(%v, %d) = %s, want %s", tc.err, tc.streak, got, tc.want)
		}
	}
}

func TestARateLimitedAccountIsLeftAloneUntilItsRetryTime(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.fake.limited["b"] = true

	h.rot.CheckAllParked(context.Background(), true)
	b := h.account("b")
	if b.RetryAt == nil || time.Until(*b.RetryAt) < time.Minute || b.FailStreak != 1 {
		t.Fatalf("no backoff after a 429: retryAt=%v streak=%d", b.RetryAt, b.FailStreak)
	}
	if !b.RateLimited || b.Problem() != "" || b.LastError == "" {
		t.Fatalf("a 429 must be tracked internally but not shown: rateLimited=%v problem=%q lastError=%q", b.RateLimited, b.Problem(), b.LastError)
	}
	if c, _ := h.rot.Candidates(quota.DefaultBars, time.Now()); len(c) != 0 {
		t.Fatalf("a rate-limited account was offered as a candidate on stale usage: %v", c)
	}
	before := h.fake.callsTo("/api/oauth/usage")
	for range 3 {
		h.rot.CheckAllParked(context.Background(), true)
	}
	if got := h.fake.callsTo("/api/oauth/usage") - before; got != 0 {
		t.Fatalf("%d usage requests during the backoff, want 0", got)
	}

	h.fake.limited["b"] = false
	b.RetryAt = nil // the backoff has passed
	h.rot.CheckAllParked(context.Background(), true)
	if b.LastError != "" || b.FailStreak != 0 || b.RetryAt != nil || b.RateLimited {
		t.Fatalf("recovery did not reset the backoff: %+v", b)
	}
}

func TestAnUnchangedPrimaryIsNotReidentifiedEveryCheck(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.setUsage("a", 10, 10)
	before := h.fake.callsTo("/api/oauth/profile")

	for range 3 {
		h.tick()
	}
	if got := h.fake.callsTo("/api/oauth/profile") - before; got != 0 {
		t.Fatalf("%d profile requests for an unchanged primary, want 0", got)
	}

	h.put(h.store.Primary(), mustParse(t, mustRead(t, h, h.store.Pool(uuidOf("b"))))) // `claude /login` as b
	h.tick()
	if h.rot.State.ActiveUUID != uuidOf("b") {
		t.Fatal("a changed primary was not re-identified")
	}
}

func TestThePlanIsRecordedFromTheGrant(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.rot.CheckAllParked(context.Background(), true)

	for _, name := range []string{"a", "b"} {
		if got := h.account(name).Plan; got != "Ultra" {
			t.Errorf("%s plan %q, want Ultra", name, got)
		}
	}
}

func TestARevokedActiveTokenIsReidentifiedAndRefreshed(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	revoked := mustParse(t, mustRead(t, h, h.store.Primary())).OAuth
	h.fake.expireAccess(revoked)

	h.tick() // usage answers 401
	h.tick() // the primary is identified again, which refreshes it

	if got := mustParse(t, mustRead(t, h, h.store.Primary())).OAuth.AccessToken; got == revoked.AccessToken {
		t.Fatal("the revoked access token was never refreshed")
	}
	if h.ownerOf(h.store.Primary()) != "a" || h.account("a").NeedsReauth != "" {
		t.Fatal("a lost its login")
	}
}

func TestAStoppedCheckLeavesTheAccountAlone(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.rot.CheckAllParked(ctx, true)
	b := h.account("b")
	if b.LastError != "" || b.FailStreak != 0 || b.RetryAt != nil {
		t.Fatalf("a cancelled check degraded the account: lastError=%q streak=%d retryAt=%v", b.LastError, b.FailStreak, b.RetryAt)
	}
	if b.Problem() != "" {
		t.Fatalf("a cancelled check shows as a problem: %q", b.Problem())
	}
}
