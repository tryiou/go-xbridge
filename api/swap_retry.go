package api

// Claim and deposit rebuild: tick-driven retry of failed HTLC builds.
//
// Live-proven holes (BLOCK/LTC e6730fe2 at the claim stage, BLOCK/DOGE
// d4df334e at the deposit stage): a one-shot build fired while the EXR-routed
// backend still could not serve a fresh counterparty tx (-5 blind), failed
// once, and never retried. The hub's redelivery burst ended after seconds and
// nothing re-drove the build, stranding a fully fundable session — manual
// recovery was the only way out. The old code said "resumption comes from hub
// redelivery"; this file adds the self-driven half without touching the wire
// contract or the C++-mirrored handshake state machine:
//   - a failed build records its trigger pointers (already stage-1 persisted)
//     and a retry timestamp;
//   - retryFailedClaimBuilds / retryFailedDepositBuilds re-post the IDENTICAL
//     build on the engine tick until it succeeds or the session ends
//     (hub/user cancel still wins).
// Retries refresh lastProgress — the session is actively recovering, not
// silent — and only ever rebuild while nothing was broadcast, so a retry can
// never double-broadcast.

import (
	"fmt"

	xlog "go-xbridge/log"

	"go-xbridge/wallet"
)

const (
	// claimRetryBaseMicro is the delay before the first rebuild of a failed
	// claim build (the tick itself runs every refundCheckInterval = 60 s, so
	// the first retry lands one to two ticks after the failure — past the
	// backend-propagation lag that caused e6730fe2, which needed ~4 min).
	claimRetryBaseMicro = 60 * 1000000
	// claimRetryCapMicro bounds the exponential backoff: a backend outage
	// longer than minutes keeps retrying every 15 min instead of every tick.
	claimRetryCapMicro = 15 * 60 * 1000000
	// broadcastRepostMax caps IDENTICAL-byte reposts of a built intent
	// (mirror maxRebroadcasts in reconcile.go): past it the sweep stops and
	// alerts, staying watched. Rebuilds (which revalidate from scratch) are
	// uncapped — only blind re-sends are bounded.
	broadcastRepostMax = 10
	// awaitStuckMicro bounds a worker round-trip: RPC timeouts kill real
	// work far sooner, so an `await` older than this means the result was
	// lost (dropped queue entry, panic in apply, result after prune). Three
	// minutes sits between with full margin both sides: far beyond healthy
	// worker RPC (seconds, even to lagging EXR backends) and far below the
	// 30-minute watchdog, so a released guard re-drives long before cancel.
	awaitStuckMicro = 3 * 60 * 1000000
)

// holdAwait marks a worker task in flight with its start stamp. Every
// `await = true` site goes through here so the tick timeout can tell a live
// worker from a lost result.
func (s *SwapSession) holdAwait() {
	s.await = true
	s.awaitSince = NowMicro()
}

// releaseAwait clears the in-flight guard and its stamp together. A result
// that never arrives leaves both set — exactly what clearStuckAwait detects.
func (s *SwapSession) releaseAwait() {
	s.await = false
	s.awaitSince = 0
}

// clearStuckAwait releases worker-in-flight guards whose result never
// arrived, re-arming the stage retry. Engine-side (first in the 60 s tick so
// every downstream sweep sees a consistent guard state). It never touches
// lastProgress: the watchdog keeps judging the session's original silence,
// and any re-drive refreshes progress on its own. Fresh guards (worker
// plausibly running) are untouched.
func (n *Node) clearStuckAwait(now uint64) {
	for orderID, s := range n.sessions {
		if !s.await || s.awaitSince == 0 || now < s.awaitSince || now-s.awaitSince <= awaitStuckMicro {
			continue
		}
		xlog.Warn("stuck guard: worker result lost, releasing", "order", orderID,
			"state", s.state.String(), "stuckSec", (now-s.awaitSince)/1000000)
		s.releaseAwait()
	}
}

