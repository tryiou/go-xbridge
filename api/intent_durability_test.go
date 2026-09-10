package api

import (
	"errors"
	"strings"
	"testing"

	"go-xbridge/crypto"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// These tests lock intent-before-broadcast: no chain broadcast may precede
// its durable refund intent, and a crash at any seam must leave the swap
// recoverable (file intent + transcript + restart reconciliation).

// TestDepositIntentDurableBeforeBroadcast blocks the deposit broadcast and
// proves the intent is durable first: refundHex + txid + lockTime on disk,
// zero chain broadcasts, state unadvanced, DepositSent false — then clears
// the blockage and proves hub redelivery (rebuild) completes the deposit.
func TestDepositIntentDurableBeforeBroadcast(t *testing.T) {
	n, s, conn := setupSwapPair(t)
	dir := t.TempDir()
	n.config.DataDir = dir
	txlogDir := txlogTestDir(t)
	idHex := hexEncode(s.id[:])
	// Production maker orders are Mine (set by MakeOrder); the pair fixture
	// bypasses it, so mark it here — only Mine orders persist (C++ isLocal).
	n.store.Update(idHex, func(o *Order) { o.Mine = true })

	conn.sendErr = errors.New("simulated broadcast failure")
	createA := &proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}
	_, body, err := s.OnCreateA(createA)
	if err != nil {
		t.Fatalf("OnCreateA build: %v", err)
	}
	if body != nil {
		t.Fatal("broadcast failure must yield no CreatedA body")
	}

	// Nothing reached the chain.
	if len(conn.broadcasts) != 0 {
		t.Fatalf("broadcasts = %d, want 0 (intent must precede broadcast)", len(conn.broadcasts))
	}
	// The intent is durable: refundHex + txid + lockTime on disk.
	ps, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("persisted %d swaps, want 1 intent", len(ps))
	}
	got := ps[0]
	if got.RefundHex == "" || got.OurDepositTxID == "" || got.OurLockTime == 0 {
		t.Fatalf("intent incomplete on disk: refund %v txid %q lockTime %d",
			got.RefundHex != "", got.OurDepositTxID, got.OurLockTime)
	}
	if got.RefundHex != s.refundHex || got.OurDepositTxID != s.ourDepositTxID {
		t.Fatal("disk intent != adopted session intent")
	}
	// State unadvanced, deposit unconfirmed.
	if s.state == csCreatedA {
		t.Fatal("state advanced without a broadcast")
	}
	o := n.store.Get(idHex)
	if o == nil {
		t.Fatal("order missing")
	}
	if o.DepositSent {
		t.Fatal("DepositSent set without a broadcast")
	}
	if o.BinTxId != s.ourDepositTxID || o.RefundTx != s.refundHex {
		t.Fatal("order does not carry the intent")
	}
	// Transcript names the built (unbroadcast) deposit + refund, and no
	// broadcast line may exist yet.
	txlog := txlogToday(t, txlogDir)
	disp := orderIDString(s.id)
	if !strings.Contains(txlog, "deposit built (NOT YET BROADCAST) for order "+disp) {
		t.Errorf("transcript missing built entry, got:\n%s", txlog)
	}
	if !strings.Contains(txlog, s.refundHex) {
		t.Errorf("transcript missing pre-signed refund hex")
	}
	if strings.Contains(txlog, "deposit transaction for order "+disp) {
		t.Errorf("transcript claims an unconfirmed broadcast, got:\n%s", txlog)
	}

	// Hub redelivery rebuilds and completes: unblock and re-drive.
	conn.sendErr = nil
	_, body2, err := s.OnCreateA(createA)
	if err != nil {
		t.Fatalf("OnCreateA redelivery: %v", err)
	}
	if body2 == nil {
		t.Fatal("redelivery must produce the CreatedA body")
	}
	if len(conn.broadcasts) != 1 {
		t.Fatalf("broadcasts = %d, want 1 after redelivery", len(conn.broadcasts))
	}
	if s.state != csCreatedA {
		t.Fatal("state did not advance after confirmed broadcast")
	}
	if o := n.store.Get(idHex); o == nil || !o.DepositSent {
		t.Fatal("DepositSent not recorded after confirmed broadcast")
	}
}

