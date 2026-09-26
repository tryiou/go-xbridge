package api

import (
	"sort"

	xlog "go-xbridge/log"
	"go-xbridge/wallet"
)

// Phase-0 confirmation tracking: every local broadcast (deposit, claim,
// refund, prep, split) is recorded with its locally-derived txid and its
// confirmation depth is polled each sweep. Later phases read this table to
// distinguish "done" from "accepted then dropped" (live-proven S2 hole:
// wallet accepted the broadcast, the chain never saw it). Observe-only in
// Phase 0: no retries, no state changes — tracking plus logs.

// broadcastKind names the swap operation a tracked broadcast paid for.
type broadcastKind string

const (
	broadcastDeposit broadcastKind = "deposit"
	broadcastClaim   broadcastKind = "claim"
	broadcastRefund  broadcastKind = "refund"
	broadcastPrep    broadcastKind = "prep"
	broadcastSplit   broadcastKind = "split"
)

// trackedBroadcast is one local broadcast under confirmation watch. TxID is
// always locally derived (never adopted from a wallet response, matching the
// deposit/claim/refund builders); Confs is the last verbose-confirmed depth
// (0 = unknown/unconfirmed). Hex is retained so later phases can rebroadcast
// the identical bytes (same txid — never RBF, never fee-bump). Seq orders
// entries for cap pruning without any chain I/O at record time.
// FirstSeenMicro is the last (re)broadcast time (wall micros); Attempts counts
// rebroadcast tries, capped by maxRebroadcasts (then alert-only).
// A deep-confirmed entry with no live session whose refund obligations are
// reconciled is finalized — dropped from this table outright, never polled
// again. The durable trade record stays in Store.history (uncapped audit
// trail); this table holds only in-flight broadcasts.
type trackedBroadcast struct {
	OrderID        string
	Kind           broadcastKind
	Coin           string
	TxID           string
	Hex            string
	Seq            uint64
	Confs          int
	FirstSeenMicro uint64
	Attempts       int
}

// trackedMu guards tracked: record sites run on worker/HTTP goroutines while
// the sweep and the persist snapshot run on the engine/background paths.
func (n *Node) recordBroadcast(orderID string, kind broadcastKind, coin, txid, hexStr string) {
	if txid == "" {
		return
	}
	// Mutex only: no wallet I/O (FirstHeight used to need a block count —
	// replaced by a local sequence, so record sites never touch the chain)
	// and no persist call. persist() snapshots the engine-owned sessions
	// map, which must never run on worker/HTTP goroutines of a started
	// node. Durability: the 60s tick persists (bounded staleness), and swap
	// intents themselves already persist synchronously at every send
	// boundary. Inline mode (tests, engine stopped) persists synchronously.
	n.trackedMu.Lock()
	if n.tracked == nil {
		n.tracked = make(map[string]*trackedBroadcast)
	}
	if _, ok := n.tracked[txid]; ok {
		n.trackedMu.Unlock()
		return // idempotent re-record (rebuild + rebroadcast replays)
	}
	n.tracked[txid] = &trackedBroadcast{
		OrderID: orderID, Kind: kind, Coin: coin,
		TxID: txid, Hex: hexStr, Seq: n.trackedSeq.Add(1),
		FirstSeenMicro: uint64(NowMicro()),
	}
	n.trackedMu.Unlock()
	if !n.engineRunning.Load() {
		n.persist()
	}
}

// restoreTracked reloads persisted broadcast tracking at startup (called
// from restoreLocalSwaps before the engine starts, so no locking is needed).
// Corrupt/duplicate entries are skipped, never fatal: tracking resumes from
// the chain on the next sweep.
func (n *Node) restoreTracked(bc []persistedBroadcast) {
	if len(bc) == 0 {
		return
	}
	n.insertTracked(bc)
}

