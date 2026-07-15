package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// rpcRequest is the bitcoind-style JSON-RPC 1.0 request envelope used by
// Blocknet. Params are positional (a JSON array), not named.
type rpcRequest struct {
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
	ID     json.RawMessage   `json:"id"`
}

// rpcResponse is the JSON-RPC envelope. The `error` field stays null even for
// business errors — those are returned as the `result` object (see rpcError).
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  interface{}     `json:"result"`
	Error   interface{}     `json:"error"`
	ID      json.RawMessage `json:"id"`
}

// envelopeError is used only for transport-level failures (parse error, unknown
// method) — distinct from business errors, which live in the result object.
type envelopeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server is the JSON-RPC 1.0 HTTP server exposing the dx* surface.
type Server struct {
	ctx    *HandlerCtx
	verify bool // when true, signatures are verified before storing orders
}

// NewServer builds a Server from a handler context.
func NewServer(ctx *HandlerCtx) *Server {
	return &Server{ctx: ctx}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body := rpcRequest{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, rpcResponse{
			Result: nil,
			Error:  &envelopeError{Code: -32700, Message: "Parse error"},
			ID:     body.ID,
		})
		return
	}

	handler := Lookup(body.Method)
	if handler == nil {
		writeJSON(w, rpcResponse{
			Result: nil,
			Error:  &envelopeError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", body.Method)},
			ID:     body.ID,
		})
		return
	}

	result, rpcErr := handler(s.ctx, body.Params)
	if rpcErr != nil {
		// Business error: Blocknet returns it as the *result* object.
		writeJSON(w, rpcResponse{Result: rpcErr, Error: nil, ID: body.ID})
		return
	}
	writeJSON(w, rpcResponse{Result: result, Error: nil, ID: body.ID})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	// Stamp the JSON-RPC 2.0 version on every response. BLOCK-DX expects the
	// field on the getnetworkinfo handshake; it is harmless for the dx* calls.
	if r, ok := v.(rpcResponse); ok {
		r.JSONRPC = "2.0"
		v = r
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
