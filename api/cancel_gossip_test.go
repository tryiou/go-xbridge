package api

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xlog "go-xbridge/log"
	"go-xbridge/proto"
)

// installSplitLogs wires the production file split (general + packet files)
// into temp dirs for routing assertions. Restores the logger and level on
// cleanup. Mirrors the log-package split test through the real Set calls.
func installSplitLogs(t *testing.T) (genPath, pktPath string) {
	t.Helper()
	dir := t.TempDir()
	genPath = filepath.Join(dir, "xbridged.log")
	pktPath = filepath.Join(dir, "log-p2p", "xbridgep2p.log")

	genRw, err := xlog.SetFileLogger(genPath, 1<<20, 2)
	if err != nil {
		t.Fatalf("SetFileLogger: %v", err)
	}
	pktRw, err := xlog.SetP2PLogFile(pktPath, 1<<20, 2)
	if err != nil {
		t.Fatalf("SetP2PLogFile: %v", err)
	}
	oldLevel := xlog.Level()
	xlog.SetLevel(slog.LevelDebug)
	t.Cleanup(func() {
		xlog.SetLevel(oldLevel)
		_ = genRw.Close()
		_ = pktRw.Close()
		xlog.SetLogger(nil)
	})
	return genPath, pktPath
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(data)
}

// TestCancelGossipRoutesByOrder pins the cancel-spam routing through the
// production helper and the real file split: third-party cancel chatter
// (unknown or foreign orders) lands ONLY in the packet file, while our own
// orders' cancels land ONLY in the general file — including forged ones,
// which are attack visibility, not gossip. Covers the Info/Debug/Warn levels
// the call sites use. handleRemoteCancel wiring itself is exercised by the
// existing cancel-path suite; the log-package split is pinned there.
func TestCancelGossipRoutesByOrder(t *testing.T) {
	genPath, pktPath := installSplitLogs(t)

	gossipLogger(&Order{Mine: true}).Info("own cancel stays", "order", "mine1")
	gossipLogger(&Order{Mine: false}).Info("foreign cancel moves", "order", "foreign1")
	gossipLogger(nil).Debug("unknown cancel moves", "order", "unknown1")
	gossipLogger(&Order{Mine: false}).Warn("foreign warn moves", "order", "foreign2")

	gen := readLogFile(t, genPath)
	pkt := readLogFile(t, pktPath)
	if !strings.Contains(gen, "own cancel stays") {
		t.Errorf("general file missing own-order line: %q", gen)
	}
	for _, wantGone := range []string{"foreign cancel moves", "unknown cancel moves", "foreign warn moves"} {
		if strings.Contains(gen, wantGone) {
			t.Errorf("general file must not contain %q: %q", wantGone, gen)
		}
		if !strings.Contains(pkt, wantGone) {
			t.Errorf("packet file missing %q: %q", wantGone, pkt)
		}
	}
	if strings.Contains(pkt, "own cancel stays") {
		t.Errorf("packet file must not contain own-order line: %q", pkt)
	}
}

// TestCancelGossipRollupsGoToP2P pins the rollup destination: the three
// suppression summaries are gossip accounting for foreign traffic, so they
// belong in the packet file even though they aggregate. Called directly —
// emission timing stays owned by the dedup sweep.
func TestCancelGossipRollupsGoToP2P(t *testing.T) {
	genPath, pktPath := installSplitLogs(t)

	summarizeCancelLookup("rollup-test-lookup", 3, 30*time.Second)
	summarizeCancelBadSig("rollup-test-badsig", 5, 32*time.Second)
	summarizeCancelNoConnector("ROLLUPTEST", 7, 30*time.Second)
	summarizeCancelAlreadyCanceled("rollup-test-already", 9, 34*time.Second)

	gen := readLogFile(t, genPath)
	pkt := readLogFile(t, pktPath)
	for _, want := range []string{
		"cancel: order lookup failures suppressed",
		"cancel: bad packet signature suppressed",
		"cancel: no connector suppressed",
		"cancel: already-canceled duplicates suppressed",
	} {
		if strings.Contains(gen, want) {
			t.Errorf("general file must not contain rollup %q: %q", want, gen)
		}
		if !strings.Contains(pkt, want) {
			t.Errorf("packet file missing rollup %q: %q", want, pkt)
		}
	}
}

// TestCancelGossipMineBypassesDedup: forged cancels against an OWNED order
// are attack visibility, not gossip — every occurrence stays in the main log,
// never collapsed into the packet-log summaries. Drives handleRemoteCancel
// twice with a bad signature: pre-change the second was swallowed by dedup.
func TestCancelGossipMineBypassesDedup(t *testing.T) {
	genPath, pktPath := installSplitLogs(t)

	forgedPriv := make([]byte, 32)
	forgedPriv[0] = 0x9d
	otherPriv := make([]byte, 32)
	otherPriv[0] = 0x9e
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "open", Mine: true, SNodePubkey: hexPub(t, otherPriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	// Forged key matches nothing on the order (no MakerKey/SNodePubkey/
	// OtherPubkey set): every cancel lands in the bad-signature branch.
	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), forgedPriv)
	n.handleRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})
	n.handleRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})

	gen := readLogFile(t, genPath)
	pktData := readLogFile(t, pktPath)
	if got := strings.Count(gen, "cancel: bad packet signature for cancelation request on order, not canceling"); got != 2 {
		t.Errorf("main log bad-sig lines = %d, want 2 (no suppression for own orders)", got)
	}
	if strings.Contains(pktData, "bad packet signature") {
		t.Errorf("packet file must not contain own-order bad-sig lines: %q", pktData)
	}
}

