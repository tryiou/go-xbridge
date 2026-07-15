package api

import (
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"xbridge-go/proto"
)

// ---------------------------------------------------------------------------
// Response object schemas — field names and JSON value types match
// src/xbridge/rpcxbridge.cpp EXACTLY (1:1). Amounts are strings (precision),
// dates are ISO-8601 strings (milliseconds, see iso8601), partial_repost is a
// bool. These structs are the contract dapps depend on; do NOT rename fields or
// change value types.
// ---------------------------------------------------------------------------

// orderBase holds the fields common to every order response (dxGetOrders,
// dxGetOrder, dxGetMyOrders, dxMakeOrder, dxTakeOrder, dxCancelOrder).
type orderBase struct {
	ID                   string `json:"id"`
	Maker                string `json:"maker"`
	MakerSize            string `json:"maker_size"`
	Taker                string `json:"taker"`
	TakerSize            string `json:"taker_size"`
	UpdatedAt            string `json:"updated_at"`
	CreatedAt            string `json:"created_at"`
	OrderType            string `json:"order_type"`
	PartialMinimum       string `json:"partial_minimum"`
	PartialOrigMakerSize string `json:"partial_orig_maker_size"`
	PartialOrigTakerSize string `json:"partial_orig_taker_size"`
	PartialRepost        bool   `json:"partial_repost"`
	PartialParentID      string `json:"partial_parent_id"`
	Status               string `json:"status"`
}

// orderListResult is used by dxGetOrders, dxGetOrder and dxTakeOrder (no
// maker_address / taker_address fields).
type orderListResult struct {
	orderBase
}

// orderDetailResult is used by dxGetMyOrders (adds maker/taker addresses).
type orderDetailResult struct {
	orderBase
	MakerAddress string `json:"maker_address"`
	TakerAddress string `json:"taker_address"`
}

// makeOrderResult is used by dxMakeOrder (adds addresses + block_id).
type makeOrderResult struct {
	orderBase
	MakerAddress string `json:"maker_address"`
	TakerAddress string `json:"taker_address"`
	BlockID      string `json:"block_id"`
}

// cancelOrderResult is the dxCancelOrder response. Note: unlike dxGetMyOrders /
// dxMakeOrder, dxCancelOrder (src/xbridge/rpcxbridge.cpp ~1385) does NOT emit
// the partial_* / order_type fields — it is a distinct shape.
type cancelOrderResult struct {
	ID           string `json:"id"`
	Maker        string `json:"maker"`
	MakerSize    string `json:"maker_size"`
	MakerAddress string `json:"maker_address"`
	Taker        string `json:"taker"`
	TakerSize    string `json:"taker_size"`
	TakerAddress string `json:"taker_address"`
	RefundTx     string `json:"refund_tx"`
	UpdatedAt    string `json:"updated_at"`
	CreatedAt    string `json:"created_at"`
	Status       string `json:"status"`
}

// orderBookResult is the dxGetOrderBook response (detail/maker/taker/asks/bids).
type orderBookResult struct {
	Detail int             `json:"detail"`
	Maker  string          `json:"maker"`
	Taker  string          `json:"taker"`
	Asks   [][]interface{} `json:"asks"`
	Bids   [][]interface{} `json:"bids"`
}

// ---------------------------------------------------------------------------
// Error response — Blocknet returns errors as the *result* object
// {"error":..., "code":..., "name":...} (the JSON-RPC envelope error stays
// null). We preserve that shape exactly.
// ---------------------------------------------------------------------------

type rpcError struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
	Name  string `json:"name"`
}

func makeError(code int, name, msg string) *rpcError {
	return &rpcError{Error: msg, Code: code, Name: name}
}

// errInvalidAmount is returned by parseXAmount on a malformed amount string.
var errInvalidAmount = errors.New("api: invalid amount")

// C++ xbridge error codes (src/xbridge/util/xbridgeerror.h subset referenced by
// rpcxbridge.cpp makeError calls).
const (
	errInvalidParameters = 1
	errNoSession         = 2
	errTxNotFound        = 3
	errInvalidAddress    = 4
	errInsufficientFunds = 5
	errInvalidState      = 6
	errBadRequest        = 7
	errNotExchangeNode   = 8
	errUnknown           = 100
)

