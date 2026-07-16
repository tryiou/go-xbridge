package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"xbridge-go/coins"
	"xbridge-go/config"
	discovery "xbridge-go/p2p/discovery"
	"xbridge-go/wallet"
)

// fillOut is one entry of dxGetOrderFills' recent-fills list.
type fillOut struct {
	ID        string `json:"id"`
	Time      string `json:"time"`
	Maker     string `json:"maker"`
	MakerSize string `json:"maker_size"`
	Taker     string `json:"taker"`
	TakerSize string `json:"taker_size"`
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
	return knownTokens(h.Config.ExchangeWallets), nil
}

func (h *HandlerCtx) dxGetNetworkTokens(params []json.RawMessage) (interface{}, *rpcError) {
	return knownTokens(h.Config.NetworkTokens), nil
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
	if h.Node == nil || h.Node.cfg == nil || h.Node.cfg.Connectors == nil {
		return nil, makeError(errNoSession, "dx", "no wallet configured")
	}
	conn, ok := h.Node.cfg.Connectors[ticker]
	if !ok || conn == nil {
		return nil, makeError(errNoSession, "dx", "no wallet configured for "+ticker)
	}
	return conn, nil
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
		return nil, e
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
	useAll, e := mustBool(params, 7, true, "dxMakeOrder")
	if e != nil {
		return nil, e
	}
	dryRun, e := mustBool(params, 8, false, "dxMakeOrder")
	if e != nil {
		return nil, e
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
	dryRun, e := mustBool(params, 4, false, "dxTakeOrder")
	if e != nil {
		return nil, e
	}
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
	detail, e := mustInt(params, 0, 1, "dxGetOrderBook")
	if e != nil {
		return nil, e
	}
	maker, ok := strParam(params, 1)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderBook", "(detail) (maker) (taker) (max_orders, default=50)[optional]")
	}
	taker, ok := strParam(params, 2)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderBook", "(detail) (maker) (taker) (max_orders, default=50)[optional]")
	}
	maxOrders, e := mustInt(params, 3, 50, "dxGetOrderBook")
	if e != nil {
		return nil, e
	}

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
	out := map[string]string{}
	if h.Node == nil || h.Node.cfg == nil || h.Node.cfg.Connectors == nil {
		return out, nil
	}
	for _, ticker := range h.Node.cfg.ExchangeWallets {
		conn, ok := h.Node.cfg.Connectors[ticker]
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
			out[ticker] = coins.FormatAmount(c, total)
		}
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
	ageMillis, e := mustInt(params, 0, 0, "dxFlushCancelledOrders")
	if e != nil {
		return nil, e
	}
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
	if len(params) < 6 {
		return nil, makeError(errInvalidParameters, "dxSplitInputs", "(token) (splitamount) (address) (include_fees) (show_rawtx) (submit) (utxos)[optional]")
	}
	ticker, _ := strParam(params, 0)
	splitAmt, _ := strParam(params, 1)
	address, _ := strParam(params, 2)
	includeFees, e := mustBool(params, 3, true, "dxSplitInputs")
	if e != nil {
		return nil, e
	}
	showRawTx, e := mustBool(params, 4, false, "dxSplitInputs")
	if e != nil {
		return nil, e
	}
	submit, e := mustBool(params, 5, true, "dxSplitInputs")
	if e != nil {
		return nil, e
	}

	var utxos []wallet.Utxo
	if len(params) > 6 {
		c, ok := coins.Get(ticker)
		if !ok {
			return nil, makeError(errInvalidParameters, "dxSplitInputs", "unknown coin: "+ticker)
		}
		u, e := parseUtxoParam(c, params[6])
		if e != nil {
			return nil, e
		}
		utxos = u
	}
	return h.splitTx(ticker, splitAmt, address, includeFees, showRawTx, submit, utxos)
}

// splitTx builds, signs and optionally submits a UTXO-split transaction for the
// given coin: it sends `splitAmount` (base units) to `address` across as many
// outputs as the selected inputs allow, returning change to a fresh address.
// Legacy P2PKH/P2SH outputs only (native segwit split is a follow-up). VERIFY:
// exact C++ return shape/semantics (include_fees handling, rawtx/txid, utxos
// field names).
func (h *HandlerCtx) splitTx(ticker, splitAmountStr, address string, includeFees, showRawTx, submit bool, utxos []wallet.Utxo) (interface{}, *rpcError) {
	conn, e := h.connector(ticker)
	if e != nil {
		return nil, e
	}
	c, ok := coins.Get(ticker)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxSplit", "unknown coin: "+ticker)
	}
	target, err := coins.ParseAmount(c, splitAmountStr)
	if err != nil {
		return nil, makeError(errInvalidParameters, "dxSplit", "invalid split amount")
	}
	cc, _ := h.Node.cfg.Confs[ticker]

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
	nSplits := total / target
	for nSplits > 0 && total < nSplits*target+fee {
		nSplits--
	}
	if nSplits == 0 {
		return nil, makeError(errInsufficientFunds, "dxSplit", "insufficient funds for split amount")
	}

	spent := nSplits*target + fee
	change := uint64(0)
	if total > spent {
		change = total - spent
	}
	if cc != nil && change < cc.DustAmount {
		fee += change
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
		tx.Outputs = append(tx.Outputs, coins.TxOut{Value: target, ScriptPubKey: destScript})
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

	res := map[string]interface{}{"address": address}
	if submit {
		txid, err := conn.SendRawTransaction(signedHex)
		if err != nil {
			return nil, makeError(errUnknown, "dxSplit", err.Error())
		}
		res["txid"] = txid
		if showRawTx {
			res["rawtx"] = signedHex
		}
	} else {
		res["rawtx"] = signedHex
	}
	return res, nil
}

// estimateFee returns a rough satoshi fee for a tx with nIn inputs and nOut
// outputs, using the connector's estimate when available and conf FeePerByte
// otherwise.
func estimateFee(cc *config.CoinConf, nIn, nOut int) uint64 {
	// Virtual-size estimate matches C++ xbridgewalletconnectorbtc.cpp:1948
	// (192 bytes per legacy input, 34 per output). Modeling inputs at 192 keeps
	// Go-built deposits inside C++'s counterpartyFees >= fee*0.95 acceptance band,
	// so a C++ counterparty accepts our orders.
	vsize := 192*nIn + 34*nOut
	if cc == nil || cc.FeePerByte == 0 {
		// 2 sat/vB default.
		return uint64(vsize * 2)
	}
	return cc.FeePerByte * uint64(vsize)
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
	ticker, ok := strParam(params, 0)
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetUtxos", "(token) (include_used, default=false)[optional]")
	}
	conn, e := h.connector(ticker)
	if e != nil {
		return nil, e
	}
	minConf := 0
	if cc, ok := h.Node.cfg.Confs[ticker]; ok {
		minConf = cc.Confirmations
	}
	utxos, err := conn.ListUnspent(minConf)
	if err != nil {
		return nil, makeError(errUnknown, "dxGetUtxos", err.Error())
	}
	c, _ := coins.Get(ticker)
	out := make([]map[string]interface{}, 0, len(utxos))
	for _, u := range utxos {
		out = append(out, map[string]interface{}{
			"txid":          u.TxID,
			"vout":          u.Vout,
			"address":       u.Address,
			"amount":        coins.FormatAmount(c, u.Amount),
			"scriptPubKey":  u.ScriptPubKey,
			"confirmations": u.Confirmations,
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
	ver := h.Config.WalletVersion
	if ver == 0 {
		ver = 4040100
	}
	sub := h.Config.WalletVersionStr
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
