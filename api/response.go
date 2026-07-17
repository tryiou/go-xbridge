package api

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
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

// xbridgeErrorText mirrors util/xbridgeerror.cpp::xbridgeErrorText(code, arg):
// each case wraps the argument in the per-code message Blocknet emits. The
// `name` (C++ __FUNCTION__) is carried separately in rpcError.Name; this only
// builds the `error` string. arg is typically the currency/order-id/param the
// caller passes as makeError's third argument.
func xbridgeErrorText(code int, arg string) string {
	switch code {
	case errSuccess:
		return ""
	case errUnauthorized:
		return "Unauthorized " + arg
	case errUnknown:
		return "Internal Server Error"
	case errBadRequest:
		return "Bad Request " + arg
	case errInvalidMakeSymbol:
		return "Invalid maker symbol " + arg
	case errInvalidTakeSymbol:
		return "Invalid taker symbol " + arg
	case errInvalidDetailLevel:
		return "Invalid detail level, possible values: 1 - 3"
	case errInvalidTime:
		return "Invalid time format, ISO 8601 date format required"
	case errInvalidCurrency:
		return "Invalid coin " + arg
	case errNoSession:
		return "No session for currency " + arg
	case errInsufficientFunds:
		return "Insufficient funds for " + arg
	case errFundsNotSigned:
		return "Funds not signed for " + arg
	case errTxNotFound:
		return "Transaction " + arg + " not found"
	case errUnknownSession:
		return "Unknown session for " + arg
	case errRevertTxFailed:
		return "Revert tx failed for " + arg
	case errInvalidAmount:
		return "Invalid amount " + arg
	case errInvalidParameters:
		return "Invalid parameters: " + arg
	case errInvalidAddress:
		return "Bad address " + arg
	case errInvalidSignature:
		return "Invalid signature " + arg
	case errInvalidState:
		return "invalid transaction state " + arg
	case errNotExchangeNode:
		return "Blocknet is not running as an exchange node"
	case errDust:
		return "Amount is dust (very small)"
	case errInsufficientFundsDX:
		return "Blocknet wallet amount is too small to cover the fee payment"
	case errNoServiceNode:
		return "Could not find a service node with required services: " + arg
	case errInvalidOnchainHist:
		return "The order information could not be written to the blockchain"
	case errInvalidPartialOrder:
		return "Partial orders not allowed for this transaction"
	}
	return "invalid error value"
}

func makeError(code int, name, msg string) *rpcError {
	return &rpcError{Error: xbridgeErrorText(code, msg), Code: code, Name: name}
}

// errBadAmount is returned by parseXAmount on a malformed amount string.
var errBadAmount = errors.New("api: invalid amount")

// C++ xbridge error codes — verbatim from src/xbridge/util/xbridgeerror.h.
// These are the wire contract for dx* errors: the JSON-RPC `result` object
// `{error, code, name}` carries `code` set to exactly one of these. The Go
// constants from before (1,2,3,...) were wrong; Blocknet uses the 1000-range
// enum. rpcxbridge.cpp::makeError forwards (statusCode, __FUNCTION__, msg) to
// util/xbridgeerror.cpp::xbridgeErrorText, which prepends the per-code text.
const (
	errSuccess             = 0
	errUnauthorized        = 1001
	errUnknown             = 1002
	errBadRequest          = 1004
	errInvalidMakeSymbol   = 1011
	errInvalidTakeSymbol   = 1012
	errInvalidDetailLevel  = 1015
	errInvalidTime         = 1016
	errInvalidCurrency     = 1017
	errNoSession           = 1018
	errInsufficientFunds   = 1019
	errFundsNotSigned      = 1020
	errTxNotFound          = 1021
	errUnknownSession      = 1022
	errRevertTxFailed      = 1023
	errInvalidAmount       = 1024
	errInvalidParameters   = 1025
	errInvalidAddress      = 1026
	errInvalidSignature    = 1027
	errInvalidState        = 1028
	errNotExchangeNode     = 1029
	errDust                = 1030
	errInsufficientFundsDX = 1031
	errNoServiceNode       = 1032
	errInvalidOnchainHist  = 1033
	errInvalidPartialOrder = 1034
)

// ---------------------------------------------------------------------------
// Amount + time formatting — 1:1 with src/xbridge/util/xutil.cpp.
//
// XBridge stores order amounts in base units of COIN = 1_000_000 (6 decimal
// places of coin value). Display uses C++ xBridgeValueFromAmount
// (amt/COIN + 1/::COIN) rendered with std::fixed setprecision(
// xBridgeSignificantDigits(COIN)). xBridgeSignificantDigits(1000000) loops
// `do { n++; i/=10 } while (i>1)` and returns 6, so amounts are rendered as
// fixed 6-decimal-place strings. The RPC help-text examples showing 7 decimals
// are misleading; the code uses 6. (Verified against util/xutil.cpp:202-274.)
// ---------------------------------------------------------------------------

// coinScale is the XBridge base-unit factor: COIN = 1e6 (6 decimal places of
// coin value). Amounts are carried on the wire as uint64 base units.
const coinScale = 1_000_000

// maxXSize is the largest order size Blocknet accepts (C++ TransactionDescr::
// MAX_COIN = 100000000 whole coins, expressed in COIN base units).
const maxXSize = uint64(100000000) * coinScale

// maxXAmount is the largest base-unit value parseXAmount will accept
// (math.MaxUint64); larger inputs overflow uint64 and are rejected.
var maxXAmount = new(big.Int).SetUint64(^uint64(0))

