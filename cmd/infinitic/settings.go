package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/stevejkang/infinitic/internal/atomicfile"
	"github.com/stevejkang/infinitic/internal/quota"
)

// settings are watch's options as stored in config.yml. Flags given to watch override them.
type settings struct {
	Interval time.Duration `yaml:"interval"`
	Session  float64       `yaml:"session"`
	Weekly   float64       `yaml:"weekly"`
}

var defaultSettings = settings{Interval: time.Minute, Session: quota.DefaultBars.Session, Weekly: quota.DefaultBars.Weekly}

func settingsPath(home string) string { return filepath.Join(home, "config.yml") }

// loadSettings reads path over the defaults. A missing file yields the defaults and exists=false;
// an unreadable one returns an error with exists=true.
func loadSettings(path string) (s settings, exists bool, err error) {
	s = defaultSettings
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, true, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return defaultSettings, true, fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return defaultSettings, true, fmt.Errorf("%s: more than one YAML document", path)
	}
	return s, true, nil
}

type field struct{ key, value, help string }

func (s settings) fields() []field {
	return []field{
		{"interval", shortDuration(s.Interval), "time between checks (minimum 30s)"},
		{"session", formatFloat(s.Session), "5-hour usage bar, percent"},
		{"weekly", formatFloat(s.Weekly), "7-day usage bar, percent"},
	}
}

func (s settings) render() string {
	var b strings.Builder
	b.WriteString("# After editing this file, run \"infinitic service start\".\n")
	for _, f := range s.fields() {
		fmt.Fprintf(&b, "%-16s # %s\n", f.key+": "+f.value, f.help)
	}
	return b.String()
}

func (s settings) write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, []byte(s.render()), 0o600)
}

// changes lists what differs from old, one "key: old -> new" per entry.
func (s settings) changes(old settings) []string {
	var out []string
	before := old.fields()
	for i, f := range s.fields() {
		if f.value != before[i].value {
			out = append(out, fmt.Sprintf("%s: %s -> %s", f.key, before[i].value, f.value))
		}
	}
	return out
}

// overriddenBy returns s with the watch flags that were given on the command line applied.
func (s settings) overriddenBy(set *pflag.FlagSet, f watchFlags) settings {
	set.Visit(func(fl *pflag.Flag) {
		switch fl.Name {
		case "interval":
			s.Interval = *f.interval
		case "session":
			s.Session = f.bars.Session
		case "weekly":
			s.Weekly = f.bars.Weekly
		}
	})
	return s
}

// apply writes s into the flag values.
func (s settings) apply(f watchFlags) {
	*f.interval = s.Interval
	f.bars.Session, f.bars.Weekly = s.Session, s.Weekly
}

func shortDuration(d time.Duration) string {
	s := d.String()
	s = strings.Replace(s, "m0s", "m", 1)
	return strings.Replace(s, "h0m", "h", 1)
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
