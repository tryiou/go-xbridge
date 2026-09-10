package api

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/wallet"
)

// reloadTestCtx builds a HandlerCtx whose Node is wired for dxLoadXBridgeConf:
// an on-disk conf (confBody) to hot-reload from, an empty connector set
// initially, and a fresh store. It mirrors newWalletTestCtx but lets the test
// control the conf body, the initial ExchangeWallets and the pre-reload store.
func reloadTestCtx(t *testing.T, confBody string) *HandlerCtx {
	t.Helper()
	td, err := os.MkdirTemp("", "xbridge-reload-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	confPath := filepath.Join(td, "xbridge.conf")
	if err := os.WriteFile(confPath, []byte(confBody), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	cfg := &Config{
		Confs:           map[string]*config.CoinConf{},
		Connectors:      map[string]wallet.Connector{},
		ExchangeWallets: []string{},
		ConfPath:        confPath,
	}
	store := NewStore()
	node := &Node{config: cfg, store: store, signer: crypto.NewBtcSigner(), stop: make(chan struct{}), snReg: servicenode.NewRegistry()}
	return &HandlerCtx{Store: store, Node: node}
}

const ewKeyingConf = `
[Main]
ExchangeWallets=BTC

[BTC]
Title=Bitcoin
CreateTxMethod=BTC
AddressPrefix=0
ScriptPrefix=5
COIN=100000000
BlockTime=600
Confirmations=2
Ip=127.0.0.1
Port=8332

[DOGE]
Title=Dogecoin
CreateTxMethod=BTC
COIN=100000000
BlockTime=60
Confirmations=6
Ip=127.0.0.1
Port=22555
`

// TestReloadAppliesEWKeying: after dxLoadXBridgeConf, exactly the
// [Main].ExchangeWallets currencies are connected — DOGE passes the static
// gates but is not in ExchangeWallets, so it gets no connector, and
// dxGetLocalTokens reflects only the connected set.
func TestReloadAppliesEWKeying(t *testing.T) {
	ctx := reloadTestCtx(t, ewKeyingConf)
	if res, err := ctx.dxLoadXBridgeConf(nil); err != nil || res != true {
		t.Fatalf("dxLoadXBridgeConf = %v %v", res, err)
	}
	lt, _ := ctx.dxGetLocalTokens(nil)
	ls, ok := lt.([]string)
	if !ok || len(ls) != 1 || ls[0] != "BTC" {
		t.Fatalf("dxGetLocalTokens after reload = %v, want [BTC] (DOGE not in ExchangeWallets)", lt)
	}
	if _, has := ctx.Node.cfg().Confs["DOGE"]; !has {
		t.Error("DOGE should still be in the admitted conf set (registry), just not connected")
	}
}

// TestReloadPrunesUnconnectedOrders locks the order clearing: when
// ShowAllOrders is false, non-local orders whose currency has no connector are
// dropped; local orders always survive. With ShowAllOrders=true everything is
// kept (C++ clearNonLocalOrders, rpcxbridge.cpp:229-233).
func TestReloadPrunesUnconnectedOrders(t *testing.T) {
	ctx := reloadTestCtx(t, ewKeyingConf)
	store := ctx.Store
	store.Add(&Order{
		ID:           [32]byte{0x01},
		FromCurrency: "BTC", ToCurrency: "BLOCK",
		Status: "open", Mine: true,
	})
	store.Add(&Order{
		ID:           [32]byte{0x02},
		FromCurrency: "LTC", ToCurrency: "BTC",
		Status: "open", Mine: false, Role: 'B',
	})
	if res, err := ctx.dxLoadXBridgeConf(nil); err != nil || res != true {
		t.Fatalf("dxLoadXBridgeConf = %v %v", res, err)
	}
	got := store.List()
	if len(got) != 1 || got[0].ID != [32]byte{0x01} {
		t.Fatalf("orders after reload = %d, want only the local BTC order (LTC pruned)", len(got))
	}

	// With ShowAllOrders, the non-local order survives the reload.
	ctx = reloadTestCtx(t, "[Main]\nExchangeWallets=BTC\nShowAllOrders=1\n"+ewKeyingConf)
	store = ctx.Store
	store.Add(&Order{ID: [32]byte{0x02}, FromCurrency: "LTC", ToCurrency: "BTC", Status: "open", Mine: false, Role: 'B'})
	if res, err := ctx.dxLoadXBridgeConf(nil); err != nil || res != true {
		t.Fatalf("dxLoadXBridgeConf (showAll) = %v %v", res, err)
	}
	if len(store.List()) != 1 {
		t.Fatalf("orders after show-all reload = %d, want 1 kept", len(store.List()))
	}
}

// TestReloadPreservesFlagOverrides locks the G2->G3 handoff: the -dxnowallets
// override and the reachability-probe toggle survive a reload
// (the pre-fix fresh Config dropped all three).
func TestReloadPreservesFlagOverrides(t *testing.T) {
	ctx := reloadTestCtx(t, ewKeyingConf)
	ctx.Node.config.ForceShowAllOrders = true
	ctx.Node.config.CheckReachability = true
	// Deterministic probe: CheckReachability is on, but a hermetic test must
	// not round-trip to the fixture's 127.0.0.1:8332 (which may host a real
	// wallet on the dev box).
	a := wallet.NewActivator()
	a.Probe = func(wallet.Connector) error { return nil }
	ctx.Node.activator = a
	if res, err := ctx.dxLoadXBridgeConf(nil); err != nil || res != true {
		t.Fatalf("dxLoadXBridgeConf = %v %v", res, err)
	}
	c := ctx.Node.cfg()
	if !c.ShowAllOrders {
		t.Error("ShowAllOrders lost on reload (ForceShowAllOrders should be preserved)")
	}
	if !c.CheckReachability {
		t.Error("CheckReachability lost on reload")
	}
}

// TestSweepConnectors locks the periodic updateActiveWallets sweep: a
// wallet that becomes unreachable is disconnected on the next sweep and
// reconnected when it recovers. The sweep never prunes orders (C++ only prunes
// on dxLoadXBridgeConf).
func TestSweepConnectors(t *testing.T) {
	cfg := &Config{
		Confs: map[string]*config.CoinConf{
			"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", Coin: 100000000, BlockTime: 600, Confirmations: 2, Ip: "127.0.0.1", Port: 8332},
			"LTC": {Ticker: "LTC", CreateTxMethod: "LTC", Coin: 100000000, BlockTime: 150, Confirmations: 2, Ip: "127.0.0.1", Port: 9332},
		},
		Connectors: map[string]wallet.Connector{
			"BTC": &stubConn{ticker: "BTC", addr: btcAddr},
			"LTC": &stubConn{ticker: "LTC", addr: btcAddr},
		},
		ExchangeWallets:   []string{"BTC", "LTC"},
		CheckReachability: true,
	}
	store := NewStore()
	store.Add(&Order{ID: [32]byte{0x09}, FromCurrency: "LTC", ToCurrency: "BTC", Status: "open", Mine: false, Role: 'B'})
	node := &Node{config: cfg, store: store, signer: crypto.NewBtcSigner(), stop: make(chan struct{})}

	var ltcDown atomic.Bool
	ltcDown.Store(true)
	a := wallet.NewActivator()
	a.Probe = func(c wallet.Connector) error {
		if c.Ticker() == "LTC" && ltcDown.Load() {
			return os.ErrDeadlineExceeded
		}
		return nil
	}
	node.activator = a

	// LTC is down: the sweep drops its connector.
	node.sweepConnectors()
	conns := node.cfg().Connectors
	if len(conns) != 1 || conns["BTC"] == nil {
		t.Fatalf("connectors after sweep (LTC down) = %v, want {BTC}", conns)
	}
	// The sweep must NOT prune orders (that is dxLoadXBridgeConf's job).
	if len(store.List()) != 1 {
		t.Fatalf("sweep pruned orders: want 1 kept, got %d", len(store.List()))
	}

	// LTC recovers: once the 300s bad-wallet retry window elapses (modelled
	// here by ClearBad, which a dxLoadXBridgeConf also triggers), the next
	// sweep re-probes and reconnects it.
	ltcDown.Store(false)
	node.activator.ClearBad()
	node.sweepConnectors()
	conns = node.cfg().Connectors
	if len(conns) != 2 || conns["LTC"] == nil {
		t.Fatalf("connectors after sweep (LTC up) = %v, want {BTC LTC}", conns)
	}
}
