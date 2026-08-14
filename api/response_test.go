package api

import (
	"strings"
	"testing"
	"time"

	"go-xbridge/coins"
)

func TestFormatXAmount(t *testing.T) {
	cases := []struct {
		amt  uint64
		want string
	}{
		{0, "0.000000"},
		{100000000, "100.000000"}, // 100 * COIN
		{1500000, "1.500000"},     // 1.5
		{1531409, "1.531409"},     // live-packet DOGE amount
		{24500001, "24.500001"},   // live-packet BLOCK amount
	}
	for _, c := range cases {
		if got := formatXAmount(c.amt); got != c.want {
			t.Errorf("formatXAmount(%d) = %q, want %q", c.amt, got, c.want)
		}
	}
}

func TestFormatBalanceNative(t *testing.T) {
	// Mirrors C++ dxGetTokenBalances: sum native UTXO amounts as a double, then
	// xBridgeStringValueFromPrice -> printf("%.6f", wholeCoinValue) where
	// wholeCoinValue = native / 10^Decimals. FormatFloat('f',6) is the Go
	// equivalent. Crucially this rounds to the NEAREST 6th decimal (matching
	// %.6f), so sub-satoshi remainders render correctly — unlike formatXAmount
	// which truncates (the original PIVX/UNO 1-satoshi divergence).
	btc8 := coins.Coin{Decimals: 8}
	pivx6 := coins.Coin{Decimals: 6}
	cases := []struct {
		c    coins.Coin
		nat  uint64
		want string
	}{
		{btc8, 100000000, "1.000000"},  // 1 BTC exact
		{btc8, 7669400, "0.076694"},    // DASH-style 0.076694
		{btc8, 1476550, "0.014766"},    // DOGE-style
		{pivx6, 14013258, "14.013258"}, // PIVX CORE value (sub-sat kept)
		{pivx6, 492755, "0.492755"},    // UNO CORE value
		{btc8, 0, "0.000000"},          // zero
		{btc8, 1, "0.000000"},          // 1 sat < 6dp rounds to 0 (%.6f)
	}
	for _, c := range cases {
		if got := formatBalanceNative(c.c, c.nat); got != c.want {
			t.Errorf("formatBalanceNative(Decimals=%d, %d) = %q, want %q", c.c.Decimals, c.nat, got, c.want)
		}
	}
}

func TestParseXAmount(t *testing.T) {
	cases := []struct {
		s    string
		want uint64
	}{
		{"1.5", 1500000},
		{"100", 100000000},
		{"0.000001", 1},
	}
	for _, c := range cases {
		got, err := parseXAmount(c.s)
		if err != nil {
			t.Fatalf("parseXAmount(%q) error: %v", c.s, err)
		}
		if got != c.want {
			t.Errorf("parseXAmount(%q) = %d, want %d", c.s, got, c.want)
		}
	}
	if _, err := parseXAmount("not-a-number"); err == nil {
		t.Error("parseXAmount(garbage) expected error")
	}
}

