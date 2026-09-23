// Package log is go-xbridge's structured, level-based logging facade.
//
// It wraps the standard library's log/slog with a package-level logger whose
// level can be changed at runtime (via a slog.LevelVar) and whose handler can be
// replaced by an embedding application (via SetLogger). The four levels map 1:1
// onto Python's logging module:
//
//	Python logging   slog level        helper
//	--------------   --------------    --------
//	DEBUG   (10)     LevelDebug (-4)   log.Debug
//	INFO    (20)     LevelInfo   (0)   log.Info
//	WARNING (30)     LevelWarn   (4)   log.Warn
//	ERROR   (40)     LevelError  (8)   log.Error
//
// Log calls take a message and alternating key/value pairs (structured fields),
// the idiomatic Go equivalent of Python's logger.info(msg, extra={...}):
//
//	log.Info("deposit broadcast", "session", id, "txid", txid, "cur", cur)
//
// The default logger writes text to stderr at DEBUG (beta stage: full
// verbosity by default so field issues arrive with evidence). Call SetLevel
// (or the -loglevel flag in cmd/xbridged) to change verbosity; INFO
// suppresses DEBUG, WARN suppresses INFO+DEBUG, and so on.
package log

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// levelVar holds the active minimum level; changing it re-filters all logging
// through the shared handler without rebuilding the logger.
var levelVar = new(slog.LevelVar)

// logger is the active *slog.Logger, swappable via SetLogger. An atomic.Pointer
// makes concurrent reads (from the feed goroutine, RPC handlers, watchers) safe.
var logger atomic.Pointer[slog.Logger]

func init() {
	levelVar.Set(slog.LevelDebug)
	logger.Store(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: levelVar,
	})))
}

// SetLevel sets the minimum level emitted by the default logger. It is a no-op
// override once SetLogger has installed a custom logger with its own leveling,
// but it still updates the shared LevelVar for handlers that reference it.
func SetLevel(l slog.Level) { levelVar.Set(l) }

// Level returns the current minimum level.
func Level() slog.Level { return levelVar.Level() }

// SetLogger installs a custom logger (e.g. from an embedding dapp). Pass nil to
// restore the default stderr text logger at the current level.
func SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar}))
	}
	logger.Store(l)
}

// L returns the active logger (never nil).
func L() *slog.Logger { return logger.Load() }

// With returns a logger that includes the given key/value pairs on every record
// — the equivalent of a Python logging adapter carrying contextual fields.
func With(args ...any) *slog.Logger { return logger.Load().With(args...) }

// ParseLevel maps a case-insensitive level name ("debug"/"info"/"warn"/"error",
// plus the aliases "warning" and "err") to an slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("log: unknown level %q (want debug|info|warn|error)", s)
	}
}

// Debug/Info/Warn/Error emit a record at the named level with structured fields.
func Debug(msg string, args ...any) { logger.Load().Debug(msg, args...) }
func Info(msg string, args ...any)  { logger.Load().Info(msg, args...) }
func Warn(msg string, args ...any)  { logger.Load().Warn(msg, args...) }
func Error(msg string, args ...any) { logger.Load().Error(msg, args...) }
