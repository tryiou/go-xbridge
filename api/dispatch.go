package api

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// HandlerCtx carries the dependencies each dx* handler needs.
type HandlerCtx struct {
	Store *Store
	Node  *Node
}

// Config returns the live node configuration. It is derived from the Node so
// there is a single mutable config slot (hot-reload via dxLoadXBridgeConf swaps
// the Node's config atomically; handlers never read a stale copy).
func (h *HandlerCtx) Config() *Config {
	if h.Node == nil {
		return &Config{}
	}
	if c := h.Node.cfg(); c != nil {
		return c
	}
	return &Config{}
}

// Handler implements one dx* method. It returns the JSON result (any
// marshalable value) and, on a business error, an *rpcError — which the server
// serializes as the JSON-RPC *result* object (Blocknet returns errors as the
// result, leaving the envelope error null).
type Handler func(h *HandlerCtx, params []json.RawMessage) (interface{}, *rpcError)

// dispatch maps every dx* method name to its handler. The set and names match
// src/xbridge/rpcxbridge.cpp commands[]. Only dxGetTradingData is exposed for
// trade history (gettradingdata is not part of the Go XBridge interface).
var dispatch = map[string]Handler{
	"dxGetOrderFills":            (*HandlerCtx).dxGetOrderFills,
	"dxGetOrders":                (*HandlerCtx).dxGetOrders,
	"dxGetOrder":                 (*HandlerCtx).dxGetOrder,
	"dxGetLocalTokens":           (*HandlerCtx).dxGetLocalTokens,
	"dxLoadXBridgeConf":          (*HandlerCtx).dxLoadXBridgeConf,
	"dxGetNewTokenAddress":       (*HandlerCtx).dxGetNewTokenAddress,
	"dxGetNetworkTokens":         (*HandlerCtx).dxGetNetworkTokens,
	"dxMakeOrder":                (*HandlerCtx).dxMakeOrder,
	"dxMakePartialOrder":         (*HandlerCtx).dxMakePartialOrder,
	"dxTakeOrder":                (*HandlerCtx).dxTakeOrder,
	"dxCancelOrder":              (*HandlerCtx).dxCancelOrder,
	"dxGetOrderHistory":          (*HandlerCtx).dxGetOrderHistory,
	"dxGetOrderBook":             (*HandlerCtx).dxGetOrderBook,
	"dxGetTokenBalances":         (*HandlerCtx).dxGetTokenBalances,
	"dxGetMyOrders":              (*HandlerCtx).dxGetMyOrders,
	"dxGetMyPartialOrderChain":   (*HandlerCtx).dxGetMyPartialOrderChain,
	"dxPartialOrderChainDetails": (*HandlerCtx).dxPartialOrderChainDetails,
	"dxGetLockedUtxos":           (*HandlerCtx).dxGetLockedUtxos,
	"dxFlushCancelledOrders":     (*HandlerCtx).dxFlushCancelledOrders,
	"dxGetTradingData":           (*HandlerCtx).dxGetTradingData,
	"dxSplitAddress":             (*HandlerCtx).dxSplitAddress,
	"dxSplitInputs":              (*HandlerCtx).dxSplitInputs,
	"dxGetUtxos":                 (*HandlerCtx).dxGetUtxos,
	"getnetworkinfo":             (*HandlerCtx).getNetworkInfo,
}

// Lookup returns the handler for a method name, or nil if unknown.
func Lookup(method string) Handler {
	return dispatch[method]
}

// ---------------------------------------------------------------------------
// Arity gates.
//
// C++ enforces each dx* method's param count up front. The old-style methods
// return a business 1025 result error with the exact param-list string
// (uret(makeError(INVALID_PARAMETERS, __FUNCTION__, <msg>))); the throw
// methods throw the full RPCHelpMan help text as an envelope error code -1
// (rpc/server.cpp:584-586). checkArity runs before the handler so the gates are
// centralized and match C++ per method.
// ---------------------------------------------------------------------------

type arityKind int

