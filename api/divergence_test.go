package api

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// reverseTxID renders a little-endian proto.UtxoEntry.TxID in display order
// (the inverse of the entry's storage), for matching wallet-reported txids.
func reverseTxID(id [32]byte) string {
	var rev [32]byte
	for i := 0; i < 32; i++ {
		rev[i] = id[31-i]
	}
	return hex.EncodeToString(rev[:])
}

// fakeXConn is a no-op XConn so dxTakeOrder can pass requireWrite and "broadcast"
// without a live service node.
type fakeXConn struct{}

func (fakeXConn) ReadPacket() (*proto.Packet, string, error) {
	return nil, "", io.EOF
}
func (fakeXConn) WritePacket(*proto.Packet, [20]byte) error { return nil }
func (fakeXConn) Close() error                              { return nil }

// btcAddr2 is a second valid BTC P2PKH address, used as a distinct
// from/to address so dxTakeOrder's "addresses must differ" check passes.
const btcAddr2 = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"

// TestStateOrdinal locks in the full C++ TransactionDescr::State enum mapping,
// including the two states the Go implementation previously omitted
// (trRollback=10, trRollbackFailed=11). T1.1.
func TestStateOrdinal(t *testing.T) {
	want := map[string]int{
		"expired": -1, "new": 0, "offline": 1, "open": 2, "accepting": 3,
		"hold": 4, "initialized": 5, "created": 6, "signed": 7, "commited": 8,
		"finished": 9, "rolled back": 10, "rollback failed": 11, "dropped": 12,
		"canceled": 13, "invalid": 14,
	}
	for s, w := range want {
		if got := stateOrdinal(s); got != w {
			t.Errorf("stateOrdinal(%q) = %d, want %d", s, got, w)
		}
	}
	// C++ dxCancelOrder blocks any state >= trCreated(6). A rolled-back order
	// (ordinal 10) must therefore be uncancellable — the bug was it fell through
	// to 0 and was wrongly allowed.
	if stateOrdinal("rolled back") < 6 {
		t.Error("rolled back must be uncancellable (ordinal >= trCreated)")
	}
	// And a canceled/invalid order likewise blocks cancel.
	if stateOrdinal("canceled") < 6 || stateOrdinal("invalid") < 6 {
		t.Error("canceled/invalid must be uncancellable")
	}
}

// TestDxPartialOrderChainDetailsEmptyChain verifies C++ returns an empty object
// `{}` (not an error) for an unknown / empty chain. T1.2.
func TestDxPartialOrderChainDetailsEmptyChain(t *testing.T) {
	ctx := newWalletTestCtx()
	// Valid non-null 64-hex id that matches no order (all-zeros would be the
	// uint256S null id, which C++ rejects with 1025 "bad order id").
	id := "0f" + strings.Repeat("0", 62)
	res, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(id)})
	if err != nil {
		t.Fatalf("empty chain should be {} not error: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok || len(m) != 0 {
		t.Errorf("empty chain = %v (%T), want {}", res, res)
	}
}

// TestDxPartialOrderChainDetailsInvalidId verifies a malformed order id is
// rejected with INVALID_PARAMETERS (matching C++ uint256S().IsNull() check). T1.2.
func TestDxPartialOrderChainDetailsInvalidId(t *testing.T) {
	ctx := newWalletTestCtx()
	if _, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr("xyz")}); err == nil || err.Code != errInvalidParameters {
		t.Errorf("non-hex id should be INVALID_PARAMETERS, got %v", err)
	}
}

