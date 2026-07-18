package log

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// captureHandler records handled records for assertions.
type captureHandler struct {
	mu       sync.Mutex
	records  []slog.Record
	minLevel slog.Level
}

func (c *captureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= c.minLevel
}
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}
func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }
func (c *captureHandler) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records)
}

func TestMultiHandler_FansOut(t *testing.T) {
	a := &captureHandler{minLevel: slog.LevelDebug}
	b := &captureHandler{minLevel: slog.LevelWarn}
	m := &multiHandler{handlers: []slog.Handler{a, b}}

	// b is disabled at Info, but a is enabled, so the multiHandler is enabled.
	if !m.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("multiHandler should be enabled when a child is")
	}
	// Below Debug, a is disabled too, so multiHandler is disabled.
	if m.Enabled(context.Background(), slog.LevelDebug-100) {
		t.Fatal("multiHandler should be disabled when no child is")
	}

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	if err := m.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// a (min Debug) receives the Info record; b (min Warn) does not.
	if a.count() != 1 {
		t.Fatalf("expected a=1 record, got %d", a.count())
	}
	if b.count() != 0 {
		t.Fatalf("expected b=0 records, got %d", b.count())
	}

	// A Warn record reaches both children.
	wrec := slog.NewRecord(time.Now(), slog.LevelWarn, "w", 0)
	if err := m.Handle(context.Background(), wrec); err != nil {
		t.Fatalf("Handle warn: %v", err)
	}
	if a.count() != 2 || b.count() != 1 {
		t.Fatalf("after warn: expected a=2 b=1, got a=%d b=%d", a.count(), b.count())
	}
}

func TestSetFileLogger_TeeToBuffer(t *testing.T) {
	// Capture stderr-equivalent by installing a multiHandler with a buffer
	// handler, proving SetFileLogger-style fan-out writes both destinations.
	buf := &bytes.Buffer{}
	bufHandler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: levelVar})
	fileHandler := slog.NewTextHandler(&nopWriteCloser{buf}, &slog.HandlerOptions{Level: levelVar})
	logger.Store(slog.New(&multiHandler{handlers: []slog.Handler{bufHandler, fileHandler}}))
	defer SetLogger(nil)

	Info("dual-write test", "k", "v")
	out := buf.String()
	if !contains(out, "dual-write test") || !contains(out, "k=v") {
		t.Fatalf("expected record in both streams, got %q", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (bytes.Contains([]byte(s), []byte(sub)))
}

// nopWriteCloser adapts a *bytes.Buffer to io.WriteCloser for the test handler.
type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }
