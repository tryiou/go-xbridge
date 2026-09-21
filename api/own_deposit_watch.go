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
// Known scope note: UNCONFIRMED spends surface in the mempool scan;
// spends that confirmed while the daemon was offline surface in the
// confirmed leg (runRescanPages), which pages forward from the
// deposit-time cursor. A confirmed-while-offline spend arms the secret
// hunt on the first failed refund (the pre-signed refund's input is gone,
// so the refund is not a fallback for a claimed deposit — only for one
// still out); the hunt then recovers through either leg, re-validates
// false positives away, and stays dashboard-visible until terminal. C++
// closes the same gap with a block-by-block rescan
// (xbridgeapp.cpp:3389-3413); this is its thin-client shape —
// verbosity-2 pages instead of per-tx RPC fan-out.

// ownWatchFetchBudget caps the per-tick GetRawTransaction fetches per session.
// C++ pays the same per-tx RPC cost (isUTXOSpentInTx per mempool entry,
// xbridgeapp.cpp:3419); the budget keeps a large mempool from monopolizing the
// worker pool, while ownWatchSeen makes the sweep incremental across ticks.
const ownWatchFetchBudget = 200

// rescanPageBudget is the default per-round page bound (see
// rescanPageBudgetFor). Worst case per hunted session per 60s tick: 1
// getblockcount + budget × (getblockhash + getblock) + at most one
// getrawtransaction per vin-matched candidate (matches are rare; our own
// refund excluded without fetch) + the one-time seed reads. The mempool
// leg's pre-existing 200-fetch budget is separate and unchanged.
const rescanPageBudget = 3

// rescanPageBudgetCap bounds the scaled budget: catch-up stretches instead
// of bursting, no matter how fast the chain.
const rescanPageBudgetCap = 12

// rescanPageBudgetFor scales the per-round page bound to chain speed: the
// bound covers double the chain's per-minute growth where scaled (sub-20s
// chains), and the floor of 3 covers normal growth on 20s+ chains with
// headroom (a sustained multi-block burst on a 20-40s chain would stretch
// catch-up, never skip — the cursor persists). Pure function, pinned by
// unit test.
func rescanPageBudgetFor(blockTime int) int {
	if blockTime > 0 && blockTime <= 20 {
		if p := 120/blockTime + 1; p < rescanPageBudgetCap {
			return p
		}
		return rescanPageBudgetCap
	}
	return rescanPageBudget
}

// rescanWindowBlocks seeds the cursor when the deposit's confirmation depth
// is unknown (verbose blind): 144 blocks of history, scanned once within
// the page budget, then owned by the advancing cursor. Wall-clock time this
// covers varies by chain (a day on 10-minute chains, hours on fast ones) —
// the unit is deliberately blocks, the chain's own coordinate.
const rescanWindowBlocks = 144

// rescanOutcome is the confirmed-leg worker result: the spender whose input
// matches our outpoint ("" if none on the scanned pages), the cursor after
// this round, whether any page fully scanned (persist the advance), and the
// first page failure (cursor held — a skipped block is never marked done).
type rescanOutcome struct {
	spender    string
	nextCursor uint32
	advanced   bool
	err        error
}

// seedRescanStart derives the first height to scan: the deposit's asserted
// confirmation depth below tip (≈ broadcast height; off-by-reorg pages are
// harmless), clamped at zero. Unasserted depth (verbose error, missing
// field, conflicted) falls back to the one-day window below. One-time per
// session; the advancing cursor owns it after.
func seedRescanStart(conn wallet.Connector, depTxID string, tip int64) uint32 {
	if vtx, err := conn.GetRawTransactionVerbose(depTxID); err == nil && vtx.HasConfirmations && vtx.Confirmations >= 0 {
		if start := tip - int64(vtx.Confirmations); start > 0 {
			return uint32(start)
		}
		return 0
	}
	if start := tip - rescanWindowBlocks; start > 0 {
		return uint32(start)
	}
	return 0
}

