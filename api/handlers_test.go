package api

import (
	"encoding/json"
	"testing"
)

// jstr wraps a Go string as a JSON-RawMessage param (a quoted
// JSON string), matching how api/handlers.go expects positional
// string params.
func jstr(s string) json.RawMessage {
	return json.RawMessage([]byte(`"` + s + `"`))
}

// seedOrder adds a BTC/BTC order (both currencies known to the test
// conf) to the store, returning it. From==To is fine for exercising
// the store-read paths (dxGetOrders / dxGetOrder / dxGetMyOrders /
// dxGetOrderBook).
func seedOrder(ctx *HandlerCtx) *Order {
	id := [32]byte{0x01}
	o := &Order{
		ID:           id,
		Type:         OrderTypeMaker,
		FromCurrency: "BTC",
		FromAmount:   1500000,
		ToCurrency:   "BTC",
		ToAmount:     300000,
		Created:      uint64(1),
		Updated:      uint64(1),
		Status:       "open",
		Mine:         true,
		MakerAddress: "mk",
		TakerAddress: "tk",
	}
	ctx.Store.Add(o)
	return o
}

func TestDxGetOrdersRead(t *testing.T) {
	ctx := newWalletTestCtx()
	seedOrder(ctx)

	// dxGetOrders: no params, returns the open order.
	res, err := ctx.dxGetOrders(nil)
	if err != nil {
		t.Fatalf("dxGetOrders: %v", err)
	}
	arr, ok := res.([]orderListResult)
	if !ok || len(arr) != 1 {
		t.Fatalf("dxGetOrders = %v (%T)", res, res)
	}
	if arr[0].Maker != "BTC" || arr[0].MakerSize != "1.500000" {
		t.Errorf("dxGetOrders entry = %+v", arr[0])
	}

	// dxGetOrders rejects params.
	if _, err := ctx.dxGetOrders([]json.RawMessage{jstr("x")}); err == nil {
		t.Error("dxGetOrders should reject params")
	}
}

func TestDxGetOrderRead(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)

	res, err := ctx.dxGetOrder([]json.RawMessage{jstr(hexEncode(o.ID[:]))})
	if err != nil {
		t.Fatalf("dxGetOrder: %v", err)
	}
	if r, ok := res.(orderListResult); !ok || r.ID != hexEncode(o.ID[:]) {
		t.Fatalf("dxGetOrder = %v (%T)", res, res)
	}

	if _, err := ctx.dxGetOrder([]json.RawMessage{jstr("deadbeef")}); err == nil {
		t.Fatal("dxGetOrder(missing) should error")
	}
	if _, err := ctx.dxGetOrder(nil); err == nil {
		t.Error("dxGetOrder(nil) should error")
	}
}

func TestDxGetMyOrdersRead(t *testing.T) {
	ctx := newWalletTestCtx()
	seedOrder(ctx)
	res, err := ctx.dxGetMyOrders(nil)
	if err != nil {
		t.Fatalf("dxGetMyOrders: %v", err)
	}
	if arr, ok := res.([]orderDetailResult); !ok || len(arr) != 1 {
		t.Fatalf("dxGetMyOrders = %v (%T)", res, res)
	}
}

func TestDxGetOrderBookRead(t *testing.T) {
	ctx := newWalletTestCtx()
	seedOrder(ctx)
	// detail=1, maker=BTC, taker=BTC -> the BTC/BTC order (From=BTC,To=BTC)
	// matches both the ask and bid filters, so it lands in both sides.
	res, err := ctx.dxGetOrderBook([]json.RawMessage{
		jstr("1"), jstr("BTC"), jstr("BTC"),
	})
	if err != nil {
		t.Fatalf("dxGetOrderBook: %v", err)
	}
	ob, ok := res.(orderBookResult)
	if !ok {
		t.Fatalf("dxGetOrderBook = %v (%T)", res, res)
	}
	if len(ob.Asks) != 1 || len(ob.Bids) != 1 {
		t.Errorf("dxGetOrderBook asks=%d bids=%d", len(ob.Asks), len(ob.Bids))
	}
	if ob.Maker != "BTC" || ob.Taker != "BTC" {
		t.Errorf("dxGetOrderBook maker/taker = %q/%q", ob.Maker, ob.Taker)
	}
	// detail=1 entry shape: [price, size, count].
	if len(ob.Asks) == 1 {
		if e := ob.Asks[0]; len(e) != 3 {
			t.Errorf("detail=1 ask entry = %v", ob.Asks[0])
		}
	}
	if len(ob.Bids) == 1 {
		if e := ob.Bids[0]; len(e) != 3 {
			t.Errorf("detail=1 bid entry = %v", ob.Bids[0])
		}
	}
}

