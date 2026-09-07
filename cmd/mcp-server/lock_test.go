package main

import (
	"context"
	"testing"
	"time"
)

func TestTimedMu_AcquireRelease(t *testing.T) {
	mu := newTimedMu()

	ctx := context.Background()
	if err := mu.Lock(ctx); err != nil {
		t.Fatalf("Lock failed: %v", err)
	}
	mu.Unlock()

	// Should be acquirable again after unlock.
	if err := mu.Lock(ctx); err != nil {
		t.Fatalf("second Lock failed: %v", err)
	}
	mu.Unlock()
}

func TestTimedMu_ContextTimeout(t *testing.T) {
	mu := newTimedMu()

	// Acquire and hold.
	if err := mu.Lock(context.Background()); err != nil {
		t.Fatalf("initial Lock failed: %v", err)
	}

	// A second acquire should fail once the context deadline expires.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := mu.Lock(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected Lock to fail with context timeout, got nil")
	}
	if elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("timeout elapsed unexpectedly: %v", elapsed)
	}

	// Unlock so we don't leave the semaphore permanently held.
	mu.Unlock()
}
