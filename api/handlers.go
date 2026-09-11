package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	discovery "go-xbridge/p2p/discovery"
	"go-xbridge/wallet"
)

// xfloat8 renders a double as a JSON number with 8 fixed decimals, matching
// C++ json_spirit write_string's precision_of_doubles=8 (std::fixed +
// setprecision(8), json_spirit_writer_template.h:195). Used for the
// dxGetOrderHistory OHLCV row.
type xfloat8 float64

func (f xfloat8) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(float64(f), 'f', 8, 64)), nil
}

// quantizePrice mirrors ccy::Asset::Price (currency.h:108-123): the to/from
// ratio is rounded half-up onto the currency-basis grid. XBridge queries use
// TransactionDescr::COIN = 1e6 as the basis, so prices land on the 1e-6 grid.
func quantizePrice(price float64) float64 {
	return math.Round(price*1e6) / 1e6
}

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
	// C++ evaluates combined (params[2]) before maker/taker
	// (rpcxbridge.cpp:539-542); keep that read order so the surfaced type error
	// matches when several params are malformed at once.
	var err *rpcError
	combined := true
	if len(params) == 3 {
		if combined, _, err = spBool(params, 2); err != nil {
			return nil, err
		}
	}
	maker, ok, err := spStr(params, 0)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderFills", "(maker) (taker) (combined, default=true)[optional]")
	}
	taker, ok, err := spStr(params, 1)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrderFills", "(maker) (taker) (combined, default=true)[optional]")
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
	// arity (==0) is enforced by checkArity (dispatch.go).
	now := NowMicro()
	orders := []*Order{}
	for _, o := range h.Store.List() {
		switch statusString(o.Status) {
		case "canceled", "finished", "expired":
			// C++ filters with (currentTime - tr->txtime).total_seconds() > 60
			// (rpcxbridge.cpp:439): currentTime is second_clock (whole seconds)
			// while txtime is microsecond-resolution (a last-update time, mutated
			// by updateTimestamp, xbridgetransactiondescr.h:630-634; Go's Store
			// tracks Updated as the last mutation). total_seconds() truncates the
			// DIFFERENCE toward zero, so the drop boundary is
			// floor(now) - txtime >= 61e6 — not a difference of floors. The
			// whole-second floor of now also keeps the comparison underflow-free
			// when Updated is ahead of now (C++ then keeps the order).
			if now/1e6*1e6 >= o.Updated+61_000_000 {
				continue
			}
		}
		// Skip orders whose currencies have no wallet connector, unless
		// ShowAllOrders (-dxnowallets) is set. Mirrors C++ rpcxbridge.cpp:432-446:
		// connFrom/connTo are plain map lookups; the order is hidden when either
		// is absent and the switch is off. Unlike the old coins.Has check, a coin
		// is only "known" when a live connector is configured for it.
		if !h.Config().ShowAllOrders {
			_, eFrom := h.connector(o.FromCurrency, "dxGetOrders")
			_, eTo := h.connector(o.ToCurrency, "dxGetOrders")
			if eFrom != nil || eTo != nil {
				continue
			}
		}
		orders = append(orders, o)
	}
	// C++ iterates m_transactions (a std::map<uint256>) in LSB-first byte order
	// (uint256.h:45-49); Go Store.List() is map-ordered, so sort to match.
	sort.Slice(orders, func(i, j int) bool { return orderIDLess(orders[i].ID, orders[j].ID) })
	out := make([]orderListResult, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.toListResult())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetOrder — single order by id.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetOrder(params []json.RawMessage) (interface{}, *rpcError) {
	id, ok, perr := spStr(params, 0)
	if perr != nil {
		return nil, perr
	}
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrder", "(id)")
	}
	// C++ parses the id via uint256S (tolerant: a malformed id yields a null id
	// whose lookup misses) and renders the not-found message with the parsed
	// id's GetHex — a zero-padded 64-hex string (rpcxbridge.cpp:785).
	raw := parseOrderIDS(id)
	o := h.Store.Get(orderIDKey(id))
	if o == nil {
		// C++ App::transaction falls back to m_historicTransactions when the
		// live map misses (xbridgeapp.cpp:1273-1292): finished/cancelled local
		// orders are still resolvable after they left the live book.
		o = h.Store.HistoryOrder(orderIDKey(id))
	}
	if o == nil {
		return nil, makeError(errTxNotFound, "dxGetOrder", orderIDString(raw))
	}
	// C++ requires a wallet session for both order currencies.
	if _, e := h.connector(o.FromCurrency, "dxGetOrder"); e != nil {
		return nil, e
	}
	if _, e := h.connector(o.ToCurrency, "dxGetOrder"); e != nil {
		return nil, e
	}
	return o.toListResult(), nil
}

