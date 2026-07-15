package p2p

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"net"
	"time"
)

// Bitcoin P2P version message field values used by xbridge-go.
const (
	// BitcoinProtocolVersion is Blocknet's PROTOCOL_VERSION (src/version.h:12).
	// The node rejects peers advertising a lower version.
	BitcoinProtocolVersion = 70713

	// ServiceNodeNone — a thin client advertises no services.
	ServiceNodeNone uint64 = 0

	// UserAgent identifies this client in the version handshake.
	UserAgent = "/xbridge-go:0.1.0/"
)

// VersionMessage is a Bitcoin P2P `version` payload.
type VersionMessage struct {
	Version     int32
	Services    uint64
	Timestamp   int64
	AddrRecv    NetAddr
	AddrFrom    NetAddr
	Nonce       uint64
	UserAgent   string
	StartHeight int32
	Relay       bool
	// FXRouter is the trailing byte in Blocknet's version message. Service
	// nodes (XRouter) set it true; a thin client leaves it false. C++ only
	// reads it `if (!vRecv.empty())`, so a peer that omits the byte defaults
	// to false — which is what enables address gossip (the peer sends us
	// getaddr and advertises its addr list). We send it explicitly false so
	// discovery works against stock service nodes.
	FXRouter bool
}

// NetAddr is a Bitcoin net_addr (services, IP, port) without a timestamp. The
// timestamp variant only appears in `addr` messages for protocol versions
// < 31402; the `version` message uses the timestamp-less form.
type NetAddr struct {
	Services uint64
	IP       net.IP
	Port     uint16
}

// ipTo16 returns the 16-byte serialization of an IP. IPv4 addresses are mapped
// into the IPv6 ::ffff:0:0/96 space, matching Bitcoin's CNetAddr::Serialize.
func ipTo16(ip net.IP) [16]byte {
	var out [16]byte
	if ip4 := ip.To4(); ip4 != nil {
		out[10], out[11] = 0xff, 0xff
		copy(out[12:], ip4)
		return out
	}
	if ip6 := ip.To16(); ip6 != nil {
		copy(out[:], ip6)
	}
	return out
}

// marshalNetAddr serializes a net_addr: services(8 LE) || ip(16) || port(2 BE).
// Note the port is big-endian (network byte order), unlike the rest of the
// frame, which is little-endian.
func marshalNetAddr(a NetAddr) []byte {
	buf := make([]byte, 26)
	binary.LittleEndian.PutUint64(buf[0:8], a.Services)
	ip := ipTo16(a.IP)
	copy(buf[8:24], ip[:])
	binary.BigEndian.PutUint16(buf[24:26], a.Port)
	return buf
}

// marshalVarStr serializes a length-prefixed string using a Bitcoin VarInt.
// User agents are short, so only the single-byte and 0xFD (uint16) encodings
// are needed here.
func marshalVarStr(s string) []byte {
	b := []byte(s)
	buf := new(bytes.Buffer)
	switch {
	case len(b) < 0xFD:
		buf.WriteByte(byte(len(b)))
	case len(b) <= 0xFFFF:
		buf.WriteByte(0xFD)
		var l [2]byte
		binary.LittleEndian.PutUint16(l[:], uint16(len(b)))
		buf.Write(l[:])
	default:
		buf.WriteByte(0xFD)
		var l [2]byte
		binary.LittleEndian.PutUint16(l[:], uint16(len(b)))
		buf.Write(l[:])
	}
	buf.Write(b)
	return buf.Bytes()
}

