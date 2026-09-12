package api

import (
	"encoding/hex"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"go-xbridge/proto"
	"go-xbridge/swap"
)

// Store is the in-memory XBridge order book. It is populated by the P2P feed
// (xbcPendingTransaction / xbcTransaction broadcasts) and by locally created
// orders, and is the backing data for the dxGet* read methods. It is safe for
// concurrent use.
//
// Ownership model: the book's live *Order records are mutated ONLY through the
// store's own methods (Add/Update/Touch/Remove/...), each under s.mu. Read
// methods (Get/List/Mine/Locked/LockedUtxoInfo) return snapshot copies, so no
// *Order pointer ever escapes for out-of-lock mutation — a reader can never
// observe a torn order or race the writer.
//
// The append-only histories (fills/history) are bounded: each is trimmed to its
// cap on write so the store cannot grow without bound. Cancelled orders are NOT
// tracked in a separate ledger — they stay in the live book (status "canceled")
// and history until dxFlushCancelledOrders prunes them, mirroring C++ which
// erases trCancelled entries from m_transactions and m_historicTransactions
// (xbridgeapp.cpp:1331-1354).
type Store struct {
	mu      sync.RWMutex
	orders  map[string]*Order
	fills   []fillEntry // recent completed fills (session-scoped, like C++)
	locked  map[string]*Order
	history []historyEntry // removed/cancelled orders kept for dxGetOrderHistory fidelity
	// reserved holds the "txid:vout" keys (display order) committed atomically
	// by a take before its Accepting packet leaves (C++ lockCoins/lockFeeUtxos
	// under m_utxosOrderLock, xbridgeapp.cpp:2236-2267). The reservation is
	// exposed through LockedUtxoInfo immediately, folded into the owner order's
	// Utxos/FeeUtxos on submit, and released on any take error path. Keys are
	// split into the BLOCK fee inputs (global exclusion, C++ m_feeUtxos) and
	// the taker's funding inputs on fundCurrency (C++ m_utxosDict[token]) so
	// lock exclusion stays per-token like getAllLockedUtxos.
	reserved map[string]reservedKeys
}

// reservedKeys splits a take's in-flight claim into the two C++ lock sets it
// mirrors: fee inputs are BLOCK-chain (m_feeUtxos, excluded for every
// currency) while the taker's funding inputs live on fundCurrency
// (m_utxosDict[fundCurrency], excluded only when that token is checked).
type reservedKeys struct {
	fee          []string // BLOCK fee-input keys, global exclusion
	fund         []string // taker funding keys on fundCurrency
	fundCurrency string   // ticker of the funding wallet (the order's ToCurrency)
}

const (
	maxStoreFills   = 1000 // bound on s.fills (bounded history)
	maxStoreHistory = 1000 // bound on s.history (bounded history)
)

// trimOldest returns s with at most max elements, dropping the oldest entries
// from the front.
func trimOldest[S ~[]E, E any](s S, max int) S {
	if len(s) > max {
		return s[len(s)-max:]
	}
	return s
}

// historyEntry is a removed order's terminal record (C++ moveTransactionToHistory),
// carrying a full snapshot of the order so finished/cancelled local orders stay
// renderable via dxGetMyOrders / dxGetOrder (C++ m_historicTransactions holds
// complete TransactionDescrPtrs, not thin records).
type historyEntry struct {
	ID      string
	Status  string // e.g. "canceled"
	Reason  uint32
	Updated uint64
	Order   *Order // snapshot of the order at removal time (nil when unavailable)
}

type fillEntry struct {
	ID        string
	Time      uint64 // microseconds since epoch
	Maker     string
	MakerSize string
	Taker     string
	TakerSize string
	ParentID  string
	PartialID string
	// Partial-order fields, carried so dxGetOrderFills can echo C++'s full
	// 12-field fill object.
	OrderType            string
	PartialMinimum       string
	PartialOrigMakerSize string
	PartialOrigTakerSize string
	PartialRepost        bool
}

