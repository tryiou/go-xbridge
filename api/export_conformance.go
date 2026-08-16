//go:build conformance

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// Conformance-test export shim.
//
// The external conformance suite (module go-xbridge/conformance, run with
// `go test -tags conformance`) exercises go-xbridge's wire/crypto/state code
// through the exported packages directly, but its fx* fixture hooks need a
// handful of go-xbridge/api functions that are intentionally unexported
// (they are internal helpers, not RPC surface). This file re-exports exactly
// those functions under the `conformance` build tag so the suite can wire its
// fixtures without widening the package's public API for normal builds.
//
// fxErrorName / fxResponseKeys are wired here to a fixture HandlerCtx built
// like the api unit-test nodes: a directly-constructed Node (no engine, no
// dial) whose store/connectors are seeded per method, so the suite drives the
// REAL dispatch/handler paths instead of re-implementing the response shapes.
//
// Build-tag discipline: this file is compiled ONLY with `-tags conformance`.
// Normal `go build ./...` / `go test ./...` never see these symbols.

// XbridgeErrorText re-exports xbridgeErrorText (mirror of C++
// util/xbridgeerror.cpp::xbridgeErrorText).
func XbridgeErrorText(code int, arg string) string { return xbridgeErrorText(code, arg) }

// FormatXAmount re-exports formatXAmount (xBridgeStringValueFromAmount).
func FormatXAmount(amt uint64) string { return formatXAmount(amt) }

// FormatBalanceNative re-exports formatBalanceNative (xBridgeStringValueFromPrice
// over a native base-unit balance) adapted to the suite's (decimals, native)
// signature.
func FormatBalanceNative(decimals, native uint64) string {
	return formatBalanceNative(coins.Coin{Decimals: int(decimals)}, native)
}

// FormatXPrice re-exports formatXPrice over a plain ratio (denominator /
// numerator). The suite's signature mirrors the C++ price formula call sites
// (xutil.cpp:293-312); Go renders the plain ratio, which is itself the
// documented DIVERGENT behavior for dxGetOrderBook (see the conformance
// suite's TestFormatXPriceVectors row dxGetOrderBook/price-formula).
func FormatXPrice(numerator, denominator uint64) string {
	return formatXPrice(float64(denominator) / float64(numerator))
}

// ISO8601 re-exports iso8601 (xutil::iso8601).
func ISO8601(us uint64) string { return iso8601(us) }

// ParseXAmount re-exports parseXAmount (xBridgeAmountFromString).
func ParseXAmount(s string) (uint64, error) { return parseXAmount(s) }

// LocktimeConstants re-exports the unexported locktime computation constants
// (api/swap.go, api/locktime.go) by the C++ constant names the suite asserts.
func LocktimeConstants() map[string]int64 {
	return map[string]int64{
		"XMIN_LOCKTIME_BLOCKS":                xMinLockTimeBlocks,
		"XMAX_LOCKTIME_DRIFT_BLOCKS":          xMaxLockTimeDriftBlocks,
		"XMAKER_LOCKTIME_TARGET_SECONDS":      makerLockTimeSec,
		"XTAKER_LOCKTIME_TARGET_SECONDS":      takerLockTimeSec,
		"XSLOW_TAKER_LOCKTIME_TARGET_SECONDS": xSlowTakerLockTimeSec,
		"XSLOW_BLOCKTIME_SECONDS":             xSlowBlockTimeSec,
		"XLOCKTIME_DRIFT_SECONDS":             xLockTimeDriftSeconds,
		"LOCKTIME_THRESHOLD":                  lockTimeThreshold,
	}
}

// OrderBookResultJSON re-exports the dxGetOrderBook result codec
// (orderBookResult.MarshalJSON) so the conformance suite can assert the
// detail-4 value SHAPE (flat ["price","amount",["ids"]] vs the nested row
// form) — the suite has no live api.Handler fixture to drive dxGetOrderBook
// itself. Mirrors the RPC marshaling path exactly.
func OrderBookResultJSON(detail int, maker, taker string, asks, bids [][]interface{}) ([]byte, error) {
	return json.Marshal(orderBookResult{
		Detail: detail, Maker: maker, Taker: taker, Asks: asks, Bids: bids,
	})
}

