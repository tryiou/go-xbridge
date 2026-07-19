package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go-xbridge/coins"
	"go-xbridge/config"
	discovery "go-xbridge/p2p/discovery"
	"go-xbridge/wallet"
)

// fillOut is one entry of dxGetOrderFills' recent-fills list. Mirrors C++'s
// 12-field object (id, time, maker, maker_size, taker, taker_size, order_type,
// partial_minimum, partial_orig_maker_size, partial_orig_taker_size,
// partial_repost, partial_parent_id).
type fillOut struct {
	ID                   string `json:"id"`
	Time                 string `json:"time"`
	Maker                string `json:"maker"`
	MakerSize            string `json:"maker_size"`
	Taker                string `json:"taker"`
	TakerSize            string `json:"taker_size"`
	OrderType            string `json:"order_type"`
	PartialMinimum       string `json:"partial_minimum"`
	PartialOrigMakerSize string `json:"partial_orig_maker_size"`
	PartialOrigTakerSize string `json:"partial_orig_taker_size"`
	PartialRepost        bool   `json:"partial_repost"`
	PartialParentID      string `json:"partial_parent_id"`
}

// ---------------------------------------------------------------------------
// dxGetOrderFills — recent filled orders (session-scoped, like C++).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetOrderFills(params []json.RawMessage) (interface{}, *rpcError) {
	maker, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderFills", "(maker) (taker) (combined, default=true)[optional]")
	}
	taker, ok := strParam(params, 1)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderFills", "(maker) (taker) (combined, default=true)[optional]")
	}
	combined, e := mustBool(params, 2, true, "dxGetOrderFills")
	if e != nil {
		return nil, e
	}

	out := []fillOut{}
	for _, f := range h.Store.Fills() {
		match := (f.Maker == maker && f.Taker == taker)
		if !match && combined {
			match = (f.Maker == taker && f.Taker == maker)
		}
		if !match {
			continue
		}
		out = append(out, fillOut{
			ID:                   f.ID,
			Time:                 iso8601(f.Time),
			Maker:                f.Maker,
			MakerSize:            f.MakerSize,
			Taker:                f.Taker,
			TakerSize:            f.TakerSize,
			OrderType:            f.OrderType,
			PartialMinimum:       f.PartialMinimum,
			PartialOrigMakerSize: f.PartialOrigMakerSize,
			PartialOrigTakerSize: f.PartialOrigTakerSize,
			PartialRepost:        f.PartialRepost,
			PartialParentID:      f.ParentID,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetOrders — all open orders (skips old cancelled/finished/expired).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetOrders(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) != 0 {
		return nil, makeError(errInvalidParameters, "dxGetOrders", "This function does not accept any parameters.")
	}
	now := NowMicro()
	out := []orderListResult{}
	for _, o := range h.Store.List() {
		switch statusString(o.Status) {
		case "canceled", "finished", "expired":
			if now-o.Updated > 60*1e6 {
				continue
			}
		}
		// Only show orders for assets we know about (mirrors the local-wallet
		// filter in C++; a thin client without a wallet shows all known coins).
		if !coins.Has(o.FromCurrency) {
			continue
		}
		if !coins.Has(o.ToCurrency) {
			continue
		}
		out = append(out, o.toListResult())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetOrder — single order by id.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetOrder(params []json.RawMessage) (interface{}, *rpcError) {
	id, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrder", "(id)")
	}
	// C++ normalizes the id via uint256S (case-insensitive); Store keys are
	// lowercased hex, so lowercase the input before lookup.
	id = strings.ToLower(id)
	o := h.Store.Get(id)
	if o == nil {
		return nil, makeError(errTxNotFound, "dxGetOrder", id)
	}
	// C++ requires a wallet session for both order currencies.
	if _, e := h.connector(o.FromCurrency); e != nil {
		return nil, e
	}
	if _, e := h.connector(o.ToCurrency); e != nil {
		return nil, e
	}
	return o.toListResult(), nil
}

// ---------------------------------------------------------------------------
// dxGetLocalTokens / dxGetNetworkTokens — supported token lists.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetLocalTokens(params []json.RawMessage) (interface{}, *rpcError) {
	return knownTokens(h.Config().ExchangeWallets), nil
}

func (h *HandlerCtx) dxGetNetworkTokens(params []json.RawMessage) (interface{}, *rpcError) {
	// C++ returns the live union of tokens servicenodes advertise
	// (walletServices()); we derive that from the connected servicenodes'
	// XbcServicesPing advertisements, falling back to config when none are
	// connected.
	if h.Node == nil {
		return knownTokens(h.Config().NetworkTokens), nil
	}
	return h.Node.NetworkTokens(), nil
}

func knownTokens(tickers []string) []string {
	out := make([]string, 0, len(tickers))
	out = append(out, tickers...)
	sort.Strings(out)
	return out
}

// connector returns the wallet connector configured for ticker, or a no-session
// business error (mirroring C++ when no wallet is loaded for that coin).
func (h *HandlerCtx) connector(ticker string) (wallet.Connector, *rpcError) {
	if h.Node == nil || h.Node.cfg() == nil || h.Node.cfg().Connectors == nil {
		return nil, makeError(errNoSession, "dx", ticker)
	}
	conn, ok := h.Node.cfg().Connectors[ticker]
	if !ok || conn == nil {
		return nil, makeError(errNoSession, "dx", ticker)
	}
	return conn, nil
}

// ---------------------------------------------------------------------------
// dxLoadXBridgeConf — returns true.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxLoadXBridgeConf(params []json.RawMessage) (interface{}, *rpcError) {
	if h.Node == nil {
		return true, nil
	}
	if err := h.Node.reloadConf(); err != nil {
		return nil, makeError(errInvalidParameters, "dxLoadXBridgeConf", err.Error())
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// dxGetNewTokenAddress — fresh address(es) for a token (requires wallet).
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// dxGetNewTokenAddress — fresh address(es) for a token (requires wallet).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetNewTokenAddress(params []json.RawMessage) (interface{}, *rpcError) {
	ticker, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetNewTokenAddress", "(ticker)")
	}
	conn, e := h.connector(ticker)
	if e != nil {
		// C++ dxGetNewTokenAddress returns an empty array (not an error) when
		// no wallet is loaded for the requested coin; mirror that here.
		return []string{}, nil
	}
	addr, err := conn.GetNewAddress()
	if err != nil {
		return nil, makeError(errUnknown, "dxGetNewTokenAddress", err.Error())
	}
	return []string{addr}, nil
}

// ---------------------------------------------------------------------------
// dxMakeOrder / dxMakePartialOrder.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxMakeOrder(params []json.RawMessage) (interface{}, *rpcError) {
	// (maker, maker_size, maker_address, taker, taker_size, taker_address, type, [use_all_funds=true], [dryrun])
	if len(params) < 7 {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "(maker) (maker_size) (maker_address) (taker) (taker_size) (taker_address) (type) (use_all_funds, default=true)[optional] (dryrun)[optional]")
	}
	maker, _ := strParam(params, 0)
	makerSize, _ := strParam(params, 1)
	makerAddr, _ := strParam(params, 2)
	taker, _ := strParam(params, 3)
	takerSize, _ := strParam(params, 4)
	takerAddr, _ := strParam(params, 5)
	typ, _ := strParam(params, 6)
	// use_all_funds is read at index 7 only when present (C++ reads it when
	// params.size() >= 8); a present-but-unparseable value is an error.
	useAll := true
	if len(params) >= 8 {
		b, ok := boolParam(params, 7, true)
		if !ok {
			return nil, makeError(errInvalidParameters, "dxMakeOrder", "invalid use_all_funds")
		}
		useAll = b
	}
	// dryrun is read as the literal string "dryrun" at index 8 only when there
	// are exactly 9 params (C++: if params.size()==9). Any other value is an
	// error, so a misspelled dryrun does not broadcast an order.
	dryRun := false
	if len(params) == 9 {
		d, ok := strParam(params, 8)
		if !ok || d != "dryrun" {
			arg := d
			if !ok {
				arg = "<invalid>"
			}
			return nil, makeError(errInvalidParameters, "dxMakeOrder", arg)
		}
		dryRun = true
	}
	// C++ only supports the "exact" type for dxMakeOrder; dxMakePartialOrder
	// handles partials.
	if typ != "exact" {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "Only the exact type is supported at this time.")
	}
	o, e := h.Node.MakeOrder(MakeOrderParams{
		Maker: maker, MakerSize: makerSize, MakerAddress: makerAddr,
		Taker: taker, TakerSize: takerSize, TakerAddress: takerAddr,
		Type: typ, UseAllFunds: useAll, DryRun: dryRun,
	})
	if e != nil {
		return nil, e
	}
	return o.makeOrderResponse(), nil
}

