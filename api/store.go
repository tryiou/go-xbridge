package api

import (
	"strconv"
	"sync"
	"time"

	"xbridge-go/proto"
)

// Store is the in-memory XBridge order book. It is populated by the P2P feed
// (xbcPendingTransaction / xbcTransaction broadcasts) and by locally created
// orders, and is the backing data for the dxGet* read methods. It is safe for
// concurrent use.
type Store struct {
	mu        sync.RWMutex
	orders    map[string]*Order
	fills     []fillEntry // recent completed fills (session-scoped, like C++)
	locked    map[string]*Order
	cancelled []cancelledEntry
	history   []historyEntry // removed/cancelled orders kept for dxGetOrderHistory fidelity
}

// historyEntry is a removed order's terminal record (C++ moveTransactionToHistory).
type historyEntry struct {
	ID      string
	Status  string // e.g. "canceled"
	Reason  uint32
	Updated uint64
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

// Add inserts or replaces an order by id.
func (s *Store) Add(o *Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[orderKey(o.ID)] = o
}

// Get returns the order with the given hex id, or nil.
func (s *Store) Get(idHex string) *Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.orders[idHex]
}

// List returns all orders (unfiltered). Callers apply filtering (e.g. skip
// cancelled/finished/expired older than 1 minute, as dxGetOrders does).
func (s *Store) List() []*Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o)
	}
	return out
}

// Remove deletes an order by id.
func (s *Store) Remove(idHex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.orders, idHex)
}

// MoveToHistory deletes the live order and appends a terminal history record,
// mirroring C++ App::moveTransactionToHistory (xbridgesession.cpp:3385). Used
// by the remote-cancel path when an order has no deposit yet.
func (s *Store) MoveToHistory(idHex, status string, reason, updated uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.orders, idHex)
	s.history = append(s.history, historyEntry{
		ID:      idHex,
		Status:  status,
		Reason:  uint32(reason),
		Updated: updated,
	})
}

// MoveToHistoryU32 is MoveToHistory with a uint32 reason (C++ TxCancelReason).
func (s *Store) MoveToHistoryU32(idHex, status string, reason uint32, updated uint64) {
	s.MoveToHistory(idHex, status, uint64(reason), updated)
}

// History returns the removed/cancelled order records.
func (s *Store) History() []historyEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]historyEntry, len(s.history))
	copy(out, s.history)
	return out
}

// RemovePendingPackets is the thin-client equivalent of C++
// xapp.removePackets: xbridge-go holds no pending-packet queue, so there is
// nothing to drop. It exists to mirror the call site verbatim.
func (s *Store) RemovePendingPackets(idHex string) {}

// Mine returns orders created locally by this node.
func (s *Store) Mine() []*Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Order, 0)
	for _, o := range s.orders {
		if o.Mine {
			out = append(out, o)
		}
	}
	return out
}

// AddFill records a completed fill (used by dxGetOrderFills / dxGetOrderHistory).
func (s *Store) AddFill(f fillEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fills = append(s.fills, f)
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

// Locked returns currently locked orders.
func (s *Store) Locked() []*Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Order, 0, len(s.locked))
	for _, o := range s.locked {
		out = append(out, o)
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
		oid := hexEncode(o.ID[:])
		for _, u := range o.Utxos {
			k := utxoEntryKey(u)
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
