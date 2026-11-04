// Command infinitic rotates the primary Claude Code login across a pool of subscription accounts,
// so claude and every tool that follows its login move with it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stevejkang/infinitic/internal/logfile"
	"github.com/stevejkang/infinitic/internal/quota"
	"github.com/stevejkang/infinitic/internal/rotator"
	"github.com/stevejkang/infinitic/internal/service"
	"github.com/stevejkang/infinitic/internal/snapshot"
	"github.com/stevejkang/infinitic/internal/state"
	"github.com/stevejkang/infinitic/internal/ui"
)

var version = "dev" // set by -ldflags at release time

func init() { cobra.EnableCommandSorting = false } // help lists commands in the order they are added

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	a := newApp(ui.NewPrinter(os.Stdout))
	cmd, err := a.root().ExecuteContextC(ctx)
	switch {
	case errors.Is(err, context.Canceled):
		os.Exit(130)
	case err != nil:
		os.Exit(report(err, wantsJSON(cmd)))
	default:
		a.notifyUpdate()
	}
}

// test entry point
func run(ctx context.Context, p *ui.Printer, args []string) error {
	root := newApp(p).root()
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func validBars(b quota.Bars) error {
	for _, bar := range []struct {
		name  string
		value float64
	}{{"--session", b.Session}, {"--weekly", b.Weekly}} {
		if bar.value <= 0 || bar.value > 100 {
			return usageErr("invalid_arguments", fmt.Sprintf("Can't use %s %g. A bar is above 0 and at most 100.", bar.name, bar.value))
		}
	}
	return nil
}

func barFlags(fs *pflag.FlagSet) *quota.Bars {
	bars := quota.DefaultBars
	fs.Float64Var(&bars.Session, "session", bars.Session, "5-hour usage bar, in percent")
	fs.Float64Var(&bars.Weekly, "weekly", bars.Weekly, "7-day usage bar, in percent")
	hint(fs, "session", "PERCENT")
	hint(fs, "weekly", "PERCENT")
	return &bars
}

type statusOptions struct{ refresh, cached, json bool }

func cmdStatus(ctx context.Context, cmd *cobra.Command, p *ui.Printer, o statusOptions, flagBars quota.Bars) error {
	bars, source, err := barsFromSettings(cmd.Flags(), flagBars)
	if err != nil {
		return err
	}
	if o.json {
		p = ui.NewPrinter(os.Stderr) // stdout carries only the JSON document
	}
	svc := backgroundWatch()
	footer := statusFooter(bars, source, svc)
	cfg := loadConfig()
	if o.cached {
		st, err := state.Load(cfg.statePath())
		if err != nil {
			return err
		}
		if o.json {
			return printJSON(snapshot.Build(st, bars, snapshot.SourceCached, svc, time.Now()))
		}
		p.Status(st, bars, footer...)
		return nil
	}
	live := !o.json && p.CanLive()
	var held []string // what the checks report goes under the panel, not above it
	check := func(ctx context.Context, p *ui.Printer) error {
		return cfg.locked(ctx, p, func(r *rotator.Rotator) error {
			p.Hold(&held)
			pin := func() { p.Pin(ui.Render(r.State, bars, p.Width(), footer...)) }
			pin()
			active, err := r.SyncActive(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return err
				}
				p.Warn("could not read the primary login: " + err.Error())
			}
			if active != nil {
				p.Busy("checking " + active.Account.Label())
				r.CheckActive(ctx, active, o.refresh)
			}
			for _, a := range r.State.Accounts {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if a.UUID == r.State.ActiveUUID {
					continue
				}
				pin()
				p.Busy("checking " + a.Label())
				r.CheckParked(ctx, a, o.refresh)
			}
			p.Busy("")
			if o.json {
				return printJSON(snapshot.Build(r.State, bars, snapshot.SourceLive, svc, time.Now()))
			}
			if live {
				pin()
			} else {
				p.Status(r.State, bars, footer...)
			}
			return nil
		})
	}
	if !live {
		err := check(ctx, p)
		p.Release(held)
		return err
	}
	err = p.RunLive(ctx, "reading usage", check)
	p.Shown()
	p.Release(held)
	if errors.Is(err, context.Canceled) {
		return nil // stopped from the keyboard: the panel so far stays on screen
	}
	return err
}

func statusFooter(bars quota.Bars, source string, svc *service.Status) []ui.Fact {
	out := []ui.Fact{barsFact(bars, source)}
	switch {
	case svc == nil:
	case svc.Running:
		out = append(out, ui.Fact{Label: "watch", Value: fmt.Sprintf("running in the background (pid %d)", svc.PID)})
	case svc.Installed:
		out = append(out, ui.Fact{Label: "watch", Value: "installed but not running", Note: "infinitic service status"})
	default:
		out = append(out, ui.Fact{Label: "watch", Value: "not installed", Note: "infinitic service start"})
	}
	return out
}