func (h *HandlerCtx) dxMakePartialOrder(params []json.RawMessage) (interface{}, *rpcError) {
	// (maker, maker_size, maker_address, taker, taker_size, taker_address, minimum_size, [repost=true], [use_all_funds=true], [auto_split=true], [dryrun])
	if len(params) < 7 {
		return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "(maker) (maker_size) (maker_address) (taker) (taker_size) (taker_address) (minimum_size) (repost, default=true)[optional] (use_all_funds, default=true)[optional] (auto_split, default=true)[optional] (dryrun)[optional]")
	}
	maker, _ := strParam(params, 0)
	makerSize, _ := strParam(params, 1)
	makerAddr, _ := strParam(params, 2)
	taker, _ := strParam(params, 3)
	takerSize, _ := strParam(params, 4)
	takerAddr, _ := strParam(params, 5)
	minSize, _ := strParam(params, 6)
	// C++ reads repost/use_all_funds/auto_split at indices 7/8/9 (defaults true)
	// only when present, and dryrun at index 10 only when there are exactly 11
	// params (a misspelled dryrun is an error, never a silent broadcast).
	repost := true
	if len(params) >= 8 {
		b, ok := boolParam(params, 7, true)
		if !ok {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "invalid repost")
		}
		repost = b
	}
	useAll := true
	if len(params) >= 9 {
		b, ok := boolParam(params, 8, true)
		if !ok {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "invalid use_all_funds")
		}
		useAll = b
	}
	autoSplit := true
	if len(params) >= 10 {
		b, ok := boolParam(params, 9, true)
		if !ok {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "invalid auto_split")
		}
		autoSplit = b
	}
	dryRun := false
	if len(params) == 11 {
		d, ok := strParam(params, 10)
		if !ok || d != "dryrun" {
			arg := d
			if !ok {
				arg = "<invalid>"
			}
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", arg)
		}
		dryRun = true
	}
	o, e := h.Node.MakeOrder(MakeOrderParams{
		Maker: maker, MakerSize: makerSize, MakerAddress: makerAddr,
		Taker: taker, TakerSize: takerSize, TakerAddress: takerAddr,
		Type: "partial", MinSize: minSize,
		UseAllFunds: useAll, AutoSplit: autoSplit, Repost: repost, DryRun: dryRun,
	})
	if e != nil {
		return nil, e
	}
	return o.makePartialOrderResponse(repost), nil
}

// ---------------------------------------------------------------------------
// dxTakeOrder.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxTakeOrder(params []json.RawMessage) (interface{}, *rpcError) {
	// (id, from_address, to_address, [amount], [dryrun])
	if len(params) < 3 {
		return nil, makeError(errInvalidParameters, "dxTakeOrder", "(id) (from_address) (to_address) (amount)[optional] (dryrun)[optional]")
	}
	id, _ := strParam(params, 0)
	fromAddr, _ := strParam(params, 1)
	toAddr, _ := strParam(params, 2)
	amount, _ := strParam(params, 3)
	// dryrun is read as the literal string "dryrun" at index 4 only when there
	// are exactly 5 params (C++: if params.size()==5). Any other value is an
	// error, so a misspelled dryrun does not broadcast a take.
	dryRun := false
	if len(params) == 5 {
		d, ok := strParam(params, 4)
		if !ok || d != "dryrun" {
			arg := d
			if !ok {
				arg = "<invalid>"
			}
			return nil, makeError(errInvalidParameters, "dxTakeOrder", arg)
		}
		dryRun = true
	}
	res, e := h.Node.TakeOrder(TakeOrderParams{ID: id, FromAddress: fromAddr, ToAddress: toAddr, Amount: amount, DryRun: dryRun})
	if e != nil {
		return nil, e
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// dxCancelOrder.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxCancelOrder(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) != 1 {
		return nil, makeError(errInvalidParameters, "dxCancelOrder", "(id)")
	}
	id, _ := strParam(params, 0)
	// C++ validates the id format up front (uint256S(sid).IsNull()).
	if _, err := hex.DecodeString(id); err != nil || len(id) != 64 {
		return nil, makeError(errInvalidParameters, "dxCancelOrder", "Invalid order id ["+id+"]")
	}
	id = strings.ToLower(id)
	o := h.Store.Get(id)
	if o == nil {
		return nil, makeError(errTxNotFound, "dxCancelOrder", id)
	}
	// C++ refuses to cancel once the swap has progressed to trCreated or beyond
	// (the order is already committed / in process).
	if stateOrdinal(o.Status) >= 6 {
		return nil, makeError(errInvalidState, "dxCancelOrder", "The order is already "+statusString(o.Status))
	}
	// C++ requires a wallet session for both currencies to build the result.
	if _, e := h.connector(o.FromCurrency); e != nil {
		return nil, e
	}
	if _, e := h.connector(o.ToCurrency); e != nil {
		return nil, e
	}
	res, e := h.Node.CancelOrder(CancelOrderParams{ID: id})
	if e != nil {
		return nil, e
	}
	return res.toCancelResult(), nil
}