func TestDxGetOrderBookDetailLevels(t *testing.T) {
	ctx := newWalletTestCtx()
	seedOrder(ctx)
	// detail out of range -> errInvalidDetailLevel.
	if _, err := ctx.dxGetOrderBook([]json.RawMessage{jstr("0"), jstr("BTC"), jstr("BTC")}); err == nil {
		t.Error("detail=0 should error")
	}
	if _, err := ctx.dxGetOrderBook([]json.RawMessage{jstr("5"), jstr("BTC"), jstr("BTC")}); err == nil {
		t.Error("detail=5 should error")
	}
	if _, err := ctx.dxGetOrderBook([]json.RawMessage{jstr("2"), jstr("BTC"), jstr("BTC")}); err != nil {
		t.Fatalf("detail=2: %v", err)
	}
	if _, err := ctx.dxGetOrderBook([]json.RawMessage{jstr("3"), jstr("BTC"), jstr("BTC")}); err != nil {
		t.Fatalf("detail=3: %v", err)
	}
	// detail=4: best ask/bid with an order-id array.
	res, err := ctx.dxGetOrderBook([]json.RawMessage{jstr("4"), jstr("BTC"), jstr("BTC")})
	if err != nil {
		t.Fatalf("detail=4: %v", err)
	}
	ob := res.(orderBookResult)
	if len(ob.Asks) != 1 {
		t.Fatalf("detail=4 asks=%d", len(ob.Asks))
	}
	if e := ob.Asks[0]; len(e) != 3 {
		t.Errorf("detail=4 ask entry = %v", e)
	} else if ids, ok := e[2].([]string); !ok || len(ids) != 1 {
		t.Errorf("detail=4 ask ids = %v", e[2])
	}
}

func TestDxGetOrderFillsRead(t *testing.T) {
	ctx := newWalletTestCtx()
	res, err := ctx.dxGetOrderFills([]json.RawMessage{jstr("BTC"), jstr("LTC")})
	if err != nil {
		t.Fatalf("dxGetOrderFills: %v", err)
	}
	if arr, ok := res.([]fillOut); !ok || len(arr) != 0 {
		t.Fatalf("dxGetOrderFills = %v (%T)", res, res)
	}
}

func TestDxGetLockedAndFlush(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)

	// No id -> all_locked_utxo (empty; backing not wired yet).
	res, err := ctx.dxGetLockedUtxos(nil)
	if err != nil {
		t.Fatalf("dxGetLockedUtxos (empty) = %v %v", res, err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("dxGetLockedUtxos (empty) = %v (%T)", res, res)
	}
	all, ok := m["all_locked_utxo"].([]string)
	if !ok || len(all) != 0 {
		t.Fatalf("dxGetLockedUtxos all_locked_utxo = %v", m["all_locked_utxo"])
	}

	// id -> object keyed by id and the order's currency (empty utxo list).
	ctx.Store.Lock(o)
	id := hexEncode(o.ID[:])
	res, _ = ctx.dxGetLockedUtxos([]json.RawMessage{jstr(id)})
	m, ok = res.(map[string]interface{})
	if !ok {
		t.Fatalf("dxGetLockedUtxos (id) = %v (%T)", res, res)
	}
	if m["id"] != id {
		t.Errorf("dxGetLockedUtxos id = %v, want %v", m["id"], id)
	}
	if _, ok := m["BTC"].([]string); !ok {
		t.Errorf("dxGetLockedUtxos missing BTC key: %v", m)
	}

	// Flush (empty, then with a recorded cancel).
	res, err = ctx.dxFlushCancelledOrders(nil)
	if err != nil {
		t.Fatalf("dxFlushCancelledOrders: %v", err)
	}
	m, ok = res.(map[string]interface{})
	if !ok || len(m["flushedOrders"].([]map[string]interface{})) != 0 {
		t.Fatalf("dxFlushCancelledOrders (empty) = %v", res)
	}
	ctx.Store.RecordCancelled(hexEncode(o.ID[:]), 5)
	res, _ = ctx.dxFlushCancelledOrders(nil)
	m, _ = res.(map[string]interface{})
	if len(m["flushedOrders"].([]map[string]interface{})) != 1 {
		t.Fatalf("dxFlushCancelledOrders (recorded) = %v", res)
	}
}