// retryBackoff returns the backoff after `retries` consecutive failures:
// 60 s, 120 s, 240 s, 480 s, then the 15-minute cap.
func retryBackoff(retries uint32) uint64 {
	shift := retries
	if shift > 3 {
		shift = 3
	}
	d := uint64(claimRetryBaseMicro) << shift
	if d > claimRetryCapMicro {
		d = claimRetryCapMicro
	}
	return d
}

// scheduleClaimRetry records a failed claim build for tick-driven rebuild.
// Engine-side (called from the phase-1 resumes). selfCancel aborts must NOT
// come here — failSelfCancel owns those. A persist failure keeps the
// in-memory schedule (this run still retries); hub redelivery still covers a
// crash before the flush, exactly as before.
func scheduleClaimRetry(s *SwapSession, now uint64, role string, err error) {
	orderID := hexEncode(s.id[:])
	delay := retryBackoff(s.claimRetries)
	s.claimRetries++
	s.claimRetryAt = now + delay
	s.lastProgress = now
	xlog.Warn("claim build failed, retry scheduled", "order", orderID, "role", role,
		"err", err, "retryInSec", delay/1000000, "attempt", s.claimRetries)
	if perr := s.n.persistNow(); perr != nil {
		xlog.Error("claim retry persist failed, in-memory schedule kept", "order", orderID, "err", perr)
	}
}

// clearClaimRetry drops the retry schedule after a successful build.
// Engine-side.
func clearClaimRetry(s *SwapSession) {
	s.claimRetryAt = 0
	s.claimRetries = 0
}

// makerClaimBuildTask returns the ConfirmA ELSE-branch claim BUILD task: it
// validates the taker's B deposit and builds (never broadcasts) our claim.
// Identical for the hub-driven first attempt and tick-driven rebuilds — the
// counterparty pointers come from the snapshot, which the sweep refreshes
// from the session before posting.
func makerClaimBuildTask(s *SwapSession, c swapCtx) workTask {
	orderID := hexEncode(s.id[:])
	return workTask{
		orderID: orderID,
		run: func() (any, error) {
			// Validate the counterparty (taker) B-deposit lockTime (C++
			// acceptableLockTimeDrift). Compare against our own expectation for
			// that same deposit role on the taker's coin (dstCur). In Go's
			// hub-driven flow this is the earliest the maker learns it (after
			// our own deposit is already broadcast); if it fails we must not
			// proceed to redeem.
			expLT := c.computeLockTimeFor(c.dstCur, false)
			if expLT == 0 {
				// Our own height read failed (wallet outage) — transient, never
				// a counterparty fault. Retry owns it; a genuine drift fault
				// still cancels below (S5 lesson: order 9698af09 died because
				// C++ :2985-2995 cancels on any non-VERIFY_ERROR redeem
				// failure instead of retrying).
				return nil, fmt.Errorf("api: locktime unavailable for %s, awaiting redelivery", c.dstCur)
			}
			if !acceptableLockTimeDrift(expLT, c.theirLockTime, c.blockTimeFor(c.dstCur)) {
				// C++ :2926-2935 — bad counterparty locktime → wire-Cancel.
				return nil, &selfCancelErr{reason: crBadBLockTime}
			}
			// Validate the taker's B deposit before redeeming it
			// (C++ :2957). Wait → no response (hub retransmits); bad → Cancel.
			dcheck, err := c.checkCounterpartyDeposit(c.secretHash, c.dstAmt)
			if err != nil {
				return nil, err
			}
			if !dcheck.IsGood {
				return nil, &selfCancelErr{reason: crBadBDepositTx}
			}
			c.theirDepositVout = dcheck.DepositVout
			c.theirP2SHNative = dcheck.P2SHNative
			xlog.Info("ConfirmA: redeeming taker deposit", "order", orderID, "takerDeposit", c.theirDepositTxID)
			payHex, cur, err := c.redeemCounterparty(true)
			if err != nil {
				return nil, err
			}
			xlog.Debug("ConfirmA: claim tx built", "order", orderID, "cur", cur)
			// Fail fast when the spending chain has no connector, but do NOT
			// broadcast here: the engine persists the claim intent first and
			// the broadcast runs as a separate gated step. Derive the payTx id
			// locally (authoritative, like the deposit txid); the wallet's
			// reported id is only ever logged, never adopted.
			wconn, e := c.connector(cur)
			if e != nil {
				return nil, e
			}
			localPayID, err := txIDFromHex(payHex)
			if err != nil {
				return nil, err
			}
			return confirmOutcome{payTxID: localPayID, payHex: payHex, cur: cur, check: dcheck, conn: wconn}, nil
		},
		apply: func(v any, terr error) { s.applyConfirmedA(v, terr) },
	}
}