// ---------------------------------------------------------------------------
// dxGetOrderHistory — OHLC volume series (requires historical blockchain data;
// the thin client returns an empty series).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetOrderHistory(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) < 5 || len(params) > 8 {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "(maker) (taker) (start time) (end time) (granularity) (order_ids, default=false)[optional] (with_inverse, default=false)[optional] (limit)[optional]")
	}
	maker, _ := strParam(params, 0)
	taker, _ := strParam(params, 1)
	start, ok := int64Param(params, 2)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "invalid start time")
	}
	end, ok := int64Param(params, 3)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "invalid end time")
	}
	granularity, ok := int64Param(params, 4)
	if !ok || granularity <= 0 {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "invalid granularity")
	}
	// C++ XSeries::earliestTime() = 2018-02-25 00:00:00 UTC = 1519516800 (util/xseries.h:108).
	// Requests starting before this are rejected with "Start time too early."
	const xSeriesEarliest = int64(1519516800)
	if start < xSeriesEarliest {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "Start time too early.")
	}
	orderIDs := false
	if len(params) > 5 {
		b, ok := boolParam(params, 5, false)
		if !ok {
			return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "invalid order_ids")
		}
		orderIDs = b
	}
	withInverse := false
	if len(params) > 6 {
		b, ok := boolParam(params, 6, false)
		if !ok {
			return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "invalid with_inverse")
		}
		withInverse = b
	}

	if end <= start {
		return []interface{}{}, nil
	}
	numBuckets := (end - start) / granularity

	// C++ walk of the XSeries cache is replaced here by the thin client's local
	// trade history (Store.Fills): OHLCV is aggregated from the fills this node
	// has actually seen. Network-wide XSeries history (requiring a blocknetd
	// block index) is not available to the thin client, so the data reflects
	// this node's local trades only. The wire *schema* matches C++ exactly.
	type bucket struct {
		fills []*fillEntry
	}
	buckets := make([]bucket, numBuckets)
	for i := range buckets {
		buckets[i].fills = make([]*fillEntry, 0)
	}
	allFills := h.Store.Fills()
	for i := range allFills {
		f := &allFills[i]
		match := (f.Maker == maker && f.Taker == taker)
		if !match && withInverse {
			match = (f.Maker == taker && f.Taker == maker)
		}
		if !match {
			continue
		}
		ft := int64(f.Time / 1e6)
		if ft < start || ft >= end {
			continue
		}
		bi := (ft - start) / granularity
		if bi < 0 || bi >= numBuckets {
			continue
		}
		buckets[bi].fills = append(buckets[bi].fills, f)
	}

	out := make([]interface{}, 0, numBuckets)
	for i := int64(0); i < numBuckets; i++ {
		bucketStart := start + i*granularity
		bf := buckets[i].fills
		// Chronological order so open=first, close=last.
		sort.SliceStable(bf, func(i, j int) bool { return bf[i].Time < bf[j].Time })
		var open, high, low, close, volume float64
		ids := make([]string, 0, len(bf))
		for _, f := range bf {
			makerNum, e1 := strconv.ParseFloat(f.MakerSize, 64)
			takerNum, e2 := strconv.ParseFloat(f.TakerSize, 64)
			if e1 != nil || e2 != nil || makerNum == 0 {
				continue
			}
			price := takerNum / makerNum
			if len(ids) == 0 {
				open = price
			}
			if price > high || high == 0 {
				high = price
			}
			if price < low || low == 0 {
				low = price
			}
			close = price
			volume += takerNum
			ids = append(ids, f.ID)
		}
		row := []interface{}{iso8601(uint64(bucketStart) * 1e6), low, high, open, close, volume}
		if orderIDs {
			row = append(row, ids)
		}
		out = append(out, row)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetOrderBook.
// ---------------------------------------------------------------------------

// obEntry is one matched order-book entry (ask or bid) with its computed price.
type obEntry struct {
	price    float64 // numeric price for sorting (ask=to/from, bid=from/to)
	priceStr string  // 6-decimal rendered price
	amount   uint64  // base-unit amount to render (ask=fromAmount, bid=toAmount)
	id       string  // hex order id
}

// obPriceGroup is one aggregated price level (detail 2): an aggregated size and
// the count of orders at that level across the full order book.
type obPriceGroup struct {
	priceStr string
	sum      uint64
	count    int
}

// groupByPrice aggregates a price-sorted (descending) entry list into best-first
// price levels. reverse=true (asks) yields lowest-price-first; reverse=false
// (bids) yields highest-price-first. Prices are grouped by their 6-decimal
// rendered string, matching C++'s floatCompare equality.
func groupByPrice(list []obEntry, reverse bool) []obPriceGroup {
	groups := []obPriceGroup{}
	var cur *obPriceGroup
	add := func(e obEntry) {
		if cur == nil || cur.priceStr != e.priceStr {
			if cur != nil {
				groups = append(groups, *cur)
			}
			g := obPriceGroup{priceStr: e.priceStr, sum: e.amount, count: 1}
			cur = &g
		} else {
			cur.sum += e.amount
			cur.count++
		}
	}
	if reverse {
		for i := len(list) - 1; i >= 0; i-- {
			add(list[i])
		}
	} else {
		for i := 0; i < len(list); i++ {
			add(list[i])
		}
	}
	if cur != nil {
		groups = append(groups, *cur)
	}
	return groups
}

func countAtPrice(list []obEntry, priceStr string) int {
	n := 0
	for _, e := range list {
		if e.priceStr == priceStr {
			n++
		}
	}
	return n
}

func idsAtPrice(list []obEntry, priceStr string) []string {
	seen := map[string]bool{}
	ids := []string{}
	for _, e := range list {
		if e.priceStr == priceStr && !seen[e.id] {
			seen[e.id] = true
			ids = append(ids, e.id)
		}
	}
	return ids
}

func (h *HandlerCtx) dxGetOrderBook(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) < 3 || len(params) > 4 {
		return nil, makeError(errInvalidParameters, "dxGetOrderBook", "(detail, 1-4) (maker) (taker) (max_orders, default=50)[optional]")
	}
	detail, e := mustInt(params, 0, 1, "dxGetOrderBook")
	if e != nil {
		return nil, e
	}
	if detail < 1 || detail > 4 {
		return nil, makeError(errInvalidDetailLevel, "dxGetOrderBook", "")
	}
	maker, ok := strParam(params, 1)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderBook", "(detail, 1-4) (maker) (taker) (max_orders, default=50)[optional]")
	}
	taker, ok := strParam(params, 2)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderBook", "(detail, 1-4) (maker) (taker) (max_orders, default=50)[optional]")
	}
	maxOrders := 50
	if len(params) == 4 {
		maxOrders, e = mustInt(params, 3, 50, "dxGetOrderBook")
		if e != nil {
			return nil, e
		}
	}
	if maxOrders < 1 {
		maxOrders = 1
	}

	// Collect matching asks/bids. C++ runs two independent filter passes (an
	// order can land in both when maker==taker currency), so use two ifs.
	asks := []obEntry{}
	bids := []obEntry{}
	for _, o := range h.Store.List() {
		if o.Status != "open" {
			continue
		}
		if o.FromAmount <= 0 || o.ToAmount <= 0 {
			continue
		}
		if strings.EqualFold(o.FromCurrency, maker) && strings.EqualFold(o.ToCurrency, taker) {
			// ask: from=maker (sold), to=taker; price = to/from (taker per maker)
			p := float64(o.ToAmount) / float64(o.FromAmount)
			asks = append(asks, obEntry{price: p, priceStr: formatXPrice(p), amount: o.FromAmount, id: hexEncode(o.ID[:])})
		}
		if strings.EqualFold(o.FromCurrency, taker) && strings.EqualFold(o.ToCurrency, maker) {
			// bid: from=taker, to=maker; priceBid = from/to (taker per maker)
			p := float64(o.FromAmount) / float64(o.ToAmount)
			bids = append(bids, obEntry{price: p, priceStr: formatXPrice(p), amount: o.ToAmount, id: hexEncode(o.ID[:])})
		}
	}

	// Sort both sides descending by price: best bid is at the front (highest),
	// best ask is at the back (lowest).
	sort.Slice(asks, func(i, j int) bool { return asks[i].price > asks[j].price })
	sort.Slice(bids, func(i, j int) bool { return bids[i].price > bids[j].price })

	// Initialize the side arrays to empty (non-nil) slices so they serialize
	// as "[]" rather than "null", matching C++ dxGetOrderBook which emits
	// default-constructed Array objects for an empty book
	// (rpcxbridge.cpp:1568-1576).
	res := orderBookResult{Detail: detail, Maker: maker, Taker: taker,
		Asks: [][]interface{}{}, Bids: [][]interface{}{}}

	switch detail {
	case 1:
		// Best bid and ask only, with the count of orders at that best price.
		if len(asks) > 0 {
			best := asks[len(asks)-1] // lowest-price ask
			res.Asks = append(res.Asks, []interface{}{best.priceStr, formatXAmount(best.amount), countAtPrice(asks, best.priceStr)})
		}
		if len(bids) > 0 {
			best := bids[0] // highest-price bid
			res.Bids = append(res.Bids, []interface{}{best.priceStr, formatXAmount(best.amount), countAtPrice(bids, best.priceStr)})
		}
	case 2:
		// Aggregated top levels (per side), best-first, capped at maxOrders.
		for _, g := range groupByPrice(asks, true) {
			if len(res.Asks) >= maxOrders {
				break
			}
			res.Asks = append(res.Asks, []interface{}{g.priceStr, formatXAmount(g.sum), g.count})
		}
		for _, g := range groupByPrice(bids, false) {
			if len(res.Bids) >= maxOrders {
				break
			}
			res.Bids = append(res.Bids, []interface{}{g.priceStr, formatXAmount(g.sum), g.count})
		}
	case 3:
		// Full, non-aggregated, per side, capped at maxOrders, with order ids.
		for i := len(asks) - 1; i >= 0 && len(res.Asks) < maxOrders; i-- {
			e := asks[i]
			res.Asks = append(res.Asks, []interface{}{e.priceStr, formatXAmount(e.amount), e.id})
		}
		for i := 0; i < len(bids) && len(res.Bids) < maxOrders; i++ {
			e := bids[i]
			res.Bids = append(res.Bids, []interface{}{e.priceStr, formatXAmount(e.amount), e.id})
		}
	case 4:
		// Best bid and ask only, with the array of order ids at that price.
		if len(asks) > 0 {
			best := asks[len(asks)-1]
			res.Asks = append(res.Asks, []interface{}{best.priceStr, formatXAmount(best.amount), idsAtPrice(asks, best.priceStr)})
		}
		if len(bids) > 0 {
			best := bids[0]
			res.Bids = append(res.Bids, []interface{}{best.priceStr, formatXAmount(best.amount), idsAtPrice(bids, best.priceStr)})
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// dxGetTokenBalances — per-token balances (requires wallet; empty when none).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetTokenBalances(params []json.RawMessage) (interface{}, *rpcError) {
	out := map[string]string{}
	if h.Node == nil || h.Node.cfg() == nil || h.Node.cfg().Connectors == nil {
		return out, nil
	}
	// UTXOs locked by active orders (keyed "txid:vout") are subtracted from each
	// balance so the figure mirrors C++'s spendable (non-locked) funds.
	keys, _ := h.Store.LockedUtxoInfo()
	lockedOf := func(utxos []wallet.Utxo) uint64 {
		var l uint64
		for _, u := range utxos {
			k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
			if keys[k] {
				l += u.Amount
			}
		}
		return l
	}
	// C++ dxGetTokenBalances always emits a "Wallet" key: the available balance of
	// the native BLOCK coin used to pay service-node fees (availableBalance()/COIN,
	// fixed-6 XBridge scale). Derive it from the BLOCK connector when loaded;
	// otherwise fall back to the first configured exchange wallet so the key stays
	// present.
	var walletTicker, walletBalance string
	if c, ok := coins.Get("BLOCK"); ok {
		if conn, ok := h.Node.cfg().Connectors["BLOCK"]; ok && conn != nil {
			if utxos, err := conn.ListUnspent(0); err == nil {
				var total uint64
				for _, u := range utxos {
					total += u.Amount
				}
				if l := lockedOf(utxos); l < total {
					total -= l
				}
				walletTicker, walletBalance = "BLOCK", formatBalanceNative(c, total)
			}
		}
	}
	for _, ticker := range h.Node.cfg().ExchangeWallets {
		conn, ok := h.Node.cfg().Connectors[ticker]
		if !ok || conn == nil {
			continue
		}
		utxos, err := conn.ListUnspent(0)
		if err != nil {
			continue
		}
		var total uint64
		for _, u := range utxos {
			total += u.Amount
		}
		if c, ok := coins.Get(ticker); ok {
			// C++ renders per-connector balances in fixed-6 XBridge scale
			// (xBridgeStringValueFromPrice on the COIN-divided wallet balance),
			// not native per-coin decimals, and subtracts UTXOs locked by pending
			// orders so the figure reflects spendable funds.
			avail := total
			if l := lockedOf(utxos); l < avail {
				avail -= l
			}
			bal := formatBalanceNative(c, avail)
			out[ticker] = bal
			if walletTicker == "" {
				walletTicker = ticker
				walletBalance = bal
			}
		}
	}
	if walletTicker != "" {
		out["Wallet"] = walletBalance
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetMyOrders — orders created locally by this node.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetMyOrders(params []json.RawMessage) (interface{}, *rpcError) {
	out := []orderDetailResult{}
	for _, o := range h.Store.Mine() {
		out = append(out, o.toDetailResult())
	}
	// C++ dxGetMyOrders sorts ascending by txtime (updated time).
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt < out[j].UpdatedAt
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetMyPartialOrderChain — the partial-repost chain rooted at order_id.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetMyPartialOrderChain(params []json.RawMessage) (interface{}, *rpcError) {
	id, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetMyPartialOrderChain", "(order_id)")
	}
	// C++ getPartialOrderChain resolves both ancestors and descendants; reuse the
	// shared chain walker so this matches dxPartialOrderChainDetails.
	chain := h.partialOrderChain(id)
	if len(chain) == 0 {
		return nil, makeError(errTxNotFound, "dxGetMyPartialOrderChain", id)
	}
	out := make([]orderDetailResult, 0, len(chain))
	for _, c := range chain {
		out = append(out, c.toDetailResult())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxPartialOrderChainDetails — aggregate details for a partial chain.
// ---------------------------------------------------------------------------

// partialOrderChain returns the full partial order chain for id — the root
// (oldest ancestor) first, then the queried order, then all descendants,
// matching C++ xbridge::App::getPartialOrderChain.
func (h *HandlerCtx) partialOrderChain(id string) []*Order {
	cur := h.Store.Get(id)
	if cur == nil {
		return nil
	}
	chain := []*Order{cur}
	seen := map[string]bool{id: true}
	// Walk up to the root via ParentID.
	for !isZeroID(cur.ParentID) {
		pid := hexEncode(cur.ParentID[:])
		p := h.Store.Get(pid)
		if p == nil || seen[pid] {
			break
		}
		chain = append([]*Order{p}, chain...)
		seen[pid] = true
		cur = p
	}
	// Index parent -> children and walk down to descendants.
	children := map[string][]string{}
	for _, o := range h.Store.List() {
		oid := hexEncode(o.ID[:])
		if isZeroID(o.ParentID) {
			continue
		}
		children[hexEncode(o.ParentID[:])] = append(children[hexEncode(o.ParentID[:])], oid)
	}
	var descend func(pid string)
	descend = func(pid string) {
		for _, cid := range children[pid] {
			if seen[cid] {
				continue
			}
			c := h.Store.Get(cid)
			if c == nil {
				continue
			}
			chain = append(chain, c)
			seen[cid] = true
			descend(cid)
		}
	}
	// Descend from every node already in the chain (the queried order sits
	// between its ancestors and descendants, so both directions must be walked).
	for _, c := range chain {
		descend(hexEncode(c.ID[:]))
	}
	return chain
}

func (h *HandlerCtx) dxPartialOrderChainDetails(params []json.RawMessage) (interface{}, *rpcError) {
	id, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxPartialOrderChainDetails", "(order_id)")
	}
	// C++ validates the order id up front (uint256S(sid).IsNull()).
	if _, err := hex.DecodeString(id); err != nil || len(id) != 64 {
		return nil, makeError(errInvalidParameters, "dxPartialOrderChainDetails", "Invalid order id ["+id+"]")
	}
	id = strings.ToLower(id)
	chain := h.partialOrderChain(id)
	// C++ returns an empty object `{}` for an unknown / empty chain (not an error).
	if len(chain) == 0 {
		return map[string]interface{}{}, nil
	}
	first := chain[0]
	last := chain[len(chain)-1]

	var totalSent, totalReceived, totalNotSent, totalNotReceived uint64
	totalOpen, totalFinished, totalCanceled := 0, 0, 0
	orderIDs := make([]string, 0, len(chain))
	deposits := make([]string, 0, len(chain))
	counter := make([]string, 0, len(chain))
	for _, t := range chain {
		switch t.Status {
		case "finished":
			totalSent += t.FromAmount
			totalReceived += t.ToAmount
			totalFinished++
		case "canceled":
			totalNotSent += t.FromAmount
			totalNotReceived += t.ToAmount
			totalCanceled++
		default:
			totalNotSent += t.FromAmount
			totalNotReceived += t.ToAmount
			// C++ counts state <= trPending (expired/new/offline/pending) as open;
			// matching that here rather than only the literal "open" string.
			if stateOrdinal(t.Status) <= 2 {
				totalOpen++
			}
		}
		orderIDs = append(orderIDs, hexEncode(t.ID[:]))
		// C++ pushes each order's deposit txids (binTxId / oBinTxId) into these
		// arrays so a caller can see the on-chain HTLC deposits for the chain.
		if t.BinTxId != "" {
			deposits = append(deposits, t.BinTxId)
		}
		if t.OBinTxId != "" {
			counter = append(counter, t.OBinTxId)
		}
	}
	details := map[string]interface{}{
		"first_order_id":             hexEncode(first.ID[:]),
		"maker":                      first.FromCurrency,
		"maker_address":              first.MakerAddress,
		"taker":                      first.ToCurrency,
		"taker_address":              first.TakerAddress,
		"partial_minimum":            formatXAmount(first.MinFromAmount),
		"partial_orig_maker_size":    formatXAmount(first.OrigFromAmount),
		"partial_orig_taker_size":    formatXAmount(first.OrigToAmount),
		"first_order_time":           iso8601(first.Created),
		"last_order_time":            iso8601(last.Updated),
		"total_reported_sent":        formatXAmount(totalSent),
		"total_reported_received":    formatXAmount(totalReceived),
		"total_reported_notsent":     formatXAmount(totalNotSent),
		"total_reported_notreceived": formatXAmount(totalNotReceived),
		"total_orders_open":          totalOpen,
		"total_orders_finished":      totalFinished,
		"total_orders_canceled":      totalCanceled,
		"orders":                     orderIDs,
		"p2sh_deposits":              deposits,
		"p2sh_deposits_counterparty": counter,
	}
	return details, nil
}

// ---------------------------------------------------------------------------
// dxGetLockedUtxos — currently locked orders.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetLockedUtxos(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) > 1 {
		return nil, makeError(errInvalidParameters, "dxGetLockedUtxos", "Too many parameters.")
	}
	// C++ gates on the Exchange (Service Node) being started; a thin client with
	// no configured exchange wallets cannot serve locked-utxo data. Guard the nil
	// Node/Config so an unconfigured handler returns the business error instead of
	// panicking.
	if h.Node == nil || h.Node.cfg() == nil || len(h.Node.cfg().ExchangeWallets) == 0 {
		return nil, makeError(errNotExchangeNode, "dxGetLockedUtxos", "not an exchange node")
	}
	keys, byOrder := h.Store.LockedUtxoInfo()
	id, _ := strParam(params, 0)
	if id == "" {
		// No id -> all locked utxos across the configured exchange wallets,
		// rendered as C++ Exchange::getUtxoItems "txid:vout:amount:address" strings
		// in fixed-6 XBridge scale.
		all := make([]string, 0)
		for _, ticker := range h.Node.cfg().ExchangeWallets {
			conn, e := h.connector(ticker)
			if e != nil {
				continue
			}
			utxos, err := conn.ListUnspent(0)
			if err != nil {
				continue
			}
			c, cok := coins.Get(ticker)
			for _, u := range utxos {
				k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
				if !keys[k] {
					continue
				}
				amtStr := formatXAmount(u.Amount)
				if cok {
					amtStr = coins.FormatAmount(c, u.Amount)
				}
				all = append(all, u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10)+":"+amtStr+":"+u.Address)
			}
		}
		return map[string]interface{}{"all_locked_utxo": all}, nil
	}
	o := h.Store.Get(id)
	if o == nil {
		return nil, makeError(errTxNotFound, "dxGetLockedUtxos", id)
	}
	// Per-order locked utxos, keyed by the order's maker currency and restricted to
	// UTXOs locked by THIS order (C++ keys the array by the order's currency).
	entries := make([]string, 0)
	if conn, e := h.connector(o.FromCurrency); e == nil {
		utxos, err := conn.ListUnspent(0)
		if err == nil {
			c, cok := coins.Get(o.FromCurrency)
			for _, u := range utxos {
				k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
				if byOrder[k] != id {
					continue
				}
				amtStr := formatXAmount(u.Amount)
				if cok {
					amtStr = coins.FormatAmount(c, u.Amount)
				}
				entries = append(entries, u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10)+":"+amtStr+":"+u.Address)
			}
		}
	}
	return map[string]interface{}{
		"id":           id,
		o.FromCurrency: entries,
	}, nil
}

