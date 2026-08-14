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
// The fixed IP seeds are the mainnet/testnet entries from src/chainparamsseeds.h
// (pnSeed6_main / pnSeed6_test), all on their network's default P2P port. The
// DNS hostnames are the DNS seeds from src/chainparams.cpp.

var (
	mainnetDNSSeeds = []string{"seednode1.blocknet.org", "seednode2.blocknet.org"}
	testnetDNSSeeds = []string{"testnet-seednode1.blocknet.org", "testnet-seednode2.blocknet.org"}

	// mainnet fixed seeds (pnSeed6_main), port 41412.
	mainnetFixedSeeds = []string{
		"178.62.90.213:41412",
		"138.197.73.214:41412",
		"34.235.49.248:41412",
		"35.157.52.158:41412",
		"18.196.208.65:41412",
		"13.251.15.150:41412",
		"13.229.39.34:41412",
		"52.56.35.74:41412",
		"35.177.138.53:41412",
		"35.178.142.231:41412",
		"35.176.65.103:41412",
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
