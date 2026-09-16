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

// filterHandler wraps a slog.Handler and routes records by a "p2p" marker
// attribute. When allowP2P is true, only records carrying "p2p" pass through;
// when false, records carrying "p2p" are dropped.
//
// The slog contract stores WithAttrs attributes in the handler, not the
// Record, so this handler intercepts WithAttrs to detect the "p2p" key and
// sets hasP2P on the returned child. Handle checks both the stored flag and
// the record's own attrs (for direct Log/Info calls carrying "p2p" inline)
// before delegating to the inner handler.
type filterHandler struct {
	allowP2P bool
	hasP2P   bool
	inner    slog.Handler
}

func (f *filterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return f.inner.Enabled(ctx, level)
}

func (f *filterHandler) Handle(ctx context.Context, r slog.Record) error {
	hasP2P := f.hasP2P
	if !hasP2P {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "p2p" {
				hasP2P = true
				return false
			}
			return true
		})
	}
	if f.allowP2P && !hasP2P {
		return nil
	}
	if !f.allowP2P && hasP2P {
		return nil
	}
	return f.inner.Handle(ctx, r)
}

func (f *filterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hasMarker := f.hasP2P
	for _, a := range attrs {
		if a.Key == "p2p" {
			hasMarker = true
			break
		}
	}
	return &filterHandler{allowP2P: f.allowP2P, hasP2P: hasMarker, inner: f.inner.WithAttrs(attrs)}
}

func (f *filterHandler) WithGroup(name string) slog.Handler {
	return &filterHandler{allowP2P: f.allowP2P, hasP2P: f.hasP2P, inner: f.inner.WithGroup(name)}
}

// SetP2PLogFile installs a packet-only file handler into the global logger.
// The existing destinations (stderr + general log file) are wrapped to drop
// records carrying the "p2p" marker, while the new packet file receives only
// those records. The packet file uses the same size-based rotation as the
// general log (rotatingWriter with maxBytes/backups). The active level (from
// SetLevel / -loglevel) gates the packet file via the shared levelVar.
//
// It returns the underlying rotating writer so the caller can Close it on
// shutdown, or an error if the log file cannot be created.
func SetP2PLogFile(path string, maxBytes int64, backups int) (io.Closer, error) {
	rw, err := NewRotatingWriter(path, maxBytes, backups)
	if err != nil {
		return nil, err
	}
	pktHandler := &filterHandler{allowP2P: true, inner: slog.NewTextHandler(rw, &slog.HandlerOptions{Level: levelVar})}
	current := logger.Load().Handler()
	genFiltered := &filterHandler{allowP2P: false, inner: current}
	logger.Store(slog.New(&multiHandler{handlers: []slog.Handler{genFiltered, pktHandler}}))
	return rw, nil
}

// stderrWriter returns the process stderr for the console log handler. Kept as
// a small indirection so tests can substitute it if needed.
func stderrWriter() *os.File {
	return os.Stderr
}
