package wallet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	xlog "go-xbridge/log"

	"go-xbridge/coins"
)

// RPCClient is a Bitcoin-Core-style JSON-RPC client (HTTP + basic auth) for the connected wallet or node.
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

// RPCError is a wallet RPC error object (code + message), returned by Call so callers can branch on the code.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("wallet: rpc error %d: %s", e.Code, e.Message)
}

// RPCErrorCode unwraps the RPC status code from a Call error, nil-safe via errors.As so it survives wrapping.
func RPCErrorCode(err error) (int, bool) {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code, true
	}
	return 0, false
}

func isMethodNotFound(err error) bool {
	code, ok := RPCErrorCode(err)
	return ok && code == -32601
}

const defaultRPCTimeout = 30 * time.Second

// NewRPCClient builds a client for the given endpoint and credentials; a zero timeout applies defaultRPCTimeout.
func NewRPCClient(url, user, pass, jsonVersion, contentType string, omitJSONVersion bool, timeout time.Duration, ticker string) *RPCClient {
	if timeout <= 0 {
		timeout = defaultRPCTimeout
	}
	return &RPCClient{url: url, user: user, pass: pass, jsonVersion: jsonVersion, omitJSONVersion: omitJSONVersion, contentType: contentType, http: &http.Client{Timeout: timeout}, ticker: ticker}
}

// Call invokes method with params and unmarshals the result into out.
func (c *RPCClient) Call(method string, params []interface{}, out interface{}) error {
	return c.call(context.Background(), method, params, out)
}

// CallContext invokes method with params under ctx; cancelling ctx aborts the in-flight request.
func (c *RPCClient) CallContext(ctx context.Context, method string, params []interface{}, out interface{}) error {
	return c.call(ctx, method, params, out)
}

