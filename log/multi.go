package log

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// multiHandler fans a single slog record out to several child handlers so logs
// can be written to multiple destinations (e.g. stderr and a file) at once.
// The standard library has no built-in multi-handler, so we implement the
// slog.Handler interface by delegating to each child.
type multiHandler struct {
	handlers []slog.Handler
}

// Enabled reports whether any child handler is enabled for the given level.
func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle delivers the record to every child handler.
func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil {
			return err
		}
	}
	return nil
}

// WithAttrs returns a multiHandler whose children all carry the attributes.
func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: hs}
}

// WithGroup returns a multiHandler whose children all apply the group.
func (m *multiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: hs}
}

// SetFileLogger installs a logger that writes both to stderr and to a
// size-rotating file at path (default name xbridged.log, typically under
// -datadir). The file rotates when it exceeds maxBytes, retaining up to
// backups rotated generations. The active level (from SetLevel / -loglevel)
// continues to gate output on both destinations via the shared levelVar.
//
// It returns the underlying rotating writer so the caller can Close it on
// shutdown, or an error if the log file cannot be created.
func SetFileLogger(path string, maxBytes int64, backups int) (io.Closer, error) {
	rw, err := NewRotatingWriter(path, maxBytes, backups)
	if err != nil {
		return nil, err
	}
	fileHandler := slog.NewTextHandler(rw, &slog.HandlerOptions{Level: levelVar})
	stderrHandler := slog.NewTextHandler(stderrWriter(), &slog.HandlerOptions{Level: levelVar})
	logger.Store(slog.New(&multiHandler{handlers: []slog.Handler{stderrHandler, fileHandler}}))
	return rw, nil
}

// stderrWriter returns the process stderr for the console log handler. Kept as
// a small indirection so tests can substitute it if needed.
func stderrWriter() *os.File {
	return os.Stderr
}
