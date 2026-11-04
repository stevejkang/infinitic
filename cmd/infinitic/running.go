package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stevejkang/infinitic/internal/atomicfile"
)

// runningWatch is what a watch process started with. It reads its settings once, so this is the
// only way to tell what a running watch uses after the settings file was edited.
type runningWatch struct {
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"startedAt"`
	Version    string    `json:"version"`
	Executable string    `json:"executable"`
	Config     string    `json:"config"`
	Settings   settings  `json:"settings"`
	File       *settings `json:"file,omitempty"` // the settings file's values at start, before flags
}

func runDir(home string) string { return filepath.Join(home, "run") }

func runPath(home string, pid int) string {
	return filepath.Join(runDir(home), "watch-"+strconv.Itoa(pid)+".json")
}

// recordWatch stores rec and returns the function that removes it. Records of watches that died
// without removing theirs are dropped on the way.
func recordWatch(home string, rec runningWatch) (remove func(), err error) {
	entries, _ := os.ReadDir(runDir(home))
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), "watch-")
		pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if ok && err == nil && pid != rec.PID && !alive(pid) {
			_ = os.Remove(filepath.Join(runDir(home), e.Name()))
		}
	}
	if err := os.MkdirAll(runDir(home), 0o700); err != nil {
		return func() {}, err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return func() {}, err
	}
	path := runPath(home, rec.PID)
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		return func() {}, err
	}
	return func() { _ = os.Remove(path) }, nil
}

// readWatch returns the record of the watch with pid, if it wrote one.
func readWatch(home string, pid int) (runningWatch, bool) {
	var rec runningWatch
	data, err := os.ReadFile(runPath(home, pid))
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.PID != pid {
		return runningWatch{}, false
	}
	return rec, true
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// e.g. "interval 1m, session 90, weekly 98"
func (s settings) describe() string {
	parts := make([]string, 0, 4)
	for _, f := range s.fields() {
		parts = append(parts, f.key+" "+f.value)
	}
	return strings.Join(parts, ", ")
}

// drift lists why the running watch no longer matches what `service start` would start, "" if nothing.
func (rec runningWatch) drift() []string {
	var out []string
	current, _, err := loadSettings(rec.Config)
	started := rec.Settings
	if rec.File != nil {
		started = *rec.File
	}
	switch {
	case err != nil:
		out = append(out, fmt.Sprintf("%s cannot be read now (%v); `service start` fails until it is fixed", rec.Config, err))
	default:
		if changes := current.changes(started); len(changes) > 0 {
			out = append(out, rec.Config+" changed since it started: "+strings.Join(changes, ", "))
		}
	}
	if info, err := os.Stat(rec.Executable); err == nil && info.ModTime().After(rec.StartedAt) {
		out = append(out, rec.Executable+" was replaced since it started")
	}
	return out
}
