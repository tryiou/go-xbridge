package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/crypto"
	xlog "go-xbridge/log"
	"go-xbridge/wallet"
)

// newPersistNode builds a minimal Node wired for persistence tests: a temp
// DataDir, an empty store + session map, and a signer. It does NOT dial, and
// its conn is left nil (persist/saveSwaps never touch it).
func newPersistNode(t *testing.T, dir string) *Node {
	t.Helper()
	return &Node{
		config:   &Config{DataDir: dir},
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		store:    NewStore(),
		sessions: map[string]*SwapSession{},
	}
}

// TestPersistRoundTrip exercises saveSwaps/loadSwaps at the persistence layer:
// a maker session (incl. its per-trade M keypair) must survive a serialize →
// deserialize round-trip with all key fields intact.
func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)

	mPriv := make([]byte, 32)
	mPriv[31] = 1
	mPub, err := crypto.CompressedPubKey(mPriv)
	if err != nil {
		t.Fatal(err)
	}

	var id [32]byte
	copy(id[:], []byte("persist-round-trip-order-id00")) // 32 bytes
	hubPriv := make([]byte, 32)
	hubPriv[31] = 0x7c
	hubPub := mustPub(t, hubPriv)
	o := &Order{
		ID:           id,
		FromCurrency: "BTC",
		ToCurrency:   "LTC",
		FromAmount:   1e8,
		ToAmount:     2e8,
		Mine:         true,
		SNodePubkey:  hexPub(t, hubPriv),
		HubAddress:   coins.KeyID(hubPub[:]),
		// The validated counterparty deposit out-params.
		OBinTxVout:       3,
		OBinTxP2SHAmount: 2001004,
		OOverpayment:     1005,
	}
	n.store.Add(o)
	n.newMakerSession(o, MakeOrderParams{MakerAddress: btcAddr, TakerAddress: btcAddr}, arr32(mPriv), mPub)
	// Retry scheduling (incl. the not-ready wait stamp) must survive the
	// round-trip so a restart resumes the same window.
	s := n.sessions[hexEncode(id[:])]
	s.claimRetryAt = 123456789
	s.claimRetries = 2
	s.depositRetryAt = 987654321
	s.notReadySince = 555555555
	s.notReadySeen = true

	n.persist()

	ps, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("loaded %d swaps, want 1", len(ps))
	}
	got := ps[0]
	if got.ID != id {
		t.Errorf("ID = %x, want %x", got.ID, id)
	}
	if got.PrivKey != arr32(mPriv) {
		t.Errorf("PrivKey did not round-trip")
	}
	if got.PubKey != mPub {
		t.Errorf("PubKey did not round-trip")
	}
	if got.State != csMaker {
		t.Errorf("State = %v, want csMaker", got.State)
	}
	if got.SrcCur != "BTC" || got.DstCur != "LTC" || got.SrcAmt != 1e8 || got.DstAmt != 2e8 {
		t.Errorf("amount/currency fields did not round-trip: %+v", got)
	}
	// The hub anchor (SNodePubkey/HubAddress) must survive the round-trip.
	if got.SNodePubkey != o.SNodePubkey || got.HubAddress != o.HubAddress {
		t.Errorf("hub anchor did not round-trip: got %q/%x want %q/%x",
			got.SNodePubkey, got.HubAddress, o.SNodePubkey, o.HubAddress)
	}
	// The validated deposit out-params round-trip.
	if got.OBinTxVout != o.OBinTxVout || got.OBinTxP2SHAmount != o.OBinTxP2SHAmount || got.OOverpayment != o.OOverpayment {
		t.Errorf("deposit out-params did not round-trip: got %d/%d/%d want %d/%d/%d",
			got.OBinTxVout, got.OBinTxP2SHAmount, got.OOverpayment, o.OBinTxVout, o.OBinTxP2SHAmount, o.OOverpayment)
	}
	// Retry scheduling (incl. the not-ready wait stamp + verdict) round-trips.
	if got.ClaimRetryAt != 123456789 || got.ClaimRetries != 2 || got.DepositRetryAt != 987654321 || got.NotReadySince != 555555555 || !got.NotReadySeen {
		t.Errorf("retry schedule did not round-trip: got %d/%d/%d/%d/%v",
			got.ClaimRetryAt, got.ClaimRetries, got.DepositRetryAt, got.NotReadySince, got.NotReadySeen)
	}
	// restoreSwap must reconstruct the order with the same hub anchor.
	n2 := newPersistNode(t, dir)
	n2.restoreSwap(got)
	if ro := n2.store.Get(hexEncode(id[:])); ro == nil {
		t.Fatal("restoreSwap did not re-add the order")
	} else if ro.SNodePubkey != o.SNodePubkey || ro.HubAddress != o.HubAddress {
		t.Errorf("restored order hub anchor lost: got %q/%x", ro.SNodePubkey, ro.HubAddress)
	} else if ro.OBinTxVout != o.OBinTxVout || ro.OBinTxP2SHAmount != o.OBinTxP2SHAmount || ro.OOverpayment != o.OOverpayment {
		t.Errorf("restored order deposit out-params lost: got %d/%d/%d",
			ro.OBinTxVout, ro.OBinTxP2SHAmount, ro.OOverpayment)
	}
	// The restored session resumes the same wait verdict, not a re-learned one.
	if rs := n2.sessions[hexEncode(id[:])]; rs == nil {
		t.Fatal("restoreSwap did not re-create the session")
	} else if !rs.notReadySeen {
		t.Error("restored session lost notReadySeen verdict")
	} else if rs.notReadySince != 555555555 {
		t.Errorf("restored session notReadySince = %d, want 555555555", rs.notReadySince)
	}
}

