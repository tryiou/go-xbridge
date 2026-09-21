package wallet

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-xbridge/config"
)

// admitConf returns a coin conf that passes every static gate; fields can be
// mutated per-test.
func admitConf(ticker, method string) *config.CoinConf {
	return &config.CoinConf{
		Ticker:         ticker,
		Ip:             "127.0.0.1",
		Port:           8332,
		Username:       "u",
		Password:       "p",
		CreateTxMethod: method,
		Coin:           100000000,
		BlockTime:      600,
		Confirmations:  2,
	}
}

// mockEndpoint points a CoinConf's Ip/Port at an httptest wallet server URL
// (the mockRPC helper's basic-auth getblockcount handler).
func mockEndpoint(c *config.CoinConf, rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	host := u.Host
	idx := strings.LastIndexByte(host, ':')
	c.Ip = host[:idx]
	c.Port = 0
	for i := idx + 1; i < len(host); i++ {
		c.Port = c.Port*10 + int(host[i]-'0')
	}
}

// TestActivateExchangeWalletsOnly locks the ExchangeWallets keying: exactly the
// currencies listed are connected — a conf coin not listed is never activated.
func TestActivateExchangeWalletsOnly(t *testing.T) {
	confs := map[string]*config.CoinConf{
		"BTC":  admitConf("BTC", "BTC"),
		"DOGE": admitConf("DOGE", "BTC"),
		"LTC":  admitConf("LTC", "BTC"),
	}
	a := NewActivator()
	conns, drops := a.Activate(confs, []string{"BTC", "DOGE"}, false)
	if len(conns) != 2 || conns["BTC"] == nil || conns["DOGE"] == nil {
		t.Fatalf("connectors = %v, want {BTC DOGE}", conns)
	}
	if conns["LTC"] != nil {
		t.Error("LTC (not in ExchangeWallets) was connected")
	}
	if len(drops) != 0 {
		t.Errorf("drops = %v, want none", drops)
	}
}

// TestActivateGateDrops locks the gate wiring: an ExchangeWallets coin failing
// the static gates is dropped with the gate reason.
func TestActivateGateDrops(t *testing.T) {
	confs := map[string]*config.CoinConf{
		"BAD": admitConf("BAD", "BTC"),
		"ETH": admitConf("ETH", "ETH"),
	}
	confs["BAD"].BlockTime = 1201
	a := NewActivator()
	_, drops := a.Activate(confs, []string{"BAD", "ETH"}, false)
	if len(drops) != 2 {
		t.Fatalf("drops = %v, want 2", drops)
	}
	if !strings.Contains(drops[0].Reason, "Failed maker locktime requirements") {
		t.Errorf("BAD reason = %q", drops[0].Reason)
	}
	if !strings.Contains(drops[1].Reason, "ETH") {
		t.Errorf("ETH reason = %q", drops[1].Reason)
	}
}

// TestActivateMissingConf: an ExchangeWallets ticker with no [TICKER] section
// is dropped (C++ fails to look the section up).
func TestActivateMissingConf(t *testing.T) {
	a := NewActivator()
	_, drops := a.Activate(map[string]*config.CoinConf{"BTC": admitConf("BTC", "BTC")}, []string{"NOPE"}, false)
	if len(drops) != 1 || drops[0].Ticker != "NOPE" || drops[0].Reason != "not found in config" {
		t.Fatalf("drops = %v, want NOPE not-found", drops)
	}
}

