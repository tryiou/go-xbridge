package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-xbridge/proto"
)

func newTestCtx() *HandlerCtx {
	store := NewStore()
	return &HandlerCtx{Store: store, Node: &Node{store: store}}
}

func TestDxGetOrderRoundTrip(t *testing.T) {
	ctx := newWalletTestCtx()
	// Inject a known order directly into the store (BTC/BTC so both wallet
	// sessions resolve, matching C++'s NO_SESSION gate on both currencies).
	body := &proto.OrderBody{
		ID:             [32]byte{0x99},
		FromCurrency:   "BTC",
		FromAmount:     1500000,
		ToCurrency:     "BTC",
		ToAmount:       150000,
		Created:        uint64(1516040130000000), // microsec
		PartialAllowed: false,
		MinFromAmount:  1500000,
	}
	o := normalizeFromOrderBody(body, "pubkeyhex")
	ctx.Store.Add(o)
	idHex := dispID(o.ID)

	res, err := ctx.dxGetOrder([]json.RawMessage{json.RawMessage(`"` + idHex + `"`)})
	if err != nil {
		t.Fatalf("dxGetOrder error: %+v", err)
	}
	obj, ok := res.(orderListResult)
	if !ok {
		t.Fatalf("dxGetOrder result type = %T", res)
	}
	if obj.ID != idHex || obj.Maker != "BTC" || obj.Taker != "BTC" {
		t.Errorf("dxGetOrder fields wrong: %+v", obj)
	}
	if obj.MakerSize != "1.500000" {
		t.Errorf("dxGetOrder maker_size = %q, want 1.500000", obj.MakerSize)
	}
	if obj.CreatedAt != "2018-01-15T18:15:30.000Z" {
		t.Errorf("dxGetOrder created_at = %q", obj.CreatedAt)
	}
	if obj.Status != "open" {
		t.Errorf("dxGetOrder status = %q", obj.Status)
	}
}

// TestDxGetOrderCaseInsensitiveID locks in C++'s uint256S id normalization:
// an upper-case id must resolve the lower-cased store key.
func TestDxGetOrderCaseInsensitiveID(t *testing.T) {
	ctx := newWalletTestCtx()
	body := &proto.OrderBody{
		ID:           [32]byte{0xab},
		FromCurrency: "BTC", FromAmount: 1000000,
		ToCurrency: "BTC", ToAmount: 100000,
		Created: 1, PartialAllowed: false,
	}
	o := normalizeFromOrderBody(body, "pubkeyhex")
	ctx.Store.Add(o)
	lower := dispID(o.ID) // display order, all lowercase
	upper := strings.ToUpper(lower)
	res, err := ctx.dxGetOrder([]json.RawMessage{json.RawMessage(`"` + upper + `"`)})
	if err != nil {
		t.Fatalf("dxGetOrder(uppercase id) error: %+v", err)
	}
	obj := res.(orderListResult)
	if obj.ID != lower {
		t.Errorf("dxGetOrder id = %q, want %q (normalized lowercase)", obj.ID, lower)
	}
}

func TestDxGetOrderNotFound(t *testing.T) {
	ctx := newTestCtx()
	res, rpcErr := ctx.dxGetOrder([]json.RawMessage{json.RawMessage(`"deadbeef"`)})
	if rpcErr == nil {
		t.Fatalf("expected rpcError, got result %v", res)
	}
	if rpcErr.Code != errTxNotFound {
		t.Errorf("expected errTxNotFound, got %d", rpcErr.Code)
	}
}