// takerClaimBuildTask returns the ConfirmB ELSE-branch claim BUILD task: it
// recovers the secret from the maker's payTx and builds (never broadcasts)
// our claim. payTxID is the hub-supplied APayTxID on the first attempt and
// the persisted theirPayTxID on tick-driven rebuilds.
func takerClaimBuildTask(s *SwapSession, c swapCtx, payTxID string) workTask {
	orderID := hexEncode(s.id[:])
	return workTask{
		orderID: orderID,
		run: func() (any, error) {
			// Recover the 33-byte secret preimage from the maker's payTx.
			conn := c.connectors[c.srcCur]
			if conn == nil {
				return nil, fmt.Errorf("api: no connector for %s", c.srcCur)
			}
			payHex, err := conn.GetRawTransaction(payTxID)
			if err != nil {
				return nil, fmt.Errorf("api: getrawtransaction %s: %w", payTxID, err)
			}
			// The maker's payTx was serialized by the maker's XBridge connector;
			// if that coin sets serializeWithTimeField we must parse the nTime
			// field accordingly.
			hasTime := false
			if cc := c.conf(c.srcCur); cc != nil {
				hasTime = cc.TxWithTimeField
			}
			// The maker's payTx spends OUR OWN deposit (the taker's B-deposit on
			// srcCur): bind extraction to it (C++ xbridgesession.cpp:3935 passes
			// binTxId/binTxVout). The vout is always 0 for own deposits:
			// BuildDepositTx emits the P2SH HTLC as output 0 and C++
			// createDepositTransaction stamps txVout=0 (connectorbtc.cpp:2413)
			// — a counterparty deposit may sit elsewhere (hence the
			// CheckDepositTransaction re-gate), but never our own.
			secret, ok := secretFromPayTx(payHex, c.theirSecretHash, hasTime, c.ourDepositTxID, ownDepositVout)
			if !ok {
				return nil, fmt.Errorf("api: could not recover secret from payTx %s", payTxID)
			}
			c.secret = secret // the claim's payment scriptSig pushes the preimage
			xlog.Info("ConfirmB: secret recovered from maker payTx", "order", orderID,
				"makerPayTx", payTxID, "secretHash", hexEncode(c.theirSecretHash[:]))

			payHex2, cur, err := c.redeemCounterparty(false)
			if err != nil {
				return nil, err
			}
			xlog.Debug("ConfirmB: claim tx built", "order", orderID, "cur", cur)
			// Fail fast when the spending chain has no connector, but do NOT
			// broadcast here: the engine persists the claim intent (including
			// the recovered secret) first and the broadcast runs separately.
			// Derive the payTx id locally (authoritative); the wallet's
			// reported id is only ever logged, never adopted.
			wconn, e := c.connector(cur)
			if e != nil {
				return nil, e
			}
			localPayID, err := txIDFromHex(payHex2)
			if err != nil {
				return nil, err
			}
			return confirmOutcome{secret: secret, payTxID: localPayID, payHex: payHex2, cur: cur, conn: wconn}, nil
		},
		apply: func(v any, terr error) { s.applyConfirmedB(v, terr) },
	}
}