// TestDxPartialOrderChainDetailsDeposits verifies the per-order deposit txids
// (BinTxId / OBinTxId) are emitted in p2sh_deposits / p2sh_deposits_counterparty,
// one entry per chain order (empty strings included — RPC-F28).
// T2.3.
func TestDxPartialOrderChainDetailsDeposits(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx) // BTC/BTC, open
	o.PartialAllowed = true
	o.BinTxId = "aa" + strings.Repeat("0", 62)
	o.OBinTxId = "bb" + strings.Repeat("0", 62)
	res, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(dispID(o.ID))})
	if err != nil {
		t.Fatalf("dxPartialOrderChainDetails: %v", err)
	}
	m := mustJSONMap(t, res)
	p, ok := m["p2sh_deposits"].([]interface{})
	if !ok || len(p) != 1 || p[0] != o.BinTxId {
		t.Errorf("p2sh_deposits = %v, want [%s]", m["p2sh_deposits"], o.BinTxId)
	}
	c, ok := m["p2sh_deposits_counterparty"].([]interface{})
	if !ok || len(c) != 1 || c[0] != o.OBinTxId {
		t.Errorf("p2sh_deposits_counterparty = %v, want [%s]", m["p2sh_deposits_counterparty"], o.OBinTxId)
	}
}

// TestDxTakeOrderFullTake verifies C++ treats an omitted or zero amount as a
// FULL-ORDER take (sizes equal the order's maker/taker sizes). T1.3.
func TestDxTakeOrderFullTake(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Node.conn = fakeXConn{}
	ctx.Node.sessions = make(map[string]*SwapSession)
	// Take #1 consumes the shared ctx's single 1.0 BTC funding utxo (the order's
	// Utxos are reserved via LockedUtxoInfo), so a second full take needs a
	// second funder. Same for the BLOCK fee utxos below.
	ctx.Node.config.Connectors["BTC"] = &stubConn{ticker: "BTC", addr: btcAddr, utxos: append(
		ctx.Node.config.Connectors["BTC"].(*stubConn).utxos,
		wallet.Utxo{TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0, Amount: 100000000, Value: 1.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
	)}
	// The take's service-node fee prep requires a funded BLOCK connector
	// (CRYPTO-F84, C++ acceptXBridgeTransaction :2236). Reward the shared ctx with the
	// default BLOCK conf + a 1.0 BLOCK p2pkh funder, matching newHubNode.
	ctx.Node.config.Confs["BLOCK"] = &config.CoinConf{Ticker: "BLOCK", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000, TxVersion: 1}
	// Two BLOCK funders: take #1 locks its fee utxo (LockedUtxoInfo reserves a
	// live order's FeeUtxos, C++ lockFeeUtxos :2267), so take #2 needs a second.
	u1 := blkUtxo()
	u2 := blkUtxo()
	u2.TxID = "0000000000000000000000000000000000000000000000000000000000000003"
	u2.Vout = 1
	ctx.Node.config.Connectors["BLOCK"] = &stubConn{ticker: "BLOCK", addr: btcAddr, utxos: []wallet.Utxo{u1, u2}}
	// A takeable order must have a hub that is a known servicenode;
	// dxTakeOrder refuses unregistered hubs with NO_SERVICE_NODE (C++
	// acceptXBridgeTransaction, getSn). Seed the registry with the hub.
	hubPriv := make([]byte, 32)
	hubPriv[31] = 0x5a
	hubPub := mustPub(t, hubPriv)
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion,
		PaymentAddress: coins.KeyID(hubPub[:]),
	})
	ctx.Node.snReg = reg
	o := &Order{
		ID:           [32]byte{0x07},
		Type:         OrderTypeMaker,
		FromCurrency: "BTC", FromAmount: 1500000, // maker size 1.5
		ToCurrency: "BTC", ToAmount: 300000, // 0.3
		Status:      "open",
		Mine:        false,
		SNodePubkey: hexPub(t, hubPriv),
		HubAddress:  coins.KeyID(hubPub[:]),
	}
	ctx.Store.Add(o)
	id := dispID(o.ID)

	// Amount omitted -> full take. After the maker/taker swap, result.Maker is the
	// order's ToCurrency and MakerSize is the full ToAmount.
	res, err := ctx.dxTakeOrder([]json.RawMessage{jstr(id), jstr(btcAddr), jstr(btcAddr2)})
	if err != nil {
		t.Fatalf("dxTakeOrder (no amount): %v", err)
	}
	r := res.(orderListResult)
	if r.MakerSize != formatXAmount(o.ToAmount) || r.TakerSize != formatXAmount(o.FromAmount) {
		t.Errorf("full take (no amount) = %s/%s, want %s/%s",
			r.MakerSize, r.TakerSize, formatXAmount(o.ToAmount), formatXAmount(o.FromAmount))
	}

	// Amount "0" is NOT a full take: C++ rejects an explicit amount <= 0 with
	// 1025 carrying the raw string (rpcxbridge.cpp:1151-1161, RPC-F14).
	if _, err := ctx.dxTakeOrder([]json.RawMessage{jstr(id), jstr(btcAddr), jstr(btcAddr2), jstr("0")}); err == nil {
		t.Fatal("dxTakeOrder (amount 0) should error")
	} else if err.Code != errInvalidParameters || err.Error != "Invalid parameters: The amount cannot be less than or equal to 0: 0" {
		t.Errorf("dxTakeOrder (amount 0) = %+v, want 1025 with raw amount", err)
	}
	// The amount <= 0 check runs BEFORE the order lookup: "0" on an unknown id
	// still errors 1025 (rpcxbridge.cpp:1151-1161 precedes :1175-1179).
	var missing [32]byte
	missing[0] = 0x77
	if _, err := ctx.dxTakeOrder([]json.RawMessage{jstr(dispID(missing)), jstr(btcAddr), jstr(btcAddr2), jstr("0")}); err == nil {
		t.Fatal("dxTakeOrder (unknown id, amount 0) should error")
	} else if err.Code != errInvalidParameters || !strings.Contains(err.Error, "cannot be less than or equal to 0") {
		t.Errorf("dxTakeOrder (unknown id, amount 0) = %+v, want the amount error (precedence)", err)
	}
	// The same-address gate precedes the amount gate: identical addresses win
	// over an amount of "0" (rpcxbridge.cpp:1146-1148 before :1151-1161).
	if _, err := ctx.dxTakeOrder([]json.RawMessage{jstr(id), jstr(btcAddr), jstr(btcAddr), jstr("0")}); err == nil {
		t.Fatal("dxTakeOrder (same address, amount 0) should error")
	} else if !strings.Contains(err.Error, "cannot be the same") {
		t.Errorf("dxTakeOrder (same address, amount 0) = %+v, want the same-address error (precedence)", err)
	}
}

