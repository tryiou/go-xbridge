package api

// Handshake-repair tests: HoldApply retransmission, silent-hub exclusion,
// and forensic packet-hex logging.
//
// After two mainnet S2 stalls at Hold (both sides sent HoldApply, the hub
// never assembled Init), the client must not trust a single fire-and-forget
// send: resend HoldApply until Init arrives, avoid provably-mute hubs on the
// next make, and log the exact bytes sent so the next stall is byte-provable.
// All use in-memory fixtures — no live hub, no network.

import (
	"encoding/hex"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// resendFixture builds a maker session parked in csHoldApplied with a pinned
// hub, a capture conn, and a registry containing exactly hubA + hubB.
func resendFixture(t *testing.T) (*Node, *captureXConn, string, [32]byte, [33]byte, [33]byte, []byte) {
	t.Helper()
	alignInitCoins(t)
	hubAPriv, hubA := newKey(t)
	_, hubB := newKey(t)
	var hubArrA, hubArrB [33]byte
	copy(hubArrA[:], hubA)
	copy(hubArrB[:], hubB)
	reg := servicenode.NewRegistry()
	for _, hk := range [][33]byte{hubArrA, hubArrB} {
		reg.AddPing(servicenode.ServiceNode{
			PubKey: hk, Tier: servicenode.TierSPV,
			Services:       []string{"BTC", "LTC"},
			XBridgeVersion: proto.ProtocolVersion,
		})
	}
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32)},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc, "LTC": btc})
	n.snReg = reg
	cc := &captureXConn{}
	n.conn = cc

	var id [32]byte
	oid := hash20("hold-resend-order")
	copy(id[:], oid[:])
	mPriv, mPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 10e6, ToAmount: 8e6, Mine: true, Status: "hold",
		SNodePubkey: hex.EncodeToString(hubA), HubAddress: coins.KeyID(hubA)}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{btc.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m-src"), TakerAddress: addrFor(48, "m-dst")}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(id[:])]
	s.state = csHoldApplied
	return n, cc, hexEncode(id[:]), id, hubArrA, hubArrB, hubAPriv
}

// TestPacketHexReplays pins the forensic guarantee: the logged hex of a sent
// handshake packet must decode and Unmarshal back to the identical packet, so
// a future stall can be replayed byte-for-byte against the hub's intake.
func TestPacketHexReplays(t *testing.T) {
	mPriv, _ := newKey(t)
	var hub, src [20]byte
	hub = hash20("hub")
	src = hash20("src")
	var id [32]byte
	oid := hash20("order")
	copy(id[:], oid[:])
	body := (&proto.HoldApplyBody{HubAddress: hub, ClientAddress: src, ID: id}).Marshal()
	if len(body) != 72 {
		t.Fatalf("HoldApply body = %d bytes, want 72", len(body))
	}
	pkt := proto.NewPacket(proto.XbcTransactionHoldApply, body)
	if err := crypto.NewBtcSigner().Sign(pkt, mPriv); err != nil {
		t.Fatal(err)
	}
	h := packetHex(pkt)
	raw, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("packetHex not hex-decodable: %v", err)
	}
	back, err := proto.Unmarshal(raw)
	if err != nil {
		t.Fatalf("logged hex does not Unmarshal: %v", err)
	}
	if back.Command != pkt.Command || string(back.Body) != string(pkt.Body) ||
		back.Pubkey != pkt.Pubkey || back.Signature != pkt.Signature {
		t.Fatal("replayed packet differs from the sent packet")
	}
	wantLen := 2 * (int(proto.HeaderSize) + 72)
	if len(h) != wantLen {
		t.Fatalf("packetHex len = %d, want %d", len(h), wantLen)
	}
	if h[len(h)-144:] != hex.EncodeToString(body) {
		t.Fatal("packetHex does not end with the exact body bytes")
	}
}

// TestResendHoldApplyAfterSilence pins Fix 1: a session parked in
// csHoldApplied whose last HoldApply predates the resend interval gets
// exactly one fresh HoldApply to the pinned hub, and the stamp advances.
func TestResendHoldApplyAfterSilence(t *testing.T) {
	n, cc, _, id, _, _, _ := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	now := NowMicro()
	s.holdApplySentAt = now - uint64(holdApplyResendMicro) - 1000

	n.resendHoldApplies(now)

	pkts := cc.snapshot()
	if len(pkts) != 1 {
		t.Fatalf("resent %d packets, want exactly 1", len(pkts))
	}
	if pkts[0].Command != proto.XbcTransactionHoldApply {
		t.Fatalf("command = %v, want HoldApply", pkts[0].Command)
	}
	if dests := cc.snapshotDests(); len(dests) != 1 || dests[0] != s.hub {
		t.Fatalf("resend dest = %x, want pinned hub %x", dests[0], s.hub)
	}
	if ok, err := crypto.NewBtcSigner().Verify(pkts[0]); err != nil || !ok {
		t.Fatalf("resent packet does not verify (ok=%v err=%v)", ok, err)
	}
	if s.holdApplySentAt != now {
		t.Fatalf("holdApplySentAt = %d, want restamp to %d", s.holdApplySentAt, now)
	}
}

