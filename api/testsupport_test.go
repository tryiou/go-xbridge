// Package api shared test fixtures.
//
// Canonical locations (do NOT redefine these elsewhere):
//   - stubConn / stubErr / errStub / newWalletTestCtx: wallet_methods_test.go
//   - fakeConnector / newTestNode / base58 helpers: swap_test.go
//   - captureXConn / blockStub / mustPub / hexPub: node_test.go
//   - newHubNode family / hubKey / blkUtxo: order_hub_test.go
//
// This file holds only cross-cutting helpers that had no home: deterministic
// txid fixtures (replacing bare strings.Repeat("aa",32) magic), a
// poll-until-condition helper (replacing raw time.Sleep polls), an rpcError
// code assertion, and the shared alignment confs. New tests should use these;
// old tests migrate on contact.
package api

import (
	"strings"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
)

// testTxID returns a deterministic 64-hex-char display txid consisting of 32
// repetitions of the given two-char hex byte (e.g. testTxID("aa")). It
// replaces bare strings.Repeat("aa", 32) magic scattered across api tests so
// fixtures read as fixtures, not inline encoding trivia.
func testTxID(hexByte string) string {
	return strings.Repeat(hexByte, 32)
}

// pollUntil repeatedly evaluates cond every interval until it returns true or
// timeout elapses. It fails the test on timeout, reporting msg. Prefer this
// over hand-rolled for { time.Sleep(...); if ... } loops, which are flake
// prone on loaded CI: the condition drives readiness, not a fixed sleep.
func pollUntil(t *testing.T, timeout, interval time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(interval)
	}
	if !cond() {
		t.Fatalf("pollUntil timeout (%s): %s", timeout, msg)
	}
}

// requireErrCode fails the test unless err is non-nil with the expected RPC
// code. It collapses the repetitive `if rerr == nil || rerr.Code != want`
// blocks in hub/order tests into one line with a readable message.
func requireErrCode(t *testing.T, err *rpcError, want int, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected rpc error code %d, got nil", context, want)
	}
	if err.Code != want {
		t.Fatalf("%s: error code = %d, want %d (error %q)", context, err.Code, want, err.Error)
	}
}

func alignConfs() map[string]*config.CoinConf {
	return map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60, FeePerByte: 2},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60, FeePerByte: 2},
	}
}

func alignInitCoins(t *testing.T) {
	t.Helper()
	if err := coins.InitFromConf(alignConfs()); err != nil {
		t.Fatal(err)
	}
}
