package api

import (
	"strconv"
	"sync"
	"time"

	"go-xbridge/proto"
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
// The append-only histories (fills/history/cancelled) are bounded: each is
// trimmed to its cap on write so the store cannot grow without bound.
type Store struct {
	mu        sync.RWMutex
	orders    map[string]*Order
	fills     []fillEntry // recent completed fills (session-scoped, like C++)
	locked    map[string]*Order
	cancelled []cancelledEntry
	history   []historyEntry // removed/cancelled orders kept for dxGetOrderHistory fidelity
}

const (
	maxStoreFills   = 1000 // bound on s.fills (F9: bounded history)
	maxStoreHistory = 1000 // bound on s.history (F9: bounded history)
	maxCancelled    = 1000 // bound on s.cancelled (in addition to the age prune)
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

type cancelledEntry struct {
	ID       string
	Txtime   uint64
	UseCount int
}

// NewStore returns an empty order store.
func NewStore() *Store {
	return &Store{
		orders: make(map[string]*Order),
		locked: make(map[string]*Order),
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

// LockedUtxoInfo returns the set of "txid:vout" (display order) reserved by
// active orders, plus a map from each key to the hex id of the order locking it.
// dxGetLockedUtxos / dxGetUtxos / dxGetTokenBalances consume this so locked
// coins are reported and excluded from available balances, matching C++.
func (s *Store) LockedUtxoInfo() (keys map[string]bool, byOrder map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys = map[string]bool{}
	byOrder = map[string]string{}
	for _, o := range s.orders {
		if isOrderTerminal(o.Status) {
			continue
		}
		oid := orderIDString(o.ID)
		for _, u := range o.Utxos {
			k := utxoEntryKey(u)
			keys[k] = true
			byOrder[k] = oid
		}
		// C++ reservers the fee utxos of an accepting order via lockFeeUtxos
		// (xbridgeapp.cpp:2267) before selecting the taker's funding set, so a
		// concurrent take cannot double-spend them. wallet.Utxo.TxID is display
		// order, so the key is the plain "txid:vout" (no reversal).
		for _, u := range o.FeeUtxos {
			k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
			keys[k] = true
			byOrder[k] = oid
		}
	}
	return keys, byOrder
}

// RecordCancelled records a flushed cancelled order (dxFlushCancelledOrders).
func (s *Store) RecordCancelled(id string, txtime uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = append(s.cancelled, cancelledEntry{ID: id, Txtime: txtime, UseCount: 1})
	s.cancelled = trimOldest(s.cancelled, maxCancelled)
}

// FlushCancelled prunes cancelled orders whose txtime is older than
// minAgeMillis, mirroring C++ dxFlushCancelledOrders. keepTime = now -
// minAgeMillis(ms); entries with Txtime < keepTime are removed and returned in
// the flushed subset (the rest are kept). A minAgeMillis of 0 prunes everything
// regardless of age.
func (s *Store) FlushCancelled(minAgeMillis uint64) []cancelledEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	// keepTime = now - minAgeMillis(ms). A very large age would make the
	// subtrahend overflow uint64 and wrap; clamp so every entry is pruned.
	now := NowMicro()
	sub := uint64(minAgeMillis) * 1000
	keepTime := uint64(0)
	if sub <= now {
		keepTime = now - sub
	}
	flushed := make([]cancelledEntry, 0)
	kept := make([]cancelledEntry, 0, len(s.cancelled))
	for _, c := range s.cancelled {
		if c.Txtime < keepTime {
			flushed = append(flushed, c)
		} else {
			kept = append(kept, c)
		}
	}
	s.cancelled = kept
	return flushed
}

// NowMicro returns the current time in microseconds since epoch (mirrors C++
// timeToInt(second_clock::universal_time())). Centralized so tests can override.
var NowMicro = func() uint64 {
	return uint64(time.Now().UnixMicro())
}
