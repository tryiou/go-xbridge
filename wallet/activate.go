package wallet

import (
	"fmt"
	"sync"
	"time"

	"go-xbridge/config"
)

// Drop records a currency that was not activated and why.
type Drop struct {
	Ticker string
	Reason string
}

// badWalletRetryInterval is how long a wallet that failed its reachability
// probe is left alone before the next probe (C++ m_badWallets 300s wait,
// xbridgeapp.cpp:963-971). Overridable in tests.
var badWalletRetryInterval = 5 * time.Minute

// probeTimeout bounds each reachability probe (a wallet RPC round-trip) so a
// hung wallet cannot wedge the activation/reload/sweep path indefinitely.
// Overridable in tests.
var probeTimeout = 5 * time.Second

// Activator builds and maintains the active wallet connector set, mirroring
// C++ App::updateActiveWallets (xbridgeapp.cpp:917-1214): only the
// [Main].ExchangeWallets currencies are connected; each passes the static
// admission gates (config.Admit, xbridgeapp.cpp:1002-1040) and, when enabled,
// a live reachability probe. A wallet that fails the probe is recorded as bad
// and skipped for badWalletRetryInterval, so a down wallet is not re-probed on
// every sweep (C++ m_badWallets, :963-971). C++ probes concurrently with
// -rpcthreads workers (:1118-1121); go-xbridge probes sequentially — a few
// wallets, each bounded by probeTimeout — a documented thin-client
// simplification.
type Activator struct {
	mu  sync.Mutex
	bad map[string]time.Time // currency -> time of last failed probe

	// Probe checks a built connector's wallet reachability. It defaults to a
	// GetBlockCount round-trip — the thin-client analog of C++ conn->init(),
	// which performs a wallet getInfo (xbridgewalletconnectorbtc.cpp:1513-1525).
	// Note the divergence: a wallet that answers getblockcount but otherwise
	// misbehaves would pass the probe, where C++ init() also validates the
	// relay-fee/mediantime fields. Overridable in tests and embeddings.
	Probe func(Connector) error
}

func NewActivator() *Activator {
	return &Activator{bad: map[string]time.Time{}, Probe: probeReachable}
}

// ClearBad drops all bad-wallet timestamps. C++ clears them on an explicit
// dxLoadXBridgeConf so a user-requested update re-probes every wallet
// (rpcxbridge.cpp:233 clearBadWallets).
func (a *Activator) ClearBad() {
	a.mu.Lock()
	a.bad = map[string]time.Time{}
	a.mu.Unlock()
}

// badSince returns the recorded last-probe-failure time for currency, if any.
func (a *Activator) badSince(currency string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.bad[currency]
	return t, ok
}

func (a *Activator) markBad(currency string) {
	a.mu.Lock()
	a.bad[currency] = time.Now()
	a.mu.Unlock()
}

func (a *Activator) clearBad(currency string) {
	a.mu.Lock()
	delete(a.bad, currency)
	a.mu.Unlock()
}

// Activate returns the connector set for the ExchangeWallets currencies that
// pass the static gates and, when checkReachability is set, the live probe.
// Drops records every currency not activated and why, in ExchangeWallets
// order. When checkReachability is false no probe runs and no bad-wallet state
// is consulted or written (the hermetic test path).
func (a *Activator) Activate(confs map[string]*config.CoinConf, exchangeWallets []string, checkReachability bool) (map[string]Connector, []Drop) {
	connectors := map[string]Connector{}
	var drops []Drop
	probe := a.Probe
	if probe == nil {
		probe = probeReachable
	}

	for _, sym := range exchangeWallets {
		cc := confs[sym]
		if cc == nil {
			drops = append(drops, Drop{Ticker: sym, Reason: "not found in config"})
			continue
		}
		if err := config.Admit(cc); err != nil {
			drops = append(drops, Drop{Ticker: sym, Reason: err.Error()})
			continue
		}
		if checkReachability {
			if t, ok := a.badSince(sym); ok {
				if time.Since(t) < badWalletRetryInterval {
					drops = append(drops, Drop{Ticker: sym, Reason: "bad wallet, retry pending"})
					continue
				}
				a.clearBad(sym) // retry window elapsed: allow a re-probe
			}
		}
		conn, err := NewConnectorFromConf(cc)
		if err != nil {
			drops = append(drops, Drop{Ticker: sym, Reason: err.Error()})
			continue
		}
		if checkReachability {
			if err := probe(conn); err != nil {
				a.markBad(sym)
				drops = append(drops, Drop{Ticker: sym, Reason: "wallet not reachable: " + err.Error()})
				continue
			}
			a.clearBad(sym)
		}
		connectors[sym] = conn
	}
	return connectors, drops
}

// probeReachable is the default reachability probe: a getblockcount round-trip
// bounded by probeTimeout (the thin-client analog of C++ conn->init()).
func probeReachable(c Connector) error {
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := c.GetBlockCount()
		done <- result{err}
	}()
	select {
	case r := <-done:
		return r.err
	case <-time.After(probeTimeout):
		return fmt.Errorf("probe timed out after %s", probeTimeout)
	}
}
