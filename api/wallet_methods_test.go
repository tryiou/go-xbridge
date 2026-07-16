package api

import (
	"encoding/json"
	"testing"

	"xbridge-go/coins"
	"xbridge-go/config"
	"xbridge-go/crypto"
	"xbridge-go/wallet"
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
}

func (s *stubConn) Ticker() string { return s.ticker }
func (s *stubConn) GetNewAddress() (string, error) {
	return s.addr, nil
}
func (s *stubConn) ListUnspent(minConf int) ([]wallet.Utxo, error) {
	return s.utxos, nil
}
func (s *stubConn) SignRawTransaction(txHex string, prevTxs []wallet.PrevTx) (string, bool, error) {
	return txHex, true, nil
}
func (s *stubConn) SendRawTransaction(txHex string) (string, error) {
	return "txid123", nil
}
func (s *stubConn) EstimateFee(confTarget int) (uint64, error) {
	return 2, nil
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
	return len(sig) == 65, nil
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
					{TxID: "0000000000000000000000000000000000000000000000000000000000000000", Vout: 0, Amount: 100000000, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
				},
			},
		},
		ExchangeWallets: []string{"BTC"},
		NetworkTokens:   []string{"BTC"},
	}
	if err := coins.InitFromConf(cfg.Confs); err != nil {
		panic(err)
	}
	store := NewStore()
	node := &Node{cfg: cfg, store: store, signer: crypto.NewBtcSigner(), stop: make(chan struct{})}
	return &HandlerCtx{Store: store, Node: node, Config: cfg}
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
	_, err := ctx.dxGetNewTokenAddress([]json.RawMessage{json.RawMessage(`"DOGE"`)})
	if err == nil {
		t.Fatal("expected no-session error for unconfigured coin")
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
	if arr[0]["amount"] != "1" {
		t.Errorf("amount = %v", arr[0]["amount"])
	}
	if arr[0]["txid"] == "" || arr[0]["scriptPubKey"] == "" {
		t.Errorf("missing utxo fields: %v", arr[0])
	}
}

func TestDxGetTokenBalances(t *testing.T) {
	ctx := newWalletTestCtx()
	res, err := ctx.dxGetTokenBalances(nil)
	if err != nil {
		t.Fatalf("dxGetTokenBalances: %v", err)
	}
	m, ok := res.(map[string]string)
	if !ok || m["BTC"] != "1" {
		t.Fatalf("result = %v (%T)", res, res)
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
	m, ok := res.(map[string]interface{})
	if !ok || m["txid"] != "txid123" {
		t.Fatalf("result = %v (%T)", res, res)
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

func TestDxTokenListsFromConf(t *testing.T) {
	ctx := newWalletTestCtx()
	local, err := ctx.dxGetLocalTokens(nil)
	if err != nil {
		t.Fatalf("dxGetLocalTokens: %v", err)
	}
	if ls, _ := local.([]string); len(ls) != 1 || ls[0] != "BTC" {
		t.Errorf("local tokens = %v", local)
	}
	net, err := ctx.dxGetNetworkTokens(nil)
	if err != nil {
		t.Fatalf("dxGetNetworkTokens: %v", err)
	}
	if ns, _ := net.([]string); len(ns) != 1 || ns[0] != "BTC" {
		t.Errorf("network tokens = %v", net)
	}
}