// scheduleDepositRetry records a failed HTLC deposit build for tick-driven
// rebuild. Same contract as scheduleClaimRetry: selfCancel aborts must NOT
// come here — failSelfCancel owns those.
func scheduleDepositRetry(s *SwapSession, now uint64, role string, err error) {
	orderID := hexEncode(s.id[:])
	delay := retryBackoff(s.depositRetries)
	s.depositRetries++
	s.depositRetryAt = now + delay
	s.lastProgress = now
	xlog.Warn("deposit build failed, retry scheduled", "order", orderID, "role", role,
		"err", err, "retryInSec", delay/1000000, "attempt", s.depositRetries)
	if perr := s.n.persistNow(); perr != nil {
		xlog.Error("deposit retry persist failed, in-memory schedule kept", "order", orderID, "err", perr)
	}
}

// clearDepositRetry drops the retry schedule after a successful build.
// Engine-side.
func clearDepositRetry(s *SwapSession) {
	s.depositRetryAt = 0
	s.depositRetries = 0
}

// makerDepositBuildTask returns the CreateA HTLC deposit BUILD task: it
// builds (never broadcasts) our deposit A. Identical for the hub-driven
// first attempt and tick-driven rebuilds — everything it needs (own funding,
// counterparty key) is stage-1 session state.
func makerDepositBuildTask(s *SwapSession, c swapCtx) workTask {
	orderID := hexEncode(s.id[:])
	return workTask{
		orderID: orderID,
		run: func() (any, error) {
			return c.buildDeposit(true)
		},
		apply: func(v any, terr error) { s.applyCreatedA(v, terr) },
	}
}

// takerDepositBuildTask returns the CreateB HTLC deposit BUILD task: it
// drift-checks and validates the maker's A deposit, then builds (never
// broadcasts) our deposit B. The counterparty pointers come from the
// snapshot, which the sweep refreshes from the session before posting.
func takerDepositBuildTask(s *SwapSession, c swapCtx) workTask {
	orderID := hexEncode(s.id[:])
	return workTask{
		orderID: orderID,
		run: func() (any, error) {
			// Validate the counterparty (maker) A-deposit lockTime BEFORE we
			// broadcast our own deposit (mirrors C++ xbridgesession.cpp:2464
			// bad-locktime cancel). Compare the received value against our own
			// expectation for that same deposit role on the maker's coin
			// (dstCur) — NOT against our B lockTime, which legitimately differs
			// by ~90 blocks.
			expLT := c.computeLockTimeFor(c.dstCur, true)
			if expLT == 0 {
				// Our own height read failed (wallet outage) — transient, never
				// a counterparty fault (S5 lesson, mirrors the maker side).
				return nil, fmt.Errorf("api: locktime unavailable for %s, awaiting redelivery", c.dstCur)
			}
			if !acceptableLockTimeDrift(expLT, c.theirLockTime, c.blockTimeFor(c.dstCur)) {
				// C++ :2464-2472 — bad counterparty locktime → wire-Cancel.
				return nil, &selfCancelErr{reason: crBadALockTime}
			}
			// Validate the maker's A deposit BEFORE committing ours
			// (C++ :2495). Wait → no response (hub retransmits); bad → Cancel.
			dcheck, err := c.checkCounterpartyDeposit(c.theirSecretHash, c.dstAmt)
			if err != nil {
				return nil, err
			}
			if !dcheck.IsGood {
				return nil, &selfCancelErr{reason: crBadADepositTx}
			}
			c.theirDepositVout = dcheck.DepositVout
			c.theirP2SHNative = dcheck.P2SHNative
			c.theirOverpayment = dcheck.Excess
			out, err := c.buildDeposit(false)
			if err != nil {
				return nil, err
			}
			return createdBOutcome{out: out, check: dcheck}, nil
		},
		apply: func(v any, terr error) { s.applyCreatedB(v, terr) },
	}
}

