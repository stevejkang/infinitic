package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var spec = Spec{
	Executable: "/opt/homebrew/bin/infinitic",
	Args:       []string{"watch", "--interval", "1m0s", "--session", "90", "--weekly", "98"},
	PATH:       "/opt/homebrew/bin:/usr/bin:/bin",
}

func TestRenderPlist(t *testing.T) {
	plist := renderPlist(spec, "/Users/me/Library/Logs/infinitic/watch.stderr.log")
	for _, want := range []string{
		"<string>com.stevejkang.infinitic</string>",
		"<string>/opt/homebrew/bin/infinitic</string>\n    <string>watch</string>",
		"<key>KeepAlive</key>\n  <true/>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<string>/opt/homebrew/bin:/usr/bin:/bin</string>",
		"<key>StandardErrorPath</key>\n  <string>/Users/me/Library/Logs/infinitic/watch.stderr.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(renderPlist(Spec{Executable: "/a&b/<x>"}, ""), "/a&b/<x>") {
		t.Error("plist values are not XML-escaped")
	}
}

func TestRenderUnit(t *testing.T) {
	unit := renderUnit(Spec{Executable: "/home/me/bin/infinitic", Args: []string{"watch", "--interval", "1m0s"}, PATH: "/home/me/.local/bin:/usr/bin"})
	for _, want := range []string{
		"ExecStart=/home/me/bin/infinitic watch --interval 1m0s\n",
		"Environment=PATH=/home/me/.local/bin:/usr/bin\n",
		"Restart=always\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "StandardOutput") {
		t.Errorf("unit redirects output away from the journal:\n%s", unit)
	}
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":        "plain",
		"has space":    `"has space"`,
		`a"b`:          `"a\"b"`,
		"$HOME":        `"$$HOME"`,
		"100%":         `"100%%"`,
		"PATH=/a b:/c": `"PATH=/a b:/c"`,
	} {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestChooseExecutablePrefersTheStablePathEntry(t *testing.T) {
	links := map[string]string{
		"/opt/homebrew/bin/infinitic":                      "/opt/homebrew/Caskroom/infinitic/1.2.0/infinitic",
		"/opt/homebrew/Caskroom/infinitic/1.2.0/infinitic": "/opt/homebrew/Caskroom/infinitic/1.2.0/infinitic",
		"/tmp/dev/infinitic":                               "/tmp/dev/infinitic",
	}
	resolve := func(p string) (string, error) {
		if r, ok := links[p]; ok {
			return r, nil
		}
		return "", errors.New("missing")
	}
	self := "/opt/homebrew/Caskroom/infinitic/1.2.0/infinitic"
	if got := chooseExecutable(self, "/opt/homebrew/bin/infinitic", resolve); got != "/opt/homebrew/bin/infinitic" {
		t.Errorf("same binary: got %s, want the PATH symlink", got)
	}
	if got := chooseExecutable("/tmp/dev/infinitic", "/opt/homebrew/bin/infinitic", resolve); got != "/tmp/dev/infinitic" {
		t.Errorf("different binary on PATH: got %s, want the running one", got)
	}
	if got := chooseExecutable(self, "", resolve); got != self {
		t.Errorf("not on PATH: got %s", got)
	}
}

func TestLaunchdLifecycleCommands(t *testing.T) {
	dir := t.TempDir()
	var calls [][]string
	l := &Launchd{Dir: dir, LogDir: filepath.Join(dir, "logs"), UID: 501, Run: func(name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "print" {
			return "\tstate = running\n\tpid = 4242\n", nil
		}
		return "", nil
	}}
	if err := l.Install(Spec{Executable: "/x/infinitic", Args: []string{"watch"}}); err != nil {
		t.Fatal(err)
	}
	plist, _ := os.ReadFile(l.UnitPath())
	logFile := "<string>--log-file</string>\n    <string>" + filepath.Join(dir, "logs", "watch.log") + "</string>"
	if !strings.Contains(string(plist), logFile) {
		t.Errorf("watch is not told to write its own log:\n%s", plist)
	}
	want := [][]string{
		{"launchctl", "bootout", "gui/501/com.stevejkang.infinitic"},
		{"launchctl", "bootstrap", "gui/501", filepath.Join(dir, "com.stevejkang.infinitic.plist")},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("install ran %v, want %v", calls, want)
	}
	st, err := l.Status()
	if err != nil || !st.Installed || !st.Running || st.PID != 4242 {
		t.Fatalf("status %+v %v", st, err)
	}
	calls = nil
	if err := l.Restart(); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"launchctl", "kickstart", "-k", "gui/501/com.stevejkang.infinitic"}}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("restart ran %v, want %v", calls, want)
	}
	if err := l.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.UnitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uninstall left the plist behind")
	}
}

func TestLaunchdInstallRetriesWhileTheOldJobIsStillUnloading(t *testing.T) {
	defer func(d time.Duration) { bootstrapRetryDelay = d }(bootstrapRetryDelay)
	bootstrapRetryDelay = 0
	bootstraps := 0
	l := &Launchd{Dir: t.TempDir(), LogDir: t.TempDir(), UID: 501, Run: func(name string, args ...string) (string, error) {
		if args[0] == "bootstrap" {
			if bootstraps++; bootstraps < 3 {
				return "Bootstrap failed: 5: Input/output error", errors.New("exit 5")
			}
		}
		return "", nil
	}}
	if err := l.Install(Spec{Executable: "/x/infinitic"}); err != nil {
		t.Fatal(err)
	}
	if bootstraps != 3 {
		t.Fatalf("bootstrap ran %d times, want 3", bootstraps)
	}
}

func TestLogsCommands(t *testing.T) {
	dir := t.TempDir()
	l := &Launchd{LogDir: dir}
	if _, err := l.Logs(10, false); err == nil {
		t.Error("launchd logs before any log exists: want an error")
	}
	if err := writeFile(filepath.Join(dir, "watch.log"), "x\n"); err != nil {
		t.Fatal(err)
	}
	cmd, err := l.Logs(10, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tail", "-n", "10", "-F", filepath.Join(dir, "watch.log")}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("launchd logs %v, want %v", cmd.Args, want)
	}
	cmd, _ = (&Systemd{}).Logs(5, true)
	if want := []string{"journalctl", "--user", "-u", "infinitic.service", "-n", "5", "-o", "cat", "--no-pager", "-f"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("systemd logs %v, want %v", cmd.Args, want)
	}
}

func TestSystemdInstallClearsTheStartLimitBeforeRestarting(t *testing.T) {
	var calls []string
	s := &Systemd{Dir: t.TempDir(), Run: func(name string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[1] == "reset-failed" {
			return "", errors.New("exit 1")
		}
		return "", nil
	}}
	if err := s.Install(spec); err != nil {
		t.Fatal(err)
	}
	want := []string{"--user daemon-reload", "--user enable infinitic.service", "--user reset-failed infinitic.service", "--user restart infinitic.service"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("install ran %v, want %v", calls, want)
	}
}

func TestSystemdStatusParsesProperties(t *testing.T) {
	dir := t.TempDir()
	s := &Systemd{Dir: dir, Run: func(name string, args ...string) (string, error) {
		if name == "loginctl" {
			return "Linger=no\n", nil
		}
		return "ActiveState=active\nMainPID=777\n", nil
	}}
	if err := writeFile(s.UnitPath(), "x"); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Status()
	if !st.Running || st.PID != 777 || !strings.Contains(st.Note, "enable-linger") {
		t.Fatalf("status %+v", st)
	}
}
