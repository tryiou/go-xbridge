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

// Store is the in-memory XBridge order book, safe for concurrent use; mutations run only in its methods under s.mu and reads return snapshot copies.
type Store struct {
	mu     sync.RWMutex
	orders map[string]*Order
	locked map[string]*Order
	// history keeps terminal records for dxGetOrderHistory; unbounded, only FlushCancelled erases.
	history []historyEntry
	// reserved claims "txid:vout" keys for a take before its Accepting packet
	// leaves, so a concurrent take cannot select the same inputs.
	reserved map[string]reservedKeys
}

type reservedKeys struct {
	fee          []string // BLOCK fee-input keys, global exclusion
	fund         []string // taker funding keys on fundCurrency
	fundCurrency string   // ticker of the funding wallet (the order's ToCurrency)
}

// historyEntry is a removed order's terminal record; Order holds a full snapshot so the order stays renderable after removal.
type historyEntry struct {
	ID      string
	Status  string // e.g. "canceled"
	Reason  uint32
	Updated uint64
	Order   *Order // snapshot of the order at removal time (nil when unavailable)
}

// fillEntry is one dxGetOrderFills row, computed from the store on every call and never stored, so no fills ledger can drift from it.
type fillEntry struct {
	ID        string
	Time      uint64 // microseconds since epoch
	Maker     string
	MakerSize string
	Taker     string
	TakerSize string
	ParentID  string
	// Partial-order fields, carried so the row matches the full dxGetOrderFills shape.
	OrderType            string
	PartialMinimum       string
	PartialOrigMakerSize string
	PartialOrigTakerSize string
	PartialRepost        bool
}

// flushedOrder is one cancelled order removed by FlushCancelled; UseCount is
// always 1 — Go has no shared_ptr refcount to report for the RPC field.
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

// Add inserts or replaces an order by id; ownership of o transfers to the store, which keeps it live, so the caller must not mutate it afterward.
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

// Update applies fn to the live order for idHex under the store lock and reports whether the order existed; fn must not retain the pointer.
func (s *Store) Update(idHex string, fn func(*Order)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.orders[idHex]; o != nil {
		fn(o)
		return true
	}
	return false
}

// Touch refreshes the Updated timestamp of a live, non-canceled order and reports whether it did.
func (s *Store) Touch(idHex string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ex := s.orders[idHex]; ex != nil && ex.Status != "canceled" {
		ex.Updated = NowMicro()
		return true
	}
	return false
}

// List returns snapshot copies of all orders, unfiltered; callers apply their own filtering.
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

// historyLocked appends a terminal history entry under the held lock.
func (s *Store) historyLocked(idHex, status string, reason uint32, updated uint64, o *Order) {
	for _, e := range s.history {
		if e.ID == idHex {
			return
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
}

// MoveToHistory deletes the live order and appends a terminal history record for it; a missing live order makes it a no-op.
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

// HasOrder reports whether idHex is known to the store, live (including canceled) or in history.
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

// AddToHistory appends a terminal history record for o without touching the live orders map; it is idempotent on duplicate ids.
func (s *Store) AddToHistory(o *Order, status string, reason, updated uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyLocked(hexEncode(o.ID[:]), status, uint32(reason), updated, o)
}

// HistoryOrder returns a snapshot copy of the removed order recorded under idHex, or nil when the id is absent or the entry carries no snapshot.
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

// RemovePendingPackets is a no-op: this port keeps no pending-packet queue, so the method exists only to mirror the C++ call site.
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

// PruneUnconnected removes non-local orders whose from/to currency has no connector; local orders are always kept.
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

// PruneExpired removes open and created orders past their TTL, skipping the caller's inSwap set; it records each expiry in history and returns the ids sorted.
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
		// A partial order awaiting its prep-tx confirmation is never swept.
		if o.PrepTx != "" {
			continue
		}
		created := ageSec(nowUs, o.Created)
		updated := ageSec(nowUs, o.Updated)
		expired := false
		switch o.Status {
		case "created":
			blockExpired := o.BlockNumber != 0 && currentBlock != 0 &&
				currentBlock > o.BlockNumber && currentBlock-o.BlockNumber > swap.BlocksTTL
			expired = blockExpired || created > swap.DeadlineTTL || updated > swap.TTL
		case "open":
			expired = updated > swap.PendingTTL || created > swap.DeadlineTTL
		}
		if expired {
			s.historyLocked(key, "expired", uint32(crTimeout), nowUs, o)
			delete(s.orders, key)
			pruned = append(pruned, key)
		}
	}
	sort.Strings(pruned)
	return pruned
}

// ageSec is the whole-second age of a microsecond timestamp; a timestamp ahead of now (clock skew) yields 0 rather than wrapping to a huge age.
func ageSec(nowUs, ts uint64) uint64 {
	if ts >= nowUs {
		return 0
	}
	return (nowUs - ts) / 1000000
}