// insertTracked is the restore insert for the active section.
// Corrupt/duplicate entries are skipped, never fatal.
func (n *Node) insertTracked(bc []persistedBroadcast) {
	if n.tracked == nil {
		n.tracked = make(map[string]*trackedBroadcast, len(bc))
	}
	restored := 0
	var maxSeq uint64
	for _, p := range bc {
		if p.TxID == "" || p.Coin == "" {
			continue
		}
		if _, ok := n.tracked[p.TxID]; ok {
			continue
		}
		n.tracked[p.TxID] = &trackedBroadcast{
			OrderID: p.OrderID, Kind: broadcastKind(p.Kind), Coin: p.Coin,
			TxID: p.TxID, Hex: p.Hex, Seq: p.Seq, Confs: p.Confs,
			FirstSeenMicro: p.FirstSeenMicro, Attempts: p.Attempts,
		}
		if p.FirstSeenMicro == 0 {
			// Pre-rebroadcast persistence: don't mass-rebroadcast on
			// upgrade — start the age clock at restore.
			n.tracked[p.TxID].FirstSeenMicro = uint64(NowMicro())
		}
		if p.Seq > maxSeq {
			maxSeq = p.Seq
		}
		restored++
	}
	// New records must sort after restored ones for oldest-first pruning.
	// The counter must therefore only ever move UP, or a newly recorded
	// broadcast (Add(1)) could collide with a restored Seq.
	if cur := n.trackedSeq.Load(); maxSeq > cur {
		n.trackedSeq.Store(maxSeq)
	}
	if restored > 0 {
		xlog.Info("restored broadcast tracking from disk", "count", restored)
	}
}

// watchNeeded reports whether a tracked broadcast must stay watched: below
// the coin's finalize depth, bound to a live session, or carrying an
// unreconciled refund. Single predicate shared by finalizeWatchEntries and
// migrateSettledArchive so the two paths cannot drift apart.
func (n *Node) watchNeeded(coin, orderID string, kind broadcastKind, confs int, depths map[string]int) bool {
	depth, ok := depths[coin]
	if !ok {
		depth = n.settleDepth(coin)
		depths[coin] = depth
	}
	live := false
	if orderID != "" {
		_, live = n.sessions[orderID]
	}
	return confs < depth || live || !n.refundSettleable(orderID, kind)
}

// migrateSettledArchive handles the settled section of pre-finalize files:
// entries that still need watching rejoin the watch table as active,
// finalized ones are dropped (history keeps the audit trail). Runs
// single-threaded at restore, before the engine starts.
func (n *Node) migrateSettledArchive(settled []persistedBroadcast) {
	var kept []persistedBroadcast
	depths := make(map[string]int)
	for _, p := range settled {
		if p.TxID == "" || p.Coin == "" {
			continue
		}
		if !n.watchNeeded(p.Coin, p.OrderID, broadcastKind(p.Kind), p.Confs, depths) {
			continue // finalized: never polled again
		}
		kept = append(kept, p)
	}
	if len(kept) > 0 {
		n.insertTracked(kept)
	}
	xlog.Info("settled broadcast archive migrated (finalized entries dropped, watch-worthy restored)",
		"dropped", len(settled)-len(kept), "restored", len(kept))
}

// trackedSnapshot returns a copy of the tracked table for tests and the
// status surface. Callers must not mutate the entries.
func (n *Node) trackedSnapshot() map[string]trackedBroadcast {
	n.trackedMu.Lock()
	defer n.trackedMu.Unlock()
	out := make(map[string]trackedBroadcast, len(n.tracked))
	for k, v := range n.tracked {
		out[k] = *v
	}
	return out
}

