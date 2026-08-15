package api

import "testing"

// TestTakeResultSwapsMakerTaker locks in C++ dxTakeOrder's post-swap render:
// the real (non-dryrun) take reports maker = the order's toCurrency and
// taker = the order's fromCurrency.
func TestTakeResultSwapsMakerTaker(t *testing.T) {
	o := &Order{
		ID:             [32]byte{0xab},
		FromCurrency:   "LTC",
		FromAmount:     1500000,
		ToCurrency:     "BLOCK",
		ToAmount:       300000,
		PartialAllowed: false,
	}
	// full take: taker sends toCurrency (BLOCK) amount o.ToAmount, receives
	// fromCurrency (LTC) amount o.FromAmount.
	res := o.toTakeResult(300000, 1500000)
	if res.Maker != "BLOCK" || res.Taker != "LTC" {
		t.Errorf("take result maker/taker not swapped: maker=%q taker=%q", res.Maker, res.Taker)
	}
	if res.MakerSize != "0.300000" || res.TakerSize != "1.500000" {
		t.Errorf("take result sizes wrong: maker_size=%q taker_size=%q", res.MakerSize, res.TakerSize)
	}
}

// TestTakeDryrunResult locks in C++ dxTakeOrder's dryrun render (PRE-swap
// frame, zero id, status "filled").
func TestTakeDryrunResult(t *testing.T) {
	o := &Order{
		ID:           [32]byte{0xab},
		FromCurrency: "LTC",
		FromAmount:   1500000,
		ToCurrency:   "BLOCK",
		ToAmount:     300000,
	}
	res := o.toTakeDryrunResult(300000, 1500000)
	if res.Maker != "LTC" || res.Taker != "BLOCK" {
		t.Errorf("dryrun maker/taker should be pre-swap: maker=%q taker=%q", res.Maker, res.Taker)
	}
	if res.Status != "filled" {
		t.Errorf("dryrun status = %q, want filled", res.Status)
	}
	wantID := "0000000000000000000000000000000000000000000000000000000000000000"
	if res.ID != wantID {
		t.Errorf("dryrun id = %q, want zero id", res.ID)
	}
}