// Fills returns this node's completed fill rows newest first, merged from finished history records and live finished orders.
func (s *Store) Fills() []fillEntry {
	out := func() []fillEntry {
		s.mu.RLock()
		defer s.mu.RUnlock()
		out := make([]fillEntry, 0, len(s.history)+len(s.orders))
		seen := make(map[string]bool, len(s.history)+len(s.orders))
		render := func(o *Order, t uint64) {
			maker, makerSize, taker, takerSize := o.localSense()
			out = append(out, fillEntry{
				ID:                   orderIDString(o.ID),
				Time:                 t,
				Maker:                maker,
				MakerSize:            formatXAmount(makerSize),
				Taker:                taker,
				TakerSize:            formatXAmount(takerSize),
				ParentID:             parentIDString(o.ParentID),
				OrderType:            orderTypeString(o.PartialAllowed),
				PartialMinimum:       formatXAmount(o.MinFromAmount),
				PartialOrigMakerSize: formatXAmount(o.OrigFromAmount),
				PartialOrigTakerSize: formatXAmount(o.OrigToAmount),
				PartialRepost:        o.PartialRepost,
			})
			seen[orderKey(o.ID)] = true
		}
		for i := len(s.history) - 1; i >= 0; i-- {
			e := s.history[i]
			o := e.Order
			if o == nil || !o.Mine || statusString(e.Status) != "finished" || seen[orderKey(o.ID)] {
				continue
			}
			render(o, e.Updated)
		}
		for _, o := range s.orders {
			if o == nil || !o.Mine || statusString(o.Status) != "finished" || seen[orderKey(o.ID)] {
				continue
			}
			render(o, o.Updated)
		}
		return out
	}()
	// Sort outside the book lock: the rows are materialized and all value
	// types, so ordering them after unlock never blocks writers.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time > out[j].Time
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Lock marks an order's UTXOs as locked.
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

func isOrderTerminal(status string) bool {
	switch statusString(status) {
	case "finished", "canceled", "dropped", "invalid":
		return true
	}
	return false
}

// utxoEntryKey returns the "txid:vout" lock key for a UTXO entry; proto.UtxoEntry.TxID
// is little-endian, so it is reversed to display order before hex-encoding.
func utxoEntryKey(e proto.UtxoEntry) string {
	var rev [32]byte
	for i := 0; i < 32; i++ {
		rev[i] = e.TxID[31-i]
	}
	return hexEncode(rev[:]) + ":" + strconv.FormatUint(uint64(e.Vout), 10)
}

// lockedInfoLocked gathers the locked-key set and its owners under a held lock.
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

// LockedUtxoInfo returns every "txid:vout" locked by a live order's inputs or by an in-flight take reservation.
func (s *Store) LockedUtxoInfo() (keys map[string]bool, byOrder map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lockedInfoLocked()
}

// utxoCurrency returns the chain an order's locked Utxos live on: the explicit tag,
// else the Role-derived rule for legacy records that predate it.
func utxoCurrency(o *Order) string {
	if o.UtxoCurrency != "" {
		return o.UtxoCurrency
	}
	if o.Role == 'B' {
		return o.ToCurrency
	}
	return o.FromCurrency
}

// LockedUtxoInfoFor returns the inputs to exclude when selecting utxos on ticker: fee inputs are global; order inputs when utxoCurrency matches ticker or resolves empty; reserved fund keys only when fundCurrency matches.
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

type takeReserve int

const (
	// reserveOrderBusy reports a second in-flight take of the same order; the refusal
	// lasts only as long as that take's reservation, not the order's whole lifetime.
	reserveOrderBusy takeReserve = iota
	reserveKeyCollision
	reserveOrderGone
	reserveOK
)

// ReserveForTake atomically claims the take's fee and funding input keys under the store lock, mirroring C++ lockFeeUtxos + lockCoins; the caller must release them if the take does not submit.
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

// ReleaseReserve drops a take's in-flight reservation; the take runs it right after Update commits the same keys into the order, even when the attempt later reverts.
func (s *Store) ReleaseReserve(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reserved, key)
}

// FlushCancelled removes cancelled orders older than minAgeMillis from both the live book and history, returning them live-first then history, id-ascending like C++ flushCancelledOrders.
func (s *Store) FlushCancelled(minAgeMillis uint64) []flushedOrder {
	s.mu.Lock()
	defer s.mu.Unlock()
	// An age that overflows the µs conversion or exceeds the clock itself is
	// skipped: keepTime stays 0, where no Updated is below it, so nothing prunes.
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
	sort.SliceStable(liveFlushed, func(i, j int) bool {
		return orderIDLess(liveFlushed[i].ID, liveFlushed[j].ID)
	})
	sort.SliceStable(histFlushed, func(i, j int) bool {
		return orderIDLess(histFlushed[i].ID, histFlushed[j].ID)
	})
	return append(liveFlushed, histFlushed...)
}

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

// NowMicro returns the current time in microseconds since epoch; centralized on one variable so tests can override it.
var NowMicro = func() uint64 {
	return uint64(time.Now().UnixMicro())
}