// flushedOrder is one cancelled order removed by FlushCancelled (C++
// App::FlushedOrder, xbridgeapp.cpp:1331). ID is the raw [32]byte so the
// C++ std::map id ordering (orderIDLess) can be reproduced; the handler renders
// the display hex. UseCount mirrors ptr.use_count() (the owning map reference);
// Go has no shared_ptr, so the debug-only refcount is reported as 1.
type flushedOrder struct {
	ID       [32]byte
	Txtime   uint64
	UseCount int
}

// NewStore returns an empty order store.
func NewStore() *Store {
	return &Store{
		orders:   make(map[string]*Order),
		locked:   make(map[string]*Order),
		reserved: make(map[string]reservedKeys),
	}
}

func orderKey(id [32]byte) string { return hexEncode(id[:]) }

// Add inserts or replaces an order by id. The caller transfers ownership of o
// (the store keeps it as the live record); it must not be mutated afterward.
func (s *Store) Add(o *Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[orderKey(o.ID)] = o
}

// Get returns a snapshot copy of the order with the given hex id, or nil.
func (s *Store) Get(idHex string) *Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o := s.orders[idHex]
	if o == nil {
		return nil
	}
	return o.Copy()
}

// Update applies fn to the live order for idHex under the store lock and
// reports whether the order existed. This is the ONLY way to mutate an order's
// fields: the callback runs while the book lock is held, so a concurrent
// reader snapshot can never observe a torn update. fn must not retain the
// pointer.
func (s *Store) Update(idHex string, fn func(*Order)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.orders[idHex]; o != nil {
		fn(o)
		return true
	}
	return false
}

// Touch refreshes the Updated timestamp of a live, non-canceled order (a
// relayed broadcast of a known order — C++ processPendingTransaction only bumps
// the timestamp). It reports whether a live non-canceled record was bumped;
// callers fall through to Add when false (unknown order only — canceled or
// historic orders are checked via HasOrder, mirroring C++ appendTransaction's
// history guard).
func (s *Store) Touch(idHex string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ex := s.orders[idHex]; ex != nil && ex.Status != "canceled" {
		ex.Updated = NowMicro()
		return true
	}
	return false
}

// List returns snapshot copies of all orders (unfiltered). Callers apply
// filtering (e.g. skip cancelled/finished/expired older than 1 minute, as
// dxGetOrders does).
func (s *Store) List() []*Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o.Copy())
	}
	return out
}

// Remove deletes an order by id.
func (s *Store) Remove(idHex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.orders, idHex)
}

// historyLocked appends a terminal history entry under the held lock. It is a
// no-op when the id is already recorded (C++ moveTransactionToHistory returns
// early on a duplicate) and trims to the bounded cap.
func (s *Store) historyLocked(idHex, status string, reason uint32, updated uint64, o *Order) {
	for _, e := range s.history {
		if e.ID == idHex {
			return // already recorded: no duplicate entries
		}
	}
	var snap *Order
	if o != nil {
		snap = o.Copy()
		snap.Status = status
		snap.Updated = updated
	}
	s.history = append(s.history, historyEntry{
		ID:      idHex,
		Status:  status,
		Reason:  reason,
		Updated: updated,
		Order:   snap,
	})
	s.history = trimOldest(s.history, maxStoreHistory)
}

// MoveToHistory deletes the live order and appends a terminal history record,
// mirroring C++ App::moveTransactionToHistory (xbridgesession.cpp:3385). Used
// by the remote-cancel path when an order has no deposit yet and by OnFinished.
// Idempotent: a missing live order (already moved, or never present) is a no-op.
func (s *Store) MoveToHistory(idHex, status string, reason, updated uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.orders[idHex]
	if o == nil {
		return
	}
	delete(s.orders, idHex)
	s.historyLocked(idHex, status, uint32(reason), updated, o)
}

// MoveToHistoryU32 is MoveToHistory with a uint32 reason (C++ TxCancelReason).
func (s *Store) MoveToHistoryU32(idHex, status string, reason uint32, updated uint64) {
	s.MoveToHistory(idHex, status, uint64(reason), updated)
}

// HasOrder reports whether idHex is known to the store — either live in the
// active orders map (including canceled-but-not-yet-moved orders) or in the
// bounded history. Mirrors C++ App::transaction(), which consults both
// m_transactions and m_historicTransactions, and backs appendTransaction's
// history guard that prevents a network rebroadcast from re-accepting a
// canceled order.
func (s *Store) HasOrder(idHex string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.orders[idHex]; ok {
		return true
	}
	for _, e := range s.history {
		if e.ID == idHex {
			return true
		}
	}
	return false
}

