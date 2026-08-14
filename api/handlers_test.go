package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/wallet"
)

// jstr wraps a Go string as a JSON-RawMessage param (a quoted
// JSON string), matching how api/handlers.go expects positional
// string params.
func jstr(s string) json.RawMessage {
	return json.RawMessage([]byte(`"` + s + `"`))
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

	// id -> object keyed by id and the order's currency (empty utxo list).
	ctx.Store.Lock(o)
	id := dispID(o.ID)
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
	ctx.Store.RecordCancelled(dispID(o.ID), 5)
	res, _ = ctx.dxFlushCancelledOrders(nil)
	m, _ = res.(map[string]interface{})
	if len(m["flushedOrders"].([]map[string]interface{})) != 1 {
		t.Fatalf("dxFlushCancelledOrders (recorded) = %v", res)
	}
}

// TestDxGetLockedUtxosKeyByState verifies dxGetLockedUtxos keys the per-order
// array by the transaction's state (C++ rpcxbridge.cpp:2674-2677), never by
// which wallets happen to be connected: an open broadcast order is keyed by
// the maker currency alone, an accepted (created) order by maker_and_taker —
// with the same result whether or not the taker wallet is connected.
func TestDxGetLockedUtxosKeyByState(t *testing.T) {
	ctx := newWalletTestCtx()

	add := func(seed byte, status string) (string, *Order) {
		id := [32]byte{seed}
		o := &Order{
			ID: id, Type: OrderTypeMaker, FromCurrency: "BTC", FromAmount: 1500000,
			ToCurrency: "SYS", ToAmount: 300000, Created: 1, Updated: 1,
			Status: status, Mine: true,
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

	// Now connect the taker wallet: the open order must STILL be single-keyed
	// (old code switched to the dual key purely on connector presence), and the
	// created order must keep the dual key.
	ctx.Node.config.Connectors["SYS"] = &stubConn{ticker: "SYS", addr: btcAddr}
	openID, _ := add(0x12, "open")
	if k := lockedKey(openID); k != "BTC" {
		t.Fatalf("open order (taker wallet connected) key = %q, want BTC", k)
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
			if f, ok := v.(xfloat); !ok || f != 0 {
				t.Errorf("bucket field = %v, want 0", v)
			}
		}
	}
	// end <= start -> no buckets -> [].
	endStart, e2 := ctx.dxGetOrderHistory([]json.RawMessage{
		jstr("BTC"), jstr("LTC"), jnum(100), jnum(0), jnum(60),
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
// order_ids=true. The time window is aligned to granularity boundaries
// (matching C++ XSeries behavior), so the effective window may be wider
// than the raw [start, end) range.
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
		if v.(xfloat) != 0 {
			t.Errorf("bucket0 field = %v, want 0", v)
		}
	}
	// bucket1: open=2.0, high=4.0, low=2.0, close=4.0, volume=6.0
	b1 := arr[1].([]interface{})
	if b1[3].(xfloat) != 2.0 || b1[2].(xfloat) != 4.0 || b1[1].(xfloat) != 2.0 || b1[4].(xfloat) != 4.0 || b1[5].(xfloat) != 6.0 {
		t.Errorf("bucket1 = %v, want [_,2,4,2,4,6]", b1)
	}
	// bucket1 trailing order-id array.
	ids1 := b1[6].([]string)
	if len(ids1) != 2 || ids1[0] != "aaa" || ids1[1] != "bbb" {
		t.Errorf("bucket1 ids = %v, want [aaa,bbb]", ids1)
	}
	// bucket2: price = 2.0/2.0 = 1.0 (open=high=low=close), volume=2.0
	b2 := arr[2].([]interface{})
	if b2[1].(xfloat) != 1.0 || b2[2].(xfloat) != 1.0 || b2[3].(xfloat) != 1.0 || b2[4].(xfloat) != 1.0 || b2[5].(xfloat) != 2.0 {
		t.Errorf("bucket2 = %v, want [_,1,1,1,1,2]", b2)
	}
	// bucket3: zero-filled.
	b3 := arr[3].([]interface{})
	for _, v := range b3[1:6] {
		if v.(xfloat) != 0 {
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
	if m, ok := res.(map[string]interface{}); !ok || m["first_order_id"] == nil {
		t.Fatalf("dxPartialOrderChainDetails = %v (%T)", res, res)
	}

	// Missing id -> empty array (C++ returns [] for a valid but unknown order;
	// a malformed id is a uint256S null whose lookup also misses).
	unknown := strings.Repeat("0", 64)
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

	res, err := ctx.dxPartialOrderChainDetails([]json.RawMessage{jstr(dispID(mid.ID))})
	if err != nil {
		t.Fatalf("dxPartialOrderChainDetails: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("result = %v (%T)", res, res)
	}
	if m["first_order_id"] != dispID(parent.ID) {
		t.Errorf("first_order_id = %v, want %v", m["first_order_id"], dispID(parent.ID))
	}
	orders, ok := m["orders"].([]string)
	if !ok || len(orders) != 3 {
		t.Fatalf("orders = %v (%T), want 3 hex ids", m["orders"], m["orders"])
	}
	if orders[0] != dispID(parent.ID) || orders[2] != dispID(child.ID) {
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

	const btcOnly = "[Main]\nExchangeWallets=BTC\n\n[BTC]\nTitle=Bitcoin\nCreateTxMethod=BTC\nAddressPrefix=0\nScriptPrefix=5\nCOIN=100000000\nTxVersion=1\nDustAmount=546\nMinTxFee=1000\nBlockTime=600\nFeePerByte=2\nConfirmations=2\n"
	const withDoge = "[Main]\nExchangeWallets=BTC,DOGE\n\n[BTC]\nTitle=Bitcoin\nCreateTxMethod=BTC\nAddressPrefix=0\nScriptPrefix=5\nCOIN=100000000\nTxVersion=1\nDustAmount=546\nMinTxFee=1000\nBlockTime=600\nFeePerByte=2\nConfirmations=2\n\n[DOGE]\nTitle=Dogecoin\nCreateTxMethod=BTC\nAddressPrefix=30\nScriptPrefix=22\nCOIN=100000000\nTxVersion=1\nDustAmount=546\nMinTxFee=1000\nBlockTime=60\nFeePerByte=1\nConfirmations=2\n"

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
	nt, _ := ctx.dxGetNetworkTokens(nil)
	got := map[string]bool{}
	for _, tk := range nt.([]string) {
		got[tk] = true
	}
	if !got["BTC"] || !got["DOGE"] {
		t.Errorf("dxGetNetworkTokens after reload = %v", nt)
	}
}

// TestDxLoadConfHotReloadMissingPath verifies that a node started without a
// conf path (ConfPath empty) reports a clean error instead of reloading.
func TestDxLoadConfHotReloadMissingPath(t *testing.T) {
	node := &Node{config: &Config{}, store: NewStore(), signer: crypto.NewBtcSigner(), stop: make(chan struct{}), snReg: servicenode.NewRegistry()}
	ctx := &HandlerCtx{Store: NewStore(), Node: node}
	if res, rerr := ctx.dxLoadXBridgeConf(nil); rerr == nil {
		t.Fatalf("expected error for empty ConfPath, got res=%v", res)
	}
}