// TestResendHoldApplySkips pins the three non-resend cases: a fresh send
// (interval not elapsed), a session past Hold (Init arrived), and a session
// that never sent (the first-send path owns it, not the resender).
func TestResendHoldApplySkips(t *testing.T) {
	n, cc, _, id, _, _, _ := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	now := NowMicro()

	s.holdApplySentAt = now // fresh: interval not elapsed
	n.resendHoldApplies(now)
	if got := len(cc.snapshot()); got != 0 {
		t.Fatalf("fresh stamp resent %d packets, want 0", got)
	}

	s.holdApplySentAt = now - uint64(holdApplyResendMicro) - 1000
	s.state = csInitialized // Init arrived: handshake moved on
	n.resendHoldApplies(now)
	if got := len(cc.snapshot()); got != 0 {
		t.Fatalf("advanced state resent %d packets, want 0", got)
	}

	s.state = csHoldApplied
	s.holdApplySentAt = 0 // never sent: first-send path owns it
	n.resendHoldApplies(now)
	if got := len(cc.snapshot()); got != 0 {
		t.Fatalf("zero stamp resent %d packets, want 0", got)
	}
}

// TestProcessSwapStampsHoldApplySentAt pins the stamp the resender depends
// on: the first HoldApply sent through processSwap records its send time.
func TestProcessSwapStampsHoldApplySentAt(t *testing.T) {
	n, cc, _, id, _, _, hubAPriv := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	s.state = csMaker
	if s.holdApplySentAt != 0 {
		t.Fatalf("initial holdApplySentAt = %d, want 0", s.holdApplySentAt)
	}
	hub := s.hub
	b := &proto.HoldBody{HubAddress: hub, ID: id, FromAmount: 8e6, ToAmount: 10e6}
	pkt := proto.NewPacket(proto.XbcTransactionHold, b.Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, hubAPriv); err != nil {
		t.Fatal(err)
	}
	n.processSwap(pkt, id, hub, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		return s.OnHold(b)
	})
	if got := len(cc.snapshot()); got != 1 {
		t.Fatalf("processSwap sent %d packets, want 1", got)
	}
	if s.holdApplySentAt == 0 {
		t.Fatal("holdApplySentAt not stamped after HoldApply send")
	}
}

// TestSilentHubRecordExcludeExpire pins Fix 2: a hold-parked session silent
// past the hub-silence threshold records its hub; the hub is excluded from
// picks until the TTL lapses, then becomes eligible again.
func TestSilentHubRecordExcludeExpire(t *testing.T) {
	n, _, _, id, hubA, hubB, _ := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	if hex.EncodeToString(s.hubKey[:]) != hex.EncodeToString(hubA[:]) {
		t.Fatal("fixture hub key mismatch")
	}
	now := NowMicro()
	s.holdApplySentAt = now - uint64(hubSilenceMicro) - 1000

	n.recordSilentHubs(now)

	excl := n.hubExclusions(now)
	if len(excl) != 1 || excl[0] != hubA {
		t.Fatalf("exclusions = %x, want [hubA]", excl)
	}
	if got, ok := n.pickHub([]string{"BTC", "LTC"}, now); !ok || got != hubB {
		t.Fatalf("pickHub = %x,%v with hubA silent, want hubB,true", got, ok)
	}
	// After the TTL lapses the hub is eligible again (and pruned).
	later := now + uint64(silentHubTTLMicro) + 1000
	if excl := n.hubExclusions(later); len(excl) != 0 {
		t.Fatalf("exclusions after TTL = %x, want empty", excl)
	}
	if got, ok := n.pickHub([]string{"BTC", "LTC"}, later); !ok {
		t.Fatalf("pickHub after TTL ok=%v, want true (got %x)", ok, got)
	}
}

