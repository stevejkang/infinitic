package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/stevejkang/infinitic/internal/creds"
	"github.com/stevejkang/infinitic/internal/rotator"
	"github.com/stevejkang/infinitic/internal/state"
	"github.com/stevejkang/infinitic/internal/ui"
)

// cmdReauth runs claude in a throwaway config dir so /login there never touches the primary login,
// then stores the new grant where its account lives (see Rotator.StoreLogin). With a selector, a
// login as any other account changes nothing. `add` is the same command without one.
func cmdReauth(ctx context.Context, p *ui.Printer, selector string) error {
	cfg := loadConfig()
	var want *state.Account
	if selector != "" {
		st, err := state.Load(cfg.statePath())
		if err != nil {
			return err
		}
		if want, err = resolveAccount(st, selector, "infinitic reauth %s"); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp("", "infinitic-login-")
	if err != nil {
		return err
	}
	// macOS temp dirs live behind the /var -> /private/var symlink. Use the resolved path so the
	// keychain item name infinitic derives is the one claude derives from the same string.
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return err
	}
	defer func() {
		_ = cfg.store.Delete(cfg.store.Isolated(dir))
		_ = os.RemoveAll(dir)
	}()

	who := "the account to add or renew"
	if want != nil {
		who = want.Label()
	}
	p.Info("An isolated claude session opens. Run /login as " + who + "; it closes itself once you're in.")
	p.Info("Tip: a browser already signed in to claude.ai logs in as that account; use an incognito window to pick another.")
	p.Hint("/exit or ctrl+c to give up")
	raw, err := loginIsolated(ctx, cfg, dir)
	if err != nil {
		return err
	}
	blob, err := creds.Parse(raw)
	if err != nil {
		return err
	}
	id, err := cfg.api.Profile(ctx, blob.OAuth.AccessToken)
	if err != nil {
		return err
	}
	if want != nil && id.AccountUUID != want.UUID {
		return failErr("wrong_account", fmt.Sprintf("Logged in as %s, not %s. Nothing changed.", cmp.Or(id.Email, id.AccountUUID), want.Label()),
			step("Run it again and log in as "+want.Label(), "infinitic reauth "+want.Label()))
	}

	// The login can take minutes, so only the bookkeeping runs under the lock.
	return cfg.locked(ctx, p, func(r *rotator.Rotator) error {
		_, err := r.SyncActive(ctx)
		var dead *rotator.DeadPrimaryError
		known := err == nil || errors.As(err, &dead)
		if !known {
			p.Warn("Couldn't read the primary login: " + err.Error())
		}
		account, target, err := r.StoreLogin(id, blob.OAuth, known)
		if err != nil {
			return fmt.Errorf("%s: %w. Nothing changed", account.Label(), err)
		}
		if oa := (rotator.IdentityFile{Path: filepath.Join(dir, ".claude.json")}).OAuthAccountFor(id.AccountUUID); oa != nil {
			account.OAuthAccount = oa
		}
		outcome := map[rotator.LoginTarget][2]string{
			rotator.LoginAdded:   {"Added", "new account"},
			rotator.LoginParked:  {"Renewed", "parked login"},
			rotator.LoginPrimary: {"Renewed", "active login"},
		}[target]
		rows := []ui.Fact{{Label: "Login", Value: outcome[1]}, {Label: "Plan", Value: account.Plan}}
		if account.LoginExpiresAt != nil {
			rows = append(rows, ui.Fact{Label: "Valid until", Value: account.LoginExpiresAt.Local().Format("2006-01-02 15:04 MST")})
		}
		p.Receipt(outcome[0], account.Label(), rows...)
		return nil
	})
}

const (
	identityWait = 10 * time.Second // claude writes .claude.json shortly after the credential
	exitWait     = 5 * time.Second  // time claude gets to exit cleanly before it is killed
)

// loginIsolated runs claude with CLAUDE_CONFIG_DIR=dir until a login lands in its credential slot.
func loginIsolated(ctx context.Context, cfg config, dir string) (string, error) {
	restoreTTY := saveTTY()
	defer restoreTTY()

	slot := cfg.store.Isolated(dir)
	cmd := exec.Command(cfg.claudeBin)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(withoutEnv(os.Environ(), credentialEnv...), "CLAUDE_CONFIG_DIR="+dir)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting %s: %w", cfg.claudeBin, err)
	}
	exited := make(chan struct{}) // claude's exit status says nothing: the login slot decides
	go func() { _ = cmd.Wait(); close(exited) }()

	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	var loggedInAt time.Time
	for {
		select {
		case <-ctx.Done(): // ctrl+c: giving up is an outcome, not a failure
			stop(cmd, exited)
			return "", canceled("No login happened. Nothing changed.")
		case <-exited:
			return readLogin(cfg, dir)
		case <-poll.C:
			if loggedInAt.IsZero() {
				if _, ok, _ := cfg.store.Read(slot); ok {
					loggedInAt = time.Now()
				}
				continue
			}
			if hasIdentity(dir) || time.Since(loggedInAt) > identityWait {
				stop(cmd, exited)
				return readLogin(cfg, dir)
			}
		}
	}
}

// stop asks claude to exit (SIGTERM lets it restore the terminal), killing it only if it hangs.
func stop(cmd *exec.Cmd, exited <-chan struct{}) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(exitWait):
		_ = cmd.Process.Kill()
		<-exited
	}
}

func hasIdentity(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	return err == nil && strings.Contains(string(data), `"oauthAccount"`)
}

// saveTTY records the terminal mode, so a TUI that dies mid-screen cannot leave it raw.
func saveTTY() (restore func()) {
	get := exec.Command("stty", "-g")
	get.Stdin = os.Stdin
	mode, err := get.Output()
	if err != nil {
		return func() {}
	}
	return func() {
		set := exec.Command("stty", strings.TrimSpace(string(mode)))
		set.Stdin = os.Stdin
		_ = set.Run()
		fmt.Print("\x1b[?1049l\x1b[?25h") // leave the alternate screen, show the cursor
	}
}

func readLogin(cfg config, dir string) (string, error) {
	raw, ok, err := cfg.store.Read(cfg.store.Isolated(dir))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", canceled("No login happened. Nothing changed.")
	}
	return raw, nil
}

// credentialEnv are variables that make claude use some other credential or backend than the
// login it is asked to create, so the isolated session must not inherit them.
var credentialEnv = []string{
	"CLAUDE_CONFIG_DIR",
	"CLAUDE_SECURESTORAGE_CONFIG_DIR",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
	"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	"CLAUDE_CODE_SUBSCRIPTION_TYPE",
	"CLAUDE_CODE_RATE_LIMIT_TIER",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
}

func withoutEnv(env []string, keys ...string) []string {
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(keys, name) {
			out = append(out, kv)
		}
	}
	return out
}
