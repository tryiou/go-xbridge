package api

import (
	"encoding/hex"
	"fmt"
	"sort"

	xlog "go-xbridge/log"

	"go-xbridge/coins"
	"go-xbridge/proto"
)

// Handshake repair: HoldApply retransmission, silent-hub exclusion, and
// forensic packet-hex logging.
//
// Two mainnet S2 swaps stalled at Hold with both sides' HoldApply sent and
// the hub never assembling Init. The client trusted a single fire-and-forget
// send and had no recovery: no resend, no hub rotation on silence, and no
// bytes logged to audit afterwards. This file closes all three gaps without
// touching the wire contract or the C++-mirrored handshake state machine:
//   - resendHoldApplies re-sends HoldApply while parked in csHoldApplied
//     (the hub counter is flag-idempotent, so repeats are side-effect free);
//   - recordSilentHubs + pickHub steer future makes away from provably-mute
//     hubs via the existing Registry.Pick exclude parameter;
//   - packetHex preserves the exact sent bytes for post-stall forensics.
const (
	// holdApplyResendMicro is the minimum age of the last HoldApply send
	// before the engine tick resends it (the tick itself runs every
	// refundCheckInterval = 60 s, so a parked session resends at most once
	// per tick). C++ has no resend; this is a deliberate safe deviation —
	// the hub-side counter only latches per-side flags, so a duplicate
	// HoldApply can never double-apply or corrupt the join. It must stay
	// well under hubSilenceMicro (silence is only meaningful after resends
	// had a chance) and sessionStallMicro (the watchdog cancels first) —
	// pinned by TestHandshakeRepairClockOrdering.
	holdApplyResendMicro = 60 * 1000000
	// hubSilenceMicro is how long a session may sit in csHoldApplied with
	// HoldApplies sent and no Init before its hub is recorded as silent.
	// Healthy hubs answer in seconds (both green S1 runs assembled Init in
	// under a minute); ten silent minutes is sick, not slow.
	hubSilenceMicro = 10 * 60 * 1000000
	// silentHubTTLMicro bounds a silence exclusion (same scale as the
	// Mine-order TTL): a hub that recovers becomes eligible again instead
	// of being banished for the process lifetime.
	silentHubTTLMicro = 24 * 60 * 60 * 1000000
)

// packetHex renders the exact wire bytes of a signed outbound handshake
// packet. The returned hex decodes and Unmarshals back to the identical
// packet (pinned by TestPacketHexReplays), so a future stall can be replayed
// byte-for-byte against the hub's intake instead of debated from log prose.
func packetHex(pkt *proto.Packet) string {
	return hex.EncodeToString(pkt.Marshal())
}

// holdApplySource resolves our 20-byte source address for the HoldApply
// body, shared by the first send (OnHold) and the resender so both emit
// identical bytes for a session.
func holdApplySource(s *SwapSession) ([20]byte, error) {
	var zero [20]byte
	c, ok := coins.Get(s.srcCur)
	if !ok {
		return zero, fmt.Errorf("api: unknown coin %s", s.srcCur)
	}
	a, err := c.DecodeAddress(s.ourSourceAddr)
	if err != nil {
		return zero, err
	}
	src := [20]byte{}
	copy(src[:], a.Hash)
	return src, nil
}

