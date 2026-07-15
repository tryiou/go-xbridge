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
	if arr[0].Maker != "BTC" || arr[0].MakerSize != "1.5000000" {
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
	// detail=0, maker=BTC, taker=BTC -> order lands in asks.
	res, err := ctx.dxGetOrderBook([]json.RawMessage{
		jstr("0"), jstr("BTC"), jstr("BTC"),
	})
	if err != nil {
		t.Fatalf("dxGetOrderBook: %v", err)
	}
	ob, ok := res.(orderBookResult)
	if !ok {
		t.Fatalf("dxGetOrderBook = %v (%T)", res, res)
	}
	if len(ob.Asks) != 1 || len(ob.Bids) != 0 {
		t.Errorf("dxGetOrderBook asks=%d bids=%d", len(ob.Asks), len(ob.Bids))
	}
	if ob.Maker != "BTC" || ob.Taker != "BTC" {
		t.Errorf("dxGetOrderBook maker/taker = %q/%q", ob.Maker, ob.Taker)
	}
	// detail=0 entry shape: [price, size, count].
	if len(ob.Asks) == 1 {
		if e := ob.Asks[0]; len(e) != 3 {
			t.Errorf("detail=0 ask entry = %v", ob.Asks[0])
		}
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

	// Nothing locked yet.
	res, err := ctx.dxGetLockedUtxos(nil)
	if err != nil || len(res.([]orderDetailResult)) != 0 {
		t.Fatalf("dxGetLockedUtxos (empty) = %v %v", res, err)
	}
	ctx.Store.Lock(o)
	res, _ = ctx.dxGetLockedUtxos(nil)
	if len(res.([]orderDetailResult)) != 1 {
		t.Fatalf("dxGetLockedUtxos (locked) = %v", res)
	}

	// Flush (empty, then with a recorded cancel).
	res, err = ctx.dxFlushCancelledOrders(nil)
	if err != nil {
		t.Fatalf("dxFlushCancelledOrders: %v", err)
	}
	m, ok := res.(map[string]interface{})
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
	// dxGetOrderHistory: needs 5-8 params, returns empty array (thin client).
	res, err := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jstr("0"), jstr("100"), jstr("60"),
	})
	if err != nil {
		t.Fatalf("dxGetOrderHistory: %v", err)
	}
	if arr, ok := res.([]interface{}); !ok || len(arr) != 0 {
		t.Fatalf("dxGetOrderHistory = %v (%T)", res, res)
	}
	// Too few params -> error.
	if _, err := ctx.dxGetOrderHistory(nil); err == nil {
		t.Error("dxGetOrderHistory() should error")
	}

	// dxGetTradingData: returns empty array.
	res, err = ctx.dxGetTradingData(nil)
	if err != nil {
		t.Fatalf("dxGetTradingData: %v", err)
	}
	if arr, ok := res.([]interface{}); !ok || len(arr) != 0 {
		t.Fatalf("dxGetTradingData = %v (%T)", res, res)
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