const (
	arityBusiness arityKind = iota // 1025 result-error
	arityThrow                     // envelope -1 with the RPCHelpMan help text
)

// maxArity marks an unbounded upper param count (C++ ignores extras past the
// last read index for dxMakeOrder / dxMakePartialOrder).
const maxArity = -1

type aritySpec struct {
	min, max int
	kind     arityKind
	msg      string
}

var arity = map[string]aritySpec{
	// Business 1025 gates (exact C++ makeError arg).
	"dxGetNewTokenAddress": {1, 1, arityBusiness, "(ticker)"},
	"dxLoadXBridgeConf":    {0, 0, arityBusiness, "This function does not accept any parameter."},
	"dxGetLocalTokens":     {0, 0, arityBusiness, "This function does not accept any parameter."},
	"dxGetNetworkTokens":   {0, 0, arityBusiness, "This function does not accept any parameters."},
	"dxGetOrders":          {0, 0, arityBusiness, "This function does not accept any parameters."},
	"dxGetOrderFills":      {2, 3, arityBusiness, "(maker) (taker) (combined, default=true)[optional]"},
	// The help text for the (limit) default is byte-faithful to C++'s
	// IntervalLimit default (2147483647, rpcxbridge.cpp:603). Go's ACTUAL
	// absent-limit behavior hard-caps the bucket grid at
	// defaultOrderHistoryMaxBuckets (see dxGetOrderHistory); the help string
	// stays C++-identical.
	"dxGetOrderHistory":      {5, 8, arityBusiness, "(maker) (taker) (start time) (end time) (granularity) (order_ids, default=false)[optional] (with_inverse, default=false)[optional] (limit, default=2147483647)[optional]"},
	"dxGetOrder":             {1, 1, arityBusiness, "(id)"},
	"dxCancelOrder":          {1, 1, arityBusiness, "(id)"},
	"dxGetOrderBook":         {3, 4, arityBusiness, "(detail, 1-4) (maker) (taker) (max_orders, default=50)[optional]"},
	"dxGetTokenBalances":     {0, 0, arityBusiness, "This function does not accept any parameters."},
	"dxGetLockedUtxos":       {0, 1, arityBusiness, "Too many parameters."},
	"dxFlushCancelledOrders": {0, 1, arityBusiness, "ageMillis must be an integer >= 0"},
	"dxGetMyOrders":          {0, 0, arityBusiness, "This function does not accept any parameters."},

	// Throw methods (envelope -1 with the byte-for-byte RPCHelpMan help text).
	"dxMakeOrder":                {7, maxArity, arityThrow, helpDxMakeOrder},
	"dxMakePartialOrder":         {6, maxArity, arityThrow, helpDxMakePartialOrder},
	"dxTakeOrder":                {3, 5, arityThrow, helpDxTakeOrder},
	"dxGetMyPartialOrderChain":   {1, 1, arityThrow, helpDxGetMyPartialOrderChain},
	"dxPartialOrderChainDetails": {1, 1, arityThrow, helpDxPartialOrderChainDetails},
	"dxSplitAddress":             {3, 6, arityThrow, helpDxSplitAddress},
	"dxSplitInputs":              {3, 7, arityThrow, helpDxSplitInputs},
	"dxGetUtxos":                 {1, 2, arityThrow, helpDxGetUtxos},
	"dxGetTradingData":           {0, 2, arityThrow, helpDxGetTradingData},
	// getnetworkinfo is a Go shim with its own gate.
}

// checkArity returns the C++ arity violation for method with n params, or nil
// when the count is within bounds. Methods without a registry entry (and
// unbounded maxima) are never gated.
func checkArity(method string, n int) *rpcError {
	spec, ok := arity[method]
	if !ok {
		return nil
	}
	if n >= spec.min && (spec.max == maxArity || n <= spec.max) {
		return nil
	}
	if spec.kind == arityThrow {
		return makeEnvelopeError(-1, spec.msg)
	}
	return makeError(errInvalidParameters, method, spec.msg)
}

