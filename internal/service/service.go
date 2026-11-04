// Package service runs `infinitic watch` in the background under the OS service manager:
// a launchd LaunchAgent on macOS, a systemd user unit on Linux. The service manager restarts it
// if it dies and starts it at login. On macOS watch writes its own rotating log file; on Linux
// its output goes to the journal.
package service

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const name = "infinitic"

// Spec describes the background process.
type Spec struct {
	Executable string   // absolute path of the infinitic binary
	Args       []string // e.g. ["watch", "--interval", "1m0s"]
	PATH       string   // PATH for the process, so it can find `claude`
}

type Status struct {
	Installed bool
	Running   bool
	PID       int
	Note      string // extra advice, e.g. about systemd lingering
}

type Manager interface {
	Install(Spec) error
	// Restart restarts the installed service as its unit file describes it, arguments unchanged.
	Restart() error
	Uninstall() error
	Status() (Status, error)
	// UnitPath is the plist or unit file the manager writes.
	UnitPath() string
	// LogLocation tells a person where the background watch's log is.
	LogLocation() string
	// Logs returns a command that prints the last lines of the log, following it if asked.
	Logs(lines int, follow bool) (*exec.Cmd, error)
}

// ForCurrentOS returns the manager for this machine.
func ForCurrentOS() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	switch runtime.GOOS {
	case "darwin":
		return &Launchd{
			Dir:    filepath.Join(home, "Library", "LaunchAgents"),
			LogDir: filepath.Join(home, "Library", "Logs", name),
			UID:    os.Getuid(),
			Run:    runCommand,
		}, nil
	case "linux":
		return &Systemd{Dir: filepath.Join(configHome(home), "systemd", "user"), Run: runCommand}, nil
	default:
		return nil, fmt.Errorf("background service is not supported on %s", runtime.GOOS)
	}
}

// StableExecutable returns the path the service should launch. When the running binary is the
// one `infinitic` resolves to on PATH, it prefers the PATH entry: Homebrew's /opt/homebrew/bin
// symlink survives upgrades, while the versioned path it points to does not.
func StableExecutable() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	onPath, _ := exec.LookPath(name)
	return chooseExecutable(self, onPath, filepath.EvalSymlinks), nil
}

func chooseExecutable(self, onPath string, resolve func(string) (string, error)) string {
	if onPath == "" {
		return self
	}
	abs, err := filepath.Abs(onPath)
	if err != nil {
		return self
	}
	resolvedPath, err1 := resolve(abs)
	resolvedSelf, err2 := resolve(self)
	if err1 != nil || err2 != nil || resolvedPath != resolvedSelf {
		return self
	}
	return abs
}

func configHome(home string) string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".config")
}

// runner executes a service-manager command and returns its combined output.
type runner func(name string, args ...string) (string, error)

func runCommand(name string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return out.String(), fmt.Errorf("%s %v: exit %d: %s", name, args, exitErr.ExitCode(), bytes.TrimSpace(out.Bytes()))
		}
		return out.String(), err
	}
	return out.String(), nil
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
