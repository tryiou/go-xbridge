package api

import (
	xlog "go-xbridge/log"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// Own-deposit spend watch — the C++ App::Impl::checkWatchesOnDepositSpends
// taker branch (xbridgeapp.cpp:3340-3457, role-B scan :3378-3427). While a
// taker session sits pre-claim (secret unknown), the daemon sweeps the source
// chain's mempool for the transaction spending its own HTLC deposit: that
// spender is the maker's payTx, whose scriptSig pushes the HTLC secret. C++
// adopts the spender as otherPayTxId (:3421-3423), redeems, and finishes
// locally (:3424-3431) — the recovery works even when the hub's ConfirmB
// never arrives. This port discovers the spender and re-drives the exact
// ConfirmB handler with it (C++ ConfirmB adoption :3163 stores the same
// payTxId), so the finish path, persistence, and the ConfirmedB hub receipt
// are byte-identical to the normal flow.
//
// Known scope note: only UNCONFIRMED spends are discoverable in the mempool
// scan. A spend that confirmed while the daemon was offline is invisible to
// getrawmempool; such a session arms the secret hunt on the first failed
// refund (the pre-signed refund's input is gone, so the refund is not a
// fallback for a claimed deposit — only for one still out). The hunt
// re-validates false positives away and stays dashboard-visible; automated
// confirmed-spend discovery follows as the next change. C++ closes that gap
// with a block-by-block rescan (xbridgeapp.cpp:3389-3413).

// ownWatchFetchBudget caps the per-tick GetRawTransaction fetches per session.
// C++ pays the same per-tx RPC cost (isUTXOSpentInTx per mempool entry,
// xbridgeapp.cpp:3419); the budget keeps a large mempool from monopolizing the
// worker pool, while ownWatchSeen makes the sweep incremental across ticks.
const ownWatchFetchBudget = 200

// watchOwnDepositSpends sweeps live taker sessions whose own deposit is
// broadcast but whose secret is still unknown, posting one mempool-scan task
// per eligible session. Engine goroutine (tick stage, after
// watchCounterpartyDeposits); the single-flight pendingOwnWatch guard keeps a
// session to one in-flight scan, mirroring pendingWatch.
func (n *Node) watchOwnDepositSpends() {
	if n.pendingOwnWatch == nil { // defensive: pre-start nodes (tests)
		n.pendingOwnWatch = map[string]bool{}
	}
	for id, s := range n.sessions {
		if s.isMaker || s.await {
			continue
		}
		// C++ role-B scan gate (:3378): the mempool sweep is taker-only —
		// the maker generates the secret itself and finishes via ConfirmA.
		// The csCreatedB state is only a proxy for "deposit out": a hunting
		// session already proved its deposit out (the spent verdict that
		// armed it), including crash-recovered sessions parked below
		// csCreatedB — excluding them would park the hunt with no watch.
		depositOut := s.state == csCreatedB || (s.secretHunt && n.orderDepositSent(id))
		if !depositOut {
			continue
		}
		// Watch only while the redeem has not happened: our deposit is out
		// (C++ watch registration follows the deposit broadcast,
		// xbridgesession.cpp:2715) and the secret/preimage is still unknown.
		if s.ourDepositTxID == "" || s.claimTxID != "" || s.secret != [33]byte{} {
			continue
		}
		// The secret comparison needs the hash CreateB taught us.
		if s.theirSecretHash == [20]byte{} {
			continue
		}
		if n.engineRunning.Load() {
			if n.pendingOwnWatch[id] {
				continue
			}
			n.pendingOwnWatch[id] = true
		}
		n.postOwnWatchTask(id)
	}
}

// ownWatchResult is the worker outcome: the discovered spender ("" when none
// matched this round) plus every txid scanned, for the incremental seen-set.
// rechecked reports the hunt re-validation below actually ran (hunted session
// with a readable backend); depositLive is its affirmative verdict — the
// deposit outpoint reads unspent again, so a false-positive hunt clears.
type ownWatchResult struct {
	spender     string
	scanned     []string
	rechecked   bool
	depositLive bool
}

// runOwnWatchTask sweeps the source chain's mempool for the spender of our
// own deposit (C++ isUTXOSpentInTx loop, xbridgeapp.cpp:3413-3423). seen is
// the engine-owned snapshot of already-scanned txids (incremental sweep). It
// returns the spender txid ("" when none matched this round) plus every txid
// whose raw bytes were successfully fetched, so the apply only advances the
// seen-set on real evidence — a failed fetch (mempool churn) must not hide
// the txid from later sweeps.
func runOwnWatchTask(conn wallet.Connector, seen map[string]struct{}, hx [20]byte, depTxID string, depVout uint32, hasTime bool) (ownWatchResult, error) {
	if conn == nil {
		return ownWatchResult{}, wallet.ErrNoChainSource
	}
	txids, err := conn.GetRawMempool()
	if err != nil {
		return ownWatchResult{}, err
	}
	var res ownWatchResult
	fetched := 0
	for _, txid := range txids {
		if _, done := seen[txid]; done {
			continue
		}
		if fetched >= ownWatchFetchBudget {
			break
		}
		fetched++
		payHex, err := conn.GetRawTransaction(txid)
		if err != nil {
			continue // mempool churn: the entry may have left between calls
		}
		// Record only fetched txids: marking a failed fetch as scanned would
		// merge it into the durable seen-set and hide it from every later
		// sweep — including a re-broadcast of the same txid.
		res.scanned = append(res.scanned, txid)
		// The spender of OUR deposit is the maker's payTx: bind the secret
		// extraction to our own deposit outpoint exactly like
		// takerClaimBuildTask (C++ getSecretFromPaymentTransaction,
		// xbridgewalletconnectorbtc.cpp:2241-2276).
		if _, ok := secretFromPayTx(payHex, hx, hasTime, depTxID, depVout); ok {
			res.spender = txid
			return res, nil
		}
	}
	return res, nil
}

// postOwnWatchTask posts one own-deposit mempool scan to the worker pool (or
// runs it synchronously in inline mode). The apply runs on the engine: a found
// spender re-drives OnConfirmB with it — the same handler the hub's ConfirmB
// packet would trigger (C++ ConfirmB adoption :3163 + watch adoption :3421
// converge on the identical redeem path) — so the claim build, broadcast,
// hub receipt, and finish are the established code path. A full task queue
// drops the scan (the next sweep re-enqueues; nothing was broadcast).
func (n *Node) postOwnWatchTask(orderID string) bool {
	s := n.sessions[orderID]
	if s == nil {
		return false
	}
	conn := n.cfg().Connectors[s.srcCur]
	// Capture plain values for the worker: the session is engine-owned and
	// must never be read off the engine goroutine (race detector).
	srcCur, depTxID := s.srcCur, s.ourDepositTxID
	hunt := s.secretHunt
	depVout := uint32(ownDepositVout)
	hx := s.theirSecretHash
	hasTime := false
	if cc := n.cfg().Confs[srcCur]; cc != nil {
		hasTime = cc.TxWithTimeField
	}
	if n.ownWatchSeen == nil { // defensive: pre-start nodes (tests)
		n.ownWatchSeen = map[string]map[string]struct{}{}
	}
	if n.ownWatchSeen[orderID] == nil {
		n.ownWatchSeen[orderID] = map[string]struct{}{}
	}
	seen := make(map[string]struct{}, len(n.ownWatchSeen[orderID]))
	for k := range n.ownWatchSeen[orderID] {
		seen[k] = struct{}{}
	}
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			var res ownWatchResult
			// Hunt re-validation runs BEFORE and INDEPENDENT of the mempool
			// scan: it rests on chain reads (verbose + gettxout), not the
			// mempool, so a blind/dead mempool must not suppress a
			// false-positive correction. Only affirmative evidence moves
			// the flag (probe folds blindness into rechecked==false).
			// Outside the mempool budget: hunted sessions are rare, and
			// correctness outranks fetch economy.
			if hunt {
				if known, spent := probeOwnDeposit(conn, depTxID, depVout); known {
					res.rechecked = true
					res.depositLive = !spent
				}
			}
			mem, merr := runOwnWatchTask(conn, seen, hx, depTxID, depVout, hasTime)
			res.spender = mem.spender
			res.scanned = mem.scanned
			return res, merr
		},
		apply: func(v any, err error) {
			delete(n.pendingOwnWatch, orderID)
			res, _ := v.(ownWatchResult)
			// Merge the scanned txids into the engine-owned seen set so the
			// next sweep is incremental (C++ per-tick incremental rescan,
			// xbridgeapp.cpp:3384-3413). A spender lands on top of the set.
			for _, k := range res.scanned {
				n.ownWatchSeen[orderID][k] = struct{}{}
			}
			// Hunt re-validation verdict stands even when the mempool leg
			// failed above: a false positive corrected is a refund resumed,
			// and the mempool's health says nothing about the outpoint.
			// The spender path below owns recovery when a spend is actually
			// found — a live spend and a live outpoint cannot both hold,
			// and the claim beats the refund.
			if res.spender == "" {
				if s := n.sessions[orderID]; s != nil && s.secretHunt && res.rechecked && res.depositLive {
					s.secretHunt = false
					s.huntSince = 0
					n.persist()
					xlog.Info("hunt cleared: deposit unspent again, refund path resumed", "order", orderID)
				}
			}
			if err != nil {
				// Unsupported/transient backend (LocalConnector, wallet down):
				// degrade to the refund sweep, never cancel (C++ :3937/:3948
				// return-false semantics — the watch simply keeps looking).
				xlog.Debug("own-deposit watch unavailable, skipping round", "order", orderID, "err", err)
				return
			}
			if res.spender == "" {
				return
			}
			// Re-resolve the session: it may have finished, been cancelled,
			// or been pruned while the scan was in flight.
			s := n.sessions[orderID]
			if s == nil {
				return
			}
			xlog.Info("own-deposit watch: spender found, re-driving ConfirmB", "order", orderID,
				"spender", res.spender, "deposit", depTxID)
			// The discovered spend IS the fact the hub's ConfirmB delivers
			// (C++ watch adoption :3421-3423 stores the identical payTxId).
			if _, _, err := s.OnConfirmB(&proto.ConfirmBBody{HubAddress: s.hub, ID: s.id, APayTxID: res.spender}); err != nil {
				xlog.Warn("own-deposit watch: ConfirmB re-drive failed", "order", orderID, "err", err)
			}
		},
	}
	if !n.engineRunning.Load() {
		v, err := safeTaskRun(task)
		task.apply(v, err)
		return true
	}
	select {
	case n.tasks <- task:
		return true
	default:
		delete(n.pendingOwnWatch, orderID)
		xlog.Warn("own-deposit watch task dropped, engine busy", "order", orderID)
		return false
	}
}