// TestTakeOrderFundingRejectsNonP2PKH — B2 Finding 2. C++ getUnspent only funds
// a taker with 25-byte P2PKH outputs (unspentP2PKH,
// xbridgewalletconnectorbtc.cpp:1605-1638); a P2SH/multisig/OP_RETURN output is
// never spendable by the deposit path and must never enter the
// AcceptingBody's Utxos. Given a P2SH and a P2PKH funder of equal size, the
// take must select only the P2PKH; with only non-P2PKH funders it must fail
// INSUFFICIENT_FUNDS.
func TestTakeOrderFundingRejectsNonP2PKH(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// A 23-byte P2SH output (OP_HASH160 <20> OP_EQUAL, "a914..87") — NOT the
	// 25-byte P2PKH template the taker is allowed to fund with.
	p2sh := wallet.Utxo{
		TxID: strings.Repeat("ab", 32), Vout: 1,
		Amount: 30000000000, Value: 300.0,
		ScriptPubKey: "a914" + strings.Repeat("11", 20) + "87",
		Address:      addrFor(5, "p2sh-out"),
	}
	p2pkh := wallet.Utxo{
		TxID: strings.Repeat("cd", 32), Vout: 0,
		Amount: 30000000000, Value: 300.0,
		ScriptPubKey: "76a914000000000000000000000000000000000000000088ac",
		Address:      addrFor(0, "p2pkh-out"),
	}
	hubPriv := make([]byte, 32)
	hubPriv[31] = 0x42
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}

	conf := func() map[string]*config.CoinConf {
		return map[string]*config.CoinConf{
			"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
			"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
		}
	}

	t.Run("p2sh plus p2pkh selects only p2pkh", func(t *testing.T) {
		btc := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{},
			funders: []wallet.Utxo{p2sh, p2pkh}}
		n, cc := newStartedNode(t, conf(), map[string]wallet.Connector{
			"BTC":   btc,
			"BLOCK": &stubConn{ticker: "BLOCK", addr: btcAddr, utxos: []wallet.Utxo{blkUtxo()}},
		})
		registerHub(t, n, hubPriv)
		var oid [32]byte
		copy(oid[:], []byte("funding-p2pkh-only-order-0000000000"))
		n.store.Add(&Order{
			ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 1e8, ToAmount: 1e8,
			Status: "open", SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
		})
		if _, rerr := n.TakeOrder(TakeOrderParams{
			ID: orderIDString(oid), FromAddress: addrFor(0, "from"), ToAddress: addrFor(0, "to"),
		}); rerr != nil {
			t.Fatalf("take with a P2PKH alternative should succeed: %v", rerr)
		}
		o := n.store.Get(hexEncode(oid[:]))
		if o == nil || len(o.Utxos) != 1 {
			t.Fatalf("accepted order utxos = %+v, want exactly 1", o)
		}
		// The single selected entry must be the P2PKH output (cd.. txid), never
		// the P2SH one (ab..). proto.UtxoEntry.TxID is little-endian, so the
		// wall-reported display txid is the reversed form.
		if got := reverseTxID(o.Utxos[0].TxID); got != p2pkh.TxID {
			t.Errorf("selected funding txid = %s, want %s (P2PKH only)", got, p2pkh.TxID)
		}
		if len(cc.snapshot()) != 1 {
			t.Fatalf("accepting packets = %d, want 1", len(cc.snapshot()))
		}
	})

	t.Run("only non-p2pkh funders fail", func(t *testing.T) {
		btc := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{},
			funders: []wallet.Utxo{p2sh}}
		n, cc := newStartedNode(t, conf(), map[string]wallet.Connector{
			"BTC":   btc,
			"BLOCK": &stubConn{ticker: "BLOCK", addr: btcAddr, utxos: []wallet.Utxo{blkUtxo()}},
		})
		registerHub(t, n, hubPriv)
		var oid [32]byte
		copy(oid[:], []byte("funding-non-p2pkh-order-00000000000"))
		n.store.Add(&Order{
			ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 1e8, ToAmount: 1e8,
			Status: "open", SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
		})
		if _, rerr := n.TakeOrder(TakeOrderParams{
			ID: orderIDString(oid), FromAddress: addrFor(0, "from"), ToAddress: addrFor(0, "to"),
		}); rerr == nil || rerr.Code != errInsufficientFunds {
			t.Fatalf("take with only non-P2PKH funders = %v, want INSUFFICIENT_FUNDS", rerr)
		}
		if len(cc.snapshot()) != 0 {
			t.Fatalf("accepting packets = %d, want 0 (no non-P2PKH funding)", len(cc.snapshot()))
		}
	})
}

