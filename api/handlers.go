package api

import (
	"encoding/json"
	"sort"

	"xbridge-go/coins"
)

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
	combined, _ := boolParam(params, 2, true)

	type fillOut struct {
		ID        string `json:"id"`
		Time      string `json:"time"`
		Maker     string `json:"maker"`
		MakerSize string `json:"maker_size"`
		Taker     string `json:"taker"`
		TakerSize string `json:"taker_size"`
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
			ID:        f.ID,
			Time:      iso8601(f.Time),
			Maker:     f.Maker,
			MakerSize: f.MakerSize,
			Taker:     f.Taker,
			TakerSize: f.TakerSize,
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
		if _, ok := coins.Coins[o.FromCurrency]; !ok {
			continue
		}
		if _, ok := coins.Coins[o.ToCurrency]; !ok {
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
	o := h.Store.Get(id)
	if o == nil {
		return nil, makeError(errTxNotFound, "dxGetOrder", id)
	}
	return o.toListResult(), nil
}

// ---------------------------------------------------------------------------
// dxGetLocalTokens / dxGetNetworkTokens — supported token lists.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetLocalTokens(params []json.RawMessage) (interface{}, *rpcError) {
	return knownTokens(h.Config.LocalTokens), nil
}

func (h *HandlerCtx) dxGetNetworkTokens(params []json.RawMessage) (interface{}, *rpcError) {
	return knownTokens(h.Config.NetworkTokens), nil
}

func knownTokens(override []string) []string {
	if override != nil {
		return override
	}
	out := make([]string, 0, len(coins.Coins))
	for k := range coins.Coins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// dxLoadXBridgeConf — returns true.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxLoadXBridgeConf(params []json.RawMessage) (interface{}, *rpcError) {
	return true, nil
}

// ---------------------------------------------------------------------------
// dxGetNewTokenAddress — fresh address(es) for a token (requires wallet).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetNewTokenAddress(params []json.RawMessage) (interface{}, *rpcError) {
	ticker, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetNewTokenAddress", "(ticker)")
	}
	_ = ticker
	// A wallet connector is required to derive addresses; the thin client
	// returns an empty array when none is configured.
	return []string{}, nil
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
	useAll, _ := boolParam(params, 7, true)
	dryRun, _ := boolParam(params, 8, false)
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
	o, e := h.Node.MakeOrder(MakeOrderParams{
		Maker: maker, MakerSize: makerSize, MakerAddress: makerAddr,
		Taker: taker, TakerSize: takerSize, TakerAddress: takerAddr,
		Type: "partial", MinSize: minSize, DryRun: false,
	})
	if e != nil {
		return nil, e
	}
	return o.makeOrderResponse(), nil
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
	dryRun, _ := boolParam(params, 4, false)
	o, e := h.Node.TakeOrder(TakeOrderParams{ID: id, FromAddress: fromAddr, ToAddress: toAddr, Amount: amount, DryRun: dryRun})
	if e != nil {
		return nil, e
	}
	return o.toListResult(), nil
}

// ---------------------------------------------------------------------------
// dxCancelOrder.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxCancelOrder(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) != 1 {
		return nil, makeError(errInvalidParameters, "dxCancelOrder", "(id)")
	}
	id, _ := strParam(params, 0)
	o, e := h.Node.CancelOrder(CancelOrderParams{ID: id})
	if e != nil {
		return nil, e
	}
	return o.toCancelResult(), nil
}

// ---------------------------------------------------------------------------
// dxGetOrderHistory — OHLC volume series (requires historical blockchain data;
// the thin client returns an empty series).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetOrderHistory(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) < 5 || len(params) > 8 {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "(maker) (taker) (start time) (end time) (granularity) (order_ids, default=false)[optional] (with_inverse, default=false)[optional] (limit)[optional]")
	}
	return []interface{}{}, nil
}

// ---------------------------------------------------------------------------
// dxGetOrderBook.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetOrderBook(params []json.RawMessage) (interface{}, *rpcError) {
	detail, _ := intParam(params, 0, 1)
	maker, ok := strParam(params, 1)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderBook", "(detail) (maker) (taker) (max_orders, default=50)[optional]")
	}
	taker, ok := strParam(params, 2)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderBook", "(detail) (maker) (taker) (max_orders, default=50)[optional]")
	}
	maxOrders, _ := intParam(params, 3, 50)

	asks := [][]interface{}{}
	bids := [][]interface{}{}
	count := 1
	for _, o := range h.Store.List() {
		if o.Status != "open" {
			continue
		}
		if o.FromCurrency == maker && o.ToCurrency == taker {
			price := formatXPrice(float64(o.FromAmount) / float64(o.ToAmount))
			size := formatXAmount(o.FromAmount)
			asks = append(asks, bookEntry(detail, price, size, o.ID, count))
		} else if o.FromCurrency == taker && o.ToCurrency == maker {
			price := formatXPrice(float64(o.ToAmount) / float64(o.FromAmount))
			size := formatXAmount(o.ToAmount)
			bids = append(bids, bookEntry(detail, price, size, o.ID, count))
		}
		if maxOrders > 0 && (len(asks)+len(bids)) >= maxOrders {
			break
		}
	}
	return orderBookResult{Detail: detail, Maker: maker, Taker: taker, Asks: asks, Bids: bids}, nil
}

func bookEntry(detail int, price, size string, id [32]byte, count int) []interface{} {
	if detail == 3 {
		return []interface{}{price, size, hexEncode(id[:])}
	}
	return []interface{}{price, size, count}
}