// adoptOrRepostDeposit handles a tick-due session whose deposit was BUILT
// (ourDepositTxID + depositHex persisted) but never confirmed broadcast.
// Engine-side. First it checks whether the "failed" broadcast actually
// landed (wallet accepted, response lost): a verbose lookup with
// confirmations >= 1 adopts the confirmation by running the success resume
// directly — never rebroadcast. Otherwise it re-posts the IDENTICAL bytes
// (same txid by construction): a repost can never double-deposit.
func (n *Node) adoptOrRepostDeposit(s *SwapSession, now uint64) {
	orderID := hexEncode(s.id[:])
	c := s.snapshot()
	conn := c.connectors[s.srcCur]
	out := depositOutcome{txid: s.ourDepositTxID, lockTime: s.ourLockTime,
		refundHex: s.refundHex, depositHex: s.depositHex, conn: conn}
	if conn != nil {
		if v, err := conn.GetRawTransactionVerbose(s.ourDepositTxID); err == nil && v.Confirmations >= 1 {
			xlog.Info("deposit rebuild: adopting on-chain confirmation", "order", orderID,
				"txid", s.ourDepositTxID, "confs", v.Confirmations)
			s.depositRetryAt = 0
			if s.isMaker {
				s.applyCreatedABroadcast(out, s.ourDepositTxID, nil)
			} else {
				check := wallet.DepositCheck{IsGood: true, DepositVout: s.theirDepositVout,
					P2SHNative: s.theirP2SHNative, Excess: s.theirOverpayment}
				s.applyCreatedBBroadcast(createdBOutcome{out: out, check: check}, s.ourDepositTxID, nil)
			}
			return
		}
	}
	if s.depositRetries >= broadcastRepostMax {
		xlog.Warn("deposit rebuild: repost cap reached, operator attention", "order", orderID,
			"txid", s.ourDepositTxID, "attempts", s.depositRetries)
		s.depositRetryAt = 0
		return
	}
	if conn == nil {
		s.depositRetryAt = now + claimRetryBaseMicro
		return
	}
	s.depositRetryAt = 0 // consumed; a failed repost reschedules
	s.holdAwait()
	s.lastProgress = now
	xlog.Info("deposit rebuild: re-posting built deposit", "order", orderID,
		"txid", s.ourDepositTxID, "attempt", s.depositRetries+1)
	var ok bool
	if s.isMaker {
		out.conn = conn
		o := out
		ok = n.postBroadcastTask(orderID, conn, s.srcCur, s.depositHex, broadcastDeposit,
			func(sentID string, berr error) { s.applyCreatedABroadcast(o, sentID, berr) })
	} else {
		check := wallet.DepositCheck{IsGood: true, DepositVout: s.theirDepositVout,
			P2SHNative: s.theirP2SHNative, Excess: s.theirOverpayment}
		bo := createdBOutcome{out: out, check: check}
		bo.out.conn = conn
		o := bo
		ok = n.postBroadcastTask(orderID, conn, s.srcCur, s.depositHex, broadcastDeposit,
			func(sentID string, berr error) { s.applyCreatedBBroadcast(o, sentID, berr) })
	}
	if !ok {
		// Engine busy and the task was dropped (postBroadcastTask cleared
		// await): try again next tick instead of losing the retry.
		s.releaseAwait()
		s.depositRetryAt = now + claimRetryBaseMicro
	}
}