func TestDxEmptyHistoryTrading(t *testing.T) {
	ctx := newWalletTestCtx()
	// dxGetOrderHistory: a valid range with no fills still emits one zero-filled
	// bucket per granularity slice (C++ emits N zero-filled OHLCV slices, not []).
	res, err := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jstr("0"), jstr("180"), jstr("60"),
	})
	if err != nil {
		t.Fatalf("dxGetOrderHistory: %v", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 3 {
		t.Fatalf("dxGetOrderHistory = %v (%T), want 3 zero-filled buckets", res, res)
	}
	for _, b := range arr {
		row, ok := b.([]interface{})
		if !ok || len(row) != 6 {
			t.Fatalf("bucket = %v (%T), want 6 fields", b, b)
		}
		for _, v := range row[1:6] {
			if f, ok := v.(float64); !ok || f != 0 {
				t.Errorf("bucket field = %v, want 0", v)
			}
		}
	}
	// end <= start -> no buckets -> [].
	endStart, e2 := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jstr("100"), jstr("0"), jstr("60"),
	})
	if e2 == nil {
		if arr, ok := endStart.([]interface{}); !ok || len(arr) != 0 {
			t.Errorf("dxGetOrderHistory(end<=start) = %v (%T), want []", endStart, endStart)
		}
	}
	// Too few params -> error.
	if _, err := ctx.dxGetOrderHistory(nil); err == nil {
		t.Error("dxGetOrderHistory() should error")
	}

	// dxGetTradingData: no fills -> empty array.
	res, err = ctx.dxGetTradingData(nil)
	if err != nil {
		t.Fatalf("dxGetTradingData: %v", err)
	}
	if arr, ok := res.([]interface{}); !ok || len(arr) != 0 {
		t.Fatalf("dxGetTradingData = %v (%T)", res, res)
	}
}

