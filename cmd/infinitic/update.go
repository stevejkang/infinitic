package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stevejkang/infinitic/internal/update"
)

const installScript = "https://raw.githubusercontent.com/stevejkang/infinitic/main/install.sh"

type updater struct {
	src  update.Source
	exe  string // the running binary; "" for os.Executable
	brew string
}

// INFINITIC_RELEASE_API and INFINITIC_RELEASE_DOWNLOAD point updates at a fake release server.
func newUpdater() updater {
	src := update.GitHub()
	src.API = cmp.Or(os.Getenv("INFINITIC_RELEASE_API"), src.API)
	src.Download = cmp.Or(os.Getenv("INFINITIC_RELEASE_DOWNLOAD"), src.Download)
	return updater{src: src, brew: cmp.Or(os.Getenv("INFINITIC_BREW_BIN"), "brew")}
}

func (u updater) executable() (string, error) {
	if u.exe != "" {
		return u.exe, nil
	}
	return os.Executable()
}

func (a *app) update() *cobra.Command {
	return &cobra.Command{
		Use:     "update [<VERSION>]",
		Aliases: []string{"upgrade"},
		Short:   "Install the latest or a given release",
		Long: `Installs the latest release (prereleases are never latest), or the version given, which may be
a prerelease such as 0.5.0-rc1. A Homebrew install is upgraded with brew, which only has the
latest release. A running background watch is restarted on the new binary with the same settings.`,
		Args:    cobra.MaximumNArgs(1),
		Example: "infinitic update\ninfinitic update 0.5.0-rc1",
		RunE:    func(cmd *cobra.Command, args []string) error { return a.runUpdate(cmd.Context(), first(args)) },
	}
}

func (a *app) runUpdate(ctx context.Context, want string) error {
	if want != "" {
		if !update.Valid(want) {
			return fmt.Errorf("%q is not a version like 0.5.0 or 0.5.0-rc1", want)
		}
		want = update.Trim(want)
	}
	exe, err := a.upd.executable()
	if err != nil {
		return err
	}
	if update.Homebrew(exe) {
		return a.brewUpgrade(ctx, want)
	}
	if !update.Valid(version) {
		return fmt.Errorf("this is a development build (%s); rebuild it, or install a release with %s", version, installScript)
	}
	target := want
	if target == "" {
		if target, err = a.upd.src.Latest(ctx); err != nil {
			return err
		}
	}
	if target == version {
		a.p.Info("infinitic " + version + " is already installed")
		return nil
	}
	a.p.Info("downloading infinitic " + target)
	if err := a.upd.src.Install(ctx, target, exe); err != nil {
		return err
	}
	a.p.Success(fmt.Sprintf("updated infinitic %s -> %s", version, target))
	return a.restartWatch(ctx, target)
}

func (a *app) brewUpgrade(ctx context.Context, want string) error {
	if want != "" {
		return failErr("managed_by_homebrew", fmt.Sprintf("Can't install %s. This install is managed by Homebrew, which has only the latest release.", want),
			step("Remove the Homebrew install", "brew uninstall infinitic"),
			step("Install the version you want", fmt.Sprintf("curl -fsSL %s | INFINITIC_VERSION=%s sh", installScript, want)))
	}
	brew := exec.CommandContext(ctx, a.upd.brew, "upgrade", "--cask", "stevejkang/tap/infinitic")
	brew.Stdin, brew.Stdout, brew.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := brew.Run(); err != nil {
		return fmt.Errorf("brew upgrade: %w", err)
	}
	out, err := exec.CommandContext(ctx, a.upd.brew, "list", "--cask", "--versions", "infinitic").Output()
	fields := strings.Fields(string(out))
	if err != nil || len(fields) < 2 {
		return fmt.Errorf("brew upgraded infinitic, but its version could not be read: %v", err)
	}
	installed := fields[len(fields)-1]
	if installed == version {
		a.p.Info("infinitic " + version + " is already the latest")
		return nil
	}
	a.p.Success(fmt.Sprintf("updated infinitic %s -> %s", version, installed))
	return a.restartWatch(ctx, installed)
}