// TestCancelAfterRestart proves the headline claim: after a restart the restored
// session re-signs dxCancelOrder with the SAME per-trade M pubkey it had before
// the restart — because crypto.BtcSigner.Sign derives the header pubkey from the
// (restored) privkey. Without persistence this would be impossible.
func TestCancelAfterRestart(t *testing.T) {
	dir := t.TempDir()

	// --- "before restart": create + persist a local maker swap ---
	mPriv := make([]byte, 32)
	mPriv[5] = 0xaa
	mPub, err := crypto.CompressedPubKey(mPriv)
	if err != nil {
		t.Fatal(err)
	}
	var id [32]byte
	copy(id[:], []byte("cancel-after-restart-order-id00"))

	n1 := newPersistNode(t, dir)
	o := &Order{
		ID:           id,
		FromCurrency: "BTC",
		ToCurrency:   "LTC",
		FromAmount:   1e8,
		ToAmount:     2e8,
		Mine:         true,
	}
	n1.store.Add(o)
	n1.newMakerSession(o, MakeOrderParams{MakerAddress: btcAddr, TakerAddress: btcAddr}, arr32(mPriv), mPub)
	n1.persist()

	// --- "after restart": fresh node, reload from disk, then cancel ---
	n2 := newPersistNode(t, dir)
	ps, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("loaded %d swaps, want 1", len(ps))
	}
	for _, p := range ps {
		n2.restoreSwap(p)
	}

	// CancelOrder needs a conn to write the packet; capture it. The from-currency
	// wallet connector must also be registered (C++ cancelXBridgeTransaction
	// gates the cancel on it, xbridgeapp.cpp:2489-2495).
	cc := &captureXConn{}
	n2.conn = cc
	n2.config.Connectors = map[string]wallet.Connector{
		"BTC": &stubConn{ticker: "BTC", addr: btcAddr},
		"LTC": &stubConn{ticker: "LTC", addr: btcAddr},
	}

	if _, rerr := n2.CancelOrder(CancelOrderParams{ID: hexEncode(id[:])}); rerr != nil {
		t.Fatalf("CancelOrder after restart: %v", rerr)
	}
	got := cc.snapshot()
	if len(got) != 1 {
		t.Fatalf("cancel broadcast %d packets, want 1", len(got))
	}
	if got[0].Pubkey != mPub {
		t.Errorf("restored cancel packet pubkey = %x, want %x (restored key mismatch)",
			got[0].Pubkey, mPub)
	}
	// The order status must reflect the cancel post-restart.
	if ro := n2.store.Get(hexEncode(id[:])); ro == nil || ro.Status != "canceled" {
		t.Errorf("restored order status = %v, want canceled", ro)
	}
}

