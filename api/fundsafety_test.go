package api

import (
	"strings"
	"testing"

	"go-xbridge/config"
	"go-xbridge/wallet"
)

// TestComputeLockTimeFor asserts the exact C++-parity locktime math:
// absoluteHeight = currentBlock + max(target / blockTime, XMIN_LOCKTIME_BLOCKS).
// The +6 floor (XMIN_LOCKTIME_BLOCKS) must hold even on chains whose target
// yields fewer than 6 blocks, and the slow-chain taker branch must select the
// longer (1h) target once blockTime >= XSLOW_BLOCKTIME_SECONDS. Mirrors C++
// xbridgewalletconnectorbtc.cpp lockTime() / the XMIN_LOCKTIME_BLOCKS clamp.
func TestComputeLockTimeFor(t *testing.T) {
	const n = int64(1000) // current block height

	btc := &fakeConnector{ticker: "BTC", blockHeight: n}
	slow := &fakeConnector{ticker: "SLW", blockHeight: n}
	verySlow := &fakeConnector{ticker: "VSL", blockHeight: n}

	c := swapCtx{
		connectors: map[string]wallet.Connector{
			"BTC": btc, "SLW": slow, "VSL": verySlow,
		},
		confs: map[string]*config.CoinConf{
			"BTC": {Ticker: "BTC", BlockTime: 60},
			"SLW": {Ticker: "SLW", BlockTime: 600},  // >= XSLOW_BLOCKTIME_SECONDS
			"VSL": {Ticker: "VSL", BlockTime: 7200}, // target/bt < XMIN_LOCKTIME_BLOCKS
		},
	}

	// expected = n + max(target/blockTime, 6)
	cases := []struct {
		cur     string
		isMaker bool
		want    uint32
	}{
		{"BTC", true, uint32(n) + 120}, // 7200/60
		{"BTC", false, uint32(n) + 30}, // 1800/60
		{"SLW", false, uint32(n) + 6},  // slow-taker branch: 3600/600
		{"SLW", true, uint32(n) + 12},  // 7200/600
		{"VSL", true, uint32(n) + 6},   // clamped floor
		{"VSL", false, uint32(n) + 6},  // clamped floor
	}
	for _, tc := range cases {
		got := c.computeLockTimeFor(tc.cur, tc.isMaker)
		if got != tc.want {
			t.Errorf("computeLockTimeFor(%s, maker=%v) = %d, want %d", tc.cur, tc.isMaker, got, tc.want)
		}
	}

	if xMinLockTimeBlocks != 6 {
		t.Errorf("xMinLockTimeBlocks = %d, want 6 (C++ XMIN_LOCKTIME_BLOCKS)", xMinLockTimeBlocks)
	}
}

// TestXAmountFormatting asserts the XBridge-scale amount codec invariants:
// formatXAmount renders exactly 6 fixed decimal places with NO scientific
// notation and TRUNCATES (never rounds up — a utxo cannot overpay); parseXAmount
// inverts it exactly and truncates sub-unit digits beyond 6 (no error); and
// formatXPrice renders fixed 6-decimal strings. Catches float-drift / %e
// regressions and any rounding-up of order amounts.
func TestXAmountFormatting(t *testing.T) {
	cases := []uint64{0, 1, 999999, 1_000_000, 1_234_567, 100_000_000 * 1_000_000}
	for _, amt := range cases {
		s := formatXAmount(amt)
		if strings.ContainsAny(s, "eE") {
			t.Errorf("formatXAmount(%d) = %q contains scientific notation", amt, s)
		}
		parts := strings.SplitN(s, ".", 2)
		if len(parts) != 2 || len(parts[1]) != 6 {
			t.Errorf("formatXAmount(%d) = %q, want exactly 6 fractional digits", amt, s)
		}
		// truncation, not rounding: the sub-unit remainder must be dropped.
		if amt == 1_234_567 && s != "1.234567" {
			t.Errorf("formatXAmount(1234567) = %q, want 1.234567", s)
		}
		back, err := parseXAmount(s)
		if err != nil {
			t.Errorf("parseXAmount(%q) err = %v", s, err)
			continue
		}
		if back != amt {
			t.Errorf("roundtrip %d -> %q -> %d", amt, s, back)
		}
	}

	// parseXAmount truncates beyond 6 decimals (no error); never rounds up.
	if v, err := parseXAmount("1.2345678"); err != nil || v != 1_234_567 {
		t.Errorf("parseXAmount(\"1.2345678\") = %d, %v; want 1234567 (truncated)", v, err)
	}
	if v, err := parseXAmount("1.9999999"); err != nil || v != 1_999_999 {
		t.Errorf("parseXAmount(\"1.9999999\") = %d, %v; want 1999999 (no rounding up)", v, err)
	}
	if v, err := parseXAmount("0." + strings.Repeat("0", 6)); err != nil || v != 0 {
		t.Errorf("parseXAmount(\"0.000000\") = %d, %v", v, err)
	}

	// formatXPrice renders prices as fixed 6-decimal strings, never scientific.
	for _, p := range []float64{0.1, 1, 1234.5, 0.000001} {
		ps := formatXPrice(p)
		if strings.ContainsAny(ps, "eE") {
			t.Errorf("formatXPrice(%v) = %q contains scientific notation", p, ps)
		}
		if got := strings.SplitN(ps, ".", 2); len(got) != 2 || len(got[1]) != 6 {
			t.Errorf("formatXPrice(%v) = %q, want exactly 6 fractional digits", p, ps)
		}
	}
}
