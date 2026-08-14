package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// jstr wraps a Go string as a JSON-RawMessage param (a quoted
// JSON string), matching how api/handlers.go expects positional
// string params.
func jstr(s string) json.RawMessage {
	return json.RawMessage([]byte(`"` + s + `"`))
}

// mustJSONMap marshals a handler result and unmarshals it into a string-keyed
// map so tests can inspect JSON field values regardless of the concrete result
// type (ordered structs marshal the same wire shape as the maps they replaced).
func mustJSONMap(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	m := map[string]interface{}{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal result %s: %v", b, err)
	}
	return m
}

// jnum wraps a Go int64 as a JSON-RawMessage number param. C++ reads int params
// via get_int()/get_int64(), which throw on a JSON string, so tests must pass
// real JSON numbers (strict parsing, RPC-F01).
func jnum(n int64) json.RawMessage {
	return json.RawMessage([]byte(strconv.FormatInt(n, 10)))
}

// dispID renders a raw order id the way the dx* RPCs display it (C++
// uint256::GetHex order). Tests use it for RPC inputs and to assert echoed ids.
func dispID(id [32]byte) string { return orderIDString(id) }

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

// seedOrderWithUtxo seeds the standard BTC/BTC open made order with ONE
// reserved utxo, populated BEFORE Store.Add to honor the store's ownership
// contract ("must not be mutated afterward", store.go:116-119).
func seedOrderWithUtxo(ctx *HandlerCtx, txid [32]byte) *Order {
	o := &Order{
		ID:           [32]byte{0x01},
		Type:         OrderTypeMaker,
		FromCurrency: "BTC",
		FromAmount:   1500000,
		ToCurrency:   "BTC",
		ToAmount:     300000,
		Created:      1,
		Updated:      1,
		Status:       "open",
		Mine:         true,
		MakerAddress: "mk",
		TakerAddress: "tk",
		Utxos:        []proto.UtxoEntry{{TxID: txid, Vout: 0}},
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

	// dxGetOrders rejects params (arity gate moved to checkArity, dispatch.go).
	if rerr := checkArity("dxGetOrders", 1); rerr == nil || rerr.Code != errInvalidParameters {
		t.Errorf("checkArity(dxGetOrders, 1) = %v, want business 1025", rerr)
	}
}

// TestDxGetOrdersShowAllGate locks in C++'s dxGetOrders wallet filter
// (rpcxbridge.cpp:432-446): an order whose currencies have no wallet connector
// is hidden unless showAllOrders (-dxnowallets) is set. The connector lookup is
// a plain map check — a coin present in the conf but without a live connector
// still counts as "no wallet".
func TestDxGetOrdersShowAllGate(t *testing.T) {
	ctx := newWalletTestCtx()
	o := &Order{
		ID:           [32]byte{0x09},
		FromCurrency: "NOPE",
		FromAmount:   1000000,
		ToCurrency:   "NOPE",
		ToAmount:     200000,
		Status:       "open",
	}
	ctx.Store.Add(o)

	// Hidden by default: no NOPE connector.
	res, err := ctx.dxGetOrders(nil)
	if err != nil {
		t.Fatalf("dxGetOrders: %v", err)
	}
	if arr, ok := res.([]orderListResult); !ok || len(arr) != 0 {
		t.Fatalf("dxGetOrders (no connector) = %v (%T), want empty", res, res)
	}

	// Shown with ShowAllOrders.
	ctx.Node.config.ShowAllOrders = true
	res, err = ctx.dxGetOrders(nil)
	if err != nil {
		t.Fatalf("dxGetOrders: %v", err)
	}
	if arr, ok := res.([]orderListResult); !ok || len(arr) != 1 {
		t.Fatalf("dxGetOrders (ShowAllOrders) = %v (%T), want 1", res, res)
	}
}

func TestDxGetOrderRead(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)

	res, err := ctx.dxGetOrder([]json.RawMessage{jstr(dispID(o.ID))})
	if err != nil {
		t.Fatalf("dxGetOrder: %v", err)
	}
	if r, ok := res.(orderListResult); !ok || r.ID != dispID(o.ID) {
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

// TestDxGetMyOrdersDedupAndSort locks in the RPC-F26 behaviors: an order
// present in BOTH the live book and history renders once (C++ seen[] dedup,
// rpcxbridge.cpp:2133-2138), and rows are ordered ascending by the raw
// microsecond updated time, not the ISO-rendered millisecond string.
func TestDxGetMyOrdersDedupAndSort(t *testing.T) {
	ctx := newWalletTestCtx()
	oldest := seedOrder(ctx) // ID {0x01}, Mine, Updated: 1
	oldest.Updated = 1000

	dup := &Order{ID: oldest.ID, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "finished", Mine: true,
		Created: 1, Updated: 1000}
	ctx.Store.AddToHistory(dup, "finished", 0, 1000)

	mid := &Order{ID: [32]byte{0x02}, FromCurrency: "BTC", FromAmount: 200,
		ToCurrency: "BTC", ToAmount: 100, Status: "canceled", Mine: true, Created: 2, Updated: 1500}
	ctx.Store.AddToHistory(mid, "canceled", 1, 1500)

	latest := &Order{ID: [32]byte{0x03}, FromCurrency: "BTC", FromAmount: 300,
		ToCurrency: "BTC", ToAmount: 150, Status: "open", Mine: true, Created: 3, Updated: 500}
	ctx.Store.Add(latest)

	res, err := ctx.dxGetMyOrders(nil)
	if err != nil {
		t.Fatalf("dxGetMyOrders: %v", err)
	}
	arr := res.([]orderDetailResult)
	if len(arr) != 3 {
		t.Fatalf("dxGetMyOrders = %d rows, want 3 (dup deduped): %+v", len(arr), arr)
	}
	// Ascending by Updated: latest(500) < oldest(1000) < mid(1500).
	if arr[0].ID != dispID(latest.ID) || arr[1].ID != dispID(oldest.ID) || arr[2].ID != dispID(mid.ID) {
		t.Fatalf("rows not sorted by µs updated: %+v", arr)
	}
}

// TestDxGetMyOrdersFieldOrder locks in the RPC-F26 fix: maker_address /
// taker_address are emitted at JSON positions 3/6 (rpcxbridge.cpp:2151-2171),
// not appended after orderBase.
func TestDxGetMyOrdersFieldOrder(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	o.MakerAddress, o.TakerAddress = "mk1", "tk1"
	res, err := ctx.dxGetMyOrders(nil)
	if err != nil {
		t.Fatalf("dxGetMyOrders: %v", err)
	}
	b, _ := json.Marshal(res)
	want := []string{
		"id", "maker", "maker_size", "maker_address", "taker", "taker_size",
		"taker_address", "updated_at", "created_at", "order_type",
		"partial_minimum", "partial_orig_maker_size", "partial_orig_taker_size",
		"partial_repost", "partial_parent_id", "status",
	}
	if got := firstObjectKeys(b); !reflect.DeepEqual(got, want) {
		t.Fatalf("dxGetMyOrders key order = %v, want %v (C++ 2151-2171)", got, want)
	}
}

// TestDxPartialOrderChainDetailsKeyOrder locks in the RPC-F30 fix: the details
// object emits keys in C++ pushKV insertion order (rpcxbridge.cpp:2460-2480),
// not Go-map alphabetical order.
func TestDxPartialOrderChainDetailsKeyOrder(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	o.PartialAllowed = true
	res, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(dispID(o.ID))})
	if err != nil {
		t.Fatalf("dxPartialOrderChainDetails: %v", err)
	}
	b, _ := json.Marshal(res)
	want := []string{
		"first_order_id", "maker", "maker_address", "taker", "taker_address",
		"partial_minimum", "partial_orig_maker_size", "partial_orig_taker_size",
		"first_order_time", "last_order_time", "total_reported_sent",
		"total_reported_received", "total_reported_notsent",
		"total_reported_notreceived", "total_orders_open", "total_orders_finished",
		"total_orders_canceled", "orders", "p2sh_deposits", "p2sh_deposits_counterparty",
	}
	if got := firstObjectKeys(b); !reflect.DeepEqual(got, want) {
		t.Fatalf("details key order = %v, want %v (C++ 2460-2480)", got, want)
	}
}

// TestDxPartialOrderChainDetailsBadId locks in the RPC-F29 fix: the malformed /
// null id error text is C++'s "bad order id" (rpcxbridge.cpp:2414), not
// "Invalid order id [<id>]".
func TestDxPartialOrderChainDetailsBadId(t *testing.T) {
	ctx := newWalletTestCtx()
	_, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(strings.Repeat("0", 64))})
	if err == nil || err.Code != errInvalidParameters {
		t.Fatalf("dxPartialOrderChainDetails(null id) = %v, want INVALID_PARAMETERS", err)
	}
	if !strings.Contains(err.Error, "bad order id") {
		t.Fatalf("error text = %q, want it to contain %q", err.Error, "bad order id")
	}
}

// firstObjectKeys returns the JSON key order of the FIRST object in b, for flat
// objects (no nested braces), matching how encoding/json serializes ordered
// structs. Used to lock C++ insertion order byte-for-byte.
func firstObjectKeys(b []byte) []string {
	s := string(b)
	start := strings.IndexByte(s, '{')
	end := strings.IndexByte(s[start+1:], '}')
	obj := s[start : start+1+end]
	re := regexp.MustCompile(`"([a-zA-Z_0-9]+)":`)
	var keys []string
	for _, m := range re.FindAllStringSubmatch(obj, -1) {
		keys = append(keys, m[1])
	}
	return keys
}

