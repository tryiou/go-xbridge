package log

import (
	"bytes"
	"context"
	"log/slog"
	"os"
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

// TestSetP2PLogFile_SplitsDestinations verifies the file wiring: after
// SetFileLogger + SetP2PLogFile, an untagged record lands in the general
// file (and not the packet file), while a "p2p"-tagged record lands in the
// packet file (and not the general file).
func TestSetP2PLogFile_SplitsDestinations(t *testing.T) {
	dir := t.TempDir()
	genPath := dir + "/xbridged.log"
	pktPath := dir + "/log-p2p/xbridgep2p.log"

	genRw, err := SetFileLogger(genPath, 1<<20, 2)
	if err != nil {
		t.Fatalf("SetFileLogger: %v", err)
	}
	defer SetLogger(nil)
	pktRw, err := SetP2PLogFile(pktPath, 1<<20, 2)
	if err != nil {
		t.Fatalf("SetP2PLogFile: %v", err)
	}
	defer SetLogger(nil)

	Info("general message", "k", "v")
	With("p2p", true).Info("packet message", "command", "HoldApply")

	if err := genRw.Close(); err != nil {
		t.Fatalf("close general: %v", err)
	}
	if err := pktRw.Close(); err != nil {
		t.Fatalf("close packet: %v", err)
	}

	genData, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatalf("ReadFile general: %v", err)
	}
	if !contains(string(genData), "general message") {
		t.Fatalf("general file missing untagged record: %q", genData)
	}
	if contains(string(genData), "packet message") {
		t.Fatalf("general file must not contain tagged record: %q", genData)
	}

	pktData, err := os.ReadFile(pktPath)
	if err != nil {
		t.Fatalf("ReadFile packet: %v", err)
	}
	if !contains(string(pktData), "packet message") {
		t.Fatalf("packet file missing tagged record: %q", pktData)
	}
	if contains(string(pktData), "general message") {
		t.Fatalf("packet file must not contain untagged record: %q", pktData)
	}
}

// TestFilterHandler_RoutesByTag locks in the packet-log routing contract: a
// record carrying the "p2p" marker passes a filterHandler with allowP2P=true
// and is dropped by one with allowP2P=false; an untagged record does the
// reverse. Both the WithAttrs path (xlog.With("p2p", true)) and the inline
// record-attr path are covered, plus WithGroup flag preservation.
func TestFilterHandler_RoutesByTag(t *testing.T) {
	newPair := func() (*captureHandler, *captureHandler, *slog.Logger, *slog.Logger) {
		genCap := &captureHandler{minLevel: slog.LevelDebug}
		pktCap := &captureHandler{minLevel: slog.LevelDebug}
		gen := slog.New(&filterHandler{allowP2P: false, inner: genCap})
		pkt := slog.New(&filterHandler{allowP2P: true, inner: pktCap})
		return genCap, pktCap, gen, pkt
	}

	// Tagged via With: packet handler passes, general handler drops.
	genCap, pktCap, gen, pkt := newPair()
	pkt.With("p2p", true).Info("encode xbridge payload", "packetLen", 129)
	if pktCap.count() != 1 {
		t.Fatalf("tagged record: packet handler got %d, want 1", pktCap.count())
	}
	gen.With("p2p", true).Info("encode xbridge payload", "packetLen", 129)
	if genCap.count() != 0 {
		t.Fatalf("tagged record: general handler got %d, want 0", genCap.count())
	}

	// Untagged: general handler passes, packet handler drops.
	genCap, pktCap, gen, pkt = newPair()
	gen.Info("peer connected", "peer", "1.2.3.4:41412")
	if genCap.count() != 1 {
		t.Fatalf("untagged record: general handler got %d, want 1", genCap.count())
	}
	pkt.Info("peer connected", "peer", "1.2.3.4:41412")
	if pktCap.count() != 0 {
		t.Fatalf("untagged record: packet handler got %d, want 0", pktCap.count())
	}

	// Inline record attr (no With): packet handler passes via the Handle
	// fallback, general handler drops.
	genCap, pktCap, gen, pkt = newPair()
	pkt.Info("inline tagged", "p2p", true)
	if pktCap.count() != 1 {
		t.Fatalf("inline tagged record: packet handler got %d, want 1", pktCap.count())
	}
	gen.Info("inline tagged", "p2p", true)
	if genCap.count() != 0 {
		t.Fatalf("inline tagged record: general handler got %d, want 0", genCap.count())
	}

	// WithGroup preserves the marker flag in both orders.
	genCap, pktCap, gen, pkt = newPair()
	pkt.WithGroup("g").With("p2p", true).Info("group then tag")
	if pktCap.count() != 1 {
		t.Fatalf("group-then-tag: packet handler got %d, want 1", pktCap.count())
	}
	pkt.With("p2p", true).WithGroup("g").Info("tag then group")
	if pktCap.count() != 2 {
		t.Fatalf("tag-then-group: packet handler got %d, want 2", pktCap.count())
	}
	gen.With("p2p", true).WithGroup("g").Info("general group tagged")
	if genCap.count() != 0 {
		t.Fatalf("group tagged: general handler got %d, want 0", genCap.count())
	}
}
