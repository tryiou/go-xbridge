package api

import (
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// Phase R (refund locktime parity): the manual/force refund path
// (enqueueRefund/tryStoredRefund) must honor the deposit CLTV lockTime exactly
// like the background sweeps (scanRefunds/scanStoredRefunds) do —
// pre-locktime attempts defer ("", nil) with no broadcast, no status churn,
// and no backoff. A force-broadcast that ignores lockTime double-spends the
// deposit's CLTV window and poisons the retry backoff to its 24h cap.

// TestLiveRefundDefersPreLocktime: a live session's force-refund before the
// deposit lockTime must defer, not broadcast; past lockTime the sweep
// broadcasts exactly once and flips the record.
func TestLiveRefundDefersPreLocktime(t *testing.T) {
	n, s, conn := setupSwapPair(t)
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
		t.Fatalf("OnCreateA: %v", err)
	}
	orderIDHex := hexEncode(s.id[:])
	if s.refundHex == "" {
		t.Fatal("no pre-signed refund on the live session")
	}
	if conn.blockHeight >= int64(s.ourLockTime) {
		t.Fatalf("fixture not pre-locktime: height %d >= lockTime %d", conn.blockHeight, s.ourLockTime)
	}
	before := len(conn.broadcasts)

	var doneTx string
	var doneErr error
	n.enqueueRefund(orderIDHex, func(txid string, err error) { doneTx, doneErr = txid, err })
	if doneErr != nil {
		t.Fatalf("enqueueRefund err = %v, want deferral (\"\", nil)", doneErr)
	}
	if doneTx != "" {
		t.Fatalf("pre-locktime force-refund broadcast (txid %s), want deferral", doneTx)
	}
	if len(conn.broadcasts) != before {
		t.Fatalf("pre-locktime broadcast: %d broadcasts, want %d (deposit only)", len(conn.broadcasts), before)
	}
	if n.refundBackoffActive(orderIDHex) {
		t.Fatal("deferral armed the failure backoff: a not-yet-due refund is not a failure")
	}

	// Past lockTime the background sweep broadcasts exactly once.
	conn.blockHeight = int64(s.ourLockTime)
	n.scanRefunds()
	if len(conn.broadcasts) != before+1 {
		t.Fatalf("post-locktime broadcasts = %d, want %d", len(conn.broadcasts), before+1)
	}
	if !s.refundDone {
		t.Error("refundDone not set after post-locktime broadcast")
	}
	after := len(conn.broadcasts)
	n.scanRefunds()
	if len(conn.broadcasts) != after {
		t.Errorf("refund broadcast twice (broadcasts %d, want %d)", len(conn.broadcasts), after)
	}
}

// TestStoredRefundDefersPreLocktime: a sessionless canceled order's stored
// refund must defer pre-locktime through the manual path and be swept once
// past lockTime (status "canceled" -> "rolled back").
func TestStoredRefundDefersPreLocktime(t *testing.T) {
	n, conn, idHex, _ := storedRefundSetup(t, 1100)
	n.store.Update(idHex, func(o *Order) { o.Status = "canceled" })

	var doneTx string
	var doneErr error
	n.enqueueRefund(idHex, func(txid string, err error) { doneTx, doneErr = txid, err })
	if doneErr != nil {
		t.Fatalf("enqueueRefund err = %v, want deferral (\"\", nil)", doneErr)
	}
	if doneTx != "" {
		t.Fatalf("pre-locktime stored force-refund broadcast (txid %s), want deferral", doneTx)
	}
	if len(conn.broadcasts) != 0 {
		t.Fatalf("pre-locktime broadcast: %v", conn.broadcasts)
	}
	if n.refundBackoffActive(idHex) {
		t.Fatal("deferral armed the failure backoff: a not-yet-due refund is not a failure")
	}
	// The store sweep still stands down pre-locktime on a canceled record.
	n.scanStoredRefunds()
	if len(conn.broadcasts) != 0 {
		t.Fatalf("pre-locktime store sweep broadcast: %v", conn.broadcasts)
	}

	conn.blockHeight = 1100
	n.scanStoredRefunds()
	if len(conn.broadcasts) != 1 {
		t.Fatalf("post-locktime broadcasts = %d, want 1", len(conn.broadcasts))
	}
	if o := n.store.Get(idHex); o == nil || o.Status != "rolled back" {
		t.Fatalf("status = %+v, want rolled back after post-locktime sweep", o)
	}
}

