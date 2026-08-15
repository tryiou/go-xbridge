package swap

import "testing"

// TestPriceSourceCppVectors ports the Source rows of the C++ vector table
// (src/test/xbridge_tests.cpp xbridge_pricecheck) to the swap package's
// priceSource, which backs the partial-order drift check in tryJoin. The
// equality rows must reproduce C++ exactly.
func TestPriceSourceCppVectors(t *testing.T) {
	vectors := []struct{ cda, sa, da, want uint64 }{
		{100000, 10000, 100000, 10000},
		{10, 1, 10, 1},
		{100, 62, 100, 62},
		{62, 11220000, 62, 11220000},
		{199, 99, 199, 99},
		{20999, 10999, 20999, 10999},
		{8920, 10000, 100000, 892},
		{2, 5, 10, 1},
		{990, 1, 10, 99},
		{27, 12345678, 9999, 33336},
		{9090909090, 99, 9, 99999999990},
		{1111, 1111, 1111, 1111},
	}
	for _, v := range vectors {
		if got := priceSource(v.cda, v.sa, v.da); got != v.want {
			t.Errorf("priceSource(%d, %d, %d) = %d, want %d", v.cda, v.sa, v.da, got, v.want)
		}
	}
}

// TestPriceSourceNoOverflow guards against the uint64 wrap in the COIN
// scaling (the same defect fixed in api/response.go). A wire-controlled
// amount near maxXSize (1e14 base units) previously wrapped counterpartyDest *
// 1e6 in uint64; an equal-ratio input must derive the source amount exactly.
func TestPriceSourceNoOverflow(t *testing.T) {
	const maxXSize = uint64(100000000) * coin
	if got := priceSource(maxXSize, maxXSize, maxXSize); got != maxXSize {
		t.Errorf("equal-ratio at maxXSize = %d, want %d", got, maxXSize)
	}
}

// TestPriceSourceTruncateFirst locks in C++'s normalize ordering
// (xutil.cpp:331-332): the +1'd double is truncated to CAmount BEFORE the
// integer /coin. Truncating a double division instead rounds a derived amount
// within ~1 ulp below an integer multiple of coin up to the next unit. The
// equal-ratio input at 9223372036854 base units is the boundary case: C++
// returns 9223372036853, while a double-divide-then-truncate returned
// 9223372036854.
func TestPriceSourceTruncateFirst(t *testing.T) {
	const cda = uint64(9223372036854)
	if got := priceSource(cda, cda, cda); got != cda-1 {
		t.Errorf("equal-ratio at %d = %d, want %d (C++ truncate-then-divide)", cda, got, cda-1)
	}
}

// TestPriceSourcePlusOnePlacement locks in C++'s +1 placement: the scaled
// double adds 1 BEFORE the double→int truncation (xutil.cpp:331). The old
// swap code truncated first and added 1 as an integer afterward
// (uint64(w) + 1); for a derived amount in the residue band just below an
// integer multiple of coin the double +1 rounds up, so the old code returned
// one less than C++. This input lands in that band: C++/new return
// 10881886403, the old shape returned 10881886402.
func TestPriceSourcePlusOnePlacement(t *testing.T) {
	if got := priceSource(13719162634, 315849581, 398202261); got != 10881886403 {
		t.Errorf("priceSource(13719162634, 315849581, 398202261) = %d, want 10881886403 (C++ double +1 before truncation)", got)
	}
}

// TestPriceDestCppVectors ports the Dest rows of the C++ vector table
// (src/test/xbridge_tests.cpp xbridge_pricecheck) to priceDest.
func TestPriceDestCppVectors(t *testing.T) {
	vectors := []struct{ csa, sa, da, want uint64 }{
		{10000, 10000, 100000, 100000},
		{1, 1, 10, 10},
		{62, 62, 100, 100},
		{11220000, 11220000, 62, 62},
		{99, 99, 199, 199},
		{10999, 10999, 20999, 20999},
		{892, 10000, 100000, 8920},
		{1, 5, 10, 2},
		{99, 1, 10, 990},
		{34567, 12345678, 9999, 27},
		{99999999999, 99, 9, 9090909090},
		{999, 999, 999, 999},
	}
	for _, v := range vectors {
		if got := priceDest(v.csa, v.sa, v.da); got != v.want {
			t.Errorf("priceDest(%d, %d, %d) = %d, want %d", v.csa, v.sa, v.da, got, v.want)
		}
	}
}

// TestPartialOrderDriftCheckZeroReject proves a zero taker amount is rejected
// as a price mismatch rather than panicking on the C++ divisibility modulo
// (xutil.cpp:356: makerSource % otherDest, UB on zero). A hostile wire hold
// with FromAmount/ToAmount 0 must return false, never crash.
func TestPartialOrderDriftCheckZeroReject(t *testing.T) {
	if PartialOrderDriftCheck(1000000, 1000000, 0, 1000000) {
		t.Error("otherSource=0 must be rejected")
	}
	if PartialOrderDriftCheck(1000000, 1000000, 1000000, 0) {
		t.Error("otherDest=0 must be rejected")
	}
	if PartialOrderDriftCheck(1000000, 1000000, 0, 0) {
		t.Error("both zero must be rejected")
	}
	// Sanity: a valid equal-amount pair still passes (no over-rejection).
	if !PartialOrderDriftCheck(1000000, 1000000, 1000000, 1000000) {
		t.Error("valid equal pair must pass")
	}
}
