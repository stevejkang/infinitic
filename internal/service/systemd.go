package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const systemdUnit = name + ".service"

// Systemd manages a systemd user unit (Linux).
type Systemd struct {
	Dir string // ~/.config/systemd/user
	Run runner
}

func (s *Systemd) UnitPath() string { return filepath.Join(s.Dir, systemdUnit) }

func (s *Systemd) systemctl(args ...string) (string, error) {
	return s.Run("systemctl", append([]string{"--user"}, args...)...)
}

func (s *Systemd) LogLocation() string { return "journalctl --user -u " + systemdUnit }

func (s *Systemd) Logs(lines int, follow bool) (*exec.Cmd, error) {
	args := []string{"--user", "-u", systemdUnit, "-n", strconv.Itoa(lines), "-o", "cat", "--no-pager"}
	if follow {
		args = append(args, "-f")
	}
	return exec.Command("journalctl", args...), nil
}

func (s *Systemd) Install(spec Spec) error {
	if err := writeFile(s.UnitPath(), renderUnit(spec)); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", systemdUnit}} {
		if _, err := s.systemctl(args...); err != nil {
			return err
		}
	}
	return s.Restart()
}

func (s *Systemd) Restart() error {
	// clears systemd's start limit (5 starts in 10s), which blocks restarts until reset
	_, _ = s.systemctl("reset-failed", systemdUnit)
	_, err := s.systemctl("restart", systemdUnit)
	return err
}

func (s *Systemd) Uninstall() error {
	_, _ = s.systemctl("disable", "--now", systemdUnit)
	if err := os.Remove(s.UnitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := s.systemctl("daemon-reload")
	return err
}

func (s *Systemd) Status() (Status, error) {
	if _, err := os.Stat(s.UnitPath()); err != nil {
		return Status{}, nil
	}
	st := Status{Installed: true}
	out, _ := s.systemctl("show", systemdUnit, "--property=ActiveState,MainPID")
	props := parseProperties(out)
	st.Running = props["ActiveState"] == "active"
	st.PID, _ = strconv.Atoi(props["MainPID"])

	if linger, err := s.Run("loginctl", "show-user", os.Getenv("USER"), "--property=Linger"); err == nil && strings.TrimSpace(linger) == "Linger=no" {
		st.Note = "user services stop when you log out; loginctl enable-linger keeps the watch running"
	}
	return st, nil
}

func parseProperties(out string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return props
}

func renderUnit(spec Spec) string {
	command := make([]string, 0, len(spec.Args)+1)
	for _, a := range append([]string{spec.Executable}, spec.Args...) {
		command = append(command, systemdQuote(a))
	}
	return fmt.Sprintf(`[Unit]
Description=infinitic - rotate the primary Claude Code login
After=network-online.target

[Service]
ExecStart=%s
Environment=%s
Restart=always
RestartSec=30

[Install]
WantedBy=default.target
`, strings.Join(command, " "), systemdQuote("PATH="+spec.PATH))
}

// systemdQuote quotes a word for an ExecStart= or Environment= line.
func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}
