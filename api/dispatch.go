package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// HandlerCtx carries the dependencies each dx* handler needs.
type HandlerCtx struct {
	Store  *Store
	Node   *Node
	Config *Config
}

// Handler implements one dx* method. It returns the JSON result (any
// marshalable value) and, on a business error, an *rpcError — which the server
// serializes as the JSON-RPC *result* object (Blocknet returns errors as the
// result, leaving the envelope error null).
type Handler func(h *HandlerCtx, params []json.RawMessage) (interface{}, *rpcError)

// dispatch maps every dx* method name (and the lowercase gettradingdata alias)
// to its handler. The set and names match src/xbridge/rpcxbridge.cpp commands[].
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
	"gettradingdata":             (*HandlerCtx).dxGetTradingData,
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
// Positional parameter parsing. Blocknet RPC uses positional params (a JSON
// array), so handlers index params[0], params[1], ... directly.
// ---------------------------------------------------------------------------

func strParam(params []json.RawMessage, i int) (string, bool) {
	if i >= len(params) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(params[i], &s); err != nil {
		return "", false
	}
	return s, true
}

func optStrParam(params []json.RawMessage, i int, def string) string {
	if i >= len(params) {
		return def
	}
	var s string
	if err := json.Unmarshal(params[i], &s); err != nil {
		return def
	}
	return s
}

func boolParam(params []json.RawMessage, i int, def bool) (bool, bool) {
	if i >= len(params) {
		return def, true
	}
	var b bool
	if err := json.Unmarshal(params[i], &b); err != nil {
		// tolerate "true"/"false" string forms
		var s string
		if json.Unmarshal(params[i], &s) == nil {
			s = strings.ToLower(strings.TrimSpace(s))
			return s == "true", true
		}
		return def, false
	}
	return b, true
}

func intParam(params []json.RawMessage, i int, def int) (int, bool) {
	if i >= len(params) {
		return def, true
	}
	var n int
	if err := json.Unmarshal(params[i], &n); err != nil {
		// tolerate numeric strings
		var s string
		if json.Unmarshal(params[i], &s) == nil {
			if v, e := strconv.Atoi(strings.TrimSpace(s)); e == nil {
				return v, true
			}
		}
		return def, false
	}
	return n, true
}

// mustBool is boolParam with error propagation: a present-but-unparseable
// param is a caller error (errInvalidParameters), not a silent default. A
// missing param still yields def (ok=true).
func mustBool(params []json.RawMessage, i int, def bool, method string) (bool, *rpcError) {
	v, ok := boolParam(params, i, def)
	if !ok {
		return def, makeError(errInvalidParameters, method, fmt.Sprintf("param %d is not a boolean", i))
	}
	return v, nil
}

// mustInt is intParam with error propagation: a present-but-unparseable param
// is a caller error, not a silent default. A missing param still yields def.
func mustInt(params []json.RawMessage, i, def int, method string) (int, *rpcError) {
	v, ok := intParam(params, i, def)
	if !ok {
		return def, makeError(errInvalidParameters, method, fmt.Sprintf("param %d is not an integer", i))
	}
	return v, nil
}

func strArrayParam(params []json.RawMessage, i int) ([]string, bool) {
	if i >= len(params) {
		return nil, true
	}
	var arr []string
	if err := json.Unmarshal(params[i], &arr); err != nil {
		return nil, false
	}
	return arr, true
}
