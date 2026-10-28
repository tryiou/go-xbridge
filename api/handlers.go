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
	"go-xbridge/version"
	"go-xbridge/wallet"
)

type xfloat8 float64

func (f xfloat8) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(float64(f), 'f', 8, 64)), nil
}

func quantizePrice(price float64) float64 {
	return math.Round(price*1e6) / 1e6
}

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

func (h *HandlerCtx) dxGetOrderFills(params []json.RawMessage) (interface{}, *rpcError) {
	// combined is parsed before maker/taker so a call with several malformed
	// params surfaces C++'s error first.
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

func (h *HandlerCtx) dxGetOrders(params []json.RawMessage) (interface{}, *rpcError) {
	// arity (==0) is enforced by checkArity (dispatch.go).
	now := NowMicro()
	orders := []*Order{}
	for _, o := range h.Store.List() {
		switch statusString(o.Status) {
		case "canceled", "finished", "expired":
			// C++ compares a whole-second clock against the µs update time and
			// truncates the difference toward zero, hence floor(now) >= Updated+61s.
			if now/1e6*1e6 >= o.Updated+61_000_000 {
				continue
			}
		}
		if !h.Config().ShowAllOrders {
			_, eFrom := h.connector(o.FromCurrency, "dxGetOrders")
			_, eTo := h.connector(o.ToCurrency, "dxGetOrders")
			if eFrom != nil || eTo != nil {
				continue
			}
		}
		orders = append(orders, o)
	}
	// Order matches C++'s id-byte-ordered map walk; Go's map order is random.
	sort.Slice(orders, func(i, j int) bool { return orderIDLess(orders[i].ID, orders[j].ID) })
	out := make([]orderListResult, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.toListResult())
	}
	return out, nil
}

func (h *HandlerCtx) dxGetOrder(params []json.RawMessage) (interface{}, *rpcError) {
	id, ok, perr := spStr(params, 0)
	if perr != nil {
		return nil, perr
	}
	if !ok {
		return nil, makeError(errInvalidParameters, "dxGetOrder", "(id)")
	}
	raw := parseOrderIDS(id)
	o := h.Store.Get(orderIDKey(id))
	if o == nil {
		o = h.Store.HistoryOrder(orderIDKey(id))
	}
	if o == nil {
		return nil, makeError(errTxNotFound, "dxGetOrder", orderIDString(raw))
	}
	if _, e := h.connector(o.FromCurrency, "dxGetOrder"); e != nil {
		return nil, e
	}
	if _, e := h.connector(o.ToCurrency, "dxGetOrder"); e != nil {
		return nil, e
	}
	return o.toListResult(), nil
}

func (h *HandlerCtx) dxGetLocalTokens(params []json.RawMessage) (interface{}, *rpcError) {
	// Sources the loaded connector map, not ExchangeWallets: the config may
	// list tickers that failed to load (C++ availableCurrencies).
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
	if h.Node == nil {
		return []string{}, nil
	}
	return h.Node.NetworkTokens(), nil
}

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

func (h *HandlerCtx) dxLoadXBridgeConf(params []json.RawMessage) (interface{}, *rpcError) {
	if h.Node == nil {
		return true, nil
	}
	if err := h.Node.reloadConf(); err != nil {
		// C++ answers success=false, not a business error, when the load fails.
		return false, nil
	}
	return true, nil
}

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
		// No wallet for the coin: C++ returns an empty array, not an error.
		return []string{}, nil
	}
	addr, err := conn.GetNewAddress()
	if err != nil {
		// C++ leaves the result array empty on failure — not a business error.
		return []string{}, nil
	}
	return []string{addr}, nil
}

func (h *HandlerCtx) dxMakeOrder(params []json.RawMessage) (interface{}, *rpcError) {
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
	useAll := true
	if len(params) >= 8 {
		if useAll, err = uvBool(params, 7); err != nil {
			return nil, err
		}
	}
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
	if dryRun {
		return o.dryrunMakeOrderResponse(), nil
	}
	return o.makeOrderResponse(), nil
}

