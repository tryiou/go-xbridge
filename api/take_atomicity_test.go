package api

import (
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// TestTakeCommitOrderGoneNoBroadcast locks in the TOCTOU fix: if the order is
// cancelled/removed between the HTTP snapshot and the engine's authoritative
// re-check (commitTake), NO Accepting packet may leave the wire and the take's
// reservation must be released. commitTake is the engine-side unit: the
// re-check runs before WritePacket, so a take never leaves the wire that then
// returns an error (C++ acceptXBridgeTransaction commits and sends on one
// thread, xbridgeapp.cpp:2122-2380).
func TestTakeCommitOrderGoneNoBroadcast(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	cc := &captureXConn{}
	n := newTestNode(t, map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}, map[string]wallet.Connector{"BTC": &stubConn{ticker: "BTC", addr: btcAddr}})
	n.conn = cc

	// A live open order the HTTP goroutine snapshots.
	o := &Order{
		ID: [32]byte{0x07}, Type: OrderTypeMaker, FromCurrency: "BTC", ToCurrency: "BTC",
		FromAmount: 1000000, ToAmount: 2000000, Status: "open",
	}
	key := hexEncode(o.ID[:])
	n.store.Add(o)

	// The HTTP goroutine reserved the take's inputs atomically.
	if got := n.store.ReserveForTake(key, []string{"blk:0"}, []string{"fund:1"}, "BTC"); got != reserveOK {
		t.Fatalf("ReserveForTake = %v, want reserveOK", got)
	}

	// A remote cancel lands on the engine between the HTTP snapshot and the
	// engine re-check: the order is moved to history (removed from the live
	// book), so commitTake's re-check must see it gone.
	n.store.MoveToHistoryU32(key, "canceled", 10, NowMicro())

	// Engine-side commit. Sign a dummy Accepting so the write would succeed if
	// the re-check were skipped.
	pkt := proto.NewPacket(proto.XbcTransactionAccepting, (&proto.AcceptingBody{ID: o.ID}).Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	var got *Order
	var terr *rpcError
	n.submit(func() {
		got, terr = n.commitTake(key, TakeOrderParams{}, pkt, [32]byte{}, [33]byte{}, nil, nil, nil)
	}, true)
	if terr != nil {
		t.Fatalf("commitTake on a gone order errored: %v", terr)
	}
	if got != nil {
		t.Fatalf("commitTake committed an order that was cancelled: %+v", got)
	}
	// No Accepting packet may reach the wire.
	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("Accepting broadcast for a cancelled order: %d packets", len(pkts))
	}
	// The reservation must be released so the inputs can be reused.
	// MoveToHistoryU32 deletes the order from the live book, which makes
	// LockedUtxoInfo skip its owner order regardless of the reservation, so
	// assert the reserved map directly (the release is the behavior under
	// test) AND prove the keys are reusable by a fresh order.
	if got := len(n.store.reserved); got != 0 {
		t.Errorf("%d reservation(s) still held after gone-order commit", got)
	}
	o2 := &Order{ID: [32]byte{0x08}, Type: OrderTypeMaker, FromCurrency: "BTC", ToCurrency: "BTC",
		FromAmount: 1000000, ToAmount: 2000000, Status: "open"}
	n.store.Add(o2)
	if got := n.store.ReserveForTake(hexEncode(o2.ID[:]), []string{"blk:0"}, []string{"fund:1"}, "BTC"); got != reserveOK {
		t.Errorf("reuse of released keys after gone-order commit = %v, want reserveOK", got)
	}
}

