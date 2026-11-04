package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/stevejkang/infinitic/internal/service"
	"github.com/stevejkang/infinitic/internal/ui"
)

type fakeManager struct {
	installed []service.Spec
	status    service.Status
	onInstall func(service.Spec)
	restarts  int
	onRestart func()
}

func (m *fakeManager) Install(s service.Spec) error {
	m.installed = append(m.installed, s)
	if m.onInstall != nil {
		m.onInstall(s)
	}
	return nil
}
func (m *fakeManager) Restart() error {
	m.restarts++
	if m.onRestart != nil {
		m.onRestart()
	}
	return nil
}
func (m *fakeManager) Uninstall() error                { return nil }
func (m *fakeManager) Status() (service.Status, error) { return m.status, nil }
func (m *fakeManager) UnitPath() string                { return "/unit" }
func (m *fakeManager) LogLocation() string             { return "/log" }
func (m *fakeManager) Logs(int, bool) (*exec.Cmd, error) {
	return nil, errors.New("no logs")
}

// install runs `service start args` with INFINITIC_HOME in a temp dir, answering the
// overwrite question with answer (asked reports whether it was asked).
func install(t *testing.T, home string, answer bool, args ...string) (m *fakeManager, asked bool, err error) {
	t.Helper()
	t.Setenv("INFINITIC_HOME", home)
	ask := func(string) (bool, error) { asked = true; return answer, nil }
	m = &fakeManager{}
	devnull, _ := os.Open(os.DevNull)
	t.Cleanup(func() { devnull.Close() })
	a := &app{p: ui.NewPrinter(devnull), mgr: m, ask: ask}
	root := a.root()
	root.SetArgs(append([]string{"service", "start"}, args...))
	err = root.Execute()
	return m, asked, err
}

func readSettings(t *testing.T, home string) settings {
	t.Helper()
	s, exists, err := loadSettings(settingsPath(home))
	if err != nil || !exists {
		t.Fatalf("settings: exists=%v err=%v", exists, err)
	}
	return s
}

func TestServiceStartWritesDefaultsWhenThereIsNoFile(t *testing.T) {
	home := t.TempDir()
	m, asked, err := install(t, home, false)
	if err != nil || asked {
		t.Fatalf("err=%v asked=%v", err, asked)
	}
	if got := readSettings(t, home); got != defaultSettings {
		t.Fatalf("settings %+v, want defaults", got)
	}
	want := []string{"watch", "--config", settingsPath(home)}
	if len(m.installed) != 1 || !slices.Equal(m.installed[0].Args, want) {
		t.Fatalf("installed %+v, want args %v", m.installed, want)
	}
}

func TestServiceStartStoresFlagsInANewFile(t *testing.T) {
	home := t.TempDir()
	m, asked, err := install(t, home, false, "--session", "80", "--interval", "2m")
	if err != nil || asked {
		t.Fatalf("err=%v asked=%v", err, asked)
	}
	if got := readSettings(t, home); got.Session != 80 || got.Interval != 2*time.Minute || got.Weekly != 98 {
		t.Fatalf("settings %+v", got)
	}
	want := []string{"watch", "--config", settingsPath(home)}
	if len(m.installed) != 1 || !slices.Equal(m.installed[0].Args, want) {
		t.Fatalf("installed %+v, want args %v", m.installed, want)
	}
}

func TestServiceStartKeepsAnExistingFileWithoutFlags(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "session: 70\n")
	m, asked, err := install(t, home, false)
	if err != nil || asked || len(m.installed) != 1 {
		t.Fatalf("err=%v asked=%v installed=%d", err, asked, len(m.installed))
	}
	if data, _ := os.ReadFile(settingsPath(home)); string(data) != "session: 70\n" {
		t.Fatalf("file was rewritten: %q", data)
	}
}

func TestServiceStartYesOverwritesTheFile(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "session: 70\nweekly: 95\n")
	m, asked, err := install(t, home, true, "--session", "80")
	if !asked || err != nil || len(m.installed) != 1 {
		t.Fatalf("asked=%v err=%v installed=%d", asked, err, len(m.installed))
	}
	if got := readSettings(t, home); got.Session != 80 || got.Weekly != 95 {
		t.Fatalf("settings %+v: the flag must change only its own key", got)
	}
	if want := []string{"watch", "--config", settingsPath(home)}; !slices.Equal(m.installed[0].Args, want) {
		t.Fatalf("args %v, want %v", m.installed[0].Args, want)
	}
}