// TestXBridgeValidCoin locks in the C++ 6-decimal precision gate used by
// dxMakeOrder.
func TestXBridgeValidCoin(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"25", true},
		{"25.000000", true},
		{"25.123456", true},
		{"25.1234567", false}, // 7 significant digits
		{"0.000001", true},
		{"100000000", true},
	}
	for _, c := range cases {
		if got := xBridgeValidCoin(c.s); got != c.want {
			t.Errorf("xBridgeValidCoin(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// TestXBridgeSourceAmountFromPrice matches util/xutil.cpp for a partial take:
// taking 1.0 LTC (fromAmount) of an order for 1.5 BLOCK (toAmount) should
// imply a taker-sent BLOCK amount near 1.5.
func TestXBridgeSourceAmountFromPrice(t *testing.T) {
	got := xBridgeSourceAmountFromPrice(1500000, 300000, 1500000)
	if got != 300000 {
		t.Errorf("xBridgeSourceAmountFromPrice for equal ratio = %d, want 300000", got)
	}
	// Partial 1.0 LTC of a 1.5-LTC/3.0-BLOCK order -> taker sends ~2.0 BLOCK.
	got = xBridgeSourceAmountFromPrice(1000000, 3000000, 1500000)
	if got != 2000000 {
		t.Errorf("xBridgeSourceAmountFromPrice partial = %d, want 2000000", got)
	}
}

// TestXBridgeSourceAmountFromPriceCppVectors ports the C++ vector table from
// src/test/xbridge_tests.cpp (xbridge_pricecheck) for the Source function. The
// equality rows must reproduce C++ exactly; the inequality rows assert the
// result is sensitive to the input (a changed counterparty amount must change
// the derived taker amount).
func TestXBridgeSourceAmountFromPriceCppVectors(t *testing.T) {
	exact := []struct {
		cda, sa, da uint64
		want        uint64
	}{
		{100000, 10000, 100000, 10000},
		{10, 1, 10, 1},
		{100, 62, 100, 62},
		{62, 11220000, 62, 11220000},
		{199, 99, 199, 99},
		{20999, 10999, 20999, 10999},
	}
	partial := []struct {
		cda, sa, da uint64
		want        uint64
	}{
		{8920, 10000, 100000, 892},
		{2, 5, 10, 1},
		{990, 1, 10, 99},
		{27, 12345678, 9999, 33336},
		{9090909090, 99, 9, 99999999990},
		{1111, 1111, 1111, 1111},
	}
	for _, c := range append(exact, partial...) {
		if got := xBridgeSourceAmountFromPrice(c.cda, c.sa, c.da); got != c.want {
			t.Errorf("xBridgeSourceAmountFromPrice(%d, %d, %d) = %d, want %d", c.cda, c.sa, c.da, got, c.want)
		}
	}
	// Inequality rows: changing only the source amount (2nd column) must move
	// the derived amount (C++ asserts != on the same input pairs).
	ineq := []struct {
		cda, sa, da, want uint64
	}{
		{8920, 100000, 100000, 892},
		{2, 20, 10, 1},
		{990, 2, 10, 99},
		{27, 123456789, 9999, 33336},
		{9090909090, 990, 9, 99999999990},
		{1111, 11112, 1111, 1111},
	}
	for _, c := range ineq {
		if got := xBridgeSourceAmountFromPrice(c.cda, c.sa, c.da); got == c.want {
			t.Errorf("xBridgeSourceAmountFromPrice(%d, %d, %d) = %d, must differ from %d (C++ != row)", c.cda, c.sa, c.da, got, c.want)
		}
	}
}

// TestXBridgeSourceAmountFromPriceNoOverflow guards against the uint64 wrap in
// the COIN scaling. An equal-ratio order at the max accepted size (maxXSize =
// 100,000,000 * COIN) must derive the source amount exactly; the old code
// computed counterpartyDestAmount * coinScale in uint64 first, wrapping 1e20
// mod 2^64 (the probe returned 7766279631452 instead of 100000000000000).
// C++ uses a CAmount int64 here; scaling in double keeps the result matching
// C++ for every non-overflowing input and defined beyond it.
func TestXBridgeSourceAmountFromPriceNoOverflow(t *testing.T) {
	if got := xBridgeSourceAmountFromPrice(maxXSize, maxXSize, maxXSize); got != maxXSize {
		t.Errorf("equal-ratio at maxXSize = %d, want %d", got, maxXSize)
	}
}

// TestXBridgeSourceAmountFromPriceTruncateFirst locks in C++'s normalize
// ordering (xutil.cpp:331-332): the +1'd double is truncated to CAmount BEFORE
// the integer /c. Truncating a double division instead can round a derived
// amount that sits within ~1 ulp below an integer multiple of COIN up to the
// next unit. The equal-ratio input at 9223372036854 base units (≈9.2M whole
// coins, within maxXSize) is the boundary case: C++ returns 9223372036853,
// while a double-divide-then-truncate produced 9223372036854.
func TestXBridgeSourceAmountFromPriceTruncateFirst(t *testing.T) {
	const cda = uint64(9223372036854)
	if got := xBridgeSourceAmountFromPrice(cda, cda, cda); got != cda-1 {
		t.Errorf("equal-ratio at %d = %d, want %d (C++ truncate-then-divide)", cda, got, cda-1)
	}
}

// TestXBridgeSourceAmountFromPriceExtremeClamp exercises the fallback for
// values beyond the CAmount range (C++ overflows there): the double normalize
// must clamp to MaxUint64 rather than rely on the implementation-dependent
// float64→uint64 conversion for values >= 2^64.
func TestXBridgeSourceAmountFromPriceExtremeClamp(t *testing.T) {
	got := xBridgeSourceAmountFromPrice(^uint64(0), ^uint64(0), ^uint64(0))
	if got == 0 {
		t.Fatal("extreme equal-ratio input returned 0, want a clamped nonzero amount")
	}
	if got != ^uint64(0) {
		t.Errorf("extreme equal-ratio input = %d, want clamp to MaxUint64", got)
	}
}

// TestMakePartialOrderResponse locks in C++ dxMakePartialOrder's SUCCESS render:
// order_type is "partial" (not "exact"), the partial_* fields carry the real
// values (not "0"), partial_repost echoes the repost flag, and status is
// "created". This guards against the old bug where partial orders were rendered
// with the exact-order shape.
func TestMakePartialOrderResponse(t *testing.T) {
	o := &Order{
		ID:             [32]byte{0xcd},
		FromCurrency:   "LTC",
		FromAmount:     1500000, // 1.5
		ToCurrency:     "BLOCK",
		ToAmount:       300000, // 0.3
		PartialAllowed: true,
		MinFromAmount:  500000, // 0.5
		OrigFromAmount: 1500000,
		OrigToAmount:   300000,
		PartialRepost:  true,
		Mine:           true,
	}
	res := o.makePartialOrderResponse(true)
	if res.OrderType != "partial" {
		t.Errorf("order_type = %q, want partial", res.OrderType)
	}
	if res.PartialMinimum != "0.500000" {
		t.Errorf("partial_minimum = %q, want 0.500000", res.PartialMinimum)
	}
	if res.PartialOrigMakerSize != "1.500000" {
		t.Errorf("partial_orig_maker_size = %q, want 1.500000", res.PartialOrigMakerSize)
	}
	if res.PartialOrigTakerSize != "0.300000" {
		t.Errorf("partial_orig_taker_size = %q, want 0.300000", res.PartialOrigTakerSize)
	}
	if res.PartialRepost != true {
		t.Errorf("partial_repost = %v, want true", res.PartialRepost)
	}
	if res.Status != "created" {
		t.Errorf("status = %q, want created", res.Status)
	}

	// repost=false echoes through.
	res2 := o.makePartialOrderResponse(false)
	if res2.PartialRepost != false {
		t.Errorf("partial_repost = %v, want false", res2.PartialRepost)
	}
}

// TestMakeOrderResponseExact locks in that dxMakeOrder (exact) renders the
// partial_* fields as literal "0" (rpcxbridge.cpp:1060-1062) and order_type
// "exact".
func TestMakeOrderResponseExact(t *testing.T) {
	o := &Order{
		ID:             [32]byte{0xab},
		FromCurrency:   "LTC",
		FromAmount:     1500000,
		ToCurrency:     "BLOCK",
		ToAmount:       300000,
		PartialAllowed: false,
	}
	res := o.makeOrderResponse()
	if res.OrderType != "exact" {
		t.Errorf("order_type = %q, want exact", res.OrderType)
	}
	if res.PartialMinimum != "0" || res.PartialOrigMakerSize != "0" || res.PartialOrigTakerSize != "0" {
		t.Errorf("partial fields = %q/%q/%q, want \"0\"", res.PartialMinimum, res.PartialOrigMakerSize, res.PartialOrigTakerSize)
	}
}