// ---------------------------------------------------------------------------
// dxGetLocalTokens / dxGetNetworkTokens — supported token lists.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetLocalTokens(params []json.RawMessage) (interface{}, *rpcError) {
	// C++ returns the loaded wallet connectors (availableCurrencies(),
	// xbridgeapp.cpp:808-821) — the connector map, so never unconnected or
	// duplicated. The config's ExchangeWallets may list tickers that failed to
	// load.
	if h.Node == nil || h.Node.cfg() == nil || h.Node.cfg().Connectors == nil {
		return []string{}, nil
	}
	out := make([]string, 0, len(h.Node.cfg().Connectors))
	for t := range h.Node.cfg().Connectors {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

func (h *HandlerCtx) dxGetNetworkTokens(params []json.RawMessage) (interface{}, *rpcError) {
	// C++ returns the pure union of tokens running servicenodes advertise
	// (walletServices(), xbridgeapp.cpp:2758; rpcxbridge.cpp:319-326) — no
	// config fallback. Empty when no servicenodes are connected.
	if h.Node == nil {
		return []string{}, nil
	}
	return h.Node.NetworkTokens(), nil
}

// connector returns the wallet connector configured for ticker, or a no-session
// business error (mirroring C++ when no wallet is loaded for that coin). method
// is the calling handler's name — C++ passes __FUNCTION__ at every NO_SESSION
// site.
func (h *HandlerCtx) connector(ticker, method string) (wallet.Connector, *rpcError) {
	if h.Node == nil || h.Node.cfg() == nil || h.Node.cfg().Connectors == nil {
		return nil, makeError(errNoSession, method, ticker)
	}
	conn, ok := h.Node.cfg().Connectors[ticker]
	if !ok || conn == nil {
		return nil, makeError(errNoSession, method, ticker)
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
		// C++ returns uret(success) with success=false when loadSettings()
		// fails (rpcxbridge.cpp:229-234) — the envelope succeeds with a false
		// result, it is NOT a business error.
		return false, nil
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
	ticker, ok, perr := spStr(params, 0)
	if perr != nil {
		return nil, perr
	}
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetNewTokenAddress", "(ticker)")
	}
	conn, e := h.connector(ticker, "dxGetNewTokenAddress")
	if e != nil {
		// C++ dxGetNewTokenAddress returns an empty array (not an error) when
		// no wallet is loaded for the requested coin; mirror that here.
		return []string{}, nil
	}
	addr, err := conn.GetNewAddress()
	if err != nil {
		// C++ getNewTokenAddress() returns an empty string on failure, leaving
		// the result array empty (rpcxbridge.cpp:186-190) — not a business
		// error.
		return []string{}, nil
	}
	return []string{addr}, nil
}

// ---------------------------------------------------------------------------
// dxMakeOrder / dxMakePartialOrder.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxMakeOrder(params []json.RawMessage) (interface{}, *rpcError) {
	// (maker, maker_size, maker_address, taker, taker_size, taker_address, type, [use_all_funds=true], [dryrun]);
	// arity (7..) is enforced by checkArity (dispatch.go).
	var err *rpcError
	var maker, makerSize, makerAddr, taker, takerSize, takerAddr, typ string
	if maker, _, err = spStr(params, 0); err != nil {
		return nil, err
	}
	if makerSize, _, err = spStr(params, 1); err != nil {
		return nil, err
	}
	if makerAddr, _, err = spStr(params, 2); err != nil {
		return nil, err
	}
	if taker, _, err = spStr(params, 3); err != nil {
		return nil, err
	}
	if takerSize, _, err = spStr(params, 4); err != nil {
		return nil, err
	}
	if takerAddr, _, err = spStr(params, 5); err != nil {
		return nil, err
	}
	if typ, _, err = spStr(params, 6); err != nil {
		return nil, err
	}
	// use_all_funds is read at index 7 only when present (C++ request.params[7]
	// get_bool(), UniValue) — a present non-boolean throws.
	useAll := true
	if len(params) >= 8 {
		if useAll, err = uvBool(params, 7); err != nil {
			return nil, err
		}
	}
	// dryrun is read as the literal string "dryrun" at index 8 only when there
	// are exactly 9 params (C++: if params.size()==9, json_spirit get_str).
	dryRun := false
	if len(params) == 9 {
		d, _, serr := spStr(params, 8)
		if serr != nil {
			return nil, serr
		}
		if d != "dryrun" {
			return nil, makeError(errInvalidParameters, "dxMakeOrder", d)
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
	// C++ renders a distinct dryrun object: zero id, no timestamps/block_id
	// (rpcxbridge.cpp:1004-1021).
	if dryRun {
		return o.dryrunMakeOrderResponse(), nil
	}
	return o.makeOrderResponse(), nil
}

func (h *HandlerCtx) dxMakePartialOrder(params []json.RawMessage) (interface{}, *rpcError) {
	// (maker, maker_size, maker_address, taker, taker_size, taker_address, minimum_size, [repost=true], [use_all_funds=true], [auto_split=true], [dryrun]);
	// arity (6..) is enforced by checkArity (dispatch.go).
	var err *rpcError
	var maker, makerSize, makerAddr, taker, takerSize, takerAddr, minSize string
	if maker, err = uvStr(params, 0); err != nil {
		return nil, err
	}
	if makerSize, err = uvStr(params, 1); err != nil {
		return nil, err
	}
	if makerAddr, err = uvStr(params, 2); err != nil {
		return nil, err
	}
	if taker, err = uvStr(params, 3); err != nil {
		return nil, err
	}
	if takerSize, err = uvStr(params, 4); err != nil {
		return nil, err
	}
	if takerAddr, err = uvStr(params, 5); err != nil {
		return nil, err
	}
	if minSize, err = uvStr(params, 6); err != nil {
		return nil, err
	}
	// C++ reads repost/use_all_funds/auto_split at indices 7/8/9 (defaults true)
	// only when present (request.params[i].get_bool(), UniValue), and dryrun at
	// index 10 only when there are exactly 11 params (a misspelled dryrun is an
	// error, never a silent broadcast).
	repost := true
	if len(params) >= 8 {
		if repost, err = uvBool(params, 7); err != nil {
			return nil, err
		}
	}
	useAll := true
	if len(params) >= 9 {
		if useAll, err = uvBool(params, 8); err != nil {
			return nil, err
		}
	}
	autoSplit := true
	if len(params) >= 10 {
		if autoSplit, err = uvBool(params, 9); err != nil {
			return nil, err
		}
	}
	dryRun := false
	if len(params) == 11 {
		d, uerr := uvStr(params, 10)
		if uerr != nil {
			return nil, uerr
		}
		if d != "dryrun" {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", d)
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
	// C++ renders a distinct dryrun object: zero id, no timestamps/block_id
	// (rpcxbridge.cpp:3106-3122).
	if dryRun {
		return o.dryrunMakePartialOrderResponse(repost), nil
	}
	return o.makePartialOrderResponse(repost), nil
}

// ---------------------------------------------------------------------------
// dxTakeOrder.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxTakeOrder(params []json.RawMessage) (interface{}, *rpcError) {
	// (id, from_address, to_address, [amount], [dryrun]); arity (3..5) is
	// enforced by checkArity (dispatch.go).
	var err *rpcError
	var id, fromAddr, toAddr, amount string
	if id, err = uvStr(params, 0); err != nil {
		return nil, err
	}
	if fromAddr, err = uvStr(params, 1); err != nil {
		return nil, err
	}
	if toAddr, err = uvStr(params, 2); err != nil {
		return nil, err
	}
	if len(params) >= 4 {
		if amount, err = uvStr(params, 3); err != nil {
			return nil, err
		}
	}
	// C++ gates in this order: same-address, then amount <= 0, then dryrun
	// (rpcxbridge.cpp:1146-1148 -> 1151-1161 -> 1164-1171). The same-address
	// and amount checks must run here (before the dryrun check, which is
	// handler-local) so the RPC precedence matches C++ even though TakeOrder
	// re-checks them for direct callers.
	if fromAddr == toAddr {
		return nil, makeError(errInvalidParameters, "dxTakeOrder", "The from_address and to_address cannot be the same: "+fromAddr)
	}
	if amount != "" {
		a, perr := lexicalAmount(amount)
		if perr != nil {
			return nil, perr
		}
		if a == 0 {
			return nil, makeError(errInvalidParameters, "dxTakeOrder", "The amount cannot be less than or equal to 0: "+amount)
		}
	}
	// dryrun is read as the literal string "dryrun" at index 4 only when there
	// are exactly 5 params (C++: if params.size()==5). Any other value is an
	// error, so a misspelled dryrun does not broadcast a take.
	dryRun := false
	if len(params) == 5 {
		d, uerr := uvStr(params, 4)
		if uerr != nil {
			return nil, uerr
		}
		if d != "dryrun" {
			return nil, makeError(errInvalidParameters, "dxTakeOrder", d)
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
	// arity (==1) is enforced by checkArity (dispatch.go).
	id, _, perr := spStr(params, 0)
	if perr != nil {
		return nil, perr
	}
	// C++ validates the id up front (uint256S(sid).IsNull()) and rejects a null
	// id with the raw param string (rpcxbridge.cpp:1345-1353).
	raw := parseOrderIDS(id)
	if idIsNull(raw) {
		return nil, makeError(errInvalidParameters, "dxCancelOrder", "Invalid order id ["+id+"]")
	}
	key := orderIDKey(id)
	o := h.Store.Get(key)
	if o == nil {
		// C++ App::transaction falls back to the history map, so a finished /
		// cancelled order still resolves here (and then fails the state gate).
		o = h.Store.HistoryOrder(key)
	}
	if o == nil {
		// C++ renders the miss with id.ToString() — a zero-padded 64-hex id
		// (rpcxbridge.cpp:1362).
		return nil, makeError(errTxNotFound, "dxCancelOrder", orderIDString(raw))
	}
	// C++ refuses to cancel once the swap has progressed to trCreated or beyond
	// (the order is already committed / in process).
	if stateOrdinal(o.Status) >= 6 {
		return nil, makeError(errInvalidState, "dxCancelOrder", "The order is already "+statusString(o.Status))
	}
	// C++ cancels FIRST (cancelXBridgeTransaction, xbridgeapp.cpp:2468-2501)
	// and only then resolves the wallet connectors to build the result
	// (rpcxbridge.cpp:1364-1385). The from-currency connector is gated INSIDE
	// CancelOrder (missing from -> NO_SESSION, no cancel); a missing TO
	// connector only fails the result build here — the cancel side effect
	// already happened.
	res, e := h.Node.CancelOrder(CancelOrderParams{ID: key})
	if e != nil {
		return nil, e
	}
	// C++ requires a wallet session for both currencies to build the result
	// (NO_SESSION carries the currency).
	if _, e := h.connector(o.FromCurrency, "dxCancelOrder"); e != nil {
		return nil, e
	}
	if _, e := h.connector(o.ToCurrency, "dxCancelOrder"); e != nil {
		return nil, e
	}
	return res.toCancelResult(), nil
}

// defaultOrderHistoryMaxBuckets is the hard cap on the number of OHLCV buckets
// dxGetOrderHistory may build when the caller omits interval_limit. C++'s
// IntervalLimit default is INT_MAX (util/xseries.h:42), so an absent-limit
// request spanning the whole 2018→now window at the 60 s granularity would
// allocate ~4.3M buckets and an equally large result slice — a remote-OOM
// vector. Go caps the default so the unbounded path is bounded; an explicit
// interval_limit still mirrors C++ exactly (cap + tail window).
const defaultOrderHistoryMaxBuckets = 100_000

// ---------------------------------------------------------------------------
// dxGetOrderHistory — OHLC volume series aggregated from the fills this node
// has actually seen (session-local fills only; no network-wide XSeries block
// index is available to the thin client — see docs/api.md Tier 3).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetOrderHistory(params []json.RawMessage) (interface{}, *rpcError) {
	// arity (5..8) is enforced by checkArity (dispatch.go).
	var err *rpcError
	var maker, taker string
	if maker, _, err = spStr(params, 0); err != nil {
		return nil, err
	}
	if taker, _, err = spStr(params, 1); err != nil {
		return nil, err
	}
	// C++ reads granularity (params[4]) before start/end (params[2], params[3])
	// (rpcxbridge.cpp:654-656); keep that read order so the surfaced type error
	// matches when several params are malformed at once.
	granularity, _, err := spInt64(params, 4)
	if err != nil {
		return nil, err
	}
	start, _, err := spInt64(params, 2)
	if err != nil {
		return nil, err
	}
	end, _, err := spInt64(params, 3)
	if err != nil {
		return nil, err
	}
	orderIDs := false
	if len(params) > 5 {
		if orderIDs, _, err = spBool(params, 5); err != nil {
			return nil, err
		}
	}
	withInverse := false
	if len(params) > 6 {
		if withInverse, _, err = spBool(params, 6); err != nil {
			return nil, err
		}
	}
	limit := 0
	if len(params) > 7 {
		if limit, _, err = spInt(params, 7); err != nil {
			return nil, err
		}
	}

	// xQuery ctor validations (util/xseries.h:91-101), in C++ order: the
	// granularity whitelist, the aligned period bounds, then the limit range.
	supportedGranularity := granularity == 60 || granularity == 300 || granularity == 900 ||
		granularity == 3600 || granularity == 21600 || granularity == 86400
	if !supportedGranularity {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "granularity="+strconv.FormatInt(granularity, 10)+" must be one of: 60,300,900,3600,21600,86400")
	}
	// get_start_time / get_end_time snap the boundaries onto the granularity
	// grid (floor start, ceil end); a negative boundary snaps to epoch 0
	// (util/xseries.h:131-142).
	alignedStart := int64(0)
	if start >= 0 {
		alignedStart = (start / granularity) * granularity
	}
	alignedEnd := int64(0)
	if end >= 0 {
		alignedEnd = ((end + granularity - 1) / granularity) * granularity
	}
	// XSeries::earliestTime() = 2018-02-25 00:00:00 UTC = 1519516800
	// (util/xseries.h:107-109). C++ compares the ALIGNED period.begin().
	const xSeriesEarliest = int64(1519516800)
	if alignedStart < xSeriesEarliest {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "Start time too early.")
	}
	if alignedEnd <= alignedStart { // time_period::is_null()
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "Start time >= end time.")
	}
	// oneDayFromNow = currentTime + 1 day (util/xseries.h:34).
	if alignedEnd > int64(NowMicro()/1e6)+86400 {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "Start/end times are too large.")
	}
	if len(params) > 7 && (limit < 1 || limit > 2147483647) {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "interval_limit must be in range 1 to 2147483647.")
	}
	numBuckets := (alignedEnd - alignedStart) / granularity
	// C++ getChainXAggregateSeries caps the bucket count at interval_limit and
	// shifts the window to the most-recent tail: period = [period.end -
	// num_intervals*granularity, period.end] (xseries.cpp:106-112). An explicit
	// interval_limit mirrors C++ exactly. C++'s DEFAULT limit is INT_MAX
	// (util/xseries.h:42), which is the remote-OOM vector closed here:
	// without an explicit limit the bucket count is hard-capped at
	// defaultOrderHistoryMaxBuckets and the window shifts to the tail, so a
	// 2018→now request allocates ≤100k buckets, not ~4.3M.
	effectiveLimit := defaultOrderHistoryMaxBuckets
	if len(params) > 7 {
		effectiveLimit = limit
	}
	if numBuckets > int64(effectiveLimit) {
		numBuckets = int64(effectiveLimit)
		alignedStart = alignedEnd - numBuckets*granularity
	}

	// C++ walk of the XSeries cache is replaced here by the thin client's local
	// trade history (Store.Fills): OHLCV is aggregated from the fills this node
	// has actually seen. Network-wide XSeries history (requiring a blocknetd
	// block index) is not available to the thin client, so the data reflects
	// this node's local trades only. The wire *schema* matches C++ exactly.
	type bucket struct {
		fills []*fillEntry
	}
	// Lazy per-bucket allocation: buckets[i].fills is nil until a fill lands
	// in it (append to a nil slice allocates on demand), so a sparse window
	// over a mostly-empty fills store allocates only for observed buckets.
	buckets := make([]bucket, numBuckets)
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
		if ft < alignedStart || ft >= alignedEnd {
			continue
		}
		bi := (ft - alignedStart) / granularity
		if bi < 0 || bi >= numBuckets {
			continue
		}
		buckets[bi].fills = append(buckets[bi].fills, f)
	}

	out := make([]interface{}, 0, numBuckets)
	for i := int64(0); i < numBuckets; i++ {
		bucketStart := alignedStart + i*granularity
		bf := buckets[i].fills
		// Chronological order so open=first, close=last.
		sort.SliceStable(bf, func(i, j int) bool { return bf[i].Time < bf[j].Time })
		var open, high, low, close, volume float64
		ids := make([]string, 0, len(bf))
		for _, f := range bf {
			inverse := f.Maker == taker && f.Taker == maker
			makerNum, e1 := strconv.ParseFloat(f.MakerSize, 64)
			takerNum, e2 := strconv.ParseFloat(f.TakerSize, 64)
			if e1 != nil || e2 != nil || makerNum == 0 || takerNum == 0 {
				continue
			}
			// C++ price = ccy::Asset::Price{to, from} (currencypair.h:38-40):
			// the to/from ratio quantized to the 1e-6 grid (currency.h:108-123).
			// Volume = fromVolume (the FROM/maker side). For a with_inverse
			// match the aggregate is inverted (xseries.cpp:176-185) and merged
			// via updateXSeries with Transform::Invert (xseries.cpp:127-130):
			// the QUANTIZED direct price is reciprocated WITHOUT re-quantizing,
			// and the volume side swaps to the fill's TAKER amount.
			price := quantizePrice(takerNum / makerNum)
			vol := makerNum
			if inverse {
				if price == 0 {
					price = 0 // C++ inverse() epsilon guard (xseries.cpp:178-179)
				} else {
					price = 1.0 / price
				}
				vol = takerNum
			}
			// The open gate mirrors C++ `if (open == 0)` (xseries.cpp:189-194),
			// so a zero-quantized first price is overwritten by the next fill.
			if open == 0 {
				open, high, low = price, price, price
			}
			if price > high {
				high = price
			}
			if price < low {
				low = price
			}
			close = price
			volume += vol
			ids = append(ids, f.ID)
		}
		// C++ row: [iso8601(x.timeEnd - offset), low, high, open, close, volume]
		// (rpcxbridge.cpp:681-682). The default interval_timestamp is at_start
		// (util/xseries.h:52; the 9th param is unreachable through the arity
		// gate), so offset = granularity and the time is the bucket START. All
		// OHLCV doubles render as fixed-8 JSON numbers (json_spirit
		// write_string precision 8, json_spirit_writer_template.h:195).
		row := []interface{}{iso8601(uint64(bucketStart) * 1e6), xfloat8(low), xfloat8(high), xfloat8(open), xfloat8(close), xfloat8(volume)}
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
	price    float64  // numeric price for sorting (ask=to/from, bid=from/to)
	priceStr string   // 6-decimal rendered price
	amount   uint64   // base-unit amount to render (ask=fromAmount, bid=toAmount)
	id       string   // hex order id in C++ display order (uint256::GetHex)
	rawID    [32]byte // raw wire id, for C++'s id-sorted iteration order
}