// adoptOrRepostClaim handles a tick-due session whose claim was BUILT
// (claimTxID + claimHex persisted) but never confirmed broadcast. Same
// adopt-then-repost contract as deposits: confirmations adopt, otherwise the
// IDENTICAL bytes go out again (same claim txid by construction).
func (n *Node) adoptOrRepostClaim(s *SwapSession, now uint64) {
	orderID := hexEncode(s.id[:])
	c := s.snapshot()
	conn := c.connectors[s.claimCur]
	check := wallet.DepositCheck{IsGood: true, DepositVout: s.theirDepositVout,
		P2SHNative: s.theirP2SHNative, Excess: s.theirOverpayment}
	out := confirmOutcome{secret: s.secret, payTxID: s.claimTxID, payHex: s.claimHex,
		cur: s.claimCur, check: check, conn: conn}
	if conn != nil {
		if v, err := conn.GetRawTransactionVerbose(s.claimTxID); err == nil && v.Confirmations >= 1 {
			xlog.Info("claim rebuild: adopting on-chain confirmation", "order", orderID,
				"payTxID", s.claimTxID, "confs", v.Confirmations)
			s.claimRetryAt = 0
			if s.isMaker {
				s.applyConfirmedABroadcast(out, s.claimTxID, nil)
			} else {
				s.applyConfirmedBBroadcast(out, s.claimTxID, nil)
			}
			return
		}
	}
	if s.claimRetries >= broadcastRepostMax {
		xlog.Warn("claim rebuild: repost cap reached, operator attention", "order", orderID,
			"payTxID", s.claimTxID, "attempts", s.claimRetries)
		s.claimRetryAt = 0
		return
	}
	if conn == nil {
		s.claimRetryAt = now + claimRetryBaseMicro
		return
	}
	s.claimRetryAt = 0 // consumed; a failed repost reschedules
	s.holdAwait()
	s.lastProgress = now
	xlog.Info("claim rebuild: re-posting built claim", "order", orderID,
		"payTxID", s.claimTxID, "attempt", s.claimRetries+1)
	out.conn = conn
	o := out
	var ok bool
	if s.isMaker {
		ok = n.postBroadcastTask(orderID, conn, s.claimCur, s.claimHex, broadcastClaim,
			func(sentID string, berr error) { s.applyConfirmedABroadcast(o, sentID, berr) })
	} else {
		ok = n.postBroadcastTask(orderID, conn, s.claimCur, s.claimHex, broadcastClaim,
			func(sentID string, berr error) { s.applyConfirmedBBroadcast(o, sentID, berr) })
	}
	if !ok {
		s.releaseAwait()
		s.claimRetryAt = now + claimRetryBaseMicro
	}
}

// retryFailedDepositBuilds re-posts the deposit BUILD for sessions whose
// build failed but whose trigger pointers are complete and whose retry is
// due. Engine-side (called from the 60 s tick). Only rebuilds while our own
// deposit was never broadcast (ourDepositTxID empty): a retry can never
// double-broadcast, and a hub/user cancel in between wins by removing the
// session first. Pre-fix sessions (failed before the retry fields existed)
// are adopted: pointers complete + no deposit + not awaiting + silent past
// one base delay means the build must have died without a schedule.
func (n *Node) retryFailedDepositBuilds(now uint64) {
	for orderID, s := range n.sessions {
		if s.await {
			continue
		}
		// REPOST branch: intent built (txid + hex persisted) but broadcast
		// never confirmed. Re-send the identical bytes or adopt the
		// on-chain confirmation — never rebuild (which would double-deposit).
		if s.ourDepositTxID != "" && s.depositHex != "" {
			rolePreCreated := (s.isMaker && s.state < csCreatedA) || (!s.isMaker && s.state < csCreatedB)
			depositSent := false
			if o := s.order(); o != nil {
				depositSent = o.DepositSent
			}
			if !rolePreCreated || depositSent {
				continue
			}
			if s.depositRetryAt == 0 || s.depositRetryAt > now {
				continue
			}
			n.adoptOrRepostDeposit(s, now)
			continue
		}
		if s.ourDepositTxID != "" {
			// Built intent without hex (pre-upgrade record): neither rebuild
			// (would double-deposit) nor repost (nothing to send) — hub
			// redelivery owns it, as before.
			continue
		}
		var task workTask
		switch {
		case s.isMaker && s.state < csCreatedA && s.theirPub != [33]byte{}:
			task = makerDepositBuildTask(s, s.snapshot())
		case !s.isMaker && s.state < csCreatedB && s.theirDepositTxID != "":
			task = takerDepositBuildTask(s, s.snapshot())
		default:
			continue
		}
		if s.depositRetryAt == 0 {
			// Adoption: a complete pre-deposit session with no schedule can
			// only exist if its build died pre-fix (or the node restarted
			// mid-flight) — and only if it has been silent past one base
			// delay, so a just-created session never double-posts with its
			// in-flight hub packet.
			if s.lastProgress != 0 && s.lastProgress+claimRetryBaseMicro > now {
				continue
			}
		} else if s.depositRetryAt > now {
			continue
		}
		s.depositRetryAt = 0 // consumed; the next failure reschedules
		s.holdAwait()
		s.lastProgress = now
		xlog.Info("deposit rebuild: retrying failed deposit build", "order", orderID,
			"state", s.state.String(), "attempt", s.depositRetries+1)
		if !n.postSwapTask(orderID, task) {
			// Engine busy and the task was dropped (postSwapTask cleared
			// await): try again next tick instead of losing the retry.
			s.releaseAwait()
			s.depositRetryAt = now + claimRetryBaseMicro
		}
	}
}

