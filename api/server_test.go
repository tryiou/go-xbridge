package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	// Unknown method -> envelope error, result null.
	rec := callRPC(t, srv, `{"method":"dxNope","params":[],"id":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var env rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error == nil || env.Result != nil {
		t.Errorf("unknown method should set envelope error and null result: %+v", env)
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

// TestServerRPCAuth verifies SEC-F01: with -rpcuser/-rpcpassword configured, the
// JSON-RPC server requires valid HTTP Basic credentials (401 + challenge
// otherwise) and rejects with constant-time semantics; without credentials
// configured, requests pass through unchanged (the loopback-default contract).
func TestServerRPCAuth(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	srv.SetAuth("alice", "s3cret")

	// Missing Authorization header -> 401 + WWW-Authenticate challenge.
	rec := callRPC(t, srv, `{"method":"dxGetOrder","params":[],"id":1}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing creds status = %d, want 401", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("missing WWW-Authenticate challenge, got %q", rec.Header().Get("WWW-Authenticate"))
	}

	// Wrong password -> 401.
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

	// SetAuth with only one of user/pass disables auth (both required).
	srv.SetAuth("alice", "")
	rec = callRPC(t, srv, `{"method":"dxGetOrder","params":[],"id":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial creds should disable auth, status = %d", rec.Code)
	}
}

// TestServerMaxBodyBytes verifies RPC-F49: an oversized JSON-RPC body is rejected
// by the http.MaxBytesReader gate rather than buffered/decoded unbounded.
func TestServerMaxBodyBytes(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	big := strings.Repeat(" ", rpcMaxBodyBytes+1) // > 4 MiB
	rec := callRPC(t, srv, `{"method":"dxGetOrder","params":[],"id":1}`+"\n"+big)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 400/413 (MaxBytesReader)", rec.Code)
	}
}