// TestDxLockedUtxoExclusion verifies that UTXOs reserved by an active order are
// excluded from dxGetUtxos (include_used=false), reported in dxGetLockedUtxos,
// and re-included (with orderid) when include_used=true. T2.1.
func TestDxLockedUtxoExclusion(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	// Reserve the stub BTC utxo (TxID all-zero, vout 0) on the order.
	o.Utxos = []proto.UtxoEntry{{TxID: [32]byte{}, Vout: 0}}
	id := dispID(o.ID)

	// dxGetLockedUtxos("") reports the locked utxo.
	res, err := ctx.dxGetLockedUtxos(nil)
	if err != nil {
		t.Fatalf("dxGetLockedUtxos: %v", err)
	}
	m := res.(map[string]interface{})
	all, ok := m["all_locked_utxo"].([]string)
	if !ok || len(all) != 1 {
		t.Fatalf("all_locked_utxo = %v, want exactly 1 locked utxo", m["all_locked_utxo"])
	}

	// dxGetUtxos(BTC) with include_used=false excludes the locked utxo entirely.
	res, err = ctx.dxGetUtxos([]json.RawMessage{json.RawMessage(`"BTC"`)})
	if err != nil {
		t.Fatalf("dxGetUtxos: %v", err)
	}
	if arr := res.([]map[string]interface{}); len(arr) != 0 {
		t.Errorf("dxGetUtxos should exclude the locked utxo, got %d", len(arr))
	}

	// With include_used=true the locked utxo is returned with its orderid set.
	res, err = ctx.dxGetUtxos([]json.RawMessage{json.RawMessage(`"BTC"`), json.RawMessage("true")})
	if err != nil {
		t.Fatalf("dxGetUtxos(used): %v", err)
	}
	arr := res.([]map[string]interface{})
	if len(arr) != 1 || arr[0]["orderid"] != id {
		t.Errorf("dxGetUtxos(used) = %v, want 1 entry with orderid=%s", arr, id)
	}
}