// TestActivateProbe locks the reachability probe: a wallet answering
// getblockcount connects; an unreachable endpoint is dropped and marked bad.
func TestActivateProbe(t *testing.T) {
	srv := mockRPC(t)
	defer srv.Close()

	reachable := admitConf("BTC", "BTC")
	mockEndpoint(reachable, srv.URL)
	dead := admitConf("LTC", "BTC")
	dead.Port = 1 // nothing listens on port 1

	confs := map[string]*config.CoinConf{"BTC": reachable, "LTC": dead}
	a := NewActivator()
	conns, drops := a.Activate(confs, []string{"BTC", "LTC"}, true)
	if conns["BTC"] == nil {
		t.Error("BTC (reachable wallet) not connected")
	}
	if conns["LTC"] != nil {
		t.Error("LTC (dead endpoint) connected")
	}
	if len(drops) != 1 || drops[0].Ticker != "LTC" || !strings.Contains(drops[0].Reason, "wallet not reachable") {
		t.Fatalf("drops = %v, want LTC not reachable", drops)
	}
	// LTC is now bad: an immediate re-activation skips the probe entirely.
	_, drops = a.Activate(confs, []string{"BTC", "LTC"}, true)
	if len(drops) != 1 || !strings.Contains(drops[0].Reason, "bad wallet, retry pending") {
		t.Fatalf("post-failure drops = %v, want bad-wallet skip", drops)
	}
}

// TestActivateBadWalletRetry locks the 300s bad-wallet retry window: a wallet
// is not re-probed until the window elapses, then it is.
func TestActivateBadWalletRetry(t *testing.T) {
	var probes atomic.Int32
	confs := map[string]*config.CoinConf{"BTC": admitConf("BTC", "BTC")}
	a := NewActivator()
	a.Probe = func(Connector) error {
		probes.Add(1)
		return errors.New("down")
	}

	if _, drops := a.Activate(confs, []string{"BTC"}, true); len(drops) != 1 {
		t.Fatalf("first drops = %v", drops)
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want 1", probes.Load())
	}
	// Within the window: skipped, no probe.
	if _, drops := a.Activate(confs, []string{"BTC"}, true); len(drops) != 1 || !strings.Contains(drops[0].Reason, "retry pending") {
		t.Fatalf("window drops = %v", drops)
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want still 1", probes.Load())
	}
	// Expire the window: re-probed.
	a.mu.Lock()
	a.bad["BTC"] = time.Now().Add(-badWalletRetryInterval - time.Second)
	a.mu.Unlock()
	if _, drops := a.Activate(confs, []string{"BTC"}, true); len(drops) != 1 {
		t.Fatalf("expired drops = %v", drops)
	}
	if probes.Load() != 2 {
		t.Fatalf("probes = %d, want 2 after expiry", probes.Load())
	}
}

// TestActivateClearBad locks ClearBad (C++ clearBadWallets on dxLoadXBridgeConf,
// rpcxbridge.cpp:233): it forces a re-probe of every wallet.
func TestActivateClearBad(t *testing.T) {
	var probes atomic.Int32
	confs := map[string]*config.CoinConf{"BTC": admitConf("BTC", "BTC")}
	a := NewActivator()
	a.Probe = func(Connector) error {
		probes.Add(1)
		return errors.New("down")
	}
	if _, drops := a.Activate(confs, []string{"BTC"}, true); len(drops) != 1 {
		t.Fatalf("first drops = %v", drops)
	}
	a.ClearBad()
	if _, drops := a.Activate(confs, []string{"BTC"}, true); len(drops) != 1 {
		t.Fatalf("after ClearBad drops = %v", drops)
	}
	if probes.Load() != 2 {
		t.Fatalf("probes = %d, want 2 (ClearBad forced a re-probe)", probes.Load())
	}
}

// hangingProbeConnector is a Connector stub whose reachability probe never
// returns: GetBlockCount blocks forever (the plain, non-cancellable call) and
// GetBlockCountContext blocks until its context is cancelled. returned is
// closed when the underlying call exits, so a test can observe whether the probe
// goroutine was joined after a timeout.
type hangingProbeConnector struct {
	block    chan struct{} // never closed: a plain GetBlockCount parks forever
	returned chan struct{}
}