func TestServerEnvelopeAndError(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)

	// Unknown method -> envelope error, result null, HTTP 404 (RPC-F47/F48).
	rec := callRPC(t, srv, `{"method":"dxNope","params":[],"id":1}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var env rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error == nil || env.Result != nil {
		t.Errorf("unknown method should set envelope error and null result: %+v", env)
	}
	if ee, ok := env.Error.(map[string]interface{}); !ok {
		t.Errorf("envelope error should be an object, got %T", env.Error)
	} else if msg, _ := ee["message"].(string); msg != "Method not found" {
		t.Errorf("method-not-found message = %q, want \"Method not found\"", msg)
	}

	// Business error (e.g. dxGetOrder with missing id) -> result IS the error object.
	rec = callRPC(t, srv, `{"method":"dxGetOrder","params":[],"id":2}`)
	var env2 rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &env2)
	if env2.Error != nil {
		t.Errorf("business error must leave envelope error null: %+v", env2.Error)
	}
	re, ok := env2.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("business error must be in result as an object, got %T", env2.Result)
	}
	if code, _ := re["code"].(float64); int(code) != errInvalidParameters {
		t.Errorf("expected INVALID_PARAMETERS, got %v", re["code"])
	}
	if name, _ := re["name"].(string); name != "dxGetOrder" {
		t.Errorf("expected name=dxGetOrder, got %v", re["name"])
	}
}

func callRPC(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestServerRPCAuth verifies RPC-F50: with -rpcuser/-rpcpassword configured, the
// JSON-RPC server requires valid HTTP Basic credentials — an empty-body 401 +
// WWW-Authenticate challenge otherwise; without credentials configured, requests
// pass through unchanged (the loopback-default contract; no cookie).
func TestServerRPCAuth(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	srv.SetAuth("alice", "s3cret")

	// Missing Authorization header -> 401 + WWW-Authenticate challenge, EMPTY body.
	rec := callRPC(t, srv, `{"method":"dxGetOrder","params":[],"id":1}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing creds status = %d, want 401", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("missing WWW-Authenticate challenge, got %q", rec.Header().Get("WWW-Authenticate"))
	}
	if rec.Body.Len() != 0 {
		t.Errorf("unauthorized reply must have an empty body (C++ sends none), got %q", rec.Body.String())
	}

	// Wrong password -> 401 (no body).
	srv.failDelay = 0
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"method":"dxGetOrder","params":[],"id":2}`))
	req.SetBasicAuth("alice", "wrong")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", rec.Code)
	}

	// Correct credentials -> request proceeds (dxGetOrder returns a business
	// error for missing id, not a 401).
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"method":"dxGetOrder","params":[],"id":3}`))
	req.SetBasicAuth("alice", "s3cret")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct creds status = %d, want 200", rec.Code)
	}
	var env rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != nil || env.Result == nil {
		t.Errorf("authenticated request should reach the handler: %+v", env)
	}

	// SetAuth with only one of user/pass disables that credential pair (open).
	srv.SetAuth("alice", "")
	rec = callRPC(t, srv, `{"method":"dxGetOrder","params":[],"id":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial creds should leave auth open, status = %d", rec.Code)
	}
}

// TestServerRpcAuthDelay verifies the C++ 250 ms brute-force deterrence sleep
// (httprpc.cpp:171): it applies to bad (present) credentials but not to a
// missing Authorization header.
func TestServerRpcAuthDelay(t *testing.T) {
	if NewServer(newTestCtx()).failDelay != 250*time.Millisecond {
		t.Errorf("default failDelay != 250ms (C++ MilliSleep(250))")
	}
	ctx := newTestCtx()
	srv := NewServer(ctx)
	srv.SetAuth("alice", "s3cret")
	srv.failDelay = 30 * time.Millisecond

	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"id":1}`))
	req.SetBasicAuth("alice", "wrong")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if elapsed := time.Since(start); elapsed < srv.failDelay {
		t.Errorf("bad-cred request returned after %v, want >= %v (sleep)", elapsed, srv.failDelay)
	}

	start = time.Now()
	rec = callRPC(t, srv, `{"id":2}`) // missing Authorization header
	if elapsed := time.Since(start); elapsed >= srv.failDelay {
		t.Errorf("missing-header request slept %v, want immediate 401", elapsed)
	}
}

func TestServerRpcAuthMultiUser(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)

	mkEntry := func(user, salt, pass string) string {
		mac := hmac.New(sha256.New, []byte(salt))
		_, _ = mac.Write([]byte(pass))
		return user + ":" + salt + "$" + hex.EncodeToString(mac.Sum(nil))
	}
	srv.SetRpcAuth([]string{
		mkEntry("bob", "abc123", "hunter2"),
		mkEntry("carol", "feedface", "s3cret"),
		"malformed-entry", // skipped
	})

	// Valid rpcauth user -> request proceeds.
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"method":"dxGetOrder","params":[],"id":1}`))
	req.SetBasicAuth("bob", "hunter2")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bob status = %d, want 200", rec.Code)
	}

	// Second user works too.
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"method":"dxGetOrder","params":[],"id":2}`))
	req.SetBasicAuth("carol", "s3cret")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("carol status = %d, want 200", rec.Code)
	}

	// Wrong password for an rpcauth user -> 401.
	srv.failDelay = 0
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"id":3}`))
	req.SetBasicAuth("bob", "wrong")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bob wrong-password status = %d, want 401", rec.Code)
	}

	// Unknown user -> 401.
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"id":4}`))
	req.SetBasicAuth("mallory", "anything")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user status = %d, want 401", rec.Code)
	}
}

