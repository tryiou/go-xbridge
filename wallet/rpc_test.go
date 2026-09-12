package wallet

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-xbridge/coins"
)

// mockMsgSigB64 is a fixed base64 BIP137 signature (65 raw bytes) the mock
// signmessage handler returns, so SignMessage's decode path is exercised.
var mockMsgSigB64 = func() string {
	b := make([]byte, 65)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(b)
}()

// mockRPC returns a test server that answers the RPC methods the connector
// calls with canned responses, checking basic auth.
// lastReq captures the most recent JSON-RPC request the mock received, so tests
// can assert on the method and params shape (e.g. getnewaddress must be sent
// with an empty params array, matching C++ rpc::getNewAddress).
var lastReq rpcRequest

func mockRPC(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "u" || pass != "p" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		lastReq = req
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		res := func(raw string) { _ = enc.Encode(rpcResponse{Result: json.RawMessage(raw), ID: req.ID}) }
		switch req.Method {
		case "getnewaddress":
			res(`"bc1qw508d6qezfhxq0t9wy3j9tg9z4r0r8e0j0q0w"`)
		case "listunspent":
			res(`[{"txid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","vout":0,"address":"bc1qw508d6qezfhxq0t9wy3j9tg9z4r0r8e0j0q0w","amount":1.5,"scriptPubKey":"76a914deadbeef88ac","confirmations":6,"spendable":true}]`)
		case "signrawtransaction":
			res(`{"hex":"deadbeef","complete":true}`)
		case "signrawtransactionwithwallet":
			res(`{"hex":"deadbeef","complete":true}`)
		case "sendrawtransaction":
			res(`"txid1234567890"`)
		case "getinfo":
			res(`{"relayfee":0.0001}`)
		case "getblockcount":
			res(`100`)
		case "getblockhash":
			// Display (big-endian) order: MSB-first. Reversed to internal
			// (little-endian) order by revHashHex, so the internal form has
			// 0xff in its last byte.
			res(`"ff00000000000000000000000000000000000000000000000000000000000000"`)
		case "signmessage":
			res(`"` + mockMsgSigB64 + `"`)
		case "verifymessage":
			res(`true`)
		default:
			res(`null`)
		}
	})
	return httptest.NewServer(mux)
}

func TestRPCConnector(t *testing.T) {
	srv := mockRPC(t)
	defer srv.Close()

	c := NewRPCConnector(Chain{Ticker: "BTC", Endpoint: srv.URL, User: "u", Pass: "p", Decimals: 8})

	t.Run("GetNewAddress", func(t *testing.T) {
		addr, err := c.GetNewAddress()
		if err != nil {
			t.Fatalf("GetNewAddress: %v", err)
		}
		if addr != "bc1qw508d6qezfhxq0t9wy3j9tg9z4r0r8e0j0q0w" {
			t.Fatalf("addr = %q", addr)
		}
		// Loyalty check: C++ rpc::getNewAddress calls "getnewaddress" with an
		// EMPTY params array (no label / no address-type). The Go port must not
		// force positional args (e.g. ["", "bech32"]), which strict wallets such
		// as DASH reject with code=-1 "Usage: getnewaddress".
		if lastReq.Method != "getnewaddress" {
			t.Fatalf("method = %q, want getnewaddress", lastReq.Method)
		}
		if len(lastReq.Params) != 0 {
			t.Fatalf("getnewaddress params = %v, want empty (C++ sends no params)", lastReq.Params)
		}
	})

	t.Run("ListUnspent", func(t *testing.T) {
		utxos, err := c.ListUnspent(1)
		if err != nil {
			t.Fatalf("ListUnspent: %v", err)
		}
		// C++ parity: listunspent is called with NO params (empty array).
		if lastReq.Method != "listunspent" {
			t.Fatalf("method = %q, want listunspent", lastReq.Method)
		}
		if len(lastReq.Params) != 0 {
			t.Fatalf("listunspent params = %v, want empty (C++ sends no params)", lastReq.Params)
		}
		if len(utxos) != 1 {
			t.Fatalf("want 1 utxo, got %d", len(utxos))
		}
		u := utxos[0]
		if u.Vout != 0 || u.Confirmations != 6 {
			t.Fatalf("unexpected utxo %+v", u)
		}
		// 1.5 BTC * 1e8 = 150000000 sat.
		if u.Amount != 150000000 {
			t.Fatalf("amount = %d, want 150000000", u.Amount)
		}
		if u.ScriptPubKey != "76a914deadbeef88ac" {
			t.Fatalf("scriptPubKey = %q", u.ScriptPubKey)
		}
	})

	t.Run("SignRawTransaction", func(t *testing.T) {
		hex, complete, err := c.SignRawTransaction("abcd", []PrevTx{{
			TxID:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Vout:         0,
			ScriptPubKey: "76a914deadbeef88ac",
			Amount:       150000000,
		}})
		if err != nil {
			t.Fatalf("SignRawTransaction: %v", err)
		}
		if hex != "deadbeef" || !complete {
			t.Fatalf("sign = %q complete=%v", hex, complete)
		}
	})

	t.Run("SendRawTransaction", func(t *testing.T) {
		txid, err := c.SendRawTransaction("abcd")
		if err != nil {
			t.Fatalf("SendRawTransaction: %v", err)
		}
		if txid != "txid1234567890" {
			t.Fatalf("txid = %q", txid)
		}
	})

	t.Run("GetRelayFee", func(t *testing.T) {
		fee, err := c.GetRelayFee()
		if err != nil {
			t.Fatalf("GetRelayFee: %v", err)
		}
		if fee != 0.0001 {
			t.Fatalf("relayfee = %v, want 0.0001", fee)
		}
	})
	t.Run("GetBlockCount", func(t *testing.T) {
		h, err := c.GetBlockCount()
		if err != nil {
			t.Fatalf("GetBlockCount: %v", err)
		}
		if h != 100 {
			t.Fatalf("count = %d, want 100", h)
		}
	})
	t.Run("GetBlockHash", func(t *testing.T) {
		// Display-order "ff00..00" reverses to internal "00..00ff" (last byte 0xff).
		var want [32]byte
		want[31] = 0xff
		h, err := c.GetBlockHash(99)
		if err != nil {
			t.Fatalf("GetBlockHash: %v", err)
		}
		if h != want {
			t.Fatalf("hash = %x, want %x", h, want)
		}
	})
}

