package service

import (
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const launchdLabel = "com.stevejkang.infinitic"

// Launchd manages a per-user LaunchAgent (macOS).
type Launchd struct {
	Dir    string // ~/Library/LaunchAgents
	LogDir string // ~/Library/Logs/infinitic
	UID    int
	Run    runner
}

func (l *Launchd) UnitPath() string { return filepath.Join(l.Dir, launchdLabel+".plist") }

func (l *Launchd) logPath() string    { return filepath.Join(l.LogDir, "watch.log") }
func (l *Launchd) outputPath() string { return filepath.Join(l.LogDir, "watch.stderr.log") }

func (l *Launchd) LogLocation() string { return l.logPath() }

func (l *Launchd) Logs(lines int, follow bool) (*exec.Cmd, error) {
	if _, err := os.Stat(l.logPath()); err != nil {
		return nil, fmt.Errorf("no log yet at %s", l.logPath())
	}
	args := []string{"-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "-F") // by name, so it survives rotation
	}
	return exec.Command("tail", append(args, l.logPath())...), nil
}

func (l *Launchd) target() string { return fmt.Sprintf("gui/%d/%s", l.UID, launchdLabel) }

func (l *Launchd) Install(spec Spec) error {
	if err := os.MkdirAll(l.LogDir, 0o755); err != nil {
		return err
	}
	spec.Args = append(spec.Args, "--log-file", l.logPath())
	if err := writeFile(l.UnitPath(), renderPlist(spec, l.outputPath())); err != nil {
		return err
	}
	_, _ = l.Run("launchctl", "bootout", l.target()) // reinstall: unload the previous definition first
	// bootout returns before the old job is gone; bootstrap fails with an I/O error until it is.
	var err error
	for attempt := 0; attempt < bootstrapAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(bootstrapRetryDelay)
		}
		if _, err = l.Run("launchctl", "bootstrap", fmt.Sprintf("gui/%d", l.UID), l.UnitPath()); err == nil {
			return nil
		}
	}
	return err
}

const bootstrapAttempts = 10

var bootstrapRetryDelay = 500 * time.Millisecond

func (l *Launchd) Restart() error {
	_, err := l.Run("launchctl", "kickstart", "-k", l.target())
	return err
}

func (l *Launchd) Uninstall() error {
	_, _ = l.Run("launchctl", "bootout", l.target())
	if err := os.Remove(l.UnitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

var (
	launchdState = regexp.MustCompile(`(?m)^\s*state = (\S+)`)
	launchdPID   = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
)

func (l *Launchd) Status() (Status, error) {
	if _, err := os.Stat(l.UnitPath()); err != nil {
		return Status{}, nil
	}
	out, err := l.Run("launchctl", "print", l.target())
	if err != nil {
		return Status{Installed: true, Note: "the agent file exists but is not loaded"}, nil
	}
	return parseLaunchdPrint(out), nil
}

func parseLaunchdPrint(out string) Status {
	s := Status{Installed: true}
	if m := launchdState.FindStringSubmatch(out); m != nil {
		s.Running = m[1] == "running"
	}
	if m := launchdPID.FindStringSubmatch(out); m != nil {
		s.PID, _ = strconv.Atoi(m[1])
	}
	return s
}

func renderPlist(spec Spec, outputPath string) string {
	var args strings.Builder
	for _, a := range append([]string{spec.Executable}, spec.Args...) {
		fmt.Fprintf(&args, "\n    <string>%s</string>", html.EscapeString(a))
	}
	output := html.EscapeString(outputPath)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>%s
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, launchdLabel, args.String(), html.EscapeString(spec.PATH), output, output)
}
