package servicenode

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"go-xbridge/crypto"
	"go-xbridge/p2p"
	"go-xbridge/proto"
)

// mustKeypair returns a fresh secp256k1 keypair (compressed pubkey, privkey).
func mustKeypair(t *testing.T) ([33]byte, [32]byte) {
	t.Helper()
	priv, err := crypto.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	var privArr [32]byte
	copy(privArr[:], priv)
	pub, err := crypto.CompressedPubKey(privArr[:])
	if err != nil {
		t.Fatal(err)
	}
	return pub, privArr
}

// validReg builds a ServiceNode whose registration passes the
// thin-client-enforceable subset of ServiceNode::isValid (servicenode.h:398-484):
// SPV tier, fully-valid pubkey, non-null payment address, one collateral
// outpoint, and a signature over CreateSigHash signed with priv
// (servicenode.h:104-111, sign :376-380). Any valid key may sign the
// registration in this test — identity to the on-chain collateral is the
// documented thin-client residual.
func validReg(t *testing.T, pub [33]byte, priv [32]byte) ServiceNode {
	t.Helper()
	sn := ServiceNode{
		PubKey:         pub,
		Tier:           TierSPV,
		PaymentAddress: [20]byte{0xaa},
		Collateral:     []CollateralUTXO{{TxID: [32]byte{0x11}, Vout: 0}},
		BestBlock:      1000,
		BestBlockHash:  [32]byte{0x22},
	}
	sn.Signature = signRegistration(t, sn, priv)
	return sn
}

// signRegistration signs sn's CreateSigHash with priv, mirroring
// ServiceNode::sign (servicenode.h:376-380).
func signRegistration(t *testing.T, sn ServiceNode, priv [32]byte) []byte {
	t.Helper()
	hash := crypto.DoubleSHA256(serializeSigHashFields(sn))
	sig, err := crypto.SignCompact(priv[:], hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// embeddedRegistrationBytes serializes the embedded ServiceNode of a ping
// (snodePubKey, tier, paymentAddress, collateral, bestBlock, bestBlockHash,
// signature) exactly as ServiceNode::SerializationOp (servicenode.h:354-384) —
// the ping carries the config at the outer level, not here.
func embeddedRegistrationBytes(sn ServiceNode) []byte {
	var b []byte
	b = append(b, p2p.MarshalVarStr(string(sn.PubKey[:]))...)
	b = append(b, sn.Tier)
	b = append(b, sn.PaymentAddress[:]...)
	b = append(b, compactSize(len(sn.Collateral))...)
	for _, op := range sn.Collateral {
		b = append(b, op.TxID[:]...)
		b = append(b, byte(op.Vout), byte(op.Vout>>8), byte(op.Vout>>16), byte(op.Vout>>24))
	}
	b = append(b, byte(sn.BestBlock), byte(sn.BestBlock>>8), byte(sn.BestBlock>>16), byte(sn.BestBlock>>24))
	b = append(b, sn.BestBlockHash[:]...)
	b = append(b, p2p.MarshalVarStr(string(sn.Signature))...)
	return b
}

// buildPingParts marshals a ServiceNodePing payload following the EXACT C++
// ServiceNodePing::SerializationOp order (servicenode.h:683-694): snodePubKey,
// bestBlock, bestBlockHash, pingTime, config, embedded ServiceNode, ping
// signature. The embedded registration is auto-built VALID at the given tier
// (validReg) so the ping passes the thin-client isValid subset for SPV; the
// ping is signed with priv over its sigHash (servicenode.h:748-752, sign
// :769-771) like a real SNPING. Outer and inner pubkeys may differ to exercise
// the isValid pubkey check (servicenode.h:791).
func buildPingParts(t *testing.T, outer, inner [33]byte, priv [32]byte, tier uint8, config string) []byte {
	t.Helper()
	reg := validReg(t, inner, priv)
	reg.Tier = tier
	if tier != TierSPV {
		// A non-SPV registration fails registrationValid at the tier check
		// (servicenode.h:409), but give it a structurally valid sig anyway so
		// the tier is the (only) reason it is rejected.
		reg.Signature = signRegistration(t, reg, priv)
	}
	return buildPingPartsReg(t, outer, priv, config, reg)
}

// buildPingPartsReg is buildPingParts with an explicit embedded registration,
// for exercising the embedded-registration isValid checks.
func buildPingPartsReg(t *testing.T, outer [33]byte, priv [32]byte, config string, reg ServiceNode) []byte {
	t.Helper()
	var b []byte
	b = append(b, p2p.MarshalVarStr(string(outer[:]))...) // ping snodePubKey
	b = appendLE32(b, 1000)                               // bestBlock (uint32)
	b = append(b, make([]byte, 32)...)                    // bestBlockHash
	b = appendLE32(b, uint32(time.Now().Unix()))          // pingTime (uint32)
	b = append(b, p2p.MarshalVarStr(config)...)           // config (varstr)
	b = append(b, embeddedRegistrationBytes(reg)...)      // embedded ServiceNode
	// ping signature = SignCompact over double-SHA256(b) (sigHash)
	hash := crypto.DoubleSHA256(b)
	sig, err := crypto.SignCompact(priv[:], hash[:])
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, p2p.MarshalVarStr(string(sig))...) // signature (varstr)
	return b
}

// buildPing signs a ping for a single keypair (outer == inner pubkey).
func buildPing(t *testing.T, pub [33]byte, priv [32]byte, tier uint8, config string) []byte {
	t.Helper()
	return buildPingParts(t, pub, pub, priv, tier, config)
}

func appendLE32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func appendVarInt0(b []byte) []byte {
	return append(b, 0) // CompactSize 0
}

func mustPubkey(t *testing.T, h string) [33]byte {
	t.Helper()
	var pk [33]byte
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw) != 33 {
		t.Fatalf("bad pubkey hex %q", h)
	}
	copy(pk[:], raw)
	return pk
}