// TestCrashBetweenIntentAndBroadcastRecoversRefund is the W1 proof: intent
// persisted, process "killed" before broadcast, deposit visible on chain
// anyway (propagation won the race). A fresh node over the same datadir must
// reconcile the ambiguity (mark DepositSent) and the sweep must broadcast the
// exact pre-signed refund once past lockTime.
func TestCrashBetweenIntentAndBroadcastRecoversRefund(t *testing.T) {
	n, s, conn := setupSwapPair(t)
	dir := t.TempDir()
	n.config.DataDir = dir
	txlogDir := txlogTestDir(t)
	n.store.Update(hexEncode(s.id[:]), func(o *Order) { o.Mine = true })

	conn.sendErr = errors.New("kill -9 before broadcast")
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
		t.Fatalf("OnCreateA build: %v", err)
	}
	txid := s.ourDepositTxID
	refundHex := s.refundHex
	lockTime := s.ourLockTime
	if txid == "" || refundHex == "" || lockTime == 0 {
		t.Fatal("no intent adopted")
	}
	// Recover the built deposit hex from the transcript (the only copy left
	// after the "crash" — the process memory is gone).
	depositHex := ""
	lines := strings.Split(txlogToday(t, txlogDir), "\n")
	for i, l := range lines {
		if strings.Contains(l, "NOT YET BROADCAST") && strings.Contains(l, orderIDString(s.id)) && i+1 < len(lines) {
			depositHex = strings.TrimSpace(lines[i+1])
		}
	}
	if depositHex == "" {
		t.Fatal("transcript has no built deposit hex to recover")
	}

	// The broadcast actually reached the chain before the kill (visible to a
	// fresh wallet as a mempool/chain tx). The restarted wallet is healthy —
	// clear the kill switch so the sweep's refund broadcast can proceed.
	conn.rawTx[txid] = depositHex
	conn.sendErr = nil

	// "Crash": drop all memory, restart over the same datadir.
	n2 := &Node{
		config:   &Config{DataDir: dir, Connectors: map[string]wallet.Connector{"BTC": conn}},
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		store:    NewStore(),
		sessions: map[string]*SwapSession{},
	}
	n2.restoreLocalSwaps(dir)

	// Reconciliation must have verified the deposit on chain and marked it.
	o := n2.store.Get(hexEncode(s.id[:]))
	if o == nil {
		t.Fatal("order not restored")
	}
	if !o.DepositSent {
		t.Fatal("reconcile did not mark the on-chain deposit sent")
	}
	s2 := n2.sessions[hexEncode(s.id[:])]
	if s2 == nil {
		t.Fatal("session not restored live for the refund sweep")
	}

	// Past lockTime the sweep broadcasts the exact pre-signed refund once.
	conn.blockHeight = int64(s2.ourLockTime) + 1
	before := len(conn.broadcasts)
	n2.scanRefunds()
	if len(conn.broadcasts) != before+1 {
		t.Fatalf("sweep broadcasts = %d, want one refund", len(conn.broadcasts)-before)
	}
	if hex, ok := conn.rawTx[conn.broadcasts[len(conn.broadcasts)-1]]; !ok || hex != refundHex {
		t.Fatal("sweep did not broadcast the persisted pre-signed refund")
	}
	if !s2.refundDone {
		t.Fatal("refundDone not set after reconciled broadcast")
	}
}

// TestCrashWithUnbroadcastDepositLeavesNoRefund proves the other branch: the
// kill landed before any broadcast and the deposit is nowhere on chain.
// Restart must NOT mark it sent and the sweep must NOT broadcast — the paths
// are hub redelivery (rebuild) or cancel.
func TestCrashWithUnbroadcastDepositLeavesNoRefund(t *testing.T) {
	n, s, conn := setupSwapPair(t)
	dir := t.TempDir()
	n.config.DataDir = dir
	n.store.Update(hexEncode(s.id[:]), func(o *Order) { o.Mine = true })

	conn.sendErr = errors.New("kill -9 before broadcast")
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
		t.Fatalf("OnCreateA build: %v", err)
	}

	n2 := &Node{
		config:   &Config{DataDir: dir, Connectors: map[string]wallet.Connector{"BTC": conn}},
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		store:    NewStore(),
		sessions: map[string]*SwapSession{},
	}
	n2.restoreLocalSwaps(dir)

	o := n2.store.Get(hexEncode(s.id[:]))
	if o == nil {
		t.Fatal("order not restored")
	}
	if o.DepositSent {
		t.Fatal("reconcile marked an off-chain deposit sent")
	}
	conn.blockHeight = 1 << 30 // far past any lockTime
	n2.scanRefunds()
	if len(conn.broadcasts) != 0 {
		t.Fatalf("sweep broadcast %d refund(s) for an unbroadcast deposit", len(conn.broadcasts))
	}
}