func TestDxGetOrderBookEmptyPairEmitsArrays(t *testing.T) {
	// An empty trading pair must serialize asks/bids as "[]" (not "null"),
	// matching C++ dxGetOrderBook which emits default-constructed Array
	// objects for an empty book (rpcxbridge.cpp:1568-1576).
	ctx := newWalletTestCtx()
	res, err := ctx.dxGetOrderBook([]json.RawMessage{
		jnum(1), jstr("BTC"), jstr("LTC"),
	})
	if err != nil {
		t.Fatalf("dxGetOrderBook: %v", err)
	}
	ob, ok := res.(orderBookResult)
	if !ok {
		t.Fatalf("dxGetOrderBook = %v (%T)", res, res)
	}
	if ob.Asks == nil {
		t.Fatal("asks must be non-nil (serialize as [])")
	}
	if ob.Bids == nil {
		t.Fatal("bids must be non-nil (serialize as [])")
	}
	b, merr := json.Marshal(ob)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	if !strings.Contains(string(b), `"asks":[]`) || !strings.Contains(string(b), `"bids":[]`) {
		t.Fatalf("empty book JSON = %s, want asks/bids as []", string(b))
	}
}

func TestDxGetOrderBookRead(t *testing.T) {
	ctx := newWalletTestCtx()
	seedOrder(ctx)
	// detail=1, maker=BTC, taker=BTC -> the BTC/BTC order (From=BTC,To=BTC)
	// matches both the ask and bid filters, so it lands in both sides.
	res, err := ctx.dxGetOrderBook([]json.RawMessage{
		jnum(1), jstr("BTC"), jstr("BTC"),
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

// TestDxGetOrderBookDetailLevels is a C++-derived known-answer test (KAT) that
// locks in the four detail-level element shapes and ordering emitted by
// dxGetOrderBook, matching rpcxbridge.cpp exactly:
//
//	level 1 (best only):       [price, size, count]            (rpcxbridge.cpp:1697-99)
//	level 2 (aggregated top):  [price, sum,  count]  — asks descending (worst→best),
//	                            emitting ~every-other window entry via C++'s
//	                            `while((++i < bound) && floatCompare(...))` skip
//	level 3 (full, capped):    [price, amount, id]   — asks descending (worst→best)
//	level 4 (best + ids):      [price, amount, [ids]]
//
// This also cements the refuted audit-D1 claim: detail 1 is [price,size,count]
// in C++ too, not a different shape.
func TestDxGetOrderBookDetailLevels(t *testing.T) {
	ctx := newWalletTestCtx()

	// detail out of range -> errInvalidDetailLevel.
	if _, err := ctx.dxGetOrderBook([]json.RawMessage{jnum(0), jstr("BTC"), jstr("BTC")}); err == nil {
		t.Error("detail=0 should error")
	}
	if _, err := ctx.dxGetOrderBook([]json.RawMessage{jnum(5), jstr("BTC"), jstr("BTC")}); err == nil {
		t.Error("detail=5 should error")
	}

	// Seed two asks (BTC->LTC) and two bids (LTC->BTC) at distinct prices so we
	// can assert per-level element counts/shapes.
	seed := func(from, to string, fAmt, tAmt uint64) {
		ctx.Store.Add(&Order{
			ID:           [32]byte{byte(len(ctx.Store.List()) + 1)},
			Type:         OrderTypeMaker,
			FromCurrency: from,
			FromAmount:   fAmt,
			ToCurrency:   to,
			ToAmount:     tAmt,
			Status:       "open",
		})
	}
	seed("BTC", "LTC", 1_500_000, 300_000) // ask price 0.2
	seed("BTC", "LTC", 1_500_000, 150_000) // ask price 0.1
	seed("LTC", "BTC", 300_000, 1_500_000) // bid price 5.0
	seed("LTC", "BTC", 300_000, 3_000_000) // bid price 10.0

	wantLen := func(side [][]interface{}, n int) {
		if len(side) != n {
			t.Fatalf("side len = %d, want %d (%v)", len(side), n, side)
		}
	}

	for lvl := 1; lvl <= 4; lvl++ {
		res, err := ctx.dxGetOrderBook([]json.RawMessage{
			jnum(int64(lvl)), jstr("BTC"), jstr("LTC"),
		})
		if err != nil {
			t.Fatalf("detail %d: %v", lvl, err)
		}
		ob := res.(orderBookResult)
		if ob.Detail != lvl {
			t.Errorf("detail %d: Detail field = %d", lvl, ob.Detail)
		}

		switch lvl {
		case 1:
			// best ask (price 0.1) and best bid (price 10.0), each 1 element.
			wantLen(ob.Asks, 1)
			wantLen(ob.Bids, 1)
			if len(ob.Asks[0]) != 3 {
				t.Errorf("detail1 ask len = %d, want 3", len(ob.Asks[0]))
			}
			if _, ok := ob.Asks[0][2].(int); !ok {
				t.Errorf("detail1 ask[2] (count) not int: %T", ob.Asks[0][2])
			}
		case 2:
			// C++ d2 (rpcxbridge.cpp:1763-1833): asks window starts at index
			// asks_len-bound (0 when bound==len), i.e. the worst ask 0.2, not
			// the best; bids window starts at index 0, the best bid, but
			// priceBid = from/to so the seeded best bid is 0.2. The
			// `while((++i < bound) && floatCompare(...))` advances i past the
			// differing price without merging, so each side emits exactly ONE
			// row: [0.200000, ...] for both asks and bids.
			wantLen(ob.Asks, 1)
			wantLen(ob.Bids, 1)
			if len(ob.Asks[0]) != 3 {
				t.Errorf("detail2 ask len = %d, want 3", len(ob.Asks[0]))
			}
			if ob.Asks[0][0] != "0.200000" {
				t.Errorf("detail2 asks[0] price = %v, want 0.200000", ob.Asks[0][0])
			}
			if ob.Bids[0][0] != "0.200000" {
				t.Errorf("detail2 bids[0] price = %v, want 0.200000", ob.Bids[0][0])
			}
		case 3:
			wantLen(ob.Asks, 2)
			wantLen(ob.Bids, 2)
			if len(ob.Asks[0]) != 3 {
				t.Errorf("detail3 ask len = %d, want 3", len(ob.Asks[0]))
			}
			if _, ok := ob.Asks[0][2].(string); !ok {
				t.Errorf("detail3 ask[2] (id) not string: %T", ob.Asks[0][2])
			}
			// C++ returns asks descending (worst→best), so the first
			// ask must be the worse (higher) price.
			if ob.Asks[0][0] != "0.200000" {
				t.Errorf("detail3 asks[0] price = %v, want 0.200000", ob.Asks[0][0])
			}
			if ob.Asks[1][0] != "0.100000" {
				t.Errorf("detail3 asks[1] price = %v, want 0.100000", ob.Asks[1][0])
			}
		case 4:
			wantLen(ob.Asks, 1)
			wantLen(ob.Bids, 1)
			if len(ob.Asks[0]) != 3 {
				t.Errorf("detail4 ask len = %d, want 3", len(ob.Asks[0]))
			}
			ids, ok := ob.Asks[0][2].([]string)
			if !ok {
				t.Errorf("detail4 ask[2] (ids) not []string: %T", ob.Asks[0][2])
			} else if len(ids) != 1 {
				t.Errorf("detail4 ask ids len = %d, want 1", len(ids))
			}
		}
	}
}

// TestDxGetOrderBookD2WindowAndMerge locks in C++ detail-2 behavior when the
// order counts exceed maxOrders (rpcxbridge.cpp:1763-1833):
//   - the asks window is [asks_len-bound, asks_len) (worst asks first),
//   - equal-priced window entries merge into one [price, sum, count] row,
//   - the `while((++i < bound) && floatCompare(...))` skip makes the iteration
//     every-other: a window price that differs from the emitted row's price is
//     consumed by the loop, so its own row is never emitted.
//
// Seeded asks (BTC->LTC, from=1.5M): prices {0.4,0.3,0.2,0.2,0.15,0.1} sorted
// descending; asks window with maxOrders=4 is indices 2..5 = {0.2,0.2,0.15,0.1}:
//
//	index 2 emits [0.200000, 3.000000, 2] (merges index 3, the while-skip
//	consumes index 4's 0.15 row without emitting it), index 5 emits
//	[0.100000, 1.500000, 1].
//
// Seeded bids (LTC->BTC, from=300k): prices {0.2,0.2,0.15,0.1,0.05,0.01} sorted
// descending; bids window is indices 0..3 = {0.2,0.2,0.15,0.1}:
//
//	index 0 emits [0.200000, 3.000000, 2] (merges index 1, the while-skip
//	consumes index 2's 0.15 row), index 3 emits [0.100000, 3.000000, 1].
func TestDxGetOrderBookD2WindowAndMerge(t *testing.T) {
	ctx := newWalletTestCtx()

	seed := func(from, to string, fAmt, tAmt uint64) {
		ctx.Store.Add(&Order{
			ID:           [32]byte{byte(len(ctx.Store.List()) + 1)},
			Type:         OrderTypeMaker,
			FromCurrency: from,
			FromAmount:   fAmt,
			ToCurrency:   to,
			ToAmount:     tAmt,
			Status:       "open",
		})
	}
	// Asks BTC->LTC (price = to/from = to/1.5M).
	seed("BTC", "LTC", 1_500_000, 600_000) // 0.4
	seed("BTC", "LTC", 1_500_000, 450_000) // 0.3
	seed("BTC", "LTC", 1_500_000, 300_000) // 0.2
	seed("BTC", "LTC", 1_500_000, 300_000) // 0.2 (merge)
	seed("BTC", "LTC", 1_500_000, 225_000) // 0.15 (skipped by while)
	seed("BTC", "LTC", 1_500_000, 150_000) // 0.1
	// Bids LTC->BTC (price = to/from = to/300k).
	seed("LTC", "BTC", 300_000, 1_500_000)  // 0.2
	seed("LTC", "BTC", 300_000, 1_500_000)  // 0.2 (merge)
	seed("LTC", "BTC", 300_000, 2_000_000)  // 0.15 (skipped by while)
	seed("LTC", "BTC", 300_000, 3_000_000)  // 0.1
	seed("LTC", "BTC", 300_000, 6_000_000)  // 0.05
	seed("LTC", "BTC", 300_000, 30_000_000) // 0.01

	res, err := ctx.dxGetOrderBook([]json.RawMessage{
		jnum(2), jstr("BTC"), jstr("LTC"), jnum(4),
	})
	if err != nil {
		t.Fatalf("dxGetOrderBook detail 2: %v", err)
	}
	ob := res.(orderBookResult)

	wantRows := func(side string, got [][]interface{}, want [][3]interface{}) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("detail2 %s len = %d, want %d (%v)", side, len(got), len(want), got)
		}
		for i, w := range want {
			if len(got[i]) != 3 {
				t.Fatalf("detail2 %s[%d] len = %d, want 3", side, i, len(got[i]))
			}
			for j := 0; j < 3; j++ {
				if got[i][j] != w[j] {
					t.Errorf("detail2 %s[%d][%d] = %v, want %v", side, i, j, got[i][j], w[j])
				}
			}
		}
	}

	wantRows("asks", ob.Asks, [][3]interface{}{
		{"0.200000", "3.000000", 2},
		{"0.100000", "1.500000", 1},
	})
	wantRows("bids", ob.Bids, [][3]interface{}{
		{"0.200000", "3.000000", 2},
		{"0.100000", "3.000000", 1},
	})
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

	// id -> object keyed by id and the order's currency key. RPC-F32: an order
	// with nothing reserved errors 1021, so the stub BTC utxo is reserved
	// (replaces the utxo-less seedOrder {0x01} record, same id).
	o = seedOrderWithUtxo(ctx, [32]byte{})
	id := dispID(o.ID)
	res, _ = ctx.dxGetLockedUtxos([]json.RawMessage{jstr(id)})
	m, ok = res.(map[string]interface{})
	if !ok {
		t.Fatalf("dxGetLockedUtxos (id) = %v (%T)", res, res)
	}
	if m["id"] != id {
		t.Errorf("dxGetLockedUtxos id = %v, want %v", m["id"], id)
	}
	// RPC-F33: a made (Mine) order lives in the accepted map from creation
	// (xbridgeapp.cpp:2034), so even an open order keys by maker_and_taker.
	if _, ok := m["BTC_and_BTC"].([]string); !ok {
		t.Errorf("dxGetLockedUtxos missing BTC_and_BTC key: %v", m)
	}

	// Flush (empty: the open made order is not cancelled).
	res, err = ctx.dxFlushCancelledOrders(nil)
	if err != nil {
		t.Fatalf("dxFlushCancelledOrders: %v", err)
	}
	m = mustJSONMap(t, res)
	if fo, _ := m["flushedOrders"].([]interface{}); len(fo) != 0 {
		t.Fatalf("dxFlushCancelledOrders (empty) = %v", res)
	}
	// RPC-F35: a cancelled order is pruned from the live book on the next
	// flush (C++ erases trCancelled from m_transactions, xbridgeapp.cpp:1336).
	ctx.Store.Update(hexEncode(o.ID[:]), func(ord *Order) {
		ord.Status = "canceled"
		ord.Updated = 1 // ancient -> flushed by age 0
	})
	res, _ = ctx.dxFlushCancelledOrders(nil)
	m = mustJSONMap(t, res)
	if fo, _ := m["flushedOrders"].([]interface{}); len(fo) != 1 {
		t.Fatalf("dxFlushCancelledOrders (cancelled) = %v", res)
	}
}

// TestFlushCancelledPrunesBookAndHistory locks in RPC-F35 (C++ erases
// trCancelled orders from BOTH m_transactions and m_historicTransactions,
// xbridgeapp.cpp:1331-1354) and RPC-F36 ordering/key-order: the flushed list is
// the live-book block (uint256 id order) followed by the history block, and the
// result object emits ageMillis, now, durationMicrosec, flushedOrders.
func TestFlushCancelledPrunesBookAndHistory(t *testing.T) {
	ctx := newWalletTestCtx()
	// Live id GREATER than the history id: C++ emits the live std::map block
	// first (id-ascending) then the history block (xbridgeapp.cpp:1336), so a
	// global id sort would give [aa, bb] but the port must give [bb, aa].
	live := &Order{ID: [32]byte{0xbb}, FromCurrency: "BTC", FromAmount: 1,
		ToCurrency: "BTC", ToAmount: 1, Status: "canceled", Mine: true, Updated: 1}
	ctx.Store.Add(live)
	hist := &Order{ID: [32]byte{0xaa}, FromCurrency: "BTC", FromAmount: 1,
		ToCurrency: "BTC", ToAmount: 1, Status: "canceled", Mine: true, Updated: 1}
	ctx.Store.AddToHistory(hist, "canceled", 0, 1)

	res, err := ctx.dxFlushCancelledOrders(nil) // age 0 -> flush everything
	if err != nil {
		t.Fatalf("dxFlushCancelledOrders: %v", err)
	}
	m := mustJSONMap(t, res)
	fo, _ := m["flushedOrders"].([]interface{})
	if len(fo) != 2 {
		t.Fatalf("flushedOrders = %v, want live + history entries", m["flushedOrders"])
	}
	// Live block (bb) before history block (aa) — NOT a global id sort.
	if fo[0].(map[string]interface{})["id"] != dispID(live.ID) ||
		fo[1].(map[string]interface{})["id"] != dispID(hist.ID) {
		t.Fatalf("flushed order order = %v, want live(%s) then history(%s)", fo, dispID(live.ID), dispID(hist.ID))
	}
	// Both sources are actually pruned.
	if ctx.Store.Get(hexEncode(live.ID[:])) != nil {
		t.Error("live cancelled order still in the book after flush")
	}
	if h := ctx.Store.History(); len(h) != 0 {
		t.Errorf("cancelled history entry still present after flush: %v", h)
	}
	// RPC-F36 key order (rpcxbridge.cpp:1474-1489).
	b, _ := json.Marshal(res)
	s := string(b)
	want := []string{"ageMillis", "now", "durationMicrosec", "flushedOrders"}
	last := -1
	for _, k := range want {
		p := strings.Index(s, `"`+k+`"`)
		if p < 0 || p < last {
			t.Fatalf("key %q out of C++ order in %s", k, s)
		}
		last = p
	}
}

// TestDxGetLockedUtxosKeyByState verifies dxGetLockedUtxos keys the per-order
// array by the transaction's MAP membership (C++ rpcxbridge.cpp:2674-2677),
// never by which wallets happen to be connected: a made (Mine) order lives in
// the accepted map from creation (xbridgeapp.cpp:2034), so both an open and a
// created order key by maker_and_taker — with the same result whether or not
// the taker wallet is connected.
func TestDxGetLockedUtxosKeyByState(t *testing.T) {
	ctx := newWalletTestCtx()

	add := func(seed byte, status string) (string, *Order) {
		id := [32]byte{seed}
		o := &Order{
			ID: id, Type: OrderTypeMaker, FromCurrency: "BTC", FromAmount: 1500000,
			ToCurrency: "SYS", ToAmount: 300000, Created: 1, Updated: 1,
			Status: status, Mine: true,
			// RPC-F32: an order must have reserved utxos or the handler 1021s.
			// Each order reserves a DISTINCT utxo (byOrder maps one owner per
			// "txid:vout", so sharing the stub utxo would make ownership
			// nondeterministic).
			Utxos: []proto.UtxoEntry{{TxID: [32]byte{seed}, Vout: 0}},
		}
		ctx.Store.Add(o)
		return dispID(id), o
	}

	lockedKey := func(id string) string {
		res, rerr := ctx.dxGetLockedUtxos([]json.RawMessage{jstr(id)})
		if rerr != nil {
			t.Fatalf("dxGetLockedUtxos(%s) = %v", id, rerr)
		}
		m, ok := res.(map[string]interface{})
		if !ok {
			t.Fatalf("dxGetLockedUtxos(%s) = %T", id, res)
		}
		for k := range m {
			if k != "id" {
				return k
			}
		}
		return ""
	}

	// Accepted (created) order with only the maker wallet connected: must still
	// use the dual key (old code fell back to the maker currency here).
	createdID, _ := add(0x11, "created")
	if k := lockedKey(createdID); k != "BTC_and_SYS" {
		t.Fatalf("accepted order (no taker wallet) key = %q, want BTC_and_SYS", k)
	}

	// Now connect the taker wallet: both the open and created MADE orders must
	// keep the dual key (they are in the accepted map, RPC-F33 — the old code
	// single-keyed a made open order on the status ordinal alone).
	ctx.Node.config.Connectors["SYS"] = &stubConn{ticker: "SYS", addr: btcAddr}
	openID, _ := add(0x12, "open")
	if k := lockedKey(openID); k != "BTC_and_SYS" {
		t.Fatalf("open made order key = %q, want BTC_and_SYS", k)
	}
	if k := lockedKey(createdID); k != "BTC_and_SYS" {
		t.Fatalf("accepted order (taker wallet connected) key = %q, want BTC_and_SYS", k)
	}
}

func TestDxEmptyHistoryTrading(t *testing.T) {
	ctx := newWalletTestCtx()
	// dxGetOrderHistory: a valid range with no fills still emits one zero-filled
	// bucket per granularity slice (C++ emits N zero-filled OHLCV slices, not []).
	// start must be >= XSeries earliest (2018-02-25 00:00:00 UTC = 1519516800) per C++ fidelity.
	const xEarly = int64(1519516800)
	res, err := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jnum(xEarly), jnum(xEarly + 180), jnum(60),
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
			if f, ok := v.(xfloat8); !ok || f != 0 {
				t.Errorf("bucket field = %v, want 0", v)
			}
		}
	}
	// start >= end (both after the earliest) -> C++ errors "Start time >= end
	// time." (util/xseries.h:94) — not an empty array.
	if _, err := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jnum(xEarly + 2000), jnum(xEarly + 1000), jnum(60),
	}); err == nil {
		t.Error("dxGetOrderHistory(start>=end) should error")
	} else if err.Code != errInvalidParameters || !strings.Contains(err.Error, "Start time >= end time") {
		t.Errorf("dxGetOrderHistory(start>=end) = %+v, want 1025 'Start time >= end time.'", err)
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
// open/high/low/close from the QUANTIZED taker/maker price ratio, volume = sum
// of the FROM/maker size (RPC-F16), zero-filled empty slices, and the trailing
// order-id array when order_ids=true. The time window is aligned to granularity
// boundaries (matching C++ XSeries behavior), so the effective window may be
// wider than the raw [start, end) range.
func TestDxGetOrderHistoryBuckets(t *testing.T) {
	ctx := newWalletTestCtx()
	const xEarly = int64(1519516800)
	// Fill timestamps are relative to the XSeries earliest (1519516800); they
	// must stay within the queried window [xEarly+1000, xEarly+1180).
	ctx.Store.AddFill(fillEntry{ID: "aaa", Time: uint64(xEarly+1030) * 1e6, Maker: "BTC", Taker: "LTC", MakerSize: "1.0", TakerSize: "2.0"})
	ctx.Store.AddFill(fillEntry{ID: "bbb", Time: uint64(xEarly+1040) * 1e6, Maker: "BTC", Taker: "LTC", MakerSize: "1.0", TakerSize: "4.0"})
	ctx.Store.AddFill(fillEntry{ID: "ccc", Time: uint64(xEarly+1090) * 1e6, Maker: "BTC", Taker: "LTC", MakerSize: "2.0", TakerSize: "2.0"})
	// A fill outside the pair (should be ignored).
	ctx.Store.AddFill(fillEntry{ID: "zzz", Time: uint64(xEarly+1035) * 1e6, Maker: "SYS", Taker: "LTC", MakerSize: "1.0", TakerSize: "1.0"})

	// range [xEarly+1000, xEarly+1180) granularity 60.
	// C++ aligns start down and end up to granularity boundaries:
	//   alignedStart = xEarly+1000 rounded down to 60s = xEarly+960
	//   alignedEnd   = xEarly+1180 rounded up   to 60s = xEarly+1200
	//   numBuckets = (xEarly+1200 - (xEarly+960)) / 60 = 4.
	// bucket0 (xEarly+960..xEarly+1020): empty
	// bucket1 (xEarly+1020..xEarly+1080): aaa (price=2.0), bbb (price=4.0)
	// bucket2 (xEarly+1080..xEarly+1140): ccc (price=1.0)
	// bucket3 (xEarly+1140..xEarly+1200): empty
	res, err := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jnum(xEarly + 1000), jnum(xEarly + 1180), jnum(60),
		json.RawMessage("true"), json.RawMessage("false"),
	})
	if err != nil {
		t.Fatalf("dxGetOrderHistory: %v", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 4 {
		t.Fatalf("dxGetOrderHistory = %v (%T), want 4 buckets", res, res)
	}
	// bucket0: zero-filled.
	b0 := arr[0].([]interface{})
	for _, v := range b0[1:6] {
		if v.(xfloat8) != 0 {
			t.Errorf("bucket0 field = %v, want 0", v)
		}
	}
	// bucket1: open=2.0, high=4.0, low=2.0, close=4.0, volume=2.0 (from/maker
	// side, RPC-F16: 1.0+1.0, NOT the taker-side 6.0).
	b1 := arr[1].([]interface{})
	if b1[3].(xfloat8) != 2.0 || b1[2].(xfloat8) != 4.0 || b1[1].(xfloat8) != 2.0 || b1[4].(xfloat8) != 4.0 || b1[5].(xfloat8) != 2.0 {
		t.Errorf("bucket1 = %v, want [_,2,4,2,4,2]", b1)
	}
	// bucket1 trailing order-id array.
	ids1 := b1[6].([]string)
	if len(ids1) != 2 || ids1[0] != "aaa" || ids1[1] != "bbb" {
		t.Errorf("bucket1 ids = %v, want [aaa,bbb]", ids1)
	}
	// bucket2: price = 2.0/2.0 = 1.0 (open=high=low=close), volume=2.0
	b2 := arr[2].([]interface{})
	if b2[1].(xfloat8) != 1.0 || b2[2].(xfloat8) != 1.0 || b2[3].(xfloat8) != 1.0 || b2[4].(xfloat8) != 1.0 || b2[5].(xfloat8) != 2.0 {
		t.Errorf("bucket2 = %v, want [_,1,1,1,1,2]", b2)
	}
	// bucket3: zero-filled.
	b3 := arr[3].([]interface{})
	for _, v := range b3[1:6] {
		if v.(xfloat8) != 0 {
			t.Errorf("bucket3 field = %v, want 0", v)
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
	// C++ getPartialOrderChain filters to local partial/partial-child orders
	// (xbridgeapp.cpp:3922): a parent-less exact order is not part of any chain.
	o.PartialAllowed = true

	res, err := ctx.dxGetMyPartialOrderChain([]json.RawMessage{jstr(dispID(o.ID))})
	if err != nil {
		t.Fatalf("dxGetMyPartialOrderChain: %v", err)
	}
	if arr, ok := res.([]orderDetailResult); !ok || len(arr) != 1 {
		t.Fatalf("dxGetMyPartialOrderChain = %v (%T)", res, res)
	}

	res, err = ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(dispID(o.ID))})
	if err != nil {
		t.Fatalf("dxPartialOrderChainDetails: %v", err)
	}
	m := mustJSONMap(t, res)
	if m["first_order_id"] == nil {
		t.Fatalf("dxPartialOrderChainDetails = %v (%T)", res, res)
	}

	// Missing id -> empty array (C++ returns [] for a valid but unknown order;
	// the all-zeros id is a uint256S null and is rejected with 1025 "bad order
	// id" up front, so use a non-null id that simply matches nothing).
	unknown := strings.Repeat("0", 63) + "f"
	if res, err := ctx.dxGetMyPartialOrderChain([]json.RawMessage{jstr(unknown)}); err != nil {
		t.Errorf("dxGetMyPartialOrderChain(missing) should return empty array, got error: %v", err)
	} else if arr, ok := res.([]interface{}); !ok || len(arr) != 0 {
		t.Errorf("dxGetMyPartialOrderChain(missing) = %v (%T), want []", res, res)
	}
	if _, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr("nope")}); err == nil {
		t.Error("dxPartialOrderChainDetails(missing) should error")
	}
}

