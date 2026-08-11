package api

import (
	"testing"

	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// TestStoreReserveForTake verifies the atomic take-input reservation (B2
// Finding 1): only one order can claim a "txid:vout" key, the reservation is
// immediately visible through LockedUtxoInfo alongside committed order inputs,
// terminal orders release their locks, and ReleaseReserve clears an in-flight
// reservation. Mirrors C++ lockFeeUtxos + lockCoins ("cannot reuse utxo
// inputs", xbridgeapp.cpp:2267).
func TestStoreReserveForTake(t *testing.T) {
	s := NewStore()
	a := testStoreOrder(1)
	b := testStoreOrder(2)
	keyA := hexEncode(a.ID[:])
	keyB := hexEncode(b.ID[:])
	s.Add(a)
	s.Add(b)

	// A first reservation succeeds and is immediately visible to other takers.
	if !s.ReserveForTake(keyA, []string{"aa:0", "bb:1"}) {
		t.Fatal("first reservation should succeed")
	}
	if !s.ReserveForTake(keyB, []string{"cc:2"}) {
		t.Fatal("disjoint reservation should succeed")
	}
	keys, byOrder := s.LockedUtxoInfo()
	for _, k := range []string{"aa:0", "bb:1", "cc:2"} {
		if !keys[k] {
			t.Fatalf("reserved key %s not reported by LockedUtxoInfo", k)
		}
	}
	if byOrder["aa:0"] != orderIDString(a.ID) {
		t.Errorf("byOrder[aa:0] = %q, want %q", byOrder["aa:0"], orderIDString(a.ID))
	}

	// A collision with an active reservation must fail the second taker.
	if s.ReserveForTake(keyB, []string{"aa:0"}) {
		t.Fatal("reuse of an actively reserved key must fail")
	}

	// A committed order input (FeeUtxos) also blocks a reservation.
	s.Update(keyA, func(o *Order) {
		o.FeeUtxos = []wallet.Utxo{{TxID: "dd", Vout: 2}}
	})
	if s.ReserveForTake(keyB, []string{"dd:2"}) {
		t.Fatal("reuse of a committed order fee utxo must fail")
	}
	lkeys, lowner := s.LockedUtxoInfo()
	if !lkeys["dd:2"] {
		t.Errorf("committed fee utxo dd:2 not reported locked")
	}
	if lowner["dd:2"] != orderIDString(a.ID) {
		t.Errorf("byOrder[dd:2] = %q, want %q (committed fee utxo owner)", lowner["dd:2"], orderIDString(a.ID))
	}

	// An unknown order cannot reserve (the take re-checks the live order).
	if s.ReserveForTake("no-such-order", []string{"zz:0"}) {
		t.Fatal("reservation for an absent order must fail")
	}

	// Release clears the reservation so the keys can be claimed again.
	s.ReleaseReserve(keyA)
	keys, _ = s.LockedUtxoInfo()
	if keys["aa:0"] || keys["bb:1"] {
		t.Error("released reservation keys still reported locked")
	}
	if !s.ReserveForTake(keyA, []string{"aa:0", "bb:1"}) {
		t.Fatal("re-reservation after release should succeed")
	}

	// A terminal (canceled) order releases all of its locks — committed inputs
	// and in-flight reservations alike.
	s.Update(keyA, func(o *Order) {
		o.Status = "canceled"
	})
	keys, _ = s.LockedUtxoInfo()
	if keys["dd:2"] {
		t.Error("terminal order's committed fee utxo still reported locked")
	}
	if keys["aa:0"] || keys["bb:1"] {
		t.Error("terminal order's in-flight reservation still reported locked")
	}
}

// testStoreOrder builds an order with a deterministic id and a couple of UTXOs
// so the deep-copy behavior of Order.Copy is exercised.
func testStoreOrder(seed byte) *Order {
	var id [32]byte
	id[0] = seed
	return &Order{
		ID:           id,
		FromCurrency: "BTC",
		ToCurrency:   "LTC",
		FromAmount:   1e6,
		ToAmount:     2e6,
		Status:       "open",
		Mine:         seed%2 == 0,
		Utxos: []proto.UtxoEntry{
			{TxID: id, Vout: 0},
			{TxID: id, Vout: 1},
		},
	}
}

// TestStoreSnapshotIndependence proves Get hands out copies, not live
// pointers: mutating a returned order (including its Utxos slice) must never
// affect the stored record. A store that returned its live *Order would fail
// these checks.
func TestStoreSnapshotIndependence(t *testing.T) {
	s := NewStore()
	id := testStoreOrder(1)
	key := hexEncode(id.ID[:])
	s.Add(id)

	// Mutate a Get result in every way a stray caller could; the store must
	// be unaffected.
	got := s.Get(key)
	if got == nil {
		t.Fatal("Get returned nil")
	}
	got.Status = "canceled"
	got.FromAmount = 999
	got.Utxos[0].Vout = 42
	got.Utxos = append(got.Utxos, proto.UtxoEntry{TxID: id.ID, Vout: 9})
	got.Mine = !got.Mine

	live := s.Get(key)
	if live.Status != "open" {
		t.Fatalf("Get copy leaked Status mutation: %q", live.Status)
	}
	if live.FromAmount != 1e6 {
		t.Fatalf("Get copy leaked FromAmount mutation: %d", live.FromAmount)
	}
	if live.Mine != id.Mine {
		t.Fatalf("Get copy leaked Mine mutation: %v", live.Mine)
	}
	if len(live.Utxos) != 2 {
		t.Fatalf("Get copy leaked Utxos append: len=%d", len(live.Utxos))
	}
	if live.Utxos[0].Vout != 0 {
		t.Fatalf("Get copy leaked Utxos element mutation: vout=%d", live.Utxos[0].Vout)
	}

	// List / Mine / Locked must also hand out independent snapshots.
	func() {
		for _, o := range s.List() {
			o.Status = "canceled"
		}
		for _, o := range s.Mine() {
			o.Status = "canceled"
		}
		for _, o := range s.Locked() {
			o.Status = "canceled"
		}
	}()
	if live := s.Get(key); live.Status != "open" {
		t.Fatalf("List/Mine/Locked leaked mutation: %q", live.Status)
	}
}

// TestStoreConcurrentReadWrite hammers the book from writer and reader
// goroutines. Under -race it proves the copy-on-write model has no data races
// (a store returning live pointers would trip the detector on Status/Utxos).
// The pairing assertions catch torn multi-field writes: a reader must never
// observe Status and Reason from different writer iterations.
func TestStoreConcurrentReadWrite(t *testing.T) {
	s := NewStore()
	const orders = 8
	keys := make([]string, 0, orders)
	for i := byte(0); i < orders; i++ {
		o := testStoreOrder(i + 1)
		s.Add(o)
		keys = append(keys, hexEncode(o.ID[:]))
	}

	done := make(chan struct{})
	const iterations = 200

	// Writers: flip each order between (open, "") and (canceled, "x") within a
	// single Update so the store lock serializes the pair.
	for w := 0; w < 4; w++ {
		go func() {
			for it := 0; it < iterations; it++ {
				for _, k := range keys {
					s.Update(k, func(o *Order) {
						if o.Status == "open" {
							o.Status = "canceled"
							o.Reason = 1
						} else {
							o.Status = "open"
							o.Reason = 0
						}
						o.FromAmount++
					})
				}
			}
			done <- struct{}{}
		}()
	}

	// Readers: snapshots must always pair Status and Reason consistently, and
	// must never expose a torn Utxos slice (len must stay 2 for every read).
	readerStop := make(chan struct{})
	readerErr := make(chan string, 8)
	for r := 0; r < 4; r++ {
		go func() {
			for {
				select {
				case <-readerStop:
					return
				default:
				}
				for _, k := range keys {
					o := s.Get(k)
					if o == nil {
						readerErr <- "Get returned nil"
						return
					}
					switch o.Status {
					case "open":
						if o.Reason != 0 {
							readerErr <- "torn read: open order with a cancel reason"
							return
						}
					case "canceled":
						if o.Reason != 1 {
							readerErr <- "torn read: canceled order without its reason"
							return
						}
					default:
						readerErr <- "torn read: unknown status " + o.Status
						return
					}
					if len(o.Utxos) != 2 {
						readerErr <- "torn read: Utxos len changed"
						return
					}
				}
				if len(s.List()) != orders {
					readerErr <- "List returned wrong order count"
					return
				}
			}
		}()
	}

	for w := 0; w < 4; w++ {
		<-done
	}
	close(readerStop)

	select {
	case err := <-readerErr:
		t.Fatal(err)
	default:
	}
}

// TestStoreUpdateTouchSemantics pins Update's existence report and Touch's
// live/canceled gate (the ingestPending relay path relies on both).
func TestStoreUpdateTouchSemantics(t *testing.T) {
	s := NewStore()
	o := testStoreOrder(7)
	key := hexEncode(o.ID[:])
	s.Add(o)

	if ok := s.Update("deadbeef", func(o *Order) { o.Status = "canceled" }); ok {
		t.Fatal("Update on unknown id reported success")
	}

	before := s.Get(key).Updated
	if !s.Touch(key) {
		t.Fatal("Touch on a live order reported false")
	}
	if after := s.Get(key).Updated; after <= before {
		t.Fatalf("Touch did not bump Updated: %d -> %d", before, after)
	}

	if ok := s.Update(key, func(o *Order) { o.Status = "canceled" }); !ok {
		t.Fatal("Update on a known id reported failure")
	}
	if s.Touch(key) {
		t.Fatal("Touch on a canceled order reported true; a relayed copy may not refresh it")
	}
}

// TestStoreHasOrder verifies that HasOrder reports orders in both the live
// orders map (including canceled-but-not-moved records) and the bounded
// history. This backs the ingestPending history guard.
func TestStoreHasOrder(t *testing.T) {
	s := NewStore()
	o := testStoreOrder(11)
	key := hexEncode(o.ID[:])

	if s.HasOrder(key) {
		t.Fatal("HasOrder returned true for an unknown order")
	}

	s.Add(o)
	if !s.HasOrder(key) {
		t.Fatal("HasOrder returned false for a live order")
	}

	// Canceled-but-still-live (not moved to history): still known.
	s.Update(key, func(o *Order) { o.Status = "canceled" })
	if !s.HasOrder(key) {
		t.Fatal("HasOrder returned false for a canceled live order")
	}

	// Moved to history: no longer in live map, but still known.
	s.MoveToHistory(key, "canceled", 0, NowMicro())
	if s.Get(key) != nil {
		t.Fatal("order still live after MoveToHistory")
	}
	if !s.HasOrder(key) {
		t.Fatal("HasOrder returned false for an order in history")
	}
}