func TestServerNoCredsOpen(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx) // no credentials
	rec := callRPC(t, srv, `{"method":"dxGetOrder","params":[],"id":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-creds status = %d, want 200 (loopback-default open)", rec.Code)
	}
}

// TestServerMaxBodyBytes verifies RPC-F49: an oversized JSON-RPC body is rejected
// by the http.MaxBytesReader gate (32 MiB cap, C++ MAX_SIZE) rather than
// buffered/decoded unbounded, and the reply is a non-envelope 413 (libevent).
func TestServerMaxBodyBytes(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	big := strings.Repeat(" ", rpcMaxBodyBytes+1) // > 32 MiB
	rec := callRPC(t, srv, `{"method":"dxGetOrder","params":[],"id":1}`+"\n"+big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413 (MaxBytesReader)", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("oversized body reply must be empty (non-envelope), got %q", rec.Body.String())
	}
}

func TestServerPostOnly405(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(m, "/", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", m, rec.Code)
		}
		if rec.Body.String() != "JSONRPC server handles only POST requests" {
			t.Errorf("%s body = %q", m, rec.Body.String())
		}
	}
}

func TestServerParseErrorStatus500(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	// Malformed JSON body -> -32700, HTTP 500 (RPC-F47).
	rec := callRPC(t, srv, `{"method":`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("parse-error status = %d, want 500", rec.Code)
	}
	var env rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	ee := env.Error.(map[string]interface{})
	if code, _ := ee["code"].(float64); int(code) != -32700 {
		t.Errorf("parse-error code = %v, want -32700", ee["code"])
	}
	// Non-object top level -> -32700 "Top-level object parse error".
	rec = callRPC(t, srv, `"scalar"`)
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	ee = env.Error.(map[string]interface{})
	if msg, _ := ee["message"].(string); msg != "Top-level object parse error" {
		t.Errorf("scalar top-level message = %q", msg)
	}
}

func TestServerInvalidRequest(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	cases := []struct {
		name     string
		body     string
		wantCode float64
		wantMsg  string
		wantID   interface{}
		want404  bool
	}{
		{name: "missing method", body: `{"params":[],"id":7}`, wantCode: -32600, wantMsg: "Missing method", wantID: float64(7)},
		{name: "null method", body: `{"method":null,"params":[],"id":8}`, wantCode: -32600, wantMsg: "Missing method", wantID: float64(8)},
		{name: "non-string method", body: `{"method":123,"params":[],"id":9}`, wantCode: -32600, wantMsg: "Method must be a string", wantID: float64(9)},
		{name: "params wrong shape", body: `{"method":"dxGetOrder","params":"str","id":10}`, wantCode: -32600, wantMsg: "Params must be an array or object", wantID: float64(10)},
		{name: "method-not-found", body: `{"method":"dxNope","params":[],"id":11}`, wantCode: -32601, wantMsg: "Method not found", wantID: float64(11)},
	}
	for _, c := range cases {
		rec := callRPC(t, srv, c.body)
		wantStatus := http.StatusBadRequest
		if c.wantCode == -32601 {
			wantStatus = http.StatusNotFound
		}
		if rec.Code != wantStatus {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, wantStatus)
		}
		var env rpcResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Result != nil {
			t.Errorf("%s: result = %v, want null", c.name, env.Result)
		}
		ee, ok := env.Error.(map[string]interface{})
		if !ok {
			t.Fatalf("%s: error = %T, want object", c.name, env.Error)
		}
		if code, _ := ee["code"].(float64); code != c.wantCode {
			t.Errorf("%s: code = %v, want %v", c.name, ee["code"], c.wantCode)
		}
		if msg, _ := ee["message"].(string); msg != c.wantMsg {
			t.Errorf("%s: message = %q, want %q", c.name, msg, c.wantMsg)
		}
		if c.wantID != nil {
			// id must be echoed on pre-dispatch errors (server.cpp:442).
			var id interface{}
			_ = json.Unmarshal(env.ID, &id)
			if id != c.wantID {
				t.Errorf("%s: id = %v, want %v", c.name, id, c.wantID)
			}
		}
	}
}

func TestServerBatch(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)

	// Empty batch -> [] (JSONRPCExecBatch over zero elements).
	rec := callRPC(t, srv, `[]`)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty batch status = %d, want 200", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("empty batch body = %q, want []", rec.Body.String())
	}

	// Mixed batch: valid, method-not-found, malformed element, non-object.
	body := `[{"method":"dxGetOrder","params":[],"id":1},` +
		`{"method":"dxNope","params":[],"id":2},` +
		`{"method":5,"params":[],"id":3},` +
		`null]`
	rec = callRPC(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch status = %d, want 200 (batch always 200)", rec.Code)
	}
	var arr []rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("batch reply not an array: %v", err)
	}
	if len(arr) != 4 {
		t.Fatalf("batch reply length = %d, want 4", len(arr))
	}
	// Element 0: dxGetOrder with no params -> business error in result.
	if arr[0].Error != nil {
		t.Errorf("element 0 should be business error in result: %+v", arr[0])
	}
	// Element 1: unknown method -> -32601.
	ee, _ := arr[1].Error.(map[string]interface{})
	if code, _ := ee["code"].(float64); int(code) != -32601 {
		t.Errorf("element 1 code = %v, want -32601", arr[1].Error)
	}
	// Element 2: non-string method -> -32600.
	ee, _ = arr[2].Error.(map[string]interface{})
	if code, _ := ee["code"].(float64); int(code) != -32600 {
		t.Errorf("element 2 code = %v, want -32600", arr[2].Error)
	}
	// Element 3: null -> "Invalid Request object" -32600.
	ee, _ = arr[3].Error.(map[string]interface{})
	if code, _ := ee["code"].(float64); int(code) != -32600 {
		t.Errorf("element 3 code = %v, want -32600", arr[3].Error)
	}
	if msg, _ := ee["message"].(string); msg != "Invalid Request object" {
		t.Errorf("element 3 message = %q", msg)
	}
}

func TestServerNamedParamsRejected(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	// dx* commands register no named args -> any key throws -8
	// "Unknown named parameter <key>" (server.cpp:549-551).
	rec := callRPC(t, srv, `{"method":"dxGetOrder","params":{"id":"abc"},"id":1}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("named-params status = %d, want 500", rec.Code)
	}
	var env rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	ee, _ := env.Error.(map[string]interface{})
	if code, _ := ee["code"].(float64); int(code) != -8 {
		t.Errorf("named-params code = %v, want -8", ee["code"])
	}
	if msg, _ := ee["message"].(string); msg != "Unknown named parameter id" {
		t.Errorf("named-params message = %q", msg)
	}

	// Empty object {} is treated as no params (transformNamedArguments no-op).
	rec = callRPC(t, srv, `{"method":"dxGetOrder","params":{},"id":2}`)
	if rec.Code != http.StatusOK {
		t.Errorf("empty-params-object status = %d, want 200", rec.Code)
	}
}