// TestDxPartialOrderChainDetailsAggregate locks in C++ dxPartialOrderChainDetails'
// aggregate schema: a full lineage (ancestors + self + descendants) rooted at the
// oldest order, with per-status sent/received/notsent/notreceived totals and the
// orders field rendered as a hex-id array (not order objects). The chain is
// filtered to local partial/partial-child orders (xbridgeapp.cpp:3922) and
// sorted by created time (xbridgeapp.cpp:3985-3988), so the fixture uses
// monotonic created values to yield a root-first chain.
func TestDxPartialOrderChainDetailsAggregate(t *testing.T) {
	ctx := newWalletTestCtx()
	parent := &Order{ID: [32]byte{0xaa}, FromCurrency: "BTC", ToCurrency: "BTC",
		FromAmount: 100, ToAmount: 50, Status: "finished", Created: 10, Updated: 10,
		Mine: true, PartialAllowed: true}
	mid := seedOrder(ctx) // ID {0x01}, BTC/BTC, open
	mid.ParentID = [32]byte{0xaa}
	// Created order must be monotonic (parent < mid < child) so the C++
	// created-time sort yields a root-first chain.
	mid.Created, mid.Updated = 20, 20
	child := &Order{ID: [32]byte{0xbb}, FromCurrency: "BTC", ToCurrency: "BTC",
		FromAmount: 200, ToAmount: 100, Status: "canceled", Created: 30, Updated: 30,
		Mine: true}
	child.ParentID = [32]byte{0x01}
	ctx.Store.Add(parent)
	ctx.Store.Add(child)

	res, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(dispID(mid.ID))})
	if err != nil {
		t.Fatalf("dxPartialOrderChainDetails: %v", err)
	}
	m := mustJSONMap(t, res)
	if m["first_order_id"] != dispID(parent.ID) {
		t.Errorf("first_order_id = %v, want %v", m["first_order_id"], dispID(parent.ID))
	}
	orders, ok := m["orders"].([]interface{})
	if !ok || len(orders) != 3 {
		t.Fatalf("orders = %v (%T), want 3 hex ids", m["orders"], m["orders"])
	}
	// Final chain is sorted ascending by created time -> root first.
	if orders[0] != dispID(parent.ID) || orders[2] != dispID(child.ID) {
		t.Errorf("orders not root-first: %v", orders)
	}
	if m["total_orders_open"] != float64(1) || m["total_orders_finished"] != float64(1) || m["total_orders_canceled"] != float64(1) {
		t.Errorf("counts wrong: open=%v finished=%v canceled=%v",
			m["total_orders_open"], m["total_orders_finished"], m["total_orders_canceled"])
	}
	if m["total_reported_sent"] != formatXAmount(100) {
		t.Errorf("total_reported_sent = %v, want %v", m["total_reported_sent"], formatXAmount(100))
	}
	if m["total_reported_notsent"] != formatXAmount(1500000+200) {
		t.Errorf("total_reported_notsent = %v, want %v", m["total_reported_notsent"], formatXAmount(1500200))
	}
	// RPC-F28: p2sh_deposits carries ONE entry per chain order, empty strings
	// included, so callers can index by position against `orders`.
	if p, _ := m["p2sh_deposits"].([]interface{}); len(p) != 3 {
		t.Errorf("p2sh_deposits should have 3 per-order entries, got %v", m["p2sh_deposits"])
	}
}