// retryFailedClaimBuilds re-posts the claim BUILD for sessions whose build
// failed but whose trigger pointers are complete and whose retry is due.
// Engine-side (called from the 60 s tick). Only rebuilds while no claim was
// ever broadcast (claimTxID empty): a retry can never double-broadcast, and
// a hub/user cancel in between wins by removing the session first.
func (n *Node) retryFailedClaimBuilds(now uint64) {
	for orderID, s := range n.sessions {
		if s.await || s.claimRetryAt == 0 || s.claimRetryAt > now {
			continue
		}
		// REPOST branch: intent built (txid + hex persisted) but broadcast
		// never confirmed. Re-send the identical bytes or adopt the
		// on-chain confirmation — never rebuild (which would double-claim).
		// The confirmed states (csConfirmedA/B) are restore-reconciliation:
		// a pre-finish session persisted after its claim broadcast (claim
		// on-chain, hub Finished never observed — live S7 order fda21ce4)
		// reconciles through the same adoption replay, which is the C++
		// trFinished terminal state (xbridgesession.cpp:3002/:3185).
		if s.claimTxID != "" && s.claimHex != "" {
			rolePreConfirmed := (s.isMaker && (s.state == csCreatedA || s.state == csConfirmedA)) ||
				(!s.isMaker && (s.state == csCreatedB || s.state == csConfirmedB))
			if !rolePreConfirmed {
				// Stale schedule on an advanced session: tidy, don't spin.
				s.claimRetryAt = 0
				continue
			}
			n.adoptOrRepostClaim(s, now)
			continue
		}
		if s.claimTxID != "" {
			// Built intent without hex (pre-upgrade record): neither rebuild
			// (would double-claim) nor repost (nothing to send) — hub
			// redelivery owns it, as before.
			continue
		}
		var task workTask
		switch {
		case !s.isMaker && s.state == csCreatedB && s.theirPayTxID != "":
			task = takerClaimBuildTask(s, s.snapshot(), s.theirPayTxID)
		case s.isMaker && s.state == csCreatedA:
			task = makerClaimBuildTask(s, s.snapshot())
		default:
			continue
		}
		s.claimRetryAt = 0 // consumed; the next failure reschedules
		s.holdAwait()
		s.lastProgress = now
		xlog.Info("claim rebuild: retrying failed claim build", "order", orderID,
			"state", s.state.String(), "attempt", s.claimRetries+1)
		if !n.postSwapTask(orderID, task) {
			// Engine busy and the task was dropped (postSwapTask cleared
			// await): try again next tick instead of losing the retry.
			s.releaseAwait()
			s.claimRetryAt = now + claimRetryBaseMicro
		}
	}
}
