package api

// Deposit-build guards: fail-closed zero locktime (C++ xbridgesession
// :2037-2043) and largest-funding change address (:2102). In-memory
// fixtures — no live hub, no network.
import (
	"encoding/hex"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/wallet"
)

type errBlockConn struct {
	wallet.Connector
}

func (e errBlockConn) GetBlockCount() (int64, error) { return 0, errNotFound }

// TestBuildDepositFailsClosedOnLocktimeZero pins the C++ lockTime==0 cancel
// guard (xbridgesession.cpp:2037-2043): no zero-lockTime (immediately
// refundable) HTLC may be built.
func TestBuildDepositFailsClosedOnLocktimeZero(t *testing.T) {
	alignInitCoins(t)

	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: testTxID("aa"), Amount: 5e8}, changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc})

	var id [32]byte
	oid := hash20("align-locktime-zero")
	copy(id[:], oid[:])
	mPriv, mPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{btc.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m-src"), TakerAddress: addrFor(48, "m-dst")}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(id[:])]

	c := s.snapshot()
	c.connectors["BTC"] = errBlockConn{btc}
	if _, err := c.buildDeposit(true); err == nil || !strings.Contains(err.Error(), "locktime") {
		t.Fatalf("buildDeposit with unavailable tip = %v, want locktime fail-closed error", err)
	}
}

// TestBuildDepositChangeToLargestFunding pins C++ largestUtxo.address change
// (xbridgesession.cpp:2102): change returns to the largest funding UTXO's
// address, a wallet-watched address, not a fresh one.
func TestBuildDepositChangeToLargestFunding(t *testing.T) {
	alignInitCoins(t)

	fPriv, fPub := newKey(t)
	fundScript := hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(fPub)))
	big, small := wallet.Utxo{TxID: testTxID("aa"), Vout: 0, Amount: 5e8,
		Address: addrFor(0, "big-funding"), ScriptPubKey: fundScript},
		wallet.Utxo{TxID: testTxID("bb"), Vout: 1, Amount: 1e8,
			Address: addrFor(0, "small-funding"), ScriptPubKey: fundScript}
	btc := &fakeConnector{ticker: "BTC", funding: big, fundingPriv: fPriv, fundingPub: fPub,
		changeAddr: addrFor(0, "fresh-change"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc, "LTC": btc})

	var id [32]byte
	oid := hash20("align-change-addr")
	copy(id[:], oid[:])
	mPriv, mPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 1e6}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{small, big}),
		MakeOrderParams{MakerAddress: addrFor(0, "m-src"), TakerAddress: addrFor(48, "m-dst")}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(id[:])]

	ctx := s.snapshot()
	out, err := ctx.buildDeposit(true)
	if err != nil {
		t.Fatalf("buildDeposit: %v", err)
	}
	raw, err := hex.DecodeString(out.depositHex)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := coins.Deserialize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Outputs) != 2 {
		t.Fatalf("want deposit+change outputs, got %d", len(tx.Outputs))
	}
	bigHash := hash20("big-funding")
	want := coins.BuildP2PKHScript(bigHash)
	if hex.EncodeToString(tx.Outputs[1].ScriptPubKey) != hex.EncodeToString(want) {
		t.Fatalf("change script = %x, want P2PKH of largest funding address", tx.Outputs[1].ScriptPubKey)
	}
}
