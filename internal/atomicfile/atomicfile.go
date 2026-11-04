// Package atomicfile replaces a file's contents so a concurrent reader sees either the old or the
// new contents, never a partial write, even across a crash.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Write replaces path with data. If path is a symlink, the link target is replaced and the link
// is kept. An existing file keeps its permissions; a new one gets perm.
func Write(path string, data []byte, perm fs.FileMode) error {
	return write(path, data, perm, true)
}

// WriteMode is Write, but an existing file gets perm too.
func WriteMode(path string, data []byte, perm fs.FileMode) error {
	return write(path, data, perm, false)
}

func write(path string, data []byte, perm fs.FileMode, keepMode bool) error {
	target, err := resolve(path)
	if err != nil {
		return err
	}
	if info, err := os.Stat(target); err == nil && keepMode {
		perm = info.Mode().Perm()
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".*.tmp") // O_EXCL, mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

// resolve follows a symlink at path; a path that does not exist yet resolves to itself.
func resolve(path string) (string, error) {
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil
	}
	return target, err
}