// AddToHistory appends a terminal history record for o without touching the
// live orders map (used by restoreSwap to rebuild history from a persisted
// terminal swap, mirroring C++ loadOrders). Idempotent on duplicate ids.
func (s *Store) AddToHistory(o *Order, status string, reason, updated uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyLocked(hexEncode(o.ID[:]), status, uint32(reason), updated, o)
}

// HistoryOrder returns the snapshot copy of a removed order by id, or nil.
// This backs the dxGetOrder history fallback (C++ App::transaction checks
// m_historicTransactions when the live map misses).
func (s *Store) HistoryOrder(idHex string) *Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.history {
		if e.ID == idHex && e.Order != nil {
			return e.Order.Copy()
		}
	}
	return nil
}

// History returns the removed/cancelled order records.
func (s *Store) History() []historyEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]historyEntry, len(s.history))
	for i, e := range s.history {
		out[i] = e
		if e.Order != nil {
			out[i].Order = e.Order.Copy()
		}
	}
	return out
}

// RemovePendingPackets is the thin-client equivalent of C++
// xapp.removePackets: go-xbridge holds no pending-packet queue, so there is
// nothing to drop. It exists to mirror the call site verbatim.
func (s *Store) RemovePendingPackets(idHex string) {}

// Mine returns snapshot copies of orders created locally by this node.
func (s *Store) Mine() []*Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Order, 0)
	for _, o := range s.orders {
		if o.Mine {
			out = append(out, o.Copy())
		}
	}
	return out
}

// PruneUnconnected removes non-local orders whose from/to currency has no
// connector, keeping local orders always. Mirrors C++ App::clearNonLocalOrders
// (xbridgeapp.cpp:3811-3821), which dxLoadXBridgeConf invokes only when
// showAllOrders is false: an order you hold no wallet for is unusable, so it is
// dropped from the live book (local orders are kept regardless — you act on
// them through the hub).
func (s *Store) PruneUnconnected(kept map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, o := range s.orders {
		if o.Mine {
			continue
		}
		if !kept[o.FromCurrency] || !kept[o.ToCurrency] {
			delete(s.orders, id)
		}
	}
}

