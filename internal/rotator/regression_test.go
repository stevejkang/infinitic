package rotator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/creds"
	"github.com/stevejkang/infinitic/internal/quota"
)

func TestARefreshKeepsTheNewGrantWhenOnlySiblingEntriesChangedMeanwhile(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	stale := h.fake.issue("a", 10*time.Second)
	h.put(h.store.Primary(), creds.New(stale))
	h.fake.onRefresh[stale.RefreshToken] = func() *http.Response {
		withSibling := mustParse(t, strings.Replace(creds.New(stale).String(), "{", `{"mcpOAuth":{"new":"entry"},`, 1))
		h.put(h.store.Primary(), withSibling) // claude wrote an MCP login, same Claude grant
		return nil
	}

	h.tick()

	blob := mustParse(t, mustRead(t, h, h.store.Primary()))
	if blob.OAuth.AccessToken == stale.AccessToken {
		t.Fatal("the refreshed grant was discarded; the primary still holds the revoked pair")
	}
	if h.fake.owner(blob.OAuth.AccessToken) != "a" {
		t.Fatal("the primary does not hold a working grant for a")
	}
	if !strings.Contains(mustRead(t, h, h.store.Primary()), `"new":"entry"`) {
		t.Fatal("the sibling entry written during the refresh was dropped")
	}
}

func TestARefreshRacingANewLoginParksTheRefreshedGrant(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	stale := h.fake.issue("a", 10*time.Second)
	h.put(h.store.Primary(), creds.New(stale))
	h.fake.onRefresh[stale.RefreshToken] = func() *http.Response {
		h.put(h.store.Primary(), creds.New(h.fake.issue("d", time.Hour))) // `claude /login` as d
		return nil
	}

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "d" {
		t.Fatalf("primary belongs to %q, want the new login d", got)
	}
	if got := h.ownerOf(h.store.Pool(uuidOf("a"))); got != "a" {
		t.Fatalf("a's refreshed grant was lost (pool holds %q)", got)
	}
}

func TestADeadPrimaryIsNotOverwrittenOnceSomeoneLoggedIn(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	dead := h.fake.issue("a", 10*time.Second)
	h.put(h.store.Primary(), creds.New(dead))
	h.fake.revoke(dead)

	_, err := h.rot.SyncActive(context.Background())
	var deadErr *DeadPrimaryError
	if !errors.As(err, &deadErr) {
		t.Fatalf("got %v, want a dead primary", err)
	}
	h.put(h.store.Primary(), creds.New(h.fake.issue("d", time.Hour))) // the user ran /login meanwhile

	err = h.rot.Switch(context.Background(), h.account("b"), SwitchOptions{ReplaceDead: deadErr})
	if !errors.Is(err, ErrPrimaryChanged) {
		t.Fatalf("got %v, want ErrPrimaryChanged", err)
	}
	if got := h.ownerOf(h.store.Primary()); got != "d" {
		t.Fatalf("the new login was overwritten by %q", got)
	}
	if got := h.ownerOf(h.store.Pool(uuidOf("b"))); got != "b" {
		t.Fatal("b's grant left its pool slot although the switch did not happen")
	}
}

func TestASwitchFallsThroughToTheNextCandidate(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b", "c")
	h.setUsage("a", 95, 10)
	h.setUsage("b", 0, 0)
	h.setUsage("c", 40, 40)
	h.rot.CheckAllParked(context.Background(), true)
	if err := h.store.Delete(h.store.Pool(uuidOf("b"))); err != nil { // b's credential vanished
		t.Fatal(err)
	}
	h.rot.CheckActive(context.Background(), &Active{Account: h.account("a")}, true)

	if _, err := h.rot.SwitchToBest(context.Background(), quota.DefaultBars, SwitchOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := h.ownerOf(h.store.Primary()); got != "c" {
		t.Fatalf("primary belongs to %q, want c after b turned out unusable", got)
	}
}

func TestAnAccountWhoseCheckFailedIsNotChosenOnStaleUsage(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b", "c")
	h.setUsage("b", 0, 0)
	h.setUsage("c", 40, 40)
	h.rot.CheckAllParked(context.Background(), true)
	h.account("b").LastError = "usage: HTTP 503"

	candidates, _ := h.rot.Candidates(quota.DefaultBars, time.Now())
	if len(candidates) != 1 || candidates[0] != h.account("c") {
		t.Fatalf("candidates %v, want only c", candidates)
	}
}

func TestA401ForcesARefreshInsteadOfAFailure(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	revokedEarly := h.fake.issue("b", time.Hour)
	h.put(h.store.Pool(uuidOf("b")), creds.New(revokedEarly))
	h.fake.expireAccess(revokedEarly)
	h.setUsage("b", 5, 5)

	h.rot.CheckParked(context.Background(), h.account("b"), true)

	b := h.account("b")
	if b.LastError != "" || b.NeedsReauth != "" || b.Usage == nil {
		t.Fatalf("401 not recovered by a refresh: lastError=%q needsReauth=%q", b.LastError, b.NeedsReauth)
	}
}

func TestALoggedOutPrimaryGetsAGrantAndKeepsItsSiblings(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	if err := h.store.Write(h.store.Primary(), `{"mcpOAuth":{"keep":"me"}}`); err != nil { // claude /logout
		t.Fatal(err)
	}
	h.setUsage("b", 1, 1)

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "b" {
		t.Fatalf("primary belongs to %q, want b", got)
	}
	if !strings.Contains(mustRead(t, h, h.store.Primary()), `"keep":"me"`) {
		t.Fatal("the logged-out blob's sibling entries were dropped")
	}
}

func TestAnInterruptedSwitchLeavesNoSecondCopy(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.put(h.store.Pool(uuidOf("a")), mustParse(t, mustRead(t, h, h.store.Primary()))) // crash after install, before delete

	if _, err := h.rot.SyncActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.ownerOf(h.store.Pool(uuidOf("a"))); got != "" {
		t.Fatal("the active account still has a pool copy")
	}
}
