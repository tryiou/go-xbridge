package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	xlog "go-xbridge/log"
)

// rawRequest preserves the id across field-by-field validation. The whole
// envelope is decoded into raw pieces first (mirroring C++ JSONRPCRequest::parse,
// which parses id before anything else, so pre-dispatch errors echo it).
type rawRequest struct {
	Method json.RawMessage
	Params json.RawMessage
	ID     json.RawMessage
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
// method, invalid request) — distinct from business errors, which live in the
// result object. The shape mirrors C++ (code, message) only.
type envelopeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// httpStatusForCode maps an envelope error code to the HTTP status the daemon
// replies with, mirroring JSONErrorReply (httprpc.cpp:70-85): RPC_INVALID_REQUEST
// (-32600) -> 400, RPC_METHOD_NOT_FOUND (-32601) -> 404, everything else
// (parse, misc -1, type -8, internal -32603, ...) -> 500.
func httpStatusForCode(code int) int {
	switch code {
	case -32600:
		return http.StatusBadRequest
	case -32601:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// Server is the JSON-RPC 1.0 HTTP server exposing the dx* surface.
type Server struct {
	ctx    *HandlerCtx
	verify bool // when true, signatures are verified before storing orders

	// user/pass are the HTTP Basic credentials RPC callers must present
	// (-rpcuser/-rpcpassword).
	user, pass string
	// authUsers holds -rpcauth multi-user entries (user -> salt/hash).
	authUsers map[string]rpcauthEntry
	// failDelay mirrors C++ MilliSleep(250) on failed auth (httprpc.cpp:171).
	failDelay time.Duration
}

// rpcauthEntry is one -rpcauth "user:salt$hash" entry; the password check is
// HMAC-SHA256(salt, password) hex-compared to hash (httprpc.cpp:89-126).
type rpcauthEntry struct {
	user string
	salt string
	hash string
}

// NewServer builds a Server from a handler context.
func NewServer(ctx *HandlerCtx) *Server {
	return &Server{ctx: ctx, failDelay: 250 * time.Millisecond}
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

// SetRpcAuth configures -rpcauth multi-user entries ("user:salt$hash").
// Malformed entries are skipped (httprpc.cpp:97-104).
func (s *Server) SetRpcAuth(entries []string) {
	s.authUsers = make(map[string]rpcauthEntry, len(entries))
	for _, e := range entries {
		u, salt, hash, ok := parseRpcAuthEntry(e)
		if !ok {
			continue
		}
		s.authUsers[u] = rpcauthEntry{user: u, salt: salt, hash: hash}
	}
}

// parseRpcAuthEntry splits a "user:salt$hash" entry into its three fields.
func parseRpcAuthEntry(entry string) (user, salt, hash string, ok bool) {
	v := strings.FieldsFunc(entry, func(r rune) bool { return r == ':' || r == '$' })
	if len(v) != 3 {
		return "", "", "", false
	}
	return v[0], v[1], v[2], true
}

// basicRealm is the "realm" served in the WWW-Authenticate challenge.
const basicRealm = "jsonrpc"

// authConfigured reports whether any credentials are configured. When none are,
// the daemon is open (loopback-default bind; documented divergence — see
// docs/api.md §Calling convention).
func (s *Server) authConfigured() bool {
	return s.user != "" || len(s.authUsers) > 0
}

// authorized reports whether the request carries valid credentials: the single
// -rpcuser/-rpcpassword pair or any -rpcauth entry. Comparisons are
// constant-time (crypto/subtle), mirroring C++ TimingResistantEqual
// (httprpc.cpp:128-146).
func (s *Server) authorized(r *http.Request) bool {
	if !s.authConfigured() {
		return true
	}
	u, p, ok := parseBasicAuth(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	if s.user != "" {
		ou := subtle.ConstantTimeCompare([]byte(u), []byte(s.user))
		op := subtle.ConstantTimeCompare([]byte(p), []byte(s.pass))
		if ou&op == 1 {
			return true
		}
	}
	for _, e := range s.authUsers {
		if subtle.ConstantTimeCompare([]byte(u), []byte(e.user)) != 1 {
			continue
		}
		mac := hmac.New(sha256.New, []byte(e.salt))
		_, _ = mac.Write([]byte(p))
		want := hex.EncodeToString(mac.Sum(nil))
		if subtle.ConstantTimeCompare([]byte(want), []byte(e.hash)) == 1 {
			return true
		}
	}
	return false
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

// rpcMaxBodyBytes caps the JSON-RPC request body: C++ sets
// evhttp_set_max_body_size to MAX_SIZE = 0x02000000 (32 MiB) — serialize.h:27,
// httpserver.cpp:395. A body larger than this is rejected with a non-envelope
// HTTP 413 (libevent behavior) instead of being buffered in whole.
const rpcMaxBodyBytes = 32 << 20

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// JSONRPC handles only POST (httprpc.cpp:151-154).
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("JSONRPC server handles only POST requests"))
		return
	}
	// RPC auth gate: authentication is always enforced when
	// credentials are configured (the daemon is never open-by-default in that
	// case). A failed attempt is met with an empty-body 401 + challenge; bad
	// (present) credentials additionally trigger the C++ 250 ms sleep that
	// deters brute-forcing (httprpc.cpp:166-174).
	if !s.authorized(r) {
		if r.Header.Get("Authorization") != "" {
			xlog.Warn("rpc incorrect password attempt", "remote", r.RemoteAddr)
			if s.failDelay > 0 {
				time.Sleep(s.failDelay)
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="`+basicRealm+`"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Bound the request body: read it fully through MaxBytesReader so
	// an oversized body is rejected (non-envelope 413, matching libevent)
	// instead of buffered unbounded.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rpcMaxBodyBytes))
	if err != nil {
		xlog.Warn("rpc transport error", "code", -32700, "msg", "Request body too large")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	// Never return an empty body: a panic in a handler must still produce a
	// valid JSON-RPC envelope (mirrors blocknetd, which never emits an empty
	// HTTP response).
	var reqID json.RawMessage
	defer func() {
		if rec := recover(); rec != nil {
			xlog.Error("server: recovered panic in handler", "panic", rec)
			s.writeResponse(w, envelopeResponse(makeEnvelopeError(-32603, fmt.Sprintf("Internal error: %v", rec)), reqID))
		}
	}()

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		s.writeResponse(w, envelopeResponse(makeEnvelopeError(-32700, "Top-level object parse error"), nil))
		return
	}
	switch trimmed[0] {
	case '{':
		raw := rawRequest{}
		if err := json.Unmarshal(body, &raw); err != nil {
			s.writeResponse(w, envelopeResponse(makeEnvelopeError(-32700, "Parse error"), nil))
			return
		}
		reqID = raw.ID
		s.writeResponse(w, s.dispatchOne(raw))
	case '[':
		// Batch request (JSONRPCExecBatch, server.cpp:497-504): process each
		// element independently; the batch itself always replies HTTP 200.
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err != nil {
			s.writeResponse(w, envelopeResponse(makeEnvelopeError(-32700, "Parse error"), nil))
			return
		}
		results := make([]rpcResponse, 0, len(arr))
		for _, el := range arr {
			results = append(results, s.execOne(el))
		}
		writeJSON(w, results)
	default:
		// Not an object or array (server.cpp:201).
		s.writeResponse(w, envelopeResponse(makeEnvelopeError(-32700, "Top-level object parse error"), nil))
	}
}

// execOne processes a single element of a batch. Non-object elements are an
// invalid request ("Invalid Request object", server.cpp:437-438).
func (s *Server) execOne(el json.RawMessage) rpcResponse {
	trimmed := bytes.TrimSpace(el)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return envelopeResponse(makeEnvelopeError(-32600, "Invalid Request object"), nil)
	}
	raw := rawRequest{}
	if err := json.Unmarshal(el, &raw); err != nil {
		return envelopeResponse(makeEnvelopeError(-32700, "Parse error"), nil)
	}
	return s.dispatchOne(raw)
}

// dispatchOne validates the request envelope, finds the handler, and runs it.
func (s *Server) dispatchOne(raw rawRequest) rpcResponse {
	method, params, id, perr := parseRequest(raw)
	if perr != nil {
		return envelopeResponse(perr, id)
	}
	handler := Lookup(method)
	if handler == nil {
		xlog.Warn("rpc transport error", "code", -32601, "method", method)
		return envelopeResponse(makeEnvelopeError(-32601, "Method not found"), id)
	}
	// Arity gate: business 1025 for the old-style methods, envelope
	// -1 with the C++ help text for the throw methods.
	if aerr := checkArity(method, len(params)); aerr != nil {
		if aerr.envelope {
			return envelopeResponse(aerr, id)
		}
		return businessResponse(aerr, id)
	}
	xlog.Debug("rpc request", "method", method)
	result, rpcErr := s.call(handler, params, id)
	if rpcErr != nil {
		if rpcErr.envelope {
			return envelopeResponse(rpcErr, id)
		}
		// Business error: Blocknet returns it as the *result* object.
		xlog.Warn("rpc error", "method", method, "code", rpcErr.Code, "msg", rpcErr.Error)
		return businessResponse(rpcErr, id)
	}
	return rpcResponse{Result: result, Error: nil, ID: id}
}

// call runs a handler with panic containment so a handler panic surfaces as an
// envelope -32603 in the current request (or batch element).
func (s *Server) call(handler Handler, params []json.RawMessage, id json.RawMessage) (result interface{}, rpcErr *rpcError) {
	defer func() {
		if rec := recover(); rec != nil {
			xlog.Error("server: recovered panic in handler", "panic", rec)
			rpcErr = makeEnvelopeError(-32603, fmt.Sprintf("Internal error: %v", rec))
		}
	}()
	return handler(s.ctx, params)
}

// parseRequest validates one request object per JSONRPCRequest::parse
// (server.cpp:434-465) and returns positional params. Named-parameter objects
// are rejected: every dx* command registers no arg names (rpcxbridge.cpp:
// 3498-3526), so transformNamedArguments throws on the first key
// (server.cpp:549-551, 578-582).
func parseRequest(raw rawRequest) (method string, params []json.RawMessage, id json.RawMessage, err *rpcError) {
	id = raw.ID

	// Method.
	if len(raw.Method) == 0 || bytes.Equal(bytes.TrimSpace(raw.Method), []byte("null")) {
		return "", nil, id, makeEnvelopeError(-32600, "Missing method")
	}
	var m string
	if json.Unmarshal(raw.Method, &m) != nil {
		return "", nil, id, makeEnvelopeError(-32600, "Method must be a string")
	}

	// Params: absent/null -> empty array; array -> positional; object -> named.
	if len(raw.Params) == 0 || bytes.Equal(bytes.TrimSpace(raw.Params), []byte("null")) {
		return m, nil, id, nil
	}
	trimmed := bytes.TrimSpace(raw.Params)
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return "", nil, id, makeEnvelopeError(-32600, "Params must be an array or object")
		}
		return m, arr, id, nil
	}
	if trimmed[0] == '{' {
		if key := firstObjectKey(trimmed); key != "" {
			return "", nil, id, makeEnvelopeError(-8, "Unknown named parameter "+key)
		}
		// Empty object {}: transformNamedArguments leaves params empty.
		return m, nil, id, nil
	}
	return "", nil, id, makeEnvelopeError(-32600, "Params must be an array or object")
}

// firstObjectKey returns the first key of a JSON object (in document order).
func firstObjectKey(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		return ""
	}
	t, err = dec.Token()
	if err != nil {
		return ""
	}
	s, _ := t.(string)
	return s
}

func (s *Server) writeResponse(w http.ResponseWriter, resp rpcResponse) {
	if env, ok := resp.Error.(*envelopeError); ok {
		w.WriteHeader(httpStatusForCode(env.Code))
	} else {
		w.WriteHeader(http.StatusOK)
	}
	writeJSON(w, resp)
}

func envelopeResponse(err *rpcError, id json.RawMessage) rpcResponse {
	return rpcResponse{
		Result: nil,
		Error:  &envelopeError{Code: err.Code, Message: err.Error},
		ID:     id,
	}
}

func businessResponse(err *rpcError, id json.RawMessage) rpcResponse {
	return rpcResponse{Result: err, Error: nil, ID: id}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	// blocknetd emits JSON-RPC 1.0: compact JSON, no "jsonrpc" field.
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