// rescanQuery bundles one confirmed-leg round's inputs (all engine-captured
// values; the worker never touches session state).
type rescanQuery struct {
	depTxID, excludeTxID string
	depVout              uint32
	cursor               uint32
	hx                   [20]byte
	hasTime              bool
	pageBudget           int
}

// rescanBackoffActive reports whether orderID is inside a page-failure
// backoff window. Engine-side (sweeps) and inline tests only.
func (n *Node) rescanBackoffActive(idHex string) bool {
	retryAt, ok := n.rescanRetryAt[idHex]
	return ok && uint64(NowMicro()) < retryAt
}

// recordRescanFailure schedules the next confirmed-leg attempt with doubling
// delay (10min, 20min, … capped at 24h — the refund backoff's windows).
// Engine-side and inline tests only.
func (n *Node) recordRescanFailure(idHex string) {
	if n.rescanAttempts == nil {
		n.rescanAttempts = map[string]int{}
	}
	if n.rescanRetryAt == nil {
		n.rescanRetryAt = map[string]uint64{}
	}
	a := n.rescanAttempts[idHex] + 1
	n.rescanAttempts[idHex] = a
	delay := uint64(refundBackoffBaseMicro)
	for i := 1; i < a && delay < refundBackoffCapMicro; i++ {
		delay *= 2
	}
	if delay > refundBackoffCapMicro {
		delay = refundBackoffCapMicro
	}
	n.rescanRetryAt[idHex] = uint64(NowMicro()) + delay
}

// clearRescanFailure drops the page-failure schedule after progress (pages
// scanned or spender found). Engine-side and inline tests only.
func (n *Node) clearRescanFailure(idHex string) {
	delete(n.rescanAttempts, idHex)
	delete(n.rescanRetryAt, idHex)
}

