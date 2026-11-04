package rotator

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/creds"
	"github.com/stevejkang/infinitic/internal/credstore"
	"github.com/stevejkang/infinitic/internal/quota"
)

func TestTickSwitchMovesGrantsAndKeepsSiblingEntries(t *testing.T) {
	h := newHarness(t)
	identity := `{"other":1,"oauthAccount":{"accountUuid":"` + uuidOf("b") + `","emailAddress":"b@example.com"}}`
	if err := os.WriteFile(h.rot.Identity.Path, []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	h.pool("a", "b", "c")
	h.account("b").OAuthAccount = json.RawMessage(`{"accountUuid":"` + uuidOf("b") + `","emailAddress":"b@example.com"}`)
	h.setUsage("a", 95, 40)
	h.setUsage("b", 10, 20)
	h.setUsage("c", 50, 99)

	// keep a sibling entry in the primary blob to prove it survives the swap
	raw, _, _ := h.store.Read(h.store.Primary())
	h.put(h.store.Primary(), mustParse(t, strings.Replace(raw, "{", `{"mcpOAuth":{"keep":"me"},`, 1)))

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "b" {
		t.Fatalf("primary belongs to %q, want b", got)
	}
	if got := h.ownerOf(h.store.Pool(uuidOf("a"))); got != "a" {
		t.Fatalf("a was not parked (pool holds %q)", got)
	}
	if got := h.ownerOf(h.store.Pool(uuidOf("b"))); got != "" {
		t.Fatalf("b's grant still has a pool copy (%q): grants must move, not copy", got)
	}
	if raw, _, _ := h.store.Read(h.store.Primary()); !strings.Contains(raw, `"keep":"me"`) {
		t.Fatalf("sibling entries were dropped from the primary blob: %s", raw)
	}
	if h.rot.State.ActiveUUID != uuidOf("b") {
		t.Fatalf("active is %q, want b", h.rot.State.ActiveUUID)
	}
	if data, _ := os.ReadFile(h.rot.Identity.Path); !strings.Contains(string(data), `"other": 1`) || !strings.Contains(string(data), "b@example.com") {
		t.Fatalf("~/.claude.json identity not updated in place: %s", data)
	}
}

func TestTickStaysPutUnderTheBars(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.setUsage("a", 30, 30)
	h.setUsage("b", 0, 0)
	before, _, _ := h.store.Read(h.store.Primary())

	h.tick()

	if after, _, _ := h.store.Read(h.store.Primary()); after != before {
		t.Fatal("primary changed although the active account is under its bars")
	}
}

func TestTickStaysPutWhenEveryAccountIsOverItsBar(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.setUsage("a", 99, 10)
	h.setUsage("b", 95, 10)

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "a" {
		t.Fatalf("switched to %q although every account is over its bar", got)
	}
	if !h.log.contains("no pooled account is below its bars - staying put") {
		t.Fatalf("missing message, log: %v", h.log.lines)
	}
}

func TestUsageIsReadFreshBeforeSwitching(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b", "c")
	h.setUsage("b", 10, 10)
	h.setUsage("c", 20, 20)
	h.rot.CheckAllParked(context.Background(), true) // cache: b looks best
	h.setUsage("b", 99, 10)                          // ...but b fills up before the switch
	h.setUsage("a", 95, 10)

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "c" {
		t.Fatalf("switched to %q using a stale sample, want c", got)
	}
}

func TestSwitchParksAnUnknownPrimaryLogin(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.put(h.store.Pool(uuidOf("a")), mustParse(t, mustRead(t, h, h.store.Primary()))) // a parked first
	h.put(h.store.Primary(), creds.New(h.fake.issue("d", time.Hour)))                 // then `claude /login` as d

	if _, err := h.rot.SyncActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.rot.Switch(context.Background(), h.account("b"), SwitchOptions{}); err != nil {
		t.Fatal(err)
	}

	if !h.log.contains("registered the current login d@example.com") {
		t.Fatalf("d was not registered, log: %v", h.log.lines)
	}
	if got := h.ownerOf(h.store.Pool(uuidOf("d"))); got != "d" {
		t.Fatalf("d was not parked (pool holds %q)", got)
	}
}

func TestAnExpiringParkedTokenIsRefreshedInItsPoolSlot(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	stale := h.fake.issue("b", 10*time.Second)
	h.put(h.store.Pool(uuidOf("b")), creds.New(stale))

	if err := h.rot.Switch(context.Background(), h.account("b"), SwitchOptions{}); err != nil {
		t.Fatal(err)
	}

	blob := mustParse(t, mustRead(t, h, h.store.Primary()))
	if blob.OAuth.AccessToken == stale.AccessToken || h.fake.refreshN != 1 {
		t.Fatalf("the parked token was not refreshed before use (refreshes: %d)", h.fake.refreshN)
	}
}

func TestATransientRefreshFailureIsRetriedNotMarkedDead(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b", "c")
	flaky := h.fake.issue("b", 10*time.Second)
	h.put(h.store.Pool(uuidOf("b")), creds.New(flaky))
	clear := h.fake.failTransiently(flaky)
	h.setUsage("a", 95, 10)
	h.setUsage("c", 20, 20)

	h.tick()

	b := h.account("b")
	if b.NeedsReauth != "" || !strings.Contains(b.LastError, "503") {
		t.Fatalf("transient failure misclassified: needsReauth=%q lastError=%q", b.NeedsReauth, b.LastError)
	}
	if got := h.ownerOf(h.store.Primary()); got != "c" {
		t.Fatalf("primary belongs to %q, want c", got)
	}

	clear()
	b.RetryAt = nil // the backoff has passed
	h.rot.CheckAllParked(context.Background(), true)
	if b.LastError != "" || b.NeedsReauth != "" {
		t.Fatalf("recovered account still flagged: needsReauth=%q lastError=%q", b.NeedsReauth, b.LastError)
	}
}

func TestADeadParkedGrantIsFlaggedAndNeverChosen(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b", "c")
	dead := h.fake.issue("b", 10*time.Second)
	h.put(h.store.Pool(uuidOf("b")), creds.New(dead))
	h.fake.revoke(dead)
	h.setUsage("a", 95, 10)
	h.setUsage("b", 0, 0)
	h.setUsage("c", 50, 50)

	h.tick()

	if !strings.Contains(h.account("b").NeedsReauth, "invalid_grant") {
		t.Fatalf("dead grant not flagged: %q", h.account("b").NeedsReauth)
	}
	if got := h.ownerOf(h.store.Primary()); got != "c" {
		t.Fatalf("primary belongs to %q, want c", got)
	}
}

func TestADeadActiveLoginMovesToAHealthyAccountWithoutParkingIt(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	dead := h.fake.issue("a", 10*time.Second)
	h.put(h.store.Primary(), creds.New(dead))
	h.fake.revoke(dead)

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "b" {
		t.Fatalf("primary belongs to %q, want b", got)
	}
	if got := h.ownerOf(h.store.Pool(uuidOf("a"))); got != "" {
		t.Fatal("the dead grant was parked")
	}
	if !strings.Contains(h.account("a").NeedsReauth, "invalid_grant") {
		t.Fatalf("dead active account not flagged: %q", h.account("a").NeedsReauth)
	}
}

func TestInvalidGrantFromAConcurrentRotationIsNotMistakenForDead(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	stale := h.fake.issue("a", 10*time.Second)
	h.put(h.store.Primary(), creds.New(stale))
	h.fake.onRefresh[stale.RefreshToken] = func() *http.Response {
		h.fake.revoke(stale)
		h.put(h.store.Primary(), creds.New(h.fake.issue("a", time.Hour))) // claude refreshed a moment earlier
		return nil
	}

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "a" {
		t.Fatalf("primary belongs to %q, want a", got)
	}
	if h.account("a").NeedsReauth != "" {
		t.Fatalf("a was marked dead: %q", h.account("a").NeedsReauth)
	}
}

func mustRead(t *testing.T, h *harness, slot credstore.Slot) string {
	t.Helper()
	raw, ok, err := h.store.Read(slot)
	if err != nil || !ok {
		t.Fatalf("read %s: ok=%v err=%v", slot.Label, ok, err)
	}
	return raw
}

func mustParse(t *testing.T, raw string) creds.Blob {
	t.Helper()
	b, err := creds.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTickUsesTheQuotaThatExpiresFirst(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b", "c")
	h.setUsage("a", 95, 40)
	h.setUsage("b", 10, 20) // most room, but its week resets in 5 days
	h.setResets("b", 3*time.Hour, 5*24*time.Hour)
	h.setUsage("c", 10, 60) // less room, but its 38% expires in 20 hours
	h.setResets("c", 3*time.Hour, 20*time.Hour)

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "c" {
		t.Fatalf("primary belongs to %q, want c (its quota expires first)", got)
	}
}

func TestTickRelaxesTheSpikeMarginRatherThanStayOnAnExhaustedAccount(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.setUsage("a", 95, 40)
	h.setUsage("b", 89.5, 20) // below the bar, but 0.5% is too little room for a post-switch spike, even on Ultra

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "b" {
		t.Fatalf("primary belongs to %q, want b", got)
	}
	if !h.log.contains("No account has room for the post-switch spike") {
		t.Fatalf("relaxed margin not reported: %v", h.log.lines)
	}
}

func TestSwitchAndCheckLogsCarryUsageResetsAndSource(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.setUsage("a", 95, 40)
	h.setUsage("b", 10, 20)
	h.setResets("b", 3*time.Hour, 20*time.Hour)

	h.tick()
	if !h.log.contains("event=switch") || !h.log.contains("to_usage.7d=20") || !h.log.contains("to_reset.7d=") {
		t.Fatalf("switch log lacks the target's usage and resets: %v", h.log.lines)
	}

	h.fake.limited["b"] = true
	h.account("b").Usage.At = time.Time{} // stale, so the next check reads it again
	h.tick()
	if !h.log.contains("source=cached") {
		t.Fatalf("a failed read was not logged as cached: %v", h.log.lines)
	}
}

func TestAnAccountWithoutASessionLimitIsUsedAndLogged(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.setUsage("a", 95, 40)
	h.fake.noSession["b"] = true
	h.setUsage("b", 0, 30)

	h.tick()
	if h.rot.State.ActiveUUID != uuidOf("b") {
		t.Fatalf("did not switch to the account without a 5-hour limit: %v", h.log.lines)
	}
	if !h.log.contains("to_usage.5h=none") || !h.log.contains("to_usage.7d=30") {
		t.Fatalf("switch log does not say the target has no 5-hour limit: %v", h.log.lines)
	}
	if u := h.account("b").Usage; u.Window(quota.SessionLabel) != nil || !quota.NoSessionLimit(u) {
		t.Fatalf("usage %+v", u)
	}
}

func TestStayingPutSaysWhenAccountsWereSkipped(t *testing.T) {
	h := newHarness(t)
	h.pool("a", "b")
	h.setUsage("a", 95, 40)
	h.setUsage("b", 10, 10)
	h.fake.limited["b"] = true

	h.tick()

	if got := h.ownerOf(h.store.Primary()); got != "a" {
		t.Fatalf("primary belongs to %q, want a: b's usage is unknown while it is rate limited", got)
	}
	if !h.log.contains("1 skipped: their last check failed or is rate limited") {
		t.Fatalf("staying put did not mention the skipped account: %v", h.log.lines)
	}
}