// PruneExpired removes open-book orders that have exceeded their TTL, mirroring
// C++ App::Impl::checkAndEraseExpiredTransactions (xbridgeapp.cpp:3573-3654) —
// the periodic sweep driven by the 15 s timer. It applies the expiry predicates
// to the OPEN book: status "open" (trPending, written by MakeOrder for a
// not-yet-taken local order and kept alive by the 240 s rebroadcast heartbeat,
// api/node.go rebroadcastOpenOrders) and status "created" (trCreated, the
// in-swap state — protected from this sweep by the inSwap guard below). In-swap
// and terminal orders are never swept here — they leave the book via their own
// lifecycle paths.
//
// inSwap is the set of order ids whose client-side handshake has STARTED (the
// node's live sessions past their initial pre-swap state, api/swap.go
// setOrderStatus advancing the order to hold/initialized/created as the swap
// drives it — xbridgesession.cpp:1529/1762/2174). The session-state guard is
// what tells an in-swap maker from an in-swap taker (both advance their Status)
// and keeps the sweep from pruning a maker mid-swap and releasing its deposit's
// utxo locks (C++ protects it by advancing the descriptor to trHold,
// xbridgesession.cpp:1529). Unmatched maker orders (session still at its initial
// state) remain sweepable.
//
// Expired orders are REMOVED without a history entry, matching C++
// eraseExpiredTransactions (xbridgeexchange.cpp:712-747, erases from
// m_pendingTransactions without moveTransactionToHistory), and removal
// auto-releases their locked-utxo contribution (lockedInfoLocked derives the
// locked set from the live orders map). Pending partial orders waiting on their
// prep-tx confirmation are never swept (C++ guards with !isOrderPending(),
// xbridgeapp.cpp:3606-3607).
//
// Predicates (strict `>`; an order exactly at a TTL is not expired). The time
// rules blend the C++ APP-side inactivity sweep that governs a trader's own
// orders (xbridgeapp.cpp:3604-3634) with the exchange-side block/deadline
// predicates (xbridgetransaction.cpp:268-311, xbridgeexchange.cpp:712-747):
//   - "created" (trCreated=6, the in-swap state): block-expired (tip −
//     BlockNumber > BlocksTTL), or created-age > DeadlineTTL, or last-activity
//     age > TTL. A live in-swap "created" order is protected from this sweep by
//     the node-level inSwap session guard (api/node.go pruneExpired); these
//     bounds fire only for an orphaned in-swap order with no live session.
//   - "open" (trPending): last-activity age > PendingTTL, or created-age >
//     DeadlineTTL. C++ flips trPending to trExpired once inactive for pendingTTL
//     (6 min) (xbridgeapp.cpp:3611-3616) and the order book only surfaces
//     trPending (rpcxbridge.cpp:1591,1606), so an inactive order leaves the book
//     at the 6 min mark. Block-height expiry never applies past trNew (C++
//     :295-296 short-circuit).
//
// The created-age > DeadlineTTL for trNew is the exchange-side isExpired rule
// (C++ app-side never applies the 7-day deadline to trNew directly); Go keeps
// it because the port has no trNew→trOffline/trPending flip, so it is the only
// bound on a "created" order whose relays keep its activity age fresh forever.
//
// now is the sweep time; currentBlock is the BLOCK-chain tip height (0 when
// unknown — block-height expiry is skipped). A BlockNumber of 0 (unknown,
// legacy persisted records) also skips block-height expiry. Returns the hex ids
// removed, in ascending id order (callers log them deterministically).
func (s *Store) PruneExpired(now time.Time, currentBlock uint32, inSwap map[string]bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	nowUs := uint64(now.UnixMicro())
	var pruned []string
	for key, o := range s.orders {
		if inSwap[key] {
			continue
		}
		if o.Status != "created" && o.Status != "open" {
			continue
		}
		// C++ isOrderPending: a partial order waiting on its prep-tx split is
		// excluded from the expiry sweep (xbridgeapp.cpp:3606-3607).
		if o.PrepTx != "" {
			continue
		}
		// Age in whole seconds, flooring the µs difference (C++ total_seconds()
		// truncates the duration toward zero).
		created := ageSec(nowUs, o.Created)
		updated := ageSec(nowUs, o.Updated)
		expired := false
		switch o.Status {
		case "created": // C++ trCreated=6 (in-swap; node-level sweep protects it via the inSwap session guard)
			blockExpired := o.BlockNumber != 0 && currentBlock != 0 &&
				currentBlock > o.BlockNumber && currentBlock-o.BlockNumber > swap.BlocksTTL
			expired = blockExpired || created > swap.DeadlineTTL || updated > swap.TTL
		case "open": // C++ trPending
			expired = updated > swap.PendingTTL || created > swap.DeadlineTTL
		}
		if expired {
			delete(s.orders, key)
			pruned = append(pruned, key)
		}
	}
	sort.Strings(pruned)
	return pruned
}

// ageSec returns the whole-second age of a microsecond timestamp relative to a
// microsecond now: max(0, floor((now−ts)/1e6)). Flooring the DIFFERENCE matches
// C++ boost::posix_time::time_duration::total_seconds() (xbridgeapp.cpp:3602),
// and a timestamp ahead of now (clock skew) yields 0, never a negative age, so
// it is never "expired" (C++ computes a negative duration, which also fails the
// strict `>` check).
func ageSec(nowUs, ts uint64) uint64 {
	if ts >= nowUs {
		return 0
	}
	return (nowUs - ts) / 1000000
}

// AddFill records a completed fill (used by dxGetOrderFills / dxGetOrderHistory).
func (s *Store) AddFill(f fillEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fills = append(s.fills, f)
	s.fills = trimOldest(s.fills, maxStoreFills)
}

// Fills returns recorded fills, most recent first.
func (s *Store) Fills() []fillEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]fillEntry, len(s.fills))
	copy(out, s.fills)
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Lock marks an order's UTXOs as locked (dxGetLockedUtxos).
func (s *Store) Lock(o *Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locked[orderKey(o.ID)] = o
}

