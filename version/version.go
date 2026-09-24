// Package version is the single source of truth for every version number
// go-xbridge speaks or advertises. Each family below has exactly ONE literal
// in the tree (in this file); flag defaults, wire stamps, decode gates,
// getnetworkinfo, tests, and docs all derive from these symbols.
//
// This package is deliberately a leaf: it imports nothing repo-internal, so
// proto, p2p, api, and cmd can all import it without an import cycle (the
// reason the XBridge version cannot live in api, which imports proto).
//
// Bump procedure (deliberate, never silent): change the const (the guard in
// api/version_singlesource_test.go carries zero literals and follows
// automatically), regenerate the conformance signature vectors that sign
// version-bearing packet bytes
// (conformance/conformance_suite_test.go TestWirePacketHeader /
// TestWireMainnetFrame), then re-verify against a live hub triangle.
package version

// DefaultXBridgeProtocolVersion is XBRIDGE_PROTOCOL_VERSION
// (src/xbridge/version.h): the XBridge wire version this client speaks unless
// overridden. THE literal for this family.
const DefaultXBridgeProtocolVersion uint32 = 55

// XBridgeProtocolVersion is the effective XBridge wire version: the value
// stamped into every outbound packet header (proto.NewPacket), required by
// the inbound decode gate (proto.Unmarshal), and required of hubs by the
// registry gate (servicenode.Registry.Pick) — matching C++'s compile-time
// XBRIDGE_PROTOCOL_VERSION. It defaults to DefaultXBridgeProtocolVersion;
// cmd/xbridged's -xbridgeversion flag overrides it pre-start via
// SetXBridgeProtocolVersion (a daemon-level knob, never touched by
// dxLoadXBridgeConf hot-reload). Tests mutate it only under save/restore
// (no t.Parallel anywhere in this repo).
var XBridgeProtocolVersion = DefaultXBridgeProtocolVersion

// SetXBridgeProtocolVersion overrides the effective XBridge wire version (the
// -xbridgeversion daemon flag). It must be called before the P2P engine
// starts: connections and the registry read XBridgeProtocolVersion at packet
// build/decode and hub-selection time, so a mid-run change would split the
// daemon's own view of the wire. Zero would make the gate reject every
// packet, so it panics.
func SetXBridgeProtocolVersion(v uint32) {
	if v == 0 {
		panic("version: SetXBridgeProtocolVersion(0) would reject every packet")
	}
	XBridgeProtocolVersion = v
}

// DefaultWalletVersion is the Blocknet CLIENT_VERSION advertised in
// getnetworkinfo (version field) when -walletversion is not set. Mirrors the
// stock Blocknet Core CLIENT_VERSION (configure.ac): BLOCK-DX pings
// getnetworkinfo for its wallet-version gate before it will talk to the
// wallet. THE literal for this family.
const DefaultWalletVersion = 4040100

// DefaultWalletVersionStr is the Blocknet subversion string advertised in
// getnetworkinfo (subversion field) when -walletversionstr is not set.
// Mirrors the stock Blocknet Core subversion format (client version string in
// clientversion.cpp). THE literal for this family.
const DefaultWalletVersionStr = "/Blocknet:4.4.1/"

// BitcoinProtocolVersion is Blocknet's PROTOCOL_VERSION (src/version.h:12):
// the Bitcoin P2P `version` message field value. The node rejects peers
// advertising a lower version. THE literal for this family.
const BitcoinProtocolVersion = 70713

// MinPeerProtoVersion is MIN_PEER_PROTO_VERSION (src/version.h:27). C++
// disconnects a peer advertising a lower version (net_processing.cpp:1617-1626);
// the Go handshake enforces the same gate. THE literal for this family.
const MinPeerProtoVersion = 70712

// XRouterProtocolVersion is the XRouter protocol version advertised in
// getnetworkinfo (xrouterprotocolversion field). Mirrors Blocknet Core's
// XROUTER_PROTOCOL_VERSION (src/xrouter/version.h). THE literal for this
// family.
//
// NOTE: the vendored C++ header read one higher when this const was pinned
// while live service nodes advertise this value; the live-network value wins
// until verified otherwise. Verify live SN behavior (peers' advertised
// xrouterversion) before bumping.
const XRouterProtocolVersion = 50

// UserAgent identifies this client in the Bitcoin P2P version handshake.
// THE literal for this family.
const UserAgent = "/go-xbridge:0.1.0/"