// ---------------------------------------------------------------------------
// fxErrorName / fxResponseKeys fixture harness.
//
// These two hooks need a live api.HandlerCtx wired like the api unit-test
// nodes (directly-constructed Node — no engine, no dial — so every handler
// runs single-threaded inline). The fixture is seeded per method with the same
// stub connector / XConn / servicenode-registry patterns the api tests use
// (wallet_methods_test.go stubConn, hub_gate_test.go newHubNode, node_test.go
// captureXConn). The write commands (dxMakeOrder / dxMakePartialOrder /
// dxTakeOrder / dxCancelOrder) require a real hub + stub XConn + funded
// connectors to reach a SUCCESS response; the conformance suite does not
// assert those shapes from this fixture (their rows are documented known-gaps,
// already covered by the api KAT tests), so the response fixture only drives
// the read-only methods.
// ---------------------------------------------------------------------------

// conformanceStubConn is a hermetic wallet.Connector returning fixed UTXOs and
// echoing signatures, mirroring the api unit-test stubConn. It is NOT a
// conformance fixture itself — it exists only so the response-shape and
// error-name hooks can invoke the real handlers without a live wallet.
type conformanceStubConn struct {
	ticker string
	addr   string
	utxos  []wallet.Utxo
}

func (s *conformanceStubConn) Ticker() string { return s.ticker }
func (s *conformanceStubConn) GetBalance() (uint64, error) {
	return 100000000, nil
}
func (s *conformanceStubConn) GetNewAddress() (string, error) { return s.addr, nil }
func (s *conformanceStubConn) ListUnspent(minConf int) ([]wallet.Utxo, error) {
	return s.utxos, nil
}
func (s *conformanceStubConn) SignRawTransaction(txHex string, prevTxs []wallet.PrevTx) (string, bool, error) {
	return txHex, true, nil
}
func (s *conformanceStubConn) SendRawTransaction(txHex string) (string, error) {
	return "txid123", nil
}
func (s *conformanceStubConn) GetRelayFee() (float64, error) { return 0.0001, nil }
func (s *conformanceStubConn) GetBlockCount() (int64, error) { return 100, nil }
func (s *conformanceStubConn) GetBlockHash(height int64) ([32]byte, error) {
	var h [32]byte
	h[0] = 0xab
	return h, nil
}
func (s *conformanceStubConn) GetRawTransaction(txid string) (string, error) {
	return "", fmt.Errorf("conformance stub: getrawtransaction not supported")
}
func (s *conformanceStubConn) CheckDepositTransaction(depositTxID, expectedScriptHex string, expectedAmount uint64, requiredConfirmations int) (wallet.DepositCheck, error) {
	return wallet.DepositCheck{IsGood: true}, nil
}
func (s *conformanceStubConn) SignMessage(address, message string) ([]byte, error) {
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
func (s *conformanceStubConn) VerifyMessage(address string, sig []byte, message string) (bool, error) {
	return len(sig) == 65, nil
}
func (s *conformanceStubConn) GetTxOut(txid string, vout uint32) (wallet.Utxo, bool, error) {
	for _, u := range s.utxos {
		if u.TxID == txid && u.Vout == vout {
			return u, true, nil
		}
	}
	return wallet.Utxo{}, false, nil
}

// conformanceXConn is a stub api.XConn whose ReadPacket blocks forever (returns
// io.EOF) so a fixture cannot accidentally consume real input; WritePacket
// succeeds so the write-command paths (requireWrite + broadcast) are reachable.
type conformanceXConn struct{}

func (c *conformanceXConn) ReadPacket() (*proto.Packet, string, error) {
	return nil, "", io.EOF
}
func (c *conformanceXConn) WritePacket(p *proto.Packet, dest [20]byte) error { return nil }
func (c *conformanceXConn) Close() error                                     { return nil }

// conformanceCoinConfs is the fixture coin set (BTC/SYS/LTC/BLOCK/XB), all
// base58 legacy (prefix 0) so one valid address decodes for every fixture coin.
func conformanceCoinConfs() map[string]*config.CoinConf {
	return map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
		"SYS": {Ticker: "SYS", CreateTxMethod: "SYS", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
		"LTC": {Ticker: "LTC", CreateTxMethod: "LTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
		"BLOCK": {Ticker: "BLOCK", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5,
			Coin: 100000000, TxVersion: 1},
		"XB": {Ticker: "XB", CreateTxMethod: "XB", AddressPrefix: 0, ScriptPrefix: 5,
			Coin: 1000000, Confirmations: 1, FeePerByte: 100, DustAmount: 1, TxVersion: 1},
	}
}

// conformanceBtcUtxo is the standard single 3.0 BTC p2pkh utxo the read
// methods iterate (dxGetUtxos / dxGetTokenBalances / dxGetLockedUtxos).
func conformanceBtcUtxo() wallet.Utxo {
	return wallet.Utxo{
		TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0,
		Amount: 300000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac",
		Address: conformanceAddr,
	}
}

// conformanceXbUtxo is a 3.0 XB (Decimals 6, native == XBridge units) p2pkh
// utxo used by the dxSplitAddress / dxSplitInputs success rows.
func conformanceXbUtxo() wallet.Utxo {
	return wallet.Utxo{
		TxID: "0000000000000000000000000000000000000000000000000000000000000002", Vout: 0,
		Amount: 3000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac",
		Address: conformanceAddr,
	}
}

// conformanceAddr is a valid BTC base58 P2PKH address (prefix 0) that decodes
// for every fixture coin (all AddressPrefix 0).
const conformanceAddr = "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"

// conformanceAddr2 is a second valid P2PKH address (also prefix 0) so the
// maker/taker address-distinctness gates can be satisfied.
const conformanceAddr2 = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"

// conformanceInitCoins seeds the fixture coin set into the package-global
// registry exactly once (see conformanceReadNode).
var conformanceInitCoins sync.Once

// conformanceReadNode builds a directly-constructed Node (no engine, no dial)
// with the fixture connectors — the same shape newWalletTestCtx / buildHubNode
// produce. hub selects whether a running servicenode registry and a stub XConn
// are wired (needed only by the dxMakePartialOrder error-name row, whose
// MakeOrder path must pass requireWrite + hub Pick before its partial gate).
func conformanceReadNode(store *Store, hub bool) *HandlerCtx {
	confs := conformanceCoinConfs()
	// The fixture coin set is fixed and shared by every row, so register it
	// once (InitFromConf replaces the package-global registry; calling it per
	// row is idempotent but a sync.Once keeps the fixture hermetic).
	conformanceInitCoins.Do(func() {
		if err := coins.InitFromConf(confs); err != nil {
			panic(err)
		}
	})
	cfg := &Config{
		Confs: confs,
		Connectors: map[string]wallet.Connector{
			"BTC": &conformanceStubConn{ticker: "BTC", addr: conformanceAddr, utxos: []wallet.Utxo{conformanceBtcUtxo()}},
			"SYS": &conformanceStubConn{ticker: "SYS", addr: conformanceAddr},
			"LTC": &conformanceStubConn{ticker: "LTC", addr: conformanceAddr},
			"BLOCK": &conformanceStubConn{ticker: "BLOCK", addr: conformanceAddr,
				utxos: []wallet.Utxo{{TxID: "0000000000000000000000000000000000000000000000000000000000000004", Vout: 0,
					Amount: 100000000, Value: 1.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: conformanceAddr}}},
			"XB": &conformanceStubConn{ticker: "XB", addr: conformanceAddr, utxos: []wallet.Utxo{conformanceXbUtxo()}},
		},
		ExchangeWallets: []string{"BLOCK", "LTC"},
		NetworkTokens:   []string{"BTC", "SYS", "LTC", "BLOCK", "XB"},
	}
	n := &Node{
		config:   cfg,
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		store:    store,
		sessions: map[string]*SwapSession{},
	}
	if hub {
		n.conn = &conformanceXConn{}
		n.snReg = conformanceHubRegistry()
	}
	return &HandlerCtx{Store: store, Node: n}
}

// conformanceHubRegistry returns a registry with one running SPV hub
// advertising BTC+SYS at the current protocol version — the same shape
// runningHub (make_order_kat_test.go) builds, so MakeOrder's Pick succeeds.
func conformanceHubRegistry() *servicenode.Registry {
	priv := make([]byte, 32)
	priv[31] = 0x51
	pub, err := crypto.CompressedPubKey(priv)
	if err != nil {
		panic(err)
	}
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: pub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"},
		XBridgeVersion: proto.ProtocolVersion,
	})
	return reg
}

