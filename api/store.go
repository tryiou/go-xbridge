package api

import (
	"sync"
	"time"
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

// RecordCancelled records a flushed cancelled order (dxFlushCancelledOrders).
func (s *Store) RecordCancelled(id string, txtime uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = append(s.cancelled, cancelledEntry{ID: id, Txtime: txtime, UseCount: 1})
}

// NowMicro returns the current time in microseconds since epoch (mirrors C++
// timeToInt(second_clock::universal_time())). Centralized so tests can override.
var NowMicro = func() uint64 {
	return uint64(time.Now().UnixMicro())
}
