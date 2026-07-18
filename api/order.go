// Package api is the xbridge-go JSON-RPC surface. It is a 1:1 port of
// blocknetd's XBridge RPC: the same method names, the same positional
// parameters, and the same response object schemas (down to field names and
// JSON value types — notably string amounts to preserve precision, ISO-8601
// date strings with millisecond precision, and a boolean partial_repost) that
// dapps built on the XBridge API depend on. Pointing a dapp's RPC URL at this
// server is a drop-in replacement for blocknetd's dx* calls.
//
// This is a thin client: it speaks the existing XBridge wire protocol to the
// live Blocknet service-node P2P network. See docs/protocol.md.
package api

import "xbridge-go/proto"

// Order is the internal normalized model of a live XBridge order. It carries
// everything required to render every dx* order response (dxGetOrders,
// dxGetOrder, dxGetMyOrders, dxMakeOrder, dxTakeOrder, dxCancelOrder) without
// re-deriving fields per call.
//
// Timestamps (Created/Updated) are microsecond-resolution unix epochs, matching
// the wire `created` field (C++ timeToInt → total_microseconds). Display via
// iso8601 truncates to milliseconds.
type Order struct {
	ID             [32]byte
	Type           OrderType
	From           [20]byte
	FromCurrency   string
	FromAmount     uint64 // XBridge base units (COIN=1e6)
	To             [20]byte
	ToCurrency     string
	ToAmount       uint64 // XBridge base units (COIN=1e6)
	Created        uint64 // microseconds since epoch
	Updated        uint64 // microseconds since epoch
	BlockHash      [32]byte
	PartialAllowed bool
	MinFromAmount  uint64
	OrigFromAmount uint64
	OrigToAmount   uint64
	PartialRepost  bool
	ParentID       [32]byte // zero = no parent
	Status         string   // TransactionDescr::strState() string
	MakerPubkey    string   // hex of 33-byte compressed pubkey that signed it
	MakerAddress   string   // decoded address string (may be "")
	TakerAddress   string   // decoded address string (may be "")
	BlockID        string   // hex block hash, for dxMakeOrder/dxCancelOrder
	RefundTx       string   // refund txid, for dxCancelOrder ("" when no deposit)
	BinTxId        string   // our HTLC deposit txid (dxPartialOrderChainDetails p2sh_deposits)
	OBinTxId       string   // counterparty HTLC deposit txid (p2sh_deposits_counterparty)
	Utxos          []proto.UtxoEntry
	Mine           bool // true if created locally by this node

	// --- cancel/reject + fidelity fields (mirror xbridge::TransactionDescr) ---
	// SNodePubkey is C++ sPubKey: the servicenode pubkey carried in the
	// packet header (pkt.Pubkey) of the SN that originated/broadcast the order
	// (xbridgesession.cpp:722,811). For observed orders it is also the maker
	// display key (MakerPubkey). It is set for every order we ingest.
	SNodePubkey string
	// OtherPubkey is C++ oPubKey: the counterparty's per-trade M pubkey,
	// learned from the CreateA/B body during the swap handshake.
	OtherPubkey string
	// MakerKey is C++ mPubKey: OUR per-trade M pubkey. It is populated
	// ONLY for locally-created orders (make/take); for observed orders it
	// stays "" (C++ never sets mPubKey from the snode header).
	MakerKey string
	// Reason is C++ xtx->reason (the TxCancelReason from a cancel/reject).
	Reason uint32
	// Role is the 'A'/'B'/0 maker/taker role (xbridgeapp.cpp:1751/2380).
	Role byte
	// DepositSent proxies C++ didSendDeposit() (xbridgetransactiondescr.h:491).
	DepositSent bool
	// CounterpartyRedeemed proxies C++ hasRedeemedCounterpartyDeposit()
	// (xbridgetransactiondescr.h:496).
	CounterpartyRedeemed bool
	// OrigFromCurrency/OrigToCurrency mirror C++ origFromCurrency/origToCurrency
	// (xbridgetransactiondescr.h:247/249); restored on a reject.
	OrigFromCurrency string
	OrigToCurrency   string
}