// TestDxLockedUtxoNativeAmount verifies dxGetLockedUtxos renders the UTXO amount
// in NATIVE coin units (matching C++ UtxoEntry::toString(), which streams the
// whole-coin double — e.g. "1" for 1 BTC), not the XBridge 1e6 scale that the
// previous port emitted ("1.000000"). This is C2. T2.1.
func TestDxLockedUtxoNativeAmount(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	// Reserve the stub BTC utxo (TxID all-zero, vout 0) on the order so it is
	// reported as locked. The stub utxo carries Amount 100000000 (= 1 BTC).
	o.Utxos = []proto.UtxoEntry{{TxID: [32]byte{}, Vout: 0}}
	res, err := ctx.dxGetLockedUtxos(nil)
	if err != nil {
		t.Fatalf("dxGetLockedUtxos: %v", err)
	}
	m := res.(map[string]interface{})
	all, ok := m["all_locked_utxo"].([]string)
	if !ok || len(all) != 1 {
		t.Fatalf("all_locked_utxo = %v, want exactly 1 locked utxo", m["all_locked_utxo"])
	}
	// Native rendering of 1 BTC must be the trimmed "1", never "1.000000"
	// (XBridge 1e6 scale) nor a raw satoshi integer.
	if !strings.Contains(all[0], ":1:") {
		t.Fatalf("locked utxo amount not rendered natively: %q (want ...:1:...)", all[0])
	}
	if strings.Contains(all[0], "1.000000") {
		t.Fatalf("locked utxo amount rendered in XBridge 1e6 scale: %q", all[0])
	}
}

// TestDxGetLockedUtxosNoReserved locks in RPC-F32: C++ getUtxoItems(id) fails
// (-> 1021 TRANSACTION_NOT_FOUND, rpcxbridge.cpp:2637) when the id has no locked
// utxos reserved (m_utxoTxMap miss). A live order with nothing reserved must
// error 1021, not return [].
func TestDxGetLockedUtxosNoReserved(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx) // open, Mine, no Utxos reserved
	_, err := ctx.dxGetLockedUtxos([]json.RawMessage{jstr(dispID(o.ID))})
	if err == nil || err.Code != errTxNotFound {
		t.Fatalf("dxGetLockedUtxos(no reserved utxos) = %v, want TRANSACTION_NOT_FOUND", err)
	}
}

// TestDxGetLockedUtxosIdEchoNormalized locks in RPC-F34: the echoed id is the
// C++ display-hex (GetHex) of the parsed value, never the raw param
// (rpcxbridge.cpp:2672). A short id left-pads to the full 64-hex form.
func TestDxGetLockedUtxosIdEchoNormalized(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrderWithUtxo(ctx, [32]byte{}) // ID {0x01}, reserves the stub utxo
	res, err := ctx.dxGetLockedUtxos([]json.RawMessage{jstr("01")})
	if err != nil {
		t.Fatalf("dxGetLockedUtxos(short id): %v", err)
	}
	m := mustJSONMap(t, res)
	if m["id"] != dispID(o.ID) {
		t.Errorf("echoed id = %v, want normalized %v", m["id"], dispID(o.ID))
	}
}

