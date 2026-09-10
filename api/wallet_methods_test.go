package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// stubErr is a trivial error type used by fake connectors.
type stubErr string

func (e stubErr) Error() string { return string(e) }

var errStub = stubErr("stub: not supported")

// stubConn is a fake wallet.Connector for unit-testing the wallet-backed dx*
// methods without a live wallet.
type stubConn struct {
	ticker string
	addr   string
	utxos  []wallet.Utxo
	// depositCheck / depositCheckErr canned the CheckDepositTransaction result
	// (default IsGood:true when neither is set).
	depositCheck    *wallet.DepositCheck
	depositCheckErr error
	// sendErr, when set, makes SendRawTransaction fail (dxSplit submit-failure
	// tests).
	sendErr error
	// listUnspentErr, when set, makes ListUnspent fail (dxGetUtxos
	// listunspent-failure case).
	listUnspentErr error
	// getNewAddrErr, when set, makes GetNewAddress fail (dxGetNewTokenAddress
	// empty-array case).
	getNewAddrErr error
	// verifyFail, when set, makes VerifyMessage reject every proof (forged
	// inbound-order proof tests).
	verifyFail bool
}

func (s *stubConn) Ticker() string { return s.ticker }
func (s *stubConn) GetBalance() (uint64, error) {
	return 100000000, nil
}
func (s *stubConn) GetNewAddress() (string, error) {
	if s.getNewAddrErr != nil {
		return "", s.getNewAddrErr
	}
	return s.addr, nil
}
func (s *stubConn) ListUnspent(minConf int) ([]wallet.Utxo, error) {
	if s.listUnspentErr != nil {
		return nil, s.listUnspentErr
	}
	return s.utxos, nil
}
func (s *stubConn) SignRawTransaction(txHex string, prevTxs []wallet.PrevTx) (string, bool, error) {
	return txHex, true, nil
}
func (s *stubConn) SendRawTransaction(txHex string) (string, error) {
	if s.sendErr != nil {
		return "", s.sendErr
	}
	return "txid123", nil
}
func (s *stubConn) GetRelayFee() (float64, error) {
	return 0.0001, nil
}
func (s *stubConn) GetBlockCount() (int64, error) { return 100, nil }
func (s *stubConn) GetBlockHash(height int64) ([32]byte, error) {
	var h [32]byte
	h[0] = 0xab
	return h, nil
}
func (s *stubConn) GetRawTransaction(txid string) (string, error) {
	return "", errStub
}
func (s *stubConn) CheckDepositTransaction(depositTxID, expectedScriptHex string, expectedAmount uint64, requiredConfirmations int) (wallet.DepositCheck, error) {
	if s.depositCheckErr != nil {
		return wallet.DepositCheck{}, s.depositCheckErr
	}
	if s.depositCheck != nil {
		return *s.depositCheck, nil
	}
	return wallet.DepositCheck{IsGood: true}, nil
}
func (s *stubConn) SignMessage(address, message string) ([]byte, error) {
	// 65-byte placeholder compact signature.
	return []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a,
		0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14,
		0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e,
		0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28,
		0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30, 0x31, 0x32,
		0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x3b, 0x3c,
		0x3d, 0x3e, 0x3f, 0x40, 0x41,
	}, nil
}
func (s *stubConn) VerifyMessage(address string, sig []byte, message string) (bool, error) {
	if s.verifyFail {
		return false, nil
	}
	return len(sig) == 65, nil
}

func (s *stubConn) GetTxOut(txid string, vout uint32) (wallet.Utxo, bool, error) {
	for _, u := range s.utxos {
		if u.TxID == txid && u.Vout == vout {
			return u, true, nil
		}
	}
	return wallet.Utxo{}, false, nil
}

// valid BTC P2PKH address (prefix 0x00), used as both destination and change.
const btcAddr = "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"