// OrderType distinguishes how an order entered our local view.
type OrderType int

const (
	OrderTypeUnknown   OrderType = iota
	OrderTypeMaker               // seen via xbcTransaction (command 3)
	OrderTypeBroadcast           // seen via xbcPendingTransaction (command 4)
)

// normalizeFromOrderBody builds an Order from a decoded xbcTransaction body.
func normalizeFromOrderBody(b *proto.OrderBody, maker string) *Order {
	return &Order{
		ID:             b.ID,
		Type:           OrderTypeMaker,
		From:           b.From,
		FromCurrency:   b.FromCurrency,
		FromAmount:     b.FromAmount,
		To:             b.To,
		ToCurrency:     b.ToCurrency,
		ToAmount:       b.ToAmount,
		Created:        b.Created,
		Updated:        b.Created,
		BlockHash:      b.BlockHash,
		PartialAllowed: b.PartialAllowed,
		MinFromAmount:  b.MinFromAmount,
		OrigFromAmount: b.FromAmount,
		OrigToAmount:   b.ToAmount,
		PartialRepost:  false,
		Status:         "open",
		MakerPubkey:    maker, // display key = snode header for observed orders
		SNodePubkey:    maker, // C++ sPubKey = pkt.Pubkey
		Utxos:          b.Utxos,
	}
}

// normalizeFromPendingBody builds an Order from a decoded xbcPendingTransaction
// body (service-node broadcast). Broadcasts omit the from/to raw addresses.
func normalizeFromPendingBody(b *proto.PendingTransactionBody, maker string) *Order {
	return &Order{
		ID:             b.ID,
		Type:           OrderTypeBroadcast,
		FromCurrency:   b.FromCurrency,
		FromAmount:     b.FromAmount,
		ToCurrency:     b.ToCurrency,
		ToAmount:       b.ToAmount,
		Created:        b.Created,
		Updated:        b.Created,
		BlockHash:      b.BlockHash,
		PartialAllowed: b.PartialAllowed,
		MinFromAmount:  b.MinFromAmount,
		OrigFromAmount: b.FromAmount,
		OrigToAmount:   b.ToAmount,
		PartialRepost:  false,
		Status:         "open",
		MakerPubkey:    maker, // display key = snode header for observed orders
		SNodePubkey:    maker, // C++ sPubKey = pkt.Pubkey
	}
}

// toOrderBase renders the fields common to all order responses.
func (o *Order) toOrderBase() orderBase {
	return orderBase{
		ID:                   orderIDString(o.ID),
		Maker:                o.FromCurrency,
		MakerSize:            formatXAmount(o.FromAmount),
		Taker:                o.ToCurrency,
		TakerSize:            formatXAmount(o.ToAmount),
		UpdatedAt:            iso8601(o.Updated),
		CreatedAt:            iso8601(o.Created),
		OrderType:            orderTypeString(o.PartialAllowed),
		PartialMinimum:       formatXAmount(o.MinFromAmount),
		PartialOrigMakerSize: formatXAmount(o.OrigFromAmount),
		PartialOrigTakerSize: formatXAmount(o.OrigToAmount),
		PartialRepost:        o.PartialRepost,
		PartialParentID:      parentIDString(o.ParentID),
		Status:               statusString(o.Status),
	}
}

func (o *Order) toListResult() orderListResult {
	return orderListResult{o.toOrderBase()}
}

// toTakeResult renders the dxTakeOrder SUCCESS response. C++ swaps
// fromCurrency<->toCurrency before rendering the real (non-dryrun) take, so
// maker becomes the order's toCurrency and taker the order's fromCurrency.
// fromSize is the (partial-adjusted) amount of toCurrency the taker sends;
// toSize is the (partial-adjusted) amount of fromCurrency the taker receives.
func (o *Order) toTakeResult(fromSize, toSize uint64) orderListResult {
	base := o.toOrderBase()
	base.Maker = o.ToCurrency
	base.MakerSize = formatXAmount(fromSize)
	base.Taker = o.FromCurrency
	base.TakerSize = formatXAmount(toSize)
	return orderListResult{base}
}

