package api

import (
	"testing"

	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// TestStoreReserveForTake verifies the atomic take-input reservation (B2
// Finding 1): only one order can claim a "txid:vout" key, the reservation is
// immediately visible through LockedUtxoInfo alongside committed order inputs,
// a second in-flight take of the SAME order is refused (C++ state gate
// xbridgeapp.cpp:2122), terminal orders release their locks, and ReleaseReserve
// clears an in-flight reservation. Mirrors C++ lockFeeUtxos + lockCoins
// ("cannot reuse utxo inputs", xbridgeapp.cpp:2267).
func TestStoreReserveForTake(t *testing.T) {
	s := NewStore()
	a := testStoreOrder(1)
	b := testStoreOrder(2)
	c := testStoreOrder(3)
	keyA := hexEncode(a.ID[:])
	keyB := hexEncode(b.ID[:])
	keyC := hexEncode(c.ID[:])
	s.Add(a)
	s.Add(b)
	s.Add(c)

	// A first reservation succeeds and is immediately visible to other takers.
	if got := s.ReserveForTake(keyA, nil, []string{"aa:0", "bb:1"}, "BTC"); got != reserveOK {
		t.Fatalf("first reservation = %v, want reserveOK", got)
	}
	if got := s.ReserveForTake(keyB, nil, []string{"cc:2"}, "BTC"); got != reserveOK {
		t.Fatalf("disjoint reservation = %v, want reserveOK", got)
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

	// A second in-flight take of the SAME order is refused even on disjoint
	// keys (C++ state gate BAD_REQUEST, xbridgeapp.cpp:2122-2125): the
	// reservation is one-per-order, never overwritten by a concurrent take.
	// The gate fires even when the proposed keys would ALSO collide with the
	// order's own claimed keys, proving the state gate precedes the lock scan.
	if got := s.ReserveForTake(keyA, nil, []string{"zz:9"}, "BTC"); got != reserveOrderBusy {
		t.Fatalf("same-order re-reservation (disjoint) = %v, want reserveOrderBusy", got)
	}
	if got := s.ReserveForTake(keyA, nil, []string{"aa:0"}, "BTC"); got != reserveOrderBusy {
		t.Fatalf("same-order re-reservation (own key) = %v, want reserveOrderBusy", got)
	}

	// A collision with an active reservation must fail an unrelated taker.
	if got := s.ReserveForTake(keyC, nil, []string{"aa:0"}, "BTC"); got != reserveKeyCollision {
		t.Fatalf("reuse of an actively reserved key = %v, want reserveKeyCollision", got)
	}

	// A committed order input (FeeUtxos) also blocks a reservation.
	s.Update(keyA, func(o *Order) {
		o.FeeUtxos = []wallet.Utxo{{TxID: "dd", Vout: 2}}
	})
	if got := s.ReserveForTake(keyC, nil, []string{"dd:2"}, "BTC"); got != reserveKeyCollision {
		t.Fatalf("reuse of a committed order fee utxo = %v, want reserveKeyCollision", got)
	}
	lkeys, lowner := s.LockedUtxoInfo()
	if !lkeys["dd:2"] {
		t.Errorf("committed fee utxo dd:2 not reported locked")
	}
	if lowner["dd:2"] != orderIDString(a.ID) {
		t.Errorf("byOrder[dd:2] = %q, want %q (committed fee utxo owner)", lowner["dd:2"], orderIDString(a.ID))
	}

	// An unknown order cannot reserve (the take re-checks the live order).
	if got := s.ReserveForTake("no-such-order", nil, []string{"zz:0"}, "BTC"); got != reserveOrderGone {
		t.Fatalf("reservation for an absent order = %v, want reserveOrderGone", got)
	}

	// Release clears the reservation so the keys can be claimed again.
	s.ReleaseReserve(keyA)
	keys, _ = s.LockedUtxoInfo()
	if keys["aa:0"] || keys["bb:1"] {
		t.Error("released reservation keys still reported locked")
	}
	if got := s.ReserveForTake(keyA, nil, []string{"aa:0", "bb:1"}, "BTC"); got != reserveOK {
		t.Fatalf("re-reservation after release = %v, want reserveOK", got)
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

// TestLockedUtxoInfoFor verifies the per-token exclusion set C++
// getAllLockedUtxos(token) (xbridgeapp.cpp:2827-2834): the global fee set
// applies to every ticker, but an order's Utxos are excluded only on the chain
// they were locked on (UtxoCurrency, else the Role-derived fallback), and a
// reservation's funding keys apply only to fundCurrency while its fee keys are
// global.
func TestLockedUtxoInfoFor(t *testing.T) {
	s := NewStore()
	btcOrder := &Order{
		ID: [32]byte{0x01}, FromCurrency: "BTC", ToCurrency: "LTC", Status: "open", Mine: true,
		UtxoCurrency: "BTC",
		Utxos:        []proto.UtxoEntry{{TxID: [32]byte{0xaa}, Vout: 0}},
		FeeUtxos:     []wallet.Utxo{{TxID: "ff", Vout: 0}},
	}
	ltcTaker := &Order{
		ID: [32]byte{0x02}, FromCurrency: "BTC", ToCurrency: "LTC", Status: "open", Mine: false, Role: 'B',
		UtxoCurrency: "LTC",
		Utxos:        []proto.UtxoEntry{{TxID: [32]byte{0xbb}, Vout: 0}},
	}
	legacyTaker := &Order{
		// No tag: the Role 'B' fallback resolves funds to ToCurrency.
		ID: [32]byte{0x03}, FromCurrency: "BTC", ToCurrency: "LTC", Status: "open", Mine: false, Role: 'B',
		Utxos: []proto.UtxoEntry{{TxID: [32]byte{0xcc}, Vout: 0}},
	}
	legacyMaker := &Order{
		// No tag, not a taker: funds resolve to FromCurrency.
		ID: [32]byte{0x04}, FromCurrency: "LTC", ToCurrency: "BTC", Status: "open", Mine: false,
		Utxos: []proto.UtxoEntry{{TxID: [32]byte{0xdd}, Vout: 0}},
	}
	for _, o := range []*Order{btcOrder, ltcTaker, legacyTaker, legacyMaker} {
		s.Add(o)
	}

	aa := utxoEntryKey(proto.UtxoEntry{TxID: [32]byte{0xaa}, Vout: 0})
	bb := utxoEntryKey(proto.UtxoEntry{TxID: [32]byte{0xbb}, Vout: 0})
	cc := utxoEntryKey(proto.UtxoEntry{TxID: [32]byte{0xcc}, Vout: 0})
	dd := utxoEntryKey(proto.UtxoEntry{TxID: [32]byte{0xdd}, Vout: 0})

	btc := s.LockedUtxoInfoFor("BTC")
	ltc := s.LockedUtxoInfoFor("LTC")

	// Global fee set: the BLOCK fee utxo is excluded for every ticker.
	if !btc["ff:0"] || !ltc["ff:0"] {
		t.Error("fee utxo must be excluded for every currency")
	}
	// BTC-tagged utxo: excluded for BTC, absent for LTC.
	if !btc[aa] {
		t.Error("BTC-tagged utxo not excluded for BTC")
	}
	if ltc[aa] {
		t.Error("BTC-tagged utxo must not be excluded for LTC")
	}
	// LTC-tagged taker utxo: excluded for LTC, absent for BTC.
	if !ltc[bb] {
		t.Error("LTC-tagged taker utxo not excluded for LTC")
	}
	if btc[bb] {
		t.Error("LTC-tagged taker utxo must not be excluded for BTC")
	}
	// Legacy Role-'B' record resolves to ToCurrency (LTC).
	if !ltc[cc] {
		t.Error("legacy Role-'B' taker utxo must resolve to ToCurrency (LTC)")
	}
	if btc[cc] {
		t.Error("legacy Role-'B' taker utxo must not be excluded for BTC")
	}
	// Legacy non-taker record resolves to FromCurrency (LTC here).
	if !ltc[dd] {
		t.Error("legacy non-taker utxo must resolve to FromCurrency (LTC)")
	}
	if btc[dd] {
		t.Error("legacy LTC utxo must not be excluded for BTC")
	}

	// Reservation: fee keys global, funding keys only on fundCurrency.
	takerKey := hexEncode((ltcTaker.ID)[:])
	if got := s.ReserveForTake(takerKey, nil, []string{aa}, "BTC"); got != reserveKeyCollision {
		t.Fatalf("reservation against the order's own committed utxo = %v, want reserveKeyCollision", got)
	}
	if got := s.ReserveForTake(takerKey, []string{"10"}, []string{"00"}, "BTC"); got != reserveOK {
		t.Fatalf("disjoint reservation = %v, want reserveOK", got)
	}
	btc2 := s.LockedUtxoInfoFor("BTC")
	ltc2 := s.LockedUtxoInfoFor("LTC")
	if !btc2["10"] || !ltc2["10"] {
		t.Error("reserved fee key must be excluded for every currency")
	}
	if !btc2["00"] {
		t.Error("reserved funding key not excluded for its fundCurrency (BTC)")
	}
	if ltc2["00"] {
		t.Error("reserved funding key must not be excluded for another currency (LTC)")
	}
}

// TestPruneUnconnected locks CFG-F87's clearNonLocalOrders port: non-local
// orders whose from/to currency has no connector are removed; local orders are
// always kept (C++ App::clearNonLocalOrders, xbridgeapp.cpp:3811-3821).
func TestPruneUnconnected(t *testing.T) {
	s := NewStore()
	s.Add(&Order{ID: [32]byte{0x01}, FromCurrency: "BTC", ToCurrency: "BLOCK", Status: "open", Mine: true})
	s.Add(&Order{ID: [32]byte{0x02}, FromCurrency: "BTC", ToCurrency: "LTC", Status: "open", Mine: false, Role: 'B'})
	s.Add(&Order{ID: [32]byte{0x03}, FromCurrency: "LTC", ToCurrency: "BTC", Status: "open", Mine: false})
	s.Add(&Order{ID: [32]byte{0x04}, FromCurrency: "BTC", ToCurrency: "DOGE", Status: "open", Mine: false, Role: 'B'})

	s.PruneUnconnected(map[string]bool{"BTC": true, "LTC": true})

	got := s.List()
	if len(got) != 3 {
		t.Fatalf("orders after prune = %d, want 3", len(got))
	}
	for _, o := range got {
		if o.ID == [32]byte{0x04} {
			t.Error("DOGE-currency order (no DOGE connector) survived PruneUnconnected")
		}
		if o.ID == [32]byte{0x01} && !o.Mine {
			t.Error("local order lost its Mine flag")
		}
	}
	// Local order kept regardless of currencies; BTC/LTC-connected order kept.
	found := map[[32]byte]bool{}
	for _, o := range got {
		found[o.ID] = true
	}
	if !found[[32]byte{0x01}] || !found[[32]byte{0x02}] || !found[[32]byte{0x03}] {
		t.Errorf("expected orders 0x01/0x02/0x03 kept, got %v", found)
	}
}