// TestTakeCommitSetsMineAndPersists locks in the local-taker durability fix: a
// committed take must mark the order Mine=true (so snapshotSwaps persists it,
// like C++ saveOrders' isLocal filter) and the durable swap file must contain
// it after persistNow. It also proves the Orig* currencies come from the live
// book record: commitTake takes no order snapshot at all, so the values
// necessarily come from the live book, never a stale HTTP snapshot.
func TestTakeCommitSetsMineAndPersists(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	cc := &captureXConn{}
	n := newTestNode(t, map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}, map[string]wallet.Connector{"BTC": &stubConn{ticker: "BTC", addr: btcAddr}})
	n.conn = cc
	n.config.DataDir = t.TempDir()

	o := &Order{
		ID: [32]byte{0x09}, Type: OrderTypeMaker, FromCurrency: "BTC", ToCurrency: "SYS",
		FromAmount: 1000000, ToAmount: 2000000, Status: "open",
	}
	key := hexEncode(o.ID[:])
	n.store.Add(o)

	// commitTake takes no order snapshot at all — Orig*, UtxoCurrency, the
	// session pinning, and the broadcast destination necessarily come from
	// the live book record, so a stale HTTP snapshot can never leak into
	// the committed take. Assert they match the live BTC/SYS pair.
	pkt := proto.NewPacket(proto.XbcTransactionAccepting, (&proto.AcceptingBody{ID: o.ID}).Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	var got *Order
	var terr *rpcError
	n.submit(func() {
		got, terr = n.commitTake(key, TakeOrderParams{}, pkt, [32]byte{}, [33]byte{}, nil, nil, nil)
	}, true)
	if terr != nil {
		t.Fatalf("commitTake errored: %v", terr)
	}
	if got == nil {
		t.Fatal("commitTake returned nil on a live order")
	}
	if !got.Mine {
		t.Error("committed take has Mine=false, want true (local taker swap must persist)")
	}
	if got.OrigFromCurrency != "BTC" || got.OrigToCurrency != "SYS" {
		t.Errorf("Orig currencies = %q/%q, want live BTC/SYS",
			got.OrigFromCurrency, got.OrigToCurrency)
	}
	if got.UtxoCurrency != "SYS" {
		t.Errorf("UtxoCurrency = %q, want live SYS", got.UtxoCurrency)
	}
	if pkts := cc.snapshot(); len(pkts) != 1 {
		t.Fatalf("wrote %d packets, want 1 Accepting broadcast", len(pkts))
	}
	s, ok := n.sessions[key]
	if !ok {
		t.Fatal("no taker session registered for the committed take")
	}
	// The session must pin the live currencies too.
	if s.srcCur != "SYS" || s.dstCur != "BTC" {
		t.Errorf("session currencies = %q/%q, want live SYS/BTC",
			s.srcCur, s.dstCur)
	}
	// The durable copy must contain the taken order: a restart must rebuild it.
	ps, _, _, err := loadSwaps(swapStatePath(n.config.DataDir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	found := false
	for _, p := range ps {
		if p.ID == o.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("durable swap file lacks taken order %s (has %d swaps)", key, len(ps))
	}
}

// TestTakeCommitSendFailureReverts proves a failed Accepting broadcast rolls
// back the store entry to its exact pre-take record, drops the taker session,
// broadcasts nothing, and leaves no phantom swap on disk.
func TestTakeCommitSendFailureReverts(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	cc := &captureXConn{}
	n := newTestNode(t, map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}, map[string]wallet.Connector{"BTC": &stubConn{ticker: "BTC", addr: btcAddr}})
	n.config.DataDir = t.TempDir()
	fwc := &failWriteConn{captureXConn: cc}
	n.conn = fwc

	o := &Order{
		ID: [32]byte{0x0a}, Type: OrderTypeMaker, FromCurrency: "BTC", ToCurrency: "SYS",
		FromAmount: 1000000, ToAmount: 2000000, Status: "open",
	}
	key := hexEncode(o.ID[:])
	n.store.Add(o)

	pkt := proto.NewPacket(proto.XbcTransactionAccepting, (&proto.AcceptingBody{ID: o.ID}).Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	var got *Order
	var terr *rpcError
	n.submit(func() {
		got, terr = n.commitTake(key, TakeOrderParams{}, pkt, [32]byte{}, [33]byte{}, nil, nil, nil)
	}, true)
	if terr == nil {
		t.Fatal("commitTake with failing send should error")
	}
	if terr.Code != errUnknown {
		t.Fatalf("commitTake error code = %d, want errUnknown (%d)", terr.Code, errUnknown)
	}
	if !fwc.called {
		t.Fatal("WritePacket was never called — the revert path was not reached")
	}
	if got != nil {
		t.Fatalf("expected nil order on failed send, got %+v", got)
	}
	assertTakeReverted(t, n, key)
	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("wrote %d packets on failed send, want 0", len(pkts))
	}
	ps, _, _, err := loadSwaps(swapStatePath(n.config.DataDir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 0 {
		t.Fatalf("persisted %d swaps after failed send, want 0 (orphan leaked on disk)", len(ps))
	}
}

// TestTakeCommitPersistFailureReverts proves a failed durable write rolls back
// the store entry and session before anything reaches the wire.
func TestTakeCommitPersistFailureReverts(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	cc := &captureXConn{}
	n := newTestNode(t, map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}, map[string]wallet.Connector{"BTC": &stubConn{ticker: "BTC", addr: btcAddr}})
	n.conn = cc
	n.config.DataDir = t.TempDir()
	orig := writeSwaps
	writeSwaps = func(path string, data []byte) error { return errors.New("injected disk failure") }
	t.Cleanup(func() { writeSwaps = orig })

	o := &Order{
		ID: [32]byte{0x0b}, Type: OrderTypeMaker, FromCurrency: "BTC", ToCurrency: "SYS",
		FromAmount: 1000000, ToAmount: 2000000, Status: "open",
	}
	key := hexEncode(o.ID[:])
	n.store.Add(o)

	pkt := proto.NewPacket(proto.XbcTransactionAccepting, (&proto.AcceptingBody{ID: o.ID}).Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	var got *Order
	var terr *rpcError
	n.submit(func() {
		got, terr = n.commitTake(key, TakeOrderParams{}, pkt, [32]byte{}, [33]byte{}, nil, nil, nil)
	}, true)
	if terr == nil {
		t.Fatal("commitTake with failing persist should error")
	}
	if got != nil {
		t.Fatalf("expected nil order on failed persist, got %+v", got)
	}
	assertTakeReverted(t, n, key)
	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("wrote %d packets on failed persist, want 0 (send must follow durability)", len(pkts))
	}
}

// assertTakeReverted checks the store entry for key is back to its pre-take
// remote-order record and no taker session remains.
func assertTakeReverted(t *testing.T, n *Node, key string) {
	t.Helper()
	restored := n.store.Get(key)
	if restored == nil {
		t.Fatalf("order %s missing after revert, want pre-take record restored", key)
	}
	if restored.Status != "open" {
		t.Errorf("Status = %q, want open", restored.Status)
	}
	if restored.Mine {
		t.Error("Mine = true after revert, want false")
	}
	if restored.Role != 0 {
		t.Errorf("Role = %q, want 0", restored.Role)
	}
	if restored.MakerKey != "" {
		t.Errorf("MakerKey = %q, want empty", restored.MakerKey)
	}
	if restored.OrigFromCurrency != "" || restored.OrigToCurrency != "" {
		t.Errorf("Orig currencies = %q/%q, want empty", restored.OrigFromCurrency, restored.OrigToCurrency)
	}
	if len(restored.Utxos) != 0 || len(restored.UsedCoins) != 0 || len(restored.FeeUtxos) != 0 {
		t.Errorf("funding residue after revert: %d utxos/%d used/%d fee, want 0/0/0",
			len(restored.Utxos), len(restored.UsedCoins), len(restored.FeeUtxos))
	}
	if _, ok := n.sessions[key]; ok {
		t.Error("taker session still registered after revert, want removed")
	}
}

// TestTakeOrderCancelRaceNoBroadcast drives the full TakeOrder path and races a
// remote cancel against it: a cancel landing before the engine re-check must
// yield an error (not a take) with ZERO Accepting packets on the wire. The
// winner ordering is nondeterministic, so the test accepts either outcome
// (take succeeds and commits, or cancel wins and the take fails cleanly), but
// asserts the cross-cutting invariant: every TakeOrder that returned an error
// broadcast nothing. Run under -race.
func TestTakeOrderCancelRaceNoBroadcast(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}); err != nil {
		t.Fatal(err)
	}
	blk := blkUtxo()
	n, cc := newStartedNode(t, map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}, map[string]wallet.Connector{
		"BTC": &stubConn{ticker: "BTC", addr: btcAddr, utxos: []wallet.Utxo{
			{TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0,
				Amount: 300000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
		}},
		"BLOCK": &stubConn{ticker: "BLOCK", addr: btcAddr, utxos: []wallet.Utxo{blk}},
	})

	hubPriv := make([]byte, 32)
	hubPriv[31] = 2
	registerHub(t, n, hubPriv)
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}

	var oid [32]byte
	copy(oid[:], []byte("cancel-race-order-000000000000"))
	o := &Order{
		ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		Status: "open", SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.store.Add(o)
	key := hexEncode(oid[:])

	const rounds = 50
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		// Re-seed the order each round (a successful take consumes it; the
		// cancel may also remove it). The invariant is on the packet count per
		// errored take, independent of who wins.
		if n.store.Get(key) == nil {
			o2 := *o
			n.store.Add(&o2)
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			before := len(cc.snapshot())
			_, rerr := n.TakeOrder(TakeOrderParams{
				ID: orderIDString(oid), FromAddress: addrFor(0, "race-from"), ToAddress: addrFor(0, "race-to"),
			})
			if rerr != nil {
				if got := len(cc.snapshot()); got != before {
					t.Errorf("errored TakeOrder broadcast %d packet(s): %v", got-before, rerr)
				}
			}
		}()
		go func() {
			defer wg.Done()
			// A remote cancel packet signed by the order's pinned hub (the SN
			// that broadcast the order) — handleRemoteCancel accepts it via the
			// o.SNodePubkey verification path.
			body := &proto.CancelBody{ID: oid, Reason: 10}
			pkt := proto.NewPacket(proto.XbcTransactionCancel, body.Marshal())
			if err := crypto.NewBtcSigner().Sign(pkt, hubPriv); err != nil {
				t.Errorf("sign cancel: %v", err)
				return
			}
			n.submit(func() { n.handleRemoteCancel(pkt, body) }, false)
		}()
		wg.Wait()
	}
}