func (c *RPCClient) call(ctx context.Context, method string, params []interface{}, out interface{}) error {
	id := fmt.Sprintf("xbg-%d", c.nextID.Add(1)-1)
	xlog.Debug("rpc call", "coin", c.ticker, "method", method, "url", c.url)
	// A nil params slice marshals to JSON null, which wallet RPCs reject;
	// default to an empty array so every call carries "params":[].
	if params == nil {
		params = []interface{}{}
	}
	ver := c.jsonVersion
	if c.omitJSONVersion {
		ver = ""
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: ver, ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	if c.contentType != "" {
		req.Header.Set("Content-Type", c.contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		xlog.Error("rpc transport failed", "coin", c.ticker, "method", method, "url", c.url, "err", err)
		return err
	}
	defer func() { _ = resp.Body.Close() }()
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
		xlog.Error("rpc decode failed", "coin", c.ticker, "method", method, "err", err)
		return fmt.Errorf("wallet: rpc decode: %w", err)
	}
	if r.Error != nil {
		xlog.Error("rpc error", "coin", c.ticker, "method", method, "code", r.Error.Code, "msg", r.Error.Message)
		return &RPCError{Code: r.Error.Code, Message: r.Error.Message}
	}
	if out != nil && len(r.Result) > 0 && string(r.Result) != "null" {
		if err := json.Unmarshal(r.Result, out); err != nil {
			xlog.Error("rpc result decode failed", "coin", c.ticker, "method", method, "err", err)
			return fmt.Errorf("wallet: rpc result decode: %w", err)
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
	return &RPCConnector{chain: chain, cli: NewRPCClient(chain.Endpoint, chain.User, chain.Pass, chain.JSONVersion, chain.ContentType, chain.OmitJSONVersion, chain.Timeout, chain.Ticker)}
}

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

// Endpoint returns the configured wallet RPC endpoint.
func (c *RPCConnector) Endpoint() string { return c.chain.Endpoint }

// GetBalance returns the wallet-wide available balance in base units, summing confirmed UTXOs only when getbalance answers -32601.
func (c *RPCConnector) GetBalance() (uint64, error) {
	var v float64
	if err := c.cli.Call("getbalance", nil, &v); err != nil {
		if !isMethodNotFound(err) {
			return 0, c.wrapErr("getbalance", err)
		}
		out, lerr := c.ListUnspent(1)
		if lerr != nil {
			return 0, lerr
		}
		var total uint64
		for _, u := range out {
			total += u.Amount
		}
		return total, nil
	}
	bal, err := amountFloatToBase(c.chain.Decimals, v)
	if err != nil {
		return 0, c.wrapErr("getbalance", err)
	}
	return bal, nil
}

// GetNewAddress returns a fresh receive address with no label or address-type argument, mirroring rpc::getNewAddress.
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

// ListUnspent returns spendable UTXOs with at least minConf confirmations;
// the wire call sends an empty params array and filtering happens client-side.
func (c *RPCConnector) ListUnspent(minConf int) ([]Utxo, error) {
	return c.listUnspentFiltered(minConf, false)
}

// ListUnspentWithZeroConf implements wallet.Connector: the same wire call as ListUnspent, keeping unconfirmed outputs.
func (c *RPCConnector) ListUnspentWithZeroConf() ([]Utxo, error) {
	return c.listUnspentFiltered(0, true)
}

func (c *RPCConnector) listUnspentFiltered(minConf int, includeZeroConf bool) ([]Utxo, error) {
	var raw []rpcUtxo
	if err := c.cli.Call("listunspent", []interface{}{}, &raw); err != nil {
		return nil, c.wrapErr("listunspent", err)
	}
	out := make([]Utxo, 0, len(raw))
	for _, u := range raw {
		if u.Spendable != nil && !*u.Spendable {
			continue
		}
		if u.Amount <= 0 {
			continue
		}
		if u.Confirmations != nil && *u.Confirmations < 0 {
			continue
		}
		if !includeZeroConf && u.Confirmations != nil && *u.Confirmations <= 0 {
			continue
		}
		confs := 0
		if u.Confirmations != nil {
			confs = *u.Confirmations
		}
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

// SignRawTransaction signs txHex with the wallet's keys, reporting whether every input was signed.
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
	// Args are [rawtx, prevtxs, privkeys]: the third slot is the privkeys
	// array (the wallet owns the keys), not a sighash type.
	args := []interface{}{txHex, nil, nil}
	if len(prev) > 0 {
		args[1] = prev
	}
	var res rpcSignResult
	if err := c.cli.Call("signrawtransaction", args, &res); err != nil {
		if ferr := c.cli.Call("signrawtransactionwithwallet", args, &res); ferr != nil {
			return "", false, c.wrapErr("signrawtransactionwithwallet", ferr)
		}
	}
	return res.Hex, res.Complete, nil
}

// SendRawTransaction broadcasts txHex, returning the network txid; hex is the sole parameter, mirroring rpc::sendrawtransaction.
func (c *RPCConnector) SendRawTransaction(txHex string) (string, error) {
	var txid string
	if err := c.cli.Call("sendrawtransaction", []interface{}{txHex}, &txid); err != nil {
		return "", c.wrapErr("sendrawtransaction", err)
	}
	return txid, nil
}

// GetBlockCount returns the best block height of the coin's chain.
func (c *RPCConnector) GetBlockCount() (int64, error) {
	return c.GetBlockCountContext(context.Background())
}

// GetBlockCountContext is the context-cancellable GetBlockCount: cancelling ctx aborts the in-flight request.
func (c *RPCConnector) GetBlockCountContext(ctx context.Context) (int64, error) {
	var h int64
	if err := c.cli.CallContext(ctx, "getblockcount", nil, &h); err != nil {
		return 0, c.wrapErr("getblockcount", err)
	}
	return h, nil
}

// GetBlockHash returns the hash at height in internal little-endian byte order, the order the XBridge wire uses.
func (c *RPCConnector) GetBlockHash(height int64) ([32]byte, error) {
	var hexStr string
	if err := c.cli.Call("getblockhash", []interface{}{height}, &hexStr); err != nil {
		return [32]byte{}, c.wrapErr("getblockhash", err)
	}
	return revHashHex(hexStr)
}

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

func displayHashHex(h [32]byte) string {
	var be [32]byte
	for i := 0; i < 32; i++ {
		be[i] = h[31-i]
	}
	return hex.EncodeToString(be[:])
}

type rpcBlockTx struct {
	TxID string `json:"txid"`
	Vin  []struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	} `json:"vin"`
}

// GetBlockTxs returns a block's decoded transactions via getblock verbosity 2;
// a bare hash asks stock Core for an id list, which cannot serve vins.
func (c *RPCConnector) GetBlockTxs(blockHash [32]byte) ([]BlockTx, error) {
	disp := displayHashHex(blockHash)
	var raw json.RawMessage
	if err := c.cli.Call("getblock", []interface{}{disp, 2}, &raw); err != nil {
		// The bare [hash] probe covers backends that reject the verbosity
		// argument; its error is discarded so the original failure surfaces.
		if berr := c.cli.Call("getblock", []interface{}{disp}, &raw); berr != nil {
			return nil, c.wrapErr("getblock", err)
		}
	}
	var obj struct {
		Tx []json.RawMessage `json:"tx"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Tx == nil {
		return nil, c.wrapErr("getblock", fmt.Errorf("wallet: getblock not a block object"))
	}
	if len(obj.Tx) == 0 {
		// Every real block carries at least a coinbase, so an empty list means
		// the backend cannot serve blocks; fail closed rather than accept it.
		return nil, c.wrapErr("getblock", fmt.Errorf("wallet: getblock empty tx list"))
	}
	out := make([]BlockTx, 0, len(obj.Tx))
	for _, entry := range obj.Tx {
		var v2 rpcBlockTx
		// A missing txid or empty vin set is undecidable — a stripped shape could
		// hide the spender — so fail the page rather than advance past it.
		if err := json.Unmarshal(entry, &v2); err != nil || v2.TxID == "" || len(v2.Vin) == 0 {
			return nil, c.wrapErr("getblock", fmt.Errorf("wallet: getblock undecodable tx entry"))
		}
		tx := BlockTx{TxID: v2.TxID}
		for _, in := range v2.Vin {
			tx.Vin = append(tx.Vin, BlockVin{TxID: in.TxID, Vout: in.Vout})
		}
		out = append(out, tx)
	}
	return out, nil
}

// GetRawTransaction returns the full serialized (hex) transaction for txid.
func (c *RPCConnector) GetRawTransaction(txid string) (string, error) {
	var hexStr string
	if err := c.cli.Call("getrawtransaction", []interface{}{txid, 0}, &hexStr); err != nil {
		return "", c.wrapErr("getrawtransaction", err)
	}
	return hexStr, nil
}

const xbridgeCoinScale = 1_000_000

const seqFinal = 0xffffffff

const doubleEpsilon = 2.220446049250313e-16

func (c *RPCConnector) chainScale() float64 {
	s := 1.0
	for i := 0; i < c.chain.Decimals; i++ {
		s *= 10
	}
	if s <= 0 {
		return 1
	}
	return s
}

func (c *RPCConnector) minTxFeeWhole(nIn, nOut int) float64 {
	fee := uint64(192*nIn+34*nOut) * c.chain.FeePerByte
	if fee < c.chain.MinTxFee {
		fee = c.chain.MinTxFee
	}
	return float64(fee) / c.chainScale()
}

type rpcRawTxVerbose struct {
	Confirmations *int `json:"confirmations"`
	Vin           []struct {
		TxID     *string `json:"txid"`
		Vout     *uint32 `json:"vout"`
		Sequence *uint64 `json:"sequence"`
	} `json:"vin"`
	Vout []struct {
		Value        float64 `json:"value"`
		N            uint32  `json:"n"`
		ScriptPubKey struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	} `json:"vout"`
}

func (c *RPCConnector) getRawTransactionVerbose(txid string) (*rpcRawTxVerbose, error) {
	var out rpcRawTxVerbose
	if err := c.cli.Call("getrawtransaction", []interface{}{txid, 1}, &out); err != nil {
		return nil, c.wrapErr("getrawtransaction", err)
	}
	return &out, nil
}

// GetRawTransactionVerbose returns the decoded transaction with chain context: confirmations and each output's native value and script hex.
func (c *RPCConnector) GetRawTransactionVerbose(txid string) (VerboseTx, error) {
	raw, err := c.getRawTransactionVerbose(txid)
	if err != nil {
		return VerboseTx{}, err
	}
	vtx := VerboseTx{TxID: txid, Outputs: make(map[uint32]VerboseTxOut, len(raw.Vout))}
	if raw.Confirmations != nil {
		vtx.Confirmations = *raw.Confirmations
		vtx.HasConfirmations = true
	}
	for _, o := range raw.Vout {
		amt, aerr := amountFloatToBase(c.chain.Decimals, o.Value)
		if aerr != nil {
			return VerboseTx{}, c.wrapErr("getrawtransaction", aerr)
		}
		vtx.Outputs[o.N] = VerboseTxOut{Value: amt, ScriptHex: o.ScriptPubKey.Hex}
	}
	return vtx, nil
}

func (c *RPCConnector) getTxOutConfirmations(txid string, vout uint32) (confs int, ok bool) {
	var out struct {
		Confirmations *int `json:"confirmations"`
	}
	if err := c.cli.Call("gettxout", []interface{}{txid, vout}, &out); err != nil {
		return 0, false
	}
	if out.Confirmations == nil {
		return 0, false
	}
	return *out.Confirmations, true
}

// GetTxOut fetches an unspent output's chain data via gettxout; ok is false when the output is unknown, spent, or the RPC errors.
func (c *RPCConnector) GetTxOut(txid string, vout uint32) (Utxo, bool, error) {
	var out struct {
		Confirmations *int     `json:"confirmations"`
		Value         *float64 `json:"value"`
		ScriptPubKey  struct {
			Addresses []string `json:"addresses"`
		} `json:"scriptPubKey"`
	}
	if err := c.cli.Call("gettxout", []interface{}{txid, vout}, &out); err != nil {
		return Utxo{}, false, err
	}
	if out.Value == nil {
		return Utxo{}, false, nil
	}
	u := Utxo{TxID: txid, Vout: vout, Value: *out.Value}
	if out.Confirmations != nil {
		u.Confirmations = *out.Confirmations
	}
	if len(out.ScriptPubKey.Addresses) > 0 {
		u.Address = out.ScriptPubKey.Addresses[0]
	}
	return u, true, nil
}

// GetRawMempool returns the mempool's transaction ids via getrawmempool.
func (c *RPCConnector) GetRawMempool() ([]string, error) {
	var txids []string
	if err := c.cli.Call("getrawmempool", []interface{}{}, &txids); err != nil {
		return nil, err
	}
	return txids, nil
}

// CheckDepositTransaction judges a counterparty's HTLC deposit against the expected p2sh script and amount; see the Connector contract for the tri-state verdict.
func (c *RPCConnector) CheckDepositTransaction(depositTxID, expectedScriptHex string, expectedAmount uint64, requiredConfirmations int) (DepositCheck, error) {
	dc := DepositCheck{}

	rawHex, err := c.GetRawTransaction(depositTxID)
	if err != nil {
		xlog.Debug("checkDepositTransaction: no tx found ...waiting", "txid", depositTxID, "err", err)
		// This is the only not-ready return where the watched bytes were never
		// observed, so Seen is false; every other one fires after the fetch above.
		return dc, &NotReadyError{Seen: false, Err: fmt.Errorf("%w: %v", ErrDepositNotReady, err)}
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		xlog.Debug("checkDepositTransaction: bad hex, decode transaction failed", "txid", depositTxID, "err", err)
		return dc, nil
	}
	tx, err := coins.DeserializeWithTime(raw, c.chain.TxWithTimeField)
	if err != nil {
		xlog.Debug("checkDepositTransaction: bad counterparty deposit, decode transaction failed", "txid", depositTxID, "err", err)
		return dc, nil
	}

	// Confirmations come from gettxout: the locally decoded transaction carries no confirmations field.
	if requiredConfirmations > 0 {
		confs, ok := c.getTxOutConfirmations(depositTxID, 0)
		if !ok {
			xlog.Debug("checkDepositTransaction: confirmations data not found in gettxout, may be stuck", "txid", depositTxID)
			return dc, fmt.Errorf("%w: gettxout confirmations unknown", ErrDepositNotReady)
		}
		if confs < requiredConfirmations {
			xlog.Debug("checkDepositTransaction: ...waiting", "txid", depositTxID, "confs", confs, "required", requiredConfirmations)
			return dc, fmt.Errorf("%w: confirmations %d of %d", ErrDepositNotReady, confs, requiredConfirmations)
		}
	} else {
		// The verbosity-0 fetch can be served from wallet knowledge rather than the
		// chain, so the verbose view must also know the tx; unknown or conflicted waits.
		vtx, verr := c.getRawTransactionVerbose(depositTxID)
		if verr != nil {
			xlog.Debug("checkDepositTransaction: deposit not visible to chain ...waiting", "txid", depositTxID, "err", verr)
			return dc, fmt.Errorf("%w: chain visibility unknown", ErrDepositNotReady)
		}
		if vtx.Confirmations != nil && *vtx.Confirmations < 0 {
			xlog.Debug("checkDepositTransaction: deposit conflicted ...waiting", "txid", depositTxID)
			return dc, fmt.Errorf("%w: deposit conflicted", ErrDepositNotReady)
		}
	}

	if len(tx.Inputs) == 0 {
		xlog.Debug("checkDepositTransaction: no vins", "txid", depositTxID)
		return dc, nil
	}
	if len(tx.Outputs) == 0 {
		xlog.Debug("checkDepositTransaction: no vouts", "txid", depositTxID)
		return dc, nil
	}
	var totalVinAmount float64
	for i := range tx.Inputs {
		vin := &tx.Inputs[i]
		if vin.Sequence != seqFinal {
			xlog.Debug("checkDepositTransaction: bad sequence for input, expected SEQUENCE_FINAL", "txid", depositTxID, "sequence", vin.Sequence)
			return dc, nil
		}
		vinTxID := hex.EncodeToString(reverse32(vin.PrevOut.Hash[:]))
		vinTx, err := c.getRawTransactionVerbose(vinTxID)
		if err != nil {
			xlog.Debug("checkDepositTransaction: vin tx not found ...waiting", "txid", depositTxID, "vin", vinTxID, "err", err)
			return dc, fmt.Errorf("%w: vin tx %s: %v", ErrDepositNotReady, vinTxID, err)
		}
		vinAmount, found := 0.0, false
		for _, vout := range vinTx.Vout {
			if vout.N == vin.PrevOut.Index {
				vinAmount, found = vout.Value, true
				break
			}
		}
		if !found {
			xlog.Debug("checkDepositTransaction: bad prevout", "txid", depositTxID, "vin", vinTxID, "vout", vin.PrevOut.Index)
			return dc, nil
		}
		totalVinAmount += vinAmount
	}

	var totalVoutAmount, depositP2SHAmount float64
	var depositTxVout uint32
	for i := range tx.Outputs {
		out := &tx.Outputs[i]
		whole := float64(out.Value) / c.chainScale()
		totalVoutAmount += whole
		scriptHex := hex.EncodeToString(out.ScriptPubKey)
		if scriptHex != expectedScriptHex {
			continue
		}
		wholeAmount := float64(expectedAmount) / xbridgeCoinScale
		if wholeAmount <= whole+doubleEpsilon {
			depositP2SHAmount = whole
			dc.P2SHNative = out.Value
			depositTxVout = uint32(i)
		}
		// Vout scan stops at the first script match: a later p2sh vout
		// (even a larger one) is never considered; the C++ scan does the same.
		break
	}
	if depositP2SHAmount == 0 {
		xlog.Debug("checkDepositTransaction: no valid p2sh in deposit transaction", "txid", depositTxID)
		return dc, nil
	}

	// Re-gate the actual P2SH output: a gettxout hit on vout 0 says nothing about
	// an HTLC output at a nonzero index, and a spent or missing one must wait.
	if requiredConfirmations > 0 && depositTxVout != 0 {
		confs, ok := c.getTxOutConfirmations(depositTxID, depositTxVout)
		if !ok {
			xlog.Debug("checkDepositTransaction: p2sh output spent or missing in gettxout", "txid", depositTxID, "vout", depositTxVout)
			return dc, fmt.Errorf("%w: p2sh gettxout unknown", ErrDepositNotReady)
		}
		if confs < requiredConfirmations {
			xlog.Debug("checkDepositTransaction: p2sh ...waiting", "txid", depositTxID, "confs", confs, "required", requiredConfirmations)
			return dc, fmt.Errorf("%w: p2sh confirmations %d of %d", ErrDepositNotReady, confs, requiredConfirmations)
		}
	}

	counterpartyFees := totalVinAmount - totalVoutAmount
	fee1 := c.minTxFeeWhole(len(tx.Inputs), len(tx.Outputs))
	fee2 := c.minTxFeeWhole(1, 1)
	wholeAmount := float64(expectedAmount) / xbridgeCoinScale
	if counterpartyFees < 0 || counterpartyFees < fee1*0.95 {
		xlog.Debug("checkDepositTransaction: not enough inputs to cover p2sh deposit fees", "txid", depositTxID, "min", fee1*0.95, "fees", counterpartyFees)
		return dc, nil
	}
	if depositP2SHAmount < wholeAmount+fee2*0.95 {
		xlog.Debug("checkDepositTransaction: not enough inputs to cover p2sh redeem fees", "txid", depositTxID, "min", fee2*0.95, "amount", depositP2SHAmount)
		return dc, nil
	}
	if depositP2SHAmount > wholeAmount+fee2 {
		dc.Excess = uint64(math.Round((depositP2SHAmount - wholeAmount - fee2) * xbridgeCoinScale))
	}
	dc.P2SHAmount = uint64(math.Round(depositP2SHAmount * xbridgeCoinScale))
	dc.DepositVout = depositTxVout
	dc.IsGood = true
	return dc, nil
}

func reverse32(b []byte) []byte {
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		out[i] = b[31-i]
	}
	return out
}

// SignMessage produces a BIP137 ownership proof; the address must be one this wallet owns.
func (c *RPCConnector) SignMessage(address, message string) ([]byte, error) {
	var b64 string
	if err := c.cli.Call("signmessage", []interface{}{address, message}, &b64); err != nil {
		return nil, c.wrapErr("signmessage", err)
	}
	return base64.StdEncoding.DecodeString(b64)
}

// VerifyMessage checks a BIP137 proof via the wallet's verifymessage RPC.
func (c *RPCConnector) VerifyMessage(address string, sig []byte, message string) (bool, error) {
	var ok bool
	b64 := base64.StdEncoding.EncodeToString(sig)
	if err := c.cli.Call("verifymessage", []interface{}{address, b64, message}, &ok); err != nil {
		return false, c.wrapErr("verifymessage", err)
	}
	return ok, nil
}

func amountFloatToBase(decimals int, f float64) (uint64, error) {
	s := strconv.FormatFloat(f, 'f', decimals, 64)
	return coins.ParseAmount(coins.Coin{Decimals: decimals}, s)
}

func amountBaseToFloat(decimals int, v uint64) (float64, error) {
	s := coins.FormatAmount(coins.Coin{Decimals: decimals}, v)
	return strconv.ParseFloat(s, 64)
}
