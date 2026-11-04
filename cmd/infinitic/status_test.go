package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/snapshot"
	"github.com/stevejkang/infinitic/internal/ui"
)

// setupPool writes a primary login for account "a" and a parked login for "b" into a
// throwaway file store, served by a fake API whose usage endpoint fails for "b".
func setupPool(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
		switch r.URL.Path {
		case "/api/oauth/profile":
			fmt.Fprintf(w, `{"account":{"uuid":"%s-uuid","email":"%s@example.com"},"organization":{"uuid":"org"}}`, who, who)
		case "/api/oauth/usage":
			if who == "b" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"five_hour":{"utilization":95,"resets_at":"2026-10-18T15:00:00Z"},"seven_day":{"utilization":40,"resets_at":null}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	for k, v := range map[string]string{
		"INFINITIC_HOME":         home,
		"INFINITIC_BACKEND":      "file",
		"INFINITIC_PRIMARY_FILE": filepath.Join(home, "claude", ".credentials.json"),
		"INFINITIC_CLAUDE_JSON":  filepath.Join(home, "claude.json"),
		"INFINITIC_CLAUDE_BIN":   "/nonexistent",
		"INFINITIC_API_BASE":     srv.URL,
		"INFINITIC_TOKEN_URL":    srv.URL + "/token",
	} {
		t.Setenv(k, v)
	}
	login := func(who string) string {
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"at-%s","refreshToken":"rt-%s","expiresAt":%d}}`, who, who, time.Now().Add(time.Hour).UnixMilli())
	}
	writeFile(t, filepath.Join(home, "claude", ".credentials.json"), login("a"))
	writeFile(t, filepath.Join(home, "pool", "b-uuid.json"), login("b"))
	writeFile(t, filepath.Join(home, "state.json"), `{"accounts":[{"uuid":"b-uuid","email":"b@example.com"}]}`)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// capture runs the CLI and returns what it wrote to stdout and stderr.
func capture(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	realOut, realErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	err := run(context.Background(), ui.NewPrinter(os.Stdout), args)
	os.Stdout, os.Stderr = realOut, realErr
	outW.Close()
	errW.Close()
	o, _ := io.ReadAll(outR)
	e, _ := io.ReadAll(errR)
	if err != nil {
		t.Fatalf("infinitic %v: %v\nstderr: %s", args, err, e)
	}
	return string(o), string(e)
}

func decode(t *testing.T, stdout string) snapshot.Snapshot {
	t.Helper()
	var snap snapshot.Snapshot
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		t.Fatalf("stdout is not a single snapshot document: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout has trailing content:\n%s", stdout)
	}
	return snap
}

func TestStatusJSONLiveKeepsStdoutPure(t *testing.T) {
	setupPool(t)
	stdout, stderr := capture(t, "status", "--json", "--refresh")

	snap := decode(t, stdout)
	if snap.SchemaVersion != 1 || snap.Source != snapshot.SourceLive {
		t.Fatalf("header %+v", snap)
	}
	if snap.Active == nil || snap.Active.Email != "a@example.com" {
		t.Fatalf("active %+v", snap.Active)
	}
	byEmail := map[string]snapshot.Account{}
	for _, a := range snap.Accounts {
		byEmail[a.Email] = a
	}
	if a := byEmail["a@example.com"]; !a.Active || !a.OverBar || a.Usage == nil || a.Usage.Windows[0].Label != "5h" || a.Usage.Windows[0].Percent != 95 {
		t.Errorf("a %+v", a)
	}
	if b := byEmail["b@example.com"]; b.Health != snapshot.HealthDegraded || !strings.Contains(b.Problem, "503") {
		t.Errorf("b %+v", b)
	}
	if !strings.Contains(stderr, "checking b@example.com failed") {
		t.Errorf("the warning should go to stderr, got stderr=%q", stderr)
	}
	if strings.Contains(stdout, "at-a") || strings.Contains(stdout, "rt-") {
		t.Error("a token leaked into the snapshot")
	}
}

func TestStatusJSONCachedNeedsNoNetwork(t *testing.T) {
	setupPool(t)
	capture(t, "status", "--json", "--refresh") // store a sample
	t.Setenv("INFINITIC_API_BASE", "http://127.0.0.1:1")
	t.Setenv("INFINITIC_PRIMARY_FILE", filepath.Join(t.TempDir(), "missing.json"))

	stdout, stderr := capture(t, "status", "--json", "--cached")
	snap := decode(t, stdout)
	if snap.Source != snapshot.SourceCached || snap.Active == nil || snap.Active.Email != "a@example.com" {
		t.Fatalf("cached snapshot %+v", snap)
	}
	if stderr != "" {
		t.Fatalf("--cached touched something: %s", stderr)
	}
}