// toTakeDryrunResult renders the dxTakeOrder dryrun response. C++ renders the
// dryrun BEFORE the swap (maker=fromCurrency, taker=toCurrency), with the id set
// to the zero uint256 and status "filled".
func (o *Order) toTakeDryrunResult(fromSize, toSize uint64) orderListResult {
	base := o.toOrderBase()
	base.Maker = o.FromCurrency
	base.MakerSize = formatXAmount(fromSize)
	base.Taker = o.ToCurrency
	base.TakerSize = formatXAmount(toSize)
	base.Status = "filled"
	base.ID = "0000000000000000000000000000000000000000000000000000000000000000"
	return orderListResult{base}
}

func (o *Order) toDetailResult() orderDetailResult {
	return orderDetailResult{
		orderBase:    o.toOrderBase(),
		MakerAddress: o.MakerAddress,
		TakerAddress: o.TakerAddress,
	}
}

// makeOrderResponse renders the dxMakeOrder result. Mirrors rpcxbridge.cpp
// dxMakeOrder SUCCESS branch: the partial_* fields are "0.000000" (formatXAmount
// of zero), order_type is "exact", and status is "created".
func (o *Order) makeOrderResponse() makeOrderResult {
	base := o.toOrderBase()
	base.PartialMinimum = formatXAmount(0)
	base.PartialOrigMakerSize = formatXAmount(0)
	base.PartialOrigTakerSize = formatXAmount(0)
	base.OrderType = "exact"
	base.Status = "created"
	return makeOrderResult{
		orderBase:    base,
		MakerAddress: o.MakerAddress,
		TakerAddress: o.TakerAddress,
		BlockID:      o.BlockID,
	}
}

// makePartialOrderResponse renders the dxMakePartialOrder result. Mirrors
// rpcxbridge.cpp dxMakePartialOrder SUCCESS branch: order_type is "partial",
// the partial_* fields carry the real values (toOrderBase already does this
// when PartialAllowed is true), order_type stays "partial", partial_repost is
// the caller-supplied repost flag, and status is "created".
func (o *Order) makePartialOrderResponse(repost bool) makeOrderResult {
	base := o.toOrderBase()
	base.PartialRepost = repost
	base.Status = "created"
	return makeOrderResult{
		orderBase:    base,
		MakerAddress: o.MakerAddress,
		TakerAddress: o.TakerAddress,
		BlockID:      o.BlockID,
	}
}

func (o *Order) toCancelResult() cancelOrderResult {
	return cancelOrderResult{
		ID:           orderIDString(o.ID),
		Maker:        o.FromCurrency,
		MakerSize:    formatXAmount(o.FromAmount),
		MakerAddress: o.MakerAddress,
		Taker:        o.ToCurrency,
		TakerSize:    formatXAmount(o.ToAmount),
		TakerAddress: o.TakerAddress,
		RefundTx:     o.RefundTx,
		UpdatedAt:    iso8601(o.Updated),
		CreatedAt:    iso8601(o.Created),
		Status:       statusString(o.Status),
	}
}

// clearUsedCoins mirrors C++ TransactionDescr::clearUsedCoins() (called on a
// reject, xbridgesession.cpp:3467): it resets the swap-role state so the order
// drops back to a fresh pending order. The Orig* currencies are preserved so the
// order still renders correctly; the wallet-side coin/fee unlocking is delegated
// to the connected wallet connector (out of xbridge-go's scope as a thin client).
func (o *Order) clearUsedCoins() {
	o.Role = 0
	o.MakerKey = ""
	o.OtherPubkey = ""
	o.Reason = 0
	o.FromCurrency = o.OrigFromCurrency
	o.ToCurrency = o.OrigToCurrency
	o.FromAmount = o.OrigFromAmount
	o.ToAmount = o.OrigToAmount
}