// TestDxGetOrderHistoryBuckets verifies the OHLCV aggregation over local fills:
// open/high/low/close from the taker/maker price ratio, volume = sum of taker
// size, zero-filled empty slices, and the trailing order-id array when
// order_ids=true.
func TestDxGetOrderHistoryBuckets(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Store.AddFill(fillEntry{ID: "aaa", Time: 1030 * 1e6, Maker: "BTC", Taker: "LTC", MakerSize: "1.0", TakerSize: "2.0"})
	ctx.Store.AddFill(fillEntry{ID: "bbb", Time: 1040 * 1e6, Maker: "BTC", Taker: "LTC", MakerSize: "1.0", TakerSize: "4.0"})
	ctx.Store.AddFill(fillEntry{ID: "ccc", Time: 1090 * 1e6, Maker: "BTC", Taker: "LTC", MakerSize: "2.0", TakerSize: "2.0"})
	// A fill outside the pair (should be ignored).
	ctx.Store.AddFill(fillEntry{ID: "zzz", Time: 1035 * 1e6, Maker: "SYS", Taker: "LTC", MakerSize: "1.0", TakerSize: "1.0"})

	// range [1000,1180) granularity 60 -> 3 buckets; bucket0 has 2 fills,
	// bucket1 has 1, bucket2 empty.
	res, err := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jstr("1000"), jstr("1180"), jstr("60"),
		json.RawMessage("true"), jstr("false"),
	})
	if err != nil {
		t.Fatalf("dxGetOrderHistory: %v", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 3 {
		t.Fatalf("dxGetOrderHistory = %v (%T), want 3 buckets", res, res)
	}
	// bucket0: open=2.0, high=4.0, low=2.0, close=4.0, volume=6.0
	b0 := arr[0].([]interface{})
	if b0[3].(float64) != 2.0 || b0[2].(float64) != 4.0 || b0[1].(float64) != 2.0 || b0[4].(float64) != 4.0 || b0[5].(float64) != 6.0 {
		t.Errorf("bucket0 = %v, want [_,2,4,2,4,6]", b0)
	}
	// bucket0 trailing order-id array.
	ids0 := b0[6].([]string)
	if len(ids0) != 2 || ids0[0] != "aaa" || ids0[1] != "bbb" {
		t.Errorf("bucket0 ids = %v, want [aaa,bbb]", ids0)
	}
	// bucket1: price = 2.0/2.0 = 1.0 (open=high=low=close), volume=2.0
	b1 := arr[1].([]interface{})
	if b1[1].(float64) != 1.0 || b1[2].(float64) != 1.0 || b1[3].(float64) != 1.0 || b1[4].(float64) != 1.0 || b1[5].(float64) != 2.0 {
		t.Errorf("bucket1 = %v, want [_,1,1,1,1,2]", b1)
	}
	// bucket2: zero-filled.
	b2 := arr[2].([]interface{})
	for _, v := range b2[1:6] {
		if v.(float64) != 0 {
			t.Errorf("bucket2 field = %v, want 0", v)
		}
	}
}

// TestDxGetTradingDataFills verifies the 8-field C++ record schema is emitted
// per local fill, with fee_txid/nodepubkey empty (on-chain BLOCK data is
// unavailable to the thin client).
func TestDxGetTradingDataFills(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Store.AddFill(fillEntry{ID: "fff", Time: 1234 * 1e6, Maker: "BTC", Taker: "LTC", MakerSize: "1.5", TakerSize: "0.5"})
	res, err := ctx.dxGetTradingData(nil)
	if err != nil {
		t.Fatalf("dxGetTradingData: %v", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 1 {
		t.Fatalf("dxGetTradingData = %v (%T)", res, res)
	}
	rec, ok := arr[0].(map[string]interface{})
	if !ok {
		t.Fatalf("record = %v (%T)", arr[0], arr[0])
	}
	if rec["timestamp"] != int64(1234) {
		t.Errorf("timestamp = %v, want 1234", rec["timestamp"])
	}
	if rec["id"] != "fff" || rec["maker"] != "BTC" || rec["taker"] != "LTC" {
		t.Errorf("id/maker/taker = %v/%v/%v", rec["id"], rec["maker"], rec["taker"])
	}
	if rec["maker_size"] != 1.5 || rec["taker_size"] != 0.5 {
		t.Errorf("maker_size/taker_size = %v/%v, want 1.5/0.5", rec["maker_size"], rec["taker_size"])
	}
	if rec["fee_txid"] != "" || rec["nodepubkey"] != "" {
		t.Errorf("fee_txid/nodepubkey = %v/%v, want empty", rec["fee_txid"], rec["nodepubkey"])
	}
}

func TestDxPartialChainRead(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)

	res, err := ctx.dxGetMyPartialOrderChain([]json.RawMessage{jstr(hexEncode(o.ID[:]))})
	if err != nil {
		t.Fatalf("dxGetMyPartialOrderChain: %v", err)
	}
	if arr, ok := res.([]orderDetailResult); !ok || len(arr) != 1 {
		t.Fatalf("dxGetMyPartialOrderChain = %v (%T)", res, res)
	}

	res, err = ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(hexEncode(o.ID[:]))})
	if err != nil {
		t.Fatalf("dxPartialOrderChainDetails: %v", err)
	}
	if m, ok := res.(map[string]interface{}); !ok || m["first_order_id"] == nil {
		t.Fatalf("dxPartialOrderChainDetails = %v (%T)", res, res)
	}

	// Missing id -> error on both.
	if _, err := ctx.dxGetMyPartialOrderChain([]json.RawMessage{jstr("nope")}); err == nil {
		t.Error("dxGetMyPartialOrderChain(missing) should error")
	}
	if _, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr("nope")}); err == nil {
		t.Error("dxPartialOrderChainDetails(missing) should error")
	}
}