// TestSilentHubSkipsFreshSessions pins that recordSilentHubs ignores
// sessions that were recently heard from (Init may still be in flight) and
// sessions with no pinned hub key.
func TestSilentHubSkipsFreshSessions(t *testing.T) {
	n, _, _, id, _, _, _ := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	now := NowMicro()
	s.holdApplySentAt = now // fresh

	n.recordSilentHubs(now)
	if excl := n.hubExclusions(now); len(excl) != 0 {
		t.Fatalf("fresh session recorded exclusions %x, want none", excl)
	}
}

// TestHandshakeRepairClockOrdering pins the composition the unit tests
// cannot: resends must fire long before a silence is recorded, which must
// precede the watchdog cancel. A resend interval past the watchdog (as
// originally written: 60 min vs the 30 min stall threshold) means the
// resender can never execute live — only this ordering test catches it.
func TestHandshakeRepairClockOrdering(t *testing.T) {
	if holdApplyResendMicro >= hubSilenceMicro || hubSilenceMicro >= sessionStallMicro {
		t.Fatalf("clock ordering broken: resend %d, silence %d, watchdog %d (want resend < silence < watchdog)",
			holdApplyResendMicro, hubSilenceMicro, sessionStallMicro)
	}
}

// TestHoldApplyStampPersistRoundTrip pins the resend clock across restarts:
// persistFromSession carries the stamp, restoreSwap rebuilds it, and a
// pre-upgrade zero stamp on a still-parked session is re-stamped (instead
// of skipping the resender and silence recorder forever).
func TestHoldApplyStampPersistRoundTrip(t *testing.T) {
	n, _, _, id, _, _, _ := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	s.state = csHoldApplied
	sentAt := NowMicro() - 5000000
	s.holdApplySentAt = sentAt
	o := n.store.Get(hexEncode(id[:]))
	if o == nil {
		t.Fatal("fixture order missing from store")
	}

	ps := persistFromSession(s, o)
	if ps.HoldApplySentAt != sentAt {
		t.Fatalf("persisted stamp = %d, want %d", ps.HoldApplySentAt, sentAt)
	}
	n2 := newTestNode(t, alignConfs(), map[string]wallet.Connector{})
	n2.restoreSwap(ps)
	rs := n2.sessions[hexEncode(id[:])]
	if rs == nil {
		t.Fatal("restoreSwap did not rebuild the session")
	}
	if rs.holdApplySentAt != sentAt {
		t.Fatalf("restored stamp = %d, want %d", rs.holdApplySentAt, sentAt)
	}

	// Legacy record (no stamp field): a parked session resumes resending
	// instead of idling on zero.
	ps.HoldApplySentAt = 0
	before := NowMicro()
	n3 := newTestNode(t, alignConfs(), map[string]wallet.Connector{})
	n3.restoreSwap(ps)
	after := NowMicro()
	ls := n3.sessions[hexEncode(id[:])]
	if ls == nil {
		t.Fatal("restoreSwap did not rebuild the legacy session")
	}
	if ls.holdApplySentAt < before || ls.holdApplySentAt > after {
		t.Fatalf("legacy stamp = %d, want re-stamp within [%d,%d]", ls.holdApplySentAt, before, after)
	}
}

// TestSilentHubIgnoresPreRestartStamps pins that silence predating process
// start (restored durable stamps across a restart) is downtime, not hub
// silence: only post-start parked time counts toward exclusion.
func TestSilentHubIgnoresPreRestartStamps(t *testing.T) {
	n, _, _, id, hubA, _, _ := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	now := NowMicro()
	s.holdApplySentAt = now - uint64(hubSilenceMicro) - 1000
	n.startedAtMicro = now // stamp predates this process

	n.recordSilentHubs(now)
	if excl := n.hubExclusions(now); len(excl) != 0 {
		t.Fatalf("pre-restart stamp recorded exclusions %x, want none", excl)
	}
	_ = hubA
}