// conformanceSeedOrder adds a fixture order to the store (BTC/SYS, open,
// maker-typed, Mine=true) — the same record the api read-path tests seed.
func conformanceSeedOrder(store *Store) *Order {
	o := &Order{
		ID:           [32]byte{0x01},
		Type:         OrderTypeMaker,
		FromCurrency: "BTC",
		FromAmount:   1500000,
		ToCurrency:   "SYS",
		ToAmount:     300000,
		Created:      uint64(1),
		Updated:      uint64(1),
		Status:       "open",
		Mine:         true,
		MakerAddress: conformanceAddr,
		TakerAddress: conformanceAddr,
	}
	store.Add(o)
	return o
}

// conformanceSeedPartialOrder adds a local partial order (PartialAllowed=true)
// so dxGetMyPartialOrderChain / dxPartialOrderChainDetails resolve a chain.
func conformanceSeedPartialOrder(store *Store) *Order {
	o := &Order{
		ID:             [32]byte{0x02},
		Type:           OrderTypeMaker,
		FromCurrency:   "BTC",
		FromAmount:     1500000,
		ToCurrency:     "SYS",
		ToAmount:       300000,
		Created:        uint64(1),
		Updated:        uint64(1),
		Status:         "open",
		Mine:           true,
		PartialAllowed: true,
		MinFromAmount:  1000000,
		OrigFromAmount: 1500000,
		OrigToAmount:   300000,
		MakerAddress:   conformanceAddr,
		TakerAddress:   conformanceAddr,
	}
	store.Add(o)
	return o
}

