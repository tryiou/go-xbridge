package swap

import (
	"testing"
)

// TestPartialOrderDriftBand exercises the non-zero drift path of
// PartialOrderDriftCheck (xutil.cpp:338): unevenly-divisible amounts where the
// taker legs fall within ±1 satoshi of the maker's quote must pass, and legs
// outside the band must fail. The zero-reject path was already covered
// (TestPartialOrderDriftCheckZeroReject); these cases drive the max64/min64
// branches, previously at 0%.
//
// Layout: maker offers makerSource of source coin for makerDest of dest coin;
// the taker answers otherSource/otherDest. Exact-ratio cases with indivisible
// amounts land in the drift band via the ±1 probe legs.
func TestPartialOrderDriftBand(t *testing.T) {
	// maker 10_000_000 → 3_000_000 (price 10/3 per unit, indivisible).
	mkS, mkD := uint64(10_000_000), uint64(3_000_000)
	cases := []struct {
		name   string
		oS, oD uint64
		want   bool
	}{
		// Exact indivisible quote passes.
		{"exact", 3_000_000, 10_000_000, true},
		// Source leg: the +1 truncation in priceSource makes the band
		// asymmetric — one below passes, one above fails.
		{"source-minus-1", 2_999_999, 10_000_000, true},
		{"source-plus-1", 3_000_001, 10_000_000, false},
		{"source-plus-2", 3_000_002, 10_000_000, false},
		// Dest leg: the ±1 probe on otherSource maps through the 10/3 price
		// ratio, so the dest band spans roughly ±3 — +2 passes, +10 fails.
		{"dest-plus-2", 3_000_000, 10_000_002, true},
		{"dest-plus-10", 3_000_000, 10_000_010, false},
		// Grossly off-price partial must fail.
		{"off-price", 5_000_000, 10_000_000, false},
	}
	for _, tc := range cases {
		if got := PartialOrderDriftCheck(mkS, mkD, tc.oS, tc.oD); got != tc.want {
			t.Errorf("%s: taker(%d,%d) = %v, want %v", tc.name, tc.oS, tc.oD, got, tc.want)
		}
	}
	// Zero legs stay rejected (fail-closed, no SIGFPE).
	for _, tc := range [][4]uint64{
		{10_000_000, 3_000_000, 0, 10_000_000},
		{10_000_000, 3_000_000, 3_000_000, 0},
		{10_000_000, 3_000_000, 0, 0},
	} {
		if PartialOrderDriftCheck(tc[0], tc[1], tc[2], tc[3]) {
			t.Errorf("zero-leg %v accepted, want reject", tc)
		}
	}
}

// TestMinMax64 pins the band helpers directly: max returns the upper probe,
// min the lower, including equality.
func TestMinMax64(t *testing.T) {
	if max64(3, 7) != 7 || max64(7, 3) != 7 || max64(5, 5) != 5 {
		t.Errorf("max64 wrong: %d %d %d", max64(3, 7), max64(7, 3), max64(5, 5))
	}
	if min64(3, 7) != 3 || min64(7, 3) != 3 || min64(5, 5) != 5 {
		t.Errorf("min64 wrong: %d %d %d", min64(3, 7), min64(7, 3), min64(5, 5))
	}
}