// Locked returns snapshot copies of currently locked orders.
func (s *Store) Locked() []*Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Order, 0, len(s.locked))
	for _, o := range s.locked {
		out = append(out, o.Copy())
	}
	return out
}

// isOrderTerminal reports whether an order's reserved UTXOs have been released
// (the swap has ended / the order was dropped). Mirrors C++'s lock release once
// a Transaction reaches a terminal state — only non-terminal orders still hold
// their maker UTXOs, so only those contribute to the locked set.
func isOrderTerminal(status string) bool {
	switch statusString(status) {
	case "finished", "canceled", "dropped", "invalid":
		return true
	}
	return false
}

// utxoEntryKey returns the "txid:vout" lock key (display order) for a UTXO entry
// carried in an order body. proto.UtxoEntry.TxID is stored little-endian, so it
// is reversed to display order before hex-encoding (matching wallet UTXO keys).
func utxoEntryKey(e proto.UtxoEntry) string {
	var rev [32]byte
	for i := 0; i < 32; i++ {
		rev[i] = e.TxID[31-i]
	}
	return hexEncode(rev[:]) + ":" + strconv.FormatUint(uint64(e.Vout), 10)
}

// lockedInfoLocked returns the "txid:vout" (display order) reservation set plus
// the order id owning each key, under a held lock. Non-terminal orders
// contribute their committed Utxos (via utxoEntryKey) and FeeUtxos (plain
// "txid:vout"), and active take reservations (s.reserved) count too — C++
// reserves the fee utxos of an accepting order via lockFeeUtxos
// (xbridgeapp.cpp:2267) before selecting the taker's funding set, so a
// concurrent take cannot double-spend them. wallet.Utxo.TxID is display order,
// so the key is the plain "txid:vout" (no reversal).
func (s *Store) lockedInfoLocked() (keys map[string]bool, byOrder map[string]string) {
	keys = map[string]bool{}
	byOrder = map[string]string{}
	for _, o := range s.orders {
		if orderLockReleased(o.Status) {
			continue
		}
		oid := orderIDString(o.ID)
		for _, u := range o.Utxos {
			k := utxoEntryKey(u)
			keys[k] = true
			byOrder[k] = oid
		}
		for _, u := range o.FeeUtxos {
			k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
			keys[k] = true
			byOrder[k] = oid
		}
	}
	for oid, list := range s.reserved {
		// A reservation only holds while its owner order is live and
		// non-terminal; once the order ends the take cannot proceed, so the
		// in-flight inputs are released like committed ones (isOrderTerminal).
		// Rolled-back orders release too (orderLockReleased): their refund
		// succeeded, so the committed inputs are spent. The reported owner
		// is the display id (orderIDString), matching the committed-input
		// loop above.
		if o := s.orders[oid]; o == nil || orderLockReleased(o.Status) {
			continue
		}
		disp := orderIDString(s.orders[oid].ID)
		for _, k := range list.fee {
			keys[k] = true
			byOrder[k] = disp
		}
		for _, k := range list.fund {
			keys[k] = true
			byOrder[k] = disp
		}
	}
	return keys, byOrder
}

// LockedUtxoInfo returns the set of "txid:vout" (display order) reserved by
// active orders, plus a map from each key to the hex id of the order locking it.
// dxGetLockedUtxos / dxGetUtxos / dxGetTokenBalances consume this so locked
// coins are reported and excluded from available balances, matching C++.
func (s *Store) LockedUtxoInfo() (keys map[string]bool, byOrder map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lockedInfoLocked()
}

// utxoCurrency returns the chain an order's locked Utxos live on: the explicit
// UtxoCurrency tag (set at make/take) when present, else the Role-derived rule
// for legacy persisted records that predate the tag ('B' taker funds on
// ToCurrency, everyone else on FromCurrency — matches where make/take set the
// tag). An empty result (degenerate order with no currencies) keeps the Utxos
// excluded for every ticker rather than growing re-selectable.
func utxoCurrency(o *Order) string {
	if o.UtxoCurrency != "" {
		return o.UtxoCurrency
	}
	if o.Role == 'B' {
		return o.ToCurrency
	}
	return o.FromCurrency
}