// TestCancelAlreadyCanceledDeduped: duplicate Cancel packets for an
// already-canceled order log once (first sighting); repeats collapse into
// the 60 s summary. Live: a hub redelivery storm emitted 591 identical
// "cancel: already canceled" main-log lines in ~150 ms for one settled
// order. Own orders confirm landing in main; foreign in the packet file.
func TestCancelAlreadyCanceledDeduped(t *testing.T) {
	genPath, pktPath := installSplitLogs(t)

	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x9e
	// Test-local IDs (not the shared mustID): cancelAlreadyCanceledDedup is
	// a package-global 60 s bucket keyed on order id, so a constant id would
	// collide with any other test hitting this branch in the window.
	var ownID, foreignID [32]byte
	ownID[0] = 0xC3
	foreignID[0] = 0xC4
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "canceled", Mine: true, SNodePubkey: hexPub(t, snodePriv)}
	o.ID = ownID
	f := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "canceled", Mine: false, SNodePubkey: hexPub(t, snodePriv)}
	f.ID = foreignID

	n := newCancelTestNode(nil)
	n.store.Add(o)
	n.store.Add(f)
	// A pending retry entry must survive repeats on this branch: the branch
	// changes nothing (no Update, no drop), so the entry stays armed.
	n.takeRetriesMu.Lock()
	n.takeRetries = map[string]*takeRetryState{hexEncode(ownID[:]): {}}
	n.takeRetriesMu.Unlock()

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), snodePriv)
	n.handleRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})
	n.handleRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})
	n.handleRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})
	fpkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: f.ID, Reason: 1}).Marshal(), snodePriv)
	n.handleRemoteCancel(fpkt, &proto.CancelBody{ID: f.ID, Reason: 1})
	n.handleRemoteCancel(fpkt, &proto.CancelBody{ID: f.ID, Reason: 1})

	gen := readLogFile(t, genPath)
	pktData := readLogFile(t, pktPath)
	if got := strings.Count(gen, "cancel: already canceled"); got != 1 {
		t.Errorf("main log already-canceled lines = %d, want 1 (own first sighting only)", got)
	}
	if got := strings.Count(pktData, "cancel: already canceled"); got != 1 {
		t.Errorf("packet file already-canceled lines = %d, want 1 (foreign first sighting only)", got)
	}
	// Summary path: flushing emits one aggregate per order key, always to
	// the packet file (the p2p marker) — including the own-order storm,
	// whose landing was already confirmed once in main.
	cancelAlreadyCanceledDedup.Flush()
	gen = readLogFile(t, genPath)
	pktData = readLogFile(t, pktPath)
	if got := strings.Count(gen, "already-canceled duplicates suppressed"); got != 0 {
		t.Errorf("main log summary lines = %d, want 0 (summaries aggregate in the packet file)", got)
	}
	if got := strings.Count(pktData, "already-canceled duplicates suppressed"); got != 2 {
		t.Errorf("packet file summary lines = %d, want 2 (one per order key): %q", got, pktData)
	}
	// Side-effect freedom: repeats on this branch change nothing — both
	// orders stay live-canceled in the store and the seeded retry entry
	// stays armed (no Update, no drop on this path).
	for _, id := range [][32]byte{ownID, foreignID} {
		got := n.store.Get(hexEncode(id[:]))
		if got == nil || got.Status != "canceled" {
			t.Fatalf("order %x after repeats = %+v, want live canceled", id[:4], got)
		}
	}
	n.takeRetriesMu.Lock()
	_, ok := n.takeRetries[hexEncode(ownID[:])]
	n.takeRetriesMu.Unlock()
	if !ok {
		t.Error("seeded retry entry missing after repeats: this branch must not disarm retries")
	}
}

// TestCancelGossipMineNoConnectorBypassesDedup: same Mine-bypass rule for the
// no-connector branch (e.g. our order's coin connector dropped mid-swap).
func TestCancelGossipMineNoConnectorBypassesDedup(t *testing.T) {
	genPath, pktPath := installSplitLogs(t)

	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x9e
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTX", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "open", Mine: true, SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), snodePriv)
	n.handleRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})
	n.handleRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})

	gen := readLogFile(t, genPath)
	pktData := readLogFile(t, pktPath)
	if got := strings.Count(gen, "cancel: no connector for currency, not canceling"); got != 2 {
		t.Errorf("main log no-connector lines = %d, want 2 (no suppression for own orders)", got)
	}
	if strings.Contains(pktData, "no connector for currency") {
		t.Errorf("packet file must not contain own-order no-connector lines: %q", pktData)
	}
}