// floatCompare mirrors C++ rpcxbridge.cpp floatCompare (Knuth 4.2.2 Eq 36):
// relative-epsilon equality with std::numeric_limits<double>::epsilon().
func floatCompare(a, b float64) bool {
	const epsilon = 2.220446049250313e-16 // std::numeric_limits<double>::epsilon()
	return (math.Abs(a-b)/math.Abs(a) <= epsilon) && (math.Abs(a-b)/math.Abs(b) <= epsilon)
}

// countAtPrice returns the number of orders in the full side list whose price
// is floatCompare-equal to price, matching C++'s std::count_if over the whole
// asksList/bidsList (rpcxbridge.cpp:1673-1688, 1722-1740, 1808-1819).
func countAtPrice(list []obEntry, price float64) int {
	n := 0
	for _, e := range list {
		if floatCompare(e.price, price) {
			n++
		}
	}
	return n
}

// idsAtPrice returns the ids of every order in the full side list that is
// floatCompare-equal to the best entry's price: the best order's id first,
// then the rest in ascending raw-id order — matching C++ detail 4
// (rpcxbridge.cpp:1905-1926), which emits the best id then walks the
// id-sorted TransactionMap for the remaining equal-price ids.
func idsAtPrice(list []obEntry, best obEntry) []string {
	ids := []string{best.id}
	rest := []obEntry{}
	for _, e := range list {
		if e.rawID == best.rawID {
			continue
		}
		if floatCompare(e.price, best.price) {
			rest = append(rest, e)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return bytes.Compare(rest[i].rawID[:], rest[j].rawID[:]) < 0 })
	for _, e := range rest {
		ids = append(ids, e.id)
	}
	return ids
}

