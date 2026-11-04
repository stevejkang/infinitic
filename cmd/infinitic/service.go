package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/stevejkang/infinitic/internal/service"
	"github.com/stevejkang/infinitic/internal/ui"
)

// serviceStart (re)registers the background watch with a settings file and the current binary,
// and waits for the new watch to report what it runs with. The file is the one given with
// --config, else the one the running watch uses, else the default; a missing one is written.
//
// Settings flags that differ from an existing file lead to a question: yes (or --overwrite) stores
// them in the file, no keeps it and passes them to this watch only, until the next start. Without
// a terminal the answer is no. A file that cannot be used leaves the running service alone.
func serviceStart(ctx context.Context, p *ui.Printer, mgr service.Manager, set *pflag.FlagSet, f watchFlags, overwrite bool, ask func(string) (bool, error), wait time.Duration) error {
	before, err := mgr.Status()
	if err != nil {
		return err
	}
	home := infiniticHome()
	path := *f.config
	if rec, ok := readWatch(home, before.PID); !set.Changed("config") && before.Running && ok {
		path = rec.Config
	}
	if path, err = filepath.Abs(path); err != nil {
		return err
	}
	current, exists, err := loadSettings(path)
	broken := err != nil && exists
	if err != nil && (!broken || !settingsFlagged(set)) {
		return failErr("invalid_config", sentence(err.Error())+" The service was left as it was.",
			step("Fix it, then run this again", "infinitic service start"))
	}
	want := current.overriddenBy(set, f)
	want.apply(f)
	if err := f.validate(); err != nil {
		ce := *asCLIError(fromFile(err, exists && !broken, path))
		ce.msg += " The service was left as it was."
		return &ce
	}

	var extra []string
	switch changes := want.changes(current); {
	case !exists:
		if err := want.write(path); err != nil {
			return err
		}
		p.Receipt("Wrote", path)
	case broken:
		p.Warn("Couldn't read " + path + ": " + err.Error())
		if !overwrite {
			ok, err := ask("Replace " + path + " with the options and the defaults? Its contents are lost.")
			switch {
			case errors.Is(err, ui.ErrNoAnswer):
				return missingInput("confirmation_required", "Can't replace the unreadable settings file without confirmation in non-interactive mode. The service was left as it was.",
					step("Replace it with the options and the defaults", "infinitic service start --overwrite"))
			case err != nil:
				return err
			case !ok:
				return canceled("Canceled. The service was left as it was.")
			}
		}
		if err := want.write(path); err != nil {
			return err
		}
		p.Receipt("Replaced", path)
	case len(changes) == 0:
		p.Info("Using settings " + path)
	default:
		p.Info(path + " differs from the options:\n    " + strings.Join(changes, "\n    "))
		ok := overwrite
		if !overwrite {
			if ok, err = confirm(ask, "Overwrite "+path+"? Comments in it are replaced."); err != nil {
				return err
			}
		}
		if ok {
			if err := want.write(path); err != nil {
				return err
			}
			p.Receipt("Updated", path)
		} else {
			extra = flagArgs(set, f)
			p.Info(path + " was kept. The options apply to this background watch only.")
		}
	}

	if err := installWatch(mgr, path, extra...); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		st, err := mgr.Status()
		if err != nil {
			return err
		}
		if rec, ok := readWatch(home, st.PID); st.Running && st.PID != before.PID && ok {
			p.Receipt("Started", fmt.Sprintf("background watch (pid %d)", st.PID), ui.Fact{Label: "Settings", Value: rec.Settings.describe()})
			p.Steps(step("Follow its log", "infinitic service logs --follow"))
			return nil
		}
		if !time.Now().Before(deadline) {
			p.Warn("Started, but the new watch hasn't reported its settings yet.")
			p.Steps(step("Check it", "infinitic service status"))
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// settingsFlagged reports whether a settings flag (not --config) was given.
func settingsFlagged(set *pflag.FlagSet) bool {
	return set.Changed("interval") || set.Changed("session") || set.Changed("weekly")
}

// confirm asks a yes/no question; without a terminal to ask on, the answer is no.
func confirm(ask func(string) (bool, error), question string) (bool, error) {
	ok, err := ask(question)
	if errors.Is(err, ui.ErrNoAnswer) {
		return false, nil
	}
	return ok, err
}

// flagArgs returns the settings flags given on the command line, as watch arguments.
func flagArgs(set *pflag.FlagSet, f watchFlags) []string {
	var args []string
	set.Visit(func(fl *pflag.Flag) {
		switch fl.Name {
		case "interval":
			args = append(args, "--interval", shortDuration(*f.interval))
		case "session":
			args = append(args, "--session", formatFloat(f.bars.Session))
		case "weekly":
			args = append(args, "--weekly", formatFloat(f.bars.Weekly))
		}
	})
	return args
}

// installWatch (re)registers `watch --config path [extra]` with the service manager, which
// (re)starts it.
func installWatch(mgr service.Manager, path string, extra ...string) error {
	exe, err := service.StableExecutable()
	if err != nil {
		return err
	}
	args := append([]string{"watch", "--config", path}, extra...)
	return mgr.Install(service.Spec{Executable: exe, Args: args, PATH: os.Getenv("PATH")})
}

const startWait = 10 * time.Second

// serviceStatus shows the service as facts: whether it runs, since when, with which settings and
// binary; then, in an attention panel, what no longer matches and how to fix it. The log itself
// is `service logs`.
func serviceStatus(p *ui.Printer, mgr service.Manager, asJSON bool) error {
	st, err := mgr.Status()
	if err != nil {
		return err
	}
	doc := serviceDoc{Installed: st.Installed, Running: st.Running, Unit: mgr.UnitPath(), Logs: mgr.LogLocation(), Attention: []string{}}
	var facts []ui.Fact
	var attention []ui.Note
	switch {
	case !st.Installed:
		facts = append(facts, ui.Fact{Label: "watch", Value: "not installed", Note: "infinitic service start"})
	case st.Running:
		doc.PID = &st.PID
		facts = append(facts, ui.Fact{Label: "watch", Value: fmt.Sprintf("running (pid %d)", st.PID)})
		if rec, ok := readWatch(infiniticHome(), st.PID); ok {
			doc.StartedAt, doc.Version, doc.Executable, doc.Config = &rec.StartedAt, &rec.Version, &rec.Executable, &rec.Config
			doc.Settings = &settingsDoc{Interval: shortDuration(rec.Settings.Interval), Session: rec.Settings.Session, Weekly: rec.Settings.Weekly}
			facts = append(facts,
				ui.Fact{Label: "since", Value: rec.StartedAt.Local().Format("Jan 2 15:04")},
				ui.Fact{Label: "settings", Value: rec.Settings.describe(), Note: rec.Config},
				ui.Fact{Label: "binary", Value: rec.Executable, Note: rec.Version})
			for _, d := range rec.drift() {
				attention = append(attention, ui.Note{Subject: "watch", Reason: d, Fix: "infinitic service start"})
			}
		} else {
			attention = append(attention, ui.Note{Subject: "watch", Reason: "the settings it runs with are unknown (started by an older infinitic)", Fix: "infinitic service start"})
		}
	default:
		facts = append(facts, ui.Fact{Label: "watch", Value: "installed but not running", Note: "infinitic service start"})
	}
	if st.Note != "" {
		attention = append(attention, ui.Note{Subject: "watch", Reason: st.Note})
	}
	if asJSON {
		for _, n := range attention {
			doc.Attention = append(doc.Attention, n.Reason)
		}
		return printJSON(map[string]any{"service": doc})
	}
	facts = append(facts,
		ui.Fact{Label: "unit", Value: mgr.UnitPath()},
		ui.Fact{Label: "logs", Value: mgr.LogLocation(), Note: "infinitic service logs --follow"})
	p.Facts(facts)
	p.Attention(attention)
	return nil
}

// serviceDoc is `service status --json`. Values that are known to be absent are null.
type serviceDoc struct {
	Installed  bool         `json:"installed"`
	Running    bool         `json:"running"`
	PID        *int         `json:"pid"`
	StartedAt  *time.Time   `json:"startedAt"`
	Version    *string      `json:"version"`
	Executable *string      `json:"executable"`
	Config     *string      `json:"config"`
	Settings   *settingsDoc `json:"settings"`
	Unit       string       `json:"unit"`
	Logs       string       `json:"logs"`
	Attention  []string     `json:"attention"`
}

type settingsDoc struct {
	Interval string  `json:"interval"`
	Session  float64 `json:"session"`
	Weekly   float64 `json:"weekly"`
}

// serviceLogs prints the service log; with follow it keeps printing until ctrl+c, which ends
// it quietly like a watch.
func serviceLogs(ctx context.Context, p *ui.Printer, mgr service.Manager, lines int, follow bool) error {
	cmd, err := mgr.Logs(lines, follow)
	if err != nil {
		return err
	}
	if follow {
		p.Hint("Following " + mgr.LogLocation() + "  ·  ctrl+c to stop")
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if follow && ctx.Err() != nil {
		return nil
	}
	return err
}