// ---------------------------------------------------------------------------
// dxGetTokenBalances — per-token balances (requires wallet; empty when none).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetTokenBalances(params []json.RawMessage) (interface{}, *rpcError) {
	// Returns an object keyed by ticker -> balance string, plus a "Wallet" key.
	return map[string]string{}, nil
}

// ---------------------------------------------------------------------------
// dxGetMyOrders — orders created locally by this node.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetMyOrders(params []json.RawMessage) (interface{}, *rpcError) {
	out := []orderDetailResult{}
	for _, o := range h.Store.Mine() {
		out = append(out, o.toDetailResult())
	}
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
	o := h.Store.Get(id)
	if o == nil {
		return nil, makeError(errTxNotFound, "dxGetMyPartialOrderChain", id)
	}
	// Follow parent links to assemble the chain (oldest -> newest).
	chain := []*Order{o}
	seen := map[string]bool{id: true}
	cur := o
	for !isZeroID(cur.ParentID) {
		p := h.Store.Get(hexEncode(cur.ParentID[:]))
		if p == nil || seen[hexEncode(p.ID[:])] {
			break
		}
		chain = append([]*Order{p}, chain...)
		seen[hexEncode(p.ID[:])] = true
		cur = p
	}
	out := []orderDetailResult{}
	for _, c := range chain {
		out = append(out, c.toDetailResult())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxPartialOrderChainDetails — aggregate details for a partial chain.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxPartialOrderChainDetails(params []json.RawMessage) (interface{}, *rpcError) {
	id, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxPartialOrderChainDetails", "(order_id)")
	}
	o := h.Store.Get(id)
	if o == nil {
		return nil, makeError(errTxNotFound, "dxPartialOrderChainDetails", id)
	}
	details := map[string]interface{}{
		"first_order_id":             id,
		"maker":                      o.FromCurrency,
		"maker_address":              o.MakerAddress,
		"taker":                      o.ToCurrency,
		"taker_address":              o.TakerAddress,
		"partial_minimum":            formatXAmount(o.MinFromAmount),
		"partial_orig_maker_size":    formatXAmount(o.OrigFromAmount),
		"partial_orig_taker_size":    formatXAmount(o.OrigToAmount),
		"first_order_time":           iso8601(o.Created),
		"last_order_time":            iso8601(o.Updated),
		"total_reported_sent":        "0",
		"total_reported_received":    "0",
		"total_reported_notsent":     "0",
		"total_reported_notreceived": "0",
		"total_orders_open":          0,
		"total_orders_finished":      0,
		"total_orders_canceled":      0,
		"orders":                     []orderDetailResult{},
		"p2sh_deposits":              []interface{}{},
		"p2sh_deposits_counterparty": []interface{}{},
	}
	return details, nil
}

// ---------------------------------------------------------------------------
// dxGetLockedUtxos — currently locked orders.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetLockedUtxos(params []json.RawMessage) (interface{}, *rpcError) {
	id, _ := strParam(params, 0)
	out := []orderDetailResult{}
	for _, o := range h.Store.Locked() {
		if id != "" && hexEncode(o.ID[:]) != id {
			continue
		}
		out = append(out, o.toDetailResult())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxFlushCancelledOrders.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxFlushCancelledOrders(params []json.RawMessage) (interface{}, *rpcError) {
	ageMillis, _ := intParam(params, 0, 0)
	start := NowMicro()
	flushed := []map[string]interface{}{}
	for _, c := range h.Store.cancelled {
		flushed = append(flushed, map[string]interface{}{
			"id":        c.ID,
			"txtime":    iso8601(c.Txtime),
			"use_count": c.UseCount,
		})
	}
	dur := NowMicro() - start
	return map[string]interface{}{
		"ageMillis":        ageMillis,
		"now":              iso8601(start),
		"durationMicrosec": int64(dur),
		"flushedOrders":    flushed,
	}, nil
}

// ---------------------------------------------------------------------------
// dxGetTradingData / gettradingdata — trade history (requires blockchain data).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetTradingData(params []json.RawMessage) (interface{}, *rpcError) {
	// (blocks, default=43200) (errors, default=false)
	_ = params
	return []interface{}{}, nil
}

// ---------------------------------------------------------------------------
// dxSplitAddress / dxSplitInputs — UTXO splitting (requires wallet).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxSplitAddress(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) < 3 {
		return nil, makeError(errInvalidParameters, "dxSplitAddress", "(token) (splitamount) (address) (include_fees, default=true)[optional] (show_rawtx, default=false)[optional] (submit, default=true)[optional]")
	}
	return nil, makeError(errNoSession, "dxSplitAddress", "a wallet connector is required to split UTXOs")
}

func (h *HandlerCtx) dxSplitInputs(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) < 6 {
		return nil, makeError(errInvalidParameters, "dxSplitInputs", "(token) (splitamount) (address) (include_fees) (show_rawtx) (submit) (utxos)[optional]")
	}
	return nil, makeError(errNoSession, "dxSplitInputs", "a wallet connector is required to split UTXOs")
}

// ---------------------------------------------------------------------------
// dxGetUtxos — UTXOs for a token (requires wallet).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetUtxos(params []json.RawMessage) (interface{}, *rpcError) {
	ticker, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetUtxos", "(token) (include_used, default=false)[optional]")
	}
	_ = ticker
	return []interface{}{}, nil
}