// bestOf returns the side's best entry (lowest-price ask / highest-price bid);
// ties are broken by the smallest raw id (orderIDLess, LSB-first), mirroring
// C++ std::min_element / std::max_element over the id-sorted TransactionMap.
// list must be non-empty.
func bestOf(list []obEntry, highest bool) obEntry {
	best := list[0]
	for _, e := range list[1:] {
		better := e.price > best.price
		if !highest {
			better = e.price < best.price
		}
		if better || (e.price == best.price && orderIDLess(e.rawID, best.rawID)) {
			best = e
		}
	}
	return best
}

func (h *HandlerCtx) dxGetOrderBook(params []json.RawMessage) (interface{}, *rpcError) {
	// arity (3..4) is enforced by checkArity (dispatch.go).
	detail, _, err := spInt(params, 0)
	if err != nil {
		return nil, err
	}
	if detail < 1 || detail > 4 {
		return nil, makeError(errInvalidDetailLevel, "dxGetOrderBook", "")
	}
	var maker, taker string
	if maker, _, err = spStr(params, 1); err != nil {
		return nil, err
	}
	if taker, _, err = spStr(params, 2); err != nil {
		return nil, err
	}
	maxOrders := 50
	if len(params) == 4 {
		if maxOrders, _, err = spInt(params, 3); err != nil {
			return nil, err
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
			// ask: from=maker (sold), to=taker; C++ price =
			// xBridgeValueFromAmount(to) / xBridgeValueFromAmount(from) — the
			// +1/::COIN bump on each amount (xutil.cpp:293-302).
			p := xBridgeValueFromAmount(o.ToAmount) / xBridgeValueFromAmount(o.FromAmount)
			asks = append(asks, obEntry{price: p, priceStr: formatXPrice(p), amount: o.FromAmount, id: orderIDString(o.ID), rawID: o.ID})
		}
		if strings.EqualFold(o.FromCurrency, taker) && strings.EqualFold(o.ToCurrency, maker) {
			// bid: from=taker, to=maker; C++ priceBid =
			// xBridgeValueFromAmount(from) / xBridgeValueFromAmount(to)
			// (xutil.cpp:303-312).
			p := xBridgeValueFromAmount(o.FromAmount) / xBridgeValueFromAmount(o.ToAmount)
			bids = append(bids, obEntry{price: p, priceStr: formatXPrice(p), amount: o.ToAmount, id: orderIDString(o.ID), rawID: o.ID})
		}
	}

	// Sort both sides descending by price; equal prices are ordered by the
	// smallest raw id (orderIDLess, LSB-first). For detail 1/4 this mirrors C++'
	// min/max_element over the id-sorted TransactionMap exactly; for
	// detail 2/3 C++ std::sort leaves the within-price-group order unspecified,
	// so the ascending-id tie-break is a deterministic superset guarantee.
	sort.Slice(asks, func(i, j int) bool {
		if asks[i].price != asks[j].price {
			return asks[i].price > asks[j].price
		}
		return orderIDLess(asks[i].rawID, asks[j].rawID)
	})
	sort.Slice(bids, func(i, j int) bool {
		if bids[i].price != bids[j].price {
			return bids[i].price > bids[j].price
		}
		return orderIDLess(bids[i].rawID, bids[j].rawID)
	})

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
			best := bestOf(asks, false) // lowest-price ask
			res.Asks = append(res.Asks, []interface{}{best.priceStr, formatXAmount(best.amount), countAtPrice(asks, best.price)})
		}
		if len(bids) > 0 {
			best := bestOf(bids, true) // highest-price bid
			res.Bids = append(res.Bids, []interface{}{best.priceStr, formatXAmount(best.amount), countAtPrice(bids, best.price)})
		}
	case 2:
		// Aggregated top levels (per side). Faithful port of C++ d2
		// (rpcxbridge.cpp:1756-1833): iterate the best window (asks from
		// asks_len-bound, bids from 0), emit one row per selected index with
		// count_if over the FULL list, and skip equal-price neighbours with
		// C++'s `while((++i < bound) && floatCompare(...))` — which advances i
		// even when prices differ (so roughly every-other window entry is
		// emitted) and merges equal prices only while i < bound.
		bound := min(maxOrders, len(bids))
		for i := 0; i < bound; {
			bid := bids[i]
			bidSize := bid.amount
			bidCount := countAtPrice(bids, bid.price)
			i++
			for i < bound && floatCompare(bids[i].price, bid.price) {
				bidSize += bids[i].amount
				i++
			}
			i++
			res.Bids = append(res.Bids, []interface{}{bid.priceStr, formatXAmount(bidSize), bidCount})
		}
		bound = min(maxOrders, len(asks))
		for i := len(asks) - bound; i < len(asks); {
			ask := asks[i]
			askSize := ask.amount
			askCount := countAtPrice(asks, ask.price)
			i++
			for i < bound && floatCompare(asks[i].price, ask.price) {
				askSize += asks[i].amount
				i++
			}
			i++
			res.Asks = append(res.Asks, []interface{}{ask.priceStr, formatXAmount(askSize), askCount})
		}
	case 3:
		// Full, non-aggregated, per side, capped at maxOrders, with order ids.
		// C++ iterates the best window (bids from 0, asks from asks_len-bound to
		// asks_len-1), so asks are descending price within the best window.
		bound := min(maxOrders, len(bids))
		for i := 0; i < bound; i++ {
			e := bids[i]
			res.Bids = append(res.Bids, []interface{}{e.priceStr, formatXAmount(e.amount), e.id})
		}
		bound = min(maxOrders, len(asks))
		for i := len(asks) - bound; i < len(asks); i++ {
			e := asks[i]
			res.Asks = append(res.Asks, []interface{}{e.priceStr, formatXAmount(e.amount), e.id})
		}
	case 4:
		// Best bid and ask only, with the array of order ids at that price.
		if len(asks) > 0 {
			best := bestOf(asks, false)
			res.Asks = append(res.Asks, []interface{}{best.priceStr, formatXAmount(best.amount), idsAtPrice(asks, best)})
		}
		if len(bids) > 0 {
			best := bestOf(bids, true)
			res.Bids = append(res.Bids, []interface{}{best.priceStr, formatXAmount(best.amount), idsAtPrice(bids, best)})
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
			out[ticker] = formatBalanceNative(c, avail)
		}
	}
	// RPC-F23 (fix-queued, tracked in docs/audit/register.md): C++ always emits
	// a "Wallet" key (the native BLOCK available balance). Until synthesized,
	// only per-ticker keys are returned; C++ ticker ordering is race-dependent.
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetMyOrders — orders created locally by this node.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetMyOrders(params []json.RawMessage) (interface{}, *rpcError) {
	// C++ builds a combined live+history vector, sorts by txtime, then renders
	// with a `seen` dedup (rpcxbridge.cpp:2103-2138). Sort on the raw µs value,
	// not the ISO-rendered string, so microsecond ordering is exact.
	orders := []*Order{}
	seen := map[string]bool{}
	for _, o := range h.Store.Mine() {
		seen[hexEncode(o.ID[:])] = true
		orders = append(orders, o)
	}
	// C++ dxGetMyOrders merges live local orders with historical local orders
	// in a terminal state (rpcxbridge.cpp:2110-2121): finished/cancelled orders
	// that left the live book stay visible.
	for _, e := range h.Store.History() {
		o := e.Order
		if o == nil || !o.Mine {
			continue
		}
		switch statusString(e.Status) {
		case "finished", "canceled":
			// Dedup against the live set: an order still in the live book must
			// not appear twice (C++ seen[t->id.GetHex()]). C++ sorts by txtime
			// and keeps the smallest-txtime record; the thin client keeps the
			// LIVE record (it is the current one) — they can only differ when
			// the same id is both live and in history with different Updated.
			if seen[hexEncode(o.ID[:])] {
				continue
			}
			seen[hexEncode(o.ID[:])] = true
			orders = append(orders, o)
		}
	}
	// C++ dxGetMyOrders sorts ascending by txtime (updated time, µs).
	sort.SliceStable(orders, func(i, j int) bool {
		return orders[i].Updated < orders[j].Updated
	})
	out := make([]orderDetailResult, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.toDetailResult())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetMyPartialOrderChain — the partial-repost chain rooted at order_id.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetMyPartialOrderChain(params []json.RawMessage) (interface{}, *rpcError) {
	// C++ runs RPCTypeCheck(params, {VSTR}) before reading the id
	// (rpcxbridge.cpp:2271) — a wrong-typed param is a -3 type error.
	id, perr := rtcStr(params, 0)
	if perr != nil {
		return nil, perr
	}
	// C++ getPartialOrderChain resolves both ancestors and descendants; reuse the
	// shared chain walker so this matches dxPartialOrderChainDetails.
	// C++ validates the id up front (uint256S(order_id).IsNull() -> "bad order id").
	raw := parseOrderIDS(id)
	if idIsNull(raw) {
		return nil, makeError(errInvalidParameters, "dxGetMyPartialOrderChain", "bad order id")
	}
	chain := h.partialOrderChain(orderIDKey(id))
	if len(chain) == 0 {
		return []interface{}{}, nil
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

// partialOrderChain returns the partial repost lineage for id. It mirrors
// xbridge::App::getPartialOrderChain (xbridgeapp.cpp:3913-3991) for the parts
// that are deterministic and contract-visible:
//
//  1. Filter: only LOCAL orders that are partial or partial-children — an exact
//     order with no parent is excluded (xbridgeapp.cpp:3922,3934). Applied to
//     both the live book and history, with the same `seen` dedup the writers
//     use. The queried id must be in the filtered set, else the chain is empty.
//  2. Walk the FULL lineage: every ancestor up via ParentID and every
//     descendant down via parent links. (The C++ child-walk only scans orders
//     with fewer utxos than the queried id and never descends to grandchildren
//     — xbridgeapp.cpp:3958-3967 — but that is sort-order dependent and is a
//     latent C++ bug; the port models the full lineage so it is deterministic
//     here.)
//  3. The final chain is sorted ascending by created time so the oldest order
//     is first (xbridgeapp.cpp:3985-3988).
func (h *HandlerCtx) partialOrderChain(id string) []*Order {
	idx := map[string]*Order{}
	add := func(o *Order) {
		hex := hexEncode(o.ID[:])
		if _, ok := idx[hex]; ok {
			return
		}
		// Local partial order, or an exact order that is a child of a partial
		// order (has a parent). Exact parent-less orders are excluded.
		if !isZeroID(o.ParentID) || o.PartialAllowed {
			idx[hex] = o
		}
	}
	for _, o := range h.Store.List() {
		if o.Mine {
			add(o)
		}
	}
	for _, e := range h.Store.History() {
		if e.Order != nil && e.Order.Mine {
			add(e.Order)
		}
	}
	if _, ok := idx[id]; !ok {
		return nil
	}

	// Ancestors: walk up via ParentID to the chain root.
	chain := []*Order{}
	seen := map[string]bool{id: true}
	cur := idx[id]
	for !isZeroID(cur.ParentID) {
		pid := hexEncode(cur.ParentID[:])
		p, ok := idx[pid]
		if !ok || seen[pid] {
			break
		}
		seen[pid] = true
		chain = append([]*Order{p}, chain...)
		cur = p
	}
	chain = append(chain, idx[id])

	// Descendants: walk down via parent links from the queried order (a
	// descendant of an ancestor that is not also a descendant of id is a
	// sibling branch and stays out of this chain).
	var descend func(pid string)
	descend = func(pid string) {
		for oid, o := range idx {
			if seen[oid] {
				continue
			}
			if hexEncode(o.ParentID[:]) == pid {
				seen[oid] = true
				chain = append(chain, o)
				descend(oid)
			}
		}
	}
	descend(id)

	// Sort ascending by created time so the root (oldest) order is first.
	sort.SliceStable(chain, func(i, j int) bool {
		return chain[i].Created < chain[j].Created
	})
	return chain
}

func (h *HandlerCtx) dxPartialOrderChainDetails(params []json.RawMessage) (interface{}, *rpcError) {
	// C++ runs RPCTypeCheck(params, {VSTR}) before reading the id
	// (rpcxbridge.cpp:2411) — a wrong-typed param is a -3 type error.
	id, perr := rtcStr(params, 0)
	if perr != nil {
		return nil, perr
	}
	// C++ validates the order id up front (uint256S(sid).IsNull()).
	raw := parseOrderIDS(id)
	if idIsNull(raw) {
		return nil, makeError(errInvalidParameters, "dxPartialOrderChainDetails", "bad order id")
	}
	chain := h.partialOrderChain(orderIDKey(id))
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
		orderIDs = append(orderIDs, orderIDString(t.ID))
		// C++ pushes ONE entry per chain order — the deposit txids or an empty
		// string — so callers can index p2sh_deposits against `orders` by
		// position (rpcxbridge.cpp:2456-2457).
		deposits = append(deposits, t.BinTxId)
		counter = append(counter, t.OBinTxId)
	}
	return partialChainDetailsResult{
		FirstOrderID:             orderIDString(first.ID),
		Maker:                    first.FromCurrency,
		MakerAddress:             first.MakerAddress,
		Taker:                    first.ToCurrency,
		TakerAddress:             first.TakerAddress,
		PartialMinimum:           formatXAmount(first.MinFromAmount),
		PartialOrigMakerSize:     formatXAmount(first.OrigFromAmount),
		PartialOrigTakerSize:     formatXAmount(first.OrigToAmount),
		FirstOrderTime:           iso8601(first.Created),
		LastOrderTime:            iso8601(last.Updated),
		TotalReportedSent:        formatXAmount(totalSent),
		TotalReportedReceived:    formatXAmount(totalReceived),
		TotalReportedNotsent:     formatXAmount(totalNotSent),
		TotalReportedNotreceived: formatXAmount(totalNotReceived),
		TotalOrdersOpen:          totalOpen,
		TotalOrdersFinished:      totalFinished,
		TotalOrdersCanceled:      totalCanceled,
		Orders:                   orderIDs,
		P2SHDeposits:             deposits,
		P2SHDepositsCounterparty: counter,
	}, nil
}

// ---------------------------------------------------------------------------
// dxGetLockedUtxos — currently locked orders.
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetLockedUtxos(_ []json.RawMessage) (interface{}, *rpcError) {
	// arity (0..1) is enforced by checkArity (dispatch.go), matching C++'s
	// "Too many parameters" 1025 for >1 (rpcxbridge.cpp:2619-2625).
	// C++ then gates on the Exchange being started (rpcxbridge.cpp:2627-2633);
	// a thin client never starts it, so EVERY call answers 1029
	// NOT_EXCHANGE_NODE (canonical text in response.go, xbridgeerror.cpp:61-62).
	// The params are intentionally unread: the gate fires before id parsing,
	// regardless of params. No list is ever served.
	return nil, makeError(errNotExchangeNode, "dxGetLockedUtxos", "")
}

// ---------------------------------------------------------------------------
// dxHelp serves Core's help command: bare lists the supported commands,
// one command returns its exact text plus Core's trailing newline, unknown
// returns the exact unknown-command message. The command list is an honest
// subset (this node only), never Core's full list.
func (h *HandlerCtx) dxHelp(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) == 0 {
		var sb strings.Builder
		sb.WriteString("== XBridge ==\n")
		for _, m := range helpCommandNames {
			sb.WriteString(m + "\n")
		}
		return sb.String(), nil
	}
	cmd, err := uvStr(params, 0)
	if err != nil {
		return nil, err
	}
	if text, ok := helpByMethod[cmd]; ok {
		return text + "\n", nil
	}
	return "help: unknown command: " + cmd, nil
}

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
		v, _, err := spInt(params, 0)
		if err != nil {
			return nil, err
		}
		// C++ holds the age in 32 bits: huge values wrap mod 2^32 and a
		// wrapped negative fails below (live Core: 9999999999999 echoes
		// 1316134911, 2^31 and 2^32-1 give 1025, 2^32 echoes 0).
		ageMillis = int(int32(v))
	default:
		return nil, makeError(errInvalidParameters, "dxFlushCancelledOrders", "ageMillis must be an integer >= 0")
	}
	if ageMillis < 0 {
		return nil, makeError(errInvalidParameters, "dxFlushCancelledOrders", "ageMillis must be an integer >= 0")
	}
	now := NowMicro()
	flushed := h.Store.FlushCancelled(uint64(ageMillis))
	dur := NowMicro() - now
	orders := make([]flushedOrderOut, 0, len(flushed))
	for _, f := range flushed {
		orders = append(orders, flushedOrderOut{
			ID:       orderIDString(f.ID),
			Txtime:   iso8601(f.Txtime),
			UseCount: f.UseCount,
		})
	}
	// Keys emitted in C++ pushKV order (rpcxbridge.cpp:1474-1489).
	return flushCancelledResult{
		AgeMillis:        int64(ageMillis),
		Now:              iso8601(now),
		DurationMicrosec: int64(dur),
		FlushedOrders:    orders,
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
	// arity (0..2) is enforced by checkArity (dispatch.go).
	// C++ runs RPCTypeCheck on the present params (rpcxbridge.cpp:2847-2854):
	// params[0] must be a number when present, params[1] a bool when there are
	// exactly 2. The thin client ignores the values but must still reject wrong
	// types with the -3 envelope error.
	if len(params) >= 1 {
		if _, perr := rtcNum(params, 0); perr != nil {
			return nil, perr
		}
	}
	if len(params) == 2 {
		if _, perr := rtcBool(params, 1); perr != nil {
			return nil, perr
		}
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
	// (token, splitamount, address, include_fees[default=true], show_rawtx[default=false], submit[default=true]);
	// arity (3..6) is enforced by checkArity (dispatch.go).
	var err *rpcError
	var ticker, splitAmt, address string
	if ticker, err = uvStr(params, 0); err != nil {
		return nil, err
	}
	if splitAmt, err = uvStr(params, 1); err != nil {
		return nil, err
	}
	if address, err = uvStr(params, 2); err != nil {
		return nil, err
	}
	// C++ reads include_fees/show_rawtx/submit via isNull()-guarded get_bool()
	// (rpcxbridge.cpp:3254-3259): absent/null keeps the default; a present
	// non-boolean throws.
	var includeFees, showRawTx, submit bool
	if includeFees, err = uvBoolOpt(params, 3, true); err != nil {
		return nil, err
	}
	if showRawTx, err = uvBoolOpt(params, 4, false); err != nil {
		return nil, err
	}
	if submit, err = uvBoolOpt(params, 5, true); err != nil {
		return nil, err
	}
	return h.splitTx(ticker, splitAmt, address, includeFees, showRawTx, submit, nil, "dxSplitAddress")
}

func (h *HandlerCtx) dxSplitInputs(params []json.RawMessage) (interface{}, *rpcError) {
	// arity (3..7) is enforced by checkArity (dispatch.go). C++ reads params[3..6]
	// unconditionally, so a short call throws at the first missing index.
	var err *rpcError
	var ticker, splitAmt, address string
	if ticker, err = uvStr(params, 0); err != nil {
		return nil, err
	}
	if splitAmt, err = uvStr(params, 1); err != nil {
		return nil, err
	}
	if address, err = uvStr(params, 2); err != nil {
		return nil, err
	}
	// C++ reads params[3..6] unconditionally via get_bool()/get_array()
	// (rpcxbridge.cpp:3355-3358); a missing/null/wrong-typed value throws the
	// UniValue type error.
	var includeFees, showRawTx, submit bool
	if includeFees, err = uvBool(params, 3); err != nil {
		return nil, err
	}
	if showRawTx, err = uvBool(params, 4); err != nil {
		return nil, err
	}
	if submit, err = uvBool(params, 5); err != nil {
		return nil, err
	}
	// C++ throws the get_array() type error on params[6] before any coin lookup
	// (rpcxbridge.cpp:3358); match that error precedence.
	if _, err = uvArr(params, 6); err != nil {
		return nil, err
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
	// C++ rejects a user-specified utxo that is locked by an order
	// (rpcxbridge.cpp:3374-3379).
	lockedKeys, _ := h.Store.LockedUtxoInfo()
	for _, u := range utxos {
		k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
		if lockedKeys[k] {
			return nil, makeError(errBadRequest, "dxSplitInputs", "Cannot split utxo already in use: "+k)
		}
	}
	return h.splitTx(ticker, splitAmt, address, includeFees, showRawTx, submit, utxos, "dxSplitInputs")
}

// splitTx builds, signs and optionally submits a UTXO-split transaction for the
// given coin, mirroring C++ dxSplitAddress/dxSplitInputs + splitUtxos
// (xbridgewalletconnectorbtc.cpp:2628-2767):
//   - each split output sends `splitAmount` (+feesPerUtxo when include_fees) to address;
//   - the real tx fee is deducted from change, clawed back from the last split
//     output when the change is dust;
//   - change goes to the REQUESTED address, not a fresh one;
//   - the result echoes C++'s 8-field object in pushKV order.
//
// Fee math is done in XBridge 1e6 units exactly as C++ (minTxFee1/minTxFee2
// whole-coin + xBridgeIntFromReal); raw tx values are converted to the
// coin's native scale at build time. txid is the double-SHA256 of the signed
// transaction, byte-reversed — always computed, even when submit=false.
func (h *HandlerCtx) splitTx(ticker, splitAmountStr, address string, includeFees, showRawTx, submit bool, utxos []wallet.Utxo, method string) (interface{}, *rpcError) {
	conn, e := h.connector(ticker, method)
	if e != nil {
		return nil, e
	}
	c, ok := coins.Get(ticker)
	if !ok {
		return nil, makeError(errInvalidParameters, method, "unknown coin: "+ticker)
	}
	targetXB, err := lexicalAmount(splitAmountStr)
	if err != nil {
		return nil, err
	}
	cc := h.Node.cfg().Confs[ticker]
	relayFee, _ := conn.GetRelayFee()
	// C++ dust gate on the minimum split amount (xbridgewalletconnectorbtc.cpp:2635).
	if isDustNative(xBridgeValueFromAmount(targetXB), cc, relayFee, coinNativeScale(c)) {
		return nil, makeError(errBadRequest, method, "split amount is dust ["+formatXAmount(targetXB)+"]")
	}
	// C++ validates the address (and wallet membership) BEFORE listing utxos
	// (xbridgewalletconnectorbtc.cpp:2639-2642): BAD_REQUEST named after the
	// method. Native-segwit destinations are also rejected here (the
	// split builder only emits legacy scripts).
	if a, derr := c.DecodeAddress(address); derr != nil || (a.Kind != coins.P2PKH && a.Kind != coins.P2SH) {
		return nil, makeError(errBadRequest, method, "address is invalid or not in the wallet for token "+ticker)
	}

	// C++ fee model in XBridge units (xbridgewalletconnectorbtc.cpp:2665-2734):
	// feesPerUtxo = minTxFee1(1,3) + minTxFee2(1,1) per split output (added only
	// when include_fees); the real tx fee minTxFee1(vins,vouts) is deducted from
	// change, and a dust change claws it back from the last split output.
	// Computed up front because the auto-path utxo filter drops entries whose
	// camount already equals splitSize (:2670-2676).
	fee1XB := xBridgeIntFromReal(minTxFeeWhole(cc, 1, 3))
	fee2XB := xBridgeIntFromReal(minTxFeeWhole(cc, 1, 1))
	feesPerUtxoXB := fee1XB + fee2XB
	splitSizeXB := targetXB
	if includeFees {
		splitSizeXB += feesPerUtxoXB
	}

	minConf := 0
	if cc != nil {
		minConf = cc.Confirmations
	}
	walletUtxos, lerr := conn.ListUnspent(minConf)
	if lerr != nil {
		return nil, makeError(errBadRequest, method, lerr.Error())
	}
	// C++ getUnspent keeps only P2PKH outputs and re-derives the entry
	// address from the script (fromXAddr), for both the auto and the
	// explicit path (xbridgewalletconnectorbtc.cpp:1617-1636). Filter here
	// so a user-specified non-P2PKH utxo also resolves as "not found or not
	// available", exactly as C++. For P2PKH the derived address equals the
	// wallet-reported one, so no re-derivation is needed.
	filtered := make([]wallet.Utxo, 0, len(walletUtxos))
	for _, u := range walletUtxos {
		if isP2PKHScript(u.ScriptPubKey) {
			filtered = append(filtered, u)
		}
	}
	walletUtxos = filtered
	lockedKeys, _ := h.Store.LockedUtxoInfo()
	if len(utxos) == 0 {
		// Auto path (dxSplitAddress): split the spendable (non-locked) utxos,
		// mirroring splitUtxos getUnspent(..., excluded) plus its auto filters
		// (xbridgewalletconnectorbtc.cpp:2670-2676): skip utxos that already
		// match the split size, or that belong to another wallet address.
		utxos = nil
		for _, u := range walletUtxos {
			k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
			if lockedKeys[k] {
				continue
			}
			if camount(u) == splitSizeXB || u.Address != address {
				continue
			}
			utxos = append(utxos, u)
		}
	} else {
		// Explicit path (dxSplitInputs): C++ needs only txid+vout (COutPoint,
		// rpcxbridge.cpp:3362-3367); resolve the entries against the wallet's
		// unspent list and reject any that are not available. The
		// wallet's data wins, and the vins follow the WALLET's order (C++
		// builds newUnspent by scanning getUnspent, :2648-2662).
		want := map[string]bool{}
		for _, u := range utxos {
			want[u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10)] = true
		}
		resolved := make([]wallet.Utxo, 0, len(utxos))
		for _, w := range walletUtxos {
			k := w.TxID + ":" + strconv.FormatUint(uint64(w.Vout), 10)
			if want[k] {
				resolved = append(resolved, w)
				delete(want, k)
			}
		}
		if len(want) > 0 {
			// C++ reports the first unavailable outpoint truncated as
			// COutPoint(<first 10 hex>, <vout>) (live Core:
			// "COutPoint(69089f37e7, 0)").
			for _, u := range utxos {
				k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
				if want[k] {
					txid := u.TxID
					if len(txid) > 10 {
						txid = txid[:10]
					}
					return nil, makeError(errBadRequest, method, "user specified utxo was not found or is not available: COutPoint("+txid+", "+strconv.FormatUint(uint64(u.Vout), 10)+")")
				}
			}
		}
		utxos = resolved
	}
	if len(utxos) == 0 {
		return nil, makeError(errBadRequest, method, "failed to get unspent transaction outputs for token "+ticker)
	}
	// C++ caps the input count at 100 (xbridgewalletconnectorbtc.cpp:2683-2685).
	if len(utxos) > 100 {
		utxos = utxos[:100]
	}
	destScript, e := legacyOutputScript(c, address)
	if e != nil {
		return nil, e
	}

	var vinsTotalXB uint64
	for _, u := range utxos {
		vinsTotalXB += camount(u)
	}
	nSplits := int(vinsTotalXB / splitSizeXB)
	if nSplits > 100 {
		nSplits = 100
	}
	if nSplits == 0 {
		return nil, makeError(errBadRequest, method, "already split all unused utxos in address ["+address+"]")
	}
	remainderXB := vinsTotalXB - uint64(nSplits)*splitSizeXB
	txFeesXB := xBridgeIntFromReal(minTxFeeWhole(cc, len(utxos), nSplits))

	outs := make([]uint64, 0, nSplits+1)
	for i := 0; i < nSplits; i++ {
		outs = append(outs, splitSizeXB)
	}
	// Change (or claw-back) — C++ xbridgewalletconnectorbtc.cpp:2707-2734.
	change := int64(remainderXB) - int64(txFeesXB)
	if change > 0 && !isDustNative(xBridgeValueFromAmount(uint64(change)), cc, relayFee, coinNativeScale(c)) {
		// Change goes to the REQUESTED address.
		outs = append(outs, uint64(change))
	} else {
		feesLeft := txFeesXB
		for feesLeft > 0 && len(outs) > 0 {
			last := outs[len(outs)-1]
			if last <= feesLeft {
				outs = outs[:len(outs)-1]
				nSplits--
				feesLeft -= last
			} else {
				outs[len(outs)-1] = last - feesLeft
				if isDustNative(xBridgeValueFromAmount(outs[len(outs)-1]), cc, relayFee, coinNativeScale(c)) {
					outs = outs[:len(outs)-1]
				}
				nSplits-- // C++ outputCount -= 1 even when the vout is kept (:2730)
				feesLeft = 0
			}
		}
	}
	if len(outs) == 0 {
		return nil, makeError(errBadRequest, method, "unable to split further, already split all unused utxos in address ["+address+"]")
	}

	tx := &coins.Tx{Version: 1}
	if cc != nil && cc.TxVersion != 0 {
		tx.Version = int32(cc.TxVersion)
	}
	for _, u := range utxos {
		hash, err := reverseTxidHex(u.TxID)
		if err != nil {
			return nil, makeError(errBadRequest, method, "bad utxo txid: "+u.TxID)
		}
		tx.Inputs = append(tx.Inputs, coins.TxIn{
			PrevOut:  coins.OutPoint{Hash: hash, Index: u.Vout},
			Sequence: 0xffffffff,
		})
	}
	for _, v := range outs {
		tx.Outputs = append(tx.Outputs, coins.TxOut{Value: splitNativeValue(c, v), ScriptPubKey: destScript})
	}

	unsigned := hex.EncodeToString(tx.Serialize())
	prevTxs := make([]wallet.PrevTx, 0, len(utxos))
	for _, u := range utxos {
		prevTxs = append(prevTxs, wallet.PrevTx{TxID: u.TxID, Vout: u.Vout, ScriptPubKey: u.ScriptPubKey, Amount: u.Amount})
	}
	signedHex, complete, signErr := conn.SignRawTransaction(unsigned, prevTxs)
	if signErr != nil {
		return nil, makeError(errBadRequest, method, signErr.Error())
	}
	if !complete {
		return nil, makeError(errBadRequest, method, "failed to sign the split transaction "+ticker)
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
			// Submit failure is 1004 BAD_REQUEST, named after the
			// actual method (rpcxbridge.cpp:3278/3392).
			return nil, makeError(errBadRequest, method, err.Error())
		}
	}
	return splitTxResult{
		Token:                ticker,
		IncludeFees:          includeFees,
		SplitAmountRequested: formatXAmount(targetXB),
		SplitAmountWithFees:  formatXAmount(splitSizeXB),
		SplitUtxoCount:       nSplits,
		SplitTotal:           formatXAmount(vinsTotalXB),
		TxID:                 txid,
		RawTx:                rawtx,
	}, nil
}