func TestServiceStartNoKeepsTheFileAndAppliesFlagsToThisInstallOnly(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "session: 70\n")
	m, asked, err := install(t, home, false, "--session", "80")
	if !asked || err != nil || len(m.installed) != 1 {
		t.Fatalf("asked=%v err=%v installed=%d", asked, err, len(m.installed))
	}
	if data, _ := os.ReadFile(settingsPath(home)); string(data) != "session: 70\n" {
		t.Fatalf("file was rewritten: %q", data)
	}
	if want := []string{"watch", "--config", settingsPath(home), "--session", "80"}; !slices.Equal(m.installed[0].Args, want) {
		t.Fatalf("args %v, want %v", m.installed[0].Args, want)
	}
}

func TestServiceStartOverwriteSkipsTheQuestion(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "session: 70\n")
	_, asked, err := install(t, home, false, "--session", "80", "--overwrite")
	if asked || err != nil {
		t.Fatalf("asked=%v err=%v", asked, err)
	}
	if readSettings(t, home).Session != 80 {
		t.Fatal("--overwrite did not store the flag")
	}
}

func TestServiceStartWithoutATerminalKeepsTheFile(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "session: 70\n")
	t.Setenv("INFINITIC_HOME", home)
	m := &fakeManager{}
	noAnswer := func(string) (bool, error) { return false, ui.ErrNoAnswer }
	devnull, _ := os.Open(os.DevNull)
	t.Cleanup(func() { devnull.Close() })
	root := (&app{p: ui.NewPrinter(devnull), mgr: m, ask: noAnswer}).root()
	root.SetArgs([]string{"service", "start", "--session", "80"})
	if err := root.Execute(); err != nil || len(m.installed) != 1 || readSettings(t, home).Session != 70 {
		t.Fatalf("err=%v installed=%d", err, len(m.installed))
	}
}

func TestServiceStartDoesNotAskWhenFlagsMatchTheFile(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "session: 80\n")
	if _, asked, err := install(t, home, false, "--session", "80"); asked || err != nil {
		t.Fatalf("asked=%v err=%v", asked, err)
	}
}

func TestServiceStartUsesTheFileOrWritesDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("INFINITIC_HOME", home)
	m := &fakeManager{}
	devnull, _ := os.Open(os.DevNull)
	t.Cleanup(func() { devnull.Close() })
	run := func(args ...string) error {
		root := (&app{p: ui.NewPrinter(devnull), mgr: m}).root()
		root.SetArgs(append([]string{"service", "start"}, args...))
		return root.Execute()
	}
	if err := run(); err != nil || readSettings(t, home) != defaultSettings {
		t.Fatalf("missing file: err=%v", err)
	}
	writeFile(t, settingsPath(home), "session: 70\n")
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(settingsPath(home)); string(data) != "session: 70\n" {
		t.Fatalf("existing file was rewritten: %q", data)
	}
	want := []string{"watch", "--config", settingsPath(home)}
	if len(m.installed) != 2 || !slices.Equal(m.installed[1].Args, want) {
		t.Fatalf("installed %+v", m.installed)
	}
	for _, flag := range []string{"--once", "--log-file=x"} {
		if err := run(flag); err == nil || !strings.Contains(err.Error(), "Unknown option") {
			t.Fatalf("start accepted %s: %v", flag, err)
		}
	}
}

func TestServiceStartRejectsInvalidValuesBeforeWriting(t *testing.T) {
	home := t.TempDir()
	if _, _, err := install(t, home, true, "--interval", "5s"); err == nil {
		t.Fatal("a 5s interval was accepted")
	}
	if _, err := os.Stat(settingsPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings written despite invalid flags: %v", err)
	}
}

