package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/service"
	"github.com/stevejkang/infinitic/internal/ui"
)

func TestRunningRecordRoundTripAndPruning(t *testing.T) {
	home := t.TempDir()
	const dead = 999_999_999 // no such process
	if _, err := recordWatch(home, runningWatch{PID: dead}); err != nil {
		t.Fatal(err)
	}
	rec := runningWatch{PID: os.Getpid(), StartedAt: time.Now().Truncate(time.Second), Config: "/c.yml", Settings: defaultSettings}
	remove, err := recordWatch(home, rec)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := readWatch(home, rec.PID); !ok || got.Settings != rec.Settings || !got.StartedAt.Equal(rec.StartedAt) {
		t.Fatalf("read back %+v %v", got, ok)
	}
	if _, ok := readWatch(home, dead); ok {
		t.Fatal("the record of a dead watch was kept")
	}
	remove()
	if _, ok := readWatch(home, rec.PID); ok {
		t.Fatal("the record outlived its watch")
	}
}

func TestDriftReportsAnEditedFileAndAReplacedBinary(t *testing.T) {
	dir := t.TempDir()
	config, exe := filepath.Join(dir, "config.yml"), filepath.Join(dir, "infinitic")
	writeFile(t, config, "session: 90\n")
	writeFile(t, exe, "old")
	rec := runningWatch{StartedAt: time.Now().Add(time.Second), Executable: exe, Config: config, Settings: defaultSettings}
	if d := rec.drift(); len(d) != 0 {
		t.Fatalf("unchanged: %q", d)
	}
	writeFile(t, config, "session: 80\n")
	rec.StartedAt = time.Now().Add(-time.Minute)
	d := rec.drift()
	if len(d) != 2 || !strings.Contains(d[0], "session: 90 -> 80") || !strings.Contains(d[1], "was replaced") {
		t.Fatalf("drift %q", d)
	}
	writeFile(t, config, "sesion: 80\n")
	if d := rec.drift(); !strings.Contains(d[0], "cannot be read") {
		t.Fatalf("broken file: %q", d)
	}
}

func TestDriftIgnoresFlagsTheWatchWasStartedWith(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, config, "session: 90\n")
	flagged := defaultSettings
	flagged.Session = 80
	rec := runningWatch{StartedAt: time.Now().Add(time.Second), Config: config, Settings: flagged, File: &defaultSettings}
	if d := rec.drift(); len(d) != 0 {
		t.Fatalf("a flag was reported as a file change: %q", d)
	}
}

// restart runs `service start args` against m with INFINITIC_HOME=home. A watch the fake
// starts reports itself as pid 4242.
func restart(t *testing.T, home string, m *fakeManager, args ...string) error {
	t.Helper()
	t.Setenv("INFINITIC_HOME", home)
	m.onInstall = func(s service.Spec) {
		config := s.Args[slices.Index(s.Args, "--config")+1]
		got, _, _ := loadSettings(config)
		if _, err := recordWatch(home, runningWatch{PID: os.Getpid(), Config: config, Settings: got}); err != nil {
			t.Fatal(err)
		}
		m.status = service.Status{Installed: true, Running: true, PID: os.Getpid()}
	}
	devnull, _ := os.Open(os.DevNull)
	t.Cleanup(func() { devnull.Close() })
	a := &app{p: ui.NewPrinter(devnull), mgr: m, wait: time.Second}
	root := a.root()
	root.SetArgs(append([]string{"service", "start"}, args...))
	return root.Execute()
}

func TestServiceStartLeavesTheServiceAloneWhenTheFileIsUnusable(t *testing.T) {
	for _, content := range []string{"sesion: 80\n", "interval: 5s\n"} {
		home := t.TempDir()
		writeFile(t, settingsPath(home), content)
		m := &fakeManager{status: service.Status{Installed: true, Running: true, PID: 1}}
		err := restart(t, home, m)
		if err == nil || !strings.Contains(err.Error(), "left as it was") || len(m.installed) != 0 {
			t.Fatalf("%q: err=%v installs=%d", content, err, len(m.installed))
		}
	}
}

func TestServiceStartInstallsWithDefaultsWhenNothingIsThere(t *testing.T) {
	home := t.TempDir()
	m := &fakeManager{}
	if err := restart(t, home, m); err != nil {
		t.Fatal(err)
	}
	if got := readSettings(t, home); got != defaultSettings || len(m.installed) != 1 {
		t.Fatalf("settings %+v installs %d", got, len(m.installed))
	}
	data, _ := os.ReadFile(settingsPath(home))
	if !strings.Contains(string(data), `# After editing this file, run "infinitic service start".`) {
		t.Fatalf("the settings file does not say how to apply an edit:\n%s", data)
	}
}

func TestServiceStartKeepsTheFileTheServiceRunsWithAndEndsTheSameEachTime(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(home, "custom.yml")
	writeFile(t, custom, "session: 70\n")
	// The service runs as another live process, started with custom.yml; the fake then restarts
	// it as this process, so the second restart starts from the first one's record.
	if _, err := recordWatch(home, runningWatch{PID: os.Getppid(), Config: custom}); err != nil {
		t.Fatal(err)
	}
	m := &fakeManager{status: service.Status{Installed: true, Running: true, PID: os.Getppid()}}
	for range 2 {
		if err := restart(t, home, m); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.installed) != 2 || !slices.Equal(m.installed[0].Args, m.installed[1].Args) || !slices.Contains(m.installed[1].Args, custom) {
		t.Fatalf("installs %+v", m.installed)
	}
	if rec, _ := readWatch(home, os.Getpid()); rec.Settings.Session != 70 {
		t.Fatalf("running settings %+v", rec.Settings)
	}
}