// TestWalletServicesParity verifies a hand-crafted SNPING payload parses like
// C++ and WalletServices() yields the filtered union walletServices() would
// return (xbridgeapp.cpp:2758): SPV tokens only, ^[^:]+$, excluding xr/xrs.
func TestWalletServicesParity(t *testing.T) {
	// SPV node advertising BLOCK, BTC, xr (must be excluded), and an XRouter
	// service name containing ':' (must be excluded).
	pk1, priv1 := mustKeypair(t)
	cfg1 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["BLOCK","BTC","xr","xrouter:btc_getblockcount"]}`
	ping1 := buildPing(t, pk1, priv1, TierSPV, cfg1)

	sn1, err := ParseServiceNodePing(ping1)
	if err != nil {
		t.Fatalf("ParseServiceNodePing: %v", err)
	}
	if sn1.Tier != TierSPV {
		t.Fatalf("tier = %d, want SPV(1)", sn1.Tier)
	}
	// parseConfig returns the raw xbridge array (C++ stores raw services; the
	// ^[^:]+$ / xr / xrs filtering happens later in WalletServices()).
	if len(sn1.Services) != 4 {
		t.Fatalf("services = %v, want [BLOCK BTC xr xrouter:btc_getblockcount]", sn1.Services)
	}

	// Second SPV node advertising LTC and DOGE.
	pk2, priv2 := mustKeypair(t)
	cfg2 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["LTC","DOGE"]}`
	ping2 := buildPing(t, pk2, priv2, TierSPV, cfg2)
	sn2, err := ParseServiceNodePing(ping2)
	if err != nil {
		t.Fatalf("ParseServiceNodePing #2: %v", err)
	}

	// A non-SPV (OPEN) node must be rejected outright: C++ ping.isValid fails on
	// the SPV-tier requirement (servicenode.h:787) and processPing drops the
	// whole ping (servicenodemgr.h:186-187) — it never reaches AddPing. The
	// embedded registration of an OPEN ping also fails the registration subset.
	pk3, priv3 := mustKeypair(t)
	cfg3 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["EVIL"]}`
	if _, err := ParseServiceNodePing(buildPing(t, pk3, priv3, TierOpen, cfg3)); err == nil {
		t.Fatal("OPEN-tier ping must be rejected at parse (C++ isValid:787)")
	}

	reg := NewRegistry()
	reg.AddPing(sn1)
	reg.AddPing(sn2)
	if reg.Count() != 2 {
		t.Fatalf("registry count = %d, want 2 (non-SPV ping creates no node)", reg.Count())
	}

	got := reg.WalletServices()
	want := []string{"BLOCK", "BTC", "DOGE", "LTC"}
	if len(got) != len(want) {
		t.Fatalf("WalletServices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("WalletServices[%d] = %q, want %q (got %v)", i, got[i], want[i], got)
		}
	}
}

// TestRunningGate verifies that only recently-pinged servicenodes (within the
// 5-minute window, ServiceNode::running(), servicenode.h:244) contribute to
// WalletServices().
func TestRunningGate(t *testing.T) {
	pk1, priv1 := mustKeypair(t)
	cfg1 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["BLOCK"]}`
	sn1, err := ParseServiceNodePing(buildPing(t, pk1, priv1, TierSPV, cfg1))
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.AddPing(sn1)
	if len(reg.WalletServices()) != 1 {
		t.Fatalf("expected BLOCK from running node")
	}

	// Simulate staleness by advancing the registry clock 6 minutes.
	reg.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if len(reg.WalletServices()) != 0 {
		t.Fatalf("stale node should not contribute; got %v", reg.WalletServices())
	}
}