func TestDxLoadConfAndTokens(t *testing.T) {
	ctx := newWalletTestCtx()
	lt, _ := ctx.dxGetLocalTokens(nil)
	if ls, _ := lt.([]string); len(ls) != 1 || ls[0] != "BTC" {
		t.Errorf("dxGetLocalTokens = %v", lt)
	}
	// RPC-F54: the network list is the pure SN service union; with no connected
	// servicenodes it is empty (no config fallback).
	nt, _ := ctx.dxGetNetworkTokens(nil)
	if ns, _ := nt.([]string); len(ns) != 0 {
		t.Errorf("dxGetNetworkTokens = %v, want []", nt)
	}
	// Reload from the same conf succeeds and rebuilds the connectors
	// (the fixture conf now carries Ip/Port), so dxGetLocalTokens keeps
	// reflecting the loaded wallet connectors (F53).
	if res, err := ctx.dxLoadXBridgeConf(nil); err != nil || res != true {
		t.Fatalf("dxLoadXBridgeConf = %v %v", res, err)
	}
	lt, _ = ctx.dxGetLocalTokens(nil)
	if ls, _ := lt.([]string); len(ls) != 1 || ls[0] != "BTC" {
		t.Errorf("dxGetLocalTokens after reload = %v, want [BTC]", lt)
	}
}

