package p2p

import (
	"testing"
)

// TestSeedTables pins the pure seed/port tables without touching the network:
// mainnet/testnet return their DNS seed lists, regtest returns none (to avoid
// lookups, as peer_manager_test.go relies on), unknown networks fall back to
// mainnet, and every network maps to its fixed P2P port. BootstrapAddrs is
// intentionally NOT covered here — it performs live DNS lookups
// (net.LookupHost); hermetic tests must never call it.
func TestSeedTables(t *testing.T) {
	dns, _ := SeedList("mainnet")
	if len(dns) == 0 {
		t.Error("mainnet SeedList: no DNS seeds")
	}
	dns, _ = SeedList("")
	if len(dns) == 0 {
		t.Error("default (empty) SeedList: want mainnet fallback, got none")
	}
	dns, _ = SeedList("testnet")
	if len(dns) == 0 {
		t.Error("testnet SeedList: no DNS seeds")
	}
	if dns, fixed := SeedList("regtest"); len(dns) != 0 || len(fixed) != 0 {
		t.Errorf("regtest SeedList = (%v, %v), want no seeds", dns, fixed)
	}
	for network, want := range map[string]int{
		"mainnet": 41412, "": 41412, "unknown-net": 41412,
		"testnet": 41474, "regtest": 41489,
	} {
		if got := defaultPort(network); got != want {
			t.Errorf("defaultPort(%q) = %d, want %d", network, got, want)
		}
	}
}
