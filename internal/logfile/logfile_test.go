package logfile

import (
	"os"
	"path/filepath"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRotatesBySizeAndKeepsBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "watch.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.MaxBytes, l.Backups = 10, 2
	for _, line := range []string{"aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n", "dddddddd\n"} {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{path: "dddddddd\n", path + ".1": "cccccccc\n", path + ".2": "bbbbbbbb\n"} {
		if got := read(t, file); got != want {
			t.Errorf("%s = %q, want %q", filepath.Base(file), got, want)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Error("kept more backups than asked")
	}
}

func TestAppendsToAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got := read(t, path); got != "old\nnew\n" {
		t.Fatalf("got %q", got)
	}
}

func TestALineLargerThanTheLimitIsStillWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.log")
	l, _ := Open(path)
	l.MaxBytes = 4
	if _, err := l.Write([]byte("0123456789\n")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got := read(t, path); got != "0123456789\n" {
		t.Fatalf("got %q", got)
	}
}
