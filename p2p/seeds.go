package p2p

import (
	"net"
	"strconv"
)

// Network bootstrap seeds. These are NETWORK bootstrap constants — analogous to
// Bitcoin's vSeeds — NOT coin definitions. They tell go-xbridge where the
// Blocknet P2P network lives so it can connect "like a core wallet" without a
// manually-supplied service-node URL. They do NOT violate the "nothing
// hardcoded" rule, which governs coin [TICKER] connectors (read from
// xbridge.conf), not the network's own bootstrap infrastructure.
//
// The testnet fixed seeds are the entries from src/chainparamsseeds.h
// (pnSeed6_test) on the testnet default P2P port. The mainnet fixed seeds
// were replaced with live-verified peers (see below); the DNS hostnames are
// the DNS seeds from src/chainparams.cpp.

// The fixed IP seeds below were verified live on 2026-09-23: each held
// chain tip as a fully-synced /Blocknet:4.4.1/ peer with a sub-200ms TCP
// handshake on the network's default P2P port (the previous
// chainparamsseeds.h snapshot had rotted — all 11 refused connections).
// Static IPs churn by nature; these are a bootstrap fallback behind the DNS
// seeds and gossip discovery, not trusted parties. Re-verify before relying
// on any entry older than a few months.

var (
	mainnetDNSSeeds = []string{"seednode1.blocknet.org", "seednode2.blocknet.org"}
	testnetDNSSeeds = []string{"testnet-seednode1.blocknet.org", "testnet-seednode2.blocknet.org"}

	// mainnet fixed seeds, port 41412 (verified live 2026-09-23, fastest first).
	mainnetFixedSeeds = []string{
		"5.189.179.250:41412",
		"169.58.115.176:41412",
		"157.90.75.76:41412",
		"88.99.144.106:41412",
		"199.241.137.81:41412",
		"94.23.163.163:41412",
		"135.181.141.137:41412",
		"62.77.152.159:41412",
		"134.195.198.209:41412",
		"45.27.73.200:41412",
		"64.31.61.150:41412",
	}
	// testnet fixed seeds (pnSeed6_test), port 41474.
	testnetFixedSeeds = []string{
		"3.16.3.126:41474",
		"18.224.130.185:41474",
		"18.213.44.27:41474",
		"34.196.102.239:41474",
	}
)

// defaultPort returns the standard P2P port for a network.
func defaultPort(network string) int {
	switch network {
	case "testnet":
		return 41474
	case "regtest":
		return 41489
	default:
		return 41412 // mainnet
	}
}

// SeedList returns the DNS seed hostnames and the fixed ip:port seeds for a
// network. "mainnet" is the default; "testnet" and "regtest" are also known.
// Unknown networks fall back to mainnet.
func SeedList(network string) (dns []string, fixed []string) {
	switch network {
	case "testnet":
		return testnetDNSSeeds, testnetFixedSeeds
	case "regtest":
		return nil, nil // no public seeds configured
	default:
		return mainnetDNSSeeds, mainnetFixedSeeds
	}
}

// BootstrapAddrs returns the concrete host:port addresses to try first when
// discovering the network: DNS seeds resolved to their network-port addresses,
// followed by the fixed seeds. DNS resolution failures are skipped silently —
// the fixed seeds and already-discovered peers cover the rest.
func BootstrapAddrs(network string) []string {
	dns, fixed := SeedList(network)
	port := defaultPort(network)
	var out []string
	for _, host := range dns {
		if ips, err := net.LookupHost(host); err == nil {
			for _, ip := range ips {
				out = append(out, net.JoinHostPort(ip, strconv.Itoa(port)))
			}
		}
	}
	out = append(out, fixed...)
	return out
}