func (h *HandlerCtx) dxMakePartialOrder(params []json.RawMessage) (interface{}, *rpcError) {
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
	if dryRun {
		return o.dryrunMakePartialOrderResponse(repost), nil
	}
	return o.makePartialOrderResponse(repost), nil
}

func (h *HandlerCtx) dxTakeOrder(params []json.RawMessage) (interface{}, *rpcError) {
	// arity (3..5) is enforced by checkArity (dispatch.go).
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
	// Same-address and amount are gated before the dryrun check so the surfaced
	// error matches C++'s precedence, even though TakeOrder re-checks both.
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

func (h *HandlerCtx) dxCancelOrder(params []json.RawMessage) (interface{}, *rpcError) {
	// arity (==1) is enforced by checkArity (dispatch.go).
	id, _, perr := spStr(params, 0)
	if perr != nil {
		return nil, perr
	}
	raw := parseOrderIDS(id)
	if idIsNull(raw) {
		return nil, makeError(errInvalidParameters, "dxCancelOrder", "Invalid order id ["+id+"]")
	}
	key := orderIDKey(id)
	o := h.Store.Get(key)
	if o == nil {
		o = h.Store.HistoryOrder(key)
	}
	if o == nil {
		return nil, makeError(errTxNotFound, "dxCancelOrder", orderIDString(raw))
	}
	// Ordinal 6 is trCreated: past it the order is committed and cannot be
	// cancelled any more.
	if stateOrdinal(o.Status) >= 6 {
		return nil, makeError(errInvalidState, "dxCancelOrder", "The order is already "+statusString(o.Status))
	}
	// CancelOrder runs before the result's connector gates: a missing from-side
	// session blocks the cancel, a missing to-side one fails after it happened.
	res, e := h.Node.CancelOrder(CancelOrderParams{ID: key})
	if e != nil {
		return nil, e
	}
	if _, e := h.connector(o.FromCurrency, "dxCancelOrder"); e != nil {
		return nil, e
	}
	if _, e := h.connector(o.ToCurrency, "dxCancelOrder"); e != nil {
		return nil, e
	}
	return res.toCancelResult(), nil
}

// defaultOrderHistoryMaxBuckets bounds the bucket count when interval_limit is
// omitted: C++'s INT_MAX default would let one request allocate millions.
const defaultOrderHistoryMaxBuckets = 100_000

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
	// granularity is parsed before start/end so a call with several malformed
	// params surfaces C++'s error first.
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

	// Validations run in xQuery's constructor order, so a request that breaks
	// several rules at once surfaces C++'s first error.
	supportedGranularity := granularity == 60 || granularity == 300 || granularity == 900 ||
		granularity == 3600 || granularity == 21600 || granularity == 86400
	if !supportedGranularity {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "granularity="+strconv.FormatInt(granularity, 10)+" must be one of: 60,300,900,3600,21600,86400")
	}
	alignedStart := int64(0)
	if start >= 0 {
		alignedStart = (start / granularity) * granularity
	}
	alignedEnd := int64(0)
	if end >= 0 {
		alignedEnd = ((end + granularity - 1) / granularity) * granularity
	}
	// XSeries::earliestTime (util/xseries.h) is 2018-02-25 00:00:00 UTC, the
	// earliest period start dxGetOrderHistory accepts.
	const xSeriesEarliest = int64(1519516800)
	if alignedStart < xSeriesEarliest {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "Start time too early.")
	}
	if alignedEnd <= alignedStart {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "Start time >= end time.")
	}
	if alignedEnd > int64(NowMicro()/1e6)+86400 {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "Start/end times are too large.")
	}
	if len(params) > 7 && (limit < 1 || limit > 2147483647) {
		return nil, makeError(errInvalidParameters, "dxGetOrderHistory", "interval_limit must be in range 1 to 2147483647.")
	}
	numBuckets := (alignedEnd - alignedStart) / granularity
	effectiveLimit := defaultOrderHistoryMaxBuckets
	if len(params) > 7 {
		effectiveLimit = limit
	}
	if numBuckets > int64(effectiveLimit) {
		numBuckets = int64(effectiveLimit)
		alignedStart = alignedEnd - numBuckets*granularity
	}

	type bucket struct {
		fills []*fillEntry
	}
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
			// For an inverse match the already-quantized price is reciprocated
			// without re-quantizing, and volume moves to the fill's taker side.
			price := quantizePrice(takerNum / makerNum)
			vol := makerNum
			if inverse {
				if price == 0 {
					price = 0 // C++ inverse() yields 0 rather than dividing by it
				} else {
					price = 1.0 / price
				}
				vol = takerNum
			}
			// open doubles as the unset flag, so a zero-quantized first price is
			// replaced by the next fill.
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
		// The row timestamp is the bucket start: C++'s default
		// interval_timestamp is at_start (util/xseries.h).
		row := []interface{}{iso8601(uint64(bucketStart) * 1e6), xfloat8(low), xfloat8(high), xfloat8(open), xfloat8(close), xfloat8(volume)}
		if orderIDs {
			row = append(row, ids)
		}
		out = append(out, row)
	}
	return out, nil
}