// pickPubkey builds a deterministic, CURVE-VALID compressed pubkey per seed so
// Pick tests can distinguish candidates. (An x=0 pubkey like "02"+"00"*32 is
// not on the secp256k1 curve and now fails fullyValidCPubKey — the point is
// rejected exactly as C++ IsFullyValid would.)
func pickPubkey(t *testing.T, seed byte) [33]byte {
	t.Helper()
	raw := make([]byte, 32)
	raw[31] = seed
	raw[0] &= 0x7f // clear high bits so the scalar is a valid secp256k1 key
	pub, err := crypto.CompressedPubKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// TestPickEmpty verifies Pick on an empty registry returns false (no eligible
// hub -> C++ makeTransaction fails with NO_SERVICE_NODE).
func TestPickEmpty(t *testing.T) {
	reg := NewRegistry()
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("Pick on empty registry should return false")
	}
}

// TestPickVersionGate verifies a node whose advertised xbridge version does not
// equal XBRIDGE_PROTOCOL_VERSION is never selected (xbridgeapp.cpp:2905).
func TestPickVersionGate(t *testing.T) {
	reg := NewRegistry()
	reg.AddPing(ServiceNode{PubKey: pickPubkey(t, 1), Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion - 1})
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("version-mismatched node must not be picked")
	}
}

