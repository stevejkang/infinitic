package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesWithPermAndKeepsExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "f.json")
	if err := Write(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "one", 0o600)

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "two", 0o640)
}

func TestWriteModeResetsExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteMode(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "two", 0o600)
}

func TestWriteKeepsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "claude.json")
	link := filepath.Join(dir, ".claude.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if err := Write(link, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file")
	}
	assertFile(t, real, "new", 0o600)
}

func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only the file", len(entries))
	}
}

func assertFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("%s holds %q (%v), want %q", path, data, err, content)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != perm {
		t.Fatalf("%s mode %v, want %v", path, info.Mode().Perm(), perm)
	}
}