type obEntry struct {
	price    float64
	priceStr string
	amount   uint64
	id       string
	rawID    [32]byte
}

func floatCompare(a, b float64) bool {
	const epsilon = 2.220446049250313e-16 // std::numeric_limits<double>::epsilon()
	return (math.Abs(a-b)/math.Abs(a) <= epsilon) && (math.Abs(a-b)/math.Abs(b) <= epsilon)
}

// countAtPrice counts floatCompare-equal entries across the whole side list,
// not just the window being rendered.
func countAtPrice(list []obEntry, price float64) int {
	n := 0
	for _, e := range list {
		if floatCompare(e.price, price) {
			n++
		}
	}
	return n
}

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

// bestOf returns the lowest-price ask or highest-price bid; list must be
// non-empty.
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

	// Two independent passes, not one with an else: when maker and taker are
	// the same currency an order is both an ask and a bid.
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
			p := xBridgeValueFromAmount(o.ToAmount) / xBridgeValueFromAmount(o.FromAmount)
			asks = append(asks, obEntry{price: p, priceStr: formatXPrice(p), amount: o.FromAmount, id: orderIDString(o.ID), rawID: o.ID})
		}
		if strings.EqualFold(o.FromCurrency, taker) && strings.EqualFold(o.ToCurrency, maker) {
			p := xBridgeValueFromAmount(o.FromAmount) / xBridgeValueFromAmount(o.ToAmount)
			bids = append(bids, obEntry{price: p, priceStr: formatXPrice(p), amount: o.ToAmount, id: orderIDString(o.ID), rawID: o.ID})
		}
	}

	// Both sides: price desc, then ascending raw-id (orderIDLess). This mirrors
	// C++ exactly for detail 1/4 (min/max_element over the id-sorted map); a superset for detail 2/3's price-only sort.
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

	// Sides start non-nil so an empty book encodes as "[]" and never "null".
	res := orderBookResult{Detail: detail, Maker: maker, Taker: taker,
		Asks: [][]interface{}{}, Bids: [][]interface{}{}}

	switch detail {
	case 1:
		if len(asks) > 0 {
			best := bestOf(asks, false)
			res.Asks = append(res.Asks, []interface{}{best.priceStr, formatXAmount(best.amount), countAtPrice(asks, best.price)})
		}
		if len(bids) > 0 {
			best := bestOf(bids, true)
			res.Bids = append(res.Bids, []interface{}{best.priceStr, formatXAmount(best.amount), countAtPrice(bids, best.price)})
		}
	case 2:
		// i advances unconditionally after the equal-price merge, so this window
		// emits roughly every other entry — intentional, not an off-by-one.
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
		// Ask-side: i starts at len(asks)-bound while the merge gate tests i < bound —
		// faithful to C++; when len(asks) >= 2*bound no merge ever fires.
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

func (h *HandlerCtx) dxGetTokenBalances(params []json.RawMessage) (interface{}, *rpcError) {
	out := map[string]string{}
	if h.Node == nil || h.Node.cfg() == nil || h.Node.cfg().Connectors == nil {
		return out, nil
	}
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
			avail := total
			if l := lockedOf(utxos); l < avail {
				avail -= l
			}
			out[ticker] = formatBalanceNative(c, avail)
		}
	}
	return out, nil
}