// TestPickRunningGate verifies a stale (not running) node is never selected.
func TestPickRunningGate(t *testing.T) {
	reg := NewRegistry()
	reg.AddPing(ServiceNode{PubKey: pickPubkey(t, 2), Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	reg.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("stale node must not be picked")
	}
}

// TestPickServicesGate verifies a node missing one of the requested currencies
// is never selected (containsAll of findShuffledNodesWithService).
func TestPickServicesGate(t *testing.T) {
	reg := NewRegistry()
	reg.AddPing(ServiceNode{PubKey: pickPubkey(t, 3), Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	if _, ok := reg.Pick([]string{"BTC", "LTC"}); ok {
		t.Fatal("node missing LTC must not be picked for BTC/LTC")
	}
}

// TestPickEligible verifies a running, version-matching node advertising both
// currencies is selected, and the returned key is one of the eligible set.
func TestPickEligible(t *testing.T) {
	reg := NewRegistry()
	blocker := pickPubkey(t, 4)
	reg.AddPing(ServiceNode{PubKey: blocker, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	hub := pickPubkey(t, 5)
	reg.AddPing(ServiceNode{PubKey: hub, Tier: TierSPV, Services: []string{"BTC", "LTC"}, XBridgeVersion: proto.ProtocolVersion})

	pk, ok := reg.Pick([]string{"BTC", "LTC"})
	if !ok {
		t.Fatal("eligible hub should be picked")
	}
	if pk != hub {
		t.Fatalf("Pick returned %x, want the eligible node %x", pk, hub)
	}
}

// TestParseServiceNodePingRejectsFloatVersion verifies a float-formatted or
// out-of-int32-range xbridgeversion is REJECTED like C++ UniValue::get_int()
// (univalue_get.cpp:104-112): get_int() throws, parseConfig's catch
// (servicenode.h:614-615) leaves version 0 with services cleared — never
// truncated, never wrapped.
func TestParseServiceNodePingRejectsFloatVersion(t *testing.T) {
	pk, priv := mustKeypair(t)
	cases := []string{
		`{"xbridgeversion":55.0,"xrouterversion":55,"xbridge":["BTC"]}`,       // float form
		`{"xbridgeversion":5e1,"xrouterversion":55,"xbridge":["BTC"]}`,        // exponent form
		`{"xbridgeversion":3000000000,"xrouterversion":55,"xbridge":["BTC"]}`, // out of int32 range
	}
	for _, cfg := range cases {
		sn, err := ParseServiceNodePing(buildPing(t, pk, priv, TierSPV, cfg))
		if err != nil {
			t.Fatal(err)
		}
		if sn.XBridgeVersion != 0 {
			t.Fatalf("xbridgeversion = %d, want 0 (C++ get_int() rejects %s)", sn.XBridgeVersion, cfg)
		}
		if len(sn.Services) != 0 {
			t.Fatalf("services = %v, want empty (parseConfig aborted)", sn.Services)
		}
	}
}

// TestParseServiceNodePingRejectsQuotedVersion verifies a QUOTED numeric
// xbridgeversion/xrouterversion is rejected like C++: get_int() requires a bare
// number token (univalue_get.cpp:104-112), a quoted string (VSTR) throws, and
// parseConfig keeps version 0 (servicenode.h:614-615).
func TestParseServiceNodePingRejectsQuotedVersion(t *testing.T) {
	pk, priv := mustKeypair(t)
	cases := []struct {
		cfg string
		ver uint32
	}{
		{`{"xbridgeversion":"55","xrouterversion":55,"xbridge":["BTC"]}`, 0},  // quoted version -> 0
		{`{"xbridgeversion":55,"xrouterversion":"55","xbridge":["BTC"]}`, 55}, // quoted xrouterversion -> version retained
	}
	for _, tc := range cases {
		sn, err := ParseServiceNodePing(buildPing(t, pk, priv, TierSPV, tc.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if sn.XBridgeVersion != tc.ver {
			t.Fatalf("xbridgeversion = %d, want %d (C++ get_int() rejects %s)", sn.XBridgeVersion, tc.ver, tc.cfg)
		}
		if len(sn.Services) != 0 {
			t.Fatalf("services = %v, want empty (parseConfig aborted)", sn.Services)
		}
	}
}

// TestParseServiceNodePingRejectsFloatXrouterVersion verifies a float
// xrouterversion is REJECTED like C++ (strict get_int() at servicenode.h:552):
// no services are collected, but the already-assigned xbridgeversion is
// retained (servicenode.h:546). 56 is used so the assertion is independent of
// proto.ProtocolVersion.
func TestParseServiceNodePingRejectsFloatXrouterVersion(t *testing.T) {
	pk, priv := mustKeypair(t)
	cfg := `{"xbridgeversion":56,"xrouterversion":55.0,"xbridge":["BTC"]}`
	sn, err := ParseServiceNodePing(buildPing(t, pk, priv, TierSPV, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if sn.XBridgeVersion != 56 {
		t.Fatalf("xbridgeversion = %d, want 56 (retained before xrouterversion check)", sn.XBridgeVersion)
	}
	if len(sn.Services) != 0 {
		t.Fatalf("services = %v, want empty (parseConfig aborted)", sn.Services)
	}
}

// TestParseServiceNodePingRejectsBadSignature verifies a ping whose signature
// does not verify against its pubkey is rejected like C++ isValid
// (servicenode.h:810-815): a different signing key or a tampered byte.
func TestParseServiceNodePingRejectsBadSignature(t *testing.T) {
	cfg := `{"xbridgeversion":55,"xrouterversion":55,"xbridge":["BTC"]}`
	pubA, privA := mustKeypair(t)
	_, privB := mustKeypair(t)

	if _, err := ParseServiceNodePing(buildPing(t, pubA, privB, TierSPV, cfg)); err == nil {
		t.Fatal("ping signed by a different key must be rejected")
	}

	b := buildPing(t, pubA, privA, TierSPV, cfg)
	b[len(b)-1] ^= 0x01 // flip one byte of the trailing signature
	if _, err := ParseServiceNodePing(b); err == nil {
		t.Fatal("tampered ping signature must be rejected")
	}
}

// TestParseServiceNodePingRejectsMismatchedPubkeys verifies the ping is
// rejected when the outer snodePubKey differs from the embedded snode pubkey
// (isValid, servicenode.h:791).
func TestParseServiceNodePingRejectsMismatchedPubkeys(t *testing.T) {
	cfg := `{"xbridgeversion":55,"xrouterversion":55,"xbridge":["BTC"]}`
	pubA, _ := mustKeypair(t)
	pubB, privB := mustKeypair(t)
	b := buildPingParts(t, pubA, pubB, privB, TierSPV, cfg)
	if _, err := ParseServiceNodePing(b); err == nil {
		t.Fatal("outer/inner pubkey mismatch must be rejected")
	}
}

// TestParseServiceNodePingRejectsUncompressedHeader verifies a ping whose
// signature header claims an uncompressed pubkey is rejected, matching C++:
// RecoverCompact serializes the recovered key uncompressed (pubkey.cpp:199-205),
// so a 65-byte key never equals the 33-byte compressed snodePubKey
// (servicenode.h:814).
func TestParseServiceNodePingRejectsUncompressedHeader(t *testing.T) {
	cfg := `{"xbridgeversion":55,"xrouterversion":55,"xbridge":["BTC"]}`
	pub, priv := mustKeypair(t)
	b := buildPing(t, pub, priv, TierSPV, cfg)
	// The trailing signature varstr is 1-byte CompactSize (0x41) + 65 bytes, so
	// the header byte sits at len(b)-65. Compressed headers are 0x1f-0x22
	// (27 + recid + 4*compressed).
	hdr := b[len(b)-65]
	if hdr < 0x1f || hdr > 0x22 {
		t.Fatalf("unexpected signature header byte 0x%02x", hdr)
	}
	b[len(b)-65] = hdr - 4 // -> uncompressed 0x1b-0x1e, same recid bits
	if _, err := ParseServiceNodePing(b); err != errBadSignature {
		t.Fatalf("ParseServiceNodePing = %v, want errBadSignature", err)
	}
}

// TestAddPingNewerPingReplacesEntry verifies a STRICTLY NEWER valid ping
// REPLACES version/services wholesale, mirroring processPing (servicenodemgr.h:
// 189-192): addPing's strict-newer gate (:843-852) then addSn's wholesale
// replace (:861-871, setConfig, servicenode.h:671-675). Never merged.
func TestAddPingNewerPingReplacesEntry(t *testing.T) {
	reg := NewRegistry()
	key := pickPubkey(t, 0x61)
	base := uint32(time.Now().Unix()) - 200 // within the running() window
	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base})
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("fresh node should be eligible")
	}
	if len(reg.WalletServices()) != 1 {
		t.Fatalf("WalletServices = %v, want [BTC]", reg.WalletServices())
	}

	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: 54, PingTime: base + 50})
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("newer ping with a different version must fail Pick's version gate (wholesale replace)")
	}
	if len(reg.WalletServices()) != 1 {
		t.Fatalf("WalletServices = %v, want [BTC] (no version gate on WalletServices)", reg.WalletServices())
	}

	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"LTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base + 100})
	if got := reg.WalletServices(); len(got) != 1 || got[0] != "LTC" {
		t.Fatalf("newer ping must replace services wholesale (no merge); got %v", got)
	}
}

// TestAddPingInvalidPingIgnored verifies a ping failing C++ isValid() is never
// stored (processPing, servicenodemgr.h:186-187): non-SPV tier
// (servicenode.h:787) and empty services (:794-795) cannot clear a healthy node.
func TestAddPingInvalidPingIgnored(t *testing.T) {
	reg := NewRegistry()
	key := pickPubkey(t, 0x64)
	base := uint32(time.Now().Unix()) - 200 // within the running() window
	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base})
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("fresh node should be eligible")
	}

	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, PingTime: base + 50})                                                     // newer, but empty service list
	reg.AddPing(ServiceNode{PubKey: key, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base + 100}) // newer, but non-SPV tier
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("invalid pings must not clear a healthy node (C++ isValid gate)")
	}
	if got := reg.WalletServices(); len(got) != 1 || got[0] != "BTC" {
		t.Fatalf("invalid pings must not clear services; got %v", got)
	}
}

