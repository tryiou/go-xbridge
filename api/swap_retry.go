package api

import (
	"errors"
	"fmt"

	xlog "go-xbridge/log"

	"go-xbridge/wallet"
)

const (
	notReadyPollMicro       = 5 * 1000000
	notReadyTaperAfterMicro = 60 * 1000000
	notReadyTaperPollMicro  = 15 * 1000000
	notReadyFastWindowMicro = 10 * 60 * 1000000

	claimRetryBaseMicro = 60 * 1000000
	claimRetryCapMicro  = 15 * 60 * 1000000
	// broadcastRepostMax bounds identical-byte reposts only (parity with
	// maxRebroadcasts in reconcile.go); rebuilds revalidate, so build retries stay uncapped.
	broadcastRepostMax = 10
	// awaitStuckMicro: a lost worker result must be released far sooner than
	// the 30-minute stall cancel, and far later than any healthy RPC round-trip.
	awaitStuckMicro = 3 * 60 * 1000000
)

func (s *SwapSession) holdAwait() {
	s.await = true
	s.awaitSince = NowMicro()
}

func (s *SwapSession) releaseAwait() {
	s.await = false
	s.awaitSince = 0
}

// clearStuckAwait never refreshes lastProgress: the stall watchdog must keep
// judging the session's original silence, not the release of a lost result.
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

func (s *SwapSession) scheduleNotReady(now uint64, role string, isClaim bool, terr error) bool {
	if !errors.Is(terr, wallet.ErrDepositNotReady) {
		return false
	}
	orderID := hexEncode(s.id[:])
	if s.notReadySince > now {
		s.notReadySince = now
	}
	if s.notReadySince != 0 && s.notReadySince <= now && now-s.notReadySince >= notReadyFastWindowMicro {
		xlog.Warn("deposit not ready past fast window, staying on backoff", "order", orderID,
			"role", role, "waitingSec", (now-s.notReadySince)/1000000)
		return false
	}
	if s.notReadySince == 0 {
		s.notReadySince = now
	}
	s.notReadySeen = wallet.NotReadySeen(terr)
	poll := uint64(notReadyPollMicro)
	if !s.notReadySeen && s.notReadySince <= now && now-s.notReadySince >= notReadyTaperAfterMicro {
		poll = uint64(notReadyTaperPollMicro)
		xlog.Info("deposit still invisible past taper age, slowing re-poll", "order", orderID,
			"role", role, "waitingSec", (now-s.notReadySince)/1000000,
			"retryInSec", poll/1000000)
	}
	if isClaim {
		s.claimRetryAt = now + poll
	} else {
		s.depositRetryAt = now + poll
	}
	s.lastProgress = now
	xlog.Info("deposit not ready, re-poll scheduled", "order", orderID, "role", role,
		"retryInSec", poll/1000000)
	if perr := s.n.persistNow(); perr != nil {
		xlog.Error("not-ready re-poll persist failed, in-memory schedule kept", "order", orderID, "err", perr)
	}
	return true
}

// Self-cancel aborts must never be scheduled as retries: failSelfCancel owns
// them (selfCancelErr) — scheduleDepositRetry follows the same rule.
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

func clearClaimRetry(s *SwapSession) {
	s.claimRetryAt = 0
	s.claimRetries = 0
	s.notReadySince = 0
	s.notReadySeen = false
}

