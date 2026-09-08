package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestMultiHandler_Enabled verifies that Enabled returns true if ANY child
// handler has the level enabled.
func TestMultiHandler_Enabled(t *testing.T) {
	// debug handler and info handler
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelDebug})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelWarn})

	multi := newMultiHandler(h1, h2)

	// Both handlers enabled for Warn
	if !multi.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("expected Enabled(Warn)=true")
	}
	// Only h1 enabled for Debug
	if !multi.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected Enabled(Debug)=true because h1 accepts Debug")
	}
	// Error handler only
	var buf3 bytes.Buffer
	h3 := slog.NewTextHandler(&buf3, &slog.HandlerOptions{Level: slog.LevelError})
	onlyError := newMultiHandler(h3)
	if onlyError.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected Enabled(Debug)=false for error-only handler")
	}
}

// TestMultiHandler_Handle verifies that a log record is written to all children.
func TestMultiHandler_Handle(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelDebug})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})
	multi := newMultiHandler(h1, h2)

	logger := slog.New(multi)
	logger.Info("hello world")

	if !strings.Contains(buf1.String(), "hello world") {
		t.Errorf("h1 did not receive the log record: %q", buf1.String())
	}
	if !strings.Contains(buf2.String(), "hello world") {
		t.Errorf("h2 did not receive the log record: %q", buf2.String())
	}
}

// TestMultiHandler_WithAttrs verifies that WithAttrs propagates to all children.
func TestMultiHandler_WithAttrs(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelDebug})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})
	multi := newMultiHandler(h1, h2)

	withAttrs := multi.WithAttrs([]slog.Attr{slog.String("req_id", "abc123")})
	logger := slog.New(withAttrs)
	logger.Info("test attr")

	if !strings.Contains(buf1.String(), "req_id=abc123") {
		t.Errorf("h1 missing attribute: %q", buf1.String())
	}
	if !strings.Contains(buf2.String(), "req_id=abc123") {
		t.Errorf("h2 missing attribute: %q", buf2.String())
	}
}

// TestMultiHandler_WithGroup verifies that WithGroup propagates to all children.
func TestMultiHandler_WithGroup(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelDebug})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})
	multi := newMultiHandler(h1, h2)

	withGroup := multi.WithGroup("request")
	logger := slog.New(withGroup)
	logger.Info("grouped", "key", "value")

	if !strings.Contains(buf1.String(), "request.key=value") {
		t.Errorf("h1 missing group: %q", buf1.String())
	}
	if !strings.Contains(buf2.String(), "request.key=value") {
		t.Errorf("h2 missing group: %q", buf2.String())
	}
}

// TestLimitedBuffer_Cap verifies that limitedBuffer stops accepting writes at 1 MiB
// and sets the truncated flag.
func TestLimitedBuffer_Cap(t *testing.T) {
	var lb limitedBuffer

	// Write enough to fill the buffer: (1 MiB - 1) so we're just under.
	chunk1 := make([]byte, maxSinkBytes-1)
	n, err := lb.Write(chunk1)
	if err != nil || n != len(chunk1) {
		t.Fatalf("first write failed: n=%d err=%v", n, err)
	}
	if lb.truncated {
		t.Fatal("truncated should be false after first write")
	}

	// Second write: pushes us over the cap → should truncate.
	// Must return len(chunk2) and nil error to satisfy the io.Writer contract
	// (callers such as slog.NewTextHandler treat n < len(p) as an error).
	chunk2 := make([]byte, 100)
	n2, err2 := lb.Write(chunk2)
	if err2 != nil {
		t.Fatalf("second write returned error: %v", err2)
	}
	if n2 != len(chunk2) {
		t.Errorf("second write: got n=%d, want %d (io.Writer contract requires n==len(p) when err==nil)", n2, len(chunk2))
	}
	if !lb.truncated {
		t.Fatal("truncated should be true after exceeding cap")
	}

	// String() should include the truncation notice
	s := lb.String()
	if !strings.Contains(s, "[runnerOutput truncated at 1MiB]") {
		t.Errorf("missing truncation notice: %q", s)
	}
}