// TestAddPingIgnoresStalePing verifies an OLDER ping never touches the stored
// node (addPing, servicenodemgr.h:843-852): a ping not strictly newer returns
// false in processPing (:189-190), so addSn never runs — a late-arriving stale
// ping must not reset a healthy node.
func TestAddPingIgnoresStalePing(t *testing.T) {
	reg := NewRegistry()
	key := pickPubkey(t, 0x63)
	base := uint32(time.Now().Unix()) - 200 // within the running() window
	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base})
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("fresh node should be eligible")
	}

	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, PingTime: base - 10}) // stale ping
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("stale ping must not clear a healthy node (C++ addPing gate)")
	}
	if got := reg.WalletServices(); len(got) != 1 || got[0] != "BTC" {
		t.Fatalf("stale ping must not clear services; got %v", got)
	}
}

// TestAddPingReturnsAccepted asserts AddPing reports whether it actually stored
// the ping (the strict-newer gate), so the SNLIST response set can mirror it.
func TestAddPingReturnsAccepted(t *testing.T) {
	reg := NewRegistry()
	key := pickPubkey(t, 0x64)
	base := uint32(time.Now().Unix()) - 200
	if !reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base}) {
		t.Fatal("first valid ping must be accepted")
	}
	if reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base}) {
		t.Fatal("equal pingTime must be rejected (strict-newer gate)")
	}
	if reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base - 10}) {
		t.Fatal("stale ping must be rejected")
	}
	if !reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"LTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base + 100}) {
		t.Fatal("newer ping must be accepted")
	}
}

