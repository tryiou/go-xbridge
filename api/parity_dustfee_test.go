package api

import (
	"testing"

	"go-xbridge/config"
)

func TestEffectiveDust(t *testing.T) {
	// C++: dustAmount = relayFee>0 ? 0.546*relayFee*COIN : 5460
	// (xbridgewalletconnectorbtc.cpp:1526). The conf-provided dust source is
	// `MinimumAmount` (C++ maps it onto the exchange wallets' dustAmount,
	// xbridgeexchange.cpp:145; it never reads a `DustAmount` key).
	// Coin=1e8 (BTC-like) unless noted.
	const coin = uint64(100_000_000)
	tests := []struct {
		name     string
		min      uint64
		relayFee float64
		want     uint64
	}{
		// Live relayfee wins (C++ order): 0.546*0.0001*1e8 = 5460.
		{"relayFee set", 0, 0.0001, 5460},
		// Relayfee differs from fallback (COIN=1e6) -> 0.546*0.0001*1e6 = 54.6 -> 54.
		{"relayFee set small coin", 0, 0.0001, 54},
		// No relayfee: conf MinimumAmount used directly when set.
		{"minimumAmount set", 100, 0, 100},
		{"minimumAmount set (large)", 546, 0, 546},
		// No relayfee, no MinimumAmount: falls back to C++ constant 5460.
		{"default 5460", 0, 0, cppDustFallback},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cc := &config.CoinConf{MinimumAmount: tc.min, Coin: coin}
			if tc.name == "relayFee set small coin" {
				cc.Coin = 1_000_000
			}
			if got := effectiveDust(cc, tc.relayFee); got != tc.want {
				t.Errorf("effectiveDust = %d, want %d", got, tc.want)
			}
		})
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