// conformanceSeedFill adds a BTC/SYS fill the read history methods aggregate.
func conformanceSeedFill(store *Store) {
	store.AddFill(fillEntry{
		ID: "aaa", Time: 1600000000000000, Maker: "BTC", MakerSize: "1.500000",
		Taker: "SYS", TakerSize: "0.300000",
	})
}

// conformanceSeedCancelled adds a cancelled order old enough to flush.
func conformanceSeedCancelled(store *Store) {
	o := &Order{
		ID:           [32]byte{0x03},
		Type:         OrderTypeMaker,
		FromCurrency: "BTC",
		FromAmount:   1500000,
		ToCurrency:   "SYS",
		ToAmount:     300000,
		Created:      uint64(1),
		Updated:      uint64(1),
		Status:       "canceled",
		Mine:         true,
	}
	store.Add(o)
}

// conformanceSeedLocked adds a made order with one reserved utxo so
// dxGetLockedUtxos reports a non-empty all_locked_utxo list. The reserved
// entry is the BLOCK connector's utxo (txid ...04:vout 0), because the
// no-id path scans only ExchangeWallets (BLOCK, LTC) for matching locked
// keys (handlers.go:1311-1328) — locking a BTC utxo would leave the list
// empty since BTC is not an exchange wallet.
func conformanceSeedLocked(store *Store) {
	o := &Order{
		ID:           [32]byte{0x04},
		Type:         OrderTypeMaker,
		FromCurrency: "BTC",
		FromAmount:   1500000,
		ToCurrency:   "SYS",
		ToAmount:     300000,
		Created:      uint64(1),
		Updated:      uint64(1),
		Status:       "open",
		Mine:         true,
		Utxos:        []proto.UtxoEntry{{TxID: [32]byte{0x04}, Vout: 0}},
	}
	store.Add(o)
}