// ---------------------------------------------------------------------------
// dxFlushCancelledOrders.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxFlushCancelledOrders(params []json.RawMessage) (interface{}, *rpcError) {
	// C++ dxFlushCancelledOrders: ageMillis is optional (default 0, must be >= 0).
	// params.size() == 0 -> 0; == 1 -> params[0]; > 1 -> -1 -> error.
	var ageMillis int
	switch {
	case len(params) == 0:
		ageMillis = 0
	case len(params) == 1:
		v, ok := intParam(params, 0, 0)
		if !ok {
			return nil, makeError(errInvalidParameters, "dxFlushCancelledOrders", "ageMillis must be an integer >= 0")
		}
		ageMillis = v
	default:
		return nil, makeError(errInvalidParameters, "dxFlushCancelledOrders", "ageMillis must be an integer >= 0")
	}
	if ageMillis < 0 {
		return nil, makeError(errInvalidParameters, "dxFlushCancelledOrders", "ageMillis must be an integer >= 0")
	}
	now := NowMicro()
	flushed := h.Store.FlushCancelled(uint64(ageMillis))
	dur := NowMicro() - now
	orders := make([]map[string]interface{}, 0, len(flushed))
	for _, c := range flushed {
		orders = append(orders, map[string]interface{}{
			"id":        c.ID,
			"txtime":    iso8601(c.Txtime),
			"use_count": c.UseCount,
		})
	}
	return map[string]interface{}{
		"ageMillis":        ageMillis,
		"now":              iso8601(now),
		"durationMicrosec": int64(dur),
		"flushedOrders":    orders,
	}, nil
}

