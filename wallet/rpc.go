package wallet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"xbridge-go/coins"
)

// RPCClient is a minimal Bitcoin-Core-style JSON-RPC client (HTTP + basic
// auth), used to talk to the connected SPV wallet / node. Bitcoin Core uses
// {"jsonrpc":"1.0","id":...,"method":...,"params":[...]} requests and
// {"result":...,"error":null|{...},"id":...} responses. The jsonrpc version and
// content-type are configurable to honor each wallet's xbridge.conf values.
type RPCClient struct {
	url          string
	user         string
	pass         string
	jsonVersion  string
	contentType  string
	http         *http.Client
	nextID       int64
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
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

// NewRPCClient builds a client for the given endpoint + basic-auth credentials.
// jsonVersion and contentType honor the wallet's xbridge.conf (defaults applied
// by the caller if empty).
func NewRPCClient(url, user, pass, jsonVersion, contentType string) *RPCClient {
	if jsonVersion == "" {
		jsonVersion = "1.0"
	}
	if contentType == "" {
		contentType = "application/json"
	}
	return &RPCClient{url: url, user: user, pass: pass, jsonVersion: jsonVersion, contentType: contentType, http: &http.Client{}}
}

// Call invokes method with params and unmarshals the result into out.
func (c *RPCClient) Call(method string, params []interface{}, out interface{}) error {
	id := fmt.Sprintf("xbg-%d", c.nextID)
	c.nextID++
	body, err := json.Marshal(rpcRequest{JSONRPC: c.jsonVersion, ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", c.contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("wallet: rpc unauthorized (check RPC user/pass)")
	}
	var r rpcResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("wallet: rpc decode: %w (body %q)", err, string(data))
	}
	if r.Error != nil {
		return fmt.Errorf("wallet: rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	if out != nil && len(r.Result) > 0 && string(r.Result) != "null" {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("wallet: rpc result decode: %w (body %q)", err, string(r.Result))
		}
	}
	return nil
}

// RPCConnector drives a single coin wallet over JSON-RPC.
type RPCConnector struct {
	chain Chain
	cli   *RPCClient
}

// NewRPCConnector builds a Connector for the given chain endpoint.
func NewRPCConnector(chain Chain) *RPCConnector {
	return &RPCConnector{chain: chain, cli: NewRPCClient(chain.Endpoint, chain.User, chain.Pass, chain.JSONVersion, chain.ContentType)}
}

func (c *RPCConnector) Ticker() string { return c.chain.Ticker }

// GetNewAddress returns a fresh receive address (native segwit when the chain
// supports it, otherwise the wallet default P2PKH).
func (c *RPCConnector) GetNewAddress() (string, error) {
	addrType := ""
	if c.chain.SegWit {
		addrType = "bech32"
	}
	var addr string
	if err := c.cli.Call("getnewaddress", []interface{}{"", addrType}, &addr); err != nil {
		return "", err
	}
	return addr, nil
}

type rpcUtxo struct {
	TxID          string  `json:"txid"`
	Vout          uint32  `json:"vout"`
	Address       string  `json:"address"`
	Amount        float64 `json:"amount"`
	ScriptPubKey  string  `json:"scriptPubKey"`
	Confirmations int     `json:"confirmations"`
}

// ListUnspent returns spendable UTXOs with at least minConf confirmations,
// converting the wallet's float amounts into base units via the coin decimals.
func (c *RPCConnector) ListUnspent(minConf int) ([]Utxo, error) {
	var raw []rpcUtxo
	// listunspent minconf maxconf addresses include_unsafe query_options
	if err := c.cli.Call("listunspent", []interface{}{minConf, 9999999, nil, true}, &raw); err != nil {
		return nil, err
	}
	out := make([]Utxo, 0, len(raw))
	for _, u := range raw {
		amt, err := amountFloatToBase(c.chain.Decimals, u.Amount)
		if err != nil {
			return nil, fmt.Errorf("wallet: utxo %s:%d amount: %w", u.TxID, u.Vout, err)
		}
		out = append(out, Utxo{
			TxID:          u.TxID,
			Vout:          u.Vout,
			Address:       u.Address,
			Amount:        amt,
			ScriptPubKey:  u.ScriptPubKey,
			Confirmations: u.Confirmations,
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

// SignRawTransaction signs txHex with the wallet's keys via
// signrawtransactionwithwallet. prevTxs carry each input's previous output
// script/amount (Bitcoin Core needs the amount to derive the sighash).
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
	var res rpcSignResult
	if err := c.cli.Call("signrawtransactionwithwallet", []interface{}{txHex, prev, "ALL"}, &res); err != nil {
		return "", false, err
	}
	return res.Hex, res.Complete, nil
}

// SendRawTransaction broadcasts txHex, returning the network txid.
func (c *RPCConnector) SendRawTransaction(txHex string) (string, error) {
	var txid string
	if err := c.cli.Call("sendrawtransaction", []interface{}{txHex, false}, &txid); err != nil {
		return "", err
	}
	return txid, nil
}

// EstimateFee returns the fee rate in sat/vB for confTarget confirmations,
// derived from estimatesmartfee's BTC/kvB result.
func (c *RPCConnector) EstimateFee(confTarget int) (uint64, error) {
	var res struct {
		Feerate float64  `json:"feerate"` // BTC per kB
		Errors  []string `json:"errors"`
	}
	if err := c.cli.Call("estimatesmartfee", []interface{}{confTarget, "ECONOMICAL"}, &res); err != nil {
		return 0, err
	}
	if res.Feerate <= 0 {
		return 0, fmt.Errorf("wallet: no fee estimate: %v", res.Errors)
	}
	// BTC/kvB → sat/vB: feerate * 1e8 (sat/BTC) / 1000 (vB/kB).
	satPerVByte := res.Feerate * 1e8 / 1000
	if satPerVByte < 1 {
		satPerVByte = 1
	}
	return uint64(satPerVByte), nil
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