// conformanceJstr renders a string as a positional JSON-RPC param (json.Marshal
// so quotes/backslashes are escaped correctly).
func conformanceJstr(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// conformanceJnum renders an integer as a positional JSON-RPC param.
func conformanceJnum(n int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf("%d", n))
}

// conformanceJbool renders a boolean as a positional JSON-RPC param.
func conformanceJbool(b bool) json.RawMessage {
	if b {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

// ---------------------------------------------------------------------------
// ConformanceErrorName — trigger a business error for method and return the
// rpcError.Name field (C++ __FUNCTION__), mirroring the server's dispatch:
// checkArity first, then the handler. Rows that cannot produce a business
// error (dxGetTradingData, gettradingdata) return ("", nil); the suite marks
// them as documented known-gaps.
// ---------------------------------------------------------------------------

// ConformanceErrorName triggers a business error for method on a fixture ctx
// and returns the rpcError.Name field. It is the fxErrorName wiring.
func ConformanceErrorName(method string) (string, error) {
	ctx, params, err := conformanceErrorParams(method)
	if err != nil {
		return "", err
	}
	if aerr := checkArity(method, len(params)); aerr != nil {
		return aerr.Name, nil
	}
	h := Lookup(method)
	if h == nil {
		return "", nil
	}
	_, rerr := h(ctx, params)
	if rerr == nil {
		return "", fmt.Errorf("conformance: %s returned no business error with fixture params", method)
	}
	return rerr.Name, nil
}

// conformanceErrorParams returns the per-method params that reach a business
// error, and the ctx they run on (hub=true for the one row that needs it).
func conformanceErrorParams(method string) (*HandlerCtx, []json.RawMessage, error) {
	store := NewStore()
	read := func() *HandlerCtx { return conformanceReadNode(store, false) }
	hub := func() *HandlerCtx { return conformanceReadNode(store, true) }
	switch method {
	case "dxGetOrderFills":
		return read(), []json.RawMessage{conformanceJstr("BTC")}, nil
	case "dxGetOrders":
		return read(), []json.RawMessage{conformanceJstr("x")}, nil
	case "dxGetOrder":
		return read(), nil, nil
	case "dxGetLocalTokens":
		return read(), []json.RawMessage{conformanceJstr("x")}, nil
	case "dxLoadXBridgeConf":
		return read(), []json.RawMessage{conformanceJstr("x")}, nil
	case "dxGetNewTokenAddress":
		return read(), nil, nil
	case "dxGetNetworkTokens":
		return read(), []json.RawMessage{conformanceJstr("x")}, nil
	case "dxMakeOrder":
		return read(), []json.RawMessage{
			conformanceJstr("BTC"), conformanceJstr("1.5"), conformanceJstr(conformanceAddr),
			conformanceJstr("SYS"), conformanceJstr("0.3"), conformanceJstr(conformanceAddr),
			conformanceJstr("partial"),
		}, nil
	case "dxMakePartialOrder":
		// Reaches the partial gate (node.go:1311-1312) only after requireWrite
		// (node.go:1214) and hub Pick (node.go:1266-1284) succeed, so this row
		// runs on the hub node. If Pick ever fails, MakeOrder surfaces the
		// errNoServiceNode path named "dxMakeOrder" (node.go:1275) — the suite
		// would then report "got dxMakeOrder, want dxMakePartialOrder".
		return hub(), []json.RawMessage{
			conformanceJstr("BTC"), conformanceJstr("1.5"), conformanceJstr(conformanceAddr),
			conformanceJstr("SYS"), conformanceJstr("0.3"), conformanceJstr(conformanceAddr2),
			conformanceJstr(""),
		}, nil
	case "dxTakeOrder":
		return read(), []json.RawMessage{
			conformanceJstr("id"), conformanceJstr(conformanceAddr), conformanceJstr(conformanceAddr),
		}, nil
	case "dxCancelOrder":
		return read(), nil, nil
	case "dxGetOrderHistory":
		return read(), nil, nil
	case "dxGetOrderBook":
		return read(), nil, nil
	case "dxGetTokenBalances":
		return read(), []json.RawMessage{conformanceJstr("x")}, nil
	case "dxGetMyOrders":
		return read(), []json.RawMessage{conformanceJstr("x")}, nil
	case "dxGetMyPartialOrderChain":
		return read(), []json.RawMessage{conformanceJstr("")}, nil
	case "dxPartialOrderChainDetails":
		return read(), []json.RawMessage{conformanceJstr("")}, nil
	case "dxGetLockedUtxos":
		return read(), []json.RawMessage{conformanceJstr("x"), conformanceJstr("y")}, nil
	case "dxFlushCancelledOrders":
		return read(), []json.RawMessage{conformanceJnum(1), conformanceJnum(2)}, nil
	case "dxSplitAddress":
		return read(), []json.RawMessage{conformanceJstr("NOPE"), conformanceJstr("1"), conformanceJstr(conformanceAddr)}, nil
	case "dxSplitInputs":
		return read(), []json.RawMessage{
			conformanceJstr("NOPE"), conformanceJstr("1"), conformanceJstr(conformanceAddr),
			conformanceJbool(true), conformanceJbool(false), conformanceJbool(true),
			json.RawMessage("[]"),
		}, nil
	case "dxGetUtxos":
		return read(), []json.RawMessage{conformanceJstr("NOPE")}, nil
	case "getnetworkinfo":
		return read(), []json.RawMessage{conformanceJnum(1)}, nil
	case "dxGetTradingData", "gettradingdata":
		// No business error path (envelope-only on both sides / no dispatch
		// entry); the suite skips these rows as documented known-gaps.
		return read(), nil, nil
	}
	return nil, nil, fmt.Errorf("conformance: no error-name recipe for %s", method)
}

// ---------------------------------------------------------------------------
// ConformanceResponseKeys — invoke method against a seeded fixture dataset and
// return the ordered top-level JSON object keys of the response. For array
// responses the keys of the FIRST element are returned (the C++ pushKV order
// is per record). Mirrors the server dispatch: checkArity then the handler.
// ---------------------------------------------------------------------------

// ConformanceResponseKeys invokes method on a seeded fixture ctx and returns
// the ordered JSON keys of the response (fxResponseKeys wiring).
func ConformanceResponseKeys(method string) ([]string, error) {
	ctx, params, err := conformanceResponseParams(method)
	if err != nil {
		return nil, err
	}
	if aerr := checkArity(method, len(params)); aerr != nil {
		return nil, fmt.Errorf("conformance: %s arity: %s", method, aerr.Error)
	}
	h := Lookup(method)
	if h == nil {
		return nil, fmt.Errorf("conformance: no handler for %s", method)
	}
	res, rerr := h(ctx, params)
	if rerr != nil {
		return nil, fmt.Errorf("conformance: %s error: %s", method, rerr.Error)
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(b)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// conformanceResponseParams returns the seeded fixture ctx and params for a
// response-shape row. Only the read-only methods are driven here; the write
// commands' rows are documented known-gaps in the suite (covered by the api
// KAT tests), so no hub/broadcast is needed.
func conformanceResponseParams(method string) (*HandlerCtx, []json.RawMessage, error) {
	store := NewStore()
	read := func() *HandlerCtx { return conformanceReadNode(store, false) }
	switch method {
	case "dxGetOrderFills":
		conformanceSeedFill(store)
		return read(), []json.RawMessage{conformanceJstr("BTC"), conformanceJstr("SYS")}, nil
	case "dxGetOrders":
		conformanceSeedOrder(store)
		return read(), nil, nil
	case "dxGetOrder":
		o := conformanceSeedOrder(store)
		return read(), []json.RawMessage{conformanceJstr(orderIDString(o.ID))}, nil
	case "dxGetOrderBook":
		conformanceSeedOrder(store)
		return read(), []json.RawMessage{conformanceJnum(1), conformanceJstr("BTC"), conformanceJstr("SYS")}, nil
	case "dxGetMyOrders":
		conformanceSeedOrder(store)
		return read(), nil, nil
	case "dxGetMyPartialOrderChain":
		o := conformanceSeedPartialOrder(store)
		return read(), []json.RawMessage{conformanceJstr(orderIDString(o.ID))}, nil
	case "dxPartialOrderChainDetails":
		o := conformanceSeedPartialOrder(store)
		return read(), []json.RawMessage{conformanceJstr(orderIDString(o.ID))}, nil
	case "dxGetLockedUtxos":
		conformanceSeedLocked(store)
		return read(), nil, nil
	case "dxGetTokenBalances":
		return read(), nil, nil
	case "dxGetTradingData":
		conformanceSeedFill(store)
		return read(), nil, nil
	case "dxGetUtxos":
		return read(), []json.RawMessage{conformanceJstr("BTC")}, nil
	case "dxFlushCancelledOrders":
		conformanceSeedCancelled(store)
		return read(), []json.RawMessage{conformanceJnum(0)}, nil
	case "getnetworkinfo":
		return read(), nil, nil
	case "dxSplitAddress":
		return read(), []json.RawMessage{
			conformanceJstr("XB"), conformanceJstr("1.000000"), conformanceJstr(conformanceAddr),
			conformanceJbool(false), conformanceJbool(false), conformanceJbool(false),
		}, nil
	case "dxSplitInputs":
		return read(), []json.RawMessage{
			conformanceJstr("XB"), conformanceJstr("1.000000"), conformanceJstr(conformanceAddr),
			conformanceJbool(false), conformanceJbool(false), conformanceJbool(false),
			json.RawMessage(`[{"txid":"0000000000000000000000000000000000000000000000000000000000000002","vout":0}]`),
		}, nil
	}
	return nil, nil, fmt.Errorf("conformance: no response recipe for %s", method)
}

// orderedObjectKeys walks the JSON document and returns the keys of the
// top-level object — or, for an array result, the keys of its FIRST element —
// in document order, exactly as encoding/json emits them (struct fields in
// declaration order; map keys sorted). Mirrors the server's marshal path so
// the suite's exact-order assertions compare against the real wire shape.
func orderedObjectKeys(b []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			return objectKeysInOrder(dec)
		case '[':
			// Peek the first element; the keys are per record.
			el, err := dec.Token()
			if err != nil {
				return nil, err
			}
			if dd, ok := el.(json.Delim); ok && dd == '{' {
				return objectKeysInOrder(dec)
			}
			return nil, fmt.Errorf("conformance: array response has no object element")
		}
	}
	return nil, fmt.Errorf("conformance: response is not an object or array")
}

// objectKeysInOrder collects the object keys in document order. The caller has
// already consumed the opening '{'.
func objectKeysInOrder(dec *json.Decoder) ([]string, error) {
	keys := []string{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("conformance: unexpected object key token")
		}
		keys = append(keys, key)
		// Skip the value (nested object/array/scalar).
		if err := skipJSONValue(dec); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// skipJSONValue consumes one complete JSON value from dec.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{', '[':
			for dec.More() {
				if err := skipJSONValue(dec); err != nil {
					return err
				}
			}
			_, err := dec.Token() // consume the closing delimiter
			return err
		}
	}
	return nil
}

// (temporary helpers for zz_value_check_test.go — will be removed)
func responseParamsForLocked() (*HandlerCtx, []json.RawMessage, error) {
	return conformanceResponseParams("dxGetLockedUtxos")
}

func callHandler(method string, ctx *HandlerCtx, params []json.RawMessage) (interface{}, *rpcError) {
	return Lookup(method)(ctx, params)
}