// TestRevHashHexCapturedBlockHash pins the block-hash byte order
// against a real captured block hash (the Bitcoin genesis block, display
// order). getblockhash returns display order; the XBridge wire carries the
// internal (little-endian) bytes, which the C++ base_blob<256>::SetHex
// (uint256.cpp:27-53) stores as the display bytes REVERSED (last hex pair in
// data[0]). revHashHex must produce exactly those bytes.
func TestRevHashHexCapturedBlockHash(t *testing.T) {
	display := "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f"
	got, err := revHashHex(display)
	if err != nil {
		t.Fatalf("revHashHex: %v", err)
	}
	wantHex := "6fe28c0ab6f1b372c1a6a246ae63f74f931e8365e15a089c68d6190000000000"
	var want [32]byte
	b, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatal(err)
	}
	copy(want[:], b)
	if got != want {
		t.Fatalf("revHashHex(%s)\n got %x\nwant %x", display, got, want)
	}
	// The reversal is an involution: reversing the internal bytes yields the
	// display hash again (the wire reader round-trips).
	var back [32]byte
	for i := range got {
		back[i] = got[31-i]
	}
	if h := hex.EncodeToString(back[:]); h != display {
		t.Errorf("round-trip = %s, want %s", h, display)
	}
}

func TestRPCConnectorUnauthorized(t *testing.T) {
	srv := mockRPC(t)
	defer srv.Close()
	c := NewRPCConnector(Chain{Ticker: "BTC", Endpoint: srv.URL, User: "bad", Pass: "bad", Decimals: 8})
	if _, err := c.GetNewAddress(); err == nil {
		t.Fatal("expected auth error")
	}
}