func newWalletTestCtx() *HandlerCtx {
	cfg := &Config{
		Confs: map[string]*config.CoinConf{
			"BTC": {
				Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5,
				Coin: 100000000, Confirmations: 2, FeePerByte: 2, DustAmount: 546, TxVersion: 1,
			},
		},
		Connectors: map[string]wallet.Connector{
			"BTC": &stubConn{
				ticker: "BTC",
				addr:   btcAddr,
				utxos: []wallet.Utxo{
					// 1 BTC exact. C++ dxGetTokenBalances sums native satoshis as a double
					// and renders printf("%.6f", 1.0) = "1.000000" (no +1/COIN; the value
					// is already a whole-coin double). formatBalanceNative reproduces this.
					{TxID: "0000000000000000000000000000000000000000000000000000000000000000", Vout: 0, Amount: 100000000, Value: 1.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
				},
			},
		},
		ExchangeWallets: []string{"BTC"},
		NetworkTokens:   []string{"BTC"},
	}
	if err := coins.InitFromConf(cfg.Confs); err != nil {
		panic(err)
	}
	// Write the conf the context was built from so dxLoadXBridgeConf can
	// hot-reload from it (mirrors the daemon, which sets Config.ConfPath).
	td, err := os.MkdirTemp("", "xbridge-conf-*")
	if err != nil {
		panic(err)
	}
	confPath := filepath.Join(td, "xbridge.conf")
	confBody := "[Main]\nExchangeWallets=BTC\n\n[BTC]\nTitle=Bitcoin\nCreateTxMethod=BTC\nAddressPrefix=0\nScriptPrefix=5\nCOIN=100000000\nTxVersion=1\nDustAmount=546\nMinTxFee=1000\nBlockTime=600\nFeePerByte=2\nConfirmations=2\nIp=127.0.0.1\nPort=8332\n"
	if err := os.WriteFile(confPath, []byte(confBody), 0o600); err != nil {
		panic(err)
	}
	cfg.ConfPath = confPath
	store := NewStore()
	node := &Node{config: cfg, store: store, signer: crypto.NewBtcSigner(), stop: make(chan struct{}), snReg: servicenode.NewRegistry()}
	return &HandlerCtx{Store: store, Node: node}
}

func TestDxGetNewTokenAddress(t *testing.T) {
	ctx := newWalletTestCtx()
	res, err := ctx.dxGetNewTokenAddress([]json.RawMessage{json.RawMessage(`"BTC"`)})
	if err != nil {
		t.Fatalf("dxGetNewTokenAddress: %v", err)
	}
	addrs, ok := res.([]string)
	if !ok || len(addrs) != 1 || addrs[0] != btcAddr {
		t.Fatalf("result = %v (%T)", res, res)
	}
}

func TestDxGetNewTokenAddressNoConnector(t *testing.T) {
	ctx := newWalletTestCtx()
	// C++ dxGetNewTokenAddress returns an empty array (not an error) when no
	// wallet is loaded for the requested coin.
	res, err := ctx.dxGetNewTokenAddress([]json.RawMessage{json.RawMessage(`"DOGE"`)})
	if err != nil {
		t.Fatalf("dxGetNewTokenAddress(no connector) should not error: %v", err)
	}
	if arr, ok := res.([]string); !ok || len(arr) != 0 {
		t.Fatalf("dxGetNewTokenAddress(no connector) = %v (%T), want []", res, res)
	}
}

// TestDxGetNewTokenAddressGetNewAddrError locks in the behavior: a GetNewAddress
// failure yields an empty array (C++ getNewTokenAddress() returns an empty
// string, rpcxbridge.cpp:186-190), never a business error.
func TestDxGetNewTokenAddressGetNewAddrError(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Node.config.Connectors["BTC"].(*stubConn).getNewAddrErr = stubErr("rpc down")
	res, err := ctx.dxGetNewTokenAddress([]json.RawMessage{json.RawMessage(`"BTC"`)})
	if err != nil {
		t.Fatalf("dxGetNewTokenAddress(GetNewAddress fail) should not error: %v", err)
	}
	if arr, ok := res.([]string); !ok || len(arr) != 0 {
		t.Fatalf("dxGetNewTokenAddress(GetNewAddress fail) = %v (%T), want []", res, res)
	}
}

func TestDxGetUtxos(t *testing.T) {
	ctx := newWalletTestCtx()
	res, err := ctx.dxGetUtxos([]json.RawMessage{json.RawMessage(`"BTC"`)})
	if err != nil {
		t.Fatalf("dxGetUtxos: %v", err)
	}
	arr, ok := res.([]map[string]interface{})
	if !ok || len(arr) != 1 {
		t.Fatalf("result = %v (%T)", res, res)
	}
	// C++ renders the amount fixed to the coin's decimal places
	// (xBridgeStringValueFromPrice(amount, conn->COIN), rpcxbridge.cpp:3483):
	// 1 BTC -> "1.00000000".
	if arr[0]["amount"] != "1.00000000" {
		t.Errorf("amount = %v, want 1.00000000", arr[0]["amount"])
	}
	if arr[0]["txid"] == "" || arr[0]["scriptPubKey"] == "" {
		t.Errorf("missing utxo fields: %v", arr[0])
	}
	// C++ dxGetUtxos always emits the orderid key (empty when not locked).
	if arr[0]["orderid"] != "" {
		t.Errorf("orderid = %v, want \"\"", arr[0]["orderid"])
	}
	// Too many params -> error (arity gate moved to checkArity, dispatch.go).
	if rerr := checkArity("dxGetUtxos", 3); rerr == nil || !rerr.envelope || rerr.Code != -1 {
		t.Errorf("checkArity(dxGetUtxos, 3) = %v, want envelope -1 (throw method)", rerr)
	}
}

// TestDxGetUtxosListUnspentError locks the C++ listunspent-failure contract
// (rpcxbridge.cpp:3476): 1004 BAD_REQUEST named after __FUNCTION__ with the
// fixed text "failed to get unspent transaction outputs".
func TestDxGetUtxosListUnspentError(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Node.cfg().Connectors["BTC"].(*stubConn).listUnspentErr = stubErr("wallet rpc down")
	_, err := ctx.dxGetUtxos([]json.RawMessage{json.RawMessage(`"BTC"`)})
	if err == nil {
		t.Fatal("dxGetUtxos should fail when ListUnspent fails")
	}
	if err.Code != errBadRequest {
		t.Errorf("code = %d, want 1004 (BAD_REQUEST)", err.Code)
	}
	if err.Name != "dxGetUtxos" {
		t.Errorf("name = %q, want dxGetUtxos (C++ __FUNCTION__)", err.Name)
	}
	if err.Error != "Bad Request failed to get unspent transaction outputs" {
		t.Errorf("text = %q, want C++ text", err.Error)
	}
}

func TestDxGetTokenBalances(t *testing.T) {
	ctx := newWalletTestCtx()
	res, err := ctx.dxGetTokenBalances(nil)
	if err != nil {
		t.Fatalf("dxGetTokenBalances: %v", err)
	}
	m, ok := res.(map[string]string)
	if !ok {
		t.Fatalf("result = %v (%T)", res, res)
	}
	// C++ renders per-coin balances in fixed-6 XBridge scale via
	// xBridgeStringValueFromPrice (printf("%.6f", wholeCoinDouble)), so 1 BTC
	// -> "1.000000". formatBalanceNative reproduces the C++ double path exactly.
	if m["BTC"] != "1.000000" {
		t.Errorf("BTC balance = %v, want 1.000000", m["BTC"])
	}
	// DOCUMENTED divergence: no synthesized "Wallet" key — the
	// thin client exposes the BLOCK connector balance under its own ticker and
	// does not duplicate it into a wallet tag.
	if _, hasWallet := m["Wallet"]; hasWallet {
		t.Errorf("result contains a synthesized 'Wallet' key (deliberately removed): %v", m)
	}
}

// TestDxGetTokenBalancesSum is the regression pin: Go sums per-UTXO
// native amounts as an EXACT integer (uint64) and renders formatBalanceNative,
// while C++ sums them as doubles and prints %.6f. The two agree to the 6th
// decimal for this vector (a double sum of 0.1+0.2+0.05 = 0.35000000000000003
// also renders "0.350000"), so the test pins the exact uint64 accumulation and
// 8-decimal scale rendering — catching a scale/coinconf or accumulation
// regression rather than double-vs-int discrimination.
func TestDxGetTokenBalancesSum(t *testing.T) {
	ctx := newWalletTestCtx()
	btc := ctx.Node.config.Connectors["BTC"].(*stubConn)
	btc.utxos = []wallet.Utxo{
		{TxID: "000000000000000000000000000000000000000000000000000000000000000a", Vout: 0, Amount: 10000000, Value: 0.1, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
		{TxID: "000000000000000000000000000000000000000000000000000000000000000b", Vout: 1, Amount: 20000000, Value: 0.2, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
		{TxID: "000000000000000000000000000000000000000000000000000000000000000c", Vout: 2, Amount: 5000000, Value: 0.05, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
	}
	res, err := ctx.dxGetTokenBalances(nil)
	if err != nil {
		t.Fatalf("dxGetTokenBalances: %v", err)
	}
	m, ok := res.(map[string]string)
	if !ok {
		t.Fatalf("result = %v (%T), want map[string]string", res, res)
	}
	// 0.1 + 0.2 + 0.05 BTC = 0.35 BTC exactly in base units.
	if m["BTC"] != "0.350000" {
		t.Errorf("BTC balance = %v, want 0.350000 (exact integer sum)", m["BTC"])
	}
}

func TestDxSplitAddress(t *testing.T) {
	ctx := newWalletTestCtx()
	res, err := ctx.dxSplitAddress([]json.RawMessage{
		json.RawMessage(`"BTC"`),
		json.RawMessage(`"0.5"`),
		json.RawMessage(`"` + btcAddr + `"`),
	})
	if err != nil {
		t.Fatalf("dxSplitAddress: %v", err)
	}
	m := mustJSONMap(t, res)
	// C++ derives txid from the signed tx (double-SHA256, byte-reversed); it is
	// always present, even before submission.
	txid, _ := m["txid"].(string)
	if len(txid) != 64 {
		t.Errorf("txid should be a 64-char hash, got %q", txid)
	}
	// show_rawtx defaults to false, so rawtx is empty.
	if m["rawtx"] != "" {
		t.Errorf("rawtx should be empty when show_rawtx=false, got %v", m["rawtx"])
	}
	if m["token"] != "BTC" || m["include_fees"] != true {
		t.Errorf("unexpected token/include_fees: %v / %v", m["token"], m["include_fees"])
	}
	if m["split_amount_requested"] != "0.500000" || m["split_total"] != "1.000000" {
		t.Errorf("unexpected amounts: requested=%v total=%v", m["split_amount_requested"], m["split_total"])
	}
}

func TestDxSplitAddressNoConnector(t *testing.T) {
	ctx := newWalletTestCtx()
	_, err := ctx.dxSplitAddress([]json.RawMessage{
		json.RawMessage(`"DOGE"`),
		json.RawMessage(`"0.5"`),
		json.RawMessage(`"` + btcAddr + `"`),
	})
	if err == nil {
		t.Fatal("expected no-session error for unconfigured coin")
	}
}

// TestDxSplitFeesPerUtxo locks in the fee math: split_amount_with_fees is the split
// size plus feesPerUtxo = minTxFee1(1,3) + minTxFee2(1,1), added only when
// include_fees. For BTC (FeePerByte=2): fee1 = (192+102)*2 = 588 -> 5 XB units,
// fee2 = (192+34)*2 = 452 -> 4 XB units, feesPerUtxo = 9.
func TestDxSplitFeesPerUtxo(t *testing.T) {
	ctx := newWalletTestCtx()
	withFees, err := ctx.dxSplitAddress([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("true"), json.RawMessage("false"), json.RawMessage("false"),
	})
	if err != nil {
		t.Fatalf("dxSplitAddress(include_fees): %v", err)
	}
	if m := mustJSONMap(t, withFees); m["split_amount_with_fees"] != "0.500009" {
		t.Errorf("split_amount_with_fees = %v, want 0.500009 (target + feesPerUtxo 9)", m["split_amount_with_fees"])
	}
	noFees, err := ctx.dxSplitAddress([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("false"), json.RawMessage("false"), json.RawMessage("false"),
	})
	if err != nil {
		t.Fatalf("dxSplitAddress(no fees): %v", err)
	}
	if m := mustJSONMap(t, noFees); m["split_amount_with_fees"] != "0.500000" {
		t.Errorf("split_amount_with_fees = %v, want 0.500000", m["split_amount_with_fees"])
	}
}

// TestDxSplitInputsTxidVoutOnly locks in the input shape: dxSplitInputs entries need only
// txid+vout (the documented example, rpcxbridge.cpp:3347); amount/script are
// resolved from the wallet's unspent list.
func TestDxSplitInputsTxidVoutOnly(t *testing.T) {
	ctx := newWalletTestCtx()
	utxo := ctx.Node.config.Connectors["BTC"].(*stubConn).utxos[0]
	res, err := ctx.dxSplitInputs([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("true"), json.RawMessage("false"), json.RawMessage("false"),
		json.RawMessage(`[{"txid":"` + utxo.TxID + `","vout":0}]`),
	})
	if err != nil {
		t.Fatalf("dxSplitInputs(txid/vout only): %v", err)
	}
	m := mustJSONMap(t, res)
	if m["split_utxo_count"] != float64(1) {
		t.Errorf("split_utxo_count = %v, want 1", m["split_utxo_count"])
	}
}

// TestDxSplitInputsLockedUtxo locks in the C++ "Cannot split utxo already in
// use" guard (rpcxbridge.cpp:3374-3379): a user-specified utxo reserved by an
// order errors 1004.
func TestDxSplitInputsLockedUtxo(t *testing.T) {
	ctx := newWalletTestCtx()
	// Reserve the stub utxo on a live order via the store's mutation path
	// (direct post-Add writes violate the ownership contract, store.go:125-126).
	o := seedOrder(ctx)
	if !ctx.Store.Update(hexEncode(o.ID[:]), func(ord *Order) {
		ord.Utxos = []proto.UtxoEntry{{TxID: [32]byte{}, Vout: 0}}
	}) {
		t.Fatal("order not found")
	}
	_, err := ctx.dxSplitInputs([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("true"), json.RawMessage("false"), json.RawMessage("false"),
		json.RawMessage(`[{"txid":"0000000000000000000000000000000000000000000000000000000000000000","vout":0}]`),
	})
	if err == nil || err.Code != errBadRequest {
		t.Fatalf("dxSplitInputs(locked utxo) = %v, want BAD_REQUEST", err)
	}
}

// TestDxSplitChangeToRequestedAddress locks in the output script behavior: ALL outputs (split and
// change) use the REQUESTED address's script. The stub signs without modifying
// the tx, so the raw tx decodes to the actual outputs; a fresh change address
// would produce a different script.
func TestDxSplitChangeToRequestedAddress(t *testing.T) {
	ctx := newWalletTestCtx()
	c, ok := coins.Get("BTC")
	if !ok {
		t.Fatal("coins.Get BTC failed")
	}
	dest, e := legacyOutputScript(c, btcAddr)
	if e != nil {
		t.Fatalf("legacyOutputScript: %v", e)
	}
	res, err := ctx.dxSplitAddress([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("true"), json.RawMessage("true"), json.RawMessage("false"),
	})
	if err != nil {
		t.Fatalf("dxSplitAddress(show_rawtx): %v", err)
	}
	m := mustJSONMap(t, res)
	wire, derr2 := hex.DecodeString(m["rawtx"].(string))
	if derr2 != nil {
		t.Fatalf("decode rawtx: %v", derr2)
	}
	parsed, derr := coins.Deserialize(wire)
	if derr != nil {
		t.Fatalf("deserialize rawtx: %v", derr)
	}
	tx := parsed
	// 1 split output + 1 change output (neither dust).
	if len(tx.Outputs) != 2 {
		t.Fatalf("tx outputs = %d, want 2 (split + change)", len(tx.Outputs))
	}
	for i, out := range tx.Outputs {
		if !bytes.Equal(out.ScriptPubKey, dest) {
			t.Errorf("output %d script = %x, want requested address script %x", i, out.ScriptPubKey, dest)
		}
	}
}

// TestDxSplitOutputsCarryOneSatBump locks in C++ output-value parity: C++
// createTransaction builds CTxOut(out.second * COIN)
// (xbridgewalletconnectorbtc.cpp:2451) where out.second already carries the
// +1sat nudge from xBridgeValueFromAmount (xutil.cpp:276-280). A clean 0.5
// split output is therefore 50000001 sats, not 50000000 — verified against a
// live Core template (split-show baseline: 57 outputs at 50000001 on Core vs
// 50000000 on go-xbridge).
func TestDxSplitOutputsCarryOneSatBump(t *testing.T) {
	ctx := newWalletTestCtx()
	res, err := ctx.dxSplitAddress([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("false"), json.RawMessage("true"), json.RawMessage("false"),
	})
	if err != nil {
		t.Fatalf("dxSplitAddress: %v", err)
	}
	m := mustJSONMap(t, res)
	wire, derr := hex.DecodeString(m["rawtx"].(string))
	if derr != nil {
		t.Fatalf("decode rawtx: %v", derr)
	}
	tx, derr := coins.Deserialize(wire)
	if derr != nil {
		t.Fatalf("deserialize rawtx: %v", derr)
	}
	if len(tx.Outputs) == 0 {
		t.Fatal("no outputs in split tx")
	}
	// The first output is always a full split (fee claw-back hits the last).
	if tx.Outputs[0].Value != 50000001 {
		t.Errorf("split output = %d sats, want 50000001 (C++ +1sat bump)", tx.Outputs[0].Value)
	}
}

// TestDxSplitSkipsNonP2PKH locks in the C++ getUnspent filter: only P2PKH
// outputs (25-byte 76a914{20}88ac) are eligible for splitting, for both the
// auto and the explicit path (xbridgewalletconnectorbtc.cpp:1617-1636). A
// P2SH utxo sent to the split address must not contribute to the split.
func TestDxSplitSkipsNonP2PKH(t *testing.T) {
	ctx := newWalletTestCtx()
	stub := ctx.Node.config.Connectors["BTC"].(*stubConn)
	stub.utxos = append(stub.utxos, wallet.Utxo{
		// 0.7 on purpose: a 0.5 P2SH utxo would be dropped by the
		// already-split-size rule instead, hiding the script filter.
		TxID: "1111111111111111111111111111111111111111111111111111111111111111", Vout: 1,
		Amount: 70000000, Value: 0.7,
		ScriptPubKey: "a914000000000000000000000000000000000000000087", Address: btcAddr,
	})
	res, err := ctx.dxSplitAddress([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("false"), json.RawMessage("false"), json.RawMessage("false"),
	})
	if err != nil {
		t.Fatalf("dxSplitAddress: %v", err)
	}
	if m := mustJSONMap(t, res); m["split_total"] != "1.000000" {
		t.Errorf("split_total = %v, want 1.000000 (P2SH utxo excluded)", m["split_total"])
	}
}

// TestDxSplitInputsRejectsNonP2PKH locks in the explicit path of the C++
// getUnspent filter (xbridgewalletconnectorbtc.cpp:1617-1636, applied at
// splitUtxos entry before user-utxo matching at :2650-2665): user-specified
// utxos resolve against the P2PKH-filtered unspent list, so a P2SH utxo
// reports "not found or not available" (1004), exactly as C++.
func TestDxSplitInputsRejectsNonP2PKH(t *testing.T) {
	ctx := newWalletTestCtx()
	stub := ctx.Node.config.Connectors["BTC"].(*stubConn)
	p2shTxid := "2222222222222222222222222222222222222222222222222222222222222222"
	stub.utxos = append(stub.utxos, wallet.Utxo{
		TxID: p2shTxid, Vout: 0,
		Amount: 70000000, Value: 0.7,
		ScriptPubKey: "a914000000000000000000000000000000000000000087", Address: btcAddr,
	})
	_, err := ctx.dxSplitInputs([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("false"), json.RawMessage("false"), json.RawMessage("false"),
		json.RawMessage(`[{"txid":"` + p2shTxid + `","vout":0}]`),
	})
	if err == nil || err.Code != errBadRequest || err.Name != "dxSplitInputs" {
		t.Fatalf("dxSplitInputs(P2SH utxo) = %v, want 1004 dxSplitInputs not-found", err)
	}
}

// TestDxSplitInputsMissingUtxoCOutPointFormat locks in the C++ not-found
// message shape: the offending outpoint renders truncated as
// COutPoint(<first 10 hex>, <vout>), in request order (live Core:
// "COutPoint(69089f37e7, 0)").
func TestDxSplitInputsMissingUtxoCOutPointFormat(t *testing.T) {
	ctx := newWalletTestCtx()
	_, err := ctx.dxSplitInputs([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("false"), json.RawMessage("false"), json.RawMessage("false"),
		json.RawMessage(`[{"txid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","vout":3}]`),
	})
	want := "Bad Request user specified utxo was not found or is not available: COutPoint(aaaaaaaaaa, 3)"
	if err == nil || err.Code != errBadRequest || err.Error != want {
		t.Fatalf("dxSplitInputs(missing utxo) = %+v, want 1004 %q", err, want)
	}
}

// TestDxSplitInputsBadVoutTypeThrow locks in the C++ UniValue get_int()
// throw for a non-integer entry vout: HTTP-500 envelope -1 "JSON value is
// not an integer as expected" (live Core), never a 1025 business error.
func TestDxSplitInputsBadVoutTypeThrow(t *testing.T) {
	ctx := newWalletTestCtx()
	_, err := ctx.dxSplitInputs([]json.RawMessage{
		jstr("BTC"), jstr("0.005"), jstr(btcAddr),
		json.RawMessage("false"), json.RawMessage("true"), json.RawMessage("false"),
		json.RawMessage(`[{"txid":"zz","vout":"x"}]`),
	})
	if err == nil || err.Code != -1 || err.Error != "JSON value is not an integer as expected" {
		t.Fatalf("dxSplitInputs(bad vout) = %+v, want -1 %q", err, "JSON value is not an integer as expected")
	}
}

// assertLexicalThrow asserts the C++ boost::lexical_cast<double> throw shape
// for unparseable amounts: HTTP-500 envelope error code -1 with the exact
// runtime message (rpc/server.cpp:584-586). Every dx* amount site must throw
// this instead of a 1025 business error.
func assertLexicalThrow(t *testing.T, err *rpcError) {
	t.Helper()
	const want = "bad lexical cast: source type value could not be interpreted as target"
	if err == nil || err.Code != -1 || err.Error != want {
		t.Fatalf("err = %+v, want code -1 %q", err, want)
	}
}

// TestDxSplitAddressBadAmountThrow locks in the throw for dxSplitAddress
// (rpcxbridge.cpp:3269: xBridgeIntFromReal(lexical_cast<double>(splitAmount))).
func TestDxSplitAddressBadAmountThrow(t *testing.T) {
	ctx := newWalletTestCtx()
	_, err := ctx.dxSplitAddress([]json.RawMessage{
		jstr("BTC"), jstr("abc"), jstr(btcAddr),
		json.RawMessage("false"), json.RawMessage("true"), json.RawMessage("false"),
	})
	assertLexicalThrow(t, err)
}

// TestDxTakeOrderBadAmountThrow locks in the throw for dxTakeOrder
// (rpcxbridge.cpp:1157). The amount check precedes the order lookup, so no
// store setup is needed.
func TestDxTakeOrderBadAmountThrow(t *testing.T) {
	ctx := newWalletTestCtx()
	_, err := ctx.dxTakeOrder([]json.RawMessage{
		jstr("0000000000000000000000000000000000000000000000000000000000000000"),
		jstr(btcAddr), jstr(btcAddr2), jstr("abc"),
	})
	assertLexicalThrow(t, err)
}

// TestDxSplitSubmitFailure locks in the failure shape: a submit failure is 1004
// BAD_REQUEST named after the actual method (rpcxbridge.cpp:3278/3392).
func TestDxSplitSubmitFailure(t *testing.T) {
	ctx := newWalletTestCtx()
	ctx.Node.config.Connectors["BTC"].(*stubConn).sendErr = stubErr("rejected")
	_, err := ctx.dxSplitAddress([]json.RawMessage{
		jstr("BTC"), jstr("0.5"), jstr(btcAddr),
		json.RawMessage("true"), json.RawMessage("false"), json.RawMessage("true"),
	})
	if err == nil || err.Code != errBadRequest || err.Name != "dxSplitAddress" {
		t.Fatalf("dxSplitAddress(submit fail) = %v, want BAD_REQUEST named dxSplitAddress", err)
	}
}

func TestDxTokenListsFromConf(t *testing.T) {
	ctx := newWalletTestCtx()
	local, err := ctx.dxGetLocalTokens(nil)
	if err != nil {
		t.Fatalf("dxGetLocalTokens: %v", err)
	}
	if ls, _ := local.([]string); len(ls) != 1 || ls[0] != "BTC" {
		t.Errorf("local tokens = %v", local)
	}
	// dxGetNetworkTokens is the pure SN service union — the config's
	// NetworkTokens/ExchangeWallets do NOT contribute, so with no connected
	// servicenodes it is empty even though the config names BTC.
	net, err := ctx.dxGetNetworkTokens(nil)
	if err != nil {
		t.Fatalf("dxGetNetworkTokens: %v", err)
	}
	if ns, _ := net.([]string); len(ns) != 0 {
		t.Errorf("network tokens = %v, want [] (pure SN union, no config fallback)", net)
	}
}

// TestDxGetNetworkTokensLive verifies the live servicenode union: tokens
// learned from SNREGISTER / SNPING messages (the same wire source a core
// XBridge wallet uses) are returned as the network list.
func TestDxGetNetworkTokensLive(t *testing.T) {
	ctx := newWalletTestCtx()
	// Simulate two SPV servicenodes advertising their supported tokens via
	// the registry (as if parsed from SNREGISTER / SNPING payloads). Pubkeys
	// must be valid secp256k1 curve points — AddPing rejects off-curve keys
	// like C++ IsFullyValid (servicenode.h:405,791).
	validKey := func(seed byte) [33]byte {
		scalar := make([]byte, 32)
		scalar[31] = seed
		pub, err := crypto.CompressedPubKey(scalar)
		if err != nil {
			t.Fatalf("CompressedPubKey: %v", err)
		}
		return pub
	}
	ctx.Node.snReg.AddPing(servicenode.ServiceNode{
		PubKey:   validKey(0x01),
		Tier:     servicenode.TierSPV,
		Services: []string{"BTC", "LTC", "SYS"},
	})
	ctx.Node.snReg.AddPing(servicenode.ServiceNode{
		PubKey:   validKey(0x02),
		Tier:     servicenode.TierSPV,
		Services: []string{"LTC", "DOGE"},
	})
	net, err := ctx.dxGetNetworkTokens(nil)
	if err != nil {
		t.Fatalf("dxGetNetworkTokens: %v", err)
	}
	ns, ok := net.([]string)
	if !ok {
		t.Fatalf("dxGetNetworkTokens = %v (%T)", net, net)
	}
	want := map[string]bool{"BTC": true, "LTC": true, "SYS": true, "DOGE": true}
	if len(ns) != len(want) {
		t.Fatalf("network tokens = %v, want union %v", ns, want)
	}
	for _, tk := range ns {
		if !want[tk] {
			t.Errorf("unexpected token %q in %v", tk, ns)
		}
	}
}
