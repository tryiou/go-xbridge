package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestMethodNameCaseInsensitive locks in Blocknet Core parity (CRPCTable
// lowercases method names on register and lookup, server.cpp): any casing of
// a supported dx* method must dispatch, not answer 404/-32601. Regression
// test for trading bots sending dxloadxbridgeconf/dxgetlocaltokens/dxgetutxos/
// dxflushcancelledorders/dxgetorderbook in lowercase.
func TestMethodNameCaseInsensitive(t *testing.T) {
	cases := []string{
		"dxloadxbridgeconf",
		"dxflushcancelledorders",
		"dxgetutxos",
		"dxgetlocaltokens",
		"dxgetorderbook",
		"DXGETMYORDERS",
		"DxGetOrder",
		"dxLoadXBridgeConf",
		"HELP",
		"GetNetworkInfo",
	}
	for _, m := range cases {
		if Lookup(m) == nil {
			t.Errorf("Lookup(%q) = nil, want handler (case-insensitive Core parity)", m)
		}
	}
}

// TestMethodNameCaseInsensitiveServer ensures lowercase methods reach the
// handler through the full HTTP envelope (200 or business 1025), never 404.
// dxLoadXBridgeConf is covered at Lookup level only: it needs a live conf path
// and panics on the empty fixture node.
func TestMethodNameCaseInsensitiveServer(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	bodies := []string{
		`{"method":"dxgetlocaltokens","params":[],"id":1}`,
		`{"method":"dxgetmyorders","params":[],"id":3}`,
		`{"method":"DXGETORDERBOOK","params":[1,"BTC","BLOCK"],"id":4}`,
	}
	for _, body := range bodies {
		rec := callRPC(t, srv, body)
		if rec.Code == http.StatusNotFound {
			t.Errorf("body %s: status = 404, want handler dispatch (case-insensitive)", body)
			continue
		}
		var env rpcResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Errorf("body %s: bad envelope: %v", body, err)
		}
	}
}

// TestUnknownMethodStillNotFound guards the fix: truly unknown methods (and// the intentionally unexposed gettradingdata) must still answer 404/-32601.
func TestUnknownMethodStillNotFound(t *testing.T) {
	ctx := newTestCtx()
	srv := NewServer(ctx)
	for _, body := range []string{
		`{"method":"dxNope","params":[],"id":1}`,
		`{"method":"gettradingdata","params":[],"id":2}`,
		`{"method":"GETTRADINGDATA","params":[],"id":3}`,
	} {
		rec := callRPC(t, srv, body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("body %s: status = %d, want 404", body, rec.Code)
		}
	}
}

// TestCaseVariantArityKeepsCanonicalName ensures a lowercase method violating
// arity yields the canonical business-1025 name (C++ __FUNCTION__ parity),
// not the request casing.
func TestCaseVariantArityKeepsCanonicalName(t *testing.T) {
	rerr := checkArity("dxgetmyorders", 1)
	if rerr == nil || rerr.envelope {
		t.Fatalf("checkArity(dxgetmyorders, 1) = %v, want business 1025", rerr)
	}
	if rerr.Code != errInvalidParameters {
		t.Errorf("code = %d, want %d", rerr.Code, errInvalidParameters)
	}
	if rerr.Name != "dxGetMyOrders" {
		t.Errorf("name = %q, want canonical %q", rerr.Name, "dxGetMyOrders")
	}
}