// TestHoldApplyEchoesInboundHubRoute pins the C++ hub-route contract
// (xbridgesession.cpp:1289-1294,1534-1542,264-280): the hub stamps Hold with
// its per-session random m_myid and drops any reply whose body[0:20] is not
// that id (checkPacketAddress, silent). Four mainnet B-maker swaps stalled at
// Hold because the maker answered with the make-time KeyID instead of the
// inbound route. Both roles must echo the inbound Hold's HubAddress in the
// HoldApply body AND envelope, and adopt it as the live route (so resends
// follow the hub's current session, not the make-time pick).
func TestHoldApplyEchoesInboundHubRoute(t *testing.T) {
	n, cc, _, id, hubA, _, hubAPriv := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	s.state = csMaker
	keyID := coins.KeyID(hubA[:])
	if s.hub != keyID {
		t.Fatalf("fixture hub = %x, want make-time KeyID %x", s.hub, keyID)
	}
	// Inbound Hold stamped with a live hub route R != KeyID (C++ m_myid is
	// 20 random bytes per session, never a KeyID).
	route := hash20("live-hub-route")
	if route == keyID {
		t.Fatal("test route collides with KeyID fixture")
	}
	b := &proto.HoldBody{HubAddress: route, ID: id, FromAmount: 8e6, ToAmount: 10e6}
	pkt := proto.NewPacket(proto.XbcTransactionHold, b.Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, hubAPriv); err != nil {
		t.Fatal(err)
	}
	n.processSwap(pkt, id, route, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		return s.OnHold(b)
	})
	if got := len(cc.snapshot()); got != 1 {
		t.Fatalf("processSwap sent %d packets, want 1", got)
	}
	if dests := cc.snapshotDests(); len(dests) != 1 || dests[0] != route {
		t.Fatalf("HoldApply envelope dest = %x, want inbound route %x", dests[0], route)
	}
	body := cc.snapshot()[0].Body
	if len(body) < 20 || string(body[:20]) != string(route[:]) {
		t.Fatalf("HoldApply body[0:20] = %x, want inbound route %x", body[:20], route)
	}
	if s.hub != route {
		t.Fatalf("session hub = %x, want adopted inbound route %x", s.hub, route)
	}
}

// TestHoldApplyEchoesInboundHubRouteTaker pins the same echo contract for
// the taker role: a taker whose pinned hub differs from the live Hold route
// must still answer (and adopt) the inbound route.
func TestHoldApplyEchoesInboundHubRouteTaker(t *testing.T) {
	n, cc, _, _, hubA, _, hubAPriv := resendFixture(t)
	var tid [32]byte
	tid20 := hash20("taker-echo-order")
	copy(tid[:], tid20[:])
	tPriv, tPub := newKey(t)
	to := &Order{ID: tid, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 10e6, ToAmount: 8e6,
		SNodePubkey: hex.EncodeToString(hubA[:]), HubAddress: coins.KeyID(hubA[:])}
	n.newTakerSession(withUsedCoins(t, n, to, []wallet.Utxo{}),
		TakeOrderParams{FromAddress: addrFor(48, "t-src"), ToAddress: addrFor(0, "t-dst")},
		arr32(tPriv), toArr33(tPub))
	s := n.sessions[hexEncode(tid[:])]
	if s.hub != coins.KeyID(hubA[:]) {
		t.Fatalf("taker pinned hub = %x, want KeyID", s.hub)
	}
	route := hash20("live-hub-route-taker")
	b := &proto.HoldBody{HubAddress: route, ID: tid, FromAmount: 8e6, ToAmount: 10e6}
	pkt := proto.NewPacket(proto.XbcTransactionHold, b.Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, hubAPriv); err != nil {
		t.Fatal(err)
	}
	n.processSwap(pkt, tid, route, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		return s.OnHold(b)
	})
	if got := len(cc.snapshot()); got != 1 {
		t.Fatalf("processSwap sent %d packets, want 1", got)
	}
	if dests := cc.snapshotDests(); len(dests) != 1 || dests[0] != route {
		t.Fatalf("taker HoldApply envelope dest = %x, want inbound route %x", dests[0], route)
	}
	body := cc.snapshot()[0].Body
	if len(body) < 20 || string(body[:20]) != string(route[:]) {
		t.Fatalf("taker HoldApply body[0:20] = %x, want inbound route %x", body[:20], route)
	}
	if s.hub != route {
		t.Fatalf("taker session hub = %x, want adopted inbound route %x", s.hub, route)
	}
}

// TestSilentHubExpiryFixedAtFirstSight pins set-if-absent TTL: continued
// silence does not slide the exclusion window forward forever.
func TestSilentHubExpiryFixedAtFirstSight(t *testing.T) {
	n, _, _, id, hubA, _, _ := resendFixture(t)
	s := n.sessions[hexEncode(id[:])]
	now := NowMicro()
	s.holdApplySentAt = now - uint64(hubSilenceMicro) - 1000

	n.recordSilentHubs(now)
	first := n.silentHubs[hubA]
	if first != now+uint64(silentHubTTLMicro) {
		t.Fatalf("expiry = %d, want %d", first, now+uint64(silentHubTTLMicro))
	}
	n.recordSilentHubs(now + uint64(hubSilenceMicro))
	if kept := n.silentHubs[hubA]; kept != first {
		t.Fatalf("expiry slid to %d, want fixed %d", kept, first)
	}
}