// TestClaimIntentDurableBeforeBroadcast blocks the claim broadcast and proves
// the claim intent (validated out-params) is durable first with zero chain
// broadcasts — then clears the blockage and proves redelivery completes.
func TestClaimIntentDurableBeforeBroadcast(t *testing.T) {
	makerSession, takerSession, _, ltcConn, hub, orderID := setupTxLogPair(t)
	dir := t.TempDir()
	makerSession.n.config.DataDir = dir
	makerSession.n.store.Update(hexEncode(orderID[:]), func(o *Order) { o.Mine = true })

	_, bodyA, err := makerSession.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: takerSession.pubKey})
	if err != nil {
		t.Fatalf("maker OnCreateA: %v", err)
	}
	createdA := bodyA.(*proto.CreatedABody)
	_, bodyB, err := takerSession.OnCreateB(&proto.CreateBBody{
		HubAddress: hub, ID: orderID, APubKey: makerSession.pubkey(),
		ADepositTxID: createdA.ADepositTxID, HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime,
	})
	if err != nil {
		t.Fatalf("taker OnCreateB: %v", err)
	}
	createdB := bodyB.(*proto.CreatedBBody)

	ltcConn.sendErr = errors.New("simulated claim broadcast failure")
	confirmA := &proto.ConfirmABody{HubAddress: hub, ID: orderID, BDepositTxID: createdB.BDepositTxID, BLockTime: createdB.BLockTime}
	beforeClaims := len(ltcConn.broadcasts) // taker deposit B already broadcast
	_, body, err := makerSession.OnConfirmA(confirmA)
	if err != nil {
		t.Fatalf("OnConfirmA build: %v", err)
	}
	if body != nil {
		t.Fatal("broadcast failure must yield no ConfirmedA body")
	}
	if len(ltcConn.broadcasts) != beforeClaims {
		t.Fatalf("claim broadcasts = %d, want %d (none new)", len(ltcConn.broadcasts), beforeClaims)
	}
	if makerSession.state == csConfirmedA {
		t.Fatal("claim state advanced without a broadcast")
	}
	idHex := hexEncode(orderID[:])
	if o := makerSession.n.store.Get(idHex); o == nil || o.CounterpartyRedeemed {
		t.Fatal("CounterpartyRedeemed set without a broadcast")
	}
	// The validated out-params are adopted (and thus durable) pre-broadcast.
	if makerSession.theirP2SHNative == 0 {
		t.Fatal("validated counterparty out-params not adopted pre-broadcast")
	}

	ltcConn.sendErr = nil
	_, body2, err := makerSession.OnConfirmA(confirmA)
	if err != nil {
		t.Fatalf("OnConfirmA redelivery: %v", err)
	}
	if body2 == nil {
		t.Fatal("redelivery must produce the ConfirmedA body")
	}
	if len(ltcConn.broadcasts) != beforeClaims+1 {
		t.Fatalf("claim broadcasts = %d, want %d after redelivery", len(ltcConn.broadcasts), beforeClaims+1)
	}
	if makerSession.state != csConfirmedA {
		t.Fatal("claim state did not advance after confirmed broadcast")
	}
}

// TestNoRefundAttemptWithoutBroadcast proves the cancel/escape-hatch path
// refuses an intent whose deposit never broadcast (nothing on chain to
// refund) instead of firing a doomed refund at the wallet.
func TestNoRefundAttemptWithoutBroadcast(t *testing.T) {
	n, s, conn := setupSwapPair(t)
	n.config.DataDir = t.TempDir()
	n.store.Update(hexEncode(s.id[:]), func(o *Order) { o.Mine = true })

	conn.sendErr = errors.New("simulated broadcast failure")
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
		t.Fatalf("OnCreateA build: %v", err)
	}
	idHex := hexEncode(s.id[:])

	if _, err := n.BroadcastRefund(idHex); err == nil || !strings.Contains(err.Error(), "no deposit broadcast") {
		t.Fatalf("BroadcastRefund err = %v, want no-deposit-broadcast refusal", err)
	}
	n.enqueueRefund(idHex, nil)
	if len(conn.broadcasts) != 0 {
		t.Fatalf("refund attempts = %d, want 0 (nothing on chain)", len(conn.broadcasts))
	}
	if got := n.store.Get(idHex); got == nil || got.Status == "rollback failed" {
		t.Fatal("unbroadcast order must not be marked rollback failed")
	}
}