// restartWatch restarts a running background watch as it is registered, so it keeps its
// settings file and the flags it was started with, and waits for the new binary to report.
func (a *app) restartWatch(ctx context.Context, installed string) error {
	mgr, err := a.manager()
	if err != nil {
		return nil // no service manager on this OS, so no background watch to restart
	}
	before, err := mgr.Status()
	if err != nil || !before.Running {
		return err
	}
	if err := mgr.Restart(); err != nil {
		return failErr("restart_failed", sentence("Couldn't restart the background watch: "+err.Error()), step("Start it again", "infinitic service start"))
	}
	home := infiniticHome()
	deadline := time.Now().Add(a.wait)
	for {
		st, err := mgr.Status()
		if err != nil {
			return err
		}
		if rec, ok := readWatch(home, st.PID); st.Running && st.PID != before.PID && ok {
			if rec.Version != installed {
				a.p.Warn(fmt.Sprintf("The background watch restarted but still runs %s (%s).", rec.Version, rec.Executable))
				a.p.Steps(step("Restart it on the new binary", "infinitic service start"))
				return nil
			}
			a.p.Info(fmt.Sprintf("Restarted the background watch on %s (pid %d) with the same settings: %s.", installed, st.PID, rec.Settings.describe()))
			return nil
		}
		if !time.Now().Before(deadline) {
			a.p.Warn("Restarted the background watch, but it hasn't reported yet.")
			a.p.Steps(step("Check it", "infinitic service status"))
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

const (
	updateCheckEvery   = 24 * time.Hour
	updateCheckTimeout = 2 * time.Second
)

// updateCheck tells, after a command, that a newer release is out. It reads a cached answer;
// once a day it also asks GitHub alongside the command, and keeps the answer only if it came
// back by the time the command finished, so it never makes a command slower.
type updateCheck struct {
	cache  update.Cache
	latest string
	fresh  chan string // nil when the cache is recent enough
}

func (a *app) startUpdateCheck(cmd *cobra.Command) {
	if a.wantsUpdateNotice(cmd) {
		a.check = a.newUpdateCheck()
	}
}

func (a *app) newUpdateCheck() *updateCheck {
	c := &updateCheck{cache: update.Cache{Path: filepath.Join(infiniticHome(), "update.json")}}
	latest, checkedAt := c.cache.Load()
	c.latest = latest
	if time.Since(checkedAt) >= updateCheckEvery {
		c.fresh = make(chan string, 1)
		src := a.upd.src
		src.Client = &http.Client{Timeout: updateCheckTimeout}
		go func() {
			v, err := src.Latest(context.Background())
			if err == nil || errors.Is(err, update.ErrNoRelease) {
				c.fresh <- v
			}
		}()
	}
	return c
}

func (a *app) notifyUpdate() {
	c := a.check
	if c == nil {
		return
	}
	select {
	case v := <-c.fresh:
		c.latest = v
		_ = c.cache.Save(v, time.Now())
	default:
	}
	if update.Newer(c.latest, version) {
		a.p.Info(fmt.Sprintf("infinitic %s is available. You have %s.", c.latest, version))
		a.p.Steps(step("Update", "infinitic update"))
	}
}

// wantsUpdateNotice: only a person at a terminal, on a release build, and never where the output
// is read by a program or the command promised not to use the network.
func (a *app) wantsUpdateNotice(cmd *cobra.Command) bool {
	if !update.Valid(version) || os.Getenv("INFINITIC_NO_UPDATE_CHECK") != "" || !a.p.Terminal() {
		return false
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "watch", "logs", "update", "completion", "help", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return false
		}
	}
	for _, flag := range []string{"cached", "json"} {
		if on, err := cmd.Flags().GetBool(flag); err == nil && on {
			return false
		}
	}
	return true
}
