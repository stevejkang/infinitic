package rotator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
	"github.com/stevejkang/infinitic/internal/creds"
	"github.com/stevejkang/infinitic/internal/credstore"
	"github.com/stevejkang/infinitic/internal/quota"
	"github.com/stevejkang/infinitic/internal/state"
)

// fakeAnthropic issues tokens per account and answers refresh/profile/usage like the real service.
type fakeAnthropic struct {
	mu        sync.Mutex
	usage     map[string][2]float64   // account -> {5h, 7d}
	resets    map[string][2]time.Time // account -> {5h, 7d} reset times; absent = null
	tokens    map[string]string       // access token -> account
	refreshes map[string]string       // refresh token -> account (single use)
	onRefresh map[string]func() *http.Response
	counter   int
	refreshN  int
	calls     map[string]int  // path -> requests served
	limited   map[string]bool // account -> usage answers 429
	noSession map[string]bool // account -> its plan has no 5-hour limit (five_hour is null)
}

func newFake() *fakeAnthropic {
	return &fakeAnthropic{
		usage:     map[string][2]float64{},
		resets:    map[string][2]time.Time{},
		tokens:    map[string]string{},
		refreshes: map[string]string{},
		onRefresh: map[string]func() *http.Response{},
		calls:     map[string]int{},
		limited:   map[string]bool{},
		noSession: map[string]bool{},
	}
}

func uuidOf(account string) string {
	return account + account + account + account + "-0000-0000-0000-000000000000"
}

func (f *fakeAnthropic) issue(account string, expiresIn time.Duration) creds.OAuth {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	access, refresh := fmt.Sprintf("at-%s-%d", account, f.counter), fmt.Sprintf("rt-%s-%d", account, f.counter)
	f.tokens[access], f.refreshes[refresh] = account, account
	var o creds.OAuth
	_ = json.Unmarshal(fmt.Appendf(nil, `{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}`,
		access, refresh, time.Now().Add(expiresIn).UnixMilli()), &o)
	return o
}

func (f *fakeAnthropic) owner(accessToken string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[accessToken]
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls[r.URL.Path]++
	f.mu.Unlock()
	if r.URL.Path == "/token" {
		f.serveRefresh(w, r)
		return
	}
	account := f.owner(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if account == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/api/oauth/profile":
		fmt.Fprintf(w, `{"account":{"uuid":%q,"email":"%s@example.com"},"organization":{"uuid":"org-%s"}}`, uuidOf(account), account, account)
	case "/api/oauth/usage":
		f.mu.Lock()
		u, limited, noSession := f.usage[account], f.limited[account], f.noSession[account]
		r, hasResets := f.resets[account]
		f.mu.Unlock()
		if limited {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		at := [2]string{"null", "null"}
		if hasResets {
			for i, t := range r {
				at[i] = `"` + t.UTC().Format(time.RFC3339) + `"`
			}
		}
		five := fmt.Sprintf(`{"utilization":%g,"resets_at":%s}`, u[0], at[0])
		if noSession {
			five = "null"
		}
		fmt.Fprintf(w, `{"five_hour":%s,"seven_day":{"utilization":%g,"resets_at":%s}}`, five, u[1], at[1])
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAnthropic) serveRefresh(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rt := r.PostForm.Get("refresh_token")
	f.mu.Lock()
	hook := f.onRefresh[rt]
	f.mu.Unlock()
	if hook != nil {
		if res := hook(); res != nil {
			w.WriteHeader(res.StatusCode)
			return
		}
	}
	f.mu.Lock()
	account, ok := f.refreshes[rt]
	delete(f.refreshes, rt)
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
		return
	}
	o := f.issue(account, 8*time.Hour)
	f.mu.Lock()
	f.refreshN++
	f.mu.Unlock()
	fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":28800}`, o.AccessToken, o.RefreshToken)
}

// revoke makes a refresh token answer invalid_grant.
func (f *fakeAnthropic) revoke(o creds.OAuth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.refreshes, o.RefreshToken)
}

