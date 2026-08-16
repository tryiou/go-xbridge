package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
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
		config:   &Config{DataDir: dir, PersistSecrets: true},
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

	n.persist()

	ps, err := loadSwaps(swapStatePath(dir))
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
	ps, err := loadSwaps(swapStatePath(dir))
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

	ps, err := loadSwaps(swapStatePath(dir))
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

// TestPersistSecretsOptOut proves the -persistsecrets=false gate: the
// per-trade M keypair, HTLC secret, and pre-signed refund are zeroed at write
// time, while the non-secret derived fields (pubkey, secretHash, deposit
// identity) survive. restoreSwap still re-adds the order and restores the live
// session via its existing signals (State > csIdle / OurDepositTxID), with the
// signing material left zero so no refund/cancel re-sign is possible.
func TestPersistSecretsOptOut(t *testing.T) {
	dir := t.TempDir()
	n := &Node{
		config:   &Config{DataDir: dir, PersistSecrets: false},
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
	copy(id[:], []byte("optout-secrets-order-id00000"))
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

	ps, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("persisted %d swaps, want 1", len(ps))
	}
	got := ps[0]
	if got.PrivKey != ([32]byte{}) {
		t.Errorf("PrivKey leaked to disk: %x", got.PrivKey)
	}
	if got.Secret != ([33]byte{}) {
		t.Errorf("Secret leaked to disk: %x", got.Secret)
	}
	if got.RefundHex != "" {
		t.Errorf("RefundHex leaked to disk: %q", got.RefundHex)
	}
	if got.PubKey != mPub {
		t.Errorf("PubKey = %x, want %x (must survive opt-out)", got.PubKey, mPub)
	}
	if got.OurDepositTxID != "aabbccdd" || got.OurLockTime != 1234 {
		t.Errorf("deposit identity lost: txid %q lockTime %d", got.OurDepositTxID, got.OurLockTime)
	}
	if got.State != csCreatedA {
		t.Errorf("State = %v, want csCreatedA", got.State)
	}

	// restoreSwap must re-add the order and restore the live session with the
	// signing material zeroed.
	n2 := &Node{
		config:   &Config{DataDir: dir, PersistSecrets: false},
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
	if rs.privKey != ([32]byte{}) {
		t.Errorf("restored session privKey = %x, want zero (opt-out)", rs.privKey)
	}
	if rs.refundHex != "" {
		t.Errorf("restored session refundHex = %q, want empty (opt-out)", rs.refundHex)
	}
	if rs.state != csCreatedA {
		t.Errorf("restored state = %v, want csCreatedA", rs.state)
	}
}

// TestCorruptSwapFileContinuesLikeCpp proves a corrupt swap-state file surfaces
// as an Error and restores nothing, mirroring C++ App::loadOrders: a failed
// orders.dat read logs "Failed to load existing orders database" at erro level
// and continues with an empty set — the node never refuses to start.
func TestCorruptSwapFileContinuesLikeCpp(t *testing.T) {
	dir := t.TempDir()

	// A valid envelope whose checksum does not match the payload.
	env := swapFile{Sum: strings.Repeat("0", 64), Swaps: nil}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(swapStatePath(dir), data, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadSwaps(swapStatePath(dir)); err == nil {
		t.Fatal("corrupt swap file: loadSwaps must return an error")
	}

	old := xlog.L()
	defer xlog.SetLogger(old)
	var buf bytes.Buffer
	xlog.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	n := newPersistNode(t, dir)
	n.restoreLocalSwaps(dir)

	if got := buf.String(); !strings.Contains(got, "level=ERROR") ||
		!strings.Contains(got, "could not load persisted swaps") {
		t.Errorf("corrupt file must log at Error severity, got:\n%s", got)
	}
	if len(n.store.List()) != 0 || len(n.store.History()) != 0 {
		t.Fatalf("corrupt file must restore nothing; live=%d history=%d", len(n.store.List()), len(n.store.History()))
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
