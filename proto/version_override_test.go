package proto

import (
	"testing"

	"go-xbridge/version"
)

// TestSetXBridgeProtocolVersionOverride locks in the -xbridgeversion override
// path: SetXBridgeProtocolVersion stamps the effective wire version used by
// NewPacket and enforced by the inbound decode gate (Unmarshal). The default
// stays version.DefaultXBridgeProtocolVersion; the override is a
// daemon-level, pre-start knob for isolated testing against a hub fleet on a
// different version. Tests restore the previous value (no t.Parallel anywhere
// in this repo — the global is safe to mutate under save/restore).
func TestSetXBridgeProtocolVersionOverride(t *testing.T) {
	orig := version.XBridgeProtocolVersion
	t.Cleanup(func() { version.SetXBridgeProtocolVersion(orig) })

	override := orig + 1
	version.SetXBridgeProtocolVersion(override)
	if version.XBridgeProtocolVersion != override {
		t.Fatalf("XBridgeProtocolVersion = %d, want %d after SetXBridgeProtocolVersion", version.XBridgeProtocolVersion, override)
	}

	// Outbound packets carry the override.
	pkt := NewPacket(XbcTransactionCancel, make([]byte, 36))
	if pkt.Version != override {
		t.Fatalf("NewPacket version = %d, want %d", pkt.Version, override)
	}
	// The inbound gate accepts the override…
	back, err := Unmarshal(pkt.Marshal())
	if err != nil {
		t.Fatalf("Unmarshal override-version packet: %v", err)
	}
	if back.Version != override {
		t.Fatalf("decoded version = %d, want %d", back.Version, override)
	}
	// …and rejects the old default, exactly as it rejects any foreign version
	// (Session::checkXBridgePacketVersion parity).
	foreign := &Packet{
		Version:   version.DefaultXBridgeProtocolVersion,
		Command:   XbcTransactionCancel,
		Timestamp: 1,
		OldSize:   36 + headerDifference,
		Size:      36,
		Body:      make([]byte, 36),
	}
	if _, err := Unmarshal(foreign.Marshal()); err == nil {
		t.Fatal("Unmarshal old-default packet must fail while the override is active")
	}
}

// TestSetXBridgeProtocolVersionRejectsZero keeps SetXBridgeProtocolVersion
// from ever making the gate reject every packet (a zero effective version
// would brick the daemon's P2P I/O silently).
func TestSetXBridgeProtocolVersionRejectsZero(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("SetXBridgeProtocolVersion(0) must panic")
		}
	}()
	version.SetXBridgeProtocolVersion(0)
}