// TestPersistOnlyLocal mirrors C++ saveOrders' isLocal() filter: only orders we
// created (Mine=true) are written; a session whose order is not ours is skipped.
func TestPersistOnlyLocal(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)

	mPriv := make([]byte, 32)
	mPriv[31] = 7
	mPub, err := crypto.CompressedPubKey(mPriv)
	if err != nil {
		t.Fatal(err)
	}
	tPriv := make([]byte, 32)
	tPriv[31] = 9
	tPub, err := crypto.CompressedPubKey(tPriv)
	if err != nil {
		t.Fatal(err)
	}

	// Local (Mine) maker order → must be persisted.
	var localID [32]byte
	copy(localID[:], []byte("local-maker-order-id00000000"))
	local := &Order{ID: localID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e8, ToAmount: 2e8, Mine: true}
	n.store.Add(local)
	n.newMakerSession(local, MakeOrderParams{MakerAddress: btcAddr, TakerAddress: btcAddr}, arr32(mPriv), mPub)

	// Remote (non-Mine) taker order → must NOT be persisted.
	var remoteID [32]byte
	copy(remoteID[:], []byte("remote-taker-order-id0000000"))
	remote := &Order{ID: remoteID, FromCurrency: "LTC", ToCurrency: "BTC", FromAmount: 2e8, ToAmount: 1e8, Mine: false}
	n.store.Add(remote)
	n.newTakerSession(remote, TakeOrderParams{FromAddress: btcAddr, ToAddress: btcAddr}, arr32(tPriv), tPub)

	n.persist()

	ps, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("persisted %d swaps, want exactly 1 (only local)", len(ps))
	}
	if ps[0].ID != localID {
		t.Errorf("persisted id = %x, want local id %x", ps[0].ID, localID)
	}
}

// TestPersistSecretsAlwaysOnDisk locks the C++ orders.dat parity: the per-trade
// M keypair, HTLC secret, and pre-signed refund are ALWAYS written — there is
// no opt-out (a restarted mid-flight swap must be able to auto-refund and
// re-sign cancels). restoreSwap re-adds the order and restores the live
// session with the signing material intact and functional.
func TestPersistSecretsAlwaysOnDisk(t *testing.T) {
	dir := t.TempDir()
	n := &Node{
		config:   &Config{DataDir: dir},
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		store:    NewStore(),
		sessions: map[string]*SwapSession{},
	}

	mPriv := make([]byte, 32)
	mPriv[31] = 0x41
	mPub, err := crypto.CompressedPubKey(mPriv)
	if err != nil {
		t.Fatal(err)
	}
	var id [32]byte
	copy(id[:], []byte("always-secrets-order-id0000"))
	o := &Order{
		ID:           id,
		FromCurrency: "BTC",
		ToCurrency:   "LTC",
		FromAmount:   1e8,
		ToAmount:     2e8,
		Mine:         true,
	}
	n.store.Add(o)
	n.newMakerSession(o, MakeOrderParams{MakerAddress: btcAddr, TakerAddress: btcAddr}, arr32(mPriv), mPub)

	// Drive the session past the deposit so every secret-bearing field is set.
	s := n.sessions[hexEncode(id[:])]
	s.state = csCreatedA
	s.ourDepositTxID = "aabbccdd"
	s.ourLockTime = 1234
	var sec [33]byte
	sec[0], sec[32] = 0x02, 0x77
	s.secret = sec
	s.secretHash = coins.KeyID(sec[:])
	s.refundHex = "01000000deadbeef"

	n.persist()

	ps, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("persisted %d swaps, want 1", len(ps))
	}
	got := ps[0]
	if got.PrivKey != arr32(mPriv) {
		t.Errorf("PrivKey not on disk: %x", got.PrivKey)
	}
	if got.Secret != sec {
		t.Errorf("Secret not on disk: %x", got.Secret)
	}
	if got.RefundHex != "01000000deadbeef" {
		t.Errorf("RefundHex not on disk: %q", got.RefundHex)
	}
	if got.PubKey != mPub {
		t.Errorf("PubKey = %x, want %x", got.PubKey, mPub)
	}
	if got.OurDepositTxID != "aabbccdd" || got.OurLockTime != 1234 {
		t.Errorf("deposit identity lost: txid %q lockTime %d", got.OurDepositTxID, got.OurLockTime)
	}
	if got.State != csCreatedA {
		t.Errorf("State = %v, want csCreatedA", got.State)
	}

	// restoreSwap must re-add the order and restore the live session with the
	// signing material intact — the session can still sign (same M pubkey).
	n2 := &Node{
		config:   &Config{DataDir: dir},
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		store:    NewStore(),
		sessions: map[string]*SwapSession{},
	}
	n2.restoreSwap(got)
	if n2.store.Get(hexEncode(id[:])) == nil {
		t.Fatal("order must be re-added on restore")
	}
	rs := n2.sessions[hexEncode(id[:])]
	if rs == nil {
		t.Fatal("live session must be restored")
	}
	if rs.privKey != arr32(mPriv) {
		t.Errorf("restored session privKey = %x, want persisted key", rs.privKey)
	}
	if rs.refundHex != "01000000deadbeef" {
		t.Errorf("restored session refundHex = %q, want persisted hex", rs.refundHex)
	}
	if rs.secret != sec {
		t.Errorf("restored session secret lost: %x", rs.secret)
	}
	if rs.state != csCreatedA {
		t.Errorf("restored state = %v, want csCreatedA", rs.state)
	}
	if rp, err := crypto.CompressedPubKey(rs.privKey[:]); err != nil || rp != mPub {
		t.Errorf("restored key cannot re-derive the M pubkey (cancel re-sign impossible): %v", err)
	}
}