// TestDxGetLocalTokensConnectedOnly locks RPC-F53: dxGetLocalTokens reflects
// the loaded wallet connectors, not the config's ExchangeWallets list (which
// may name unconnected/duplicate tickers).
func TestDxGetLocalTokensConnectedOnly(t *testing.T) {
	ctx := newWalletTestCtx()
	// Config names DOGE in ExchangeWallets but no DOGE connector is loaded.
	ctx.Node.config.ExchangeWallets = []string{"BTC", "DOGE", "BTC"}
	lt, err := ctx.dxGetLocalTokens(nil)
	if err != nil {
		t.Fatalf("dxGetLocalTokens: %v", err)
	}
	ls, ok := lt.([]string)
	if !ok {
		t.Fatalf("dxGetLocalTokens = %v (%T)", lt, lt)
	}
	if len(ls) != 1 || ls[0] != "BTC" {
		t.Errorf("dxGetLocalTokens = %v, want [BTC] (connected connectors only)", ls)
	}
}

// TestDxLoadConfFailureFalse locks RPC-F56: a reload failure is a successful
// envelope whose bool result is false (C++ uret(success), rpcxbridge.cpp:229-234),
// not a business error.
func TestDxLoadConfFailureFalse(t *testing.T) {
	ctx := newWalletTestCtx()
	// No ConfPath configured -> reloadConf fails (node.go:307-310).
	ctx.Node.config.ConfPath = ""
	res, err := ctx.dxLoadXBridgeConf(nil)
	if err != nil {
		t.Fatalf("dxLoadXBridgeConf(fail) should not be a business error: %v", err)
	}
	if b, ok := res.(bool); !ok || b {
		t.Fatalf("dxLoadXBridgeConf(fail) = %v (%T), want false", res, res)
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
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr(dispID(missingID))}); err == nil || err.Code != errTxNotFound {
		t.Errorf("dxCancelOrder(unknown) = %+v, want TRANSACTION_NOT_FOUND", err)
	}
	// Malformed id -> INVALID_PARAMETERS.
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr("not-a-hex")}); err == nil || err.Code != errInvalidParameters {
		t.Errorf("dxCancelOrder(bad id) = %+v, want INVALID_PARAMETERS", err)
	}
	// In-process order (created) cannot be cancelled.
	o.Status = "created"
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr(dispID(o.ID))}); err == nil || err.Code != errInvalidState {
		t.Errorf("dxCancelOrder(created) = %+v, want INVALID_STATE", err)
	}
}

// TestDxCancelOrderNoLiveSession documents the per-trade M-key model: cancel is
// signed with the trade's live SwapSession key (C++ session sendCancelTransaction
// uses ptr->mPrivKey). An order stored in the order book with no live session —
// e.g. after a daemon restart, since the M keypair is in-memory only, matching
// C++ which also holds it only in the live XBridgeTransaction — must fail with
// "no active session for order" rather than signing with a stale/global key.
func TestDxCancelOrderNoLiveSession(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Node.conn = fakeXConn{}
	ctx.Node.sessions = map[string]*SwapSession{} // open order in store, no live swap session
	o := seedOrder(ctx)                           // status "open", cancelable by state

	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr(dispID(o.ID))}); err == nil || err.Code != errBadRequest {
		t.Errorf("dxCancelOrder(no live session) = %+v, want BAD_REQUEST (no active session for order)", err)
	}
}

// TestDxSplitInputsBadBoolParam verifies a malformed (non-boolean) flag errors
// out as the C++-thrown envelope error (code -1, "JSON value is not a boolean
// as expected") instead of silently defaulting (Module F; RPC-F01).
func TestDxSplitInputsBadBoolParam(t *testing.T) {
	ctx := newWalletTestCtx()
	// C++ requires exactly 7 params: ticker, splitamount, address, include_fees,
	// show_rawtx, submit, utxos. include_fees=123 is not a boolean -> the
	// UniValue get_bool() throw becomes an envelope error (code -1).
	params := []json.RawMessage{
		jstr("BTC"), jstr("1000000"), jstr(btcAddr),
		json.RawMessage("123"), // not a boolean -> must error
		json.RawMessage("false"),
		json.RawMessage("true"),
		json.RawMessage(`[{"txid":"0000000000000000000000000000000000000000000000000000000000000000","vout":0,"amount":"1","scriptPubKey":"76a914000000000000000000000000000000000000000088ac","address":"` + btcAddr + `"}]`),
	}
	res, err := ctx.dxSplitInputs(params)
	if err == nil {
		t.Fatalf("expected envelope error for non-boolean include_fees, got res=%v", res)
	}
	if err.Code != -1 {
		t.Errorf("err.Code = %d, want -1 (envelope misc error)", err.Code)
	}
	if !err.envelope {
		t.Errorf("err must be an envelope error (C++ get_bool() throw), got business error %+v", err)
	}
	if err.Error != "JSON value is not a boolean as expected" {
		t.Errorf("err.Error = %q, want UniValue message", err.Error)
	}
}

func TestGetNetworkInfo(t *testing.T) {
	// Defaults when Config leaves the version blank (fall back to 4.4.1).
	ctx := &HandlerCtx{Store: NewStore(), Node: &Node{}}
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
	if m["subversion"] != "/Blocknet:4.4.1/" {
		t.Errorf("subversion = %v, want /Blocknet:4.4.1/", m["subversion"])
	}
	// F46 alignment with real blocknetd (rpc/net.cpp:495-527, version.h).
	if m["protocolversion"] != 70713 {
		t.Errorf("protocolversion = %v, want 70713", m["protocolversion"])
	}
	if m["xbridgeprotocolversion"] != 55 {
		t.Errorf("xbridgeprotocolversion = %v, want 55", m["xbridgeprotocolversion"])
	}
	if m["xrouterprotocolversion"] != 50 {
		t.Errorf("xrouterprotocolversion = %v, want 50", m["xrouterprotocolversion"])
	}
	if m["relayfee"] != "0.00010000" {
		t.Errorf("relayfee = %v, want \"0.00010000\"", m["relayfee"])
	}
	if m["incrementalfee"] != "0.00001000" {
		t.Errorf("incrementalfee = %v, want \"0.00001000\"", m["incrementalfee"])
	}
	nets, ok := m["networks"].([]map[string]interface{})
	if !ok || len(nets) != 3 {
		t.Fatalf("networks = %v (%T), want 3 entries", m["networks"], m["networks"])
	}
	for _, n := range nets {
		if n["proxy_randomize_credentials"] != false {
			t.Errorf("networks[].proxy_randomize_credentials = %v, want false", n["proxy_randomize_credentials"])
		}
	}
	// Key set must be the 15 C++ getnetworkinfo fields (order is sorted-map).
	wantKeys := []string{
		"version", "subversion", "protocolversion", "xbridgeprotocolversion",
		"xrouterprotocolversion", "localservices", "localrelay", "timeoffset",
		"networkactive", "connections", "networks", "relayfee", "incrementalfee",
		"localaddresses", "warnings",
	}
	if len(m) != len(wantKeys) {
		t.Errorf("field count = %d, want %d", len(m), len(wantKeys))
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing getnetworkinfo field %q", k)
		}
	}
	if m["connections"] != 0 {
		t.Errorf("connections = %v, want 0 (no live conn)", m["connections"])
	}
	// Rejects params.
	if _, err := ctx.getNetworkInfo([]json.RawMessage{jstr("x")}); err == nil {
		t.Error("getNetworkInfo should reject params")
	}

	// Explicit Config overrides the defaults.
	ctx2 := &HandlerCtx{Store: NewStore(), Node: &Node{config: &Config{WalletVersion: 4120000, WalletVersionStr: "/blocknet:4.12.0/"}}}
	res2, _ := ctx2.getNetworkInfo(nil)
	m2 := res2.(map[string]interface{})
	if m2["version"] != 4120000 || m2["subversion"] != "/blocknet:4.12.0/" {
		t.Errorf("override = %v / %v", m2["version"], m2["subversion"])
	}
}

