package p2p

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"xbridge-go/proto"
)

// liveXBridgePacket is a real `xbridge` P2P message payload captured from a
// Blocknet 4.4.1 service node (coreproxy.airdns.org:42111). It is the full
// Bitcoin P2P payload: a CompactSize varint length prefix, the 28-byte
// transport envelope (20-byte dest addr + 8-byte timestamp), then the
// XBridgePacket (version=55, command=3 xbcTransaction, size=279).
//
// This guards the wire format end-to-end: decode envelope -> proto.Unmarshal
// must yield the documented fields.
const liveXBridgePacket = "fdb4016894ff47163a031d3ac8bfce10dfa3fbe290a48a4d27edd3985606003700000003000000faa8566a780100001701000002c6d68e9a98bf4bc54ee9fa11429bde598d7aae4cb2b94fe99b534addcd31ceb7d265f986f41bed10f44a5d1cabb55481dcfb23a69eebc44118ec12c2f51a855b0a05905ecb036dc013993e447a3572d634fbd13c8aaab697668647cccd5c391e0000000000000000000000001e380c064ef996a30c44913c779be71b7121e6e9e5c28f2a06af3add363e1e91d8cf2f4e12873469a75d71c74778f3935c2d53b2444f474500000000115e1700000000001a11b75482580340dc4cc6bd349553e7767e1f94424c4f434b00000021d77501000000001c7238c59856060030bdbd7fe31bba634ebe9a567d79de601047d7f2cfbf81b43760d7f507e8c58300000000000000000000010000001c569806f0bd7a450eb49fbf6dcf5a0f3c3182a74978701f0204d1e948da798e02000000d8cf2f4e12873469a75d71c74778f3935c2d53b22076244bfe101207df00bb14ea647ea319599b0690014e6eb909cb6203190a5e2e2d82e48083df1e395c95f989741227452ebdcb0f1641d8ce9594031a69e1472b"

func TestDecodeLiveXBridgePacket(t *testing.T) {
	raw, err := hex.DecodeString(liveXBridgePacket)
	if err != nil {
		t.Fatalf("decode test vector: %v", err)
	}
	pkt, err := DecodeXBridgePayload(raw)
	if err != nil {
		t.Fatalf("DecodeXBridgePayload: %v", err)
	}
	// 439-byte P2P payload: varint(436) + 28-byte envelope + 408-byte packet.
	if len(pkt) != 408 {
		t.Fatalf("packet len = %d, want 408", len(pkt))
	}

	p, err := proto.Unmarshal(pkt)
	if err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	if p.Version != proto.ProtocolVersion {
		t.Errorf("version = %d, want %d", p.Version, proto.ProtocolVersion)
	}
	if p.Command != proto.XbcTransaction {
		t.Errorf("command = %d (%s), want %d (xbcTransaction)", p.Command, p.Command, proto.XbcTransaction)
	}
	if p.Size != 279 {
		t.Errorf("size = %d, want 279", p.Size)
	}
	if len(p.Body) != 279 {
		t.Errorf("body len = %d, want 279", len(p.Body))
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	// A synthetic packet (129-byte header + 100-byte body) should survive an
	// encode/decode through the transport envelope unchanged.
	pkt := make([]byte, proto.HeaderSize+100)
	pkt[0] = 55 // version
	copy(pkt[proto.PubkeyOffset:proto.PubkeyOffset+33], []byte{0x02})
	copy(pkt[proto.SigOffset:proto.SigOffset+64], []byte{0x03})

	enveloped := encodeXBridgePayload(pkt)
	// The enveloped payload must start with a CompactSize varint whose value is
	// 28 + len(pkt) = 257 (so the 0xfd 2-byte form). Decode it back to confirm.
	n, off, err := readVarInt(enveloped, 0)
	if err != nil {
		t.Fatalf("readVarInt: %v", err)
	}
	if n != 28+len(pkt) {
		t.Fatalf("varint = %d, want %d", n, 28+len(pkt))
	}
	// Broadcast dest addr (immediately after the varint) must be 20 zero bytes.
	destStart := off
	for i := 0; i < 20; i++ {
		if enveloped[destStart+i] != 0 {
			t.Errorf("envelope dest addr byte %d not zero", i)
			break
		}
	}
	// The packet must survive an encode/decode.
	got, err := DecodeXBridgePayload(enveloped)
	if err != nil {
		t.Fatalf("DecodeXBridgePayload: %v", err)
	}
	if string(got) != string(pkt) {
		t.Errorf("round-trip mismatch: got %d bytes, want %d", len(got), len(pkt))
	}
}

func TestEnvelopeLengthMismatch(t *testing.T) {
	// Payload shorter than the declared varint length must error.
	bad := append([]byte{0x05}, make([]byte, 3)...) // claims 5 bytes, only 3
	if _, err := DecodeXBridgePayload(bad); err == nil {
		t.Fatal("expected length-mismatch error")
	}
}

func TestEnvelopeTimestampUnit(t *testing.T) {
	// The 8-byte transport timestamp MUST be in MICROSECONDS to match C++
	// (timeToInt = total_microseconds(), xutil.cpp:280) — it is part of the
	// signed body. Microsecond scale for a 21st-century date is >= 1e15
	// (2026-ish ≈ 1.7e15); millisecond scale would be ≈ 1.7e12. Asserting the
	// microsecond scale rules out the prior millisecond regression.
	pkt := make([]byte, proto.HeaderSize) // any packet body works for this check
	env := encodeXBridgePayload(pkt)

	n, off, err := readVarInt(env, 0)
	if err != nil {
		t.Fatalf("readVarInt: %v", err)
	}
	if n < xbridgeEnvelopeSize {
		t.Fatalf("envelope len = %d, want >= %d", n, xbridgeEnvelopeSize)
	}
	// Timestamp immediately follows the 20-byte broadcast dest addr.
	tsOff := off + xbridgeAddrSize
	ts := binary.LittleEndian.Uint64(env[tsOff : tsOff+xbridgeTimestampSize])

	if ts < 1e15 {
		t.Fatalf("timestamp = %d, want microsecond scale (>= 1e15); got millisecond-scale value", ts)
	}
}
