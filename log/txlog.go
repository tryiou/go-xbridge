package log

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Dedicated per-day swap transcript, the go-xbridge analog of Core's
// log-tx/xbridgep2p_YYYYMMDD.log (util/txlog.cpp): every broadcast deposit,
// pre-signed refund, broadcast claim, and broadcast refund lands here with the
// order id, locktime, and full raw hex, so a user can always complete a manual
// refund with the deposit currency's own wallet (sendrawtransaction) even if
// the daemon and its swap-state file are both gone.
//
// The transcript is written to <dir>/xbridgep2p_YYYYMMDD.log (local date),
// one file per day, appended (0600) under a 0700 directory. Daily files are
// never deleted or rotated by the daemon — the operator prunes them. What is
// deliberately NEVER written here: per-trade private keys and HTLC preimages
// (Core logs those via LOG_KEYPAIR_VALUES; that is a vulnerability, not a
// recovery feature — the pre-signed refund hex suffices for manual recovery).
//
// TxLog is best-effort by design: a transcript failure must never fail a
// trade. (Distinction: the daemon still refuses to START when the transcript
// directory cannot even be created — a broken datadir, same as the main log
// file — but no running swap ever fails because a transcript write failed.)
// With no directory configured (library/test use) it is a silent
// no-op; write failures are reported once per failure through the main logger
// (never through the transcript itself, so there is no recursion).

// txlogNow is the clock for the daily filename, a var so tests can force a
// day rollover without waiting for midnight.
var txlogNow = time.Now

// txlogMu serializes transcript state (directory, open day/file).
var txlogMu sync.Mutex

// txlogDir is the configured transcript directory ("" = disabled).
var txlogDir string

// txlogDay is the YYYYMMDD the open file belongs to; txlogFile is its handle
// (nil when closed or never opened).
var txlogDay string
var txlogFile *os.File

// txlogFilename formats the daily transcript name for t (local date, Core
// xbridgep2p_YYYYMMDD.log naming).
func txlogFilename(t time.Time) string {
	return "xbridgep2p_" + t.Format("20060102") + ".log"
}

// SetTxLogDir enables the swap transcript under dir (the daemon passes
// <datadir>/log-tx). An empty dir disables the transcript (test/library use).
func SetTxLogDir(dir string) error {
	txlogMu.Lock()
	defer txlogMu.Unlock()
	if txlogFile != nil {
		_ = txlogFile.Close()
		txlogFile = nil
		txlogDay = ""
	}
	txlogDir = dir
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		txlogDir = ""
		return fmt.Errorf("log: mkdir txlog dir: %w", err)
	}
	// Tighten a pre-existing directory: MkdirAll leaves existing perms alone.
	if err := os.Chmod(dir, 0700); err != nil {
		txlogDir = ""
		return fmt.Errorf("log: chmod txlog dir: %w", err)
	}
	return nil
}

// CloseTxLog closes the open transcript file, if any. A later TxLog reopens
// the transcript on demand (the directory stays configured) — close is purely
// a shutdown flush, not a latch. The daemon calls this after HTTP drain and
// node close, so no swap path can write after it in practice; safe to call
// twice.
func CloseTxLog() {
	txlogMu.Lock()
	defer txlogMu.Unlock()
	if txlogFile != nil {
		_ = txlogFile.Close()
		txlogFile = nil
		txlogDay = ""
	}
}

// txlogFileLocked opens (creating if needed) the transcript for the current
// day, rolling over when the date changed since the last write. Caller must
// hold txlogMu.
func txlogFileLocked() (*os.File, error) {
	day := txlogFilename(txlogNow())
	if txlogFile != nil && txlogDay == day {
		return txlogFile, nil
	}
	if txlogFile != nil {
		_ = txlogFile.Close()
		txlogFile = nil
	}
	f, err := os.OpenFile(filepath.Join(txlogDir, day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	// Tighten a pre-existing file: OpenFile leaves existing perms alone. A
	// chmod failure is non-fatal (the file is still usable); it is reported
	// through the main logger so a loose transcript never goes unnoticed.
	if err := f.Chmod(0600); err != nil {
		Warn("txlog chmod failed", "dir", txlogDir, "err", err)
	}
	txlogFile = f
	txlogDay = day
	return f, nil
}

// TxLog appends one transcript entry: a timestamped header line carrying the
// display order id, then each body line verbatim (raw hex goes on its own
// line, Core TXLOG style). A header example:
//
//	2026-09-10 14:02:11 deposit transaction for order abc... (submit manually using sendrawtransaction) ...
func TxLog(orderID, header string, body ...string) {
	txlogMu.Lock()
	defer txlogMu.Unlock()
	if txlogDir == "" {
		return // transcript disabled: library/test use
	}
	f, err := txlogFileLocked()
	if err != nil {
		Warn("txlog write failed", "dir", txlogDir, "err", err)
		return
	}
	ts := txlogNow().Format("2006-01-02 15:04:05")
	out := ts + " order " + orderID + " " + header + "\n"
	for _, b := range body {
		out += b + "\n"
	}
	if _, err := f.WriteString(out); err != nil {
		Warn("txlog write failed", "dir", txlogDir, "err", err)
	}
}
