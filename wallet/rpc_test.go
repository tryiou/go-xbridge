package wallet

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		res := func(raw string) { enc.Encode(rpcResponse{Result: json.RawMessage(raw), ID: req.ID}) }
		switch req.Method {
		case "getnewaddress":
			res(`"bc1qw508d6qezfhxq0t9wy3j9tg9z4r0r8e0j0q0w"`)
		case "listunspent":
			res(`[{"txid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","vout":0,"address":"bc1qw508d6qezfhxq0t9wy3j9tg9z4r0r8e0j0q0w","amount":1.5,"scriptPubKey":"76a914deadbeef88ac","confirmations":6}]`)
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

func TestRPCConnectorUnauthorized(t *testing.T) {
	srv := mockRPC(t)
	defer srv.Close()
	c := NewRPCConnector(Chain{Ticker: "BTC", Endpoint: srv.URL, User: "bad", Pass: "bad", Decimals: 8})
	if _, err := c.GetNewAddress(); err == nil {
		t.Fatal("expected auth error")
	}
}

// slowRPC returns a test server whose every handler blocks longer than the
// client timeout, to exercise the timeout path.
func slowRPC(delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		enc := json.NewEncoder(w)
		enc.Encode(rpcResponse{Result: json.RawMessage(`"ok"`), ID: "1"})
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
		json.NewEncoder(w).Encode(rpcResponse{Result: json.RawMessage(`0`), ID: "x"})
	}))
	return srv, &got
}

// TestRPCClientJSONRPCField asserts the "jsonrpc" field is present by default
// (Bitcoin Core / blocknetd expect {"jsonrpc":"1.0",...}) and omitted only when
// omitJSONVersion is set (XLite-style wallets reject the field).
func TestRPCClientJSONRPCField(t *testing.T) {
	cases := []struct {
		name      string
		version   string
		omit      bool
		wantField bool
	}{
		{"default keeps 1.0", "", false, true},
		{"explicit 1.0 keeps field", "1.0", false, true},
		{"omit drops field", "1.0", true, false},
		{"omit drops field even when empty version", "", true, false},
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
			if tc.wantField && !bytes.Contains(*got, []byte(`"jsonrpc":"1.0"`)) {
				t.Fatalf("expected jsonrpc:\"1.0\", got %s", *got)
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