func barsFact(bars quota.Bars, note string) ui.Fact {
	return ui.Fact{Label: "bars", Value: fmt.Sprintf("5h %g%% · 7d %g%%", bars.Session, bars.Weekly), Note: note}
}

// barsFromSettings is the bars a command runs with: the settings file's, as watch uses them,
// unless flags override them. An unreadable file is reported, not ignored.
func barsFromSettings(fs *pflag.FlagSet, flags quota.Bars) (quota.Bars, string, error) {
	path := settingsPath(infiniticHome())
	file, exists, err := loadSettings(path)
	if err != nil {
		return flags, "", err
	}
	bars := quota.Bars{Session: file.Session, Weekly: file.Weekly}
	if fs.Changed("session") {
		bars.Session = flags.Session
	}
	if fs.Changed("weekly") {
		bars.Weekly = flags.Weekly
	}
	if err := validBars(bars); err != nil {
		return bars, "", fromFile(err, exists, path)
	}
	if file.Interval < minInterval {
		return bars, "", failErr("invalid_config", fmt.Sprintf("%s: interval %s is too short. The minimum is %s.", path, shortDuration(file.Interval), shortDuration(minInterval)))
	}
	return bars, barsSource(fs, exists, path), nil
}

// barsSource says where the bars came from, the same way in the status and watch footers.
func barsSource(fs *pflag.FlagSet, exists bool, path string) string {
	fromFlags := fs.Changed("session") || fs.Changed("weekly")
	switch {
	case fromFlags && exists:
		return "from flags and " + path
	case fromFlags:
		return "from flags"
	case exists:
		return "from " + path
	default:
		return "defaults (no " + path + ")"
	}
}

func printJSON(v any) error { return ui.WriteJSON(os.Stdout, v) }

// nil when it cannot be determined
func backgroundWatch() *service.Status {
	mgr, err := service.ForCurrentOS()
	if err != nil {
		return nil
	}
	st, err := mgr.Status()
	if err != nil {
		return nil
	}
	return &st
}

func cmdImport(ctx context.Context, p *ui.Printer) error {
	return loadConfig().locked(ctx, p, func(r *rotator.Rotator) error {
		active, err := r.SyncActive(ctx)
		if err != nil {
			return err
		}
		if active == nil {
			return failErr("not_logged_in", "Can't import. Claude Code isn't logged in.",
				step("Log in with /login in Claude Code, then run infinitic import again", "claude"))
		}
		p.Receipt("Imported", active.Account.Label(), ui.Fact{Label: "Plan", Value: active.Account.Plan})
		return nil
	})
}

// An empty selector means the best account.
func cmdSwitch(ctx context.Context, p *ui.Printer, selector string, bars quota.Bars) error {
	if err := validBars(bars); err != nil {
		return err
	}
	return loadConfig().locked(ctx, p, func(r *rotator.Rotator) error {
		// The target is resolved before anything is read or written: a wrong one changes nothing.
		var target *state.Account
		if selector != "" {
			var err error
			if target, err = resolveAccount(r.State, selector, "infinitic switch %s"); err != nil {
				return err
			}
		}
		var opts rotator.SwitchOptions
		_, err := r.SyncActive(ctx)
		var dead *rotator.DeadPrimaryError
		switch {
		case errors.As(err, &dead):
			p.Warn("The current primary login is dead (" + dead.Error() + "). It will be replaced, not parked.")
			opts.ReplaceDead = dead
		case err != nil:
			return err
		}

		if target == nil {
			r.CheckAllParked(ctx, true)
			switched, err := r.SwitchToBest(ctx, bars, opts)
			if err == nil && switched == nil {
				return failErr("no_candidate", "Can't switch. No parked account is under the bars and healthy.",
					step("See usage per account", "infinitic status"))
			}
			return err
		}
		if target.UUID == r.State.ActiveUUID && dead == nil {
			p.Info(target.Label() + " is already the active login.")
			return nil
		}
		if !target.Healthy() {
			p.Warn(fmt.Sprintf("%s needs a new login (%s). Switching anyway.", target.Label(), target.NeedsReauth))
			defer p.Steps(step("Renew its login", "infinitic reauth "+target.Label()))
		}
		return r.Switch(ctx, target, opts)
	})
}

// watchFlags are watch's flags; `service start` takes the settings ones and passes them on.
type watchFlags struct {
	interval *time.Duration
	once     *bool
	logFile  *string
	config   *string
	bars     *quota.Bars
}