// failTransiently makes a refresh token answer 503 until cleared.
func (f *fakeAnthropic) failTransiently(o creds.OAuth) (clear func()) {
	f.mu.Lock()
	f.onRefresh[o.RefreshToken] = func() *http.Response { return &http.Response{StatusCode: http.StatusServiceUnavailable} }
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		delete(f.onRefresh, o.RefreshToken)
		f.mu.Unlock()
	}
}

// recorder is a Reporter that keeps every message.
type recorder struct{ lines []string }

func (r *recorder) Info(m string, f ...any)    { r.add(m, f) }
func (r *recorder) Warn(m string, f ...any)    { r.add(m, f) }
func (r *recorder) Success(m string, f ...any) { r.add(m, f) }

// add keeps the message followed by its fields as " key=value" pairs.
func (r *recorder) add(m string, fields []any) {
	for i := 0; i+1 < len(fields); i += 2 {
		m += fmt.Sprintf(" %v=%v", fields[i], fields[i+1])
	}
	r.lines = append(r.lines, m)
}

func (r *recorder) contains(sub string) bool {
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

type harness struct {
	t     *testing.T
	fake  *fakeAnthropic
	store credstore.File
	rot   *Rotator
	log   *recorder
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFake()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	st, err := state.Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:     t,
		fake:  fake,
		store: credstore.File{PrimaryPath: filepath.Join(dir, "claude", ".credentials.json"), PoolDir: filepath.Join(dir, "pool")},
		log:   &recorder{},
	}
	h.rot = &Rotator{
		Store:    h.store,
		API:      &anthropic.Client{APIBase: srv.URL, TokenURL: srv.URL + "/token", UserAgent: func() string { return "test" }, HTTP: srv.Client()},
		State:    st,
		Identity: IdentityFile{Path: filepath.Join(dir, "claude.json")},
		Report:   h.log,
	}
	return h
}

func (h *harness) put(slot credstore.Slot, blob creds.Blob) {
	h.t.Helper()
	if err := h.store.Write(slot, blob.String()); err != nil {
		h.t.Fatal(err)
	}
}

// ownerOf returns the account owning the grant in slot ("" if empty).
func (h *harness) ownerOf(slot credstore.Slot) string {
	h.t.Helper()
	raw, ok, err := h.store.Read(slot)
	if err != nil {
		h.t.Fatal(err)
	}
	if !ok {
		return ""
	}
	blob, err := creds.Parse(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.fake.owner(blob.OAuth.AccessToken)
}

func (h *harness) account(name string) *state.Account {
	h.t.Helper()
	a := h.rot.State.ByUUID(uuidOf(name))
	if a == nil {
		h.t.Fatalf("account %s is not in the pool", name)
	}
	return a
}

// pool sets up: a active in the primary, the others parked, all with long-lived tokens.
func (h *harness) pool(active string, parked ...string) {
	h.t.Helper()
	h.put(h.store.Primary(), creds.New(h.fake.issue(active, time.Hour)))
	for _, p := range parked {
		h.put(h.store.Pool(uuidOf(p)), creds.New(h.fake.issue(p, time.Hour)))
		h.rot.State.Upsert(anthropic.Identity{AccountUUID: uuidOf(p), Email: p + "@example.com"})
	}
	if _, err := h.rot.SyncActive(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) setUsage(account string, fiveHour, sevenDay float64) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	h.fake.usage[account] = [2]float64{fiveHour, sevenDay}
}

func (h *harness) setResets(account string, fiveHour, sevenDay time.Duration) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	now := time.Now()
	h.fake.resets[account] = [2]time.Time{now.Add(fiveHour), now.Add(sevenDay)}
}

func (h *harness) tick() {
	h.t.Helper()
	if err := h.rot.Tick(context.Background(), quota.DefaultBars); err != nil {
		h.t.Fatalf("tick: %v (log: %v)", err, h.log.lines)
	}
}

// expireAccess makes an access token answer 401 while its refresh token stays valid.
func (f *fakeAnthropic) expireAccess(o creds.OAuth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tokens, o.AccessToken)
}

func (f *fakeAnthropic) callsTo(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}
