package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"go-xbridge/coins"
	"go-xbridge/swap"
)

// JSON field names, value types, and declaration order must match the C++ dx*
// responses byte-for-byte (rpcxbridge.cpp): encoding/json emits fields in declaration order.

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

type orderListResult struct {
	orderBase
}

type orderDetailResult struct {
	ID                   string `json:"id"`
	Maker                string `json:"maker"`
	MakerSize            string `json:"maker_size"`
	MakerAddress         string `json:"maker_address"`
	Taker                string `json:"taker"`
	TakerSize            string `json:"taker_size"`
	TakerAddress         string `json:"taker_address"`
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

// p2sh_deposits / p2sh_deposits_counterparty hold one entry per chain order
// (empty string when there is none), parallel to `orders` for indexing.
type partialChainDetailsResult struct {
	FirstOrderID             string   `json:"first_order_id"`
	Maker                    string   `json:"maker"`
	MakerAddress             string   `json:"maker_address"`
	Taker                    string   `json:"taker"`
	TakerAddress             string   `json:"taker_address"`
	PartialMinimum           string   `json:"partial_minimum"`
	PartialOrigMakerSize     string   `json:"partial_orig_maker_size"`
	PartialOrigTakerSize     string   `json:"partial_orig_taker_size"`
	FirstOrderTime           string   `json:"first_order_time"`
	LastOrderTime            string   `json:"last_order_time"`
	TotalReportedSent        string   `json:"total_reported_sent"`
	TotalReportedReceived    string   `json:"total_reported_received"`
	TotalReportedNotsent     string   `json:"total_reported_notsent"`
	TotalReportedNotreceived string   `json:"total_reported_notreceived"`
	TotalOrdersOpen          int      `json:"total_orders_open"`
	TotalOrdersFinished      int      `json:"total_orders_finished"`
	TotalOrdersCanceled      int      `json:"total_orders_canceled"`
	Orders                   []string `json:"orders"`
	P2SHDeposits             []string `json:"p2sh_deposits"`
	P2SHDepositsCounterparty []string `json:"p2sh_deposits_counterparty"`
}

type networkInfoEntry struct {
	Name                      string `json:"name"`
	Limited                   bool   `json:"limited"`
	Reachable                 bool   `json:"reachable"`
	Proxy                     string `json:"proxy"`
	ProxyRandomizeCredentials bool   `json:"proxy_randomize_credentials"`
}

// Fees keep their exact literal as json.Number — float64 would marshal
// 0.00010000 as 0.0001 and break byte parity with the C++ response.
type networkInfoResult struct {
	Version                int                `json:"version"`
	Subversion             string             `json:"subversion"`
	ProtocolVersion        int                `json:"protocolversion"`
	XBridgeProtocolVersion int                `json:"xbridgeprotocolversion"`
	XRouterProtocolVersion int                `json:"xrouterprotocolversion"`
	LocalServices          string             `json:"localservices"`
	LocalRelay             bool               `json:"localrelay"`
	TimeOffset             int                `json:"timeoffset"`
	NetworkActive          bool               `json:"networkactive"`
	Connections            int                `json:"connections"`
	Networks               []networkInfoEntry `json:"networks"`
	RelayFee               json.Number        `json:"relayfee"`
	IncrementalFee         json.Number        `json:"incrementalfee"`
	LocalAddresses         []interface{}      `json:"localaddresses"`
	Warnings               string             `json:"warnings"`
}

type flushCancelledResult struct {
	AgeMillis        int64             `json:"ageMillis"`
	Now              string            `json:"now"`
	DurationMicrosec int64             `json:"durationMicrosec"`
	FlushedOrders    []flushedOrderOut `json:"flushedOrders"`
}

type flushedOrderOut struct {
	ID       string `json:"id"`
	Txtime   string `json:"txtime"`
	UseCount int    `json:"use_count"`
}

type splitTxResult struct {
	Token                string `json:"token"`
	IncludeFees          bool   `json:"include_fees"`
	SplitAmountRequested string `json:"split_amount_requested"`
	SplitAmountWithFees  string `json:"split_amount_with_fees"`
	SplitUtxoCount       int    `json:"split_utxo_count"`
	SplitTotal           string `json:"split_total"`
	TxID                 string `json:"txid"`
	RawTx                string `json:"rawtx"`
}

type makeOrderResult struct {
	ID                   string `json:"id"`
	MakerAddress         string `json:"maker_address"`
	Maker                string `json:"maker"`
	MakerSize            string `json:"maker_size"`
	TakerAddress         string `json:"taker_address"`
	Taker                string `json:"taker"`
	TakerSize            string `json:"taker_size"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
	BlockID              string `json:"block_id"`
	OrderType            string `json:"order_type"`
	PartialMinimum       string `json:"partial_minimum"`
	PartialOrigMakerSize string `json:"partial_orig_maker_size"`
	PartialOrigTakerSize string `json:"partial_orig_taker_size"`
	PartialRepost        bool   `json:"partial_repost"`
	PartialParentID      string `json:"partial_parent_id"`
	Status               string `json:"status"`
}

type dryrunMakeOrderResult struct {
	ID                   string `json:"id"`
	Maker                string `json:"maker"`
	MakerSize            string `json:"maker_size"`
	MakerAddress         string `json:"maker_address"`
	Taker                string `json:"taker"`
	TakerSize            string `json:"taker_size"`
	TakerAddress         string `json:"taker_address"`
	OrderType            string `json:"order_type"`
	PartialMinimum       string `json:"partial_minimum"`
	PartialOrigMakerSize string `json:"partial_orig_maker_size"`
	PartialOrigTakerSize string `json:"partial_orig_taker_size"`
	PartialRepost        bool   `json:"partial_repost"`
	PartialParentID      string `json:"partial_parent_id"`
	Status               string `json:"status"`
}

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

type orderBookResult struct {
	Detail int             `json:"detail"`
	Maker  string          `json:"maker"`
	Taker  string          `json:"taker"`
	Asks   [][]interface{} `json:"asks"`
	Bids   [][]interface{} `json:"bids"`
}

// MarshalJSON keeps detail 4's side flat [price, amount, ids] to match the C++ writer.
func (o orderBookResult) MarshalJSON() ([]byte, error) {
	asks, bids := interface{}(o.Asks), interface{}(o.Bids)
	if o.Asks == nil {
		asks = [][]interface{}{}
	}
	if o.Bids == nil {
		bids = [][]interface{}{}
	}
	if o.Detail == 4 {
		if len(o.Asks) > 0 {
			asks = o.Asks[0]
		} else {
			asks = []interface{}{}
		}
		if len(o.Bids) > 0 {
			bids = o.Bids[0]
		} else {
			bids = []interface{}{}
		}
	}
	return json.Marshal(struct {
		Detail int         `json:"detail"`
		Maker  string      `json:"maker"`
		Taker  string      `json:"taker"`
		Asks   interface{} `json:"asks"`
		Bids   interface{} `json:"bids"`
	}{o.Detail, o.Maker, o.Taker, asks, bids})
}

type rpcError struct {
	Error    string `json:"error"`
	Code     int    `json:"code"`
	Name     string `json:"name"`
	envelope bool   // transport-class error (envelope channel), not a business code (result)
}

func makeEnvelopeError(code int, msg string) *rpcError {
	return &rpcError{Code: code, Error: msg, envelope: true}
}

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

var errBadAmount = errors.New("api: invalid amount")

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

const coinScale = 1_000_000

const maxXSize = uint64(100000000) * coinScale

var maxXAmount = new(big.Int).SetUint64(^uint64(0))

func formatXAmount(amt uint64) string {
	q := amt / coinScale
	r := amt % coinScale
	return strconv.FormatUint(q, 10) + "." + fmt.Sprintf("%06d", r)
}

// Rounds to the nearest 6th decimal; formatXAmount truncates instead so an
// order amount never rounds up.
func formatBalanceNative(c coins.Coin, native uint64) string {
	nc := uint64(1)
	for i := 0; i < c.Decimals; i++ {
		nc *= 10
	}
	if nc == 0 {
		nc = 1
	}
	whole := float64(native) / float64(nc)
	return strconv.FormatFloat(whole, 'f', 6, 64)
}

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

func formatXPrice(p float64) string {
	return strconv.FormatFloat(p, 'f', 6, 64)
}

// Scales each amount in float64 before the COIN multiply (C++ scales as
// CAmount int64): maxXSize*COIN would wrap uint64.
func xBridgeSourceAmountFromPrice(counterpartyDestAmount, sourceAmount, destAmount uint64) uint64 {
	const c = coinScale
	if destAmount == 0 {
		return 0
	}
	cda := float64(counterpartyDestAmount) * c
	sa := float64(sourceAmount) * c
	da := float64(destAmount) * c
	v := cda*(sa/da) + 1.0 // +1 scaled unit (C++ adds 1 before the /c normalize)
	// C++ truncates the +1'd double before the /COIN divide (dividing first
	// can round a unit up); v ≥ MaxInt64 divides first, clamping at MaxUint64.
	var out uint64
	if v < float64(math.MaxInt64) {
		out = uint64(v) / c
	} else if q := v / float64(c); q < float64(^uint64(0)) {
		out = uint64(q)
	} else {
		out = ^uint64(0)
	}
	if out < 1 {
		return 1
	}
	return out
}

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

func iso8601(us uint64) string {
	if us == 0 {
		return "1970-01-01T00:00:00.000Z"
	}
	t := time.Unix(0, int64(us)*1000).UTC()
	return t.Format("2006-01-02T15:04:05.000Z")
}

func orderTypeString(partial bool) string {
	if partial {
		return "partial"
	}
	return "exact"
}

func parentIDString(id [32]byte) string {
	if isZeroID(id) {
		return ""
	}
	return orderIDString(id)
}

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

func stateOrdinal(s string) int {
	return swap.DescrStateOrdinal(statusString(s))
}

func isZeroID(id [32]byte) bool {
	for _, b := range id {
		if b != 0 {
			return false
		}
	}
	return true
}

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

// Display hex is uint256 byte-reversed (GetHex): ids stay raw wire bytes
// internally and are reversed only when rendered.
func orderIDString(id [32]byte) string {
	var r [32]byte
	for i := 0; i < 32; i++ {
		r[31-i] = id[i]
	}
	return hexEncode(r[:])
}

// parseOrderIDS maps display-hex back to the raw 32-byte id, tolerantly like
// uint256S: empty or non-hex input yields the zero id, never an error.
func parseOrderIDS(s string) [32]byte {
	var id [32]byte
	i := 0
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	if i+1 < len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') {
		i += 2
	}
	start := i
	for i < len(s) && hexDigit(s[i]) >= 0 {
		i++
	}
	n := 0
	for j := i - 1; j >= start && n < 64; j-- {
		if n&1 == 0 {
			id[n>>1] = byte(hexDigit(s[j]))
		} else {
			id[n>>1] |= byte(hexDigit(s[j])) << 4
		}
		n++
	}
	return id
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func isSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

func idIsNull(id [32]byte) bool { return id == [32]byte{} }

// Orders by raw bytes (LSB first, as std::map<uint256> compares), NOT by
// display-hex ascending.
func orderIDLess(a, b [32]byte) bool {
	return bytes.Compare(a[:], b[:]) < 0
}

func orderIDKey(id string) string {
	raw := parseOrderIDS(id)
	return hexEncode(raw[:])
}
