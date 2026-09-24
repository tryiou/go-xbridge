package api

import (
	"strings"
	"testing"
	"time"

	"go-xbridge/crypto"
	"go-xbridge/proto"
)

// TestHandshakeInflightDropsCollapse pins the retransmit diet: while a
// deposit/claim task is in flight, every hub retransmit hits the same drop,
// so the per-packet line must collapse to first-sighting plus summary
// instead of one main-log line per retransmit.
func TestHandshakeInflightDropsCollapse(t *testing.T) {
	genPath, _ := installSplitLogs(t)

	n, _, _, id, _, _, hubAPriv := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	// Test-local ID (not the fixture's shared hash20("hold-resend-order")):
	// handshakeInflightDedup is a package-global 60 s bucket keyed on order
	// id, so a constant id would collide with any other test hitting the
	// await path in the window.
	var inID [32]byte
	inID[0] = 0xC7
	delete(n.sessions, hexEncode(id[:]))
	s.id = inID
	n.sessions[hexEncode(inID[:])] = s

	s.await = true // a deposit/claim task is "in flight"
	hub := s.hub

	for i := 0; i < 3; i++ {
		b := &proto.HoldBody{HubAddress: hub, ID: inID, FromAmount: 8e6, ToAmount: 10e6}
		pkt := proto.NewPacket(proto.XbcTransactionHold, b.Marshal())
		if err := crypto.NewBtcSigner().Sign(pkt, hubAPriv); err != nil {
			t.Fatal(err)
		}
		n.processSwap(pkt, inID, hub, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return 0, nil, nil
		})
	}

	// Settle: the drops are synchronous here, but the file write may lag a
	// beat behind the call returns.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(readLogFile(t, genPath), "handshake task in flight") {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for in-flight drop line")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	if got := strings.Count(readLogFile(t, genPath), "handshake task in flight"); got != 1 {
		t.Errorf("general log has %d in-flight drop lines, want 1 (first-sighting only)", got)
	}
	// Summary path: flushing emits one aggregate for the order key, staying
	// in the main log to join its first-sighting line.
	handshakeInflightDedup.Flush()
	if got := strings.Count(readLogFile(t, genPath), "handshake retransmits suppressed"); got != 1 {
		t.Errorf("general log summary lines = %d, want 1", got)
	}
}