// TestDxLoadConfHotReload verifies dxLoadXBridgeConf re-reads xbridge.conf from
// ConfPath and rebuilds the coin registry + connectors live (C++ reload-from-
// the-same-conf behaviour), without the daemon needing a restart.
func TestDxLoadConfHotReload(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "xbridge.conf")

	const btcOnly = "[Main]\nExchangeWallets=BTC\n\n[BTC]\nTitle=Bitcoin\nCreateTxMethod=BTC\nAddressPrefix=0\nScriptPrefix=5\nCOIN=100000000\nTxVersion=1\nDustAmount=546\nMinTxFee=1000\nBlockTime=600\nFeePerByte=2\nConfirmations=2\nIp=127.0.0.1\nPort=8332\n"
	const withDoge = "[Main]\nExchangeWallets=BTC,DOGE\n\n[BTC]\nTitle=Bitcoin\nCreateTxMethod=BTC\nAddressPrefix=0\nScriptPrefix=5\nCOIN=100000000\nTxVersion=1\nDustAmount=546\nMinTxFee=1000\nBlockTime=600\nFeePerByte=2\nConfirmations=2\nIp=127.0.0.1\nPort=8332\n\n[DOGE]\nTitle=Dogecoin\nCreateTxMethod=BTC\nAddressPrefix=30\nScriptPrefix=22\nCOIN=100000000\nTxVersion=1\nDustAmount=546\nMinTxFee=1000\nBlockTime=60\nFeePerByte=1\nConfirmations=2\nIp=127.0.0.1\nPort=22555\n"

	if err := os.WriteFile(confPath, []byte(btcOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	// Build a node whose config mirrors what the daemon parsed from confPath.
	cc, err := config.Load(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := coins.InitFromConf(cc.Coins); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Confs:           cc.Coins,
		Connectors:      map[string]wallet.Connector{},
		ExchangeWallets: cc.Main.ExchangeWallets,
		NetworkTokens:   []string{"BTC"},
		ConfPath:        confPath,
	}
	node := &Node{config: cfg, store: NewStore(), signer: crypto.NewBtcSigner(), stop: make(chan struct{}), snReg: servicenode.NewRegistry()}
	ctx := &HandlerCtx{Store: NewStore(), Node: node}

	if !coins.Has("BTC") || coins.Has("DOGE") {
		t.Fatalf("precondition: coins = BTC only, got BTC=%v DOGE=%v", coins.Has("BTC"), coins.Has("DOGE"))
	}

	// Swap the on-disk conf to include DOGE, then hot-reload.
	if err := os.WriteFile(confPath, []byte(withDoge), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, rerr := ctx.dxLoadXBridgeConf(nil); rerr != nil || res != true {
		t.Fatalf("dxLoadXBridgeConf = %v %v", res, rerr)
	}

	// The live registry + config must now reflect DOGE, and BTC must persist.
	if !coins.Has("BTC") || !coins.Has("DOGE") {
		t.Errorf("after reload: coins.Has BTC=%v DOGE=%v", coins.Has("BTC"), coins.Has("DOGE"))
	}
	// The reload rebuilt connectors for both coins, so dxGetLocalTokens reflects
	// the connected wallet set (F53).
	lt, _ := ctx.dxGetLocalTokens(nil)
	gotLocal := map[string]bool{}
	for _, tk := range lt.([]string) {
		gotLocal[tk] = true
	}
	if !gotLocal["BTC"] || !gotLocal["DOGE"] {
		t.Errorf("dxGetLocalTokens after reload = %v, want [BTC DOGE]", lt)
	}
	// dxGetNetworkTokens stays the pure SN union (empty: no connected SNs).
	nt, _ := ctx.dxGetNetworkTokens(nil)
	got := map[string]bool{}
	for _, tk := range nt.([]string) {
		got[tk] = true
	}
	if len(got) != 0 {
		t.Errorf("dxGetNetworkTokens after reload = %v, want [] (pure SN union)", nt)
	}
}

// TestDxLoadConfHotReloadMissingPath verifies that a node started without a
// conf path (ConfPath empty) reports the failure as a false result — not an
// error — mirroring C++ uret(success) (RPC-F56).
func TestDxLoadConfHotReloadMissingPath(t *testing.T) {
	node := &Node{config: &Config{}, store: NewStore(), signer: crypto.NewBtcSigner(), stop: make(chan struct{}), snReg: servicenode.NewRegistry()}
	ctx := &HandlerCtx{Store: NewStore(), Node: node}
	if res, rerr := ctx.dxLoadXBridgeConf(nil); rerr != nil {
		t.Fatalf("dxLoadXBridgeConf(empty ConfPath) should not be a business error: %v", rerr)
	} else if b, ok := res.(bool); !ok || b {
		t.Fatalf("dxLoadXBridgeConf(empty ConfPath) = %v (%T), want false", res, res)
	}
}

// TestDxGetOrdersSortedById locks in RPC-F03: C++ iterates m_transactions
// (std::map<uint256>) in LSB-first byte order (uint256.h:45-49), so Go's
// map-ordered Store.List() must be sorted by orderIDLess — NOT display-hex
// ascending. Three open BTC/BTC orders; the LSB order is B < A < C while
// display-hex ascending would give A < C < B.
func TestDxGetOrdersSortedById(t *testing.T) {
	ctx := newWalletTestCtx()
	add := func(bs ...byte) string {
		var id [32]byte
		copy(id[:], bs)
		ctx.Store.Add(&Order{
			ID: id, Type: OrderTypeMaker, FromCurrency: "BTC", FromAmount: 1000000,
			ToCurrency: "BTC", ToAmount: 200000, Created: 1, Updated: 1, Status: "open", Mine: true,
		})
		return dispID(id)
	}
	a := add(0x01)       // display "..0001"
	b := add(0x00, 0x01) // display "..000100"
	c := add(0xff)       // display "..00ff"
	if strings.Compare(a, b) >= 0 {
		t.Fatalf("test setup: display-hex a<b expected, got %s vs %s", a, b)
	}

	res, err := ctx.dxGetOrders(nil)
	if err != nil {
		t.Fatalf("dxGetOrders: %v", err)
	}
	arr, ok := res.([]orderListResult)
	if !ok || len(arr) != 3 {
		t.Fatalf("dxGetOrders = %v (%T), want 3 orders", res, res)
	}
	want := []string{b, a, c} // LSB-first: [0x00,0x01] < [0x01] < [0xff]
	for i, id := range want {
		if arr[i].ID != id {
			t.Errorf("dxGetOrders[%d].id = %s, want %s (LSB order)", i, arr[i].ID, id)
		}
	}
}

// TestDxGetOrdersSixtySecondBoundary locks in RPC-F04: the 60 s filter on
// canceled/finished/expired orders drops exactly when
// floor(now) - txtime >= 61e6, mirroring C++
// (second_clock - txtime).total_seconds() > 60 (rpcxbridge.cpp:439).
// total_seconds() truncates the DIFFERENCE toward zero, so a txtime with a
// sub-second fraction keeps the order one whole second longer than a
// whole-second-aligned txtime (61.9 s old is still "60 s").
func TestDxGetOrdersSixtySecondBoundary(t *testing.T) {
	ctx := newWalletTestCtx()
	orig := NowMicro
	defer func() { NowMicro = orig }()

	seed := func(updated uint64) {
		ctx.Store.Add(&Order{
			ID: [32]byte{0x02}, Type: OrderTypeMaker, FromCurrency: "BTC", FromAmount: 1000000,
			ToCurrency: "BTC", ToAmount: 200000, Created: updated, Updated: updated, Status: "canceled", Mine: true,
		})
	}
	count := func() int {
		res, err := ctx.dxGetOrders(nil)
		if err != nil {
			t.Fatalf("dxGetOrders: %v", err)
		}
		return len(res.([]orderListResult))
	}
	cases := []struct {
		name    string
		updated uint64
		now     uint64
		want    int
	}{
		// Whole-second txtime: drops exactly at floor(now) - T >= 61e6.
		{"aligned 60.9s kept", 0, 60_900_000, 1},
		{"aligned 61.0s dropped", 0, 61_000_000, 0},
		{"aligned 600s dropped", 0, 600_000_000, 0},
		// Sub-second txtime fraction: total_seconds() truncates the difference,
		// so an order 61.9 s old is still "60 s" and is kept; it drops at 62 s.
		{"fractional 61.9s kept", 1_000, 61_900_000, 1},
		{"fractional 62.0s dropped", 1_000, 62_000_000, 0},
	}
	for _, c := range cases {
		seed(c.updated)
		NowMicro = func() uint64 { return c.now }
		if n := count(); n != c.want {
			t.Errorf("%s: got %d orders, want %d", c.name, n, c.want)
		}
	}
	// An open order is never filtered regardless of age.
	seed(0)
	NowMicro = func() uint64 { return 600_000_000 }
	ctx.Store.Update(orderKey([32]byte{0x02}), func(o *Order) { o.Status = "open" })
	if n := count(); n != 1 {
		t.Errorf("open order 600s old: got %d orders, want 1", n)
	}
}

// TestDxGetOrderNotFoundPadded locks in RPC-F06: dxGetOrder renders the
// not-found message with the PARSED id's GetHex (rpcxbridge.cpp:785), so a
// short/malformed id is zero-padded to 64 hex chars, not echoed raw.
func TestDxGetOrderNotFoundPadded(t *testing.T) {
	ctx := newWalletTestCtx()
	if _, err := ctx.dxGetOrder([]json.RawMessage{jstr("deadbeef")}); err == nil {
		t.Fatal("dxGetOrder(short) should error")
	} else if err.Code != errTxNotFound {
		t.Fatalf("dxGetOrder(short) code = %d, want 1021", err.Code)
	} else if err.Error != "Transaction "+strings.Repeat("0", 56)+"deadbeef not found" {
		t.Errorf("dxGetOrder(short) message = %q, want zero-padded 64-hex id", err.Error)
	}
	// An all-zeros id (uint256S null) has no null gate here: it just misses.
	if _, err := ctx.dxGetOrder([]json.RawMessage{jstr(strings.Repeat("0", 64))}); err == nil {
		t.Fatal("dxGetOrder(null id) should error")
	} else if err.Code != errTxNotFound {
		t.Fatalf("dxGetOrder(null id) code = %d, want 1021", err.Code)
	}
}

// TestDxCancelOrderShortId locks in RPC-F05: dxCancelOrder rejects a NULL
// uint256S id with 1025 "Invalid order id [<raw param>]" (rpcxbridge.cpp:
// 1345-1353). A short-but-valid hex id ("abc") is NOT null — it is left-padded
// to 64 hex and misses the store -> 1021; only genuinely unparseable input
// ("zz", "0x", empty) parses to the null id and hits the 1025 gate.
func TestDxCancelOrderShortId(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	// Unparseable -> null id -> 1025 with the raw param string.
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr("zz")}); err == nil {
		t.Fatal("dxCancelOrder(non-hex) should error")
	} else if err.Code != errInvalidParameters || err.Error != "Invalid parameters: Invalid order id [zz]" {
		t.Errorf("dxCancelOrder(non-hex) = %+v, want 1025 with raw id", err)
	}
	// "0x" and empty also parse to null -> 1025, and the order survives.
	for _, id := range []string{"0x", ""} {
		if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr(id)}); err == nil || err.Code != errInvalidParameters {
			t.Errorf("dxCancelOrder(%q) = %+v, want 1025", id, err)
		}
	}
	if o.Status != "open" {
		t.Errorf("order status = %q, want open (null id must not cancel)", o.Status)
	}
	// The all-zeros id is null too -> 1025.
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr(strings.Repeat("0", 64))}); err == nil || err.Code != errInvalidParameters {
		t.Errorf("dxCancelOrder(null id) = %+v, want 1025", err)
	}
	// A short-but-valid hex id is left-padded, not rejected: it misses the
	// store -> 1021 not-found (uint256S("abc") is non-null, rpcxbridge.cpp:1362).
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr("abc")}); err == nil {
		t.Fatal("dxCancelOrder(abc) should error")
	} else if err.Code != errTxNotFound {
		t.Errorf("dxCancelOrder(abc) = %+v, want 1021 not-found (short id is valid)", err)
	}
}