// pollBroadcastConfirmations refreshes confirmation depth for every tracked
// broadcast, each sweep. Finalized entries are deleted by the finalize pass,
// so they are never polled: a completed order costs zero wallet RPC on every
// tick and every restart. Engine-tick entry point: it snapshots connector
// handles and posts the wallet I/O to the worker pool (never blocks the
// engine on RPC — the established pendingRefunds/pendingWatch pattern), with
// a single-flight guard so sweeps never stack. The apply also runs the
// finalize pass so depth changes immediately drop finalized entries.
// Inline mode (tests) runs synchronously.
func (n *Node) pollBroadcastConfirmations() {
	if !n.pendingConfirm.CompareAndSwap(false, true) {
		return // a poll is already in flight; next tick retries
	}
	type pair struct {
		txid string
		coin string
	}
	var pairs []pair
	n.trackedMu.Lock()
	for txid, tb := range n.tracked {
		pairs = append(pairs, pair{txid, tb.Coin})
	}
	n.trackedMu.Unlock()
	if len(pairs) == 0 {
		n.pendingConfirm.Store(false)
		// Nothing due on the wire: still run the finalize pass (no wallet
		// I/O) so newly sessionless entries drop without waiting a tick.
		if n.finalizeWatchEntries() {
			n.persist()
		}
		return // nothing due: no connector resolution, no worker task
	}
	// Resolve connectors off-lock (wallet handles are goroutine-safe and
	// re-resolved every round, so a conf reload cannot stick).
	conns := make(map[string]wallet.Connector)
	byTx := make(map[string]wallet.Connector, len(pairs))
	for _, p := range pairs {
		c, ok := conns[p.coin]
		if !ok {
			var err error
			if c, err = n.connector(p.coin); err != nil || c == nil {
				continue // no connector: keep watching at last depth
			}
			conns[p.coin] = c
		}
		n.trackedMu.Lock()
		_, alive := n.tracked[p.txid]
		n.trackedMu.Unlock()
		if !alive {
			continue
		}
		byTx[p.txid] = c
	}
	if len(byTx) == 0 {
		n.pendingConfirm.Store(false)
		if n.finalizeWatchEntries() {
			n.persist()
		}
		return
	}
	task := workTask{
		run: func() (any, error) {
			depths := make(map[string]int, len(byTx))
			for txid, conn := range byTx {
				vtx, verr := conn.GetRawTransactionVerbose(txid)
				if verr != nil {
					continue // unknown/unreachable: keep last depth
				}
				if !vtx.HasConfirmations || vtx.Confirmations < 0 {
					continue // depth unasserted or conflicted: keep watching, never regress
				}
				depths[txid] = vtx.Confirmations
			}
			return depths, nil
		},
		apply: func(v any, err error) {
			n.pendingConfirm.Store(false)
			if err != nil {
				return
			}
			depths, _ := v.(map[string]int)
			confsChanged := false
			n.trackedMu.Lock()
			for txid, confs := range depths {
				if cur, ok := n.tracked[txid]; ok && cur.Confs != confs {
					cur.Confs = confs
					confsChanged = true
				}
			}
			n.trackedMu.Unlock()
			// Finalize pass: runs on the engine goroutine (apply contract),
			// reading n.sessions and the store — never under trackedMu.
			finalized := n.finalizeWatchEntries()
			if confsChanged || finalized {
				n.persist()
			}
			if confsChanged {
				xlog.Debug("reconcile: confirmation depths updated")
			}
		},
	}
	if !n.engineRunning.Load() {
		v, err := safeTaskRun(task)
		task.apply(v, err)
		return
	}
	select {
	case n.tasks <- task:
	default:
		n.pendingConfirm.Store(false)
		xlog.Debug("reconcile: confirm poll dropped, engine busy")
	}
}

// settleDepth returns the confirmation depth past which a sessionless tracked
// broadcast is finalized and dropped from the watch table: the deeper of the
// global trackedRetainDepth floor and the coin's own required Confirmations —
// the port never stops watching below the depth the swap logic itself
// requires.
func (n *Node) settleDepth(coin string) int {
	depth := trackedRetainDepth
	if cfg := n.cfg(); cfg != nil && cfg.Confs != nil {
		if cc, ok := cfg.Confs[coin]; ok && cc != nil && cc.Confirmations > depth {
			depth = cc.Confirmations
		}
	}
	return depth
}

// refundSettleable reports whether a tracked entry's refund obligations are
// reconciled so it can settle. Only refund-kind entries carry one:
// scanStoredRefunds re-posts the stored refund of a live non-terminal order
// whose txid is untracked, so the entry must stay active until the order is
// terminal or "rolled back" (its Confs>=1 reconcile has run). Every other
// kind has no post-broadcast reader.
func (n *Node) refundSettleable(orderID string, kind broadcastKind) bool {
	if kind != broadcastRefund {
		return true
	}
	o := n.store.Get(orderID)
	if o == nil {
		return true // history or unknown: nothing left to reconcile
	}
	return isOrderTerminal(o.Status) || statusString(o.Status) == "rolled back"
}