// resendHoldApplies re-sends HoldApply for sessions parked in csHoldApplied
// whose last send predates the resend interval. Engine-tick owned (called
// from the 60 s tick like the other sweeps); sessions are engine-confined
// there. The 30-minute hub-silence watchdog still owns cancellation —
// resends never touch lastProgress, so a mute hub is retried AND eventually
// cancelled instead of either stalling silently or retrying forever.
func (n *Node) resendHoldApplies(now uint64) {
	for _, s := range n.sessions {
		if s.state != csHoldApplied || s.holdApplySentAt == 0 {
			continue
		}
		// await is set only by post-Init deposit/claim task stage-1s, so a
		// parked pre-Init session never holds it; if that invariant ever
		// breaks, skipping the resend is the safe direction (never race a
		// deposit broadcast), and the Debug line marks the spot.
		if s.await {
			xlog.Debug("holdapply resend skipped: task in flight on parked session",
				"order", hexEncode(s.id[:]))
			continue
		}
		if now <= s.holdApplySentAt || now-s.holdApplySentAt < uint64(holdApplyResendMicro) {
			continue
		}
		src, err := holdApplySource(s)
		if err != nil {
			xlog.Error("holdapply resend: source address undecodable, retrying next tick",
				"order", hexEncode(s.id[:]), "err", err)
			continue
		}
		body := &proto.HoldApplyBody{HubAddress: s.hub, ClientAddress: src, ID: s.id}
		if err := n.send(s.hub, proto.XbcTransactionHoldApply, body, s.privKey[:]); err != nil {
			xlog.Error("holdapply resend failed, retrying next tick",
				"order", hexEncode(s.id[:]), "err", err)
			continue
		}
		s.holdApplySentAt = now
		xlog.Info("holdapply resent, awaiting Init", "order", hexEncode(s.id[:]))
	}
}

// recordSilentHubs records hubs whose sessions sit in csHoldApplied with
// HoldApplies sent and no Init past the silence threshold. The exclusion
// only steers future makes (pickHub) — the parked session itself keeps
// resending until the watchdog cancels it, so a slow-but-alive hub is never
// abandoned early.
func (n *Node) recordSilentHubs(now uint64) {
	for _, s := range n.sessions {
		if s.state != csHoldApplied || s.holdApplySentAt == 0 || s.hubKey == ([33]byte{}) {
			continue
		}
		if now <= s.holdApplySentAt || now-s.holdApplySentAt < uint64(hubSilenceMicro) {
			continue
		}
		// Pre-restart parked time is downtime, not hub silence: only
		// silence observed since process start counts (the resender keeps
		// the durable stamp and recovers promptly regardless).
		if n.startedAtMicro != 0 && s.holdApplySentAt < n.startedAtMicro {
			continue
		}
		n.silentHubsMu.Lock()
		if n.silentHubs == nil {
			n.silentHubs = map[[33]byte]uint64{}
		}
		// First observation wins: the TTL runs from when the silence was
		// established, not refreshed every tick, so a parked session the
		// watchdog somehow skips cannot extend the exclusion forever.
		if _, dup := n.silentHubs[s.hubKey]; !dup {
			xlog.Warn("hub silent through HoldApplies, excluded from future makes",
				"order", hexEncode(s.id[:]), "hub", hexEncode(s.hubKey[:]))
			n.silentHubs[s.hubKey] = now + uint64(silentHubTTLMicro)
		}
		n.silentHubsMu.Unlock()
	}
}

// hubExclusions returns the currently-excluded silent hub keys, pruning
// lapsed entries. Sorted for deterministic picks and logs.
func (n *Node) hubExclusions(now uint64) [][33]byte {
	n.silentHubsMu.Lock()
	defer n.silentHubsMu.Unlock()
	var out [][33]byte
	for k, until := range n.silentHubs {
		if now >= until {
			delete(n.silentHubs, k)
			continue
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		for b := 0; b < 33; b++ {
			if out[i][b] != out[j][b] {
				return out[i][b] < out[j][b]
			}
		}
		return false
	})
	return out
}

// pickHub chooses the servicenode for a new make, excluding silent hubs.
// C++ has no exclusion (findNodeWithService picks blind); this is a
// deliberate safe deviation — a hub that provably swallowed a previous
// handshake is the worst candidate for the next one. If every known hub is
// excluded, it falls back to an unexcluded pick (a possibly-slow trade beats
// a refused one) and says so loudly.
func (n *Node) pickHub(currencies []string, now uint64) ([33]byte, bool) {
	if n.snReg == nil {
		return [33]byte{}, false
	}
	if excl := n.hubExclusions(now); len(excl) > 0 {
		if key, ok := n.snReg.Pick(currencies, excl...); ok {
			return key, true
		}
		xlog.Warn("all hubs silent-excluded, falling back to unexcluded pick", "currencies", currencies)
	}
	return n.snReg.Pick(currencies)
}
