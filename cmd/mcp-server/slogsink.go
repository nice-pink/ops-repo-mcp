package main

import (
	"bytes"
	"context"
	"log/slog"
)

// multiHandler is a slog.Handler that fans out every log record to two or
// more child handlers. All four interface methods propagate to both children.
// This is used to simultaneously write to stderr (the persistent default) and
// a per-call buffer that feeds runnerOutput in the MCP response.
type multiHandler struct {
	children []slog.Handler
}

func newMultiHandler(hs ...slog.Handler) *multiHandler {
	return &multiHandler{children: hs}
}

func (m *multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.children {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.children {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(m.children))
	for i, h := range m.children {
		out[i] = h.WithAttrs(as)
	}
	return &multiHandler{children: out}
}

func (m *multiHandler) WithGroup(g string) slog.Handler {
	out := make([]slog.Handler, len(m.children))
	for i, h := range m.children {
		out[i] = h.WithGroup(g)
	}
	return &multiHandler{children: out}
}

const maxSinkBytes = 1 << 20 // 1 MiB

// limitedBuffer wraps bytes.Buffer with a 1 MiB hard cap. Once the cap is
// reached, further writes are silently discarded and the truncated flag is set.
// The caller appends a truncation notice to the returned string when the flag
// is set, so the LLM knows the buffer was cut.
type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.truncated {
		return len(p), nil // discard silently
	}
	remaining := maxSinkBytes - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		// Write only as much as fits. Always return len(p) so callers (e.g.
		// slog.NewTextHandler via bufio.Writer) do not see a short write and
		// treat it as an error — the truncation is intentional.
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	s := b.buf.String()
	if b.truncated {
		s += "\n[runnerOutput truncated at 1MiB]\n"
	}
	return s
}