// ---------------------------------------------------------------------------
// dxGetTradingData — trade history (requires blockchain data).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetTradingData(params []json.RawMessage) (interface{}, *rpcError) {
	// (blocks, default=43200) (errors, default=false). C++ reads BLOCK blockchain
	// blocks and parses on-chain trade-fee transactions; a thin client has no
	// blocknetd block index, so we surface the same 8-field record schema built
	// from the local fills this node has seen. `blocks`/`errors` are accepted for
	// contract compatibility but cannot bound a BLOCK block scan here.
	if len(params) > 2 {
		return nil, makeError(errInvalidParameters, "dxGetTradingData", "(blocks, default=43200)[optional] (errors, default=false)[optional]")
	}
	out := make([]interface{}, 0)
	for _, f := range h.Store.Fills() {
		takerSize, _ := strconv.ParseFloat(f.TakerSize, 64)
		makerSize, _ := strconv.ParseFloat(f.MakerSize, 64)
		out = append(out, map[string]interface{}{
			"timestamp":  int64(f.Time / 1e6),
			"fee_txid":   "",
			"nodepubkey": "",
			"id":         f.ID,
			"taker":      f.Taker,
			"taker_size": takerSize,
			"maker":      f.Maker,
			"maker_size": makerSize,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxSplitAddress / dxSplitInputs — UTXO splitting (requires wallet).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxSplitAddress(params []json.RawMessage) (interface{}, *rpcError) {
	// (token, splitamount, address, include_fees[default=true], show_rawtx[default=false], submit[default=true])
	if len(params) < 3 || len(params) > 6 {
		return nil, makeError(errInvalidParameters, "dxSplitAddress", "(token) (splitamount) (address) (include_fees, default=true)[optional] (show_rawtx, default=false)[optional] (submit, default=true)[optional]")
	}
	ticker, _ := strParam(params, 0)
	splitAmt, _ := strParam(params, 1)
	address, _ := strParam(params, 2)
	includeFees, e := mustBool(params, 3, true, "dxSplitAddress")
	if e != nil {
		return nil, e
	}
	showRawTx, e := mustBool(params, 4, false, "dxSplitAddress")
	if e != nil {
		return nil, e
	}
	submit, e := mustBool(params, 5, true, "dxSplitAddress")
	if e != nil {
		return nil, e
	}
	return h.splitTx(ticker, splitAmt, address, includeFees, showRawTx, submit, nil)
}

func (h *HandlerCtx) dxSplitInputs(params []json.RawMessage) (interface{}, *rpcError) {
	// C++ advertises 3-7 params (rpcxbridge.cpp:3295) but then reads params[3]
	// through params[6] unconditionally via get_bool()/get_array(); on a missing
	// index UniValue::operator[] yields NullUniValue and get_bool()/get_array()
	// throw. So C++ only actually succeeds with all 7 params present — the 3-6
	// range throws a generic type error at the first missing param. We therefore
	// require exactly 7: (token, splitamount, address, include_fees, show_rawtx,
	// submit, utxos).
	if len(params) != 7 {
		return nil, makeError(errInvalidParameters, "dxSplitInputs", "(token) (splitamount) (address) (include_fees) (show_rawtx) (submit) (utxos)")
	}
	ticker, _ := strParam(params, 0)
	splitAmt, _ := strParam(params, 1)
	address, _ := strParam(params, 2)
	includeFees, e := mustBool(params, 3, false, "dxSplitInputs")
	if e != nil {
		return nil, e
	}
	showRawTx, e := mustBool(params, 4, false, "dxSplitInputs")
	if e != nil {
		return nil, e
	}
	submit, e := mustBool(params, 5, false, "dxSplitInputs")
	if e != nil {
		return nil, e
	}
	c, ok := coins.Get(ticker)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxSplitInputs", "unknown coin: "+ticker)
	}
	utxos, e := parseUtxoParam(c, params[6])
	if e != nil {
		return nil, e
	}
	if len(utxos) == 0 {
		return nil, makeError(errBadRequest, "dxSplitInputs", "No utxos were specified")
	}
	return h.splitTx(ticker, splitAmt, address, includeFees, showRawTx, submit, utxos)
}

// splitTx builds, signs and optionally submits a UTXO-split transaction for the
// given coin, mirroring C++ dxSplitAddress/dxSplitInputs:
//   - each split output sends `splitAmount` (and +fee when include_fees) to address;
//   - change returns to a fresh address;
//   - the result echoes C++'s 8-field object
//     {token, include_fees, split_amount_requested, split_amount_with_fees,
//     split_utxo_count, split_total, txid, rawtx}.
//
// Amounts are rendered in XBridge 1e6 scale (formatXAmount); the tx itself is
// built in the coin's native scale. txid is the double-SHA256 of the signed
// transaction, byte-reversed — always computed, even when submit=false.
func (h *HandlerCtx) splitTx(ticker, splitAmountStr, address string, includeFees, showRawTx, submit bool, utxos []wallet.Utxo) (interface{}, *rpcError) {
	conn, e := h.connector(ticker)
	if e != nil {
		return nil, e
	}
	c, ok := coins.Get(ticker)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxSplit", "unknown coin: "+ticker)
	}
	// C++ parses splitamount via xBridgeAmountFromString (1e6 scale); the real tx
	// output values are converted to the coin's native scale.
	targetXB, err := parseXAmount(splitAmountStr)
	if err != nil {
		return nil, makeError(errInvalidParameters, "dxSplit", "invalid split amount")
	}
	target := fromXBridgeAmt(c, targetXB)
	cc, _ := h.Node.cfg().Confs[ticker]

	// C++ dust gate on the minimum split amount.
	relayFee, _ := conn.GetRelayFee()
	if cc != nil && targetXB < effectiveDust(cc, relayFee) {
		return nil, makeError(errBadRequest, "dxSplit", "split amount is dust ["+formatXAmount(targetXB)+"]")
	}

	if len(utxos) == 0 {
		minConf := 0
		if cc != nil {
			minConf = cc.Confirmations
		}
		utxos, err = conn.ListUnspent(minConf)
		if err != nil {
			return nil, makeError(errUnknown, "dxSplit", err.Error())
		}
	}
	if len(utxos) == 0 {
		return nil, makeError(errInsufficientFunds, "dxSplit", "no UTXOs to split")
	}

	destScript, e := legacyOutputScript(c, address)
	if e != nil {
		return nil, e
	}
	changeAddr, err := conn.GetNewAddress()
	if err != nil {
		return nil, makeError(errUnknown, "dxSplit", err.Error())
	}
	changeScript, e := legacyOutputScript(c, changeAddr)
	if e != nil {
		return nil, e
	}

	var total uint64
	var prevTxs []wallet.PrevTx
	for _, u := range utxos {
		total += u.Amount
		prevTxs = append(prevTxs, wallet.PrevTx{TxID: u.TxID, Vout: u.Vout, ScriptPubKey: u.ScriptPubKey, Amount: u.Amount})
	}

	// Fee estimate (fallback to conf FeePerByte when estimatesmartfee is absent).
	fee := estimateFee(cc, len(utxos), 2)

	// Per-output size matches C++: splitAmount, plus fee when include_fees.
	splitSize := target
	if includeFees {
		splitSize += fee
	}
	nSplits := total / splitSize
	if nSplits > 100 {
		nSplits = 100
	}
	if nSplits == 0 {
		return nil, makeError(errInsufficientFunds, "dxSplit", "insufficient funds for split amount")
	}

	spent := nSplits * splitSize
	change := uint64(0)
	if total > spent {
		change = total - spent
	}
	// Dust change is dropped (C++ claws it back into fees); keeps the tx relayable.
	if cc != nil && change < effectiveDust(cc, relayFee) {
		change = 0
	}

	tx := &coins.Tx{Version: 1}
	if cc != nil && cc.TxVersion != 0 {
		tx.Version = int32(cc.TxVersion)
	}
	for _, u := range utxos {
		hash, err := reverseTxidHex(u.TxID)
		if err != nil {
			return nil, makeError(errInvalidParameters, "dxSplit", "bad utxo txid: "+u.TxID)
		}
		tx.Inputs = append(tx.Inputs, coins.TxIn{
			PrevOut:  coins.OutPoint{Hash: hash, Index: u.Vout},
			Sequence: 0xffffffff,
		})
	}
	for i := uint64(0); i < nSplits; i++ {
		tx.Outputs = append(tx.Outputs, coins.TxOut{Value: splitSize, ScriptPubKey: destScript})
	}
	if change > 0 {
		tx.Outputs = append(tx.Outputs, coins.TxOut{Value: change, ScriptPubKey: changeScript})
	}

	unsigned := hex.EncodeToString(tx.Serialize())
	signedHex, complete, err := conn.SignRawTransaction(unsigned, prevTxs)
	if err != nil {
		return nil, makeError(errUnknown, "dxSplit", err.Error())
	}
	if !complete {
		return nil, makeError(errUnknown, "dxSplit", "signing incomplete (wallet missing keys?)")
	}

	txid, e := txidOfRawTx(signedHex)
	if e != nil {
		return nil, e
	}

	rawtx := ""
	if showRawTx {
		rawtx = signedHex
	}
	if submit {
		if _, err := conn.SendRawTransaction(signedHex); err != nil {
			return nil, makeError(errUnknown, "dxSplit", err.Error())
		}
	}

	return map[string]interface{}{
		"token":                  ticker,
		"include_fees":           includeFees,
		"split_amount_requested": formatXAmount(targetXB),
		"split_amount_with_fees": formatXAmount(toXBridgeAmt(c, splitSize)),
		"split_utxo_count":       int(nSplits),
		"split_total":            formatXAmount(toXBridgeAmt(c, total)),
		"txid":                   txid,
		"rawtx":                  rawtx,
	}, nil
}

// cppDustFallback is C++'s own dust fallback constant (xbridgewalletconnectorbtc.cpp:1526):
// when no relay fee is available, C++ uses 5460 base units. go-xbridge gathers
// the relay fee live from the wallet's getinfo.relayfee (C++ :74-76); when that
// is unavailable it falls back to the conf `DustAmount` key, and finally to this
// C++-defined constant.
const cppDustFallback = 5460

// effectiveDust returns the minimum non-dust amount (base units) for a coin.
// It mirrors C++ exactly (xbridgewalletconnectorbtc.cpp:1526):
// dustAmount = relayFee>0 ? 0.546*relayFee*COIN : 5460. The conf `DustAmount`
// key is a secondary override and 5460 the final fallback, matching C++'s order.
func effectiveDust(cc *config.CoinConf, relayFee float64) uint64 {
	if relayFee > 0 {
		return uint64(0.546 * relayFee * float64(cc.Coin))
	}
	if cc != nil && cc.DustAmount > 0 {
		return cc.DustAmount
	}
	return cppDustFallback
}

// estimateFee returns the fee (base units) for a tx with nIn inputs and nOut
// outputs. It mirrors C++'s minTxFee1/minTxFee2 (xbridgewalletconnectorbtc.cpp:1948-1969):
// fee = (192*nIn + 34*nOut) * FeePerByte (FeePerByte from xbridge.conf [TICKER]),
// floored at MinTxFee. No estimatesmartfee/estimatefee RPC is used, matching C++.
func estimateFee(cc *config.CoinConf, nIn, nOut int) uint64 {
	// Virtual-size estimate matches C++ xbridgewalletconnectorbtc.cpp:1948
	// (192 bytes per legacy input, 34 per output). Modeling inputs at 192 keeps
	// Go-built deposits inside C++'s counterpartyFees >= fee*0.95 acceptance band,
	// so a C++ counterparty accepts our orders.
	vsize := 192*nIn + 34*nOut
	var fee uint64
	if cc == nil || cc.FeePerByte == 0 {
		// 2 sat/vB default when the connector reports no fee rate.
		fee = uint64(vsize * 2)
	} else {
		fee = cc.FeePerByte * uint64(vsize)
	}
	// C++ floors every fee at minTxFee in minTxFee1/minTxFee2
	// (xbridgewalletconnectorbtc.cpp:1952,1968); apply the same floor.
	if cc != nil && cc.MinTxFee > 0 && fee < cc.MinTxFee {
		fee = cc.MinTxFee
	}
	return fee
}

// legacyOutputScript builds a P2PKH/P2SH output script for addr. Native segwit
// destinations are rejected in A1 (BIP143 signing not yet implemented).
func legacyOutputScript(c coins.Coin, addr string) ([]byte, *rpcError) {
	a, err := c.DecodeAddress(addr)
	if err != nil {
		return nil, makeError(errInvalidAddress, "dxSplit", addr)
	}
	var h [20]byte
	copy(h[:], a.Hash)
	switch a.Kind {
	case coins.P2PKH:
		return coins.BuildP2PKHScript(h), nil
	case coins.P2SH:
		return coins.BuildP2SHScript(h), nil
	default:
		return nil, makeError(errInvalidAddress, "dxSplit", "segwit destinations not supported for split in A1: "+addr)
	}
}

// reverseTxidHex converts a display-order txid hex into the 32-byte internal
// (little-endian) form Bitcoin uses in outpoints.
func reverseTxidHex(s string) ([32]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return [32]byte{}, fmt.Errorf("bad txid %q", s)
	}
	var out [32]byte
	for i := 0; i < 32; i++ {
		out[i] = b[31-i]
	}
	return out, nil
}

