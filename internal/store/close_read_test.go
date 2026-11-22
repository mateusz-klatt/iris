package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// closeDrainWait bounds how long a drain test waits for Close to cancel a read.
// Cancellation is an in-process context signal, so this is a hang detector.
const closeDrainWait = 15 * time.Second

// heldConsoleRead is a readTransact call parked inside its callback. That callback
// is the test hook readTransact already takes: Close must not unlock while it runs.
type heldConsoleRead struct {
	entered   chan struct{}
	sawCancel chan struct{}
	release   chan struct{}
	done      chan error
	once      sync.Once
}

func holdConsoleRead(s *Store) *heldConsoleRead {
	h := &heldConsoleRead{
		entered:   make(chan struct{}),
		sawCancel: make(chan struct{}),
		release:   make(chan struct{}),
		done:      make(chan error, 1),
	}
	go func() {
		h.done <- s.readTransact(context.Background(), func(tx *transaction) error {
			close(h.entered)
			select {
			case <-tx.ctx.Done():
				close(h.sawCancel)
				<-h.release
				return tx.ctx.Err()
			case <-h.release:
				return nil
			}
		})
	}()
	return h
}

func (h *heldConsoleRead) finish() { h.once.Do(func() { close(h.release) }) }

func (h *heldConsoleRead) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(closeDrainWait):
		t.Fatal("console read did not enter readTransact")
	}
}

func (h *heldConsoleRead) waitCanceled(t *testing.T, closeDone <-chan error) {
	t.Helper()
	select {
	case <-h.sawCancel:
	case err := <-closeDone:
		t.Fatalf("Close returned before the read observed cancellation: %v", err)
	case <-time.After(closeDrainWait):
		t.Fatal("Close did not cancel the in-flight console read")
	}
}

// databaseLockFree reports whether path can be locked exclusively right now.
// The store's own lock file is the one Close releases, so this is that file.
func databaseLockFree(path string) bool {
	f, err := lockDatabase(path)
	if err != nil {
		return false
	}
	unlockDatabase(f)
	return true
}

func assertLockHeld(t *testing.T, path string) {
	t.Helper()
	if databaseLockFree(path) {
		t.Fatal("database lock was free")
	}
}

func assertCloseStillWaiting(t *testing.T, closeDone <-chan error) {
	t.Helper()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned while a console read was still inside readTransact: %v", err)
	default:
	}
}

// TestCloseWaitsForConsoleReadBeforeUnlock is finding (a): a read held open
// across Close keeps the database lock until that read has observed cancellation
// and left readTransact.
func TestCloseWaitsForConsoleReadBeforeUnlock(t *testing.T) {
	s, _, _ := testStore(t)
	lockPath := s.lock.Name()
	held := holdConsoleRead(s)
	defer held.finish()
	held.waitEntered(t)
	assertLockHeld(t, lockPath)

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	held.waitCanceled(t, closeDone)
	assertCloseStillWaiting(t, closeDone)
	assertLockHeld(t, lockPath)

	held.finish()
	select {
	case err := <-held.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight read returned %v, want context.Canceled", err)
		}
	case <-time.After(closeDrainWait):
		t.Fatal("in-flight read did not finish after cancellation")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(closeDrainWait):
		t.Fatal("Close did not return after the console read finished")
	}
	if !databaseLockFree(lockPath) {
		t.Fatal("database lock stayed held after Close returned")
	}
}

// TestConsoleReadStartedAfterCloseBeginsReturnsStoreClosed is finding (b): a read
// that arrives after Close has begun does not join the drain, and it fails with
// the store-closed error while the lock is still held for the read already inside.
func TestConsoleReadStartedAfterCloseBeginsReturnsStoreClosed(t *testing.T) {
	s, _, _ := testStore(t)
	lockPath := s.lock.Name()
	held := holdConsoleRead(s)
	defer held.finish()
	held.waitEntered(t)

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	held.waitCanceled(t, closeDone)

	started := false
	err := s.readTransact(context.Background(), func(*transaction) error {
		started = true
		return nil
	})
	if started {
		t.Fatal("console read callback ran after Close began")
	}
	if !errors.Is(err, errStoreClosed) {
		t.Fatalf("read after Close began: got %v, want %v", err, errStoreClosed)
	}
	assertCloseStillWaiting(t, closeDone)
	assertLockHeld(t, lockPath)

	held.finish()
	select {
	case err := <-held.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight read returned %v, want context.Canceled", err)
		}
	case <-time.After(closeDrainWait):
		t.Fatal("in-flight read did not finish")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(closeDrainWait):
		t.Fatal("Close did not return after the console read finished")
	}
}

// TestCloseTwiceIsSafe is finding (c): a second Close does not panic, does not
// return an error, and leaves the database lock free for a successor.
func TestCloseTwiceIsSafe(t *testing.T) {
	s, _, _ := testStore(t)
	lockPath := s.lock.Name()
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if !databaseLockFree(lockPath) {
		t.Fatal("database lock stayed held after the first Close")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !databaseLockFree(lockPath) {
		t.Fatal("database lock stayed held after the second Close")
	}
	err := s.readTransact(context.Background(), func(*transaction) error { return nil })
	if !errors.Is(err, errStoreClosed) {
		t.Fatalf("read after Close: got %v, want %v", err, errStoreClosed)
	}
}