// TestDxGetLockedUtxosAmountDefaultDouble locks in RPC-F31: the amount is the
// native whole-coin double streamed with the C++ default precision 6
// (UtxoEntry::toString, xbridgewalletconnector.cpp:25-30), so 0.1234567 renders
// "0.123457" — NOT the registry fixed-decimals / XBridge 1e6 forms the previous
// port emitted.
func TestDxGetLockedUtxosAmountDefaultDouble(t *testing.T) {
	ctx := newWalletTestCtx()
	seedOrderWithUtxo(ctx, [32]byte{}) // reserves the stub BTC utxo
	btc := ctx.Node.config.Connectors["BTC"].(*stubConn)
	btc.utxos[0].Value = 0.1234567
	res, err := ctx.dxGetLockedUtxos(nil)
	if err != nil {
		t.Fatalf("dxGetLockedUtxos: %v", err)
	}
	m := mustJSONMap(t, res)
	all := m["all_locked_utxo"].([]interface{})
	if len(all) != 1 || !strings.Contains(all[0].(string), ":0.123457:") {
		t.Fatalf("all_locked_utxo = %v, want ...:0.123457:...", m["all_locked_utxo"])
	}
}

// TestDxGetLockedUtxosTerminalOrder locks in RPC-F32 for a finished/canceled
// order still in the live store (status flipped, MoveToHistory not yet run):
// lockedInfoLocked skips terminal orders (store.go:378-380) so the id has no
// locked utxos -> 1021, matching C++ where a finished transaction is in
// neither the pending nor the accepted map.
func TestDxGetLockedUtxosTerminalOrder(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrderWithUtxo(ctx, [32]byte{})
	if !ctx.Store.Update(hexEncode(o.ID[:]), func(ord *Order) { ord.Status = "finished" }) {
		t.Fatal("order not found")
	}
	_, err := ctx.dxGetLockedUtxos([]json.RawMessage{jstr(dispID(o.ID))})
	if err == nil || err.Code != errTxNotFound {
		t.Fatalf("dxGetLockedUtxos(finished live order) = %v, want TRANSACTION_NOT_FOUND", err)
	}
}

// TestFlushCancelledUnderflow verifies a huge ageMillis does not underflow uint64
// (keepTime stays large so every old entry is pruned) and age 0 prunes all
// remaining entries. It drives the C++-faithful path: cancelled orders are
// pruned from the LIVE BOOK and history (xbridgeapp.cpp:1331-1354, RPC-F35).
// T1.4.
func TestFlushCancelledUnderflow(t *testing.T) {
	s := NewStore()
	addCancelled := func(seed byte, updated uint64) {
		o := &Order{ID: [32]byte{seed}, FromCurrency: "BTC", FromAmount: 1,
			ToCurrency: "BTC", ToAmount: 1, Status: "canceled", Mine: true, Updated: updated}
		s.Add(o)
	}
	addCancelled(0xa, 100)
	addCancelled(0xb, 200)
	// A huge age makes keepTime large (no uint64 wrap), so every old entry is
	// pruned.
	if got := s.FlushCancelled(1 << 40); len(got) != 2 {
		t.Errorf("huge age should prune all 2 entries, got %d", len(got))
	}
	addCancelled(0xc, 300)
	if got := s.FlushCancelled(0); len(got) != 1 {
		t.Errorf("age 0 should prune the remaining entry, got %d", len(got))
	}
	// An absurd age whose µs conversion would overflow uint64 clamps keepTime
	// to the epoch: nothing is pruned (C++ now - absurd age is far in the past).
	addCancelled(0xd, 1)
	if got := s.FlushCancelled(math.MaxInt64); len(got) != 0 {
		t.Errorf("overflowing age should prune nothing, got %d", len(got))
	}
}
