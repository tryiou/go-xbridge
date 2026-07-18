package api

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"xbridge-go/proto"
)

// fakeXConn is a no-op XConn so dxTakeOrder can pass requireWrite and "broadcast"
// without a live service node.
type fakeXConn struct{}

func (fakeXConn) ReadPacket() (*proto.Packet, string, error) {
	return nil, "", io.EOF
}
func (fakeXConn) WritePacket(*proto.Packet) error { return nil }
func (fakeXConn) Close() error                    { return nil }

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
	// Valid 64-hex id that matches no order.
	id := "00" + strings.Repeat("0", 62)
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
// (BinTxId / OBinTxId) are emitted in p2sh_deposits / p2sh_deposits_counterparty.
// T2.3.
func TestDxPartialOrderChainDetailsDeposits(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx) // BTC/BTC, open
	o.BinTxId = "aa" + strings.Repeat("0", 62)
	o.OBinTxId = "bb" + strings.Repeat("0", 62)
	res, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(hexEncode(o.ID[:]))})
	if err != nil {
		t.Fatalf("dxPartialOrderChainDetails: %v", err)
	}
	m := res.(map[string]interface{})
	p, ok := m["p2sh_deposits"].([]string)
	if !ok || len(p) != 1 || p[0] != o.BinTxId {
		t.Errorf("p2sh_deposits = %v, want [%s]", m["p2sh_deposits"], o.BinTxId)
	}
	c, ok := m["p2sh_deposits_counterparty"].([]string)
	if !ok || len(c) != 1 || c[0] != o.OBinTxId {
		t.Errorf("p2sh_deposits_counterparty = %v, want [%s]", m["p2sh_deposits_counterparty"], o.OBinTxId)
	}
}

// TestDxTakeOrderFullTake verifies C++ treats an omitted or zero amount as a
// FULL-ORDER take (sizes equal the order's maker/taker sizes). T1.3.
func TestDxTakeOrderFullTake(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Node.conn = fakeXConn{}
	ctx.Node.cfg.PrivKey = make([]byte, 32)
	ctx.Node.sessions = make(map[string]*SwapSession)
	o := &Order{
		ID:           [32]byte{0x07},
		Type:         OrderTypeMaker,
		FromCurrency: "BTC", FromAmount: 1500000, // maker size 1.5
		ToCurrency: "BTC", ToAmount: 300000, // 0.3
		Status: "open", Mine: false,
	}
	ctx.Store.Add(o)
	id := hexEncode(o.ID[:])

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

	// Amount "0" -> also a full take.
	res, err = ctx.dxTakeOrder([]json.RawMessage{jstr(id), jstr(btcAddr), jstr(btcAddr2), jstr("0")})
	if err != nil {
		t.Fatalf("dxTakeOrder (amount 0): %v", err)
	}
	r = res.(orderListResult)
	if r.MakerSize != formatXAmount(o.ToAmount) || r.TakerSize != formatXAmount(o.FromAmount) {
		t.Errorf("full take (amount 0) = %s/%s, want %s/%s",
			r.MakerSize, r.TakerSize, formatXAmount(o.ToAmount), formatXAmount(o.FromAmount))
	}
}

// TestDxLockedUtxoExclusion verifies that UTXOs reserved by an active order are
// excluded from dxGetUtxos (include_used=false), reported in dxGetLockedUtxos,
// and re-included (with orderid) when include_used=true. T2.1.
func TestDxLockedUtxoExclusion(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	// Reserve the stub BTC utxo (TxID all-zero, vout 0) on the order.
	o.Utxos = []proto.UtxoEntry{{TxID: [32]byte{}, Vout: 0}}
	id := hexEncode(o.ID[:])

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

// TestFlushCancelledUnderflow verifies a huge ageMillis does not underflow uint64
// (it prunes everything) and age 0 prunes all remaining entries. T1.4.
func TestFlushCancelledUnderflow(t *testing.T) {
	s := NewStore()
	s.RecordCancelled("a", 100)
	s.RecordCancelled("b", 200)
	// A huge age far exceeds any utxo txtime, so every entry is pruned. The
	// uint64 subtraction must be clamped, not wrap around.
	if got := s.FlushCancelled(1 << 40); len(got) != 2 {
		t.Errorf("huge age should prune all 2 entries, got %d", len(got))
	}
	s.RecordCancelled("c", 300)
	if got := s.FlushCancelled(0); len(got) != 1 {
		t.Errorf("age 0 should prune the remaining entry, got %d", len(got))
	}
}
