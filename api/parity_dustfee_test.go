package api

import (
	"testing"

	"go-xbridge/config"
)

// DustAmount is the sole definition of a coin's dust threshold, in native base
// units. Nothing derives it and nothing falls back to a constant.
func TestEffectiveDust(t *testing.T) {
	tests := []struct {
		name string
		cc   *config.CoinConf
		want uint64
	}{
		{"btc 546", &config.CoinConf{DustAmount: 546, Coin: 100_000_000}, 546},
		{"ltc 5460", &config.CoinConf{DustAmount: 5460, Coin: 100_000_000}, 5460},
		{"doge 1e6", &config.CoinConf{DustAmount: 1_000_000, Coin: 100_000_000}, 1_000_000},
		{"coin with 1e6 scale", &config.CoinConf{DustAmount: 100, Coin: 1_000_000}, 100},
		{"source disables dust", &config.CoinConf{DustAmount: 0, Coin: 100_000_000}, 0},
		// Absent conf has no threshold and must not dereference.
		{"nil conf", nil, 0},
		// MinimumAmount is the order minimum; it must never leak into dust.
		{"minimumAmount ignored", &config.CoinConf{DustAmount: 546, MinimumAmount: 1_000_000}, 546},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveDust(tc.cc); got != tc.want {
				t.Errorf("effectiveDust = %d, want %d", got, tc.want)
			}
		})
	}
}

// The 3288-sat change from the rejected LTC swap is dust at LTC's 5460
// threshold, and correctly not dust at BTC's 546.
func TestIsDustNative(t *testing.T) {
	const coin = uint64(100_000_000)
	ltc := &config.CoinConf{DustAmount: 5460, Coin: coin}
	btc := &config.CoinConf{DustAmount: 546, Coin: coin}
	firo := &config.CoinConf{DustAmount: 0, Coin: coin}

	if !isDustNative(3288.0/1e8, ltc, coin) {
		t.Error("3288 sat must be dust at LTC's 5460")
	}
	if isDustNative(3288.0/1e8, btc, coin) {
		t.Error("3288 sat must not be dust at BTC's 546")
	}
	if !isDustNative(5459.0/1e8, ltc, coin) || isDustNative(5459.0/1e8, btc, coin) {
		t.Error("5459: below LTC's threshold, above BTC's")
	}
	if isDustNative(5460.0/1e8, ltc, coin) {
		t.Error("5460 == threshold is not dust")
	}
	if isDustNative(1.0/1e8, firo, coin) {
		t.Error("a coin with DustAmount=0 has no dust gate")
	}
}

func TestEstimateFeeFloor(t *testing.T) {
	// vsize(1in,1out) = 192 + 34 = 226. 2 sat/vB fallback = 452.
	tests := []struct {
		name      string
		cc        *config.CoinConf
		nIn, nOut int
		want      uint64
	}{
		{"feePerByte 2, no floor", &config.CoinConf{FeePerByte: 2}, 1, 1, 452},
		{"feePerByte 2, floored to 1000", &config.CoinConf{FeePerByte: 2, MinTxFee: 1000}, 1, 1, 1000},
		{"nil cc -> 2 sat/vB fallback", nil, 1, 1, 452},
		{"feePerByte 0 -> fallback floored to 1000", &config.CoinConf{MinTxFee: 1000}, 1, 1, 1000},
		{"feePerByte 0 -> fallback 452", &config.CoinConf{}, 1, 1, 452},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateFee(tc.cc, tc.nIn, tc.nOut); got != tc.want {
				t.Errorf("estimateFee = %d, want %d", got, tc.want)
			}
		})
	}
}
