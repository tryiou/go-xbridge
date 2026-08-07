package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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

	// user/pass are the HTTP Basic credentials RPC callers must present. When
	// both are empty (the daemon's default) no authentication is enforced —
	// F1 (RPC auth) binds to loopback by default and defers to the operator to
	// enable auth explicitly via -rpcuser/-rpcpassword.
	user, pass string
}

// NewServer builds a Server from a handler context.
func NewServer(ctx *HandlerCtx) *Server {
	return &Server{ctx: ctx}
}

// SetAuth enables HTTP Basic authentication on every RPC request. It is only
// meaningful when BOTH a user and password are supplied; either alone is
// ignored (matching the daemon's "no auth unless -rpcuser and -rpcpassword are
// both configured" contract).
func (s *Server) SetAuth(user, pass string) {
	if user == "" || pass == "" {
		s.user, s.pass = "", ""
		return
	}
	s.user, s.pass = user, pass
}

// basicRealm is the "realm" served in the WWW-Authenticate challenge.
const basicRealm = "jsonrpc"

// authorized reports whether the request carries valid HTTP Basic credentials
// matching the configured rpcuser/rpcpassword. The comparison is constant-time
// (crypto/subtle), mirroring C++ RPCAuthorized's TimingResistantEqual
// (httprpc.cpp:128-146). With no auth configured it accepts everything (the
// daemon's default: localhost-only bind).
func (s *Server) authorized(r *http.Request) bool {
	if s.user == "" {
		return true
	}
	u, p, ok := parseBasicAuth(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	ou := subtle.ConstantTimeCompare([]byte(u), []byte(s.user))
	op := subtle.ConstantTimeCompare([]byte(p), []byte(s.pass))
	return ou&op == 1
}

// parseBasicAuth decodes the "Basic <base64 user:pass>" Authorization header.
func parseBasicAuth(h string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	i := bytes.IndexByte(b, ':')
	if i < 0 {
		return "", "", false
	}
	return string(b[:i]), string(b[i+1:]), true
}

// rpcMaxBodyBytes caps the JSON-RPC request body (F10/S3): a body larger than
// this is rejected instead of being buffered in whole, closing the
// unbounded-decode surface in ServeHTTP.
const rpcMaxBodyBytes = 4 << 20

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// RPC auth gate (F1): reject without a 401 + challenge when credentials
	// are configured, before any handler runs.
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+basicRealm+`"`)
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, rpcResponse{
			Result: nil,
			Error:  &envelopeError{Code: -401, Message: "Authentication failed"},
			ID:     nil,
		})
		return
	}
	// Bound the request body (F10/S3): read it fully through MaxBytesReader so
	// an oversized body is rejected instead of buffered unbounded.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rpcMaxBodyBytes))
	if err != nil {
		xlog.Warn("rpc transport error", "code", -32700, "msg", "Request body too large")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		writeJSON(w, rpcResponse{
			Result: nil,
			Error:  &envelopeError{Code: -32700, Message: "Parse error: request body too large"},
			ID:     nil,
		})
		return
	}
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

	req := rpcRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		xlog.Warn("rpc transport error", "code", -32700, "msg", "Parse error")
		writeJSON(w, rpcResponse{
			Result: nil,
			Error:  &envelopeError{Code: -32700, Message: "Parse error"},
			ID:     req.ID,
		})
		return
	}
	reqID = req.ID

	handler := Lookup(req.Method)
	if handler == nil {
		xlog.Warn("rpc transport error", "code", -32601, "method", req.Method, "msg", "Method not found")
		writeJSON(w, rpcResponse{
			Result: nil,
			Error:  &envelopeError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)},
			ID:     req.ID,
		})
		return
	}

	xlog.Debug("rpc request", "method", req.Method)
	result, rpcErr := handler(s.ctx, req.Params)
	if rpcErr != nil {
		// Business error: Blocknet returns it as the *result* object.
		xlog.Warn("rpc error", "method", req.Method, "code", rpcErr.Code, "msg", rpcErr.Error)
		writeJSON(w, rpcResponse{Result: rpcErr, Error: nil, ID: req.ID})
		return
	}
	writeJSON(w, rpcResponse{Result: result, Error: nil, ID: req.ID})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	// blocknetd emits JSON-RPC 1.0: compact JSON, no "jsonrpc" field.
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
