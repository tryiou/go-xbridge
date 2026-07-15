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
	RefundTx       string   // refund txid, for dxCancelOrder
	Utxos          []proto.UtxoEntry
	Mine           bool // true if created locally by this node
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
		MakerPubkey:    maker,
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
		MakerPubkey:    maker,
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

func (o *Order) toDetailResult() orderDetailResult {
	return orderDetailResult{
		orderBase:    o.toOrderBase(),
		MakerAddress: o.MakerAddress,
		TakerAddress: o.TakerAddress,
	}
}

// makeOrderResponse renders the dxMakeOrder result. Mirrors rpcxbridge.cpp
// dxMakeOrder SUCCESS branch: the partial_* fields are the literal string "0",
// order_type is "exact", and status is "created".
func (o *Order) makeOrderResponse() makeOrderResult {
	base := o.toOrderBase()
	base.PartialMinimum = "0"
	base.PartialOrigMakerSize = "0"
	base.PartialOrigTakerSize = "0"
	base.OrderType = "exact"
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
