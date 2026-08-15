package api

import (
	"encoding/hex"
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
		got, terr = n.commitTake(key, o, TakeOrderParams{}, pkt, [32]byte{}, [33]byte{}, nil, nil, nil)
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
