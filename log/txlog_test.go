package log

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// txlogTestReset disables the transcript and restores the clock; every test
// that enables it defers this so transcript state never leaks between tests.
func txlogTestReset(t *testing.T) {
	t.Helper()
	oldNow := txlogNow
	t.Cleanup(func() {
		txlogNow = oldNow
		if err := SetTxLogDir(""); err != nil {
			t.Fatalf("SetTxLogDir reset: %v", err)
		}
	})
}

func TestTxLogDisabledByDefault(t *testing.T) {
	txlogTestReset(t)
	// No directory configured: must be a silent no-op, never a panic.
	TxLog("someid", "deposit transaction for order someid", "deadbeef")
}

func TestTxLogWritesDatedFile(t *testing.T) {
	txlogTestReset(t)
	dir := t.TempDir()
	txlogNow = func() time.Time { return time.Date(2026, 9, 10, 14, 2, 11, 0, time.Local) }
	if err := SetTxLogDir(filepath.Join(dir, "log-tx")); err != nil {
		t.Fatalf("SetTxLogDir: %v", err)
	}
	TxLog("abc123", "deposit transaction for order abc123 (submit manually using sendrawtransaction)", "deadbeef")

	data, err := os.ReadFile(filepath.Join(dir, "log-tx", "xbridgep2p_20260910.log"))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "2026-09-10 14:02:11 order abc123 deposit transaction for order abc123") {
		t.Errorf("missing timestamped header, got:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "deadbeef") {
		t.Errorf("hex must be the verbatim last line, got:\n%s", got)
	}
}

func TestTxLogAppendsAndRollsDay(t *testing.T) {
	txlogTestReset(t)
	dir := filepath.Join(t.TempDir(), "log-tx")
	day1 := time.Date(2026, 9, 10, 23, 59, 59, 0, time.Local)
	day2 := time.Date(2026, 9, 11, 0, 0, 1, 0, time.Local)
	txlogNow = func() time.Time { return day1 }
	if err := SetTxLogDir(dir); err != nil {
		t.Fatalf("SetTxLogDir: %v", err)
	}
	TxLog("id1", "first")
	TxLog("id1", "second")
	txlogNow = func() time.Time { return day2 }
	TxLog("id2", "third")

	d1, err := os.ReadFile(filepath.Join(dir, "xbridgep2p_20260910.log"))
	if err != nil {
		t.Fatalf("read day1: %v", err)
	}
	if strings.Count(string(d1), "\n") != 2 {
		t.Errorf("day1 lines = %d, want 2 appended", strings.Count(string(d1), "\n"))
	}
	d2, err := os.ReadFile(filepath.Join(dir, "xbridgep2p_20260911.log"))
	if err != nil {
		t.Fatalf("read day2: %v", err)
	}
	if !strings.Contains(string(d2), "order id2 third") {
		t.Errorf("day2 missing rolled entry, got:\n%s", d2)
	}
}

func TestTxLogPermissions(t *testing.T) {
	txlogTestReset(t)
	// Pin the clock: wall-clock dates flake across a midnight rollover.
	txlogNow = func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.Local) }
	dir := filepath.Join(t.TempDir(), "log-tx")
	if err := SetTxLogDir(dir); err != nil {
		t.Fatalf("SetTxLogDir: %v", err)
	}
	TxLog("id", "entry")

	fi, err := os.Stat(filepath.Join(dir, txlogFilename(txlogNow())))
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("transcript perms = %o, want 600", fi.Mode().Perm())
	}
	if st, err := os.Stat(dir); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if st.Mode().Perm() != 0700 {
		t.Errorf("dir perms = %o, want 700", st.Mode().Perm())
	}
}

func TestTxLogConcurrent(t *testing.T) {
	txlogTestReset(t)
	txlogNow = func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.Local) }
	dir := filepath.Join(t.TempDir(), "log-tx")
	if err := SetTxLogDir(dir); err != nil {
		t.Fatalf("SetTxLogDir: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			TxLog("id", "concurrent entry", "hexline")
		}()
	}
	wg.Wait() // run with -race: no data race on day/file state
	data, err := os.ReadFile(filepath.Join(dir, txlogFilename(txlogNow())))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if strings.Count(string(data), "concurrent entry") != 16 {
		t.Errorf("entries = %d, want 16 (no lost writes)", strings.Count(string(data), "concurrent entry"))
	}
}

// TestTxLogTightensExistingPerms proves setup and open re-tighten a
// pre-existing loose directory/file: operator-created 0755/0644 must not
// survive holding (or about to hold) refund hex.
func TestTxLogTightensExistingPerms(t *testing.T) {
	txlogTestReset(t)
	txlogNow = func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.Local) }
	dir := filepath.Join(t.TempDir(), "log-tx")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	loose := filepath.Join(dir, txlogFilename(txlogNow()))
	if err := os.WriteFile(loose, []byte("prior\n"), 0644); err != nil {
		t.Fatalf("write loose file: %v", err)
	}
	if err := SetTxLogDir(dir); err != nil {
		t.Fatalf("SetTxLogDir: %v", err)
	}
	TxLog("id", "entry")
	if st, err := os.Stat(dir); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if st.Mode().Perm() != 0700 {
		t.Errorf("dir perms = %o, want 700", st.Mode().Perm())
	}
	if fi, err := os.Stat(loose); err != nil {
		t.Fatalf("stat file: %v", err)
	} else if fi.Mode().Perm() != 0600 {
		t.Errorf("file perms = %o, want 600", fi.Mode().Perm())
	}
}
