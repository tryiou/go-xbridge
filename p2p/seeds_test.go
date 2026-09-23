package p2p

import (
	"net"
	"strconv"
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

// TestFixedSeedsWellFormed pins the fixed-seed table shape without touching
// the network: every entry must be a parseable IP carrying its network's
// default P2P port, with no duplicates. Ports are literals (not defaultPort)
// so a regression in defaultPort itself fails here independently of
// TestSeedTables. Liveness itself is not unit-testable (it rots with the
// internet); the 2026-09-23 refresh was verified with live TCP handshakes
// against each entry, and TestSeedTables pins the tables' presence.
func TestFixedSeedsWellFormed(t *testing.T) {
	for network, seeds := range map[string][]string{
		"mainnet": mainnetFixedSeeds,
		"testnet": testnetFixedSeeds,
	} {
		if len(seeds) == 0 {
			t.Errorf("%s: no fixed seeds", network)
		}
		wantPort := map[string]int{"mainnet": 41412, "testnet": 41474}[network]
		seen := make(map[string]bool, len(seeds))
		for _, s := range seeds {
			if seen[s] {
				t.Errorf("%s: duplicate entry %q", network, s)
			}
			seen[s] = true
			host, portStr, err := net.SplitHostPort(s)
			if err != nil {
				t.Errorf("%s: entry %q does not parse as host:port: %v", network, s, err)
				continue
			}
			if net.ParseIP(host) == nil {
				t.Errorf("%s: entry %q host %q is not an IP", network, s, host)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil || port != wantPort {
				t.Errorf("%s: entry %q port = %q, want %d", network, s, portStr, wantPort)
			}
		}
	}
}