// TestCancelWritesCanceledNotRolledBack: a counterparty cancel on a
// post-deposit order must record "canceled" (refund pending) — never
// "rolled back" (refund done) before any broadcast. A redelivered cancel is a
// dedup no-op: no second broadcast, no backoff. Past lockTime the sweep
// completes the refund and flips to "rolled back".
func TestCancelWritesCanceledNotRolledBack(t *testing.T) {
	n, s, conn := setupSwapPair(t)
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
		t.Fatalf("OnCreateA: %v", err)
	}
	orderIDHex := hexEncode(s.id[:])
	if s.refundHex == "" {
		t.Fatal("no pre-signed refund on the live session")
	}
	if conn.blockHeight >= int64(s.ourLockTime) {
		t.Fatalf("fixture not pre-locktime: height %d >= lockTime %d", conn.blockHeight, s.ourLockTime)
	}
	// Counterparty-signed cancel (snode key, like the gossip harness): the
	// store record carries DepositSent and a refund, so the cancel takes the
	// refund branch.
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x9e
	n.store.Update(orderIDHex, func(o *Order) {
		o.DepositSent = true
		o.RefundTx = s.refundHex
		o.SNodePubkey = hexPub(t, snodePriv)
	})
	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: s.id, Reason: 1}).Marshal(), snodePriv)

	before := len(conn.broadcasts)
	n.handleRemoteCancel(pkt, &proto.CancelBody{ID: s.id, Reason: 1})
	if o := n.store.Get(orderIDHex); o == nil || o.Status != "canceled" {
		t.Fatalf("status after pre-locktime cancel = %+v, want canceled (refund pending, not done)", o)
	}
	if len(conn.broadcasts) != before {
		t.Fatalf("pre-locktime cancel broadcast the refund: %d broadcasts, want %d", len(conn.broadcasts), before)
	}
	if n.refundBackoffActive(orderIDHex) {
		t.Fatal("cancel-time deferral armed the failure backoff: a not-yet-due refund is not a failure")
	}

	// Redelivery collapses into the already-canceled dedup: nothing new.
	n.handleRemoteCancel(pkt, &proto.CancelBody{ID: s.id, Reason: 1})
	if o := n.store.Get(orderIDHex); o == nil || o.Status != "canceled" {
		t.Fatalf("status after redelivered cancel = %+v, want canceled", o)
	}
	if len(conn.broadcasts) != before {
		t.Fatalf("redelivered cancel broadcast: %d broadcasts, want %d", len(conn.broadcasts), before)
	}
	if n.refundBackoffActive(orderIDHex) {
		t.Fatal("redelivered cancel armed the failure backoff")
	}

	// Past lockTime the sweep completes the refund and flips the record.
	conn.blockHeight = int64(s.ourLockTime)
	n.scanRefunds()
	if len(conn.broadcasts) != before+1 {
		t.Fatalf("post-locktime broadcasts = %d, want %d", len(conn.broadcasts), before+1)
	}
	if o := n.store.Get(orderIDHex); o == nil || o.Status != "rolled back" {
		t.Fatalf("status = %+v, want rolled back after post-locktime sweep", o)
	}
}

// TestStoredRefundBadHexNeverBroadcasts: an undecodable stored refund is
// reported, never broadcast blind against any candidate chain.
func TestStoredRefundBadHexNeverBroadcasts(t *testing.T) {
	n, ltcConn, _, _ := storedRefundSetup(t, 900)
	btcConn := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{}}
	n.config.Connectors["BTC"] = btcConn
	var doneTx string
	var doneErr error
	n.tryStoredRefund("deadbeef", "zz-nohex", 900, []string{"LTC", "BTC"}, func(txid string, err error) {
		doneTx, doneErr = txid, err
	})
	if doneErr == nil {
		t.Fatal("undecodable refund hex must report an error")
	}
	if doneTx != "" {
		t.Fatalf("undecodable refund broadcast (txid %s)", doneTx)
	}
	if len(ltcConn.broadcasts) != 0 || len(btcConn.broadcasts) != 0 {
		t.Fatalf("broadcast attempted on undecodable hex: ltc=%v btc=%v", ltcConn.broadcasts, btcConn.broadcasts)
	}
}

// TestStoredRefundTriesOwnChainFirst: the stored-refund candidate order must
// start with the order's own chain (utxoCurrency). A deferral on the right
// chain is reported without attempting any other chain.
func TestStoredRefundTriesOwnChainFirst(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	ltcConn := &fakeConnector{ticker: "LTC", blockHeight: 1000, rawTx: map[string]string{}}
	btcConn := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"LTC": ltcConn, "BTC": btcConn})
	// Decodable refund with a future lockTime, on the order's own chain (BTC)
	// while the legacy From-first order would try LTC.
	tx := &coins.Tx{Version: 1, LockTime: 1100}
	tx.Inputs = append(tx.Inputs, coins.TxIn{PrevOut: coins.OutPoint{Hash: [32]byte{0x33}, Index: 0}, Sequence: 0xfffffffe})
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: 100000, ScriptPubKey: []byte{0x51}})
	hexStr := hexEncode(tx.Serialize())
	var oid [32]byte
	copy(oid[:], []byte("own-chain-first-order-000000"))
	idHex := hexEncode(oid[:])
	n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "LTC", ToCurrency: "BTC",
		DepositSent: true, RefundTx: hexStr, UtxoCurrency: "BTC", Status: "canceled"})

	var doneTx string
	var doneErr error
	n.enqueueRefund(idHex, func(txid string, err error) { doneTx, doneErr = txid, err })
	if doneErr != nil {
		t.Fatalf("enqueueRefund err = %v, want deferral (\"\", nil)", doneErr)
	}
	if doneTx != "" {
		t.Fatalf("force-refund broadcast (txid %s) pre-locktime, want deferral on the own chain", doneTx)
	}
	if len(ltcConn.broadcasts) != 0 {
		t.Fatalf("wrong-chain broadcast on LTC: %v", ltcConn.broadcasts)
	}
	if len(btcConn.broadcasts) != 0 {
		t.Fatalf("pre-locktime broadcast on BTC: %v", btcConn.broadcasts)
	}
}