// TestTakeFeeSelectionAvoidsOtherOrderFundingLock reproduces the live n=11
// failure (run13 1019 "cannot reuse utxo inputs" on a fresh take of a
// different order): an open BLOCK maker order (M0) locks its funding BLOCK
// utxo u1 via LockedUtxoInfo; the fee-utxo selection for a take whose
// funding currency is BTC must still exclude u1, because ReserveForTake
// checks feeKeys against the ALL-token locked set (lockedInfoLocked — fee
// utxos are the global BLOCK fee pool, C++ m_feeUtxos, checked for every
// take). Selecting u1 therefore always collides: the fee selection filter
// and the reservation check must see the same locked set. With the fix the
// fee prep falls through to u2 and the take broadcasts; u1 stays locked by
// M0 for M0's lifetime.
func TestTakeFeeSelectionAvoidsOtherOrderFundingLock(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Two BLOCK fee-eligible funders: u1 (small) is locked by M0, u2 (large)
	// is the only remaining candidate. selectFeeUtxos prefers the smallest
	// sufficient single input, so the RED path picks u1 and collides.
	u1 := blkUtxo()
	u2 := blkUtxo()
	u2.TxID = "0000000000000000000000000000000000000000000000000000000000000003"
	u2.Vout = 1
	n := newTestNode(t, map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}, map[string]wallet.Connector{
		"BTC": &stubConn{ticker: "BTC", addr: btcAddr, utxos: []wallet.Utxo{
			{TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0,
				Amount: 300000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
		}},
		"BLOCK": &stubConn{ticker: "BLOCK", addr: btcAddr, utxos: []wallet.Utxo{u1, u2}},
	})
	cc := &captureXConn{}
	n.conn = cc
	n.config.DataDir = t.TempDir()
	n.start()
	t.Cleanup(func() { _ = n.Close() })

	hubPriv := make([]byte, 32)
	hubPriv[31] = 3
	registerHub(t, n, hubPriv)
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}

	// M0: an observed maker order selling BLOCK whose funding proof is u1 —
	// its Utxos lock u1 for M0's whole lifetime (lockedInfoLocked derives the
	// locked set from live orders' Utxos).
	var u1raw [32]byte
	raw, err := hex.DecodeString(u1.TxID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		u1raw[i] = raw[31-i]
	}
	m0 := &Order{
		ID: [32]byte{0x31}, Type: OrderTypeMaker, FromCurrency: "BLOCK", ToCurrency: "BTC",
		FromAmount: 1e8, ToAmount: 1e8, Status: "open", UtxoCurrency: "BLOCK",
		Utxos: []proto.UtxoEntry{{TxID: u1raw, Vout: u1.Vout}},
	}
	n.store.Add(m0)
	u1Key := u1.TxID + ":0"
	keys, _ := n.store.LockedUtxoInfo()
	if !keys[u1Key] {
		t.Fatal("pre-take: M0's BLOCK funding utxo not reported locked")
	}

	// T: a plain BTC/BTC order for the take; its fee prep is BLOCK.
	var oid [32]byte
	copy(oid[:], []byte("fee-lock-order-00000000000000"))
	takePub := mustPub(t, hubPriv)
	take := &Order{
		ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		Status: "open", SNodePubkey: hexEncode(takePub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.store.Add(take)

	res, rerr := n.TakeOrder(TakeOrderParams{
		ID: orderIDString(oid), FromAddress: btcAddr, ToAddress: btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("dxTakeOrder: %v (fee selection must avoid M0's locked u1 and fall through to u2)", rerr)
	}
	if res.Status != "accepting" {
		t.Fatalf("take status = %q, want accepting", res.Status)
	}
	// The committed take's fee utxo must be u2 — u1 is M0's.
	takeKey := hexEncode(oid[:])
	got := n.store.Get(takeKey)
	if got == nil {
		t.Fatal("taken order missing from store")
	}
	if len(got.FeeUtxos) != 1 || got.FeeUtxos[0].TxID != u2.TxID {
		t.Fatalf("take fee utxo = %+v, want exactly u2 (%s) — u1 is locked by M0", got.FeeUtxos, u2.TxID)
	}
	// u1 must still be locked — by M0, not by the take (byOrder reports the
	// display id, Store.lockedInfoLocked's orderIDString).
	keys, byOrder := n.store.LockedUtxoInfo()
	if !keys[u1Key] || byOrder[u1Key] != orderIDString(m0.ID) {
		t.Fatalf("M0's u1 lock vanished or was re-attributed: keys[%s]=%v byOrder=%v want %v", u1Key, keys[u1Key], byOrder[u1Key], orderIDString(m0.ID))
	}
}