// TestDxCancelOrderSideEffectBeforeValidation locks in RPC-F07's to-connector
// case: C++ cancels the order FIRST (cancelXBridgeTransaction,
// xbridgeapp.cpp:2468-2501) and only then resolves the connectors to build the
// result (rpcxbridge.cpp:1364-1385). A missing TO connector must NOT prevent
// the cancel side effect — the order is cancelled, and only the result build
// fails with NO_SESSION carrying the to currency.
func TestDxCancelOrderSideEffectBeforeValidation(t *testing.T) {
	ctx := newWalletTestCtx()
	priv, pub := newKey(t)
	o := &Order{
		ID: [32]byte{0x07}, Type: OrderTypeMaker, FromCurrency: "BTC", FromAmount: 1000000,
		ToCurrency: "NOPE", ToAmount: 200000, Created: 1, Updated: 1, Status: "open", Mine: true,
		MakerAddress: "mk", TakerAddress: "tk",
	}
	ctx.Store.Add(o)
	ctx.Node.conn = fakeXConn{}
	ctx.Node.sessions = map[string]*SwapSession{}
	ctx.Node.newMakerSession(o, MakeOrderParams{MakerAddress: "mk", TakerAddress: "tk"}, arr32(priv), toArr33(pub))

	res, err := ctx.dxCancelOrder([]json.RawMessage{jstr(dispID(o.ID))})
	// The cancel side effect happened despite the missing TO connector...
	if got := ctx.Store.Get(hexEncode(o.ID[:])); got == nil || got.Status != "canceled" {
		t.Errorf("order status = %v, want canceled (cancel must run before connector validation)", got)
	}
	// ...and the RPC then fails to build the result: NO_SESSION carrying the
	// TO currency (the from connector was present, so the cancel went through).
	if err == nil {
		t.Fatalf("dxCancelOrder = %+v, want NO_SESSION error", res)
	}
	if err.Code != errNoSession || err.Error != "No session for currency NOPE" {
		t.Errorf("dxCancelOrder code/msg = %d/%q, want 1018 'No session for currency NOPE'", err.Code, err.Error)
	}
}

// TestDxCancelOrderFromConnectorGate locks in RPC-F08's from-connector gate:
// C++ cancelXBridgeTransaction requires the FROM-currency wallet connector
// BEFORE broadcasting the cancel (xbridgeapp.cpp:2489-2495). A missing
// from-currency session prevents the cancel side effect entirely, surfaced as
// NO_SESSION with an EMPTY argument — "No session for currency " (trailing
// space).
func TestDxCancelOrderFromConnectorGate(t *testing.T) {
	ctx := newWalletTestCtx()
	o := &Order{
		ID: [32]byte{0x08}, Type: OrderTypeMaker, FromCurrency: "NOPE", FromAmount: 1000000,
		ToCurrency: "NOPE", ToAmount: 200000, Created: 1, Updated: 1, Status: "open", Mine: true,
		MakerAddress: "mk", TakerAddress: "tk",
	}
	ctx.Store.Add(o)
	ctx.Node.conn = fakeXConn{}

	res, err := ctx.dxCancelOrder([]json.RawMessage{jstr(dispID(o.ID))})
	if err == nil {
		t.Fatalf("dxCancelOrder(missing from connector) = %+v, want NO_SESSION error", res)
	}
	if err.Code != errNoSession || err.Error != "No session for currency " {
		t.Errorf("dxCancelOrder(missing from connector) = %d/%q, want 1018 empty-arg message", err.Code, err.Error)
	}
	// The missing from-connector gate fires BEFORE the cancel: no side effect.
	if got := ctx.Store.Get(hexEncode(o.ID[:])); got == nil || got.Status != "open" {
		t.Errorf("order status = %v, want open (from-connector gate must prevent the cancel)", got)
	}
}

// TestDxCancelOrderNonLocal locks in RPC-F08's isLocal branch: C++
// cancelXBridgeTransaction refuses to cancel an order this node does not own
// with TRANSACTION_NOT_FOUND, surfaced via makeError(res, __FUNCTION__) with
// an EMPTY argument — the double-space "Transaction  not found"
// (xbridgeapp.cpp:2473-2479; rpcxbridge.cpp:1370).
func TestDxCancelOrderNonLocal(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Node.conn = fakeXConn{} // satisfy requireWrite (runs before the Mine gate)
	o := seedOrder(ctx)
	o.Mine = false // observed, not created locally
	res, err := ctx.dxCancelOrder([]json.RawMessage{jstr(dispID(o.ID))})
	if err == nil {
		t.Fatalf("dxCancelOrder(non-local) = %+v, want error", res)
	}
	if err.Code != errTxNotFound || err.Error != "Transaction  not found" {
		t.Errorf("dxCancelOrder(non-local) = %+v, want 1021 with double-space text", err)
	}
	if got := ctx.Store.Get(hexEncode(o.ID[:])); got == nil || got.Status != "open" {
		t.Errorf("non-local order must not be cancelled, status = %v", got)
	}
}

// TestDxCancelOrderHistoryStateGate verifies the C++ history fallback: an order
// that only lives in the history map still resolves (App::transaction falls
// back) and is then rejected by the state gate — 1028, not 1021
// (rpcxbridge.cpp:1355-1363).
func TestDxCancelOrderHistoryStateGate(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	ctx.Store.MoveToHistory(hexEncode(o.ID[:]), "finished", 0, NowMicro())
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr(dispID(o.ID))}); err == nil {
		t.Fatal("dxCancelOrder(finished-in-history) should error")
	} else if err.Code != errInvalidState || err.Error != "invalid transaction state The order is already finished" {
		t.Errorf("dxCancelOrder(finished-in-history) = %+v, want 1028 already finished", err)
	}
}

// TestDxCancelOrderNotFoundPadded verifies the store-miss message renders the
// PARSED id's GetHex (zero-padded 64-hex), matching rpcxbridge.cpp:1362.
func TestDxCancelOrderNotFoundPadded(t *testing.T) {
	ctx := newWalletTestCtx()
	if _, err := ctx.dxCancelOrder([]json.RawMessage{jstr("abc")}); err == nil {
		t.Fatal("dxCancelOrder(short) should error")
	} else if err.Code != errTxNotFound {
		t.Fatalf("dxCancelOrder(short) code = %d, want 1021", err.Code)
	} else if err.Error != "Transaction "+strings.Repeat("0", 60)+"0abc not found" {
		t.Errorf("dxCancelOrder(short) message = %q, want zero-padded 64-hex id", err.Error)
	}
}

// TestDxGetOrderHistoryValidations locks in RPC-F18: the xQuery ctor rejects
// bad queries with the EXACT C++ messages in C++ precedence order
// (util/xseries.h:91-101): granularity whitelist, aligned start too early,
// start >= end, end beyond now+1day, then the interval_limit range.
func TestDxGetOrderHistoryValidations(t *testing.T) {
	ctx := newWalletTestCtx()
	const early = int64(1519516800) // XSeries earliest (2018-02-25)

	hist := func(args ...interface{}) (interface{}, *rpcError) {
		raw := make([]json.RawMessage, len(args))
		for i, a := range args {
			switch v := a.(type) {
			case string:
				raw[i] = jstr(v)
			case int64:
				raw[i] = jnum(v)
			case bool:
				b, _ := json.Marshal(v)
				raw[i] = b
			default:
				t.Fatalf("bad arg %d: %v", i, a)
			}
		}
		return ctx.dxGetOrderHistory(raw)
	}
	base := []interface{}{"BTC", "LTC", early + 1000, early + 1180, int64(60)}
	expectErr := func(name string, args []interface{}, want string) {
		if _, err := hist(args...); err == nil {
			t.Errorf("%s: want error, got nil", name)
		} else if err.Code != errInvalidParameters || err.Error != "Invalid parameters: "+want {
			t.Errorf("%s = %+v, want 1025 %q", name, err, want)
		}
	}
	ok := func(args []interface{}) {
		if _, err := hist(args...); err != nil {
			t.Errorf("valid query errored: %v", err)
		}
	}

	// Bad granularity (whitelist of 60,300,900,3600,21600,86400).
	badG := append([]interface{}{}, base...)
	badG[4] = int64(45)
	expectErr("bad granularity", badG, "granularity=45 must be one of: 60,300,900,3600,21600,86400")
	// Granularity 0 / negative are also outside the whitelist.
	zeroG := append([]interface{}{}, base...)
	zeroG[4] = int64(0)
	expectErr("zero granularity", zeroG, "granularity=0 must be one of: 60,300,900,3600,21600,86400")

	// Start before the earliest (2018-02-25) -> too early.
	tooEarly := append([]interface{}{}, base...)
	tooEarly[2] = int64(1500000000)
	expectErr("start too early", tooEarly, "Start time too early.")
	// Negative start snaps to epoch 0 -> too early too.
	negStart := append([]interface{}{}, base...)
	negStart[2] = int64(-5)
	expectErr("negative start", negStart, "Start time too early.")

	// start >= end -> "Start time >= end time."
	inv := append([]interface{}{}, base...)
	inv[2] = int64(early + 2000)
	inv[3] = int64(early + 1000)
	expectErr("start >= end", inv, "Start time >= end time.")

	// end beyond now + 1 day -> too large. Pin NowMicro.
	orig := NowMicro
	defer func() { NowMicro = orig }()
	NowMicro = func() uint64 { return uint64(early+1180) * 1e6 }
	tooLate := append([]interface{}{}, base...)
	tooLate[3] = int64(early + 1180 + 2*86400)
	expectErr("end too large", tooLate, "Start/end times are too large.")

	// interval_limit out of range (1..2147483647).
	badLimit := append([]interface{}{}, base...)
	badLimit = append(badLimit, bool(false), bool(false), int64(0))
	expectErr("limit 0", badLimit, "interval_limit must be in range 1 to 2147483647.")
	goodLimit := append([]interface{}{}, base...)
	goodLimit = append(goodLimit, bool(false), bool(false), int64(18000))
	ok(goodLimit)
}