// cppDustFallback is C++'s own dust fallback constant (xbridgewalletconnectorbtc.cpp:1526):
// when no relay fee is available, C++ uses 5460 base units. go-xbridge gathers
// the relay fee live from the wallet's getinfo.relayfee (C++ :74-76); when that
// is unavailable it falls back to the conf `MinimumAmount` key — C++ maps that
// key onto the exchange wallets' dustAmount (xbridgeexchange.cpp:145) and never
// reads a `DustAmount` key (a createConf-template key it does not read back) —
// and finally to this C++-defined constant.
const cppDustFallback = 5460

// effectiveDust returns the minimum non-dust amount (base units) for a coin.
// C++ computes dustAmount = relayFee>0 ? 0.546*relayFee*COIN : 5460
// (xbridgewalletconnectorbtc.cpp:1526); go-xbridge has no live relay-fee feed,
// so when it is unavailable the conf `MinimumAmount` key serves as the
// thin-client conf fallback (C++ maps that key onto the exchange wallets'
// dustAmount, xbridgeexchange.cpp:145) and 5460 is the final fallback.
func effectiveDust(cc *config.CoinConf, relayFee float64) uint64 {
	if relayFee > 0 {
		return uint64(0.546 * relayFee * float64(cc.Coin))
	}
	if cc != nil && cc.MinimumAmount > 0 {
		return cc.MinimumAmount
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
// destinations are rejected here (BIP143 signing not yet implemented).
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
		return nil, makeError(errInvalidAddress, "dxSplit", "segwit destinations not supported for split: "+addr)
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

// prepVout is one planned output of a partial-order prep (split) transaction.
// It mirrors C++ createPartialTransaction's (address, amount) pair; every output
// of a prep tx goes to the maker's from address (xbridgeapp.cpp:1799-1817).
type prepVout struct {
	addr   string
	amount float64
}

// buildPrepTx builds, signs, and hashes the partial-order prep transaction the
// way C++ createPartialTransaction does (xbridgewalletconnectorbtc.cpp:2594-2631,
// via the generic createTransaction :2425-2453): a tx with the coin's configured
// version spending the maker's non-exact vins into P2PKH outputs (vout =
// amount*COIN, truncating) with the change clawed back into the fee when dust.
// The returned txid is the REAL tx hash (double-SHA256 of the signed raw tx),
// NOT the wallet's SendRawTransaction return — C++ derives orderPrepTx from
// createPartialTransaction's txid (xbridgeapp.cpp:1844).
func buildPrepTx(conn wallet.Connector, cc *config.CoinConf, coin coins.Coin, vins []wallet.Utxo, vouts []prepVout) (signedHex, txid string, rerr *rpcError) {
	if len(vouts) == 0 {
		return "", "", makeError(errInvalidParameters, "dxMakePartialOrder", "no prep outputs")
	}
	destScript, e := legacyOutputScript(coin, vouts[0].addr)
	if e != nil {
		return "", "", e
	}
	tx := &coins.Tx{Version: 1}
	if cc != nil && cc.TxVersion != 0 {
		tx.Version = int32(cc.TxVersion)
	}
	// C++ CTransaction(txWithTimeField) defaults nTime to time(nullptr)
	// (xbitcointransaction.h:92) when the coin serializes the time field.
	tx.WithTime = coin.TxWithTimeField
	tx.TxTime = uint32(time.Now().Unix())
	var prevTxs []wallet.PrevTx
	for _, u := range vins {
		hash, err := reverseTxidHex(u.TxID)
		if err != nil {
			return "", "", makeError(errInvalidParameters, "dxMakePartialOrder", "bad prep input txid: "+u.TxID)
		}
		tx.Inputs = append(tx.Inputs, coins.TxIn{
			PrevOut:  coins.OutPoint{Hash: hash, Index: u.Vout},
			Sequence: 0xffffffff,
		})
		prevTxs = append(prevTxs, wallet.PrevTx{TxID: u.TxID, Vout: u.Vout, ScriptPubKey: u.ScriptPubKey, Amount: u.Amount})
	}
	scale := uint64(coinScale)
	if cc != nil {
		scale = cc.Coin
	}
	for _, vo := range vouts {
		tx.Outputs = append(tx.Outputs, coins.TxOut{Value: uint64(vo.amount * float64(scale)), ScriptPubKey: destScript})
	}
	unsigned := hex.EncodeToString(tx.Serialize())
	signedHex, complete, err := conn.SignRawTransaction(unsigned, prevTxs)
	if err != nil {
		return "", "", makeError(errUnknown, "dxMakePartialOrder", err.Error())
	}
	if !complete {
		return "", "", makeError(errUnknown, "dxMakePartialOrder", "signing incomplete (wallet missing keys?)")
	}
	txid, e = txidOfRawTx(signedHex)
	if e != nil {
		return "", "", e
	}
	return signedHex, txid, nil
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

// coinNativeScale returns the native base-unit scale of coin c (10^Decimals,
// e.g. 1e8 for BTC), used where C++ references the wallet's ::COIN.
func coinNativeScale(c coins.Coin) uint64 {
	nc := uint64(1)
	for i := 0; i < c.Decimals; i++ {
		nc *= 10
	}
	return nc
}

// isP2PKHScript reports whether the hex scriptPubKey is a P2PKH script
// (25 bytes: OP_DUP OP_HASH160 <20> OP_EQUALVERIFY OP_CHECKSIG), the only
// script type C++ getUnspent keeps for splitting
// (xbridgewalletconnectorbtc.cpp:1622-1632).
func isP2PKHScript(scriptHex string) bool {
	script, err := hex.DecodeString(scriptHex)
	if err != nil || len(script) != 25 {
		return false
	}
	return script[0] == 0x76 && script[1] == 0xa9 && script[2] == 0x14 &&
		script[23] == 0x88 && script[24] == 0xac
}

// splitNativeValue converts an XBridge 1e6-scale split output into the coin's
// native base units exactly as C++ createTransaction
// (xbridgewalletconnectorbtc.cpp:2451): CTxOut(out.second * COIN) truncates the
// +1sat-nudged double from xBridgeValueFromAmount (xutil.cpp:276-280). The
// float operations run in the same order as C++, so the truncation lands on
// the same satoshi. This is intentionally separate from fromXBridgeAmt (used
// by the swap paths): the swap-side conversions are out of scope for the
// split parity item.
func splitNativeValue(c coins.Coin, xb uint64) uint64 {
	return uint64(xBridgeValueFromAmount(xb) * float64(coinNativeScale(c)))
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
// C++ needs only txid+vout per entry (COutPoint, rpcxbridge.cpp:3362-3367);
// amount/scriptPubKey/address are optional and, when absent, resolved from the
// wallet's unspent list by splitTx.
func parseUtxoParam(c coins.Coin, raw json.RawMessage) ([]wallet.Utxo, *rpcError) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, makeError(errInvalidParameters, "dxSplitInputs", "invalid utxos array")
	}
	out := make([]wallet.Utxo, 0, len(arr))
	for _, e := range arr {
		var x struct {
			TxID         string          `json:"txid"`
			Vout         json.RawMessage `json:"vout"`
			Amount       string          `json:"amount"`
			ScriptPubKey string          `json:"scriptPubKey"`
			Address      string          `json:"address"`
		}
		if err := json.Unmarshal(e, &x); err != nil {
			return nil, makeError(errInvalidParameters, "dxSplitInputs", "invalid utxos array")
		}
		// C++ reads vout via UniValue get_int() (COutPoint,
		// rpcxbridge.cpp:3362-3367): a non-integer throws the -1 envelope.
		vout, verr := uvInt(x.Vout)
		if verr != nil {
			return nil, verr
		}
		u := wallet.Utxo{TxID: x.TxID, Vout: vout, Address: x.Address, ScriptPubKey: x.ScriptPubKey}
		if x.Amount != "" {
			amt, err := coins.ParseAmount(c, x.Amount)
			if err != nil {
				return nil, makeError(errInvalidParameters, "dxSplitInputs", "invalid utxo amount")
			}
			u.Amount = amt
		}
		out = append(out, u)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// dxGetUtxos — UTXOs for a token (requires wallet).
// ---------------------------------------------------------------------------

func (h *HandlerCtx) dxGetUtxos(params []json.RawMessage) (interface{}, *rpcError) {
	// arity (1..2) is enforced by checkArity (dispatch.go).
	ticker, perr := uvStr(params, 0)
	if perr != nil {
		return nil, perr
	}
	// include_used is an isNull()-guarded bool (default false): absent/null
	// keeps the default; a present non-boolean throws.
	includeUsed := false
	if includeUsed, perr = uvBoolOpt(params, 1, false); perr != nil {
		return nil, perr
	}
	conn, e := h.connector(ticker, "dxGetUtxos")
	if e != nil {
		return nil, e
	}
	minConf := 0
	if cc, ok := h.Node.cfg().Confs[ticker]; ok {
		minConf = cc.Confirmations
	}
	utxos, err := conn.ListUnspent(minConf)
	if err != nil {
		return nil, makeError(errBadRequest, "dxGetUtxos", "failed to get unspent transaction outputs")
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
			"txid":    u.TxID,
			"vout":    u.Vout,
			"address": u.Address,
			// C++ renders the amount fixed to the coin's decimal places via
			// xBridgeStringValueFromPrice(utxo.amount, conn->COIN)
			// (rpcxbridge.cpp:3483), e.g. "1.00000000" for BTC.
			"amount":        coins.FormatAmountFixed(c, u.Amount),
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
// count so the dapp reports the wallet as connected. The remaining fields
// mirror real blocknetd's getnetworkinfo (rpc/net.cpp:495-527) for compatibility.
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
		sub = "/Blocknet:4.4.1/"
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
	return networkInfoResult{
		Version:    ver,
		Subversion: sub,
		// Alignment with real blocknetd (rpc/net.cpp:495-527): protocol
		// version 70713 (version.h), XBridge 55 / XRouter 50 (xbridge,xrouter
		// version.h), networks entries carrying proxy_randomize_credentials.
		// Fees are JSON numbers with 8 decimals (ValueFromAmount), not
		// strings. localservices reports the thin client's own capability
		// bits — deliberately not a full node's bits.
		ProtocolVersion:        70713,
		XBridgeProtocolVersion: 55,
		XRouterProtocolVersion: 50,
		LocalServices:          "000000000000000d",
		LocalRelay:             true,
		TimeOffset:             0,
		NetworkActive:          true,
		Connections:            conns,
		Networks: []networkInfoEntry{
			{Name: "ipv4", Limited: false, Reachable: true, Proxy: "", ProxyRandomizeCredentials: false},
			{Name: "ipv6", Limited: false, Reachable: true, Proxy: "", ProxyRandomizeCredentials: false},
			{Name: "onion", Limited: true, Reachable: false, Proxy: "", ProxyRandomizeCredentials: false},
		},
		RelayFee:       json.Number("0.00010000"),
		IncrementalFee: json.Number("0.00001000"),
		LocalAddresses: []interface{}{},
		Warnings:       "",
	}, nil
}