// TestListUnspentFiltering verifies the C++-parity client-side filter
// (xbridgewalletconnectorbtc.cpp:536-577): non-spendable entries, non-positive
// amounts, and non-positive confirmations are dropped; a UTXO with confirmations
// below the caller's minConf is dropped; a UTXO missing the confirmations field
// (confs==-1 in C++) is kept.
func TestListUnspentFiltering(t *testing.T) {
	body := `[
		{"txid":"1111111111111111111111111111111111111111111111111111111111111111","vout":0,"amount":1.0,"scriptPubKey":"51","confirmations":6,"spendable":true},
		{"txid":"2222222222222222222222222222222222222222222222222222222222222222","vout":0,"amount":1.0,"scriptPubKey":"51","confirmations":6,"spendable":false},
		{"txid":"3333333333333333333333333333333333333333333333333333333333333333","vout":0,"amount":0,"scriptPubKey":"51","confirmations":6,"spendable":true},
		{"txid":"4444444444444444444444444444444444444444444444444444444444444444","vout":0,"amount":1.0,"scriptPubKey":"51","confirmations":0,"spendable":true},
		{"txid":"5555555555555555555555555555555555555555555555555555555555555555","vout":0,"amount":1.0,"scriptPubKey":"51","confirmations":2,"spendable":true},
		{"txid":"6666666666666666666666666666666666666666666666666666666666666666","vout":0,"amount":1.0,"scriptPubKey":"51","spendable":true}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "listunspent" && len(req.Params) != 0 {
			t.Errorf("listunspent params = %v, want empty", req.Params)
		}
		_ = json.NewEncoder(w).Encode(rpcResponse{Result: json.RawMessage(body), ID: req.ID})
	}))
	defer srv.Close()

	c := NewRPCConnector(Chain{Ticker: "BTC", Endpoint: srv.URL, User: "u", Pass: "p", Decimals: 8})
	utxos, err := c.ListUnspent(5)
	if err != nil {
		t.Fatalf("ListUnspent: %v", err)
	}
	// Kept: #1 (confs 6 >= 5), #6 (confirmations field absent -> confs==-1 kept).
	// Dropped: #2 (spendable=false), #3 (amount 0), #4 (confs 0), #5 (confs 2 < 5).
	got := map[string]bool{}
	for _, u := range utxos {
		got[u.TxID[:1]] = true
	}
	if len(utxos) != 2 || !got["1"] || !got["6"] {
		t.Fatalf("filtered utxos = %d %+v, want #1 and #6 only", len(utxos), utxos)
	}
}

// TestSignRawTransactionFallback verifies the C++ legacy-first behavior
// (xbridgewalletconnectorbtc.cpp:1091-1100): "signrawtransaction" is tried
// first, and when the wallet has removed it (error), the connector falls back
// to "signrawtransactionwithwallet".
func TestSignRawTransactionFallback(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls = append(calls, req.Method)
		enc := json.NewEncoder(w)
		switch req.Method {
		case "signrawtransaction":
			// Simulate a newer wallet that removed the legacy RPC.
			_ = enc.Encode(rpcResponse{Error: &rpcError{Code: -32601, Message: "Method not found"}, ID: req.ID})
		case "signrawtransactionwithwallet":
			_ = enc.Encode(rpcResponse{Result: json.RawMessage(`{"hex":"cafe","complete":true}`), ID: req.ID})
		default:
			_ = enc.Encode(rpcResponse{Result: json.RawMessage(`null`), ID: req.ID})
		}
	}))
	defer srv.Close()

	c := NewRPCConnector(Chain{Ticker: "BTC", Endpoint: srv.URL, User: "u", Pass: "p", Decimals: 8})
	hex, complete, err := c.SignRawTransaction("abcd", nil)
	if err != nil {
		t.Fatalf("SignRawTransaction: %v", err)
	}
	if hex != "cafe" || !complete {
		t.Fatalf("sign = %q complete=%v, want cafe/true", hex, complete)
	}
	if len(calls) != 2 || calls[0] != "signrawtransaction" || calls[1] != "signrawtransactionwithwallet" {
		t.Fatalf("call order = %v, want [signrawtransaction signrawtransactionwithwallet]", calls)
	}
}

// TestSignRawTransactionLegacyPrimary verifies that when the legacy RPC
// succeeds, the connector uses it and does NOT call the modern fallback.
func TestSignRawTransactionLegacyPrimary(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls = append(calls, req.Method)
		enc := json.NewEncoder(w)
		switch req.Method {
		case "signrawtransaction":
			_ = enc.Encode(rpcResponse{Result: json.RawMessage(`{"hex":"beef","complete":true}`), ID: req.ID})
		case "signrawtransactionwithwallet":
			t.Error("modern RPC must not be called when legacy succeeds")
			_ = enc.Encode(rpcResponse{Result: json.RawMessage(`{"hex":"WRONG","complete":true}`), ID: req.ID})
		default:
			_ = enc.Encode(rpcResponse{Result: json.RawMessage(`null`), ID: req.ID})
		}
	}))
	defer srv.Close()

	c := NewRPCConnector(Chain{Ticker: "BTC", Endpoint: srv.URL, User: "u", Pass: "p", Decimals: 8})
	hex, complete, err := c.SignRawTransaction("abcd", nil)
	if err != nil {
		t.Fatalf("SignRawTransaction: %v", err)
	}
	if hex != "beef" || !complete {
		t.Fatalf("sign = %q complete=%v, want beef/true", hex, complete)
	}
	if len(calls) != 1 || calls[0] != "signrawtransaction" {
		t.Fatalf("call order = %v, want [signrawtransaction] only", calls)
	}
}

// TestSignRawTransactionPayloadMatchesCpp pins the signrawtransaction
// request payload to the C++ form (xbridgewalletconnectorbtc.cpp:1055-1089):
// [rawtx, prevtxs|null, keys|null]. The old port sent "ALL" in the privkeys
// slot (position 3) — a sighash type string where C++ sends JSON null — and
// always sent an empty array instead of null for empty prevtxs.
func TestSignRawTransactionPayloadMatchesCpp(t *testing.T) {
	var got [][]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req.Params)
		enc := json.NewEncoder(w)
		switch req.Method {
		case "signrawtransaction":
			_ = enc.Encode(rpcResponse{Error: &rpcError{Code: -32601, Message: "Method not found"}, ID: req.ID})
		case "signrawtransactionwithwallet":
			_ = enc.Encode(rpcResponse{Result: json.RawMessage(`{"hex":"cafe","complete":true}`), ID: req.ID})
		default:
			_ = enc.Encode(rpcResponse{Result: json.RawMessage(`null`), ID: req.ID})
		}
	}))
	defer srv.Close()

	c := NewRPCConnector(Chain{Ticker: "BTC", Endpoint: srv.URL, User: "u", Pass: "p", Decimals: 8})

	// Empty prevTxs -> [rawtx, null, null]; both RPCs must carry it.
	if _, _, err := c.SignRawTransaction("abcd", nil); err != nil {
		t.Fatalf("SignRawTransaction(empty prev): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d RPC calls, want 2", len(got))
	}
	for i, p := range got {
		if len(p) != 3 {
			t.Fatalf("call %d: params = %v, want 3 elements [rawtx, prevtxs|null, keys|null]", i, p)
		}
		if p[0] != "abcd" {
			t.Errorf("call %d: rawtx = %v, want abcd", i, p[0])
		}
		if p[1] != nil {
			t.Errorf("call %d: prevtxs = %v, want null for empty prevTxs", i, p[1])
		}
		if p[2] != nil {
			t.Errorf("call %d: keys = %v, want null (C++ sends no privkeys)", i, p[2])
		}
	}

	// Non-empty prevTxs -> [rawtx, [prev], null].
	got = nil
	if _, _, err := c.SignRawTransaction("beef", []PrevTx{{
		TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0,
		ScriptPubKey: "76a914deadbeef88ac", Amount: 150000000,
	}}); err != nil {
		t.Fatalf("SignRawTransaction(prev): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d RPC calls, want 2", len(got))
	}
	p := got[0]
	if len(p) != 3 || p[2] != nil {
		t.Fatalf("params = %v, want [beef, [prev], null]", p)
	}
	prevArr, ok := p[1].([]interface{})
	if !ok || len(prevArr) != 1 {
		t.Fatalf("prevtxs = %T %v, want a 1-element array", p[1], p[1])
	}
	po, ok := prevArr[0].(map[string]interface{})
	if !ok || po["txid"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || po["scriptPubKey"] != "76a914deadbeef88ac" {
		t.Fatalf("prevtx entry = %v, want the mapped PrevTx fields", prevArr[0])
	}
	if vout, ok := po["vout"].(float64); !ok || vout != 0 {
		t.Fatalf("prevtx vout = %v, want 0", po["vout"])
	}
	if amt, ok := po["amount"].(float64); !ok || amt != 1.5 {
		t.Fatalf("prevtx amount = %v, want 1.5 (whole coins, like C++)", po["amount"])
	}
}

// slowRPC returns a test server whose every handler blocks longer than the
// client timeout, to exercise the timeout path.
func slowRPC(delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		enc := json.NewEncoder(w)
		_ = enc.Encode(rpcResponse{Result: json.RawMessage(`"ok"`), ID: "1"})
	}))
}

// TestRPCClientTimeout confirms a hung wallet RPC surfaces an error instead of
// blocking the caller (which would wedge the swap feed goroutine) forever.
func TestRPCClientTimeout(t *testing.T) {
	srv := slowRPC(300 * time.Millisecond)
	defer srv.Close()
	c := NewRPCClient(srv.URL, "u", "p", "", "", false, 50*time.Millisecond, "BTC")
	var out string
	err := c.Call("getblockcount", nil, &out)
	if err == nil {
		t.Fatal("expected timeout error for a slow RPC")
	}
	if !strings.Contains(err.Error(), "Client.Timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected a timeout error, got: %v", err)
	}
}

// TestRPCClientDefaultTimeout confirms NewRPCClient applies the 30s default
// when no explicit timeout is given (zero value), so callers relying on the
// constructor always get a bounded client.
func TestRPCClientDefaultTimeout(t *testing.T) {
	c := NewRPCClient("http://example.invalid", "u", "p", "", "", false, 0, "BTC")
	if c.http.Timeout != defaultRPCTimeout {
		t.Fatalf("expected default timeout %v, got %v", defaultRPCTimeout, c.http.Timeout)
	}
}

// captureServer echoes the request body it received so tests can assert on the
// exact JSON the client sends (including the jsonrpc field and params shape).
func captureServer(t *testing.T) (*httptest.Server, *[]byte) {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = b
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rpcResponse{Result: json.RawMessage(`0`), ID: "x"})
	}))
	return srv, &got
}

// TestRPCClientJSONRPCField asserts the "jsonrpc" field is emitted only when a
// non-empty JSONVersion is configured (C++ XBridgeJSONRPCRequestObj pushes
// "jsonrpc" only when non-empty) and is always omitted when omitJSONVersion is
// set (XLite-style wallets reject the field).
func TestRPCClientJSONRPCField(t *testing.T) {
	cases := []struct {
		name      string
		version   string
		omit      bool
		wantField bool
		wantValue string
	}{
		{"empty version omits field", "", false, false, ""},
		{"explicit 1.0 keeps field", "1.0", false, true, `"jsonrpc":"1.0"`},
		{"explicit 2.0 keeps field verbatim", "2.0", false, true, `"jsonrpc":"2.0"`},
		{"omit drops field", "1.0", true, false, ""},
		{"omit drops field even when empty version", "", true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := captureServer(t)
			defer srv.Close()
			c := NewRPCClient(srv.URL, "u", "p", tc.version, "", tc.omit, 0, "BTC")
			var out int
			if err := c.Call("getblockcount", nil, &out); err != nil {
				t.Fatal(err)
			}
			has := bytes.Contains(*got, []byte(`"jsonrpc"`))
			if has != tc.wantField {
				t.Fatalf("jsonrpc field present=%v, want %v (body %s)", has, tc.wantField, *got)
			}
			if tc.wantField && !bytes.Contains(*got, []byte(tc.wantValue)) {
				t.Fatalf("expected %s, got %s", tc.wantValue, *got)
			}
		})
	}
}

// TestRPCClientContentTypeHeader asserts the Content-Type request header is set
// only when a non-empty ContentType is configured, matching C++ CallRPC which
// adds the header only when contenttype is non-empty.
func TestRPCClientContentTypeHeader(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		wantHeader  string // "" means header must be absent
	}{
		{"empty leaves header unset", "", ""},
		{"explicit application/json", "application/json", "application/json"},
		{"explicit custom", "text/plain", "text/plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotCT string
			var present bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, present = r.Header["Content-Type"]
				gotCT = r.Header.Get("Content-Type")
				_ = json.NewEncoder(w).Encode(rpcResponse{Result: json.RawMessage(`0`), ID: "x"})
			}))
			defer srv.Close()
			c := NewRPCClient(srv.URL, "u", "p", "1.0", tc.contentType, false, 0, "BTC")
			var out int
			if err := c.Call("getblockcount", nil, &out); err != nil {
				t.Fatal(err)
			}
			if tc.wantHeader == "" {
				if present {
					t.Fatalf("Content-Type header should be absent, got %q", gotCT)
				}
				return
			}
			if gotCT != tc.wantHeader {
				t.Fatalf("Content-Type = %q, want %q", gotCT, tc.wantHeader)
			}
		})
	}
}

// TestRPCClientParamsDefault asserts a nil params argument is sent as an empty
// array "params":[], never as "params":null — XLite rejects null params with an
// empty body (HTTP 400), surfacing as a decode error.
func TestRPCClientParamsDefault(t *testing.T) {
	srv, got := captureServer(t)
	defer srv.Close()
	c := NewRPCClient(srv.URL, "u", "p", "1.0", "", false, 0, "BTC")
	var out int
	if err := c.Call("getblockcount", nil, &out); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(*got, []byte(`"params":null`)) {
		t.Fatalf("params must not be null, got %s", *got)
	}
	if !bytes.Contains(*got, []byte(`"params":[]`)) {
		t.Fatalf("expected params:[], got %s", *got)
	}
}

// TestRPCConnectorSignMessage exercises the BIP137 proof plumbing: signmessage
// returns a base64 compact sig that SignMessage decodes to 65 raw bytes, and
// verifymessage round-trips true.
func TestRPCConnectorSignMessage(t *testing.T) {
	srv := mockRPC(t)
	defer srv.Close()
	c := NewRPCConnector(Chain{Ticker: "BTC", Endpoint: srv.URL, User: "u", Pass: "p", Decimals: 8})
	sig, err := c.SignMessage("1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2", "msg")
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}
	if len(sig) != 65 {
		t.Fatalf("SignMessage sig len = %d, want 65", len(sig))
	}
	want, _ := base64.StdEncoding.DecodeString(mockMsgSigB64)
	if !bytes.Equal(sig, want) {
		t.Fatalf("SignMessage sig = %x, want %x", sig, want)
	}
	ok, err := c.VerifyMessage("1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2", sig, "msg")
	if err != nil || !ok {
		t.Fatalf("VerifyMessage ok=%v err=%v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// CheckDepositTransaction goldens — fixtures mirror the C++ writers
// (xbridgewalletconnectorbtc.cpp:1981-2194), not comments.
// ---------------------------------------------------------------------------

// testTxID returns the display-order txid of a serialized tx.
func testTxID(b []byte) string {
	h1 := sha256.Sum256(b)
	h2 := sha256.Sum256(h1[:])
	var out [32]byte
	for i := 0; i < 32; i++ {
		out[i] = h2[31-i]
	}
	return hex.EncodeToString(out[:])
}

// depositFixtures builds the RPC fixtures for the goldens. The funding prevout
// tx carries a single 3.0 BTC output; the deposit spends it (SEQUENCE_FINAL),
// locking 2.500226 BTC into the expected p2sh (2.5 amount + minTxFee2(1,1)
// 0.000226) with 0.4995 BTC change. Note the C++ totalVoutAmount accrual quirk:
// the vout loop breaks on the FIRST p2sh script match, so vouts after it (the
// change) are excluded; counterpartyFees = totalVin − p2sh = 0.499774 clears
// the fee1 band trivially. Fixed chain: FeePerByte=100 sat/vB (whole 1e-6),
// MinTxFee=0, scale 1e8.
type depositFixtures struct {
	fundingHex    string
	fundingTxID   string
	depositHex    string
	depositTxID   string
	p2shScriptHex string
}

func buildDepositFixtures(t *testing.T) *depositFixtures {
	t.Helper()
	// Funding prevout: 3.0 BTC in output 0.
	funding := &coins.Tx{Version: 1}
	funding.Outputs = append(funding.Outputs, coins.TxOut{Value: 300000000, ScriptPubKey: []byte{0x51}})
	fundingHex := hex.EncodeToString(funding.Serialize())
	fundingTxID := testTxID(funding.Serialize())
	fundingInternal, err := revHashHex(fundingTxID)
	if err != nil {
		t.Fatal(err)
	}

	p2sh := coins.BuildP2SHScript([20]byte{0xde, 0xad, 0xbe, 0xef})

	// Deposit: spend funding:0, output0 = 2.500226 p2sh, output1 = 0.4995 change.
	deposit := &coins.Tx{Version: 1}
	deposit.Inputs = append(deposit.Inputs, coins.TxIn{PrevOut: coins.OutPoint{Hash: fundingInternal, Index: 0}, Sequence: seqFinal})
	deposit.Outputs = append(deposit.Outputs,
		coins.TxOut{Value: 250022600, ScriptPubKey: p2sh},
		coins.TxOut{Value: 49950000, ScriptPubKey: []byte{0x51}},
	)
	return &depositFixtures{
		fundingHex:    fundingHex,
		fundingTxID:   fundingTxID,
		depositHex:    hex.EncodeToString(deposit.Serialize()),
		depositTxID:   testTxID(deposit.Serialize()),
		p2shScriptHex: hex.EncodeToString(p2sh),
	}
}

// checkDepositServer serves the RPCs CheckDepositTransaction issues. The
// deposit's raw hex is fully controlled per test (depositRaw); the funding
// prevout's verbose JSON is served only for the fixture funding txid, so any
// other vin txid lookup fails as "vin tx not found ...waiting".
type checkDepositServer struct {
	fx             *depositFixtures
	depositRaw     string // getrawtransaction [txid,0] raw hex
	depositErr     bool   // getrawtransaction fails (tx not found)
	depositVerbose string // verbose body for the DEPOSIT txid ("" → error, wallet-only)
	confs          string // gettxout result body ("" → default 6 confirmations)
	confsNull      bool   // gettxout returns null (unknown output)
	confsVout1     string // gettxout result body when vout==1 ("null" → null; "" → default path)
	fundingValue   string // prevout value (whole BTC); "" → "3.0"
	calls          []string
}

func newCheckDepositServer(t *testing.T, cfg *checkDepositServer) *httptest.Server {
	t.Helper()
	fx := cfg.fx
	fv := cfg.fundingValue
	if fv == "" {
		fv = "3.0"
	}
	fundingVerbose := fmt.Sprintf(`{"txid":%q,"vin":[{"txid":null,"sequence":4294967295}],"vout":[{"value":%s,"n":0,"scriptPubKey":{"hex":"51"}}]}`, fx.fundingTxID, fv)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		cfg.calls = append(cfg.calls, req.Method)
		enc := json.NewEncoder(w)
		respond := func(res string) { _ = enc.Encode(rpcResponse{Result: json.RawMessage(res), ID: req.ID}) }
		respondErr := func(msg string) {
			_ = enc.Encode(rpcResponse{Error: &rpcError{Code: -5, Message: msg}, ID: req.ID})
		}
		switch req.Method {
		case "getrawtransaction":
			if cfg.depositErr {
				respondErr("No such mempool or blockchain transaction.")
				return
			}
			if len(req.Params) > 1 {
				if v, ok := req.Params[1].(float64); ok && v == 1 {
					if txid, _ := req.Params[0].(string); txid == fx.fundingTxID {
						respond(fundingVerbose)
						return
					}
					if txid, _ := req.Params[0].(string); txid == fx.depositTxID && cfg.depositVerbose != "" {
						respond(cfg.depositVerbose)
						return
					}
					respondErr("vin tx not found")
					return
				}
			}
			respond(`"` + cfg.depositRaw + `"`)
		case "gettxout":
			if len(req.Params) > 1 {
				if v, ok := req.Params[1].(float64); ok && v == 1 && cfg.confsVout1 != "" {
					if cfg.confsVout1 == "null" {
						respond(`null`)
						return
					}
					respond(cfg.confsVout1)
					return
				}
			}
			if cfg.confsNull {
				respond(`null`)
				return
			}
			if cfg.confs == "" {
				respond(`{"confirmations":6}`)
				return
			}
			respond(cfg.confs)
		default:
			respond(`null`)
		}
	}))
}

// checkDepositConn builds an RPCConnector for a checkDepositServer with the
// fixed fee/scale settings the goldens assert against (FeePerByte=100 sat/vB
// → whole 1e-6; MinTxFee=0; BTC scale 1e8).
func checkDepositConn(srvURL string) *RPCConnector {
	return NewRPCConnector(Chain{Ticker: "BTC", Endpoint: srvURL, User: "u", Pass: "p",
		Decimals: 8, FeePerByte: 100, MinTxFee: 0})
}

const depositAmountXB = 2500000 // 2.5 in XBridge 1e6 base

func TestCheckDepositTransaction(t *testing.T) {
	fx := buildDepositFixtures(t)

	// newDeposit returns the fixture deposit with a per-test mutation applied,
	// rewires the server's depositRaw, and yields (conn, callLog).
	newDeposit := func(cfg *checkDepositServer, mutate func(*coins.Tx)) (*RPCConnector, *checkDepositServer) {
		cfg.fx = fx
		cfg.depositRaw = fx.depositHex
		if mutate != nil {
			raw, err := hex.DecodeString(fx.depositHex)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := coins.Deserialize(raw)
			if err != nil {
				t.Fatal(err)
			}
			mutate(tx)
			cfg.depositRaw = hex.EncodeToString(tx.Serialize())
		}
		srv := newCheckDepositServer(t, cfg)
		t.Cleanup(srv.Close)
		return checkDepositConn(srv.URL), cfg
	}

	t.Run("good", func(t *testing.T) {
		c, cfg := newDeposit(&checkDepositServer{}, nil)
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if err != nil {
			t.Fatalf("CheckDepositTransaction: %v", err)
		}
		if !dc.IsGood || dc.P2SHAmount != 2500226 || dc.DepositVout != 0 || dc.Excess != 0 {
			t.Fatalf("verdict = %+v, want IsGood with P2SHAmount 2500226 vout 0 excess 0", dc)
		}
		// Call order mirrors C++: raw getrawtransaction → gettxout gate →
		// verbose getrawtransaction (prevout).
		want := []string{"getrawtransaction", "gettxout", "getrawtransaction"}
		if len(cfg.calls) != len(want) {
			t.Fatalf("call log = %v, want %v", cfg.calls, want)
		}
		for i := range want {
			if cfg.calls[i] != want[i] {
				t.Fatalf("call log = %v, want %v", cfg.calls, want)
			}
		}
	})

	t.Run("not found waits", func(t *testing.T) {
		c, _ := newDeposit(&checkDepositServer{depositErr: true}, nil)
		_, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if !errors.Is(err, ErrDepositNotReady) {
			t.Fatalf("err = %v, want ErrDepositNotReady", err)
		}
	})

	t.Run("insufficient confirmations waits", func(t *testing.T) {
		c, _ := newDeposit(&checkDepositServer{confs: `{"confirmations":1}`}, nil)
		_, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if !errors.Is(err, ErrDepositNotReady) {
			t.Fatalf("err = %v, want ErrDepositNotReady", err)
		}
	})

	t.Run("gettxout unknown waits", func(t *testing.T) {
		c, _ := newDeposit(&checkDepositServer{confsNull: true}, nil)
		_, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if !errors.Is(err, ErrDepositNotReady) {
			t.Fatalf("err = %v, want ErrDepositNotReady", err)
		}
	})

	// Zero-conf chain visibility (live-proven S2 hole: the facade serves
	// wallet-known bytes for a broadcast the chain never saw, and with
	// Confirmations=0 the gettxout gate is skipped — B validated a phantom).
	t.Run("zero-conf wallet-only waits", func(t *testing.T) {
		c, cfg := newDeposit(&checkDepositServer{}, nil)
		_, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 0)
		if !errors.Is(err, ErrDepositNotReady) {
			t.Fatalf("err = %v, want ErrDepositNotReady", err)
		}
		// Must have asked the chain (verbose), not trusted wallet bytes alone.
		found := false
		for _, m := range cfg.calls {
			if m == "getrawtransaction" {
				found = true
			}
		}
		if !found || len(cfg.calls) < 2 {
			t.Fatalf("call log = %v, want a verbose chain-visibility query", cfg.calls)
		}
	})

	t.Run("zero-conf chain-visible proceeds", func(t *testing.T) {
		verbose := fmt.Sprintf(`{"txid":%q,"confirmations":0,"vin":[],"vout":[]}`, fx.depositTxID)
		c, cfg := newDeposit(&checkDepositServer{depositVerbose: verbose}, nil)
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 0)
		if err != nil {
			t.Fatalf("CheckDepositTransaction: %v", err)
		}
		if !dc.IsGood {
			t.Fatalf("expected IsGood, got %+v", dc)
		}
		// No gettxout gate at 0-conf; verbose deposit + verbose prevout seen.
		want := []string{"getrawtransaction", "getrawtransaction", "getrawtransaction"}
		if len(cfg.calls) != len(want) {
			t.Fatalf("call log = %v, want %v", cfg.calls, want)
		}
	})

	t.Run("zero-conf conflicted waits", func(t *testing.T) {
		verbose := fmt.Sprintf(`{"txid":%q,"confirmations":-1,"vin":[],"vout":[]}`, fx.depositTxID)
		c, _ := newDeposit(&checkDepositServer{depositVerbose: verbose}, nil)
		_, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 0)
		if !errors.Is(err, ErrDepositNotReady) {
			t.Fatalf("err = %v, want ErrDepositNotReady", err)
		}
	})

	t.Run("missing prevout waits", func(t *testing.T) {
		c, _ := newDeposit(&checkDepositServer{}, func(tx *coins.Tx) {
			tx.Inputs[0].PrevOut.Hash = [32]byte{} // spend an unknown funding txid
		})
		_, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if !errors.Is(err, ErrDepositNotReady) {
			t.Fatalf("err = %v, want ErrDepositNotReady", err)
		}
	})

	t.Run("bad sequence is bad", func(t *testing.T) {
		c, _ := newDeposit(&checkDepositServer{}, func(tx *coins.Tx) {
			tx.Inputs[0].Sequence = seqFinal - 1
		})
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if err != nil {
			t.Fatalf("err = %v, want bad (nil)", err)
		}
		if dc.IsGood {
			t.Fatalf("expected IsGood=false, got %+v", dc)
		}
	})

	t.Run("decode failure is bad", func(t *testing.T) {
		c, cfg := newDeposit(&checkDepositServer{}, nil)
		cfg.depositRaw = "zznothex"
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if err != nil {
			t.Fatalf("err = %v, want bad (nil)", err)
		}
		if dc.IsGood {
			t.Fatalf("expected IsGood=false, got %+v", dc)
		}
	})

	t.Run("no valid p2sh is bad", func(t *testing.T) {
		c, _ := newDeposit(&checkDepositServer{}, func(tx *coins.Tx) {
			tx.Outputs[0].ScriptPubKey = []byte{0x51} // wrong script
		})
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if err != nil {
			t.Fatalf("err = %v, want bad (nil)", err)
		}
		if dc.IsGood {
			t.Fatalf("expected IsGood=false, got %+v", dc)
		}
	})

	t.Run("fee1 shortfall is bad", func(t *testing.T) {
		// Funding input only just covers the p2sh (2.5003 vs 2.500226), so the
		// C++ counterpartyFees (totalVin minus the vouts up to the matched p2sh,
		// which excludes the change output after the break) = 0.000074 <
		// 0.95*fee1 (0.000247).
		c, _ := newDeposit(&checkDepositServer{fundingValue: "2.5003"}, func(tx *coins.Tx) {
			tx.Outputs = tx.Outputs[:1] // p2sh only, no change
		})
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if err != nil {
			t.Fatalf("err = %v, want bad (nil)", err)
		}
		if dc.IsGood {
			t.Fatalf("expected IsGood=false, got %+v", dc)
		}
	})

	t.Run("fee2 shortfall is bad", func(t *testing.T) {
		// P2SH lowered to 2.5 < 2.5 + 0.95*fee2.
		c, _ := newDeposit(&checkDepositServer{}, func(tx *coins.Tx) {
			tx.Outputs[0].Value = 250000000
			tx.Outputs[1].Value = 49900000 // keep totals: fee stays 0.000274 ≥ fee1 band
		})
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if err != nil {
			t.Fatalf("err = %v, want bad (nil)", err)
		}
		if dc.IsGood {
			t.Fatalf("expected IsGood=false, got %+v", dc)
		}
	})

	t.Run("excess computed", func(t *testing.T) {
		// P2SH 2.510226 = 2.5 + fee2 + 0.01 excess.
		c, _ := newDeposit(&checkDepositServer{}, func(tx *coins.Tx) {
			tx.Outputs[0].Value = 251022600
			tx.Outputs[1].Value = 48950000 // keep fee constant 0.000274
		})
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !dc.IsGood {
			t.Fatalf("expected good, got %+v", dc)
		}
		if dc.Excess != 10000 || dc.P2SHAmount != 2510226 {
			t.Fatalf("Excess/P2SHAmount = %d/%d, want 10000/2510226", dc.Excess, dc.P2SHAmount)
		}
	})

	t.Run("no confirmation gate when required=0", func(t *testing.T) {
		// gettxout is never consulted at 0-conf (confsNull proves it), but
		// the chain-visibility gate applies instead: wallet bytes alone are
		// not enough.
		verbose := fmt.Sprintf(`{"txid":%q,"confirmations":0,"vin":[],"vout":[]}`, fx.depositTxID)
		c, _ := newDeposit(&checkDepositServer{confsNull: true, depositVerbose: verbose}, nil)
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 0)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !dc.IsGood {
			t.Fatalf("expected good (gettxout skipped, chain visible), got %+v", dc)
		}
	})

	t.Run("p2sh at vout1 gates on vout1", func(t *testing.T) {
		// Deposit with the P2SH at output 1 (change first): the entry gate
		// reads gettxout(txid, 0) like C++ (whose depositTxVout out-param is
		// still 0 there), but confirmations must be judged on the actual
		// P2SH output once discovered.
		swapOutputs := func(tx *coins.Tx) {
			tx.Outputs[0], tx.Outputs[1] = tx.Outputs[1], tx.Outputs[0]
		}
		c, _ := newDeposit(&checkDepositServer{}, swapOutputs)
		dc, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !dc.IsGood || dc.DepositVout != 1 || dc.P2SHAmount != 2500226 {
			t.Fatalf("verdict = %+v, want IsGood with P2SHAmount 2500226 vout 1", dc)
		}
	})

	t.Run("spent p2sh at vout1 waits", func(t *testing.T) {
		// gettxout(txid, 0) reports 6 confirmations but the P2SH output
		// itself is spent/missing: judging the tx-level hit would accept a
		// deposit whose HTLC output is gone. Must wait, not accept.
		swapOutputs := func(tx *coins.Tx) {
			tx.Outputs[0], tx.Outputs[1] = tx.Outputs[1], tx.Outputs[0]
		}
		c, _ := newDeposit(&checkDepositServer{confsVout1: "null"}, swapOutputs)
		_, err := c.CheckDepositTransaction(fx.depositTxID, fx.p2shScriptHex, depositAmountXB, 3)
		if !errors.Is(err, ErrDepositNotReady) {
			t.Fatalf("err = %v, want ErrDepositNotReady", err)
		}
	})
}

// GetBalance fallback goldens — live-proven against non-Core backends
// (xlite-daemon 0.5.15 facades), which answer -32601 Method not found to
// "getbalance" while serving "listunspent". C++ never calls getbalance over
// RPC (it uses its in-process CWallet::GetBalance()), so the Go connector
// must degrade to a confirmed-UTXO sum there instead of failing every take.
// ---------------------------------------------------------------------------

// balanceServer serves getbalance/listunspent with per-test bodies and records
// call order, so tests can assert fallback precedence (legacy first,
// listunspent only on -32601, never on other errors).
type balanceServer struct {
	getbalance  string // result body; "" → RPC error -32601
	authFail    bool   // getbalance answers HTTP 401
	otherErr    bool   // getbalance answers RPC error -1 (non-fallback error)
	listunspent string // result body for listunspent
	listErr     bool   // listunspent answers RPC error -5
	calls       []string
}

func newBalanceServer(t *testing.T, cfg *balanceServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		cfg.calls = append(cfg.calls, req.Method)
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		res := func(raw string) { _ = enc.Encode(rpcResponse{Result: json.RawMessage(raw), ID: req.ID}) }
		rerr := func(code int, msg string) {
			_ = enc.Encode(rpcResponse{Error: &rpcError{Code: code, Message: msg}, ID: req.ID})
		}
		switch req.Method {
		case "getbalance":
			switch {
			case cfg.authFail:
				w.WriteHeader(http.StatusUnauthorized)
			case cfg.otherErr:
				rerr(-1, "some other failure")
			case cfg.getbalance != "":
				res(cfg.getbalance)
			default:
				rerr(-32601, "Method not found.")
			}
		case "listunspent":
			if cfg.listErr {
				rerr(-5, "listunspent failed")
			} else {
				res(cfg.listunspent)
			}
		case "sendrawtransaction":
			res(`"txid1234567890"`)
		default:
			res(`null`)
		}
	})
	return httptest.NewServer(mux)
}

// balanceFixture mixes every C++ listUnspent filter branch: 1.5 confirmed
// (kept), 2.0 zero-conf (dropped), 0.4 unspendable (dropped), 0.0 amount
// (dropped), 0.5 with absent confirmations (kept, C++ confs==-1 rule).
const balanceFixture = `[
		{"txid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","vout":0,"address":"addr1","amount":1.5,"scriptPubKey":"76a914deadbeef88ac","confirmations":6,"spendable":true},
		{"txid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","vout":1,"address":"addr2","amount":2.0,"scriptPubKey":"76a914deadbeef88ac","confirmations":0,"spendable":true},
		{"txid":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","vout":2,"address":"addr3","amount":0.4,"scriptPubKey":"76a914deadbeef88ac","confirmations":9,"spendable":false},
		{"txid":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","vout":3,"address":"addr4","amount":0.0,"scriptPubKey":"76a914deadbeef88ac","confirmations":9,"spendable":true},
		{"txid":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","vout":4,"address":"addr5","amount":0.5,"scriptPubKey":"76a914deadbeef88ac"}
	]`

func balanceConn(t *testing.T, cfg *balanceServer) (*RPCConnector, *httptest.Server) {
	t.Helper()
	srv := newBalanceServer(t, cfg)
	return NewRPCConnector(Chain{Ticker: "BLOCK", Endpoint: srv.URL, User: "u", Pass: "p", Decimals: 8}), srv
}

func TestGetBalanceLegacyPrimary(t *testing.T) {
	cfg := &balanceServer{getbalance: `2.5`, listunspent: balanceFixture}
	c, srv := balanceConn(t, cfg)
	defer srv.Close()
	bal, err := c.GetBalance()
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if bal != 250000000 {
		t.Fatalf("bal = %d, want 250000000", bal)
	}
	// Precedence lock: the legacy path must not touch listunspent, so a
	// backend that serves getbalance sees byte-identical behavior to before.
	for _, m := range cfg.calls {
		if m == "listunspent" {
			t.Fatalf("legacy getbalance path called listunspent: %v", cfg.calls)
		}
	}
}

func TestGetBalanceFallbackMethodNotFound(t *testing.T) {
	cfg := &balanceServer{listunspent: balanceFixture}
	c, srv := balanceConn(t, cfg)
	defer srv.Close()
	bal, err := c.GetBalance()
	if err != nil {
		t.Fatalf("GetBalance fallback: %v", err)
	}
	// Kept: 1.5 (confirmed) + 0.5 (absent confirmations) = 2.0 BTC.
	if bal != 200000000 {
		t.Fatalf("bal = %d, want 200000000", bal)
	}
	if len(cfg.calls) != 2 || cfg.calls[0] != "getbalance" || cfg.calls[1] != "listunspent" {
		t.Fatalf("call order = %v, want [getbalance listunspent]", cfg.calls)
	}
}

func TestGetBalanceFallbackPropagatesListUnspentError(t *testing.T) {
	cfg := &balanceServer{listErr: true}
	c, srv := balanceConn(t, cfg)
	defer srv.Close()
	if _, err := c.GetBalance(); err == nil {
		t.Fatal("GetBalance with failing listunspent must error, not report zero")
	}
}

func TestGetBalanceNoFallbackOnOtherError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		authFail bool
		otherErr bool
	}{
		{"auth", true, false},
		{"rpc-other", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &balanceServer{authFail: tc.authFail, otherErr: tc.otherErr, listunspent: balanceFixture}
			c, srv := balanceConn(t, cfg)
			defer srv.Close()
			if _, err := c.GetBalance(); err == nil {
				t.Fatal("GetBalance must surface non-32601 errors, not fall back")
			}
			// Anti-masking: an auth/credential failure must never be hidden
			// behind a listunspent call (which would report INSUFFICIENT_FUNDS).
			for _, m := range cfg.calls {
				if m == "listunspent" {
					t.Fatalf("non-32601 error triggered listunspent: %v", cfg.calls)
				}
			}
		})
	}
}

func TestSendRawTransactionSingleParam(t *testing.T) {
	var gotParams []interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		gotParams = req.Params
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"result":"txid1234567890","error":null,"id":%q}`, req.ID)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewRPCConnector(Chain{Ticker: "BLOCK", Endpoint: srv.URL, User: "u", Pass: "p", Decimals: 8})
	txid, err := c.SendRawTransaction("abcd")
	if err != nil {
		t.Fatalf("SendRawTransaction: %v", err)
	}
	if txid != "txid1234567890" {
		t.Fatalf("txid = %q", txid)
	}
	// Strict wallets (facade usage gate: exactly 1 param) reject the Core
	// 2-arg form; Core accepts 1 arg with identical semantics
	// (allowhighfees defaults false), so a single hex param works on both.
	if len(gotParams) != 1 || gotParams[0] != "abcd" {
		t.Fatalf("sendrawtransaction params = %v, want [abcd]", gotParams)
	}
}

// TestGetRawTransactionVerbose proves the verbose decoder serves the
// deposit-existence fallback: confirmations plus per-output native value and
// script hex, converted through the coin's decimals (facade wire shape:
// whole-coin float value, hex scriptPubKey).
func TestGetRawTransactionVerbose(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Method != "getrawtransaction" {
			t.Errorf("method = %q, want getrawtransaction", req.Method)
		}
		// Verbose form is [txid, 1] (C++ show-tx shape); anything else is
		// a usage error so a wrong-arity call cannot silently pass.
		if len(req.Params) != 2 {
			t.Errorf("params = %v, want [txid 1]", req.Params)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"result":{"txid":"%s","confirmations":15,"vout":[{"value":0.01010002,"n":0,"scriptPubKey":{"hex":"a9146a1c36bb5ad92adb9e65c5968652f364326581ec87"}},{"value":0.16943998,"n":1,"scriptPubKey":{"hex":"76a914abea45e4519baa43b1b2683558c8fca2c978641b88ac"}}]},"error":null,"id":%q}`,
			"396ddcdaf9bcd6beeedb0951b982bb1af8f935e3d906a39a1f1fc7365487636d", req.ID)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewRPCConnector(Chain{Ticker: "BLOCK", Endpoint: srv.URL, User: "u", Pass: "p", Decimals: 8})
	vtx, err := c.GetRawTransactionVerbose("396ddcdaf9bcd6beeedb0951b982bb1af8f935e3d906a39a1f1fc7365487636d")
	if err != nil {
		t.Fatalf("GetRawTransactionVerbose: %v", err)
	}
	if vtx.Confirmations != 15 {
		t.Fatalf("confirmations = %d, want 15", vtx.Confirmations)
	}
	out, ok := vtx.Outputs[0]
	if !ok {
		t.Fatalf("vout 0 missing: %+v", vtx.Outputs)
	}
	if out.Value != 1010002 {
		t.Fatalf("vout0 value = %d, want 1010002", out.Value)
	}
	if out.ScriptHex != "a9146a1c36bb5ad92adb9e65c5968652f364326581ec87" {
		t.Fatalf("vout0 script = %q", out.ScriptHex)
	}
	if _, ok := vtx.Outputs[1]; !ok {
		t.Fatalf("vout 1 missing: %+v", vtx.Outputs)
	}
}
