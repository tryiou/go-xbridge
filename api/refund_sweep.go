package api

import (
	"encoding/hex"

	"go-xbridge/coins"
	xlog "go-xbridge/log"
)

// Phase-1 R2: session-less refund sweep + rollback backoff.
//
// G12: stored Mine orders with a verified deposit (DepositSent) and a
// pre-signed refund (RefundTx) but NO live session (restart, prune) were never
// swept — scanRefunds only iterates live sessions, and the stored path
// (enqueueRefund/tryStoredRefund) only ran on manual BroadcastRefund. The
// sweep below closes that hole with the exact complement guards.
//
// Backoff: every sweep used to re-broadcast a failing refund (live-proven log
// spam on terminal sessions). Failures now schedule an escalating retry
// (10min doubling, 24h cap); manual BroadcastRefund always bypasses the gate.

// refundBackoffBaseMicro is the first retry delay after a failed refund.
const refundBackoffBaseMicro = 10 * 60 * 1000000

// refundBackoffCapMicro caps the escalating retry delay.
const refundBackoffCapMicro = 24 * 60 * 60 * 1000000

// refundBackoffActive reports whether orderID is inside a failure backoff
// window. Engine-side (sweeps) and inline tests only.
func (n *Node) refundBackoffActive(idHex string) bool {
	retryAt, ok := n.refundRetryAt[idHex]
	return ok && uint64(NowMicro()) < retryAt
}

// recordRefundFailure schedules the next retry with doubling delay
// (10min, 20min, 40min, … capped at 24h). Engine-side (postRefundTask apply)
// and inline tests only.
func (n *Node) recordRefundFailure(idHex string) {
	if n.refundAttempts == nil {
		n.refundAttempts = make(map[string]int)
	}
	if n.refundRetryAt == nil {
		n.refundRetryAt = make(map[string]uint64)
	}
	a := n.refundAttempts[idHex] + 1
	n.refundAttempts[idHex] = a
	delay := uint64(refundBackoffBaseMicro)
	for i := 1; i < a && delay < refundBackoffCapMicro; i++ {
		delay *= 2
	}
	if delay > refundBackoffCapMicro {
		delay = refundBackoffCapMicro
	}
	n.refundRetryAt[idHex] = uint64(NowMicro()) + delay
}

// clearRefundBackoff drops the retry schedule after a successful refund.
// Engine-side and inline tests only.
func (n *Node) clearRefundBackoff(idHex string) {
	delete(n.refundAttempts, idHex)
	delete(n.refundRetryAt, idHex)
}

// scanStoredRefunds sweeps stored Mine orders with no live session and
// auto-broadcasts any pre-signed refund whose deposit lockTime has passed.
// The locktime gate runs inside the worker (runRefundTask with checkLock), so
// the engine never blocks on wallet I/O; the refund chain comes from the
// order's UtxoCurrency tag (our funding chain = our deposit chain). Runs on
// the engine goroutine; inline mode (tests) broadcasts synchronously.
func (n *Node) scanStoredRefunds() {
	dirty := false
	for _, o := range n.store.List() {
		idHex := hexEncode(o.ID[:])
		if !o.Mine || o.RefundTx == "" || !o.DepositSent {
			continue
		}
		// "finished" never owes a refund: the deposit was claimed by the
		// counterparty, and broadcasting our refund could only fail. Dropped /
		// invalid orders died before any deposit existed. "canceled" MUST be
		// swept: C++ refunds trCancelled transactions in its redeem scan, and
		// the live 2026-09-15 stall-watchdog cancel (order a4198f2d…) left a
		// canceled record with the deposit locked in the P2SH — skipping it
		// stranded the funds.
		switch statusString(o.Status) {
		case "finished", "dropped", "invalid":
			continue
		case "rolled back":
			continue // refund already went out; unconfirmed retries are
			// owned by the rebroadcast sweep while the tx stays tracked
			// (pruning only drops deep-confirmed entries)
		}
		if _, live := n.sessions[idHex]; live {
			continue // the session sweep owns it
		}
		if n.refundBackoffActive(idHex) {
			continue
		}
		// Single decode: the txid derives from the same bytes the locktime
		// is read from, so an undecodable hex skips before any chain use.
		raw, derr := hex.DecodeString(o.RefundTx)
		if derr != nil {
			xlog.Warn("stored refund bad hex, skipping sweep", "order", idHex)
			continue
		}
		rtx, derr := coins.Deserialize(raw)
		if derr != nil {
			xlog.Warn("stored refund undecodable, skipping sweep", "order", idHex)
			continue
		}
		refundTxid := txIDFromBytes(raw)
		n.trackedMu.Lock()
		tb, watched := n.tracked[refundTxid]
		n.trackedMu.Unlock()
		if watched {
			// Already broadcast (tracked at any depth): never re-post
			// from here — unconfirmed retries belong to the rebroadcast
			// sweep, which re-sends the identical bytes on its own
			// cadence. Reconcile the state once confirmed.
			if tb.Confs >= 1 && n.rollbackGate(idHex) {
				if n.store.Update(idHex, func(u *Order) {
					// Canceled records flip to "rolled back" too: C++'s
					// redeemOrderDeposit sets trRollback from trCancelled
					// (:3911) — the refund just confirmed on-chain.
					if !isOrderTerminal(u.Status) || u.Status == "canceled" {
						if u.Status != "rolled back" {
							u.Status = "rolled back"
							u.Updated = NowMicro()
						}
					}
				}) {
					dirty = true
				}
			}
			n.clearRefundBackoff(idHex)
			continue
		}
		coin := utxoCurrency(o)
		if coin == "" || n.cfg().Connectors[coin] == nil {
			continue
		}
		if n.engineRunning.Load() {
			// Started mode: guard against double-enqueue, mirroring
			// scanRefunds. Inline mode runs synchronously (redundant).
			if n.pendingRefunds[idHex] {
				continue
			}
			n.pendingRefunds[idHex] = true
		}
		n.postRefundTask(idHex, coin, o.RefundTx, rtx.LockTime, true, nil)
	}
	if dirty {
		n.persist()
	}
}