// TestParseXAmountPrecision confirms the big.Int parser is exact for values
// past float64's 2^53 precision cliff and rejects overflow/negative/garbage.
func TestParseXAmountPrecision(t *testing.T) {
	cases := []struct {
		s    string
		want uint64
		ok   bool
	}{
		// "10000000000.123456" = 1e10 coins → 1e16 base units, above 2^53, where
		// float64 silently drops the trailing 456. big.Int must keep it exact.
		{"10000000000.123456", 10000000000123456, true},
		// Sub-base-unit (7th-decimal) input is truncated to 6-decimal precision.
		{"1.5000009", 1500000, true},
		// Large but in-range: round-trips exactly.
		{"18446744073709.000000", 18446744073709000000, true}, // < maxUint64
		// Overflow beyond uint64 is rejected.
		{"18446744073709551616", 0, false}, // 2^64
		// Negative input is rejected (amounts are unsigned).
		{"-1.5", 0, false},
		// Multiple dots / garbage rejected.
		{"1.2.3", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := parseXAmount(c.s)
		if c.ok {
			if err != nil {
				t.Fatalf("parseXAmount(%q) unexpected error: %v", c.s, err)
			}
			if got != c.want {
				t.Errorf("parseXAmount(%q) = %d, want %d", c.s, got, c.want)
			}
		} else if err == nil {
			t.Errorf("parseXAmount(%q) expected error, got %d", c.s, got)
		}
	}
}

func TestISO8601(t *testing.T) {
	if got := iso8601(0); got != "1970-01-01T00:00:00.000Z" {
		t.Errorf("iso8601(0) = %q", got)
	}
	tm := time.Date(2018, 1, 15, 18, 15, 30, 123000000, time.UTC)
	us := uint64(tm.UnixMicro())
	if got := iso8601(us); got != "2018-01-15T18:15:30.123Z" {
		t.Errorf("iso8601(2018-01-15T18:15:30.123Z) = %q, want 2018-01-15T18:15:30.123Z", got)
	}
}

func TestOrderTypeAndParent(t *testing.T) {
	if orderTypeString(false) != "exact" || orderTypeString(true) != "partial" {
		t.Error("orderTypeString mismatch")
	}
	var zero [32]byte
	if parentIDString(zero) != "" {
		t.Error("parentIDString(zero) should be empty")
	}
	id := [32]byte{0xab}
	// C++ uint256::GetHex renders reversed display order (orderIDString).
	if parentIDString(id) != orderIDString(id) {
		t.Errorf("parentIDString(nonzero) should match orderIDString (reversed hex), got %q", parentIDString(id))
	}
}

func TestStatusString(t *testing.T) {
	cases := map[string]string{
		"trPending":   "open",
		"trCreated":   "created",
		"trCancelled": "canceled",
		"trFinished":  "finished",
		"open":        "open",
		"created":     "created",
	}
	for in, want := range cases {
		if got := statusString(in); got != want {
			t.Errorf("statusString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMakeOrderResponseShapes(t *testing.T) {
	// dxMakeOrder must report partial_* = "0.000000" (formatXAmount of zero) and
	// status = "created".
	o := &Order{
		ID:             [32]byte{0x01},
		FromCurrency:   "SYS",
		FromAmount:     1500000,
		ToCurrency:     "LTC",
		ToAmount:       150000,
		Created:        uint64(time.Date(2018, 1, 15, 18, 15, 30, 0, time.UTC).UnixMicro()),
		Updated:        uint64(time.Date(2018, 1, 15, 18, 25, 5, 0, time.UTC).UnixMicro()),
		PartialAllowed: false,
		Status:         "created",
		MakerAddress:   "makerAddr",
		TakerAddress:   "takerAddr",
		BlockID:        "blockhash",
	}
	r := o.makeOrderResponse()
	if r.PartialMinimum != "0.000000" || r.PartialOrigMakerSize != "0.000000" || r.PartialOrigTakerSize != "0.000000" {
		t.Errorf("dxMakeOrder partial fields must be \"0.000000\", got %q/%q/%q",
			r.PartialMinimum, r.PartialOrigMakerSize, r.PartialOrigTakerSize)
	}
	if r.Status != "created" || r.OrderType != "exact" {
		t.Errorf("dxMakeOrder status/type = %q/%q", r.Status, r.OrderType)
	}
	if r.MakerAddress != "makerAddr" || r.BlockID != "blockhash" {
		t.Errorf("dxMakeOrder addresses lost: %+v", r)
	}
}

func TestMakeErrorShape(t *testing.T) {
	e := makeError(errInvalidParameters, "dxGetOrder", "boom")
	want := "Invalid parameters: boom"
	if e.Code != errInvalidParameters || e.Name != "dxGetOrder" || e.Error != want {
		t.Errorf("makeError shape wrong: got %+v, want error=%q", e, want)
	}
	// Unknown errors ignore the argument.
	u := makeError(errUnknown, "dx", "anything")
	if u.Error != "Internal Server Error" {
		t.Errorf("unknown error must ignore arg, got %q", u.Error)
	}
}

// TestParseOrderIDS verifies the tolerant uint256S/SetHex port (uint256.cpp:
// 27-53): leading spaces and an optional 0x/0X prefix are skipped, a contiguous
// hex run is consumed, nibbles are filled from the run's end into the LSB
// upward (<64 hex chars left-pad with zeros, >=64 truncate the leading chars),
// and unparseable input yields the null id — never an error.
func TestParseOrderIDS(t *testing.T) {
	keyOf := func(id [32]byte) string { return hexEncode(id[:]) }
	key := func(s string) string { return keyOf(parseOrderIDS(s)) }
	// keyForDisplay builds the expected internal key from a display string.
	displayKey := func(s string) string { return orderIDKey(s) }

	zero := keyOf([32]byte{})
	if key("") != zero || key("   ") != zero || key("Z12") != zero || key("0x") != zero || key("0X") != zero {
		t.Errorf("null-parse inputs must yield the zero id")
	}
	cases := []struct{ in, want string }{
		{"1234", "3412" + strings.Repeat("00", 30)},               // last char -> id[0] low nibble
		{"  0x1234", "3412" + strings.Repeat("00", 30)},           // spaces + 0x skipped
		{"0X12", "12" + strings.Repeat("00", 31)},                 // uppercase 0X
		{"1234Z", "3412" + strings.Repeat("00", 30)},              // trailing non-hex ignored
		{"12 34", "12" + strings.Repeat("00", 31)},                // interior whitespace ends the run
		{"abc", "bc0a" + strings.Repeat("00", 30)},                // odd length: leading nibble zero
		{"ABcd", "cdab" + strings.Repeat("00", 30)},               // mixed case
		{"80", "80" + strings.Repeat("00", 31)},                   // high nibble at a nonzero byte
		{"1", "01" + strings.Repeat("00", 31)},                    // single char -> id[0] low
		{"0", zero},                                               // single "0" is the null id in C++ too
		{"0x0", zero},                                             // "0x0" also parses to null
		{strings.Repeat("a", 64), strings.Repeat("aa", 32)},       // exact 64
		{"b" + strings.Repeat("a", 64), strings.Repeat("aa", 32)}, // 65: leading char truncated
	}
	for _, c := range cases {
		if got := key(c.in); got != c.want {
			t.Errorf("parseOrderIDS(%q) key = %s, want %s", c.in, got, c.want)
		}
	}
	// Display round-trip: parse(orderIDString(id)) == id.
	for _, raw := range [][32]byte{{0x01}, {0x00, 0x01}, {0xff}, {0xde, 0xad, 0xbe, 0xef}, {0x80}} {
		var id [32]byte
		copy(id[:], raw[:])
		if displayKey(orderIDString(id)) != keyOf(id) {
			t.Errorf("display round-trip failed for %x", id)
		}
	}
}

// TestOrderIDLess verifies the comparator matches C++ std::map<uint256> order:
// memcmp from data[0] (LSB-first), which is the REVERSE of display-hex
// ascending (uint256.h:45-49).
func TestOrderIDLess(t *testing.T) {
	ids := func(bs ...byte) [32]byte {
		var id [32]byte
		copy(id[:], bs)
		return id
	}
	a := ids(0x01)          // display "..0001"
	b := ids(0x00, 0x01)    // display "..000100"
	if !orderIDLess(b, a) { // byte0 0x00 < 0x01
		t.Errorf("orderIDLess(%x, %x) = false, want true (LSB)", b, a)
	}
	if orderIDLess(a, b) {
		t.Errorf("orderIDLess(%x, %x) = true, want false (LSB)", a, b)
	}
	if strings.Compare(orderIDString(a), orderIDString(b)) >= 0 {
		t.Errorf("display-hex order should be the reverse (a < b), got %s vs %s", orderIDString(a), orderIDString(b))
	}
	if !orderIDLess(ids(0x01), ids(0xff)) {
		t.Errorf("orderIDLess(0x01, 0xff) should be true")
	}
	if orderIDLess(ids(0xff), ids(0xff)) {
		t.Errorf("orderIDLess must be strict (equal ids)")
	}
}
