package wallet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockRPC returns a test server that answers the RPC methods the connector
// calls with canned responses, checking basic auth.
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
		case "estimatesmartfee":
			res(`{"feerate":0.0001,"errors":[]}`)
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

	t.Run("EstimateFee", func(t *testing.T) {
		// 0.0001 BTC/kB = 10000 sat/kB = 10 sat/vB.
		fee, err := c.EstimateFee(6)
		if err != nil {
			t.Fatalf("EstimateFee: %v", err)
		}
		if fee != 10 {
			t.Fatalf("fee = %d sat/vB, want 10", fee)
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
