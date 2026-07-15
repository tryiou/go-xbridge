package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xbridge-go/proto"
)

func newTestCtx() *HandlerCtx {
	store := NewStore()
	return &HandlerCtx{Store: store, Node: &Node{store: store}, Config: &Config{}}
}

func TestDxGetOrderRoundTrip(t *testing.T) {
	ctx := newTestCtx()
	// Inject a known order directly into the store.
	body := &proto.OrderBody{
		ID:             [32]byte{0x99},
		FromCurrency:   "SYS",
		FromAmount:     1500000,
		ToCurrency:     "LTC",
		ToAmount:       150000,
		Created:        uint64(1516040130000000), // microsec
		PartialAllowed: false,
		MinFromAmount:  1500000,
	}
	o := normalizeFromOrderBody(body, "pubkeyhex")
	ctx.Store.Add(o)
	idHex := hex.EncodeToString(o.ID[:])

	res, err := ctx.dxGetOrder([]json.RawMessage{json.RawMessage(`"` + idHex + `"`)})
	if err != nil {
		t.Fatalf("dxGetOrder error: %+v", err)
	}
	obj, ok := res.(orderListResult)
	if !ok {
		t.Fatalf("dxGetOrder result type = %T", res)
	}
	if obj.ID != idHex || obj.Maker != "SYS" || obj.Taker != "LTC" {
		t.Errorf("dxGetOrder fields wrong: %+v", obj)
	}
	if obj.MakerSize != "1.5000000" {
		t.Errorf("dxGetOrder maker_size = %q, want 1.5000000", obj.MakerSize)
	}
	if obj.CreatedAt != "2018-01-15T18:15:30.000Z" {
		t.Errorf("dxGetOrder created_at = %q", obj.CreatedAt)
	}
	if obj.Status != "open" {
		t.Errorf("dxGetOrder status = %q", obj.Status)
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