func makerClaimBuildTask(s *SwapSession, c swapCtx) workTask {
	orderID := hexEncode(s.id[:])
	return workTask{
		orderID: orderID,
		run: func() (any, error) {
			expLT := c.computeLockTimeFor(c.dstCur, false)
			if expLT == 0 {
				return nil, fmt.Errorf("api: locktime unavailable for %s, awaiting redelivery", c.dstCur)
			}
			if !acceptableLockTimeDrift(expLT, c.theirLockTime, c.blockTimeFor(c.dstCur)) {
				return nil, &selfCancelErr{reason: crBadBLockTime}
			}
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

func takerClaimBuildTask(s *SwapSession, c swapCtx, payTxID string) workTask {
	orderID := hexEncode(s.id[:])
	return workTask{
		orderID: orderID,
		run: func() (any, error) {
			conn := c.connectors[c.srcCur]
			if conn == nil {
				return nil, fmt.Errorf("api: no connector for %s", c.srcCur)
			}
			payHex, err := conn.GetRawTransaction(payTxID)
			if err != nil {
				if code, ok := wallet.RPCErrorCode(err); ok && code == -5 {
					return nil, &wallet.NotReadyError{Seen: false, Err: fmt.Errorf("api: getrawtransaction %s not ready: %w", payTxID, wallet.ErrDepositNotReady)}
				}
				return nil, fmt.Errorf("api: getrawtransaction %s: %w", payTxID, err)
			}
			hasTime := false
			if cc := c.conf(c.srcCur); cc != nil {
				hasTime = cc.TxWithTimeField
			}
			secret, ok := secretFromPayTx(payHex, c.theirSecretHash, hasTime, c.ourDepositTxID, ownDepositVout)
			if !ok {
				return nil, fmt.Errorf("api: could not recover secret from payTx %s", payTxID)
			}
			c.secret = secret
			xlog.Info("ConfirmB: secret recovered from maker payTx", "order", orderID,
				"makerPayTx", payTxID, "secretHash", hexEncode(c.theirSecretHash[:]))

			payHex2, cur, err := c.redeemCounterparty(false)
			if err != nil {
				return nil, err
			}
			xlog.Debug("ConfirmB: claim tx built", "order", orderID, "cur", cur)
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

func clearDepositRetry(s *SwapSession) {
	s.depositRetryAt = 0
	s.depositRetries = 0
	s.notReadySince = 0
	s.notReadySeen = false
}

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

func takerDepositBuildTask(s *SwapSession, c swapCtx) workTask {
	orderID := hexEncode(s.id[:])
	return workTask{
		orderID: orderID,
		run: func() (any, error) {
			expLT := c.computeLockTimeFor(c.dstCur, true)
			if expLT == 0 {
				return nil, fmt.Errorf("api: locktime unavailable for %s, awaiting redelivery", c.dstCur)
			}
			if !acceptableLockTimeDrift(expLT, c.theirLockTime, c.blockTimeFor(c.dstCur)) {
				return nil, &selfCancelErr{reason: crBadALockTime}
			}
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
	s.depositRetryAt = 0
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
		s.releaseAwait()
		s.depositRetryAt = now + claimRetryBaseMicro
	}
}

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
	s.claimRetryAt = 0
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

func (n *Node) retryFailedDepositBuilds(now uint64) {
	for orderID, s := range n.sessions {
		if s.await {
			continue
		}
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
			// ourDepositTxID without depositHex can be neither rebuilt (a second
			// deposit attempt) nor reposted (no bytes): hub redelivery owns it.
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
			// No schedule: restored mid-build, or a fresh session whose staged task was
			// dropped while the hub retransmits — adopt only after one base delay.
			if s.lastProgress != 0 && s.lastProgress+claimRetryBaseMicro > now {
				continue
			}
		} else if s.depositRetryAt > now {
			continue
		}
		s.depositRetryAt = 0
		s.holdAwait()
		s.lastProgress = now
		xlog.Info("deposit rebuild: retrying failed deposit build", "order", orderID,
			"state", s.state.String(), "attempt", s.depositRetries+1)
		if !n.postSwapTask(orderID, task) {
			s.releaseAwait()
			s.depositRetryAt = now + claimRetryBaseMicro
		}
	}
}

func (n *Node) retryFailedClaimBuilds(now uint64) {
	for orderID, s := range n.sessions {
		if s.await || s.claimRetryAt == 0 || s.claimRetryAt > now {
			continue
		}
		if s.claimTxID != "" && s.claimHex != "" {
			rolePreConfirmed := (s.isMaker && (s.state == csCreatedA || s.state == csConfirmedA)) ||
				(!s.isMaker && (s.state == csCreatedB || s.state == csConfirmedB))
			if !rolePreConfirmed {
				s.claimRetryAt = 0
				continue
			}
			n.adoptOrRepostClaim(s, now)
			continue
		}
		if s.claimTxID != "" {
			// A claimTxID without claimHex can be neither rebuilt (a second
			// claim attempt) nor reposted (no bytes): hub redelivery owns it.
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
		s.claimRetryAt = 0
		s.holdAwait()
		s.lastProgress = now
		xlog.Info("claim rebuild: retrying failed claim build", "order", orderID,
			"state", s.state.String(), "attempt", s.claimRetries+1)
		if !n.postSwapTask(orderID, task) {
			s.releaseAwait()
			s.claimRetryAt = now + claimRetryBaseMicro
		}
	}
}
