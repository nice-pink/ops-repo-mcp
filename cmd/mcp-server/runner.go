package main

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

// callRunner acquires the per-repo lock, attaches a per-call slog sink, and runs
// fn in a goroutine. Lock ownership transfers to the goroutine — the goroutine
// releases the lock when fn returns (or panics). The handler never calls Unlock.
//
// Invariants:
//  1. The lock is released exactly once, by the goroutine's defer.
//  2. No concurrent runner calls are possible: the lock spans from Lock() in the
//     handler through to the goroutine's defer Unlock().
//  3. On RUNNER_TIMEOUT the goroutine is leaked but still holds the lock. The
//     next tool call will block on Lock() and eventually return LOCK_TIMEOUT.
//  4. slog.Default() is always restored before callRunner returns.
func (h *handler) callRunner(parentCtx context.Context, toolName string, fn func() error) (runnerOutput string, err error) {
	// Acquire the per-repo lock; bound the wait by MCP_LOCK_TIMEOUT.
	lockCtx, cancel := context.WithTimeout(parentCtx, h.cfg.LockTimeout)
	defer cancel()

	slog.Default().Info("lock_wait", "repo", h.cfg.OpsRepoPath, "timeout_s", h.cfg.LockTimeout.Seconds())
	if lockErr := h.repoMu.Lock(lockCtx); lockErr != nil {
		slog.Default().Error("lock_timeout", "repo", h.cfg.OpsRepoPath)
		return "", errLockTimeout(lockErr)
	}
	// NOTE: do NOT defer repoMu.Unlock() here.
	// Lock ownership transfers to the runner goroutine.

	// Per-call slog sink (set BEFORE goroutine spawns so the runner inherits it).
	var buf limitedBuffer
	sink := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	prev := slog.Default()
	slog.SetDefault(slog.New(newMultiHandler(prev.Handler(), sink)))

	type result struct{ err error }
	ch := make(chan result, 1)

	start := time.Now()
	go func() {
		// Goroutine owns the lock — releases it when fn returns or panics.
		defer h.repoMu.Unlock()

		// Panic recovery: turn a panic in pkg/runner into RUNNER_PANIC.
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("runner_panic",
					"tool", toolName,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				ch <- result{err: errRunnerPanic(r)}
			}
		}()

		ch <- result{err: fn()}
	}()

	select {
	case r := <-ch:
		elapsed := time.Since(start)
		slog.SetDefault(prev)
		if r.err != nil {
			slog.Default().Error("runner_failed", "tool", toolName, "err", r.err.Error(), "duration_ms", elapsed.Milliseconds())
		} else {
			slog.Default().Info("runner_ok", "tool", toolName, "duration_ms", elapsed.Milliseconds())
		}
		return buf.String(), r.err

	case <-time.After(h.cfg.RunnerTimeout):
		// Goroutine is leaked and still holds the lock. Restore slog so subsequent
		// MCP code does not write into the orphaned sink buffer.
		slog.SetDefault(prev)
		slog.Default().Warn("runner_timeout_leaked",
			"tool", toolName,
			"timeout_s", h.cfg.RunnerTimeout.Seconds(),
		)
		return buf.String(), errRunnerTimeout()
	}
}