// TestDxPartialOrderChainDetailsAggregate locks in C++ dxPartialOrderChainDetails'
// aggregate schema: a full chain (ancestors + self + descendants) rooted at the
// oldest order, with per-status sent/received/notsent/notreceived totals and the
// orders field rendered as a hex-id array (not order objects).
func TestDxPartialOrderChainDetailsAggregate(t *testing.T) {
	ctx := newWalletTestCtx()
	parent := &Order{ID: [32]byte{0xaa}, FromCurrency: "BTC", ToCurrency: "BTC",
		FromAmount: 100, ToAmount: 50, Status: "finished", Created: 10, Updated: 10}
	mid := seedOrder(ctx) // ID {0x01}, BTC/BTC, open
	mid.ParentID = [32]byte{0xaa}
	child := &Order{ID: [32]byte{0xbb}, FromCurrency: "BTC", ToCurrency: "BTC",
		FromAmount: 200, ToAmount: 100, Status: "canceled", Created: 30, Updated: 30}
	child.ParentID = [32]byte{0x01}
	ctx.Store.Add(parent)
	ctx.Store.Add(child)

	res, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(hexEncode(mid.ID[:]))})
	if err != nil {
		t.Fatalf("dxPartialOrderChainDetails: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("result = %v (%T)", res, res)
	}
	if m["first_order_id"] != hexEncode(parent.ID[:]) {
		t.Errorf("first_order_id = %v, want %v", m["first_order_id"], hexEncode(parent.ID[:]))
	}
	orders, ok := m["orders"].([]string)
	if !ok || len(orders) != 3 {
		t.Fatalf("orders = %v (%T), want 3 hex ids", m["orders"], m["orders"])
	}
	if orders[0] != hexEncode(parent.ID[:]) || orders[2] != hexEncode(child.ID[:]) {
		t.Errorf("orders not root-first: %v", orders)
	}
	if m["total_orders_open"] != 1 || m["total_orders_finished"] != 1 || m["total_orders_canceled"] != 1 {
		t.Errorf("counts wrong: open=%v finished=%v canceled=%v",
			m["total_orders_open"], m["total_orders_finished"], m["total_orders_canceled"])
	}
	if m["total_reported_sent"] != formatXAmount(100) {
		t.Errorf("total_reported_sent = %v, want %v", m["total_reported_sent"], formatXAmount(100))
	}
	if m["total_reported_notsent"] != formatXAmount(1500000+200) {
		t.Errorf("total_reported_notsent = %v, want %v", m["total_reported_notsent"], formatXAmount(1500200))
	}
	if p, _ := m["p2sh_deposits"].([]string); len(p) != 0 {
		t.Errorf("p2sh_deposits should be empty, got %v", p)
	}
}

func TestDxLoadConfAndTokens(t *testing.T) {
	ctx := newWalletTestCtx()
	if res, err := ctx.dxLoadXBridgeConf(nil); err != nil || res != true {
		t.Fatalf("dxLoadXBridgeConf = %v %v", res, err)
	}
	lt, _ := ctx.dxGetLocalTokens(nil)
	if ls, _ := lt.([]string); len(ls) != 1 || ls[0] != "BTC" {
		t.Errorf("dxGetLocalTokens = %v", lt)
	}
	nt, _ := ctx.dxGetNetworkTokens(nil)
	if ns, _ := nt.([]string); len(ns) != 1 || ns[0] != "BTC" {
		t.Errorf("dxGetNetworkTokens = %v", nt)
	}
}

func TestDxWriteCommandsNoSession(t *testing.T) {
	ctx := newWalletTestCtx() // Node has no live conn.
	// dxMakeOrder: 7 params -> requireWrite -> errNoSession.
	if _, err := ctx.dxMakeOrder([]json.RawMessage{
		jstr("BTC"), jstr("1"), jstr("mk"),
		jstr("LTC"), jstr("2"), jstr("tk"),
		jstr("exact"),
	}); err == nil {
		t.Error("dxMakeOrder(no conn) should error")
	}
	// dxTakeOrder: 3 params -> errNoSession.
	if _, err := ctx.dxTakeOrder([]json.RawMessage{
		jstr("id"), jstr("a"), jstr("b"),
	}); err == nil {
		t.Error("dxTakeOrder(no conn) should error")
	}
	// dxCancelOrder: 1 param -> errNoSession.
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr("id")}); err == nil {
		t.Error("dxCancelOrder(no conn) should error")
	}
}

