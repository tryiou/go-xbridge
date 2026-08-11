package wallet

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	xlog "go-xbridge/log"

	"go-xbridge/coins"
)

// RPCClient is a minimal Bitcoin-Core-style JSON-RPC client (HTTP + basic
// auth), used to talk to the connected SPV wallet / node. Bitcoin Core uses
// {"jsonrpc":"1.0","id":...,"method":...,"params":[...]} requests and
// {"result":...,"error":null|{...},"id":...} responses. The jsonrpc version and
// content-type are configurable to honor each wallet's xbridge.conf values.
// When omitJSONVersion is true the "jsonrpc" field is dropped entirely (C++
// XBridge and XLite-style wallets reject requests that carry it).
type RPCClient struct {
	url             string
	user            string
	pass            string
	jsonVersion     string
	omitJSONVersion bool
	contentType     string
	http            *http.Client
	nextID          atomic.Int64
	ticker          string
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      string        `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	ID     string          `json:"id"`
}

// defaultRPCTimeout bounds each JSON-RPC call when a chain sets no explicit
// timeout, so a hung wallet/node cannot wedge the caller indefinitely.
const defaultRPCTimeout = 30 * time.Second

// NewRPCClient builds a client for the given endpoint + basic-auth credentials.
// jsonVersion and contentType honor the wallet's xbridge.conf and are used
// verbatim (empty stays empty, matching C++ CallRPC/XBridgeJSONRPCRequestObj):
// an empty jsonVersion omits the "jsonrpc" request field, and an empty
// contentType leaves the Content-Type header unset. When omitJSONVersion is
// true the "jsonrpc" field is omitted regardless (XLite-style wallets reject
// it). A zero timeout applies defaultRPCTimeout. ticker (may be empty) labels
// transport logs/errors with the coin this client serves.
func NewRPCClient(url, user, pass, jsonVersion, contentType string, omitJSONVersion bool, timeout time.Duration, ticker string) *RPCClient {
	if timeout <= 0 {
		timeout = defaultRPCTimeout
	}
	return &RPCClient{url: url, user: user, pass: pass, jsonVersion: jsonVersion, omitJSONVersion: omitJSONVersion, contentType: contentType, http: &http.Client{Timeout: timeout}, ticker: ticker}
}

// Call invokes method with params and unmarshals the result into out.
func (c *RPCClient) Call(method string, params []interface{}, out interface{}) error {
	id := fmt.Sprintf("xbg-%d", c.nextID.Add(1)-1)
	xlog.Debug("rpc call", "coin", c.ticker, "method", method, "url", c.url)
	// Wallet RPCs (XLite, Bitcoin Core) require "params" to be an array; a nil
	// slice marshals to JSON null, which some wallets reject with an empty body
	// (e.g. XLite returns HTTP 400 + ""), surfacing as a decode error. Default
	// to an empty array so every call carries "params":[].
	if params == nil {
		params = []interface{}{}
	}
	// When omitJSONVersion is set, drop the "jsonrpc" field (XLite-style
	// wallets reject it); otherwise use the configured version verbatim. An
	// empty version is omitted via the rpcRequest omitempty tag, matching C++
	// XBridgeJSONRPCRequestObj (jsonrpc pushed only when non-empty).
	ver := c.jsonVersion
	if c.omitJSONVersion {
		ver = ""
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: ver, ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	// Only set Content-Type when configured; an empty contentType leaves the
	// header unset, matching C++ CallRPC (Content-Type added only when
	// non-empty).
	if c.contentType != "" {
		req.Header.Set("Content-Type", c.contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		xlog.Error("rpc transport failed", "coin", c.ticker, "method", method, "url", c.url, "err", err)
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		xlog.Error("rpc read failed", "coin", c.ticker, "method", method, "err", err)
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("wallet: rpc unauthorized (check RPC user/pass)")
	}
	var r rpcResponse
	if err := json.Unmarshal(data, &r); err != nil {
		xlog.Error("rpc decode failed", "coin", c.ticker, "method", method, "err", err, "body", string(data))
		return fmt.Errorf("wallet: rpc decode: %w (body %q)", err, string(data))
	}
	if r.Error != nil {
		xlog.Error("rpc error", "coin", c.ticker, "method", method, "code", r.Error.Code, "msg", r.Error.Message)
		return fmt.Errorf("wallet: rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	if out != nil && len(r.Result) > 0 && string(r.Result) != "null" {
		if err := json.Unmarshal(r.Result, out); err != nil {
			xlog.Error("rpc result decode failed", "coin", c.ticker, "method", method, "err", err, "body", string(r.Result))
			return fmt.Errorf("wallet: rpc result decode: %w (body %q)", err, string(r.Result))
		}
	}
	return nil
}

// RPCConnector drives a single coin wallet over JSON-RPC.
type RPCConnector struct {
	chain    Chain
	cli      *RPCClient
	relayFee relayFeeCache
}

// NewRPCConnector builds a Connector for the given chain endpoint.
func NewRPCConnector(chain Chain) *RPCConnector {
	return &RPCConnector{chain: chain, cli: NewRPCClient(chain.Endpoint, chain.User, chain.Pass, chain.JSONVersion, chain.ContentType, chain.OmitJSONVersion, chain.Timeout, chain.Ticker)}
}

// wrapErr annotates an RPC error with this connector's coin ticker so callers
// (and logs) can identify which coin's wallet failed. It preserves the inner
// error for errors.Is/errors.As.
func (c *RPCConnector) wrapErr(method string, err error) error {
	if err == nil {
		return nil
	}
	if c.chain.Ticker == "" {
		return fmt.Errorf("%s: %w", method, err)
	}
	return fmt.Errorf("coin %s %s: %w", c.chain.Ticker, method, err)
}

func (c *RPCConnector) Ticker() string { return c.chain.Ticker }

// Endpoint returns the configured wallet RPC endpoint (e.g. http://host:port).
func (c *RPCConnector) Endpoint() string { return c.chain.Endpoint }

// GetBalance returns the wallet-wide available balance in native base units via
// the getbalance RPC (C++ CWallet::GetBalance()). The wallet returns a
// whole-coin float; it is converted to base units through the coin's decimals.
func (c *RPCConnector) GetBalance() (uint64, error) {
	var v float64
	if err := c.cli.Call("getbalance", nil, &v); err != nil {
		return 0, c.wrapErr("getbalance", err)
	}
	bal, err := amountFloatToBase(c.chain.Decimals, v)
	if err != nil {
		return 0, c.wrapErr("getbalance", err)
	}
	return bal, nil
}

// GetNewAddress returns a fresh receive address. Mirrors C++
// xbridgewalletconnectorbtc.cpp rpc::getNewAddress, which calls "getnewaddress"
// with an EMPTY params array (no label, no address-type argument) so the wallet
// returns its default address type. Forcing a specific address type (e.g.
// "bech32") diverges from C++ and is rejected by strict wallets such as DASH
// (which answers code=-1 "Usage: getnewaddress").
func (c *RPCConnector) GetNewAddress() (string, error) {
	var addr string
	if err := c.cli.Call("getnewaddress", nil, &addr); err != nil {
		return "", c.wrapErr("getnewaddress", err)
	}
	return addr, nil
}

type rpcUtxo struct {
	TxID          string  `json:"txid"`
	Vout          uint32  `json:"vout"`
	Address       string  `json:"address"`
	Amount        float64 `json:"amount"`
	ScriptPubKey  string  `json:"scriptPubKey"`
	Confirmations *int    `json:"confirmations"`
	Spendable     *bool   `json:"spendable"`
}

// ListUnspent returns spendable UTXOs with at least minConf confirmations.
//
// C++ XBridge calls listunspent with NO parameters
// (xbridgewalletconnectorbtc.cpp:509-512), relying on the wallet default
// (minconf=1, maxconf=9999999), and then filters the result: it skips entries
// with spendable==false, requires amount>0, and requires confs==-1 (field
// absent) or confs>0 (xbridgewalletconnectorbtc.cpp:536-577). We mirror that
// exactly on the wire (empty params) and apply the same filter client-side.
// minConf is a Go-side caller contract (C++ has no per-call minconf here); a
// UTXO with a known confirmation count below minConf is dropped.
func (c *RPCConnector) ListUnspent(minConf int) ([]Utxo, error) {
	var raw []rpcUtxo
	// C++ parity: empty params (wallet default minconf/maxconf).
	if err := c.cli.Call("listunspent", []interface{}{}, &raw); err != nil {
		return nil, c.wrapErr("listunspent", err)
	}
	out := make([]Utxo, 0, len(raw))
	for _, u := range raw {
		// Skip explicitly non-spendable outputs (C++ spendable==false guard).
		if u.Spendable != nil && !*u.Spendable {
			continue
		}
		// Require a positive amount (C++ amount>0 guard).
		if u.Amount <= 0 {
			continue
		}
		// C++ confs guard: keep when the field is absent (confs==-1) or >0;
		// drop when present and <=0 (unconfirmed/conflicted).
		if u.Confirmations != nil && *u.Confirmations <= 0 {
			continue
		}
		confs := 0
		if u.Confirmations != nil {
			confs = *u.Confirmations
		}
		// Go-side minConf contract: drop known-confs below the caller's floor.
		if u.Confirmations != nil && confs < minConf {
			continue
		}
		amt, err := amountFloatToBase(c.chain.Decimals, u.Amount)
		if err != nil {
			return nil, fmt.Errorf("wallet: utxo %s:%d amount: %w", u.TxID, u.Vout, err)
		}
		out = append(out, Utxo{
			TxID:          u.TxID,
			Vout:          u.Vout,
			Address:       u.Address,
			Amount:        amt,
			Value:         u.Amount,
			ScriptPubKey:  u.ScriptPubKey,
			Confirmations: confs,
		})
	}
	return out, nil
}

type rpcPrevTx struct {
	TxID         string  `json:"txid"`
	Vout         uint32  `json:"vout"`
	ScriptPubKey string  `json:"scriptPubKey"`
	Amount       float64 `json:"amount"`
}

type rpcSignResult struct {
	Hex      string `json:"hex"`
	Complete bool   `json:"complete"`
}

// SignRawTransaction signs txHex with the wallet's keys. It mirrors C++
// XBridge (xbridgewalletconnectorbtc.cpp:1091-1100): it calls the legacy
// "signrawtransaction" first and, if that errors (newer wallets removed it),
// falls back to "signrawtransactionwithwallet". prevTxs carry each input's
// previous output script/amount (Bitcoin Core needs the amount to derive the
// sighash).
func (c *RPCConnector) SignRawTransaction(txHex string, prevTxs []PrevTx) (string, bool, error) {
	prev := make([]rpcPrevTx, 0, len(prevTxs))
	for _, p := range prevTxs {
		amt, err := amountBaseToFloat(c.chain.Decimals, p.Amount)
		if err != nil {
			return "", false, err
		}
		prev = append(prev, rpcPrevTx{
			TxID:         p.TxID,
			Vout:         p.Vout,
			ScriptPubKey: p.ScriptPubKey,
			Amount:       amt,
		})
	}
	args := []interface{}{txHex, prev, "ALL"}
	var res rpcSignResult
	// Legacy primary (old wallets); fall back to the modern RPC on error.
	if err := c.cli.Call("signrawtransaction", args, &res); err != nil {
		if ferr := c.cli.Call("signrawtransactionwithwallet", args, &res); ferr != nil {
			return "", false, c.wrapErr("signrawtransactionwithwallet", ferr)
		}
	}
	return res.Hex, res.Complete, nil
}

// SendRawTransaction broadcasts txHex, returning the network txid.
func (c *RPCConnector) SendRawTransaction(txHex string) (string, error) {
	var txid string
	if err := c.cli.Call("sendrawtransaction", []interface{}{txHex, false}, &txid); err != nil {
		return "", c.wrapErr("sendrawtransaction", err)
	}
	return txid, nil
}

// relayFeeCache memoizes the live relay fee from getinfo so the wallet is not
// probed on every dust calculation (C++ caches relayFee in init()).
type relayFeeCache struct {
	mu   sync.Mutex
	once bool
	val  float64
	err  error
}

// GetRelayFee returns the per-coin relay fee (BTC per kB) gathered from the
// wallet's getinfo RPC (C++ xbridgewalletconnectorbtc.cpp:74-76), which C++
// uses to compute dust (dustAmount = relayFee>0 ? 0.546*relayFee*COIN : 5460,
// xbridgewalletconnectorbtc.cpp:1526). It is fetched once and cached.
func (c *RPCConnector) GetRelayFee() (float64, error) {
	c.relayFee.mu.Lock()
	defer c.relayFee.mu.Unlock()
	if c.relayFee.once {
		return c.relayFee.val, c.relayFee.err
	}
	var res struct {
		RelayFee float64 `json:"relayfee"`
	}
	// Go order preference (deliberate deviation from C++'s
	// getblockchaininfo/getnetworkinfo-then-getinfo,
	// xbridgewalletconnectorbtc.cpp:1586-1593): XLite reliably serves getinfo
	// with relayfee, so try getinfo first, then getnetworkinfo as a fallback.
	var err error
	var getInfoErr error
	if e := c.cli.Call("getinfo", nil, &res); e != nil {
		getInfoErr = e
	}
	if res.RelayFee <= 0 {
		if e2 := c.cli.Call("getnetworkinfo", nil, &res); e2 != nil {
			if getInfoErr != nil {
				err = c.wrapErr("getinfo/relayfee", getInfoErr)
			} else {
				err = c.wrapErr("getnetworkinfo/relayfee", e2)
			}
		}
	}
	if err == nil {
		c.relayFee.once = true
		c.relayFee.val = res.RelayFee
		c.relayFee.err = err
	}
	return res.RelayFee, err
}

// GetBlockCount returns the best block height of the coin's chain via the
// wallet's getblockcount RPC.
func (c *RPCConnector) GetBlockCount() (int64, error) {
	var h int64
	if err := c.cli.Call("getblockcount", nil, &h); err != nil {
		return 0, c.wrapErr("getblockcount", err)
	}
	return h, nil
}

// GetBlockHash returns the block hash at height as a 32-byte internal
// (little-endian) hash, matching the XBridge wire order. Bitcoin Core's
// getblockhash returns the hash in display (big-endian) order, so it is
// reversed — the same convention api's reverseTxidHex uses for txids.
func (c *RPCConnector) GetBlockHash(height int64) ([32]byte, error) {
	var hexStr string
	if err := c.cli.Call("getblockhash", []interface{}{height}, &hexStr); err != nil {
		return [32]byte{}, c.wrapErr("getblockhash", err)
	}
	return revHashHex(hexStr)
}

// revHashHex converts a display-order block-hash hex into the 32-byte internal
// (little-endian) form XBridge carries on the wire.
func revHashHex(s string) ([32]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return [32]byte{}, fmt.Errorf("wallet: bad block hash %q", s)
	}
	var out [32]byte
	for i := 0; i < 32; i++ {
		out[i] = b[31-i]
	}
	return out, nil
}

// GetRawTransaction returns the full serialized (hex) transaction for txid via
// getrawtransaction (verbosity 0). The taker reads the maker's payTx to recover
// the HTLC secret preimage.
func (c *RPCConnector) GetRawTransaction(txid string) (string, error) {
	var hexStr string
	if err := c.cli.Call("getrawtransaction", []interface{}{txid, 0}, &hexStr); err != nil {
		return "", c.wrapErr("getrawtransaction", err)
	}
	return hexStr, nil
}

// SignMessage produces a BIP137 ownership proof. Bitcoin Core's signmessage
// returns the compact signature base64-encoded; XBridge carries it raw (65
// bytes: 1 recovery byte + 64), so we decode it. The address must be one this
// wallet owns (the UTXO's address).
func (c *RPCConnector) SignMessage(address, message string) ([]byte, error) {
	var b64 string
	if err := c.cli.Call("signmessage", []interface{}{address, message}, &b64); err != nil {
		return nil, c.wrapErr("signmessage", err)
	}
	return base64.StdEncoding.DecodeString(b64)
}

// VerifyMessage checks a BIP137 proof via Bitcoin Core's verifymessage, which
// expects the signature base64-encoded.
func (c *RPCConnector) VerifyMessage(address string, sig []byte, message string) (bool, error) {
	var ok bool
	b64 := base64.StdEncoding.EncodeToString(sig)
	if err := c.cli.Call("verifymessage", []interface{}{address, b64, message}, &ok); err != nil {
		return false, c.wrapErr("verifymessage", err)
	}
	return ok, nil
}

// amountFloatToBase converts a wallet float amount (coin units) to base units
// using the coin's decimals, via string formatting to avoid float drift.
func amountFloatToBase(decimals int, f float64) (uint64, error) {
	s := strconv.FormatFloat(f, 'f', decimals, 64)
	return coins.ParseAmount(coins.Coin{Decimals: decimals}, s)
}

// amountBaseToFloat converts base units to a wallet float amount (coin units).
func amountBaseToFloat(decimals int, v uint64) (float64, error) {
	s := coins.FormatAmount(coins.Coin{Decimals: decimals}, v)
	return strconv.ParseFloat(s, 64)
}
