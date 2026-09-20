package api

import (
	xlog "go-xbridge/log"
)

// Phase-1 R3 hub-silence watchdog + R4 TTL guard and lock release.
//
// R3: the handshake is 100% hub-redelivery-driven — a dead hub stalls a
// session until its deposit locktime with no local action. The watchdog
// cancels sessions silent past the stall threshold (counterparty notified via
// wire Cancel, funds recovered via the pre-signed refund sweep) instead.
// Sessions with a built-but-unbroadcast claim are left alone: completing them
// is Phase-2 auto-complete territory, and a cancel there could only harm.
//
// R4a: a "rolled back" order's funding is spent (refund broadcast succeeded),
// so its UTXO locks are released for future funding selection. "rollback
// failed" stays locked — the refund never went out, funds may still sit in
// the P2SH.
//
// R4b: Mine orders stuck non-terminal with no session, no deposit, and
// nothing tracked are dropped past the TTL (locks released, book cleaned).
// Anything owned by the R1/R2 sweeps is never touched.

// sessionStallMicro is the hub-silence period before a live session is
// canceled: far beyond a healthy swap (minutes) and hub redelivery (~45s).
const sessionStallMicro = 30 * 60 * 1000000

// sessionStallWarnMicro is the early-visibility threshold: a live session with
// no observable progress for this long gets a "no hub progress" WARN naming
// the hub — long before the 30-minute cancel. Between "swap session created"
// and the stall cancel the operator otherwise sees NOTHING from a hub that
// silently dropped the handshake (a hub rejects a take on its side and
// addresses the reject to the taker only, so the maker never learns why
// nothing arrives). Progress resets the warning; the cancel threshold is
// untouched (observation only, no behavior change).
const sessionStallWarnMicro = 5 * 60 * 1000000

// mineOrderTTLMicro is the age past which a Mine limbo order (no session, no
// deposit, nothing tracked) is dropped.
const mineOrderTTLMicro = 24 * 60 * 60 * 1000000

// watchStalledSessions cancels live sessions with no observable progress
// past the stall threshold. Runs on the engine goroutine; inline mode
// (tests) acts synchronously. Skips: finished, refund-done (nothing left),
// in-flight work (await — bounded by RPC timeouts), and built-but-unbroadcast
// claims (Phase-2 owns those).
func (n *Node) watchStalledSessions() {
	now := uint64(NowMicro())
	if n.stallWarned == nil {
		n.stallWarned = map[string]uint64{} // lazily built nodes (tests) never ran start()
	}
	for id, s := range n.sessions {
		// A hunting session recovers through the deposit watch, not the
		// hub: cancelling it would force-broadcast the same impossible
		// refund the hunt was armed to avoid (the input is spent).
		if s.state == csFinished || s.refundDone || s.await || s.claimHex != "" || s.secretHunt {
			continue
		}
		if s.lastProgress == 0 {
			continue // unstamped (test-built) session: never fire
		}
		silent := uint64(0)
		if now >= s.lastProgress {
			silent = now - s.lastProgress
		}
		// Early WARN, once per silent period (a new silence = a new
		// lastProgress value). Progress resumed since the last warn drops the
		// entry so a later stall warns again.
		if silent > sessionStallWarnMicro {
			if n.stallWarned[id] != s.lastProgress {
				n.stallWarned[id] = s.lastProgress
				status := ""
				if o := n.store.Get(id); o != nil {
					status = o.Status
				}
				xlog.Warn("swap session: no hub progress", "order", id,
					"role", map[bool]string{true: "maker", false: "taker"}[s.isMaker],
					"state", s.state.String(), "hub", hexEncode(s.hubKey[:]),
					"silentSec", silent/1000000, "orderStatus", status)
			}
		} else {
			delete(n.stallWarned, id)
		}
		if silent <= sessionStallMicro {
			continue
		}
		// Already resolved or cooling down: an earlier fire rolled the
		// order back (re-firing would re-cancel + re-enqueue every tick,
		// bypassing the refund backoff), the counterparty already redeemed
		// (cancel is locally ignored but still broadcasts every tick), or
		// a failed refund is inside its backoff window.
		if o := n.store.Get(id); o != nil &&
			(isOrderTerminal(o.Status) || o.Status == "rolled back" ||
				o.Status == "canceled" || o.CounterpartyRedeemed) {
			continue
		}
		if n.refundBackoffActive(id) {
			continue
		}
		xlog.Warn("stall watchdog: hub silent, canceling swap", "order", id,
			"state", s.state.String(), "silentSec", (now-s.lastProgress)/1000000)
		s.sendSelfCancel(crTimeout)
	}
}

// gcStaleMineOrders drops ancient Mine limbo orders no sweep owns: no live
// session, no verified deposit, nothing tracked unconfirmed, Updated past the
// TTL. Runs on the engine goroutine; inline mode (tests) acts synchronously.
func (n *Node) gcStaleMineOrders() {
	now := uint64(NowMicro())
	trackedOrders := map[string]bool{}
	n.trackedMu.Lock()
	for _, tb := range n.tracked {
		if tb.Confs == 0 {
			trackedOrders[tb.OrderID] = true
		}
	}
	n.trackedMu.Unlock()
	for _, o := range n.store.List() {
		idHex := hexEncode(o.ID[:])
		if !o.Mine || isOrderTerminal(o.Status) || o.DepositSent {
			continue
		}
		if _, live := n.sessions[idHex]; live {
			continue
		}
		if trackedOrders[idHex] {
			continue // R1 owns it
		}
		if now < o.Updated || now-o.Updated <= mineOrderTTLMicro {
			continue
		}
		xlog.Warn("ttl guard: dropping ancient limbo order", "order", idHex, "status", o.Status)
		n.store.MoveToHistory(idHex, "dropped", uint64(crTimeout), now)
	}
}

// orderLockReleased reports whether an order's UTXO locks are released for
// funding selection: terminal orders plus rolled-back ones (refund broadcast
// succeeded, so the funding is spent and the locks are dead weight). A
// "rollback failed" order stays locked — its refund never went out.
func orderLockReleased(status string) bool {
	if isOrderTerminal(status) {
		return true
	}
	return statusString(status) == "rolled back"
}
