package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	xlog "go-xbridge/log"
)

// rpcRequest is the bitcoind-style JSON-RPC 1.0 request envelope used by
// Blocknet. Params are positional (a JSON array), not named.
type rpcRequest struct {
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
	ID     json.RawMessage   `json:"id"`
}

// rpcResponse is the JSON-RPC 1.0 envelope, matching blocknetd's dx* surface
// (C++ is JSON-RPC 1.0: only result/error/id, no "jsonrpc" version field, and
// compact — not pretty-printed). Business errors live in the `result` object
// (see rpcError); the `error` field stays null for them.
type rpcResponse struct {
	Result interface{}     `json:"result"`
	Error  interface{}     `json:"error"`
	ID     json.RawMessage `json:"id"`
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
	// Never return an empty body: a panic in a handler must still produce a
	// valid JSON-RPC envelope (mirrors blocknetd, which never emits an empty
	// HTTP response). Without this, a transient handler panic would surface as
	// an empty body to the caller.
	var reqID json.RawMessage
	defer func() {
		if rec := recover(); rec != nil {
			xlog.Error("server: recovered panic in handler", "panic", rec)
			writeJSON(w, rpcResponse{
				Result: nil,
				Error:  &envelopeError{Code: -32603, Message: fmt.Sprintf("Internal error: %v", rec)},
				ID:     reqID,
			})
		}
	}()

	body := rpcRequest{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		xlog.Warn("rpc transport error", "code", -32700, "msg", "Parse error")
		writeJSON(w, rpcResponse{
			Result: nil,
			Error:  &envelopeError{Code: -32700, Message: "Parse error"},
			ID:     body.ID,
		})
		return
	}
	reqID = body.ID

	handler := Lookup(body.Method)
	if handler == nil {
		xlog.Warn("rpc transport error", "code", -32601, "method", body.Method, "msg", "Method not found")
		writeJSON(w, rpcResponse{
			Result: nil,
			Error:  &envelopeError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", body.Method)},
			ID:     body.ID,
		})
		return
	}

	xlog.Debug("rpc request", "method", body.Method)
	result, rpcErr := handler(s.ctx, body.Params)
	if rpcErr != nil {
		// Business error: Blocknet returns it as the *result* object.
		xlog.Warn("rpc error", "method", body.Method, "code", rpcErr.Code, "msg", rpcErr.Error)
		writeJSON(w, rpcResponse{Result: rpcErr, Error: nil, ID: body.ID})
		return
	}
	writeJSON(w, rpcResponse{Result: result, Error: nil, ID: body.ID})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	// blocknetd emits JSON-RPC 1.0: compact JSON, no "jsonrpc" field.
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
