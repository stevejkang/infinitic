package credstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFile(t *testing.T) {
	dir := t.TempDir()
	exercise(t, File{PrimaryPath: filepath.Join(dir, "claude", ".credentials.json"), PoolDir: filepath.Join(dir, "pool")})

}

func TestFileWritesOwnerOnly(t *testing.T) {
	f := File{PrimaryPath: filepath.Join(t.TempDir(), "claude", ".credentials.json")}
	if err := f.Write(f.Primary(), "{}"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.PrimaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode is %v, want 0600", info.Mode().Perm())
	}

	if err := os.Chmod(f.PrimaryPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Write(f.Primary(), `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(f.PrimaryPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten world-readable credential file mode is %v, want 0600", info.Mode().Perm())
	}
}

func TestKeychainRefusesSecretsThatWouldNeedArgv(t *testing.T) {
	err := addGenericPassword("svc", "acct", strings.Repeat("x", interactiveLineLimit))
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("got %v, want errTooLarge (and no security(1) call)", err)
	}
}

func TestRedactHidesTheSecret(t *testing.T) {
	if got := redact("add-generic-password -w 'tok-123': error", "tok-123"); strings.Contains(got, "tok-123") {
		t.Fatalf("secret leaked: %q", got)
	}
}

// TestKeychain writes throwaway items into the login keychain, so it only runs when asked:
// INFINITIC_TEST_KEYCHAIN=1 go test ./internal/credstore   (run it inside a macOS VM)
func TestKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("INFINITIC_TEST_KEYCHAIN") != "1" {
		t.Skip("set INFINITIC_TEST_KEYCHAIN=1 on macOS to run")
	}
	tag := fmt.Sprintf("infinitic-test-%d", os.Getpid())
	k := Keychain{PrimaryService: tag + "-primary", PoolPrefix: tag + "-pool", Account: "infinitic-test"}
	t.Cleanup(func() {
		_ = k.Delete(k.Primary())
		_ = k.Delete(k.Pool("acct-1234"))
	})
	exercise(t, k)

	// Regression: an existing item under another account name must be updated, not duplicated.
	if _, stderr, code := runSecurity(nil, "add-generic-password", "-U", "-a", "someone-else", "-s", k.PrimaryService, "-w", "old"); code != 0 {
		t.Fatalf("seeding item: %s", stderr)
	}
	if err := k.Write(k.Primary(), `{"v":"new"}`); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := k.Read(k.Primary()); got != `{"v":"new"}` {
		t.Fatalf("read %q after updating an item owned by another account name", got)
	}
}

func exercise(t *testing.T, s Store) {
	t.Helper()
	for _, slot := range []Slot{s.Primary(), s.Pool("acct-1234")} {
		if _, ok, err := s.Read(slot); err != nil || ok {
			t.Fatalf("%s: empty slot read ok=%v err=%v", slot.Label, ok, err)
		}
		secret := `{"claudeAiOauth":{"accessToken":"it's","refreshToken":"r\"t","expiresAt":1}}`
		if err := s.Write(slot, secret); err != nil {
			t.Fatalf("%s: write: %v", slot.Label, err)
		}
		if got, ok, err := s.Read(slot); err != nil || !ok || got != secret {
			t.Fatalf("%s: read back %q ok=%v err=%v", slot.Label, got, ok, err)
		}
		if err := s.Delete(slot); err != nil {
			t.Fatalf("%s: delete: %v", slot.Label, err)
		}
		if err := s.Delete(slot); err != nil {
			t.Fatalf("%s: deleting a missing slot must not fail: %v", slot.Label, err)
		}
	}
}

func TestIsolatedSlotMatchesClaudeNaming(t *testing.T) {
	// claude names the keychain item for CLAUDE_CONFIG_DIR=/Users/admin/.lane-projB like this
	// (verified in the macOS VM during the PoC).
	got := Keychain{PrimaryService: "Claude Code-credentials"}.Isolated("/Users/admin/.lane-projB").key
	if got != "Claude Code-credentials-f4321433" {
		t.Fatalf("isolated service %q", got)
	}
	if !strings.HasSuffix(File{}.Isolated("/tmp/x").key, "/tmp/x/.credentials.json") {
		t.Fatal("file isolated slot is not <dir>/.credentials.json")
	}
}
