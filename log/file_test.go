package log

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotatingWriter_BasicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xbridged.log")

	rw, err := NewRotatingWriter(path, 1<<20, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	msg := "hello\n"
	if _, err := rw.Write([]byte(msg)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != msg {
		t.Fatalf("got %q, want %q", string(data), msg)
	}
}

func TestRotatingWriter_Rotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xbridged.log")

	// Tiny threshold so a few writes trigger rotation; keep 1 backup.
	const maxBytes int64 = 20
	rw, err := NewRotatingWriter(path, maxBytes, 1)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	// Each write is 10 bytes; after 2 writes (20) the 3rd forces a rotate.
	for i := 0; i < 3; i++ {
		if _, err := rw.Write([]byte("0123456789")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// Current file should have the most recent writes (>= 1 write, <= 2).
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile current: %v", err)
	}
	if len(cur) == 0 || len(cur) > 20 {
		t.Fatalf("current file size unexpected: %d", len(cur))
	}

	// The rotated backup must exist and contain the very first write.
	backup := path + ".1"
	bdata, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("ReadFile backup: %v", err)
	}
	if !strings.Contains(string(bdata), "0123456789") {
		t.Fatalf("backup missing first write: %q", string(bdata))
	}
}

func TestRotatingWriter_BackupShift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xbridged.log")

	const maxBytes int64 = 10
	rw, err := NewRotatingWriter(path, maxBytes, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	// Four 10-byte writes => 3 rotations => .1 and .2 should both exist.
	for i := 0; i < 4; i++ {
		if _, err := rw.Write([]byte("AAAAAAAAAA")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("backup .1 should exist: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("backup .2 should exist: %v", err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("backup .3 should NOT exist: %v", err)
	}
}

// TestRotatingWriter_WriteAfterClose locks in the no-op-after-close contract: a
// late Write (e.g. a log line racing daemon shutdown) must not panic or error,
// and a second Close must be a no-op — the log path stays nil-safe.
func TestRotatingWriter_WriteAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xbridged.log")

	rw, err := NewRotatingWriter(path, 1<<20, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	if _, err := rw.Write([]byte("before\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Write after close: dropped, never an error or panic.
	const msg = "after\n"
	if n, err := rw.Write([]byte(msg)); err != nil || n != len(msg) {
		t.Fatalf("Write after close = (%d, %v), want (%d, nil)", n, err, len(msg))
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "before\n" {
		t.Fatalf("file = %q, want %q (post-close write must not land)", data, "before\n")
	}
}

// TestRotatingWriter_ConcurrentCloseWrite races writers against Close so a
// shutdown-path regression (nil-file deref, data race) shows up under -race.
func TestRotatingWriter_ConcurrentCloseWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xbridged.log")

	rw, err := NewRotatingWriter(path, 1<<20, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = rw.Write([]byte("line\n"))
			}
		}()
	}
	_ = rw.Close()
	wg.Wait()
}