func (h *HandlerCtx) dxGetMyOrders(params []json.RawMessage) (interface{}, *rpcError) {
	orders := []*Order{}
	seen := map[string]bool{}
	for _, o := range h.Store.Mine() {
		seen[hexEncode(o.ID[:])] = true
		orders = append(orders, o)
	}
	for _, e := range h.Store.History() {
		o := e.Order
		if o == nil || !o.Mine {
			continue
		}
		switch statusString(e.Status) {
		case "finished", "canceled":
			// Dedup keeps the LIVE record, not C++'s smallest-txtime one: for an
			// id present in both the live order is the current one.
			if seen[hexEncode(o.ID[:])] {
				continue
			}
			seen[hexEncode(o.ID[:])] = true
			orders = append(orders, o)
		}
	}
	sort.SliceStable(orders, func(i, j int) bool {
		return orders[i].Updated < orders[j].Updated
	})
	out := make([]orderDetailResult, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.toDetailResult())
	}
	return out, nil
}

func (h *HandlerCtx) dxGetMyPartialOrderChain(params []json.RawMessage) (interface{}, *rpcError) {
	id, perr := rtcStr(params, 0)
	if perr != nil {
		return nil, perr
	}
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

// partialOrderChain models the full lineage — ancestors and every descendant —
// and does not mirror getPartialOrderChain's narrower, order-dependent walk.
func (h *HandlerCtx) partialOrderChain(id string) []*Order {
	idx := map[string]*Order{}
	add := func(o *Order) {
		hex := hexEncode(o.ID[:])
		if _, ok := idx[hex]; ok {
			return
		}
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

	sort.SliceStable(chain, func(i, j int) bool {
		return chain[i].Created < chain[j].Created
	})
	return chain
}

func (h *HandlerCtx) dxPartialOrderChainDetails(params []json.RawMessage) (interface{}, *rpcError) {
	id, perr := rtcStr(params, 0)
	if perr != nil {
		return nil, perr
	}
	raw := parseOrderIDS(id)
	if idIsNull(raw) {
		return nil, makeError(errInvalidParameters, "dxPartialOrderChainDetails", "bad order id")
	}
	chain := h.partialOrderChain(orderIDKey(id))
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
			// Ordinal 2 is trPending: every state up to it counts as open, not
			// just the literal "open".
			if stateOrdinal(t.Status) <= 2 {
				totalOpen++
			}
		}
		orderIDs = append(orderIDs, orderIDString(t.ID))
		// One entry per chain order keeps P2SHDeposits indexable against Orders
		// by position.
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

func (h *HandlerCtx) dxGetLockedUtxos(_ []json.RawMessage) (interface{}, *rpcError) {
	// arity (0..1) is enforced by checkArity (dispatch.go); each call that passes
	// fails 1029 before reading params — this thin client never runs the exchange, so no list is served.
	return nil, makeError(errNotExchangeNode, "dxGetLockedUtxos", "")
}

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

func (h *HandlerCtx) dxFlushCancelledOrders(params []json.RawMessage) (interface{}, *rpcError) {
	var ageMillis int
	switch {
	case len(params) == 0:
		ageMillis = 0
	case len(params) == 1:
		v, _, err := spInt(params, 0)
		if err != nil {
			return nil, err
		}
		// The age is held in 32 bits: out-of-range values wrap mod 2^32, and a
		// wrapped negative fails the >= 0 gate below.
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
	return flushCancelledResult{
		AgeMillis:        int64(ageMillis),
		Now:              iso8601(now),
		DurationMicrosec: int64(dur),
		FlushedOrders:    orders,
	}, nil
}

func (h *HandlerCtx) dxGetTradingData(params []json.RawMessage) (interface{}, *rpcError) {
	// arity (0..2) is enforced by checkArity (dispatch.go).
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

func (h *HandlerCtx) dxSplitAddress(params []json.RawMessage) (interface{}, *rpcError) {
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
	// arity (3..7) is enforced by checkArity (dispatch.go).
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
	// params[6] is type-checked before any coin lookup, so a bad type surfaces
	// as a -1 envelope error rather than "unknown coin".
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
	lockedKeys, _ := h.Store.LockedUtxoInfo()
	for _, u := range utxos {
		k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
		if lockedKeys[k] {
			return nil, makeError(errBadRequest, "dxSplitInputs", "Cannot split utxo already in use: "+k)
		}
	}
	return h.splitTx(ticker, splitAmt, address, includeFees, showRawTx, submit, utxos, "dxSplitInputs")
}

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
	cc, cerr := h.Node.confFor(ticker)
	if cerr != nil {
		return nil, makeError(errNoSession, method, ticker)
	}
	if isDustNative(xBridgeValueFromAmount(targetXB), cc, coinNativeScale(c)) {
		return nil, makeError(errBadRequest, method, "split amount is dust ["+formatXAmount(targetXB)+"]")
	}
	if a, derr := c.DecodeAddress(address); derr != nil || (a.Kind != coins.P2PKH && a.Kind != coins.P2SH) {
		return nil, makeError(errBadRequest, method, "address is invalid or not in the wallet for token "+ticker)
	}

	// Fee provisions round UP to XBridge base units: split sizes must cover
	// the exact-sats fees the funding session will later need (a truncated
	// floor starves per-fill deposits on 678-class coins).
	fee1XB := ceilFeeXB(cc, estimateFee(cc, 1, 3))
	fee2XB := ceilFeeXB(cc, estimateFee(cc, 1, 1))
	feesPerUtxoXB := fee1XB + fee2XB
	splitSizeXB := targetXB
	if includeFees {
		splitSizeXB += feesPerUtxoXB
	}

	minConf := cc.Confirmations
	walletUtxos, lerr := conn.ListUnspent(minConf)
	if lerr != nil {
		return nil, makeError(errBadRequest, method, lerr.Error())
	}
	// The P2PKH filter covers the auto and the explicit path alike, so a
	// user-specified non-P2PKH utxo fails as unavailable (C++ getUnspent).
	filtered := make([]wallet.Utxo, 0, len(walletUtxos))
	for _, u := range walletUtxos {
		if isP2PKHScript(u.ScriptPubKey) {
			filtered = append(filtered, u)
		}
	}
	walletUtxos = filtered
	lockedKeys, _ := h.Store.LockedUtxoInfo()
	if len(utxos) == 0 {
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
		// Requested outpoints resolve against the wallet's unspent list: the
		// wallet's data and ordering win, and an unavailable one is rejected.
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
	txFeesXB := ceilFeeXB(cc, estimateFee(cc, len(utxos), nSplits))

	outs := make([]uint64, 0, nSplits+1)
	for i := 0; i < nSplits; i++ {
		outs = append(outs, splitSizeXB)
	}
	// A change is emitted only when positive and non-dust; otherwise the fee
	// is clawed back from the split outputs.
	change := int64(remainderXB) - int64(txFeesXB)
	if change > 0 && !isDustNative(xBridgeValueFromAmount(uint64(change)), cc, coinNativeScale(c)) {
		// Change pays the requested address, not a fresh one.
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
				if isDustNative(xBridgeValueFromAmount(outs[len(outs)-1]), cc, coinNativeScale(c)) {
					outs = outs[:len(outs)-1]
				}
				nSplits-- // the count drops even when this output is kept
				feesLeft = 0
			}
		}
	}
	if len(outs) == 0 {
		return nil, makeError(errBadRequest, method, "unable to split further, already split all unused utxos in address ["+address+"]")
	}

	tx := &coins.Tx{Version: 1}
	if cc.TxVersion != 0 {
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
		// Exact integer XB -> native conversion (== C++ xBridgeDescrToSats,
		// splitUtxos :2762): split outputs carry no nudge.
		tx.Outputs = append(tx.Outputs, coins.TxOut{Value: fromXBridgeAmt(c, v), ScriptPubKey: destScript})
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
			return nil, makeError(errBadRequest, method, err.Error())
		}
		// No order context: a split is a wallet utility tx, not an order.
		if h.Node != nil {
			h.Node.recordBroadcast("", broadcastSplit, ticker, txid, signedHex)
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

// effectiveDust is the coin's dust threshold in native base units, from its
// conf and nothing else. A nil conf has no threshold, so it yields 0 rather
// than dereferencing; callers that need a coin must check the conf is present.
func effectiveDust(cc *config.CoinConf) uint64 {
	if cc == nil {
		return 0
	}
	return cc.DustAmount
}

func estimateFee(cc *config.CoinConf, nIn, nOut int) uint64 {
	// The 192/34 vbyte estimate must match C++'s, or our fee falls outside the
	// counterpartyFees >= fee*0.95 acceptance band and orders get rejected.
	vsize := 192*nIn + 34*nOut
	var fee uint64
	if cc == nil || cc.FeePerByte == 0 {
		fee = uint64(vsize * 2)
	} else {
		fee = cc.FeePerByte * uint64(vsize)
	}
	if cc != nil && cc.MinTxFee > 0 && fee < cc.MinTxFee {
		fee = cc.MinTxFee
	}
	return fee
}

// legacyOutputScript builds a P2PKH/P2SH script only: split outputs are legacy,
// so a segwit destination is rejected with errInvalidAddress.
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

type prepVout struct {
	addr   string
	amount uint64 // native base units (exact integer, == C++ prep vouts)
}

// buildPrepTx returns the local double-SHA256 of the signed prep tx, never a
// wallet send result: the order's prep id is derived from that hash.
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
	for _, vo := range vouts {
		tx.Outputs = append(tx.Outputs, coins.TxOut{Value: vo.amount, ScriptPubKey: destScript})
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

// toXBridgeAmt scales for response strings only: raw tx output values stay in
// the coin's native base units.
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

func coinNativeScale(c coins.Coin) uint64 {
	nc := uint64(1)
	for i := 0; i < c.Decimals; i++ {
		nc *= 10
	}
	return nc
}

func isP2PKHScript(scriptHex string) bool {
	script, err := hex.DecodeString(scriptHex)
	if err != nil || len(script) != 25 {
		return false
	}
	return script[0] == 0x76 && script[1] == 0xa9 && script[2] == 0x14 &&
		script[23] == 0x88 && script[24] == 0xac
}

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

// parseUtxoParam needs only txid and vout per entry: the remaining fields are
// optional and, when absent, resolved from the wallet's unspent list in splitTx.
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

func (h *HandlerCtx) dxGetUtxos(params []json.RawMessage) (interface{}, *rpcError) {
	// arity (1..2) is enforced by checkArity (dispatch.go).
	ticker, perr := uvStr(params, 0)
	if perr != nil {
		return nil, perr
	}
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
			"amount":        coins.FormatAmountFixed(c, u.Amount),
			"scriptPubKey":  u.ScriptPubKey,
			"confirmations": u.Confirmations,
			"orderid":       orderid,
		})
	}
	return out, nil
}

// getnetworkinfo exists because BLOCK-DX pings it for its wallet-version gate
// before it will talk to the wallet; the remaining fields mirror blocknetd.
func (h *HandlerCtx) getNetworkInfo(params []json.RawMessage) (interface{}, *rpcError) {
	if len(params) != 0 {
		return nil, makeError(errInvalidParameters, "getnetworkinfo", "no parameters")
	}
	ver := h.Config().WalletVersion
	if ver == 0 {
		ver = version.DefaultWalletVersion
	}
	sub := h.Config().WalletVersionStr
	if sub == "" {
		sub = version.DefaultWalletVersionStr
	}
	conns := 0
	if h.Node != nil && h.Node.conn != nil {
		// Discovery reports its live peer count; a single explicitly configured
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
		// localservices carries this thin client's own capability bits,
		// deliberately not a full node's.
		ProtocolVersion:        version.BitcoinProtocolVersion,
		XBridgeProtocolVersion: int(version.XBridgeProtocolVersion),
		XRouterProtocolVersion: version.XRouterProtocolVersion,
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
