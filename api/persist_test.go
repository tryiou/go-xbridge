package api

import (
	"testing"

	"xbridge-go/crypto"
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
	n2.sessMu.Lock()
	for _, p := range ps {
		n2.restoreSwap(p)
	}
	n2.sessMu.Unlock()

	// CancelOrder needs a conn to write the packet; capture it.
	cc := &captureXConn{}
	n2.conn = cc

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