// TestCorruptSwapFileSalvagesAndQuarantines proves a corrupt file no longer
// discards everything (the old C++ loadOrders behavior, deliberately
// exceeded): the file is quarantined byte-identical and each valid record is
// restored, with counts logged. Envelope: checksum mismatch, 2 good records
// (live session + historical), 1 type-broken, 1 zero-ID.
func TestCorruptSwapFileSalvagesAndQuarantines(t *testing.T) {
	dir := t.TempDir()

	var liveID [32]byte
	copy(liveID[:], []byte("salvage-live-session-record-00")) // 32 bytes
	var histID [32]byte
	copy(histID[:], []byte("salvage-historical-record-000")) // 32 bytes
	var mkey [32]byte
	mkey[31] = 9
	live := persistedSwap{
		ID: liveID, Type: OrderTypeMaker, FromCurrency: "BTC", ToCurrency: "LTC",
		Status: "open", IsMaker: true, SrcCur: "BTC", DstCur: "LTC",
		PrivKey: mkey, RefundHex: "01000000salvageme",
		OurDepositTxID: "depositsalvage", State: csCreatedA,
	}
	hist := persistedSwap{
		ID: histID, Type: OrderTypeMaker, FromCurrency: "BTC", ToCurrency: "LTC",
		Status: "finished", Historical: true, Updated: 42,
	}
	g1, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := json.Marshal(hist)
	if err != nil {
		t.Fatal(err)
	}
	// All-zero checksum guarantees the strict loader rejects the envelope,
	// routing restore through the salvage fallback.
	file := `{"sum":"` + strings.Repeat("0", 64) + `","swaps":[` +
		string(g1) + `,` + string(g2) + `,{"fromAmount":"boom"},{}]}`
	path := swapStatePath(dir)
	if err := os.WriteFile(path, []byte(file), 0600); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := loadSwaps(path); err == nil {
		t.Fatal("corrupt swap file: strict loadSwaps must return an error")
	}

	old := xlog.L()
	defer xlog.SetLogger(old)
	var buf bytes.Buffer
	xlog.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	n := newPersistNode(t, dir)
	n.restoreLocalSwaps(dir)

	// The live session record restores with its refund material intact.
	rs := n.sessions[hexEncode(liveID[:])]
	if rs == nil {
		t.Fatal("salvage must restore the live session")
	}
	if rs.refundHex != "01000000salvageme" || rs.privKey != mkey {
		t.Errorf("salvaged session lost refund material: refundHex=%q privKey=%x", rs.refundHex, rs.privKey)
	}
	// The historical record restores to history, never the live set.
	if len(n.store.History()) != 1 {
		t.Fatalf("salvage must restore the historical record; history=%d", len(n.store.History()))
	}
	if got := n.store.Get(hexEncode(histID[:])); got != nil {
		t.Error("historical record must not leak into the live set")
	}

	// The corrupt original is quarantined byte-identical at 0600, and the
	// live path is gone so the next persist starts clean.
	bads, err := filepath.Glob(path + ".bad.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(bads) != 1 {
		t.Fatalf("want exactly one quarantine file, got %v", bads)
	}
	qdata, err := os.ReadFile(bads[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(qdata) != file {
		t.Error("quarantine must preserve the corrupt file byte-identical")
	}
	if fi, err := os.Stat(bads[0]); err != nil || fi.Mode().Perm() != 0600 {
		t.Errorf("quarantine must keep mode 0600: %v %v", bads[0], err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("corrupt original must be renamed away from the live path")
	}

	// Two records dropped (one unparseable, one zero-ID), two salvaged —
	// all named at Error severity with the quarantine path.
	got := buf.String()
	for _, want := range []string{"level=ERROR", "quarantine", "salvaged=2", "dropped=2", bads[0]} {
		if !strings.Contains(got, want) {
			t.Errorf("salvage log must contain %q, got:\n%s", want, got)
		}
	}
}

// TestQuarantineFailureStillRestoresSalvaged proves a filesystem error during
// quarantine does not discard fund-recovery material: the salvaged records
// (already in memory) still restore, startup continues degraded-loud, and the
// corrupt original stays at the live path for a later retry.
func TestQuarantineFailureStillRestoresSalvaged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir does not block rename for root")
	}
	dir := t.TempDir()

	var liveID [32]byte
	copy(liveID[:], []byte("quarantine-fail-restore-00000")) // 32 bytes
	live := persistedSwap{
		ID: liveID, Type: OrderTypeMaker, FromCurrency: "BTC", ToCurrency: "LTC",
		Status: "open", RefundHex: "01000000nodepsloss",
		OurDepositTxID: "dep-nodepsloss", State: csCreatedA,
	}
	g1, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	path := swapStatePath(dir)
	file := `{"sum":"` + strings.Repeat("0", 64) + `","swaps":[` + string(g1) + `,{"fromAmount":"boom"}]}`
	if err := os.WriteFile(path, []byte(file), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}()

	old := xlog.L()
	defer xlog.SetLogger(old)
	var buf bytes.Buffer
	xlog.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	n := newPersistNode(t, dir)
	n.restoreLocalSwaps(dir)

	if rs := n.sessions[hexEncode(liveID[:])]; rs == nil || rs.refundHex != "01000000nodepsloss" {
		t.Fatal("salvaged session must restore even when quarantine fails")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("corrupt original must stay at the live path, stat: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "without evidence backup") {
		t.Errorf("must log degraded-loud quarantine failure, got:\n%s", got)
	}
}

// TestEmptyCorruptFileWarnsNotErrors proves a checksum-bad envelope with zero
// records quarantines as evidence but logs at Warn, not Error — no swap
// material is at stake, so it must not cry wolf.
func TestEmptyCorruptFileWarnsNotErrors(t *testing.T) {
	dir := t.TempDir()
	path := swapStatePath(dir)
	if err := os.WriteFile(path, []byte(`{"sum":"deadbeef","swaps":[]}`), 0600); err != nil {
		t.Fatal(err)
	}

	old := xlog.L()
	defer xlog.SetLogger(old)
	var buf bytes.Buffer
	xlog.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	newPersistNode(t, dir).restoreLocalSwaps(dir)

	bads, err := filepath.Glob(path + ".bad.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(bads) != 1 {
		t.Fatalf("empty corrupt file must still quarantine, got %v", bads)
	}
	got := buf.String()
	if !strings.Contains(got, "empty corrupt") || !strings.Contains(got, "level=WARN") {
		t.Errorf("want Warn-level empty-corrupt log, got:\n%s", got)
	}
	if strings.Contains(got, "level=ERROR") {
		t.Errorf("zero-record corruption must not log at Error, got:\n%s", got)
	}
}

// TestGarbageSwapFileQuarantinesAndStartsFresh proves an unlistable envelope
// is quarantined as evidence and the node starts fresh — never refuses to
// start, never silently discards bytes.
func TestGarbageSwapFileQuarantinesAndStartsFresh(t *testing.T) {
	dir := t.TempDir()
	path := swapStatePath(dir)
	garbage := "\x00\x01not-json{{{"
	if err := os.WriteFile(path, []byte(garbage), 0600); err != nil {
		t.Fatal(err)
	}

	old := xlog.L()
	defer xlog.SetLogger(old)
	var buf bytes.Buffer
	xlog.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	n := newPersistNode(t, dir)
	n.restoreLocalSwaps(dir)

	if len(n.store.List()) != 0 || len(n.store.History()) != 0 {
		t.Fatalf("garbage file must restore nothing; live=%d history=%d", len(n.store.List()), len(n.store.History()))
	}
	bads, err := filepath.Glob(path + ".bad.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(bads) != 1 {
		t.Fatalf("garbage file must be quarantined, got %v", bads)
	}
	qdata, err := os.ReadFile(bads[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(qdata) != garbage {
		t.Error("quarantine must preserve garbage bytes identical")
	}
	if got := buf.String(); !strings.Contains(got, "level=ERROR") ||
		!strings.Contains(got, "nothing salvageable") {
		t.Errorf("garbage file must log at Error severity, got:\n%s", got)
	}
}

// TestValidSwapFileNeverQuarantines pins the strict-first fast path: a valid
// file restores exactly like before and leaves no quarantine evidence behind.
func TestValidSwapFileNeverQuarantines(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)

	var id [32]byte
	copy(id[:], []byte("no-quarantine-when-valid-0000")) // 32 bytes
	n.store.Add(&Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", Mine: true, Status: "open"})
	n.persist()

	n2 := newPersistNode(t, dir)
	n2.restoreLocalSwaps(dir)

	if got := n2.store.Get(hexEncode(id[:])); got == nil {
		t.Fatal("valid file must restore the order")
	}
	bads, err := filepath.Glob(swapStatePath(dir) + ".bad.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(bads) != 0 {
		t.Errorf("valid file must leave no quarantine behind, got %v", bads)
	}
}

// TestQuarantineNameUniqueAcrossRapidRestarts proves back-to-back corrupt
// restarts each preserve their own evidence instead of colliding on one name.
func TestQuarantineNameUniqueAcrossRapidRestarts(t *testing.T) {
	dir := t.TempDir()
	path := swapStatePath(dir)
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(path, []byte("{corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
		newPersistNode(t, dir).restoreLocalSwaps(dir)
	}
	bads, err := filepath.Glob(path + ".bad.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(bads) != 2 || bads[0] == bads[1] {
		t.Errorf("want two distinct quarantine files, got %v", bads)
	}
}

// TestLenientLoaderUnitMatrix pins loadSwapsLenient accounting without I/O.
func TestLenientLoaderUnitMatrix(t *testing.T) {
	var goodID [32]byte
	goodID[0] = 1
	goodRec, err := json.Marshal(persistedSwap{ID: goodID, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	zeroRec, err := json.Marshal(persistedSwap{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		data       string
		wantGood   int
		wantDrop   int
		wantUsable bool
	}{
		{"empty array", `{"sum":"x","swaps":[]}`, 0, 0, true},
		{"garbage", `{{{`, 0, 0, false},
		{"missing array", `{"sum":"x"}`, 0, 0, false},
		{"mixed", `{"sum":"x","swaps":[` + string(goodRec) + `,{"fromAmount":"boom"},` + string(zeroRec) + `]}`, 1, 2, true},
		{"all bad", `{"sum":"x","swaps":[{"fromAmount":"boom"}]}`, 0, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			good, dropped, err := loadSwapsLenient([]byte(c.data))
			if c.wantUsable && err != nil {
				t.Fatalf("usable envelope must not error: %v", err)
			}
			if !c.wantUsable && err == nil {
				t.Fatal("unusable envelope must error")
			}
			if len(good) != c.wantGood || dropped != c.wantDrop {
				t.Errorf("got good=%d dropped=%d, want %d/%d", len(good), dropped, c.wantGood, c.wantDrop)
			}
		})
	}
}

// TestPersistWriteFailurePropagated proves a durable swap-state write that
// ultimately fails is NOT silently dropped: persistFailures is incremented and
// surfaced, so an operator can see that on-disk state may lag in-memory state
// (a restart could otherwise be unable to refund/claim an in-flight swap).
func TestPersistWriteFailurePropagated(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)

	mPriv := make([]byte, 32)
	mPriv[31] = 3
	mPub, err := crypto.CompressedPubKey(mPriv)
	if err != nil {
		t.Fatal(err)
	}
	var id [32]byte
	copy(id[:], []byte("persist-fail-prop-order-id0000"))
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e8, ToAmount: 2e8, Mine: true}
	n.store.Add(o)
	n.newMakerSession(o, MakeOrderParams{MakerAddress: btcAddr, TakerAddress: btcAddr}, arr32(mPriv), mPub)

	orig := writeSwaps
	writeSwaps = func(path string, data []byte) error { return errors.New("disk full") }
	t.Cleanup(func() { writeSwaps = orig })

	n.persist() // inline path (node not started)

	if got := n.persistFailures.Load(); got != 1 {
		t.Fatalf("persistFailures = %d, want 1 (failure must be counted, not dropped)", got)
	}
	if _, err := os.Stat(swapStatePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("swap file should not exist after a failed write; stat err = %v", err)
	}
}

// TestPersistWriteRetrySucceeds proves a transient disk failure does not lose the
// durable copy: writeLatestPersist retries with bounded backoff and the swap
// file is written once a write succeeds, with no persistFailure recorded.
func TestPersistWriteRetrySucceeds(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)

	var attempts int
	orig := writeSwaps
	writeSwaps = func(path string, data []byte) error {
		attempts++
		if attempts < persistWriteMaxAttempts {
			return errors.New("transient EIO")
		}
		return orig(path, data)
	}
	t.Cleanup(func() { writeSwaps = orig })

	n.persistLatest = &persistJob{path: swapStatePath(dir), swaps: []persistedSwap{}}
	n.writeLatestPersist()

	if attempts != persistWriteMaxAttempts {
		t.Fatalf("write attempts = %d, want %d (retried to the last attempt)", attempts, persistWriteMaxAttempts)
	}
	if got := n.persistFailures.Load(); got != 0 {
		t.Fatalf("persistFailures = %d, want 0 (transient failure must not be counted)", got)
	}
	if _, err := os.Stat(swapStatePath(dir)); err != nil {
		t.Fatalf("swap file must exist after retry succeeds; stat err = %v", err)
	}
}

// TestFinishedSwapSurvivesRestart pins the persistence contract for terminal
// trades: a finished local order that moved to Store.history (the real finish
// path, swap.go applyFinished → MoveToHistory, mirroring C++
// moveTransactionToHistory, xbridgesession.cpp:3385) is written to the swap
// file as a historical record, restored back into history by a fresh node
// (restoreSwap's terminal branch routes it via AddToHistory, never the live
// book), and stays resolvable — dxGetOrder via the HistoryOrder fallback
// (C++ App::transaction consults m_historicTransactions,
// xbridgeapp.cpp:1273-1292) and dxGetMyOrders via the live+history merge
// (rpcxbridge.cpp:2110-2121) — across REPEATED restarts with no record loss,
// no duplication, and no field drift. Regression guard for a runtime
// finished-history wipe once observed under a pre-refactor build: the exact
// wipe mechanism could not be reproduced (its log window rotated away), so
// this double-restart round-trip plus the snapshot count-change log in
// noteSnapshotCount are the standing detectors.
func TestFinishedSwapSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	var id [32]byte
	copy(id[:], []byte("finished-swap-restart-id00000000")) // 32 bytes
	idHex := hexEncode(id[:])
	const now = uint64(1726310000000000)

	// Mirrors a live maker-side trade record: maker-order frame with the
	// maker/taker addresses from the original book side (Role 'A').
	mk := func() *Order {
		return &Order{
			ID:           id,
			FromCurrency: "BLOCK",
			ToCurrency:   "PIVX",
			FromAmount:   10000, // 0.01 COIN at the 1e6 XBridge scale
			ToAmount:     10000,
			Created:      now,
			Updated:      now,
			Mine:         true,
			Role:         'A',
			MakerAddress: "BWS8Vt58uk4gzJmZ17H6cBrcXUuJ1fccZZ",
			TakerAddress: "DFosJqyKADvh9qsePrYQtqefS74TtJe2gF",
		}
	}

	n1 := newPersistNode(t, dir)
	n1.store.Add(mk())
	n1.store.MoveToHistory(idHex, "finished", 0, now)

	n1.persist()

	ps, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("persisted %d swap records, want 1", len(ps))
	}
	if !ps[0].Historical || ps[0].Status != "finished" {
		t.Fatalf("persisted record: historical=%v status=%q, want historical=true status=finished", ps[0].Historical, ps[0].Status)
	}

	// Restart #1: the fresh node must restore the terminal record into
	// history — never the live book — with identity, role and addresses intact.
	n2 := newPersistNode(t, dir)
	n2.restoreLocalSwaps(dir)
	if o := n2.store.Get(idHex); o != nil {
		t.Fatal("terminal record was re-registered as a live order")
	}
	ho := n2.store.HistoryOrder(idHex)
	if ho == nil {
		t.Fatal("finished order missing from history after restart")
	}
	if ho.Status != "finished" || !ho.Mine || ho.Role != 'A' {
		t.Fatalf("restored history order: status=%q mine=%v role=%q", ho.Status, ho.Mine, ho.Role)
	}
	if ho.MakerAddress != "BWS8Vt58uk4gzJmZ17H6cBrcXUuJ1fccZZ" || ho.TakerAddress != "DFosJqyKADvh9qsePrYQtqefS74TtJe2gF" {
		t.Fatalf("restored history order lost its addresses: maker=%q taker=%q", ho.MakerAddress, ho.TakerAddress)
	}
	if got := n2.store.History(); len(got) != 1 {
		t.Fatalf("history holds %d entries after restart, want 1", len(got))
	}

	// Restart #2 — the real-world failure mode was a LATER run dropping the
	// history, so re-persist from the restored state and restore once more.
	n2.persist()
	ps2, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps #2: %v", err)
	}
	if len(ps2) != 1 {
		t.Fatalf("second persist wrote %d swap records, want 1 (no duplication, no loss)", len(ps2))
	}
	if ps2[0].ID != id || !ps2[0].Historical || ps2[0].Status != "finished" {
		t.Fatalf("second persist record drifted: id-match=%v historical=%v status=%q", ps2[0].ID == id, ps2[0].Historical, ps2[0].Status)
	}

	n3 := newPersistNode(t, dir)
	n3.restoreLocalSwaps(dir)
	if ho := n3.store.HistoryOrder(idHex); ho == nil || ho.Status != "finished" || !ho.Mine {
		t.Fatalf("finished order did not survive the second restart: %+v", ho)
	}
}

// TestNoteSnapshotCountPinsTheGate pins noteSnapshotCount's counter logic:
// the first observation establishes the baseline without logging a spurious
// 0→N transition (swapCountSeen gate), repeated equal counts are steady, and
// the tracked value always reflects the latest snapshot — so a decrease can
// only ever WARN against a real previously-seen count, never against a zero
// value. The log emissions themselves are exercised by every persist test.
func TestNoteSnapshotCountPinsTheGate(t *testing.T) {
	n := newPersistNode(t, t.TempDir())
	if n.swapCountSeen.Load() {
		t.Fatal("swapCountSeen must start false (first-shot gate armed)")
	}
	n.noteSnapshotCount(3)
	if !n.swapCountSeen.Load() || n.lastSwapCount.Load() != 3 {
		t.Fatalf("first observation: seen=%v count=%d, want seen=true count=3",
			n.swapCountSeen.Load(), n.lastSwapCount.Load())
	}
	n.noteSnapshotCount(3)
	if n.lastSwapCount.Load() != 3 {
		t.Fatalf("steady count drifted: %d", n.lastSwapCount.Load())
	}
	n.noteSnapshotCount(1)
	if n.lastSwapCount.Load() != 1 {
		t.Fatalf("decrease not tracked: %d", n.lastSwapCount.Load())
	}
	n.noteSnapshotCount(5)
	if n.lastSwapCount.Load() != 5 {
		t.Fatalf("increase not tracked: %d", n.lastSwapCount.Load())
	}
}

// TestClaimIntentPersistRoundTrip pins the crash-safe claim intent: the built
// claim (hex/id/chain) plus the validated deposit out-params survive a
// save/load cycle, so a post-build crash resumes the broadcast.
func TestClaimIntentPersistRoundTrip(t *testing.T) {
	alignInitCoins(t)
	n := newTestNode(t, alignConfs(), nil)

	var id [32]byte
	oid := hash20("align-claim-persist")
	copy(id[:], oid[:])
	var secret [33]byte
	for i := range secret {
		secret[i] = byte(i + 7)
	}
	s := &SwapSession{
		n: n, isMaker: true, id: id,
		srcCur: "BTC", dstCur: "LTC", srcAmt: 2.5e6, dstAmt: 2e6,
		state:  csConfirmedA,
		secret: secret, secretHash: coins.KeyID(secret[:]),
		ourLockTime: 1100, ourDepositTxID: strings.Repeat("cd", 32),
		refundHex: "0300", refundDone: false,
		theirDepositTxID: strings.Repeat("ab", 32), theirLockTime: 1050,
		theirDepositVout: 2, theirP2SHNative: 200045200, theirOverpayment: 11,
		claimHex: "0400", claimTxID: strings.Repeat("ef", 32), claimCur: "LTC",
	}
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6,
		ToAmount: 2e6, Status: "created", Mine: true,
		// Phase-1 ordering: the order's OBinTx* copy is only updated at
		// broadcast, so it is still zero here while the session already
		// holds the validated out-params. Restore must use the session
		// copy, not the stale order copy.
		OOverpayment: 0}
	n.store.Add(o)

	n2 := newTestNode(t, alignConfs(), nil)
	n2.restoreSwap(persistFromSession(s, o))
	rs := n2.sessions[hexEncode(id[:])]
	if rs == nil {
		t.Fatal("restored session missing")
	}
	if rs.claimHex != "0400" || rs.claimTxID != strings.Repeat("ef", 32) || rs.claimCur != "LTC" {
		t.Fatalf("claim intent lost: %+v", rs)
	}
	if rs.theirDepositVout != 2 || rs.theirP2SHNative != 200045200 || rs.theirOverpayment != 11 {
		t.Fatalf("validated out-params lost: vout=%d p2sh=%d over=%d",
			rs.theirDepositVout, rs.theirP2SHNative, rs.theirOverpayment)
	}
	if rs.secret != secret {
		t.Fatal("secret lost across restore")
	}
}