// txidOfRawTx computes a transaction id from serialized (signed) raw tx hex:
// double-SHA256 then byte-reversed (Bitcoin display order). This is identical
// to how C++ derives txid from a signed tx.
func txidOfRawTx(rawHex string) (string, *rpcError) {
	b, err := hex.DecodeString(rawHex)
	if err != nil {
		return "", makeError(errInvalidParameters, "dxSplit", "bad signed transaction")
	}
	h := sha256.Sum256(b)
	h = sha256.Sum256(h[:])
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return hex.EncodeToString(h[:]), nil
}

// toXBridgeAmt converts a native-chain amount (coins.Coin.Decimals base units)
// into XBridge 1e6-scale base units for display. Raw tx output values stay in
// native scale; only response amount strings use this conversion.
func toXBridgeAmt(c coins.Coin, native uint64) uint64 {
	nc := uint64(1)
	for i := 0; i < c.Decimals; i++ {
		nc *= 10
	}
	if nc == 0 {
		nc = 1
	}
	return native * coinScale / nc
}

// fromXBridgeAmt converts an XBridge 1e6-scale amount into the coin's native
// base units for use as a real on-chain output value.
func fromXBridgeAmt(c coins.Coin, xb uint64) uint64 {
	nc := uint64(1)
	for i := 0; i < c.Decimals; i++ {
		nc *= 10
	}
	if nc == 0 {
		nc = 1
	}
	return xb * nc / coinScale
}