// finalizeWatchEntries drops finalized entries from the watch table:
// deep-confirmed + sessionless + refund-reconciled. A finalized entry is never
// polled again — the durable trade record stays in Store.history, so order
// visibility (dxGetOrder/MyOrders/Fills/History) is unchanged. A live
// session or an unreconciled refund keeps its entry watched. If a swap
// reopens later, recordBroadcast re-watches the txid.
//
// Tradeoff (operator decision): unlike the former hourly archive recheck, a
// finalized tx that later regresses below depth (deep reorg / SPV reset) is
// not re-detected — the entry is gone, so the rebroadcast sweep cannot regain
// ownership of it. Accepted because finalize requires depth >= settleDepth
// (>= 6, or the coin's own required Confirmations), past which a regression
// is not recoverable by re-polling alone. Engine-goroutine pass
// (reads n.sessions and the store — never under trackedMu); inline mode runs
// it single-threaded. Returns whether any entry was dropped so the caller
// persists.
func (n *Node) finalizeWatchEntries() bool {
	type snap struct {
		txid    string
		coin    string
		kind    broadcastKind
		orderID string
		confs   int
	}
	n.trackedMu.Lock()
	snaps := make([]snap, 0, len(n.tracked))
	for txid, tb := range n.tracked {
		snaps = append(snaps, snap{txid, tb.Coin, tb.Kind, tb.OrderID, tb.Confs})
	}
	n.trackedMu.Unlock()
	if len(snaps) == 0 {
		return false
	}
	depths := make(map[string]int)
	var drop []string
	for _, s := range snaps {
		if !n.watchNeeded(s.coin, s.orderID, s.kind, s.confs, depths) {
			drop = append(drop, s.txid)
		}
	}
	if len(drop) == 0 {
		return false
	}
	n.trackedMu.Lock()
	for _, txid := range drop {
		if tb, ok := n.tracked[txid]; ok {
			xlog.Info("reconcile: broadcast finalized, watch dropped", "order", tb.OrderID,
				"kind", string(tb.Kind), "coin", tb.Coin, "txid", txid, "confs", tb.Confs)
			delete(n.tracked, txid)
		}
	}
	n.trackedMu.Unlock()
	return true
}

// Bounds on the in-memory/persisted watch table. trackedCap bounds it:
// fully-confirmed entries (depth >= trackedRetainDepth) for orders with no
// live session are dropped once over the cap, oldest first by Seq.
// Live-session entries are never dropped here (Phase 1+ drivers may still
// need them). The Store.history records remain the durable trade audit trail
// (uncapped, like C++ m_historicTransactions); the watch table holds only
// in-flight broadcasts — finalized entries are deleted by
// finalizeWatchEntries, never capped.
const trackedCap = 500
const trackedRetainDepth = 6

// pruneTracked bounds the watch table (see trackedCap).
// Engine-tick only: it reads the engine-owned sessions map, so it must never
// run on a worker goroutine.
func (n *Node) pruneTracked() {
	type cand struct {
		txid string
		seq  uint64
	}
	n.trackedMu.Lock()
	defer n.trackedMu.Unlock()
	if len(n.tracked) <= trackedCap {
		return
	}
	// Cap fallback: collect droppable deep sessionless entries,
	// oldest (lowest Seq) first. A refund entry whose order is still live
	// and unreconciled is kept — deleting it would let scanStoredRefunds
	// re-post the refund every sweep (tracked => never re-posted).
	var drop []cand
	for txid, tb := range n.tracked {
		if tb.Confs < trackedRetainDepth {
			continue
		}
		if _, live := n.sessions[tb.OrderID]; live {
			continue
		}
		if !n.refundSettleable(tb.OrderID, tb.Kind) {
			continue
		}
		drop = append(drop, cand{txid, tb.Seq})
	}
	sort.Slice(drop, func(i, j int) bool { return drop[i].seq < drop[j].seq })
	for _, c := range drop {
		if len(n.tracked) <= trackedCap {
			break
		}
		delete(n.tracked, c.txid)
	}
}

// Phase-1 R1 rebroadcast: a tracked broadcast stuck at 0 confs past the
// coin-aware age threshold is rebroadcast with the IDENTICAL bytes (same txid
// — never rebuilt, never fee-bumped, so a rebroadcast can never double-spend:
// the network treats it as the same transaction). Attempts are capped
// (maxRebroadcasts); past the cap the entry stays under watch and logs WARN
// (alert-only — e.g. a confirmed double-spend made ours invalid, which only an
// operator can resolve). A failed send counts the attempt but keeps watching
// (transient wallet outage); a success restarts the age clock.

// maxRebroadcasts bounds rebroadcast tries per tracked txid.
const maxRebroadcasts = 10

// rebroadcastFloorMicro is the minimum age before a first rebroadcast.
const rebroadcastFloorMicro = 120 * 1000000

// rebroadcastAfterMicro returns the age (wall micros) after which an
// unconfirmed tracked broadcast of coin is due for rebroadcast: three block
// times worth of patience, floored so fast coins still get a grace window.
func (n *Node) rebroadcastAfterMicro(coin string) uint64 {
	blockTime := int64(60)
	if cfg := n.cfg(); cfg != nil && cfg.Confs != nil {
		if cc, ok := cfg.Confs[coin]; ok && cc != nil && cc.BlockTime > 0 {
			blockTime = int64(cc.BlockTime)
		}
	}
	after := blockTime * 3 * 1000000
	if after < rebroadcastFloorMicro {
		after = rebroadcastFloorMicro
	}
	return uint64(after)
}

