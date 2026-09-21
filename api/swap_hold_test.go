package api

// Maker Hold resizing: a partial take resizes the session and the stored
// order to the partial amounts (C++ xbridgesession.cpp:1525-1528); full takes
// are unaffected. In-memory fixtures — no live hub, no network.
import (
	"testing"

	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// TestHoldResizesMakerToPartialAmounts pins the C++ maker Hold reassignment
// (xbridgesession.cpp:1525-1528 `fromAmount=damount;toAmount=samount`): a
// partial take resizing the session AND the stored order so the maker deposit
// locks the partial amount. Full takes are unaffected.
func TestHoldResizesMakerToPartialAmounts(t *testing.T) {
	alignInitCoins(t)

	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: testTxID("aa")}, changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc, "LTC": btc})

	var id [32]byte
	oid := hash20("align-partial")
	copy(id[:], oid[:])
	mPriv, mPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 10e6, ToAmount: 8e6, PartialAllowed: true, MinFromAmount: 1e6}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{btc.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m-src"), TakerAddress: addrFor(48, "m-dst")}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(id[:])]

	var hub [20]byte
	// Partial take: taker gives 4e6 of 8e6, takes 5e6 of 10e6 (exact ratio).
	cmd, body, err := s.OnHold(&proto.HoldBody{HubAddress: hub, ID: id, FromAmount: 4e6, ToAmount: 5e6})
	if err != nil || cmd != proto.XbcTransactionHoldApply || body == nil {
		t.Fatalf("OnHold partial: cmd=%v body=%v err=%v", cmd, body, err)
	}
	if s.srcAmt != 5e6 || s.dstAmt != 4e6 {
		t.Fatalf("session amounts = (%d,%d), want resized (5e6,4e6)", s.srcAmt, s.dstAmt)
	}
	if got := n.store.Get(hexEncode(id[:])); got.FromAmount != 5e6 || got.ToAmount != 4e6 {
		t.Fatalf("stored order amounts = (%d,%d), want resized (5e6,4e6)", got.FromAmount, got.ToAmount)
	}

	// Taker side never resizes: exact-match session keeps its amounts.
	tPriv, tPub := newKey(t)
	to := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 10e6, ToAmount: 8e6}
	n.newTakerSession(withUsedCoins(t, n, to, []wallet.Utxo{btc.funding}),
		TakeOrderParams{FromAddress: addrFor(48, "t-src"), ToAddress: addrFor(0, "t-dst")}, arr32(tPriv), toArr33(tPub))
	ts := n.sessions[hexEncode(id[:])]
	beforeSrc, beforeDst := ts.srcAmt, ts.dstAmt
	if _, _, err := ts.OnHold(&proto.HoldBody{HubAddress: hub, ID: id, FromAmount: beforeSrc, ToAmount: beforeDst}); err != nil {
		t.Fatalf("taker OnHold: %v", err)
	}
	if ts.srcAmt != beforeSrc || ts.dstAmt != beforeDst {
		t.Fatalf("taker session resized to (%d,%d), want unchanged (%d,%d)", ts.srcAmt, ts.dstAmt, beforeSrc, beforeDst)
	}
}