func TestServerEnvelopeStatusCodes(t *testing.T) {
	cases := []struct {
		code int
		want int
	}{
		{-32600, http.StatusBadRequest},
		{-32601, http.StatusNotFound},
		{-32700, http.StatusInternalServerError},
		{-1, http.StatusInternalServerError},
		{-8, http.StatusInternalServerError},
		{-32603, http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := httpStatusForCode(c.code); got != c.want {
			t.Errorf("httpStatusForCode(%d) = %d, want %d", c.code, got, c.want)
		}
	}
}

func TestServerPanicStatus500(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	// A handler that panics must surface as envelope -32603, HTTP 500
	// (previously HTTP 200).
	before := dispatch["dxPanicTest"]
	dispatch["dxPanicTest"] = func(*HandlerCtx, []json.RawMessage) (interface{}, *rpcError) {
		panic("boom")
	}
	defer func() { dispatch["dxPanicTest"] = before }()

	rec := callRPC(t, srv, `{"method":"dxPanicTest","params":[],"id":42}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d, want 500", rec.Code)
	}
	var env rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	ee, _ := env.Error.(map[string]interface{})
	if code, _ := ee["code"].(float64); int(code) != -32603 {
		t.Errorf("panic code = %v, want -32603", ee["code"])
	}
	var id interface{}
	_ = json.Unmarshal(env.ID, &id)
	if id != float64(42) {
		t.Errorf("panic id = %v, want 42", id)
	}
}
