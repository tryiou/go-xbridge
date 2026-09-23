package api

// Take auto-retry on crNotAccepted rejects.
//
// A hub's processTransactionAccepting validates the take against ITS OWN
// wallets with a ±1-block height tolerance (xbridgesession.cpp:975-1014).
// When a coin's block cadence is fast (PIVX minted blocks every ~16-30s in
// the live campaign), the hub's tip can drift 2+ blocks from the taker's
// reported height in the seconds between the taker's blockContext fetch and
// the hub's packet processing, and the take is rejected with crNotAccepted
// (reason 8) — a pure race, not a wire or encoding fault (the reported hash
// had been the hub's own tip seconds earlier; confirmed in our service-node
// logs 2026-09-23, "out of bounds block height for <PIVX>"). C++ takers do
// not retry: processTransactionReject merely restores the order to trPending
// (xbridgesession.cpp:3579), so every network client eats the ~9% reject
// rate observed over 54 takes.
//
// go-xbridge retries a locally-initiated take that was rejected with
// crNotAccepted: the reject handler schedules a fresh TakeOrder with the
// original parameters after a short backoff — the retry re-fetches the block
// context, re-selects the fee and funding utxos, and re-broadcasts a NEW
// Accepting packet. That is wire-compatible: every early reject path on the
// hub runs BEFORE trPending->setAccepting(true) (xbridgesession.cpp:981-1014
// precede :1032), so the hub processes the re-take like any first attempt.
// A rejected take burns no service-node fee (the hub broadcasts the fee tx
// only after acceptTransaction succeeds, xbridgesession.cpp:1255-1269), so
// retries are fee-free up to the cap; a swap that DOES progress pays the
// same single fee the first successful take would have paid.
//
// The feature is gated on Config.TakeRetry (max retries after the initial
// take; 0 disables). Every retry is logged with its attempt number; when the
// cap is exhausted the order is left open exactly as C++ leaves it and the
// entry is dropped. Deterministic rejects (bad utxo, dust, bad fee tx, …)
// never retry — they drop the entry. Entries are in-memory only: a restart
// drops pending retries (matching C++ no-retry-after-restart); the knob
// itself (Config.TakeRetry) survives reloads and restarts.

import (
	"time"

	xlog "go-xbridge/log"
)

// takeRetryBackoff is the delay before each retry attempt. Index i serves
// attempt i+1 (the first retry fires after Backoff[0]); the last value
// serves every attempt beyond the list. Package-level so tests can shorten
// it (restore with t.Cleanup).
var takeRetryBackoff = []time.Duration{20 * time.Second, 40 * time.Second}

// takeRetryBackoffFor returns the delay for attempt n (1-based): entry n-1,
// or the last entry for attempts beyond the list. An emptied list (only a
// test override can do that) yields no delay instead of panicking on
// index -1.
func takeRetryBackoffFor(attempt int) time.Duration {
	if len(takeRetryBackoff) == 0 {
		return 0
	}
	if attempt-1 < len(takeRetryBackoff) {
		return takeRetryBackoff[attempt-1]
	}
	return takeRetryBackoff[len(takeRetryBackoff)-1]
}

// takeRetryState tracks one locally-taken order eligible for crNotAccepted
// retries: the original TakeOrderParams (a retry re-runs the FULL take path
// — fresh block context, fresh fee/funding selection, fresh reservation) and
// the number of retries already scheduled.
type takeRetryState struct {
	params   TakeOrderParams
	attempts int
}

// registerTakeRetry records the params of a take that just left the wire, so
// a later crNotAccepted reject can re-run it. Engine goroutine (commitTake).
// Create-if-absent: a retry's own TakeOrder passes through commitTake again
// and must not reset the attempt counter.
func (n *Node) registerTakeRetry(idHex string, p TakeOrderParams) {
	// Snapshot under the config read lock: reloadConf swaps n.config, so a
	// direct read races it (engine + retry goroutines vs hot-reload).
	cfg := n.cfg()
	if cfg == nil || cfg.TakeRetry <= 0 {
		return
	}
	n.takeRetriesMu.Lock()
	defer n.takeRetriesMu.Unlock()
	if n.takeRetries == nil {
		n.takeRetries = map[string]*takeRetryState{}
	}
	if _, ok := n.takeRetries[idHex]; !ok {
		n.takeRetries[idHex] = &takeRetryState{params: p}
	}
}

// dropTakeRetry removes a pending retry entry (reject exhausted or
// deterministic, order cancelled, retry re-ran to completion, tick prune).
func (n *Node) dropTakeRetry(idHex string) {
	n.takeRetriesMu.Lock()
	defer n.takeRetriesMu.Unlock()
	if n.takeRetries != nil {
		delete(n.takeRetries, idHex)
	}
}