// runRescanPages walks [cursor, tip] hunting the spender of
// (depTxID, depVout) — the C++ block leg
// (App::checkWatchesOnDepositSpends, xbridgeapp.cpp:3389-3413) minus the
// per-tx RPC fan-out: verbosity-2 blocks carry vins inline, so each page
// costs two RPCs (hash + body) instead of thousands. Pure worker-side.
// Runs hunted-only (caller's gate): the hub and the refund own every other
// session, and backend load stays proportional to hunts, never swaps.
//
// Per-tick backend ceiling for one hunt (verify, don't trust): 1
// getblockcount + budget × (getblockhash + getblock) + at most one
// getrawtransaction per vin-matched candidate (matches are rare; our own
// refund excluded without fetch) + the one-time seed reads (count already
// counted, verbose). The mempool leg's pre-existing budget is separate.
//
// A vin match is necessary but not sufficient: the candidate is fetched and
// secret-validated before declaring (the mempool leg's secretFromPayTx
// rule). Our own refund txid is excluded outright (spends ours, carries no
// secret). Anything undecidable — fetch failure, backend error, undecodable
// page — holds the cursor: a skipped block is never marked done.
func runRescanPages(conn wallet.Connector, q rescanQuery) rescanOutcome {
	tip, err := conn.GetBlockCount()
	if err != nil || tip < 1 {
		return rescanOutcome{nextCursor: q.cursor, err: err}
	}
	cursor := q.cursor
	if cursor == 0 {
		cursor = seedRescanStart(conn, q.depTxID, tip)
	}
	budget := q.pageBudget
	if budget < 1 {
		budget = rescanPageBudget
	}
	start := cursor
	next := cursor
	for h := int64(cursor); h <= tip && h < int64(cursor)+int64(budget); h++ {
		bh, err := conn.GetBlockHash(h)
		if err != nil {
			return rescanOutcome{nextCursor: next, advanced: next != start, err: err}
		}
		txs, err := conn.GetBlockTxs(bh)
		if err != nil {
			return rescanOutcome{nextCursor: next, advanced: next != start, err: err}
		}
		// The C++ isUTXOSpentInTx vin match
		// (xbridgewalletconnectorbtc.cpp:1791-1794), minus its RPC: the
		// vins are already in hand.
		for _, tx := range txs {
			if q.excludeTxID != "" && tx.TxID == q.excludeTxID {
				continue // our own refund: spends ours, carries no secret
			}
			matched := false
			for _, in := range tx.Vin {
				if in.TxID == q.depTxID && in.Vout == q.depVout {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			payHex, ferr := conn.GetRawTransaction(tx.TxID)
			if ferr != nil {
				return rescanOutcome{nextCursor: next, advanced: next != start, err: ferr}
			}
			if _, ok := secretFromPayTx(payHex, q.hx, q.hasTime, q.depTxID, q.depVout); !ok {
				continue // vin-matched but secretless: not our counterparty
			}
			return rescanOutcome{spender: tx.TxID, nextCursor: uint32(h) + 1, advanced: true}
		}
		next = uint32(h) + 1
	}
	return rescanOutcome{nextCursor: next, advanced: next != start}
}

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
// nextCursor/cursorAdvanced carry the confirmed-leg page progress (persist
// the advance); rescanErr is the first page failure (cursor held).
type ownWatchResult struct {
	spender        string
	scanned        []string
	rechecked      bool
	depositLive    bool
	nextCursor     uint32
	cursorAdvanced bool
	rescanErr      error
	// ranRescanLeg reports the confirmed leg executed this round (hunted,
	// no mempool hit, no backoff): only then do rescanErr/cursorAdvanced
	// mean anything.
	ranRescanLeg bool
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
	cursor := s.scanCursor
	depVout := uint32(ownDepositVout)
	hx := s.theirSecretHash
	hasTime := false
	blockTime := 0
	if cc := n.cfg().Confs[srcCur]; cc != nil {
		hasTime = cc.TxWithTimeField
		blockTime = cc.BlockTime
	}
	// Our own refund txid is excluded from spender candidacy (it spends
	// ours by construction and carries no secret); undecodable hex means
	// no exclusion, never a mismatch.
	excludeTxID := ""
	if s.refundHex != "" {
		if rid, rerr := txIDFromHex(s.refundHex); rerr == nil {
			excludeTxID = rid
		}
	}
	rescanDue := !n.rescanBackoffActive(orderID)
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
			// Confirmed leg, hunted only: page forward hunting a confirmed
			// spender the mempool never saw. Skipped when the mempool
			// already found one (recovery owns the round), for every
			// non-hunted session (the hub and the refund own them — backend
			// load stays proportional to hunts, never swaps), and while a
			// page-failure backoff is active (the cursor is held anyway).
			if hunt && res.spender == "" && rescanDue {
				res.ranRescanLeg = true
				rsc := runRescanPages(conn, rescanQuery{
					depTxID: depTxID, excludeTxID: excludeTxID, depVout: depVout,
					cursor: cursor, hx: hx, hasTime: hasTime,
					pageBudget: rescanPageBudgetFor(blockTime),
				})
				res.nextCursor, res.cursorAdvanced, res.rescanErr = rsc.nextCursor, rsc.advanced, rsc.err
				if rsc.spender != "" {
					res.spender = rsc.spender
				}
			}
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
			// Confirmed-leg page progress persists even when the mempool
			// leg failed: scanned pages stay scanned across ticks. A page
			// failure spaces out re-tries with backoff (pruned backends
			// fail permanently — retrying every tick would be the burst
			// this design exists to avoid); progress clears it.
			if s := n.sessions[orderID]; s != nil && res.cursorAdvanced {
				s.scanCursor = res.nextCursor
				n.clearRescanFailure(orderID)
				n.persist()
			}
			if res.ranRescanLeg {
				if res.rescanErr != nil {
					n.recordRescanFailure(orderID)
					xlog.Debug("rescan page failed, cursor held with backoff", "order", orderID, "err", res.rescanErr)
				} else if res.spender != "" || res.cursorAdvanced {
					n.clearRescanFailure(orderID)
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
