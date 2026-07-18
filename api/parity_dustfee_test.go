package api

import (
	"testing"

	"go-xbridge/config"
)

func TestEffectiveDust(t *testing.T) {
	tests := []struct {
		name  string
		relay float64
		dust  uint64
		want  uint64
	}{
		// 0.546 * 0.00001 * 1e6 = 5.46 -> 5
		{"relayFee primary", 0.00001, 100, 5},
		{"relayFee only", 0.0001, 0, 54}, // 0.546*0.0001*1e6 = 54.6 -> 54
		{"dustAmount fallback", 0, 100, 100},
		{"default 5460", 0, 0, 5460},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cc := &config.CoinConf{RelayFee: tc.relay, DustAmount: tc.dust}
			if got := effectiveDust(cc); got != tc.want {
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