func (c *hangingProbeConnector) Ticker() string                  { return "HANG" }
func (c *hangingProbeConnector) GetBalance() (uint64, error)     { return 0, errors.New("unused") }
func (c *hangingProbeConnector) GetNewAddress() (string, error)  { return "", errors.New("unused") }
func (c *hangingProbeConnector) ListUnspent(int) ([]Utxo, error) { return nil, errors.New("unused") }
func (c *hangingProbeConnector) SignRawTransaction(string, []PrevTx) (string, bool, error) {
	return "", false, errors.New("unused")
}
func (c *hangingProbeConnector) SendRawTransaction(string) (string, error) {
	return "", errors.New("unused")
}
func (c *hangingProbeConnector) GetRelayFee() (float64, error) { return 0, errors.New("unused") }
func (c *hangingProbeConnector) GetBlockCount() (int64, error) {
	<-c.block
	close(c.returned)
	return 0, nil
}
func (c *hangingProbeConnector) GetBlockHash(int64) ([32]byte, error) {
	return [32]byte{}, errors.New("unused")
}
func (c *hangingProbeConnector) GetBlockTxs([32]byte) ([]BlockTx, error) {
	return nil, errors.New("unused")
}
func (c *hangingProbeConnector) GetRawTransaction(string) (string, error) {
	return "", errors.New("unused")
}
func (c *hangingProbeConnector) CheckDepositTransaction(string, string, uint64, int) (DepositCheck, error) {
	return DepositCheck{}, errors.New("unused")
}
func (c *hangingProbeConnector) SignMessage(string, string) ([]byte, error) {
	return nil, errors.New("unused")
}
func (c *hangingProbeConnector) VerifyMessage(string, []byte, string) (bool, error) {
	return false, errors.New("unused")
}
func (c *hangingProbeConnector) GetTxOut(string, uint32) (Utxo, bool, error) {
	return Utxo{}, false, errors.New("unused")
}
func (c *hangingProbeConnector) GetRawTransactionVerbose(string) (VerboseTx, error) {
	return VerboseTx{}, errors.New("unused")
}
func (c *hangingProbeConnector) GetRawMempool() ([]string, error) {
	return nil, errors.New("unused")
}

// GetBlockCountContext blocks until ctx is done, then reports the context's
// error and signals that the call exited.
func (c *hangingProbeConnector) GetBlockCountContext(ctx context.Context) (int64, error) {
	<-ctx.Done()
	close(c.returned)
	return 0, ctx.Err()
}

// TestProbeGoroutineJoinedOnTimeout proves a timed-out reachability probe does
// not leak its goroutine: probeReachable runs the ctx-aware connector under a
// cancellable context, aborts the in-flight RPC on timeout, and waits for the
// probe goroutine to exit before returning. Without the ctx-aware path the
// goroutine would park in GetBlockCount and outlive the probe by the wallet RPC
// timeout. The assertion observes prompt exit (returned closed) after the
// timeout error, which is the observable effect of the ctx cancellation + join.
func TestProbeGoroutineJoinedOnTimeout(t *testing.T) {
	orig := probeTimeout
	probeTimeout = 50 * time.Millisecond
	defer func() { probeTimeout = orig }()

	c := &hangingProbeConnector{block: make(chan struct{}), returned: make(chan struct{})}
	start := time.Now()
	err := probeReachable(c)
	elapsed := time.Since(start)
	timeoutErr := err != nil && (strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "context deadline exceeded"))
	if !timeoutErr {
		t.Fatalf("probe err = %v, want timeout", err)
	}
	// The probe goroutine must exit promptly around the time probeReachable
	// returns: the context cancelled the in-flight RPC and the goroutine was
	// joined (bounded), so the deadline cannot be blown past the probe timeout.
	select {
	case <-c.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("probe goroutine not joined after timeout")
	}
	if elapsed > 5*probeTimeout {
		t.Fatalf("probe took %v, want ~%v", elapsed, probeTimeout)
	}
}