func addWatchFlags(fs *pflag.FlagSet) watchFlags {
	f := watchFlags{
		interval: fs.Duration("interval", defaultSettings.Interval, "Time between checks, such as 5m; the minimum is 30s"),
		once:     fs.Bool("once", false, "Run a single check and exit"),
		logFile:  fs.String("log-file", "", "Write structured log records to this file, rotated by size"),
		config:   fs.String("config", settingsPath(infiniticHome()), "Settings file; options override it"),
		bars:     barFlags(fs),
	}
	hint(fs, "interval", "DURATION")
	hint(fs, "log-file", "FILE")
	hint(fs, "config", "FILE")
	return f
}

const minInterval = 30 * time.Second

func (f watchFlags) validate() error {
	if *f.interval < minInterval {
		return usageErr("invalid_arguments", fmt.Sprintf("Can't use --interval %s. The minimum is %s, since every check calls the usage API, which throttles frequent polling.", shortDuration(*f.interval), shortDuration(minInterval)))
	}
	return validBars(*f.bars)
}

func cmdWatch(ctx context.Context, p *ui.Printer, fs *pflag.FlagSet, f watchFlags) error {
	file, exists, err := loadSettings(*f.config)
	if err != nil {
		return err
	}
	if !exists && fs.Changed("config") {
		return failErr("not_found", fmt.Sprintf("Couldn't find settings file %s.", *f.config),
			step("Write it from the defaults", "infinitic service start --config "+*f.config))
	}
	file.overriddenBy(fs, f).apply(f)
	if err := f.validate(); err != nil {
		return fromFile(err, exists, *f.config)
	}
	p, closeLog, err := watchPrinter(p, *f.logFile)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	live := *f.logFile == "" && p.CanLive()
	p.Timestamps = true // a watch runs for hours: each line says when it happened

	cfg := loadConfig()
	if remove, err := recordStart(cfg.home, *f.config, file, f); err != nil {
		p.Warn("could not record the settings this watch runs with: " + err.Error())
	} else {
		defer remove()
	}
	p.Info(fmt.Sprintf("Watching: 5h bar %g%%, 7d bar %g%%, every %s", f.bars.Session, f.bars.Weekly, shortDuration(*f.interval)),
		"event", "start", "version", version, "session_bar", f.bars.Session, "weekly_bar", f.bars.Weekly, "interval", *f.interval)

	footer := []ui.Fact{barsFact(*f.bars, barsSource(fs, exists, *f.config)), {Label: "every", Value: shortDuration(*f.interval)}}
	loop := func(ctx context.Context, p *ui.Printer) error {
		if st, err := state.Load(cfg.statePath()); err == nil {
			p.Pin(ui.Render(st, *f.bars, p.Width(), footer...))
		}
		for {
			err := cfg.locked(ctx, p, func(r *rotator.Rotator) error {
				err := r.Tick(ctx, *f.bars)
				p.Pin(ui.Render(r.State, *f.bars, p.Width(), footer...)) // after the tick: show what it did
				return err
			})
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if err != nil {
				p.Error("The check failed: "+err.Error(), "event", "tick_failed", "error", err)
			}
			if *f.once {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(*f.interval):
			}
		}
	}
	if live {
		return p.RunLive(ctx, "watching", loop)
	}
	return loop(ctx, p)
}

// fromFile points a validation error at the settings file too, when values may have come from it.
func fromFile(err error, exists bool, path string) error {
	if !exists {
		return err
	}
	ce := *asCLIError(err)
	ce.details = append(slices.Clone(ce.details), "Set by an option or in "+path+".")
	return &ce
}

// recordStart writes this watch's running record (see runningWatch).
func recordStart(home, config string, file settings, f watchFlags) (func(), error) {
	exe, err := os.Executable()
	if err != nil {
		return func() {}, err
	}
	if config, err = filepath.Abs(config); err != nil {
		return func() {}, err
	}
	s := settings{Interval: *f.interval, Session: f.bars.Session, Weekly: f.bars.Weekly}
	return recordWatch(home, runningWatch{PID: os.Getpid(), StartedAt: time.Now(), Version: version, Executable: exe, Config: config, Settings: s, File: &file})
}

func watchPrinter(terminal *ui.Printer, logPath string) (*ui.Printer, func() error, error) {
	noop := func() error { return nil }
	switch {
	case logPath != "":
		file, err := logfile.Open(logPath)
		if err != nil {
			return nil, nil, err
		}
		return ui.NewLogPrinter(file), file.Close, nil
	case !terminal.Terminal():
		return ui.NewLogPrinter(os.Stdout), noop, nil
	default:
		return terminal, noop, nil
	}
}

func cmdRemove(ctx context.Context, p *ui.Printer, selector string) error {
	return loadConfig().locked(ctx, p, func(r *rotator.Rotator) error {
		a, err := resolveAccount(r.State, selector, "infinitic rm %s")
		if err != nil {
			return err
		}
		if a.UUID == r.State.ActiveUUID {
			return activeRemoveErr(a)
		}
		if err := r.Forget(a); err != nil {
			return err
		}
		p.Receipt("Removed", a.Label())
		return nil
	})
}
