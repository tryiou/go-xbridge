package log

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rotatingWriter is an io.Writer that appends to a file and, once the file
// grows past maxBytes, rotates it: the current file is renamed to "<path>.1",
// any existing "<path>.1" is shifted to "<path>.2", and so on up to backups
// generations, then a fresh file is opened. It is safe for concurrent use.
//
// This is a deliberately minimal, dependency-free size-based rotator (no
// time-based rotation) matching go-xbridge's stdlib-only convention.
type rotatingWriter struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	maxBytes int64
	backups  int
	size     int64
}

// NewRotatingWriter opens (creating if needed) the file at path for appending
// and returns a writer that rotates when the file exceeds maxBytes. backups is
// the number of rotated generations to retain (e.g. 2 keeps "<path>.1" and
// "<path>.2"). The parent directory is created with 0700 if missing.
func NewRotatingWriter(path string, maxBytes int64, backups int) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("log: mkdir log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("log: open log file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("log: stat log file: %w", err)
	}
	return &rotatingWriter{
		f:        f,
		path:     path,
		maxBytes: maxBytes,
		backups:  backups,
		size:     fi.Size(),
	}, nil
}

// Write appends p, rotating first if doing so would exceed maxBytes. A Write
// after Close (e.g. a late log line racing daemon shutdown) is a no-op: the
// bytes are dropped rather than dereferencing the now-nil file.
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(p), nil
	}
	if w.maxBytes > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked closes and shifts the current file into the rotation chain,
// then opens a fresh file. Caller must hold w.mu.
func (w *rotatingWriter) rotateLocked() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("log: close log on rotate: %w", err)
	}
	// Shift existing backups: .backups-1 -> .backups ... 1 -> 2, then
	// current -> .1. Iterating high-to-low avoids clobbering.
	for i := w.backups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("log: shift backup %s: %w", src, err)
			}
		}
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return fmt.Errorf("log: rotate log: %w", err)
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("log: reopen log: %w", err)
	}
	w.f = f
	w.size = 0
	return nil
}

// Close flushes and closes the current log file (best-effort for daemon exit).
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