// formatXAmount renders a base-unit (COIN=1e6) amount as the fixed 6-decimal
// string Blocknet returns. Mirrors xBridgeStringValueFromAmount: integer base
// units scaled to coin value and rendered with C++ setprecision(6) (since
// xBridgeSignificantDigits(COIN)==6). Computed with integer division to avoid
// float drift.
func formatXAmount(amt uint64) string {
	q := amt / coinScale
	r := amt % coinScale
	return strconv.FormatUint(q, 10) + "." + fmt.Sprintf("%06d", r)
}

// parseXAmount converts a user-supplied decimal amount string (e.g. "1.5") into
// XBridge base units (COIN=1e6). Mirrors xBridgeAmountFromString /
// xBridgeIntFromReal (floor(val*COIN)) but uses big.Int so values up to
// math.MaxUint64 parse exactly — float64's 2^53 precision cliff is eliminated.
// The fractional part is truncated to base-unit (6-decimal) precision; overflow
// beyond uint64 and non-numeric / negative input are rejected.
func parseXAmount(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errBadAmount
	}
	if s[0] == '-' {
		return 0, errBadAmount
	}
	dot := strings.IndexByte(s, '.')
	intStr := s
	fracStr := ""
	if dot >= 0 {
		intStr = s[:dot]
		fracStr = s[dot+1:]
	}
	for _, c := range intStr {
		if c < '0' || c > '9' {
			return 0, errBadAmount
		}
	}
	for _, c := range fracStr {
		if c < '0' || c > '9' {
			return 0, errBadAmount
		}
	}
	if intStr == "" {
		intStr = "0"
	}
	// Scale the fractional part to base-unit precision (6 decimals), dropping any
	// sub-base-unit digits (C++ truncates to COIN precision).
	frac := fracStr
	if len(frac) > 6 {
		frac = frac[:6]
	}
	for len(frac) < 6 {
		frac += "0"
	}
	combined := new(big.Int)
	if _, ok := combined.SetString(intStr+frac, 10); !ok {
		return 0, errBadAmount
	}
	if combined.Sign() < 0 || combined.Cmp(maxXAmount) > 0 {
		return 0, errBadAmount
	}
	return combined.Uint64(), nil
}

// formatXPrice renders a price ratio (double) as the fixed 6-decimal string
// Blocknet returns. Mirrors xBridgeStringValueFromPrice
// (util/xutil.cpp:209) which uses setprecision(xBridgeSignificantDigits(COIN))
// == setprecision(6).
func formatXPrice(p float64) string {
	return strconv.FormatFloat(p, 'f', 6, 64)
}

// xBridgeSourceAmountFromPrice mirrors util/xutil.cpp
// xBridgeSourceAmountFromPrice(counterpartyDestAmount, sourceAmount, destAmount):
// scales the three base-unit amounts by COIN, computes counterpartyDestAmount *
// (sourceAmount/destAmount) in double precision, adds 1 scaled unit (1/COIN),
// then scales back down and truncates — producing the taker-sent amount implied
// by a partial take. Used by dxTakeOrder to recompute the swap sizes.
func xBridgeSourceAmountFromPrice(counterpartyDestAmount, sourceAmount, destAmount uint64) uint64 {
	const c = coinScale
	if destAmount == 0 {
		return 0
	}
	cda := float64(counterpartyDestAmount * c)
	sa := float64(sourceAmount * c)
	da := float64(destAmount * c)
	v := cda*(sa/da) + 1.0 // +1 scaled unit (C++ adds 1 before the /c normalize)
	v /= float64(c)
	out := uint64(v) // truncation toward zero (v >= 0)
	if out < 1 {
		return 1
	}
	return out
}

// xBridgeValidCoin mirrors util/xutil.cpp xBridgeValidCoin: counts the decimal
// precision of an amount string (ignoring trailing zeros) and returns whether
// it is within the 6-digit limit Blocknet enforces. "25.000000" → ok;
// "25.1234567" → too precise.
func xBridgeValidCoin(amountStr string) bool {
	f := false
	n := 0
	trailingZeros := 0
	for i := 0; i < len(amountStr); i++ {
		c := amountStr[i]
		if !f && c == '.' {
			f = true
		} else if f {
			n++
			if c == '0' {
				trailingZeros++
			} else {
				trailingZeros = 0
			}
		}
	}
	return n-trailingZeros <= xBridgeSignificantDigits(coinScale)
}

// xBridgeSignificantDigits mirrors util/xutil.cpp xBridgeSignificantDigits:
// the number of significant base-unit digits for the given COIN factor. For
// COIN=1_000_000 it loops `do { n++; i/=10 } while (i>1)` → 6.
func xBridgeSignificantDigits(coin int64) int {
	n := 0
	i := coin
	for {
		n++
		i /= 10
		if i <= 1 {
			break
		}
	}
	return n
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

// stateOrdinal maps a status string to its xbridge::TransactionDescr::State
// integer, mirroring the C++ enum order. Used by guards such as dxCancelOrder's
// "cannot cancel once state >= trCreated".
func stateOrdinal(s string) int {
	switch statusString(s) {
	case "expired":
		return -1
	case "new":
		return 0
	case "offline":
		return 1
	case "open":
		return 2
	case "accepting":
		return 3
	case "hold":
		return 4
	case "initialized":
		return 5
	case "created":
		return 6
	case "signed":
		return 7
	case "commited":
		return 8
	case "finished":
		return 9
	case "rolled back":
		return 10
	case "rollback failed":
		return 11
	case "dropped":
		return 12
	case "canceled":
		return 13
	case "invalid":
		return 14
	default:
		return 0
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
