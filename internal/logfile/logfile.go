// Package logfile is an append-only log file that rotates itself by size, because launchd never
// rotates the files it redirects output to.
package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

const (
	DefaultMaxBytes = 5 << 20
	DefaultBackups  = 3
)

// File appends to Path and, when a write would grow it past MaxBytes, renames it to Path.1,
// shifting older backups up to Path.<Backups>.
type File struct {
	Path     string
	MaxBytes int64
	Backups  int

	mu   sync.Mutex
	f    *os.File
	size int64
}

func Open(path string) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	l := &File{Path: path, MaxBytes: DefaultMaxBytes, Backups: DefaultBackups}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wouldOverflow(len(p)) {
		if err := l.rotate(); err != nil {
			fmt.Fprintf(os.Stderr, "infinitic: rotating %s: %v (still writing to the current file)\n", l.Path, err)
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

func (l *File) wouldOverflow(n int) bool {
	return l.size > 0 && l.size+int64(n) > l.MaxBytes
}

func (l *File) rotate() error {
	if err := l.shiftBackups(); err != nil {
		return err
	}
	if err := os.Rename(l.Path, l.backup(1)); err != nil {
		return err
	}
	previous := l.f
	if err := l.open(); err != nil {
		return err
	}
	return previous.Close()
}

func (l *File) shiftBackups() error {
	for n := l.Backups; n > 1; n-- {
		err := os.Rename(l.backup(n-1), l.backup(n))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, info.Size()
	return nil
}

func (l *File) backup(n int) string { return fmt.Sprintf("%s.%d", l.Path, n) }