// ---------------------------------------------------------------------------
// Strict positional parameter parsing.
//
// C++ reads dx* params either through json_spirit (Array params; a wrong/null
// value type throws std::runtime_error) or directly through UniValue
// (request.params[i].get_*(); a different message). Both surface as a JSON-RPC
// envelope error code -1 via rpc/server.cpp:584-586. The helpers below
// reproduce those throws as envelope rpcErrors with the exact C++ message.
//
// Absent params (i >= len(params)) are NOT an error here: C++ guards every
// optional read by params.size() and the arity registry (checkArity) enforces
// required counts before the handler runs.
// ---------------------------------------------------------------------------

// jsonTypeOf classifies a raw JSON value using json_spirit's value names
// (json_spirit_value.h:586-604): Object, Array, string, boolean, integer,
// real, null.
func jsonTypeOf(raw json.RawMessage) string {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 {
		return "null"
	}
	switch s[0] {
	case '{':
		return "Object"
	case '[':
		return "Array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	// Number: integer unless it carries a fraction or exponent (int_type vs
	// real_type in json_spirit).
	for _, c := range s {
		switch c {
		case '.', 'e', 'E':
			return "real"
		}
	}
	return "integer"
}

func spErr(msg string) *rpcError { return makeEnvelopeError(-1, msg) }

// spStr is the json_spirit get_str() equivalent: a present non-string (incl.
// null) is a thrown envelope -1 "get_value< string > called on <T> Value".
func spStr(params []json.RawMessage, i int) (string, bool, *rpcError) {
	if i >= len(params) {
		return "", false, nil
	}
	if t := jsonTypeOf(params[i]); t != "string" {
		return "", true, spErr(fmt.Sprintf("get_value< string > called on %s Value", t))
	}
	var s string
	_ = json.Unmarshal(params[i], &s)
	return s, true, nil
}

// spBool is the json_spirit get_bool() equivalent.
func spBool(params []json.RawMessage, i int) (bool, bool, *rpcError) {
	if i >= len(params) {
		return false, false, nil
	}
	if t := jsonTypeOf(params[i]); t != "boolean" {
		return false, true, spErr(fmt.Sprintf("get_value< boolean > called on %s Value", t))
	}
	var b bool
	_ = json.Unmarshal(params[i], &b)
	return b, true, nil
}

// spInt is the json_spirit get_int() equivalent (message uses the int_type
// name, "integer").
func spInt(params []json.RawMessage, i int) (int, bool, *rpcError) {
	if i >= len(params) {
		return 0, false, nil
	}
	if t := jsonTypeOf(params[i]); t != "integer" {
		return 0, true, spErr(fmt.Sprintf("get_value< integer > called on %s Value", t))
	}
	var n int
	if json.Unmarshal(params[i], &n) != nil {
		return 0, true, spErr(fmt.Sprintf("get_value< integer > called on %s Value", jsonTypeOf(params[i])))
	}
	return n, true, nil
}

// spInt64 is the json_spirit get_int64() equivalent.
func spInt64(params []json.RawMessage, i int) (int64, bool, *rpcError) {
	if i >= len(params) {
		return 0, false, nil
	}
	if t := jsonTypeOf(params[i]); t != "integer" {
		return 0, true, spErr(fmt.Sprintf("get_value< integer > called on %s Value", t))
	}
	var n int64
	if json.Unmarshal(params[i], &n) != nil {
		return 0, true, spErr(fmt.Sprintf("get_value< integer > called on %s Value", jsonTypeOf(params[i])))
	}
	return n, true, nil
}

// uvStr is the UniValue get_str() equivalent ("JSON value is not a string as
// expected"). A missing index in C++ yields NullUniValue, so absent and null
// throw exactly like a wrong type.
func uvStr(params []json.RawMessage, i int) (string, *rpcError) {
	if i >= len(params) || jsonTypeOf(params[i]) != "string" {
		return "", spErr("JSON value is not a string as expected")
	}
	var s string
	_ = json.Unmarshal(params[i], &s)
	return s, nil
}

// uvBool is the UniValue get_bool() equivalent ("JSON value is not a boolean
// as expected"); used for unconditional reads (dxSplitInputs params[3..5]).
func uvBool(params []json.RawMessage, i int) (bool, *rpcError) {
	if i >= len(params) || jsonTypeOf(params[i]) != "boolean" {
		return false, spErr("JSON value is not a boolean as expected")
	}
	var b bool
	_ = json.Unmarshal(params[i], &b)
	return b, nil
}

// uvBoolOpt is an isNull()-guarded UniValue bool read (dxSplitAddress
// params[3..5], dxGetUtxos include_used): absent or null keeps the default; a
// present non-boolean throws.
func uvBoolOpt(params []json.RawMessage, i int, def bool) (bool, *rpcError) {
	if i >= len(params) || jsonTypeOf(params[i]) == "null" {
		return def, nil
	}
	if jsonTypeOf(params[i]) != "boolean" {
		return false, spErr("JSON value is not a boolean as expected")
	}
	var b bool
	_ = json.Unmarshal(params[i], &b)
	return b, nil
}

// uvArr is the UniValue get_array() equivalent (dxSplitInputs utxos).
func uvArr(params []json.RawMessage, i int) ([]json.RawMessage, *rpcError) {
	if i >= len(params) || jsonTypeOf(params[i]) != "Array" {
		return nil, spErr("JSON value is not an array as expected")
	}
	var arr []json.RawMessage
	if json.Unmarshal(params[i], &arr) != nil {
		return nil, spErr("JSON value is not an array as expected")
	}
	return arr, nil
}

// uvTypeNameOf classifies a raw JSON value using UniValue's type names
// (univalue.cpp:219-232): null, bool, object, array, string, number. Unlike
// json_spirit, UniValue treats integer and real alike ("number").
func uvTypeNameOf(raw json.RawMessage) string {
	t := jsonTypeOf(raw)
	switch t {
	case "integer", "real":
		return "number"
	case "boolean":
		return "bool"
	case "Object":
		return "object"
	case "Array":
		return "array"
	case "null":
		return "null"
	}
	return "string"
}

// rtcStr/rtcNum/rtcBool are the RPCTypeCheck equivalents (rpc/server.cpp:81-103,
// RPCTypeCheckArgument). Several dx* handlers call RPCTypeCheck before reading
// the value, which throws envelope code -3 (RPC_TYPE_ERROR) with
// "Expected type <t>, got <name>" — distinct from the get_*() throws (-1).
// Present null is a mismatch (fAllowNull defaults to false).
func rtcErr(want, got string) *rpcError {
	return makeEnvelopeError(-3, fmt.Sprintf("Expected type %s, got %s", want, got))
}

func rtcStr(params []json.RawMessage, i int) (string, *rpcError) {
	if i >= len(params) {
		return "", nil
	}
	if t := uvTypeNameOf(params[i]); t != "string" {
		return "", rtcErr("string", t)
	}
	var s string
	_ = json.Unmarshal(params[i], &s)
	return s, nil
}

func rtcNum(params []json.RawMessage, i int) (int, *rpcError) {
	if i >= len(params) {
		return 0, nil
	}
	if t := uvTypeNameOf(params[i]); t != "number" {
		return 0, rtcErr("number", t)
	}
	// C++ follows RPCTypeCheck with a json_spirit get_int() read
	// (rpcxbridge.cpp:2853), which throws -1 on a real value.
	if t := jsonTypeOf(params[i]); t == "real" {
		return 0, spErr("get_value< integer > called on real Value")
	}
	var n int
	if json.Unmarshal(params[i], &n) != nil {
		return 0, spErr(fmt.Sprintf("get_value< integer > called on %s Value", jsonTypeOf(params[i])))
	}
	return n, nil
}

func rtcBool(params []json.RawMessage, i int) (bool, *rpcError) {
	if i >= len(params) {
		return false, nil
	}
	if t := uvTypeNameOf(params[i]); t != "bool" {
		return false, rtcErr("bool", t)
	}
	var b bool
	_ = json.Unmarshal(params[i], &b)
	return b, nil
}