// Marshal serializes the version message to wire bytes:
//
//	version(4 LE) || services(8 LE) || timestamp(8 LE) ||
//	addr_recv(26) || addr_from(26) || nonce(8 LE) ||
//	user_agent(varstr) || start_height(4 LE) || relay(1) || fxrouter(1)
func (m *VersionMessage) Marshal() []byte {
	buf := new(bytes.Buffer)
	var f [8]byte
	binary.LittleEndian.PutUint32(f[:4], uint32(m.Version))
	buf.Write(f[:4])
	binary.LittleEndian.PutUint64(f[:], m.Services)
	buf.Write(f[:])
	binary.LittleEndian.PutUint64(f[:], uint64(m.Timestamp))
	buf.Write(f[:])
	buf.Write(marshalNetAddr(m.AddrRecv))
	buf.Write(marshalNetAddr(m.AddrFrom))
	binary.LittleEndian.PutUint64(f[:], m.Nonce)
	buf.Write(f[:])
	buf.Write(marshalVarStr(m.UserAgent))
	binary.LittleEndian.PutUint32(f[:4], uint32(m.StartHeight))
	buf.Write(f[:4])
	if m.Relay {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	if m.FXRouter {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

// NewVersion builds an outbound version message for a connection to remote.
// remote may be nil (e.g. in tests), in which case addr_recv is left zeroed.
func NewVersion(remote net.Addr) *VersionMessage {
	var addrRecv NetAddr
	if remote != nil {
		if tcp, ok := remote.(*net.TCPAddr); ok {
			addrRecv = NetAddr{Services: ServiceNodeNone, IP: tcp.IP, Port: uint16(tcp.Port)}
		}
	}
	return &VersionMessage{
		Version:     BitcoinProtocolVersion,
		Services:    ServiceNodeNone,
		Timestamp:   time.Now().Unix(),
		AddrRecv:    addrRecv,
		AddrFrom:    NetAddr{},
		Nonce:       rand.Uint64(),
		UserAgent:   UserAgent,
		StartHeight: 0, // thin client has no chain height
		Relay:       false,
		FXRouter:    false, // thin client is not an XRouter hub
	}
}

// UnmarshalVersion parses a `version` payload from the wire (inverse of
// Marshal). It is used to inspect the peer's advertised version.
func UnmarshalVersion(b []byte) (*VersionMessage, error) {
	m := &VersionMessage{}
	if len(b) < 4+8+8 {
		return nil, errors.New("p2p: version payload too short")
	}
	pos := 0
	m.Version = int32(binary.LittleEndian.Uint32(b[pos:]))
	pos += 4
	m.Services = binary.LittleEndian.Uint64(b[pos:])
	pos += 8
	m.Timestamp = int64(binary.LittleEndian.Uint64(b[pos:]))
	pos += 8
	addrRecv, n, err := unmarshalNetAddr(b[pos:])
	if err != nil {
		return nil, err
	}
	m.AddrRecv = addrRecv
	pos += n
	addrFrom, n, err := unmarshalNetAddr(b[pos:])
	if err != nil {
		return nil, err
	}
	m.AddrFrom = addrFrom
	pos += n
	if pos+8 > len(b) {
		return nil, errors.New("p2p: version payload too short (nonce)")
	}
	m.Nonce = binary.LittleEndian.Uint64(b[pos:])
	pos += 8
	ua, n, err := unmarshalVarStr(b[pos:])
	if err != nil {
		return nil, err
	}
	m.UserAgent = ua
	pos += n
	if pos+4 > len(b) {
		return nil, errors.New("p2p: version payload too short (start height)")
	}
	m.StartHeight = int32(binary.LittleEndian.Uint32(b[pos:]))
	pos += 4
	if pos < len(b) {
		m.Relay = b[pos] != 0
		pos++
	}
	if pos < len(b) {
		m.FXRouter = b[pos] != 0
	}
	return m, nil
}

// unmarshalNetAddr parses a 26-byte net_addr (services || ip(16) || port(2 BE)).
func unmarshalNetAddr(b []byte) (NetAddr, int, error) {
	if len(b) < 26 {
		return NetAddr{}, 0, errors.New("p2p: net_addr too short")
	}
	a := NetAddr{}
	a.Services = binary.LittleEndian.Uint64(b[0:8])
	var ip16 [16]byte
	copy(ip16[:], b[8:24])
	a.IP = netIPFrom16(ip16)
	a.Port = binary.BigEndian.Uint16(b[24:26])
	return a, 26, nil
}

// netIPFrom16 converts a 16-byte address; IPv4-mapped (::ffff:a.b.c.d) becomes
// an IPv4 address, matching ipTo16's inverse.
func netIPFrom16(b [16]byte) net.IP {
	if b[10] == 0xff && b[11] == 0xff {
		return net.IPv4(b[12], b[13], b[14], b[15])
	}
	return net.IP(b[:])
}

// unmarshalVarStr parses a Bitcoin VarInt-prefixed string.
func unmarshalVarStr(b []byte) (string, int, error) {
	if len(b) < 1 {
		return "", 0, errors.New("p2p: varstr too short")
	}
	first := b[0]
	pos := 1
	var n int
	switch {
	case first < 0xfd:
		n = int(first)
	case first == 0xfd:
		if len(b) < 3 {
			return "", 0, errors.New("p2p: varstr too short")
		}
		n = int(binary.LittleEndian.Uint16(b[1:3]))
		pos = 3
	case first == 0xfe:
		if len(b) < 5 {
			return "", 0, errors.New("p2p: varstr too short")
		}
		n = int(binary.LittleEndian.Uint32(b[1:5]))
		pos = 5
	default:
		if len(b) < 9 {
			return "", 0, errors.New("p2p: varstr too short")
		}
		n = int(binary.LittleEndian.Uint64(b[1:9]))
		pos = 9
	}
	if pos+n > len(b) {
		return "", 0, errors.New("p2p: varstr length exceeds payload")
	}
	return string(b[pos : pos+n]), pos + n, nil
}