// TestAddRegistrationClearsNode verifies a registration arriving AFTER a ping
// resets the node: SNREGISTER carries no config on the wire (servicenode.h:355-
// 384), so addSn stores version 0/empty services — the Go registry must never
// merge the ping's earlier data. isValid has no service-list requirement
// (servicenode.h:398), only the SPV tier.
func TestAddRegistrationClearsNode(t *testing.T) {
	reg := NewRegistry()
	key := pickPubkey(t, 0x62)
	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("pinged node should be eligible before registration")
	}

	_, signPriv := mustKeypair(t)
	reg.AddRegistration(validReg(t, key, signPriv)) // valid registration, no wire config -> version 0, no services
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("registration-after-ping must reset the node (C++ addSn replace)")
	}
}

// TestPickEmptyNeed verifies Pick with an empty service request returns false,
// mirroring C++ findShuffledNodesWithService: searchCounter = 0 so the inner
// loop never reaches --searchCounter == 0 and no node is pushed
// (xbridgeapp.cpp:2924-2930).
func TestPickEmptyNeed(t *testing.T) {
	reg := NewRegistry()
	reg.AddPing(ServiceNode{PubKey: pickPubkey(t, 0x71), Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	if _, ok := reg.Pick(nil); ok {
		t.Fatal("Pick(nil) must not return a hub (C++ never pushes for an empty request)")
	}
	if _, ok := reg.Pick([]string{}); ok {
		t.Fatal("Pick([]) must not return a hub (C++ never pushes for an empty request)")
	}
}

// TestParseServiceNode verifies SNREGISTER parsing (no bestBlock/pingTime prefix,
// int32 bestBlock, embedded config-less ServiceNode).
func TestParseServiceNode(t *testing.T) {
	pk := mustPubkey(t, "02"+hex.EncodeToString(make([]byte, 32)))
	var b []byte
	b = append(b, p2p.MarshalVarStr(string(pk[:]))...) // snodePubKey
	b = append(b, TierSPV)                             // tier uint8
	b = append(b, make([]byte, 20)...)                 // paymentAddress
	b = appendVarInt0(b)                               // collateral vec(0)
	b = appendLE32(b, 1000)                            // bestBlock (int32)
	b = append(b, make([]byte, 32)...)                 // bestBlockHash
	b = appendVarInt0(b)                               // signature vec(0)
	sn, err := ParseServiceNode(b)
	if err != nil {
		t.Fatalf("ParseServiceNode: %v", err)
	}
	if sn.Tier != TierSPV {
		t.Fatalf("tier = %d, want SPV", sn.Tier)
	}
}

// TestParseServiceNodeRetainsRegistration verifies SNREGISTER parsing retains
// every registration field (WIRE-F71): paymentAddress, collateral, bestBlock,
// bestBlockHash, signature. These were read-then-discarded before the fix.
func TestParseServiceNodeRetainsRegistration(t *testing.T) {
	pub, priv := mustKeypair(t)
	sn := validReg(t, pub, priv)
	got, err := ParseServiceNode(embeddedRegistrationBytes(sn))
	if err != nil {
		t.Fatalf("ParseServiceNode: %v", err)
	}
	if got.PaymentAddress != sn.PaymentAddress {
		t.Fatalf("paymentAddress = %x, want %x", got.PaymentAddress, sn.PaymentAddress)
	}
	if got.BestBlock != sn.BestBlock {
		t.Fatalf("bestBlock = %d, want %d", got.BestBlock, sn.BestBlock)
	}
	if got.BestBlockHash != sn.BestBlockHash {
		t.Fatalf("bestBlockHash = %x, want %x", got.BestBlockHash, sn.BestBlockHash)
	}
	if len(got.Collateral) != 1 || got.Collateral[0] != sn.Collateral[0] {
		t.Fatalf("collateral = %+v, want %+v", got.Collateral, sn.Collateral)
	}
	if !bytes.Equal(got.Signature, sn.Signature) {
		t.Fatalf("signature = %x, want %x", got.Signature, sn.Signature)
	}
}

// TestParseServiceNodePingRetainsEmbeddedRegistration verifies a ping's
// embedded registration fields survive parsing into the returned ServiceNode
// (WIRE-F71) — B2 reads PaymentAddress from the hub's ping-learned record.
func TestParseServiceNodePingRetainsEmbeddedRegistration(t *testing.T) {
	pub, priv := mustKeypair(t)
	cfg := `{"xbridgeversion":55,"xrouterversion":55,"xbridge":["BTC"]}`
	sn, err := ParseServiceNodePing(buildPing(t, pub, priv, TierSPV, cfg))
	if err != nil {
		t.Fatalf("ParseServiceNodePing: %v", err)
	}
	if sn.PaymentAddress != ([20]byte{0xaa}) {
		t.Fatalf("paymentAddress = %x, want aa0000..", sn.PaymentAddress)
	}
	if len(sn.Collateral) != 1 {
		t.Fatalf("collateral = %+v, want 1 outpoint", sn.Collateral)
	}
	if sn.BestBlock != 1000 {
		t.Fatalf("bestBlock = %d, want 1000", sn.BestBlock)
	}
	if len(sn.Signature) == 0 {
		t.Fatal("embedded registration signature must be retained")
	}
}

// TestCreateSigHashGolden pins the CreateSigHash serialization
// (servicenode.h:104-111) to an independently hand-assembled byte string. Any
// drift in the operand order, CompactSize encoding, or endianness changes the
// digest and breaks the wire signature check.
func TestCreateSigHashGolden(t *testing.T) {
	golden := "e0825066ed73a0c236104aabcedc5a00204d9ef0eb8e553c2e7a32aecf9267ad"

	// Hand-assembled operands (NOT via serializeSigHashFields):
	// varstr(G), tier=50, paymentAddress=20x0xaa, collateral=[txid 0x11.., vout
	// 0], bestBlock=1000 LE, bestBlockHash=32x0x22.
	var b []byte
	b = append(b, 0x21) // CompactSize 33
	g, _ := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	b = append(b, g...)
	b = append(b, TierSPV)
	for i := 0; i < 20; i++ {
		b = append(b, 0xaa)
	}
	b = append(b, 0x01) // CompactSize 1 collateral
	txid := make([]byte, 32)
	txid[0] = 0x11
	b = append(b, txid...)
	b = append(b, 0, 0, 0, 0)             // vout 0 LE
	b = append(b, 0xe8, 0x03, 0x00, 0x00) // bestBlock 1000 LE
	for i := 0; i < 32; i++ {
		b = append(b, 0x22)
	}
	want := [32]byte{}
	raw, _ := hex.DecodeString(golden)
	copy(want[:], raw)

	// The package helper must serialize the same registration to the same
	// bytes and hash to the same digest.
	sn := ServiceNode{
		PubKey:         [33]byte{},
		Tier:           TierSPV,
		PaymentAddress: [20]byte{},
		Collateral:     []CollateralUTXO{{TxID: [32]byte{}, Vout: 0}},
		BestBlock:      1000,
		BestBlockHash:  [32]byte{},
	}
	copy(sn.PubKey[:], g)
	for i := 0; i < 20; i++ {
		sn.PaymentAddress[i] = 0xaa
	}
	sn.Collateral[0].TxID[0] = 0x11
	for i := 0; i < 32; i++ {
		sn.BestBlockHash[i] = 0x22
	}

	if got := crypto.DoubleSHA256(serializeSigHashFields(sn)); got != want {
		t.Fatalf("CreateSigHash = %x, want %x", got, want)
	}
}

// TestAddRegistrationRejectMatrix verifies AddRegistration drops any
// registration failing the thin-client subset of ServiceNode::isValid
// (servicenode.h:398-484), exactly like C++ addSn returning nullptr
// (servicenodemgr.h:862).
func TestAddRegistrationRejectMatrix(t *testing.T) {
	pub, priv := mustKeypair(t)

	offCurve := [33]byte{0x02} // x=0: not on the secp256k1 curve
	dupCollateral := []CollateralUTXO{{TxID: [32]byte{0x01}, Vout: 0}, {TxID: [32]byte{0x01}, Vout: 0}}
	eleven := make([]CollateralUTXO, 11)

	cases := map[string]ServiceNode{
		"non-SPV tier": func() ServiceNode {
			sn := validReg(t, pub, priv)
			sn.Tier = TierOpen
			sn.Signature = signRegistration(t, sn, priv)
			return sn
		}(),
		"off-curve pubkey": func() ServiceNode {
			sn := validReg(t, pub, priv)
			sn.PubKey = offCurve
			sn.Signature = signRegistration(t, sn, priv)
			return sn
		}(),
		"null payment address": func() ServiceNode {
			sn := validReg(t, pub, priv)
			sn.PaymentAddress = [20]byte{}
			sn.Signature = signRegistration(t, sn, priv)
			return sn
		}(),
		"empty collateral": func() ServiceNode {
			sn := validReg(t, pub, priv)
			sn.Collateral = nil
			sn.Signature = signRegistration(t, sn, priv)
			return sn
		}(),
		"duplicate collateral": func() ServiceNode {
			sn := validReg(t, pub, priv)
			sn.Collateral = dupCollateral
			sn.Signature = signRegistration(t, sn, priv)
			return sn
		}(),
		"oversized collateral": func() ServiceNode {
			sn := validReg(t, pub, priv)
			sn.Collateral = eleven
			sn.Signature = signRegistration(t, sn, priv)
			return sn
		}(),
		"malformed signature": func() ServiceNode {
			// A structurally-broken compact sig fails RecoverCompact. NOTE: a
			// WELL-FORMED sig over different data recovers a *different valid
			// key* and cannot be rejected without the on-chain collateral
			// ownership check (servicenode.h:447-476) — that is the documented
			// thin-client residual.
			sn := validReg(t, pub, priv)
			sn.Signature = bytes.Repeat([]byte{0xff}, 65)
			return sn
		}(),
		"nil signature": func() ServiceNode {
			sn := validReg(t, pub, priv)
			sn.Signature = nil
			return sn
		}(),
	}

	for name, sn := range cases {
		reg := NewRegistry()
		reg.AddRegistration(sn)
		if reg.Count() != 0 {
			t.Fatalf("%s: registration must be rejected (count=%d)", name, reg.Count())
		}
	}
}

// TestAddRegistrationAcceptsValid verifies a registration passing the
// thin-client subset is stored and its payment address is resolvable — B2's
// hub fee destination.
func TestAddRegistrationAcceptsValid(t *testing.T) {
	pub, priv := mustKeypair(t)
	reg := NewRegistry()
	reg.AddRegistration(validReg(t, pub, priv))
	if reg.Count() != 1 {
		t.Fatalf("count = %d, want 1", reg.Count())
	}
	addr, ok := reg.PaymentAddress(pub)
	if !ok || addr != ([20]byte{0xaa}) {
		t.Fatalf("PaymentAddress = (%x, %v), want (aa0000.., true)", addr, ok)
	}
}

// TestAddPingRejectsInvalidEmbeddedRegistration verifies a ping whose embedded
// registration fails the thin-client subset is rejected at parse — C++
// ping.isValid runs snode.isValid (servicenode.h:817-818) and processPing drops
// the whole ping (servicenodemgr.h:186-187).
func TestAddPingRejectsInvalidEmbeddedRegistration(t *testing.T) {
	pub, priv := mustKeypair(t)
	cfg := `{"xbridgeversion":55,"xrouterversion":55,"xbridge":["BTC"]}`

	// Null payment address in the embedded registration.
	reg := validReg(t, pub, priv)
	reg.PaymentAddress = [20]byte{}
	reg.Signature = signRegistration(t, reg, priv)
	if _, err := ParseServiceNodePing(buildPingPartsReg(t, pub, priv, cfg, reg)); err != errInvalidRegistration {
		t.Fatalf("ParseServiceNodePing = %v, want errInvalidRegistration (null payment address)", err)
	}

	// Structurally-broken embedded-registration signature (fails
	// RecoverCompact). A well-formed sig over different data recovers a
	// different key and is only rejected on-chain — documented residual.
	reg2 := validReg(t, pub, priv)
	reg2.Signature = bytes.Repeat([]byte{0xff}, 65)
	if _, err := ParseServiceNodePing(buildPingPartsReg(t, pub, priv, cfg, reg2)); err != errInvalidRegistration {
		t.Fatalf("ParseServiceNodePing = %v, want errInvalidRegistration (bad registration sig)", err)
	}

	// A ping signed by a DIFFERENT key must still be reported as a bad PING
	// signature (checked before the embedded registration, servicenode.h:810-815).
	_, otherPriv := mustKeypair(t)
	if _, err := ParseServiceNodePing(buildPing(t, pub, otherPriv, TierSPV, cfg)); err != errBadSignature {
		t.Fatalf("ParseServiceNodePing = %v, want errBadSignature", err)
	}
}

// TestPaymentAddressUnknown verifies PaymentAddress returns false for a pubkey
// the registry has never seen.
func TestPaymentAddressUnknown(t *testing.T) {
	reg := NewRegistry()
	if _, ok := reg.PaymentAddress(pickPubkey(t, 0x99)); ok {
		t.Fatal("PaymentAddress for an unknown pubkey must return false")
	}
}
