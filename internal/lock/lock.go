// Package lock serializes infinitic processes (the background watch and commands run by hand),
// so two of them never move grants at the same time.
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const pollEvery = 100 * time.Millisecond

// Acquire waits until it holds the exclusive lock at path, or until ctx is done. If the lock is
// busy it calls waiting once, so a command run by hand can say why it pauses. Call the returned
// func to release the lock. The lock is an flock(2), so the kernel releases it if the process dies.
func Acquire(ctx context.Context, path string, waiting func()) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for notified := false; ; notified = true {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if !notified && waiting != nil {
			waiting()
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}