func TestSettingsRejectUnknownKeysAndExtraDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	for content, want := range map[string]string{
		"sesion: 80\n":                  "sesion",
		"session: 80\n---\nweekly: 9\n": "more than one YAML document",
	} {
		writeFile(t, path, content)
		if _, exists, err := loadSettings(path); !exists || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: exists=%v err=%v, want an error naming %q", content, exists, err, want)
		}
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	for _, d := range []time.Duration{30 * time.Second, time.Minute, 90 * time.Second, time.Hour, time.Hour + 30*time.Second} {
		s := defaultSettings
		s.Interval, s.Session = d, 87.5
		if err := s.write(path); err != nil {
			t.Fatal(err)
		}
		if got, _, err := loadSettings(path); err != nil || got != s {
			t.Fatalf("round trip of %s: got %+v err %v", d, got, err)
		}
	}
	writeFile(t, path, "")
	if got, exists, err := loadSettings(path); err != nil || !exists || got != defaultSettings {
		t.Fatalf("empty file: %+v %v %v", got, exists, err)
	}
}

func TestServiceStartReplacesAnUnreadableFileOnlyWhenAllowed(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "sesion: 70\n")
	if m, _, err := install(t, home, false, "--session", "80"); !errors.Is(err, ui.ErrCancelled) || len(m.installed) != 0 {
		t.Fatalf("declined: err=%v installed=%d", err, len(m.installed))
	}
	if data, _ := os.ReadFile(settingsPath(home)); string(data) != "sesion: 70\n" {
		t.Fatalf("declined replace rewrote the file: %q", data)
	}
	if _, asked, err := install(t, home, false, "--session", "80", "--overwrite"); asked || err != nil {
		t.Fatalf("asked=%v err=%v", asked, err)
	}
	if got := readSettings(t, home); got.Session != 80 || got.Weekly != defaultSettings.Weekly {
		t.Fatalf("settings %+v", got)
	}
}

func TestValidationErrorsNameTheSettingsFile(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "interval: 5s\n")
	if _, _, err := install(t, home, true); err == nil || !strings.Contains(strings.Join(asCLIError(err).details, " "), settingsPath(home)) {
		t.Fatalf("err %v does not point at %s", err, settingsPath(home))
	}
}

func TestWatchFlagsOverrideTheSettingsFile(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "session: 70\nweekly: 95\ninterval: 2m\n")
	t.Setenv("INFINITIC_HOME", home)
	fs := pflag.NewFlagSet("watch", pflag.ContinueOnError)
	f := addWatchFlags(fs)
	if err := fs.Parse([]string{"--session", "80"}); err != nil {
		t.Fatal(err)
	}
	file, _, err := loadSettings(*f.config)
	if err != nil {
		t.Fatal(err)
	}
	file.overriddenBy(fs, f).apply(f)
	if f.bars.Session != 80 || f.bars.Weekly != 95 || *f.interval != 2*time.Minute {
		t.Fatalf("got session %v weekly %v interval %v", f.bars.Session, f.bars.Weekly, *f.interval)
	}
}

func TestStatusBarsComeFromTheSettingsFileUnlessFlagged(t *testing.T) {
	home := t.TempDir()
	writeFile(t, settingsPath(home), "session: 70\nweekly: 95\n")
	t.Setenv("INFINITIC_HOME", home)
	fs := pflag.NewFlagSet("status", pflag.ContinueOnError)
	flags := barFlags(fs)
	bars, source, err := barsFromSettings(fs, *flags)
	if err != nil || bars.Session != 70 || bars.Weekly != 95 || !strings.Contains(source, settingsPath(home)) {
		t.Fatalf("got %v %q %v", bars, source, err)
	}
	if err := fs.Parse([]string{"--session", "80"}); err != nil {
		t.Fatal(err)
	}
	bars, source, _ = barsFromSettings(fs, *flags)
	if bars.Session != 80 || bars.Weekly != 95 || !strings.HasPrefix(source, "from flags and ") {
		t.Fatalf("got %v %q", bars, source)
	}

	writeFile(t, settingsPath(home), "interval: 29s\n")
	if _, _, err := barsFromSettings(fs, *flags); err == nil || !strings.Contains(err.Error(), "interval 29s is too short") {
		t.Fatalf("an interval watch would refuse: got %v", err)
	}
}
