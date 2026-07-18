// Package discovery implements lightweight P2P network discovery for
// go-xbridge: it maintains a small pool of outbound Blocknet peers, exchanges
// getaddr/addr gossip to learn more peers, relays XBridge packets from any peer
// into a single consumer channel, and broadcasts our own packets to the pool.
//
// It is a thin client, not a full node: it never downloads blocks or serves
// inbound connections, and it relies on the fact that every service node relays
// XBridge traffic to all its peers (so any single healthy peer delivers the
// full order book).
package discovery

import (
	"math/rand"
	"net"
	"sync"
	"time"

	"go-xbridge/p2p"
)

// addrEntry is a discovered peer address tracked by AddrMan.
type addrEntry struct {
	addr     string // "host:port", also the map key
	ip       net.IP
	port     uint16
	services uint64
	seen     time.Time
	lastTry  time.Time
}

// AddrMan is a thread-safe, best-effort address manager. It de-duplicates peers
// by "host:port", refreshes their seen time on re-observation, and can return a
// random sample for getaddr responses or for choosing new outbound peers.
type AddrMan struct {
	mu    sync.Mutex
	addrs map[string]*addrEntry
}

// NewAddrMan returns an empty AddrMan.
func NewAddrMan() *AddrMan {
	return &AddrMan{addrs: make(map[string]*addrEntry)}
}

// key builds the de-dup key for an IP/port pair.
func key(ip net.IP, port uint16) string {
	return net.JoinHostPort(ip.String(), itoa(port))
}

// Add records (or refreshes) a peer address. A nil IP or zero port is ignored.
func (a *AddrMan) Add(ip net.IP, port uint16, services uint64) {
	if ip == nil || port == 0 {
		return
	}
	k := key(ip, port)
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.addrs[k]; ok {
		e.seen = time.Now()
		e.services = services
		return
	}
	a.addrs[k] = &addrEntry{
		addr:     k,
		ip:       ip,
		port:     port,
		services: services,
		seen:     time.Now(),
	}
}

// AddSlice records a batch of addr-message entries.
func (a *AddrMan) AddSlice(entries []p2p.AddrEntry) {
	for _, e := range entries {
		a.Add(e.IP, e.Port, e.Services)
	}
}

// Count returns the number of unique known addresses.
func (a *AddrMan) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.addrs)
}

// All returns every known address as p2p.AddrEntry records.
func (a *AddrMan) All() []p2p.AddrEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]p2p.AddrEntry, 0, len(a.addrs))
	for _, e := range a.addrs {
		out = append(out, p2p.AddrEntry{
			Time:     uint32(e.seen.Unix()),
			Services: e.services,
			IP:       e.ip,
			Port:     e.port,
		})
	}
	return out
}

// Random returns up to n addresses, shuffled. If fewer than n are known, all are
// returned. The result is safe to modify by the caller.
func (a *AddrMan) Random(n int) []p2p.AddrEntry {
	all := a.All()
	if n > len(all) {
		n = len(all)
	}
	if n == 0 {
		return nil
	}
	rand.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	return all[:n]
}

// Prune drops addresses not seen within the given window. It is best-effaced if
// called concurrently with Add/Random (guarded by the mutex).
func (a *AddrMan) Prune(olderThan time.Duration) {
	cutoff := time.Now().Add(-olderThan)
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, e := range a.addrs {
		if e.seen.Before(cutoff) {
			delete(a.addrs, k)
		}
	}
}

// itoa is a tiny uint16→string helper to avoid importing strconv in the hot
// path of Add.
func itoa(u uint16) string {
	if u == 0 {
		return "0"
	}
	var buf [5]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}
