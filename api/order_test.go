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

// takerSenseFixture returns an order as it is stored after a take: the record
// keeps the MAKER-facing currency pair (From = maker's sending asset, To =
// maker's receiving asset) with Role 'B' marking the local node as the taker
// (commitTake). C++ models the same position by destructively swapping the
// descriptor at take time (rpcxbridge.cpp:1262-1264), so every renderer that
// emits maker=fromCurrency/taker=toCurrency (rpcxbridge.cpp:795-798) reports
// the swap from the TAKER's own perspective on a taken order.
func takerSenseFixture() *Order {
	return &Order{
		ID:           [32]byte{0xab},
		FromCurrency: "BLOCK",
		FromAmount:   1000000,
		ToCurrency:   "PIVX",
		ToAmount:     2000000,
		Status:       "accepting",
		Mine:         true,
		Role:         'B',
		MakerAddress: "takerSendAddr",
		TakerAddress: "takerRecvAddr",
	}
}

// TestTakerOrderRendersLocalSense proves the order renderers report the LOCAL
// node's perspective: a taken order (Role 'B') renders maker = the currency the
// taker sends (the order's toCurrency) and taker = the currency the taker
// receives (the order's fromCurrency), mirroring C++'s post-swap descriptor.
func TestTakerOrderRendersLocalSense(t *testing.T) {
	o := takerSenseFixture()
	lr := o.toListResult()
	if lr.Maker != "PIVX" || lr.Taker != "BLOCK" {
		t.Errorf("taken order list sense: maker=%q taker=%q, want PIVX/BLOCK (taker perspective)", lr.Maker, lr.Taker)
	}
	if lr.MakerSize != "2.000000" || lr.TakerSize != "1.000000" {
		t.Errorf("taken order list sizes: maker_size=%q taker_size=%q, want 2.000000/1.000000", lr.MakerSize, lr.TakerSize)
	}

	dr := o.toDetailResult()
	if dr.Maker != "PIVX" || dr.Taker != "BLOCK" {
		t.Errorf("taken order detail sense: maker=%q taker=%q, want PIVX/BLOCK", dr.Maker, dr.Taker)
	}
	if dr.MakerAddress != "takerSendAddr" || dr.TakerAddress != "takerRecvAddr" {
		t.Errorf("taken order detail addresses: maker=%q taker=%q, want takerSendAddr/takerRecvAddr", dr.MakerAddress, dr.TakerAddress)
	}

	cr := o.toCancelResult()
	if cr.Maker != "PIVX" || cr.Taker != "BLOCK" {
		t.Errorf("taken order cancel sense: maker=%q taker=%q, want PIVX/BLOCK", cr.Maker, cr.Taker)
	}
}

// TestMakerOrderSenseUnchanged proves maker and observed orders (Role 'A'/0)
// keep rendering the maker-facing pair, matching C++ where only the TAKER's
// descriptor is swapped at take time (rpcxbridge.cpp:1262-1264).
func TestMakerOrderSenseUnchanged(t *testing.T) {
	for _, role := range []byte{'A', 0} {
		o := &Order{
			ID:           [32]byte{0xac},
			FromCurrency: "BLOCK",
			FromAmount:   1000000,
			ToCurrency:   "PIVX",
			ToAmount:     2000000,
			Status:       "open",
			Role:         role,
			MakerAddress: "makerSendAddr",
			TakerAddress: "makerRecvAddr",
		}
		lr := o.toListResult()
		if lr.Maker != "BLOCK" || lr.Taker != "PIVX" {
			t.Errorf("role %q list sense: maker=%q taker=%q, want BLOCK/PIVX (maker perspective)", role, lr.Maker, lr.Taker)
		}
		dr := o.toDetailResult()
		if dr.MakerAddress != "makerSendAddr" || dr.TakerAddress != "makerRecvAddr" {
			t.Errorf("role %q detail addresses: maker=%q taker=%q", role, dr.MakerAddress, dr.TakerAddress)
		}
	}
}

// TestTakerRejectRestoresMakerSense proves a rejected take renders the
// maker-facing pair again, mirroring C++ processTransactionReject's restore
// from orig* currencies and its address clear (xbridgesession.cpp:3464-3476).
func TestTakerRejectRestoresMakerSense(t *testing.T) {
	o := takerSenseFixture()
	o.OrigFromCurrency = "BLOCK"
	o.OrigToCurrency = "PIVX"
	o.clearUsedCoins()
	if o.Role != 0 {
		t.Errorf("clearUsedCoins role = %q, want 0", o.Role)
	}
	lr := o.toListResult()
	if lr.Maker != "BLOCK" || lr.Taker != "PIVX" {
		t.Errorf("rejected take sense: maker=%q taker=%q, want BLOCK/PIVX (restored maker pair)", lr.Maker, lr.Taker)
	}
	dr := o.toDetailResult()
	if dr.MakerAddress != "" || dr.TakerAddress != "" {
		t.Errorf("rejected take addresses = %q/%q, want cleared", dr.MakerAddress, dr.TakerAddress)
	}
}

// TestTakeResultUnaffectedByLocalSense pins the interaction between the
// render-time local-sense swap and the take renderers: toTakeResult /
// toTakeDryrunResult set Maker/Taker explicitly and must NOT compose with
// localSense (a Role-'B' order passed to toTakeResult — the committed take
// response path, node.go TakeOrder → result.toTakeResult — must render the
// SAME single swap as before, never a double swap).
func TestTakeResultUnaffectedByLocalSense(t *testing.T) {
	o := takerSenseFixture()
	res := o.toTakeResult(o.ToAmount, o.FromAmount)
	if res.Maker != "PIVX" || res.Taker != "BLOCK" {
		t.Errorf("take result on Role-'B' order: maker=%q taker=%q, want single swap PIVX/BLOCK", res.Maker, res.Taker)
	}
	if res.MakerSize != "2.000000" || res.TakerSize != "1.000000" {
		t.Errorf("take result sizes: maker_size=%q taker_size=%q, want 2.000000/1.000000", res.MakerSize, res.TakerSize)
	}
	dry := o.toTakeDryrunResult(o.ToAmount, o.FromAmount)
	if dry.Maker != "BLOCK" || dry.Taker != "PIVX" {
		t.Errorf("take dryrun on Role-'B' order: maker=%q taker=%q, want pre-swap BLOCK/PIVX", dry.Maker, dry.Taker)
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
