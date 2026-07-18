package api

import (
	"testing"
)

// TestAcceptableLockTimeDrift mirrors C++ BtcWalletConnector::acceptableLockTimeDrift:
// ourLT is OUR expectation (same deposit role, same coin) and theirLT is the
// counterparty's; the check is one-sided (reject when theirs is far below ours).
func TestAcceptableLockTimeDrift(t *testing.T) {
	// baseline: our deposit locks at block 800000, blockTime 600s.
	// drift = max(900, 4*600=2400) = 2400s. A counterparty lockTime up to
	// 2400s/600s = 4 blocks below ours is allowed.
	const our = uint32(800000)

	tests := []struct {
		name   string
		their  uint32
		blockT uint32
		want   bool
	}{
		{"counterparty zero rejected", 0, 600, false},
		{"our zero rejected", our, 600, false}, // set via ourLT param below
		{"counterparty >= threshold rejected", 500_000_000, 600, false},
		{"our >= threshold rejected", 500_000_000, 600, false},
		{"equal locktimes accepted", our, 600, true},
		{"3 blocks below accepted (BTC)", our - 3, 600, true},    // 3*600=1800 <= 2400
		{"10 blocks below rejected (BTC)", our - 10, 600, false}, // 10*600=6000 > 2400
		{"far above accepted (allowed)", our + 1000, 600, true},  // one-sided: larger OK
		// default blockTime 60: drift = max(900, 240) = 900s => 15 blocks.
		{"3 below accepted (default bt)", our - 3, 0, true},    // 3*60=180 <= 900
		{"20 below rejected (default bt)", our - 20, 0, false}, // 20*60=1200 > 900
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bt := tc.blockT
			if bt == 0 {
				bt = 60 // default when unspecified
			}
			if tc.name == "our zero rejected" {
				if got := acceptableLockTimeDrift(0, tc.their, bt); got != false {
					t.Errorf("acceptableLockTimeDrift(0, %d) = %v, want false", tc.their, got)
				}
				return
			}
			if got := acceptableLockTimeDrift(our, tc.their, bt); got != tc.want {
				t.Errorf("acceptableLockTimeDrift(%d, %d, %d) = %v, want %v", our, tc.their, bt, got, tc.want)
			}
		})
	}
}
