package lock

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	release, err := Acquire(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var waited atomic.Int32
	acquired := make(chan func())
	go func() {
		r, err := Acquire(context.Background(), path, func() { waited.Add(1) })
		if err != nil {
			t.Error(err)
		}
		acquired <- r
	}()
	select {
	case <-acquired:
		t.Fatal("a second holder got the lock while the first still held it")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case r := <-acquired:
		r()
	case <-time.After(2 * time.Second):
		t.Fatal("the lock was not handed over after release")
	}
	if waited.Load() != 1 {
		t.Fatalf("waiting was called %d times, want once", waited.Load())
	}
}

func TestAcquireGivesUpWhenCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	release, err := Acquire(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the context error", err)
	}
}
