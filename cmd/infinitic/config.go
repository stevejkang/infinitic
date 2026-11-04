package main

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
	"github.com/stevejkang/infinitic/internal/credstore"
	"github.com/stevejkang/infinitic/internal/lock"
	"github.com/stevejkang/infinitic/internal/rotator"
	"github.com/stevejkang/infinitic/internal/state"
	"github.com/stevejkang/infinitic/internal/ui"
)

// config is resolved from the environment. Every path and endpoint is overridable so the
// whole tool can be exercised against a fake server and throwaway credential slots.
type config struct {
	home       string // INFINITIC_HOME
	claudeBin  string // INFINITIC_CLAUDE_BIN
	claudeJSON string // INFINITIC_CLAUDE_JSON
	store      credstore.Store
	api        *anthropic.Client
}

// infiniticHome holds state.json, config.yml and, with the file backend, the pool.
func infiniticHome() string {
	userHome, _ := os.UserHomeDir()
	return cmp.Or(os.Getenv("INFINITIC_HOME"), filepath.Join(userHome, ".config", "infinitic"))
}

func loadConfig() config {
	userHome, _ := os.UserHomeDir()
	home := infiniticHome()
	claudeBin := cmp.Or(os.Getenv("INFINITIC_CLAUDE_BIN"), "claude")
	return config{
		home:       home,
		claudeBin:  claudeBin,
		claudeJSON: cmp.Or(os.Getenv("INFINITIC_CLAUDE_JSON"), filepath.Join(userHome, ".claude.json")),
		store:      newStore(home, userHome),
		api: &anthropic.Client{
			APIBase:   cmp.Or(os.Getenv("INFINITIC_API_BASE"), "https://api.anthropic.com"),
			TokenURL:  cmp.Or(os.Getenv("INFINITIC_TOKEN_URL"), "https://claude.ai/v1/oauth/token"),
			UserAgent: sync.OnceValue(func() string { return claudeUserAgent(claudeBin) }),
			HTTP:      debugClient(&http.Client{Timeout: 15 * time.Second}),
		},
	}
}

// newStore picks the backend claude itself uses: the keychain on macOS, files on Linux.
// INFINITIC_BACKEND=file forces files on macOS (tests).
func newStore(home, userHome string) credstore.Store {
	if runtime.GOOS == "darwin" && os.Getenv("INFINITIC_BACKEND") != "file" {
		return credstore.Keychain{
			PrimaryService: cmp.Or(os.Getenv("INFINITIC_PRIMARY_SERVICE"), "Claude Code-credentials"),
			PoolPrefix:     cmp.Or(os.Getenv("INFINITIC_POOL_PREFIX"), "infinitic-pool"),
			Account:        cmp.Or(os.Getenv("USER"), "infinitic"),
		}
	}
	claudeDir := cmp.Or(os.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join(userHome, ".claude"))
	return credstore.File{
		PrimaryPath: cmp.Or(os.Getenv("INFINITIC_PRIMARY_FILE"), filepath.Join(claudeDir, ".credentials.json")),
		PoolDir:     filepath.Join(home, "pool"),
	}
}

// claudeUserAgent mirrors claude's own User-Agent. It runs `claude --version` at most once, and
// only when a request is actually sent, so `status --cached` never spawns claude.
func claudeUserAgent(claudeBin string) string {
	version := "2.1.0"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, claudeBin, "--version").Output(); err == nil {
		if fields := strings.Fields(string(out)); len(fields) > 0 {
			version = fields[0]
		}
	}
	return "claude-cli/" + version + " (external, cli)"
}

func (c config) statePath() string { return filepath.Join(c.home, "state.json") }

// locked runs fn while holding infinitic's lock, with state loaded inside the lock and saved
// afterwards, so a background watch and a command run by hand never overwrite each other. State
// is saved even when fn fails: a partial check still learned something, and a failing fn must
// undo what it does not want kept (see StoreLogin).
func (c config) locked(ctx context.Context, p *ui.Printer, fn func(*rotator.Rotator) error) error {
	release, err := lock.Acquire(ctx, filepath.Join(c.home, "lock"), func() {
		p.Info("waiting for another infinitic process (the background watch?) to finish its check")
	})
	if err != nil {
		return err
	}
	defer release()

	st, err := state.Load(c.statePath())
	if err != nil {
		return err
	}
	r := &rotator.Rotator{
		Store:    c.store,
		API:      c.api,
		State:    st,
		Identity: rotator.IdentityFile{Path: c.claudeJSON},
		Report:   p,
	}
	return errors.Join(fn(r), st.Save())
}