// TestDxGetOrderHistoryInverse locks in the with_inverse=true path (P2-1): a
// fill whose pair is the INVERSE of the query is aggregated via the inverted
// aggregate. C++ quantizes the DIRECT price (ccy::Asset::Price) then
// reciprocates WITHOUT re-quantizing (xseries.cpp:176-185), and the volume
// side swaps to the fill's taker amount (xseries.cpp:127-130). A fill ratio
// that is not grid-exact discriminates: LTC 3.0 / BTC 1.0 direct price =
// quantize(1/3) = 0.333333, inverted = 1/0.333333 = 3.000003 (NOT 3.000000).
func TestDxGetOrderHistoryInverse(t *testing.T) {
	ctx := newWalletTestCtx()
	const xEarly = int64(1519516800)
	// Query maker=BTC taker=LTC. This fill is LTC->BTC (inverse).
	ctx.Store.AddFill(fillEntry{ID: "inv", Time: uint64(xEarly+1030) * 1e6, Maker: "LTC", Taker: "BTC", MakerSize: "3.0", TakerSize: "1.0"})

	withInverse, _ := json.Marshal(true)
	res, err := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jnum(xEarly + 1000), jnum(xEarly + 1180), jnum(60),
		json.RawMessage("false"), withInverse,
	})
	if err != nil {
		t.Fatalf("dxGetOrderHistory(inverse): %v", err)
	}
	arr := res.([]interface{})
	// The fill lands in bucket1 (xEarly+1020..xEarly+1080), window aligned to
	// [xEarly+960, xEarly+1200) -> 4 buckets.
	row := arr[1].([]interface{})
	// Inverted price = 1/quantize(1.0/3.0) = 1/0.333333 = 3.0000030000030002,
	// which C++ renders fixed-8 as "3.00000300" (NOT "3.00000000"). Assert the
	// RENDERED strings since the float64 has trailing precision artifacts.
	// Inverted volume = the fill's taker side = 1.0.
	render := func(v interface{}) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for i := 1; i <= 4; i++ {
		if s := render(row[i]); s != "3.00000300" {
			t.Errorf("inverse bucket1 field %d = %s, want 3.00000300", i, s)
		}
	}
	if s := render(row[5]); s != "1.00000000" {
		t.Errorf("inverse bucket1 volume = %s, want 1.00000000", s)
	}
}

// TestDxGetOrderHistoryLimitTail locks in the interval_limit cap (P2-2): C++
// getChainXAggregateSeries caps the bucket count at limit and shifts the
// window to the most-recent tail (xseries.cpp:106-112). A 4-bucket window with
// limit=1 returns ONLY the last bucket.
func TestDxGetOrderHistoryLimitTail(t *testing.T) {
	ctx := newWalletTestCtx()
	const xEarly = int64(1519516800)
	ctx.Store.AddFill(fillEntry{ID: "aaa", Time: uint64(xEarly+1030) * 1e6, Maker: "BTC", Taker: "LTC", MakerSize: "1.0", TakerSize: "2.0"})
	ctx.Store.AddFill(fillEntry{ID: "ddd", Time: uint64(xEarly+1150) * 1e6, Maker: "BTC", Taker: "LTC", MakerSize: "1.0", TakerSize: "8.0"})

	res, err := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jnum(xEarly + 1000), jnum(xEarly + 1180), jnum(60),
		json.RawMessage("false"), json.RawMessage("false"), json.RawMessage("1"),
	})
	if err != nil {
		t.Fatalf("dxGetOrderHistory(limit=1): %v", err)
	}
	arr := res.([]interface{})
	if len(arr) != 1 {
		t.Fatalf("limit=1: got %d buckets, want 1", len(arr))
	}
	// Only the LAST bucket survives: the fill at xEarly+1150 (bucket3,
	// [xEarly+1140, xEarly+1200)); the earlier one is outside the shifted tail.
	row := arr[0].([]interface{})
	if row[3].(xfloat8) != 8.0 || row[5].(xfloat8) != 1.0 {
		t.Errorf("limit=1 tail bucket = %v, want open/close 8.0 (ddd only)", row)
	}
}

// TestDxGetOrderBookPriceBump locks in RPC-F20: dxGetOrderBook prices use
// C++'s xBridgeValueFromAmount formula — a/COIN + 1/::COIN on each amount
// before dividing (xutil.cpp:293-312). Observable only for tiny base-unit
// amounts: an ask from=3, to=1 renders 0.335548 (the plain ratio would be
// 0.333333) and the inverse bid from=3, to=1 renders 2.980198 (plain 3.000000).
func TestDxGetOrderBookPriceBump(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Store.Add(&Order{
		ID: [32]byte{0x20}, Type: OrderTypeMaker, FromCurrency: "BTC", FromAmount: 3,
		ToCurrency: "LTC", ToAmount: 1, Status: "open",
	})
	// Bid-side bump: the LTC->BTC order lands in bids for the BTC/LTC query.
	ctx.Store.Add(&Order{
		ID: [32]byte{0x21}, Type: OrderTypeMaker, FromCurrency: "LTC", FromAmount: 3,
		ToCurrency: "BTC", ToAmount: 1, Status: "open",
	})
	res, err := ctx.dxGetOrderBook([]json.RawMessage{jnum(1), jstr("BTC"), jstr("LTC")})
	if err != nil {
		t.Fatalf("dxGetOrderBook: %v", err)
	}
	ob := res.(orderBookResult)
	if len(ob.Asks) != 1 {
		t.Fatalf("asks = %v, want 1", ob.Asks)
	}
	if ob.Asks[0][0] != "0.335548" {
		t.Errorf("ask price = %v, want 0.335548 (C++ +1/COIN formula)", ob.Asks[0][0])
	}
	if len(ob.Bids) != 1 {
		t.Fatalf("bids = %v, want 1", ob.Bids)
	}
	if ob.Bids[0][0] != "2.980198" {
		t.Errorf("bid price = %v, want 2.980198 (C++ +1/COIN formula)", ob.Bids[0][0])
	}
}

// TestDxGetOrderBookTieBreak locks in RPC-F21: equal-price best orders are
// broken by the smallest raw id (orderIDLess), so detail 4's first id is the
// smallest-id order regardless of insertion order.
func TestDxGetOrderBookTieBreak(t *testing.T) {
	ctx := newWalletTestCtx()
	big := [32]byte{0x02}
	small := [32]byte{0x01}
	for _, id := range [][32]byte{big, small} {
		ctx.Store.Add(&Order{
			ID: id, Type: OrderTypeMaker, FromCurrency: "BTC", FromAmount: 1500000,
			ToCurrency: "LTC", ToAmount: 300000, Status: "open", // same price 0.2
		})
	}
	res, err := ctx.dxGetOrderBook([]json.RawMessage{jnum(4), jstr("BTC"), jstr("LTC")})
	if err != nil {
		t.Fatalf("dxGetOrderBook: %v", err)
	}
	ob := res.(orderBookResult)
	if len(ob.Asks) != 1 {
		t.Fatalf("asks = %v, want 1", ob.Asks)
	}
	ids, ok := ob.Asks[0][2].([]string)
	if !ok || len(ids) != 2 {
		t.Fatalf("detail4 ask ids = %v (%T), want 2 ids", ob.Asks[0][2], ob.Asks[0][2])
	}
	if ids[0] != dispID(small) {
		t.Errorf("best tie-break id = %s, want %s (smallest raw id)", ids[0], dispID(small))
	}
}

// TestDxGetOrderBookDetail4Golden locks in RPC-F57: detail 4 rows are
// [[price, amount, [ids]]] — the ids array NESTED inside the row array
// (rpcxbridge.cpp:1905-1926).
func TestDxGetOrderBookDetail4Golden(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx) // BTC/BTC open order; a BTC/BTC ask (maker==taker) and bid
	res, err := ctx.dxGetOrderBook([]json.RawMessage{jnum(4), jstr("BTC"), jstr("BTC")})
	if err != nil {
		t.Fatalf("dxGetOrderBook: %v", err)
	}
	b, merr := json.Marshal(res)
	if merr != nil {
		t.Fatal(merr)
	}
	id := dispID(o.ID)
	want := `{"detail":4,"maker":"BTC","taker":"BTC","asks":[["0.200000","1.500000",["` + id + `"]]],"bids":[["5.000000","0.300000",["` + id + `"]]]}`
	if string(b) != want {
		t.Errorf("detail4 JSON:\n got %s\nwant %s", string(b), want)
	}
}
