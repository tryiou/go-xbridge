// Package api is go-xbridge's JSON-RPC surface: a drop-in replacement for
// blocknetd's dx* calls, speaking the XBridge wire protocol. See docs/protocol.md.
package api

import (
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// Order is the internal normalized model of a live XBridge order, the record
// the order-object dx* responses render from.
type Order struct {
	ID           [32]byte
	Type         OrderType
	From         [20]byte
	FromCurrency string
	FromAmount   uint64 // XBridge base units (COIN=1e6)
	To           [20]byte
	ToCurrency   string
	ToAmount     uint64 // XBridge base units (COIN=1e6)
	Created      uint64 // microseconds since epoch
	Updated      uint64 // microseconds since epoch
	BlockHash    [32]byte
	// BlockNumber is the chain tip at first sighting, stamped at ingest/make;
	// 0 = unknown, so the expiry check falls back to the time TTLs only.
	BlockNumber    uint32
	PartialAllowed bool
	MinFromAmount  uint64
	OrigFromAmount uint64
	OrigToAmount   uint64
	PartialRepost  bool
	ParentID       [32]byte // zero = no parent
	Status         string   // internal state string; statusString maps it to the dx* name
	MakerPubkey    string   // hex of 33-byte compressed pubkey that signed it
	MakerAddress   string   // decoded address string (may be "")
	TakerAddress   string   // decoded address string (may be "")
	BlockID        string   // hex block hash, for the dxMakeOrder/dxMakePartialOrder responses
	RefundTx       string   // refund txid, for dxCancelOrder ("" when no deposit)
	BinTxId        string   // our HTLC deposit txid (dxPartialOrderChainDetails p2sh_deposits)
	OBinTxId       string   // counterparty HTLC deposit txid (p2sh_deposits_counterparty)
	// Counterparty deposit check results: vout; P2SH amount and overpayment,
	// both in XBridge base units (1e6).
	OBinTxVout       uint32
	OBinTxP2SHAmount uint64
	OOverpayment     uint64
	// PrepTx is the partial-order prep/split txid (display hex) from an
	// autoSplit make; empty for exact and non-split orders.
	PrepTx string
	Utxos  []proto.UtxoEntry
	Mine   bool // true if created locally by this node

	// UsedCoins is the funding utxo set the local maker/taker selected for the order.
	UsedCoins []wallet.Utxo
	// FeeUtxos funded the service-node fee tx of a take; they stay reserved
	// while the order is live, so a second take cannot double-spend them.
	FeeUtxos []wallet.Utxo

	// UtxoCurrency is the chain the order's locked Utxos live on.
	UtxoCurrency string

	// SNodePubkey is the service node's packet key for the order; for observed
	// orders it doubles as the maker display key.
	SNodePubkey string
	// HubAddress is the chosen coordinator service node's 20-byte id
	// (coins.KeyID of its key), the route the order's packets take.
	HubAddress [20]byte
	// OtherPubkey is the counterparty's per-trade M pubkey from the CreateA/B body.
	OtherPubkey string
	// MakerKey is OUR per-trade M pubkey; set only for locally-created orders
	// (make/take) and "" for observed ones.
	MakerKey string
	// Reason is the TxCancelReason from a cancel/reject.
	Reason uint32
	// Role is the 'A'/'B'/0 maker/taker role.
	Role byte
	// DepositSent reports that our deposit tx was broadcast.
	DepositSent bool
	// CounterpartyRedeemed reports that our claim on the counterparty's deposit was broadcast.
	CounterpartyRedeemed bool
	// OrigFromCurrency/OrigToCurrency are the maker pair a reject restores.
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
		SNodePubkey:    maker,
		Utxos:          b.Utxos,
		UtxoCurrency:   b.FromCurrency,
	}
}

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
		SNodePubkey:    maker,
		HubAddress:     b.HubAddress,
	}
}

// Copy returns a deep snapshot of the order so a caller cannot mutate the store's live record.
func (o *Order) Copy() *Order {
	c := *o
	c.Utxos = make([]proto.UtxoEntry, len(o.Utxos))
	copy(c.Utxos, o.Utxos)
	c.UsedCoins = make([]wallet.Utxo, len(o.UsedCoins))
	copy(c.UsedCoins, o.UsedCoins)
	c.FeeUtxos = make([]wallet.Utxo, len(o.FeeUtxos))
	copy(c.FeeUtxos, o.FeeUtxos)
	return &c
}

// localSense reports the pair in the local node's frame: stored orders keep the
// maker-facing pair, so Role 'B' (the local taker) renders it swapped.
func (o *Order) localSense() (maker string, makerSize uint64, taker string, takerSize uint64) {
	if o.Role == 'B' {
		return o.ToCurrency, o.ToAmount, o.FromCurrency, o.FromAmount
	}
	return o.FromCurrency, o.FromAmount, o.ToCurrency, o.ToAmount
}