// ---------------------------------------------------------------------------
// Amount + time formatting — 1:1 with src/xbridge/util/xutil.cpp.
//
// XBridge stores order amounts in base units of COIN = 1_000_000 (6 decimal
// places of coin value). Display uses C++ xBridgeValueFromAmount
// (amt/COIN + 1/::COIN) rendered with std::fixed setprecision(
// xBridgeSignificantDigits(COIN)) = setprecision(7). So amounts are rendered as
// fixed 7-decimal-place strings. (The help-text examples showing 6 decimals are
// misleading; the code uses 7.)
// ---------------------------------------------------------------------------

// formatXAmount renders a base-unit (COIN=1e6) amount as the fixed 7-decimal
// string Blocknet returns. Mirrors xBridgeStringValueFromAmount.
func formatXAmount(amt uint64) string {
	v := float64(amt)/1e6 + 1e-8
	return strconv.FormatFloat(v, 'f', 7, 64)
}

// parseXAmount converts a user-supplied decimal amount string (e.g. "1.5") into
// XBridge base units (COIN=1e6). Mirrors xBridgeAmountFromString /
// xBridgeIntFromReal: floor(val*COIN + 1/::COIN).
func parseXAmount(s string) (uint64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	d := f*1e6 + 1e-8
	if d < 0 {
		return 0, errInvalidAmount
	}
	return uint64(d), nil
}

// formatXPrice renders a price ratio (double) as the fixed 7-decimal string
// Blocknet returns (mirrors xBridgeStringValueFromPrice).
func formatXPrice(p float64) string {
	return strconv.FormatFloat(p, 'f', 7, 64)
}

// iso8601 renders a microsecond-resolution unix timestamp as the ISO-8601
// string Blocknet returns (millisecond precision, Z suffix). Mirrors
// src/xbridge/util/xutil.cpp iso8601: "YYYY-MM-DDTHH:MM:SS.mmmZ". Zero maps to
// the epoch sentinel used by Blocknet.
func iso8601(us uint64) string {
	if us == 0 {
		return "1970-01-01T00:00:00.000Z"
	}
	t := time.Unix(0, int64(us)*1000).UTC()
	return t.Format("2006-01-02T15:04:05.000Z")
}

// orderTypeString maps the partial-order flag to the C++ "exact"/"partial"
// string returned by Transaction::orderType().
func orderTypeString(partial bool) string {
	if partial {
		return "partial"
	}
	return "exact"
}

// parentIDString renders a parent order id, or "" when there is none. Mirrors
// parseParentId (returns "" when null).
func parentIDString(id [32]byte) string {
	if isZeroID(id) {
		return ""
	}
	return hexEncode(id[:])
}

// statusString maps an internal order status to the C++ TransactionDescr::
// strState() string set used by the dx* responses. It accepts both the mapped
// strings and the raw trXxx enum names (defensive, since the two notations
// appear in different layers).
func statusString(s string) string {
	switch s {
	case "trExpired", "expired":
		return "expired"
	case "trNew", "new":
		return "new"
	case "trOffline", "offline":
		return "offline"
	case "trPending", "pending", "open":
		return "open"
	case "trAccepting", "accepting":
		return "accepting"
	case "trHold", "hold":
		return "hold"
	case "trInitialized", "initialized":
		return "initialized"
	case "trCreated", "created":
		return "created"
	case "trSigned", "signed":
		return "signed"
	case "trCommited", "trCommitted", "commited", "committed":
		return "commited"
	case "trFinished", "finished":
		return "finished"
	case "trRollback", "rolled back":
		return "rolled back"
	case "trRollbackFailed", "rollback failed":
		return "rollback failed"
	case "trDropped", "dropped":
		return "dropped"
	case "trCancelled", "canceled", "cancelled":
		return "canceled"
	case "trInvalid", "invalid":
		return "invalid"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Small shared helpers.
// ---------------------------------------------------------------------------

func isZeroID(id [32]byte) bool {
	for _, b := range id {
		if b != 0 {
			return false
		}
	}
	return true
}

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

// orderIDString renders a 32-byte order id as hex (matches uint256::GetHex).
func orderIDString(id [32]byte) string { return hexEncode(id[:]) }

var _ = proto.ProtocolVersion // keep proto import referenced for codec parity
