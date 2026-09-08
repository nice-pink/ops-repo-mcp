package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestRunnerPanicRecovery verifies that a panic inside fn is converted to a
// RUNNER_PANIC error and does NOT propagate to the caller as a Go panic.
func TestRunnerPanicRecovery(t *testing.T) {
	h := &handler{
		cfg:    serverConfig{LockTimeout: 5 * time.Second, RunnerTimeout: 5 * time.Second},
		repoMu: newTimedMu(),
	}

	// Suppress slog output during test
	setBaseHandler(slog.NewTextHandler(noopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))

	fn := func() error {
		panic("synthetic test panic")
	}

	_, err := h.callRunner(context.Background(), "test", fn)
	if err == nil {
		t.Fatal("expected error from panicking fn, got nil")
	}
	me, ok := err.(*mcpError)
	if !ok {
		t.Fatalf("expected *mcpError, got %T: %v", err, err)
	}
	if me.code != codeRunnerPanic {
		t.Errorf("expected code %s, got %s", codeRunnerPanic, me.code)
	}
}

// TestRunnerTimeoutLockOwnership is the load-bearing regression guard.
// It verifies that:
//  1. A slow fn causes RUNNER_TIMEOUT to be returned.
//  2. The lock remains held until fn completes (so a second call queues, not races).
func TestRunnerTimeoutLockOwnership(t *testing.T) {
	lockTimeout := 200 * time.Millisecond
	runnerTimeout := 50 * time.Millisecond

	h := &handler{
		cfg: serverConfig{
			LockTimeout:   lockTimeout,
			RunnerTimeout: runnerTimeout,
		},
		repoMu: newTimedMu(),
	}

	setBaseHandler(slog.NewTextHandler(noopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))

	// Channel to signal when the slow goroutine has started.
	started := make(chan struct{})
	// Channel to release the slow goroutine.
	release := make(chan struct{})

	slowFn := func() error {
		close(started)
		<-release // block until told to continue
		return nil
	}

	// Fire first call in background — it will time out.
	call1Done := make(chan error, 1)
	go func() {
		_, err := h.callRunner(context.Background(), "slow", slowFn)
		call1Done <- err
	}()

	// Wait for slowFn to start (lock is now held by the goroutine).
	<-started

	// Wait for RUNNER_TIMEOUT
	err1 := <-call1Done
	if err1 == nil {
		t.Fatal("expected RUNNER_TIMEOUT error from first call")
	}
	me, ok := err1.(*mcpError)
	if !ok || me.code != codeRunnerTimeout {
		t.Errorf("expected RUNNER_TIMEOUT, got %v", err1)
	}

	// Now fire a second call. The lock is still held by the leaked goroutine.
	// The second call should queue and return LOCK_TIMEOUT (not race with the first).
	call2Start := time.Now()
	_, err2 := h.callRunner(context.Background(), "second", func() error { return nil })
	call2Elapsed := time.Since(call2Start)

	// Release the first goroutine so the lock is returned.
	close(release)

	// The second call should have waited at least some portion of lockTimeout.
	if call2Elapsed < 10*time.Millisecond {
		t.Errorf("second call returned suspiciously fast (%v); expected it to block on the lock", call2Elapsed)
	}

	// The second call should return LOCK_TIMEOUT (because the slow goroutine
	// still held the lock when lock acquisition timed out) OR succeed (if the
	// goroutine completed between the timeout and the second acquire).
	// Either outcome is valid; what's invalid is a race or panic.
	if err2 != nil {
		me2, ok := err2.(*mcpError)
		if !ok {
			t.Errorf("second call returned non-mcpError: %v", err2)
		} else if me2.code != codeLockTimeout && me2.code != codeRunnerTimeout {
			t.Errorf("unexpected error code from second call: %s", me2.code)
		}
	}
	// No further assertions needed — if we got here without a race or panic, the test passes.
}

// noopWriter discards all log output.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestCallRunnerComposesWithBaseHandler pins the fix for a deadlock that hung
// CI for the full 10-minute test timeout.
//
// callRunner used to build its per-call multiHandler from slog.Default(). In a
// test binary main() never runs, so slog.Default() was slog's zero-value
// handler, which emits through the standard log package. slog.SetDefault then
// redirects that same log package back into the composition, so the first
// record logged by the runner re-entered log.Logger's non-reentrant mutex and
// deadlocked: log.Output -> multiHandler -> default handler -> log.Output.
//
// The invariant is that callRunner composes with the explicitly captured base
// handler and never with whatever slog.Default() happens to be. Asserting that
// the record reaches the base and not the ambient default is what keeps a
// future edit from reintroducing the cycle.
func TestCallRunnerComposesWithBaseHandler(t *testing.T) {
	h := &handler{
		cfg:    serverConfig{LockTimeout: 2 * time.Second, RunnerTimeout: 5 * time.Second},
		repoMu: newTimedMu(),
	}

	prevBase, prevDefault := baseHandler, slog.Default()
	t.Cleanup(func() { baseHandler = prevBase; slog.SetDefault(prevDefault) })

	var base, ambient bytes.Buffer
	setBaseHandler(slog.NewTextHandler(&base, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// An ambient default distinct from the base: callRunner must ignore it.
	slog.SetDefault(slog.New(slog.NewTextHandler(&ambient, &slog.HandlerOptions{Level: slog.LevelDebug})))

	done := make(chan string, 1)
	go func() {
		out, _ := h.callRunner(context.Background(), "test", func() error {
			slog.Default().Error("marker-line")
			return nil
		})
		done <- out
	}()

	var runnerOutput string
	select {
	case runnerOutput = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("callRunner deadlocked; the per-call sink is composed with a handler that re-enters the log package")
	}

	if !strings.Contains(runnerOutput, "marker-line") {
		t.Errorf("per-call sink did not capture the record; got %q", runnerOutput)
	}
	if !strings.Contains(base.String(), "marker-line") {
		t.Errorf("base handler did not receive the record; got %q", base.String())
	}
	if strings.Contains(ambient.String(), "marker-line") {
		t.Error("callRunner composed with slog.Default() instead of the captured base handler")
	}
}

// TestCurrentBaseHandlerFallbackWritesDirectly guards the other half: with no
// base captured, the fallback must be a direct-writer handler. A fallback that
// routed through the log package would re-arm the same deadlock.
func TestCurrentBaseHandlerFallbackWritesDirectly(t *testing.T) {
	prev := baseHandler
	t.Cleanup(func() { baseHandler = prev })

	baseHandler = nil
	if _, ok := currentBaseHandler().(*slog.TextHandler); !ok {
		t.Fatalf("fallback base handler must be a *slog.TextHandler, got %T", currentBaseHandler())
	}
}
