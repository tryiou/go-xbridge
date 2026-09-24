package api

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-xbridge/config"
	"go-xbridge/crypto"
	xlog "go-xbridge/log"
	"go-xbridge/wallet"
)

// TestRpcPollDietCollapsesGuiPolls pins the access-log diet through a live
// server: read-only GUI polls (getnetworkinfo at Hz rates and friends)
// collapse to first-sighting plus periodic counts, while state-changing
// calls keep one line per call and unlisted methods stay per-call.
func TestRpcPollDietCollapsesGuiPolls(t *testing.T) {
	genPath, _ := installSplitLogs(t)
	rpcPollDedup.Flush() // start clean: package-global 60 s bucket keyed on method
	t.Cleanup(rpcPollDedup.Flush)
	ctx := newTestCtx()
	srv := NewServer(ctx)

	for i := 0; i < 3; i++ {
		callRPC(t, srv, `{"method":"getnetworkinfo","params":[],"id":1}`)
	}
	// A state-changing call with valid arity logs every time (the order id
	// is unknown, so the handler answers a business error — the access
	// line fires before the handler runs either way).
	callRPC(t, srv, `{"method":"dxCancelOrder","params":["deadbeef"],"id":2}`)
	callRPC(t, srv, `{"method":"dxCancelOrder","params":["deadbeef"],"id":3}`)
	// An unlisted read-only method stays per-call too (fail-open): it is
	// rare enough that collapsing it buys nothing and hiding it costs
	// debuggability.
	callRPC(t, srv, `{"method":"dxGetOrder","params":["deadbeef"],"id":4}`)
	callRPC(t, srv, `{"method":"dxGetOrder","params":["deadbeef"],"id":5}`)

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(readLogFile(t, genPath), "method=getnetworkinfo") {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for poll access line")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // settle: repeats must stay collapsed

	gen := readLogFile(t, genPath)
	if got := strings.Count(gen, `msg="rpc request" method=getnetworkinfo`); got != 1 {
		t.Errorf("poll access lines = %d, want 1 (first-sighting only)", got)
	}
	if got := strings.Count(gen, `msg="rpc request" method=dxCancelOrder`); got != 2 {
		t.Errorf("write access lines = %d, want 2 (one per call)", got)
	}
	if got := strings.Count(gen, `msg="rpc request" method=dxGetOrder`); got != 2 {
		t.Errorf("unlisted-read access lines = %d, want 2 (fail-open per-call)", got)
	}
}

// TestSweepUnreachableStaysDebug pins the sweep diet: a wallet that stays
// down is steady state, not a warning — the 30 s re-probe must not Warn on
// every tick. The drop is still logged (at Debug) and the disconnect
// behavior is unchanged.
func TestSweepUnreachableStaysDebug(t *testing.T) {
	genPath, _ := installSplitLogs(t)
	old := xlog.Level()
	xlog.SetLevel(slog.LevelWarn)
	t.Cleanup(func() { xlog.SetLevel(old) })

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

	node.sweepConnectors()
	if conns := node.cfg().Connectors; len(conns) != 1 || conns["BTC"] == nil {
		t.Fatalf("connectors after sweep (LTC down) = %v, want {BTC}", conns)
	}
	if strings.Contains(readLogFile(t, genPath), "wallet not reachable") {
		t.Errorf("steady-state down wallet must not Warn on every sweep")
	}

	// Still visible at Debug: re-probe after the bad-window clears.
	xlog.SetLevel(slog.LevelDebug)
	node.activator.ClearBad()
	node.sweepConnectors()
	if !strings.Contains(readLogFile(t, genPath), "wallet not reachable") {
		t.Errorf("down wallet drop missing at Debug level")
	}

	// Transition attribution: on recovery the Info line names the mover.
	ltcDown.Store(false)
	node.activator.ClearBad()
	node.sweepConnectors()
	if conns := node.cfg().Connectors; len(conns) != 2 {
		t.Fatalf("connectors after sweep (LTC up) = %v, want {BTC LTC}", conns)
	}
	if gen := readLogFile(t, genPath); !strings.Contains(gen, "added=LTC") {
		t.Errorf("sweep Info line must name the recovered coin")
	}

	// And on re-failure it names the dropped coin.
	ltcDown.Store(true)
	node.activator.ClearBad()
	node.sweepConnectors()
	if gen := readLogFile(t, genPath); !strings.Contains(gen, "removed=LTC") {
		t.Errorf("sweep Info line must name the dropped coin")
	}
}
