package api

// Retry-cadence tests: due claim/deposit rebuilds must fire on a fast
// re-evaluation cadence, not only on the 60 s engine tick.
//
// Live-proven cost (run-3 matrix, 2026-09-21): every failed build waited its
// full backoff (60-120 s) PLUS up to 60 s of tick quantization (+11 to +46 s
// observed per wait, ~174 s total) — while the missing counterparty tx was
// often already visible. The retry sweeps are due-gated (retryAt == 0 or in
// the future means skip with zero RPC), so evaluating them more often cannot
// add a single wallet call: attempts still fire exactly when the backoff
// allows. These tests pin that contract.

import (
	"testing"
	"time"

	"go-xbridge/proto"
)

// awaitRetries polls an engine-owned counter until it reaches want or the
// deadline passes. Polling (not channels) because the firing path runs
// through the engine goroutine + worker pool on its own schedule.
func awaitRetries(t *testing.T, n *Node, get func() uint32, want uint32, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := readOnEngine(t, n, get); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s retries did not reach %d within 5s", what, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startWithFastRetry boots the node's engine with the retry re-evaluation
// interval shrunk so tests observe due-firings in milliseconds instead of
// minutes. The production value is restored on cleanup.
func startWithFastRetry(t *testing.T, n *Node) {
	t.Helper()
	old := retryCheckInterval
	retryCheckInterval = 100 * time.Millisecond
	t.Cleanup(func() { retryCheckInterval = old })
	n.conn = &captureXConn{}
	n.start()
	t.Cleanup(func() { _ = n.Close() })
}

// TestFastTickerFiresDueClaimRetry: a scheduled claim retry whose backoff has
// elapsed must rebuild without any 60 s tick running — the fast re-evaluation
// path fires it. The backend stays blind, so the rebuild fails again and
// reschedules (retries 1 -> 2): the assertion counts attempts, not success.
func TestFastTickerFiresDueClaimRetry(t *testing.T) {
	_, takerNode, _, takerSession, hub, orderID, makerPayTxID, _, _ := blindTakerFixture(t)
	if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID}); err == nil {
		t.Fatal("blind-backend ConfirmB unexpectedly succeeded")
	}
	if got := takerSession.claimRetries; got != 1 {
		t.Fatalf("claimRetries = %d, want 1 (scheduled once)", got)
	}
	startWithFastRetry(t, takerNode)
	// Restamp the retry near-term through the engine (session is engine-owned
	// once started).
	takerNode.submit(func() { takerSession.claimRetryAt = NowMicro() + 200000 }, true)
	// No tickStages anywhere on this path: only the fast ticker may fire it.
	awaitRetries(t, takerNode, func() uint32 { return takerSession.claimRetries }, 2, "claim")
	if got := readOnEngine(t, takerNode, func() string { return takerSession.claimTxID }); got != "" {
		t.Fatalf("claimTxID = %q, want empty (backend still blind: retry, not success)", got)
	}
}

// TestFastTickerFiresDueDepositRetry mirrors the claim half for the deposit
// slot: a scheduled taker deposit rebuild whose backoff elapsed must rebuild
// without any 60 s tick.
func TestFastTickerFiresDueDepositRetry(t *testing.T) {
	_, _, makerSession, takerSession, hub, orderID, createdA, _, _ := blindDepositFixture(t)
	_, _, err := takerSession.OnCreateB(&proto.CreateBBody{HubAddress: hub, ID: orderID,
		APubKey: makerSession.pubkey(), ADepositTxID: createdA.ADepositTxID,
		HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime})
	if err == nil {
		t.Fatal("blind-backend CreateB unexpectedly succeeded")
	}
	takerNode := takerSession.n
	if got := takerSession.depositRetries; got != 1 {
		t.Fatalf("depositRetries = %d, want 1 (scheduled once)", got)
	}
	startWithFastRetry(t, takerNode)
	takerNode.submit(func() { takerSession.depositRetryAt = NowMicro() + 200000 }, true)
	awaitRetries(t, takerNode, func() uint32 { return takerSession.depositRetries }, 2, "deposit")
	if got := readOnEngine(t, takerNode, func() string { return takerSession.ourDepositTxID }); got != "" {
		t.Fatalf("ourDepositTxID = %q, want empty (backend still blind: retry, not success)", got)
	}
}

// TestFastPlusSlowTickSingleFire: after the fast path fires a due retry (and
// the failure reschedules it into the future), a slow-tick evaluation of the
// same sweeps must NOT attempt a second build — the consume-on-fire makes
// double-drive structurally impossible.
func TestFastPlusSlowTickSingleFire(t *testing.T) {
	_, takerNode, _, takerSession, hub, orderID, makerPayTxID, _, _ := blindTakerFixture(t)
	if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID}); err == nil {
		t.Fatal("blind-backend ConfirmB unexpectedly succeeded")
	}
	startWithFastRetry(t, takerNode)
	takerNode.submit(func() { takerSession.claimRetryAt = NowMicro() + 200000 }, true)
	awaitRetries(t, takerNode, func() uint32 { return takerSession.claimRetries }, 2, "claim")
	// Slow-tick equivalent, serialized on the engine like the real tick.
	takerNode.submit(func() {
		takerNode.retryFailedClaimBuilds(NowMicro())
		takerNode.retryFailedDepositBuilds(NowMicro())
	}, true)
	if got := readOnEngine(t, takerNode, func() uint32 { return takerSession.claimRetries }); got != 2 {
		t.Fatalf("claimRetries = %d, want still 2 (slow tick must not re-fire)", got)
	}
}