// TestDxCancelOrderGuards locks in C++ dxCancelOrder's pre-broadcast guards:
// invalid id format -> INVALID_PARAMETERS, unknown id -> TRANSACTION_NOT_FOUND,
// and an in-process (state >= trCreated) order -> INVALID_STATE.
func TestDxCancelOrderGuards(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx) // BTC/BTC, status "open"

	// Unknown but well-formed id -> TRANSACTION_NOT_FOUND.
	var missingID [32]byte
	missingID[0] = 0x55
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr(hexEncode(missingID[:]))}); err == nil || err.Code != errTxNotFound {
		t.Errorf("dxCancelOrder(unknown) = %+v, want TRANSACTION_NOT_FOUND", err)
	}
	// Malformed id -> INVALID_PARAMETERS.
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr("not-a-hex")}); err == nil || err.Code != errInvalidParameters {
		t.Errorf("dxCancelOrder(bad id) = %+v, want INVALID_PARAMETERS", err)
	}
	// In-process order (created) cannot be cancelled.
	o.Status = "created"
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr(hexEncode(o.ID[:]))}); err == nil || err.Code != errInvalidState {
		t.Errorf("dxCancelOrder(created) = %+v, want INVALID_STATE", err)
	}
}

// TestDxSplitInputsBadBoolParam verifies a malformed (non-boolean) flag errors
// out via errInvalidParameters instead of silently defaulting to true (Module F).
func TestDxSplitInputsBadBoolParam(t *testing.T) {
	ctx := newWalletTestCtx()
	// C++ requires exactly 7 params: ticker, splitamount, address, include_fees,
	// show_rawtx, submit, utxos. include_fees=123 is not a boolean -> must error.
	params := []json.RawMessage{
		jstr("BTC"), jstr("1000000"), jstr(btcAddr),
		json.RawMessage("123"), // not a boolean -> must error
		json.RawMessage("false"),
		json.RawMessage("true"),
		json.RawMessage(`[{"txid":"0000000000000000000000000000000000000000000000000000000000000000","vout":0,"amount":"1","scriptPubKey":"76a914000000000000000000000000000000000000000088ac","address":"` + btcAddr + `"}]`),
	}
	res, err := ctx.dxSplitInputs(params)
	if err == nil {
		t.Fatalf("expected errInvalidParameters for non-boolean include_fees, got res=%v", res)
	}
	if err.Code != errInvalidParameters {
		t.Errorf("err.Code = %d, want %d", err.Code, errInvalidParameters)
	}
}

func TestGetNetworkInfo(t *testing.T) {
	// Defaults when Config leaves the version blank (fall back to 4.4.1).
	ctx := &HandlerCtx{Store: NewStore(), Node: &Node{}, Config: &Config{}}
	res, err := ctx.getNetworkInfo(nil)
	if err != nil {
		t.Fatalf("getNetworkInfo: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("getNetworkInfo result = %T", res)
	}
	if m["version"] != 4040100 {
		t.Errorf("version = %v, want 4040100", m["version"])
	}
	if m["subversion"] != "/blocknet:4.4.1/" {
		t.Errorf("subversion = %v, want /blocknet:4.4.1/", m["subversion"])
	}
	if m["connections"] != 0 {
		t.Errorf("connections = %v, want 0 (no live conn)", m["connections"])
	}
	// Rejects params.
	if _, err := ctx.getNetworkInfo([]json.RawMessage{jstr("x")}); err == nil {
		t.Error("getNetworkInfo should reject params")
	}

	// Explicit Config overrides the defaults.
	ctx2 := &HandlerCtx{Store: NewStore(), Node: &Node{}, Config: &Config{WalletVersion: 4120000, WalletVersionStr: "/blocknet:4.12.0/"}}
	res2, _ := ctx2.getNetworkInfo(nil)
	m2 := res2.(map[string]interface{})
	if m2["version"] != 4120000 || m2["subversion"] != "/blocknet:4.12.0/" {
		t.Errorf("override = %v / %v", m2["version"], m2["subversion"])
	}
}