// scheduleTakeRetry arms one retry after a crNotAccepted reject of a local
// take. Runs on the engine goroutine (handleRemoteReject). The entry lookup
// is the local-take gate: registerTakeRetry only records takes this node
// committed (remote takes never register).
func (n *Node) scheduleTakeRetry(idHex string) {
	// Snapshot under the config read lock (see registerTakeRetry).
	cfg := n.cfg()
	if cfg == nil || cfg.TakeRetry <= 0 {
		return
	}
	maxRetries := cfg.TakeRetry
	n.takeRetriesMu.Lock()
	st, ok := n.takeRetries[idHex]
	if !ok {
		n.takeRetriesMu.Unlock()
		return
	}
	if st.attempts >= maxRetries {
		attempts := st.attempts
		n.takeRetriesMu.Unlock()
		n.dropTakeRetry(idHex)
		xlog.Error("take retry: exhausted after crNotAccepted, leaving order open",
			"order", idHex, "attempts", attempts)
		return
	}
	st.attempts++
	attempt := st.attempts
	params := st.params
	n.takeRetriesMu.Unlock()

	backoff := takeRetryBackoffFor(attempt)
	xlog.Warn("take rejected with crNotAccepted, retrying with fresh block context",
		"order", idHex, "attempt", attempt, "max", maxRetries,
		"backoffSec", int(backoff/time.Second))
	go func() {
		// NewTimer (not AfterFunc/After): the timer is stopped on the stop
		// path so teardown does not leave it live until it fires.
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-n.stop:
			return
		}
		n.runTakeRetry(idHex, params, attempt)
	}()
}

// runTakeRetry re-runs the take once. A parked timer only fires for a take
// still eligible: the order must still be a live open local order (a remote
// cancel, our own cancel, or an expiry between the reject and the timer ends
// the retry), and the entry must still exist. On success the entry is KEPT:
// the retry take left the wire and can itself be rejected, so the next
// reject (or the tick prune, once the order progresses past accepting)
// drives what happens next. On a pre-broadcast rpcError the next attempt is
// scheduled while any remain — a local failure broadcast nothing, so no
// reject will ever arrive to drive the next attempt.
func (n *Node) runTakeRetry(idHex string, params TakeOrderParams, attempt int) {
	n.takeRetriesMu.Lock()
	_, alive := n.takeRetries[idHex]
	n.takeRetriesMu.Unlock()
	if !alive {
		return
	}
	o := n.store.Get(idHex)
	if o == nil || o.Status != "open" {
		// Mine is not re-checked here: the reject restore resets it (C++
		// isLocal() derives from from/to, cleared by the reject), so the entry
		// itself — registered only for takes this node committed — is the
		// local-take proof.
		n.dropTakeRetry(idHex)
		xlog.Info("take retry skipped: order no longer open", "order", idHex, "attempt", attempt)
		return
	}
	res, rerr := n.TakeOrder(params)
	if rerr == nil {
		// The retry take left the wire and can itself be rejected — keep the
		// entry (commitTake's register is create-if-absent, so the attempt
		// counter survives) and let the next reject / the tick prune drive
		// what happens next.
		xlog.Info("take retry broadcast", "order", idHex, "attempt", attempt, "status", res.Status)
		return
	}
	switch rerr.Code {
	case errBadRequest, errTxNotFound, errInvalidState:
		// The order is being taken/cancelled elsewhere: a retry is pointless.
		n.dropTakeRetry(idHex)
		xlog.Info("take retry abandoned: order no longer takeable", "order", idHex,
			"attempt", attempt, "code", rerr.Code, "err", rerr.Error)
		return
	}
	// Snapshot under the config read lock before taking the entry lock
	// (see registerTakeRetry; never nest cfgMu inside takeRetriesMu).
	max := 0
	if cfg := n.cfg(); cfg != nil {
		max = cfg.TakeRetry
	}
	n.takeRetriesMu.Lock()
	st, ok := n.takeRetries[idHex]
	if !ok || max <= 0 || st.attempts >= max {
		n.takeRetriesMu.Unlock()
		if ok {
			n.dropTakeRetry(idHex)
			xlog.Error("take retry failed and cap exhausted, leaving order open",
				"order", idHex, "attempt", attempt, "code", rerr.Code, "err", rerr.Error)
		}
		return
	}
	st.attempts++
	next := st.attempts
	n.takeRetriesMu.Unlock()
	backoff := takeRetryBackoffFor(next)
	xlog.Warn("take retry failed before broadcast, scheduling next attempt",
		"order", idHex, "attempt", attempt, "next", next, "code", rerr.Code, "err", rerr.Error,
		"backoffSec", int(backoff/time.Second))
	go func() {
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-n.stop:
			return
		}
		n.runTakeRetry(idHex, params, next)
	}()
}

// pruneTakeRetries drops retry entries whose order is no longer waiting on a
// possible reject: gone from the book, cancelled, expired, or progressed past
// the accepting phase (hold applied etc. — a reject can no longer arrive).
// Runs on the engine tick with the other guards (engine.go tickStages).
// Lock order: takeRetriesMu is a leaf — store.Get under it never takes
// takeRetriesMu (drops happen after Update returns), so no inversion with the
// store lock is possible.
func (n *Node) pruneTakeRetries() {
	n.takeRetriesMu.Lock()
	defer n.takeRetriesMu.Unlock()
	for idHex := range n.takeRetries {
		o := n.store.Get(idHex)
		if o == nil || (o.Status != "open" && o.Status != "accepting") {
			delete(n.takeRetries, idHex)
		}
	}
}
