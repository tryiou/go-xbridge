package p2p

import (
	"encoding/binary"
	"errors"
	"net"
)

// P2P command names beyond XBridge, used by the discovery layer.
const (
	CmdGetAddr = "getaddr"
	CmdAddr    = "addr"
	CmdPing    = "ping"
	CmdPong    = "pong"
)

// AddrEntry is one decoded record from an `addr` message: a Unix timestamp, the
// peer's advertised services, its IP, and its port. This mirrors Bitcoin's
// addr wire format (time(4 LE) || services(8 LE) || net_addr(26)), which is the
// legacy form peers send (addrv2 is only negotiated via sendaddrv2, which we
// do not advertise).
type AddrEntry struct {
	Time     uint32
	Services uint64
	IP       net.IP
	Port     uint16
}

// MaxAddrRecords mirrors C++ MAX_ADDR_TO_SEND (net.h:53): the cap on how many
// addresses a single `addr` message may carry. C++ Misbehaves and drops a
// message with more (net_processing.cpp:1825-1830).
const MaxAddrRecords = 1000

// ParseAddr decodes a legacy `addr` message payload: a CompactSize count
// followed by that many (time, services, net_addr) tuples. It is tolerant of a
// payload that carries fewer than the declared count (it returns what it can
// decode), matching Bitcoin's own lenient parsing, but rejects a declared count
// above MaxAddrRecords like C++.
func ParseAddr(payload []byte) ([]AddrEntry, error) {
	n, off, err := readVarInt(payload, 0)
	if err != nil {
		return nil, err
	}
	if n > MaxAddrRecords {
		return nil, errors.New("p2p: addr payload exceeds 1000 records")
	}
	out := make([]AddrEntry, 0, n)
	for i := 0; i < n; i++ {
		// Per-entry wire size: time(4) + services(8) + ip(16) + port(2).
		if off+4+8+16+2 > len(payload) {
			// Truncated record; stop rather than mis-decode.
			break
		}
		t := binary.LittleEndian.Uint32(payload[off : off+4])
		off += 4
		svc := binary.LittleEndian.Uint64(payload[off : off+8])
		off += 8
		var ip16 [16]byte
		copy(ip16[:], payload[off:off+16])
		off += 16
		port := binary.BigEndian.Uint16(payload[off : off+2])
		off += 2
		out = append(out, AddrEntry{
			Time:     t,
			Services: svc,
			IP:       netIPFrom16(ip16),
			Port:     port,
		})
	}
	return out, nil
}

// MarshalAddr encodes an AddrEntry slice into a legacy `addr` payload.
func MarshalAddr(addrs []AddrEntry) []byte {
	buf := writeVarInt(len(addrs))
	for _, a := range addrs {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], a.Time)
		buf = append(buf, b[:]...)

		var s [8]byte
		binary.LittleEndian.PutUint64(s[:], a.Services)
		buf = append(buf, s[:]...)

		ip := ipTo16(a.IP)
		buf = append(buf, ip[:]...)

		var p [2]byte
		binary.BigEndian.PutUint16(p[:], a.Port)
		buf = append(buf, p[:]...)
	}
	return buf
}