// LockedUtxoInfoFor returns the exclusion set C++ App::getAllLockedUtxos(token)
// (xbridgeapp.cpp:2827-2834) yields for one token: the GLOBAL fee utxo set
// (m_feeUtxos — every order's FeeUtxos and every reservation's fee inputs) plus
// the coins locked on that specific token (m_utxosDict[token] — orders whose
// Utxos are tagged token and reservations whose fundCurrency is token). The
// take/make funding and balance-check paths use it so a check only excludes
// utxos locked on the checked chain, matching C++ getUnspent's excluded set.
func (s *Store) LockedUtxoInfoFor(ticker string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := map[string]bool{}
	for _, o := range s.orders {
		if orderLockReleased(o.Status) {
			continue
		}
		for _, u := range o.FeeUtxos {
			keys[u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10)] = true
		}
		if cur := utxoCurrency(o); cur != "" && cur != ticker {
			continue
		}
		for _, u := range o.Utxos {
			keys[utxoEntryKey(u)] = true
		}
	}
	for oid, r := range s.reserved {
		if o := s.orders[oid]; o == nil || orderLockReleased(o.Status) {
			continue
		}
		for _, k := range r.fee {
			keys[k] = true
		}
		if r.fundCurrency == ticker {
			for _, k := range r.fund {
				keys[k] = true
			}
		}
	}
	return keys
}

// takeReserve is the outcome of ReserveForTake. It distinguishes C++'s two
// distinct take-refusal paths: the order state gate (xbridgeapp.cpp:2122-2125,
// "not accepting, order already accepted", BAD_REQUEST) and the utxo lock
// collision (lockFeeUtxos + lockCoins, "cannot reuse utxo inputs",
// INSUFFICIENT_FUNDS).
type takeReserve int

const (
	// reserveOrderBusy reports a second in-flight take of the same order. C++
	// refuses any accept while the order's state is already >= trAccepting
	// (xbridgeapp.cpp:2122); Go's analog is the single in-flight reservation,
	// so a concurrent take can never overwrite the first take's claimed keys.
	// Unlike C++'s persistent trAccepting state, the gate only spans the
	// reservation window: ReleaseReserve clears it on submit, so sequential
	// re-takes of a settled order are still allowed (Go divergence, kept for
	// TestDxTakeOrderFullTake).
	reserveOrderBusy takeReserve = iota
	// reserveKeyCollision reports that a claimed "txid:vout" key is already
	// locked by another non-terminal order's committed Utxos/FeeUtxos or by an
	// active reservation ("cannot reuse utxo inputs").
	reserveKeyCollision
	// reserveOrderGone reports the order is absent or terminal (the take's
	// authoritative re-check under the store lock).
	reserveOrderGone
	// reserveOK reports the keys were claimed for this take.
	reserveOK
)

// ReserveForTake atomically reserves the "txid:vout" (display order) keys for
// the order identified by key, mirroring C++ lockFeeUtxos + lockCoins
// (xbridgeapp.cpp:2267) behind the state gate (xbridgeapp.cpp:2122). The
// claimed keys are split: feeKeys are the BLOCK fee inputs (global, C++
// m_feeUtxos) and fundKeys are the taker's funding inputs on fundCurrency
// (C++ m_utxosDict[fundCurrency]). The same-order gate fires FIRST: an order
// with an in-flight reservation is refused with reserveOrderBusy (BAD_REQUEST)
// before any utxo work, matching C++'s check order. Otherwise any overlap with
// an already-reserved key — another non-terminal order's committed
// Utxos/FeeUtxos or an active reservation — fails the take with
// reserveKeyCollision ("cannot reuse utxo inputs"). On success the reservation
// is immediately visible through LockedUtxoInfo / LockedUtxoInfoFor, so a
// concurrent take can never select the same inputs. The reservation is folded
// into the owner order on submit (Update) and must be released (ReleaseReserve)
// on any take error path.
func (s *Store) ReserveForTake(key string, feeKeys, fundKeys []string, fundCurrency string) takeReserve {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.orders[key]; o == nil || isOrderTerminal(o.Status) {
		return reserveOrderGone
	}
	if _, ok := s.reserved[key]; ok {
		return reserveOrderBusy
	}
	locked, _ := s.lockedInfoLocked()
	for _, k := range feeKeys {
		if locked[k] {
			return reserveKeyCollision
		}
	}
	for _, k := range fundKeys {
		if locked[k] {
			return reserveKeyCollision
		}
	}
	if len(feeKeys)+len(fundKeys) > 0 {
		s.reserved[key] = reservedKeys{fee: feeKeys, fund: fundKeys, fundCurrency: fundCurrency}
	}
	return reserveOK
}