// parseUtxoParam parses the explicit-utxos array argument of dxSplitInputs.
func parseUtxoParam(c coins.Coin, raw json.RawMessage) ([]wallet.Utxo, *rpcError) {
	var arr []struct {
		TxID         string `json:"txid"`
		Vout         uint32 `json:"vout"`
		Amount       string `json:"amount"`
		ScriptPubKey string `json:"scriptPubKey"`
		Address      string `json:"address"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, makeError(errInvalidParameters, "dxSplitInputs", "invalid utxos array")
	}
	out := make([]wallet.Utxo, 0, len(arr))
	for _, x := range arr {
		amt, err := coins.ParseAmount(c, x.Amount)
		if err != nil {
			return nil, makeError(errInvalidParameters, "dxSplitInputs", "invalid utxo amount")
		}
		out = append(out, wallet.Utxo{TxID: x.TxID, Vout: x.Vout, Address: x.Address, Amount: amt, ScriptPubKey: x.ScriptPubKey})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetUtxos — UTXOs for a token (requires wallet).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetUtxos(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) < 1 || len(params) > 2 {
		return nil, makeError(errInvalidParameters, "dxGetUtxos", "(token) (include_used, default=false)[optional]")
	}
	ticker, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetUtxos", "(token) (include_used, default=false)[optional]")
	}
	// include_used defaults to false (C++: excluded locked UTXOs unless true).
	includeUsed := false
	if len(params) >= 2 {
		b, ok := boolParam(params, 1, false)
		if !ok {
			return nil, makeError(errInvalidParameters, "dxGetUtxos", "invalid include_used")
		}
		includeUsed = b
	}
	conn, e := h.connector(ticker)
	if e != nil {
		return nil, e
	}
	minConf := 0
	if cc, ok := h.Node.cfg().Confs[ticker]; ok {
		minConf = cc.Confirmations
	}
	utxos, err := conn.ListUnspent(minConf)
	if err != nil {
		return nil, makeError(errUnknown, "dxGetUtxos", err.Error())
	}
	c, _ := coins.Get(ticker)
	keys, byOrder := h.Store.LockedUtxoInfo()
	out := make([]map[string]interface{}, 0, len(utxos))
	for _, u := range utxos {
		k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
		// C++ excludes UTXOs locked by pending orders unless include_used=true,
		// and always emits "orderid" — the id of the order locking the UTXO, or ""
		// when it is unused. LockedUtxoInfo provides both the reserved set and the
		// per-UTXO order-id map.
		if !includeUsed && keys[k] {
			continue
		}
		orderid := ""
		if byOrder[k] != "" {
			orderid = byOrder[k]
		}
		out = append(out, map[string]interface{}{
			"txid":          u.TxID,
			"vout":          u.Vout,
			"address":       u.Address,
			"amount":        coins.FormatAmount(c, u.Amount),
			"scriptPubKey":  u.ScriptPubKey,
			"confirmations": u.Confirmations,
			"orderid":       orderid,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// getnetworkinfo — standard Bitcoin-core-style RPC that BLOCK-DX pings
// (via its getinfo() wrapper) for its wallet-version gate before it will talk
// to the wallet. We advertise a Blocknet version/subversion (configurable via
// -walletversion/-walletversionstr, defaulting to 4.4.1) and the live peer
// count so the dapp reports the wallet as connected. Only the fields BLOCK-DX
// actually reads (version, subversion, connections) are meaningful; the rest
// mirror bitcoind's shape for compatibility.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) getNetworkInfo(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) != 0 {
		return nil, makeError(errInvalidParameters, "getnetworkinfo", "no parameters")
	}
	ver := h.Config().WalletVersion
	if ver == 0 {
		ver = 4040100
	}
	sub := h.Config().WalletVersionStr
	if sub == "" {
		sub = "/blocknet:4.4.1/"
	}
	conns := 0
	if h.Node != nil && h.Node.conn != nil {
		// Discovery pool reports its live peer count; a single explicit -node
		// conn counts as one.
		if pm, ok := h.Node.conn.(*discovery.PeerManager); ok {
			conns = len(pm.Peers())
		} else {
			conns = 1
		}
	}
	return map[string]interface{}{
		"version":         ver,
		"subversion":      sub,
		"protocolversion": 70015,
		"localservices":   "000000000000000d",
		"localrelay":      true,
		"timeoffset":      0,
		"networkactive":   true,
		"connections":     conns,
		"networks": []map[string]interface{}{
			{"name": "ipv4", "limited": false, "reachable": true, "proxy": ""},
			{"name": "ipv6", "limited": false, "reachable": true, "proxy": ""},
			{"name": "onion", "limited": true, "reachable": false, "proxy": ""},
		},
		"relayfee":       0.00001,
		"incrementalfee": 0.00000001,
		"localaddresses": []interface{}{},
		"warnings":       "",
	}, nil
}