// rebroadcastUnconfirmed rebroadcasts due tracked entries. Engine-tick entry
// point: connectors resolve on-engine (non-blocking map lookup), wallet I/O
// runs on the worker pool with a single-flight guard (the established
// pendingRefunds/pendingWatch pattern). Inline mode (tests) runs
// synchronously.
func (n *Node) rebroadcastUnconfirmed() {
	if !n.pendingRebroadcast.CompareAndSwap(false, true) {
		return // a rebroadcast is already in flight; next tick retries
	}
	now := uint64(NowMicro())
	type cand struct {
		txid      string
		coin      string
		hex       string
		firstSeen uint64
	}
	var cands []cand
	n.trackedMu.Lock()
	for txid, tb := range n.tracked {
		if tb.Confs != 0 || tb.Attempts >= maxRebroadcasts || tb.Hex == "" {
			continue
		}
		cands = append(cands, cand{txid, tb.Coin, tb.Hex, tb.FirstSeenMicro})
	}
	n.trackedMu.Unlock()
	// Age filter off-lock: rebroadcastAfterMicro takes cfgMu, which must
	// never be evaluated while holding trackedMu.
	type due struct {
		txid string
		coin string
		hex  string
	}
	var dues []due
	for _, c := range cands {
		if now < c.firstSeen || now-c.firstSeen <= n.rebroadcastAfterMicro(c.coin) {
			continue
		}
		dues = append(dues, due{c.txid, c.coin, c.hex})
	}
	byTx := make(map[string]wallet.Connector, len(dues))
	conns := make(map[string]wallet.Connector)
	for _, d := range dues {
		c, ok := conns[d.coin]
		if !ok {
			var err error
			if c, err = n.connector(d.coin); err != nil || c == nil {
				continue // no connector: keep watching at last depth
			}
			conns[d.coin] = c
		}
		n.trackedMu.Lock()
		tb, alive := n.tracked[d.txid]
		n.trackedMu.Unlock()
		if !alive {
			continue
		}
		// Re-check freshness under no lock using the re-read entry: the
		// poller may have confirmed it since the snapshot.
		if tb.Confs != 0 || tb.Attempts >= maxRebroadcasts {
			continue
		}
		byTx[d.txid] = c
	}
	hexes := make(map[string]string, len(byTx))
	n.trackedMu.Lock()
	for txid := range byTx {
		if tb, ok := n.tracked[txid]; ok {
			hexes[txid] = tb.Hex
		} else {
			delete(byTx, txid)
		}
	}
	n.trackedMu.Unlock()
	if len(byTx) == 0 {
		n.pendingRebroadcast.Store(false)
		return // nothing due: no worker task needed
	}
	task := workTask{
		run: func() (any, error) {
			ok := make(map[string]bool, len(byTx))
			for txid, conn := range byTx {
				if _, serr := conn.SendRawTransaction(hexes[txid]); serr != nil {
					ok[txid] = false
					continue
				}
				ok[txid] = true
			}
			return ok, nil
		},
		apply: func(v any, err error) {
			n.pendingRebroadcast.Store(false)
			if err != nil {
				return
			}
			ok, _ := v.(map[string]bool)
			changed := false
			now := uint64(NowMicro())
			n.trackedMu.Lock()
			for txid, sent := range ok {
				tb, alive := n.tracked[txid]
				if !alive {
					continue
				}
				tb.Attempts++
				if sent {
					tb.FirstSeenMicro = now
				}
				if tb.Attempts >= maxRebroadcasts {
					xlog.Warn("reconcile: broadcast never confirmed, giving up rebroadcast (operator attention)",
						"order", tb.OrderID, "kind", string(tb.Kind), "coin", tb.Coin, "txid", txid)
				} else if sent {
					xlog.Info("reconcile: rebroadcast unconfirmed tx", "order", tb.OrderID,
						"kind", string(tb.Kind), "coin", tb.Coin, "txid", txid, "attempt", tb.Attempts)
				}
				changed = true
			}
			n.trackedMu.Unlock()
			if changed {
				n.persist()
			}
		},
	}
	if !n.engineRunning.Load() {
		v, err := safeTaskRun(task)
		task.apply(v, err)
		return
	}
	select {
	case n.tasks <- task:
	default:
		n.pendingRebroadcast.Store(false)
		xlog.Debug("reconcile: rebroadcast dropped, engine busy")
	}
}