// ReleaseReserve drops a take's in-flight reservation (error paths where the
// order never submits). Success releases it implicitly: the take's Update
// commits the same keys into the order's Utxos/FeeUtxos.
func (s *Store) ReleaseReserve(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reserved, key)
}

// FlushCancelled prunes cancelled orders whose txtime is older than
// minAgeMillis from BOTH the live book and history, mirroring C++
// App::flushCancelledOrders (xbridgeapp.cpp:1331-1354) which erases
// trCancelled transactions from m_transactions AND m_historicTransactions.
// keepTime = now - minAgeMillis(ms); orders with txtime < keepTime
// are removed and returned (the rest are kept). A minAgeMillis of 0 prunes
// everything regardless of age. The returned list is ordered the way C++
// iterates its std::maps — the live book first, then history, each in
// uint256-ascending (id) order.
func (s *Store) FlushCancelled(minAgeMillis uint64) []flushedOrder {
	s.mu.Lock()
	defer s.mu.Unlock()
	// keepTime = now - minAgeMillis(ms). A huge age whose µs conversion would
	// overflow uint64 is skipped entirely, leaving keepTime at 0 (the epoch):
	// `Updated < 0` matches nothing, so nothing is pruned — matching C++ where
	// now - an absurd age is far in the past (bpt::ptime cannot be negative).
	now := NowMicro()
	keepTime := uint64(0)
	if minAgeMillis <= math.MaxInt64/1000 {
		sub := uint64(minAgeMillis) * 1000
		if sub <= now {
			keepTime = now - sub
		}
	}
	var liveFlushed, histFlushed []flushedOrder
	for key, o := range s.orders {
		if statusString(o.Status) == "canceled" && o.Updated < keepTime {
			liveFlushed = append(liveFlushed, flushedOrder{ID: o.ID, Txtime: o.Updated, UseCount: 1})
			delete(s.orders, key)
		}
	}
	kept := s.history[:0]
	for _, e := range s.history {
		if statusString(e.Status) == "canceled" && e.Updated < keepTime {
			histFlushed = append(histFlushed, flushedOrder{ID: historyOrderID(e), Txtime: e.Updated, UseCount: 1})
		} else {
			kept = append(kept, e)
		}
	}
	s.history = kept
	// C++ iterates each std::map in uint256 ascending (id order), live first
	// then history (xbridgeapp.cpp:1336,1340); reproduce that per-block order.
	sort.SliceStable(liveFlushed, func(i, j int) bool {
		return orderIDLess(liveFlushed[i].ID, liveFlushed[j].ID)
	})
	sort.SliceStable(histFlushed, func(i, j int) bool {
		return orderIDLess(histFlushed[i].ID, histFlushed[j].ID)
	})
	return append(liveFlushed, histFlushed...)
}

// historyOrderID returns the raw [32]byte id of a history record (from the
// order snapshot when present, else decoded from the store-key hex), so a
// flushed history entry can be rendered/ordered like the C++ descriptor id.
func historyOrderID(e historyEntry) [32]byte {
	if e.Order != nil {
		return e.Order.ID
	}
	raw, err := hex.DecodeString(e.ID)
	if err != nil || len(raw) != 32 {
		return [32]byte{}
	}
	var b [32]byte
	copy(b[:], raw)
	return b
}

// NowMicro returns the current time in microseconds since epoch (mirrors C++
// timeToInt(second_clock::universal_time())). Centralized so tests can override.
var NowMicro = func() uint64 {
	return uint64(time.Now().UnixMicro())
}