func (o *Order) toOrderBase() orderBase {
	maker, makerSize, taker, takerSize := o.localSense()
	return orderBase{
		ID:                   orderIDString(o.ID),
		Maker:                maker,
		MakerSize:            formatXAmount(makerSize),
		Taker:                taker,
		TakerSize:            formatXAmount(takerSize),
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

// toTakeResult renders the dxTakeOrder success response; fromSize/toSize are
// the (partial-adjusted) amounts the taker sends and receives.
func (o *Order) toTakeResult(fromSize, toSize uint64) orderListResult {
	base := o.toOrderBase()
	base.Maker = o.ToCurrency
	base.MakerSize = formatXAmount(fromSize)
	base.Taker = o.FromCurrency
	base.TakerSize = formatXAmount(toSize)
	return orderListResult{base}
}

// toTakeDryrunResult renders the pre-swap pairing (fromSize against FromCurrency);
// toTakeResult swaps the pair to the taker's view.
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
	maker, makerSize, taker, takerSize := o.localSense()
	return orderDetailResult{
		ID:                   orderIDString(o.ID),
		Maker:                maker,
		MakerSize:            formatXAmount(makerSize),
		MakerAddress:         o.MakerAddress,
		Taker:                taker,
		TakerSize:            formatXAmount(takerSize),
		TakerAddress:         o.TakerAddress,
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

func (o *Order) makeOrderResponse() makeOrderResult {
	return makeOrderResult{
		ID:                   orderIDString(o.ID),
		MakerAddress:         o.MakerAddress,
		Maker:                o.FromCurrency,
		MakerSize:            formatXAmount(o.FromAmount),
		TakerAddress:         o.TakerAddress,
		Taker:                o.ToCurrency,
		TakerSize:            formatXAmount(o.ToAmount),
		CreatedAt:            iso8601(o.Created),
		UpdatedAt:            iso8601(NowMicro()),
		BlockID:              o.BlockID,
		OrderType:            "exact",
		PartialMinimum:       "0",
		PartialOrigMakerSize: "0",
		PartialOrigTakerSize: "0",
		PartialRepost:        false,
		PartialParentID:      parentIDString([32]byte{}),
		Status:               "created",
	}
}

func (o *Order) dryrunMakeOrderResponse() dryrunMakeOrderResult {
	return dryrunMakeOrderResult{
		ID:                   orderIDString([32]byte{}),
		Maker:                o.FromCurrency,
		MakerSize:            formatXAmount(o.FromAmount),
		MakerAddress:         o.MakerAddress,
		Taker:                o.ToCurrency,
		TakerSize:            formatXAmount(o.ToAmount),
		TakerAddress:         o.TakerAddress,
		OrderType:            "exact",
		PartialMinimum:       "0",
		PartialOrigMakerSize: "0",
		PartialOrigTakerSize: "0",
		PartialRepost:        false,
		PartialParentID:      parentIDString([32]byte{}),
		Status:               "created",
	}
}

func (o *Order) makePartialOrderResponse(repost bool) makeOrderResult {
	return makeOrderResult{
		ID:                   orderIDString(o.ID),
		MakerAddress:         o.MakerAddress,
		Maker:                o.FromCurrency,
		MakerSize:            formatXAmount(o.FromAmount),
		TakerAddress:         o.TakerAddress,
		Taker:                o.ToCurrency,
		TakerSize:            formatXAmount(o.ToAmount),
		CreatedAt:            iso8601(o.Created),
		UpdatedAt:            iso8601(NowMicro()),
		BlockID:              o.BlockID,
		OrderType:            "partial",
		PartialMinimum:       formatXAmount(o.MinFromAmount),
		PartialOrigMakerSize: formatXAmount(o.OrigFromAmount),
		PartialOrigTakerSize: formatXAmount(o.OrigToAmount),
		PartialRepost:        repost,
		PartialParentID:      parentIDString(o.ParentID),
		Status:               "created",
	}
}

func (o *Order) dryrunMakePartialOrderResponse(repost bool) dryrunMakeOrderResult {
	return dryrunMakeOrderResult{
		ID:                   orderIDString([32]byte{}),
		Maker:                o.FromCurrency,
		MakerSize:            formatXAmount(o.FromAmount),
		MakerAddress:         o.MakerAddress,
		Taker:                o.ToCurrency,
		TakerSize:            formatXAmount(o.ToAmount),
		TakerAddress:         o.TakerAddress,
		OrderType:            "partial",
		PartialMinimum:       formatXAmount(o.MinFromAmount),
		PartialOrigMakerSize: formatXAmount(o.OrigFromAmount),
		PartialOrigTakerSize: formatXAmount(o.OrigToAmount),
		PartialRepost:        repost,
		PartialParentID:      parentIDString([32]byte{}),
		Status:               "created",
	}
}

func (o *Order) toCancelResult() cancelOrderResult {
	maker, makerSize, taker, takerSize := o.localSense()
	return cancelOrderResult{
		ID:           orderIDString(o.ID),
		Maker:        maker,
		MakerSize:    formatXAmount(makerSize),
		MakerAddress: o.MakerAddress,
		Taker:        taker,
		TakerSize:    formatXAmount(takerSize),
		TakerAddress: o.TakerAddress,
		RefundTx:     o.RefundTx,
		UpdatedAt:    iso8601(o.Updated),
		CreatedAt:    iso8601(o.Created),
		Status:       statusString(o.Status),
	}
}

// clearUsedCoins restores the pre-take order on reject (role/mine reset, MakerKey
// cleared, orig amounts back), releasing the locks; OtherPubkey survives deliberately.
func (o *Order) clearUsedCoins() {
	o.Role = 0
	o.Mine = false
	o.MakerKey = ""
	o.Reason = 0
	o.FromCurrency = o.OrigFromCurrency
	o.ToCurrency = o.OrigToCurrency
	o.FromAmount = o.OrigFromAmount
	o.ToAmount = o.OrigToAmount
	o.MakerAddress = ""
	o.TakerAddress = ""
	o.UsedCoins = nil
	o.FeeUtxos = nil
	o.Utxos = nil
}
