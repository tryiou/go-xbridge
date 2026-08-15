// Package api is the go-xbridge JSON-RPC surface. It is a 1:1 port of
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

import (
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// Order is the internal normalized model of a live XBridge order. It carries
// everything required to render every dx* order response (dxGetOrders,
// dxGetOrder, dxGetMyOrders, dxMakeOrder, dxTakeOrder, dxCancelOrder) without
// re-deriving fields per call.
//
// Timestamps (Created/Updated) are microsecond-resolution unix epochs, matching
// the wire `created` field (C++ timeToInt → total_microseconds). Display via
// iso8601 truncates to milliseconds.
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
	// BlockNumber is the BLOCK-chain height at which the order was first
	// observed, stamped at ingest/make from the cached BLOCK-chain tip. It is
	// the reference for the block-height expiry check
	// (Transaction::isExpiredByBlockNumber, xbridgetransaction.cpp:288-311).
	// Thin-client divergence: C++ derives the height at expiry time via
	// LookupBlockIndex(m_blockHash) (:298-306); with no chain index the Go
	// client approximates it with the tip height at first sighting. Zero means
	// "unknown" (legacy persisted records predate the stamp); the expiry sweep
	// then relies on the time-based TTLs only.
	BlockNumber    uint32
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
	// Validated counterparty deposit out-params: the C++
	// checkDepositTransaction results recorded when we accepted the
	// counterparty's deposit — oBinTxVout (counterPartyVoutN), oBinTxP2SHAmount,
	// oOverpayment (xbridgesession.cpp:2511/2571, 2973/2981). P2SHAmount and
	// Overpayment are XBridge 1e6 base; the claim spend and p2sh_deposits*
	// fidelity use them instead of the nominal amount / vout 0.
	OBinTxVout       uint32
	OBinTxP2SHAmount uint64
	OOverpayment     uint64
	// PrepTx is the partial-order prep/split transaction id (display hex) built
	// by an autoSplit make. C++ TransactionDescr::orderPrepTx; the order stays
	// pending ("open") until the split confirms and the lifecycle broadcasts it.
	// Empty for exact/non-partial orders.
	PrepTx string
	Utxos  []proto.UtxoEntry
	Mine   bool // true if created locally by this node

	// UsedCoins is the caller's selected funding utxo set (C++ xtx->usedCoins):
	// for a take it is the taker's funding selection attached to the Accepting
	// body; for a make it is the maker's selection. The deposit builder consumes
	// it instead of re-running ListUnspent.
	UsedCoins []wallet.Utxo
	// FeeUtxos is the BLOCK utxo set that funded the service-node fee tx of a
	// take. LockedUtxoInfo reserves them for the order's lifetime so a second
	// take cannot double-spend them (C++ lockFeeUtxos, xbridgeapp.cpp:2267).
	// Runtime-only: the reserved set derives from live (non-terminal) orders.
	FeeUtxos []wallet.Utxo

	// UtxoCurrency is the chain the order's locked Utxos live on: the maker's
	// FromCurrency (dxMakeOrder / a remote maker body) or the taker's funding
	// ToCurrency (TakeOrder). It mirrors C++ m_utxosDict[token] so the lock
	// exclusion is per-token (App::getAllLockedUtxos, xbridgeapp.cpp:2827),
	// not store-wide. Unset (legacy persisted records that predate the tag)
	// falls back to the Role-derived rule in Store.utxoCurrency. The tag is not
	// cleared by clearUsedCoins: a rejected take's Utxos remain claimed on the
	// same chain, so the tag stays correct.
	UtxoCurrency string

	// --- cancel/reject + fidelity fields (mirror xbridge::TransactionDescr) ---
	// SNodePubkey is C++ sPubKey: the servicenode pubkey carried in the
	// packet header (pkt.Pubkey) of the SN that originated/broadcast the order
	// (xbridgesession.cpp:722,811). For observed orders it is also the maker
	// display key (MakerPubkey). It is set for every order we ingest.
	SNodePubkey string
	// HubAddress is C++ hubAddress: the chosen coordinator servicenode's address
	// (GetID(sPubKey), xbridgetransactiondescr.h:656-663). For broadcast
	// (cmd-4) orders it is the HubAddress field of the body; the maker's SEND
	// carries it only in the transport envelope. The invariant
	// HubAddress == GetID(SNodePubkey) gates taker-side acceptance.
	HubAddress [20]byte
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
		UtxoCurrency:   b.FromCurrency,
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
		HubAddress:     b.HubAddress,
	}
}

// Copy returns a deep snapshot copy of the order. The store hands out copies
// so a caller rendering an order can never mutate the book's live record (or
// race the writer that owns it); mutations go through Store.Update instead.
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
	// Field order mirrors the C++ writer (addresses at 3/6), so this builds
	// orderDetailResult directly rather than embedding orderBase first.
	return orderDetailResult{
		ID:                   orderIDString(o.ID),
		Maker:                o.FromCurrency,
		MakerSize:            formatXAmount(o.FromAmount),
		MakerAddress:         o.MakerAddress,
		Taker:                o.ToCurrency,
		TakerSize:            formatXAmount(o.ToAmount),
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

// makeOrderResponse renders the dxMakeOrder SUCCESS result (Layout B,
// rpcxbridge.cpp:1047-1067). C++ emits created_at BEFORE updated_at (updated
// is a now() estimate), the addresses at 2/5, block_id 10th, the partial_*
// fields as literal "0", order_type "exact", and status "created".
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

// dryrunMakeOrderResponse renders the dxMakeOrder DRYRUN (rpcxbridge.cpp:
// 1004-1021): zero id, no created_at/updated_at/block_id, the addresses after
// maker_size/taker_size, partial_* literal "0".
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

// makePartialOrderResponse renders the dxMakePartialOrder SUCCESS result
// (Layout B, rpcxbridge.cpp:3070-3090): order_type "partial", the real
// partial_* values, partial_repost from the caller, status "created".
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

// dryrunMakePartialOrderResponse renders the dxMakePartialOrder DRYRUN
// (rpcxbridge.cpp:3106-3122): zero id, no timestamps/block_id, real partial
// values.
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
// to the connected wallet connector (out of go-xbridge's scope as a thin client).
func (o *Order) clearUsedCoins() {
	o.Role = 0
	o.MakerKey = ""
	o.OtherPubkey = ""
	o.Reason = 0
	o.FromCurrency = o.OrigFromCurrency
	o.ToCurrency = o.OrigToCurrency
	o.FromAmount = o.OrigFromAmount
	o.ToAmount = o.OrigToAmount
	o.UsedCoins = nil
	o.FeeUtxos = nil
}
