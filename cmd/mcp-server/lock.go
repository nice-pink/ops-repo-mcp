package main

import "context"

// timedMu is a channel-based semaphore with capacity 1 that supports
// context-cancellable acquire. Unlike sync.Mutex, Lock accepts a context so
// the caller can bound how long it waits before giving up.
//
// Invariant: Unlock is called exactly once per successful Lock, and always by
// the goroutine that will perform the mutation — never by the goroutine that
// called Lock. (See callRunner for the ownership transfer pattern.)
type timedMu struct {
	ch chan struct{}
}

func newTimedMu() *timedMu {
	m := &timedMu{ch: make(chan struct{}, 1)}
	m.ch <- struct{}{} // start unlocked
	return m
}

// Lock acquires the semaphore, blocking until it is available or ctx is done.
// Returns ctx.Err() if the context is cancelled before the lock is acquired.
func (m *timedMu) Lock(ctx context.Context) error {
	select {
	case <-m.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock releases the semaphore. Panics if called more times than Lock.
func (m *timedMu) Unlock() {
	m.ch <- struct{}{}
}
