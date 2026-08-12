package api

import (
	"bytes"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
)

// TestOnConfirmAMissingConnectorIsError verifies the swap handler returns an
// error (not a nil-interface panic) when the destination currency's wallet
// connector is absent — the exact crash the audit flagged at api/swap.go:225.
// Before the fix, s.n.cfg().Connectors[cur] returned a nil interface and the
// SendRawTransaction call panicked, killing the feed goroutine (and process).
func TestOnConfirmAMissingConnectorIsError(t *testing.T) {
	node := newWalletTestCtx().Node
	// A valid per-trade M keypair so redeemCounterparty can sign the payTx and
	// reach the connector lookup.
	mPriv := bytes.Repeat([]byte{0x01}, 32)
	mPub, err := crypto.CompressedPubKey(mPriv)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a missing connector for the destination currency.
	delete(node.cfg().Connectors, "BTC")

	s := &SwapSession{
		n:                node,
		isMaker:          true,
		id:               [32]byte{1},
		srcCur:           "BTC",
		dstCur:           "BTC",
		srcAmt:           100000000,
		dstAmt:           100000000,
		privKey:          arr32(mPriv),
		pubKey:           mPub,
		ourSourceAddr:    btcAddr,
		ourDestAddr:      btcAddr,
		theirPub:         [33]byte{0x02},
		secret:           [33]byte{0x02},
		secretHash:       [20]byte{0x03},
		theirDepositTxID: "0000000000000000000000000000000000000000000000000000000000000000",
		theirLockTime:    100,
		state:            csMaker,
	}
	cmd, body, err := s.OnConfirmA(&proto.ConfirmABody{
		BDepositTxID: s.theirDepositTxID,
		BLockTime:    100,
	})
	if err == nil {
		t.Fatal("expected error when destination connector is missing")
	}
	if cmd != 0 || body != nil {
		t.Fatalf("expected no response on error (cmd=%v body=%v)", cmd, body)
	}
}

// TestNodeConnectorMissing verifies the centralized connector lookup returns an
// error for an unconfigured ticker and succeeds for a configured one.
func TestNodeConnectorMissing(t *testing.T) {
	node := newWalletTestCtx().Node
	if _, e := node.connector("DOGE"); e == nil {
		t.Fatal("expected error for missing connector")
	}
	if _, e := node.connector("BTC"); e != nil {
		t.Fatalf("unexpected error for configured connector: %v", e)
	}
}

// TestDispatchSwapRecoversFromPanic ensures a panicking swap handler cannot
// crash the process: dispatchSwap must recover and return. The session is a
// pinned maker so the packet passes the hub-key auth gate and reaches the
// handler (which is what panics).
func TestDispatchSwapRecoversFromPanic(t *testing.T) {
	node := newWalletTestCtx().Node
	node.sessions = map[string]*SwapSession{}
	id := [32]byte{7}

	hubPriv := make([]byte, 32)
	hubPriv[31] = 7
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	node.sessions[hexEncode(id[:])] = &SwapSession{
		n:       node,
		id:      id,
		isMaker: true,
		hubKey:  hubPub,
		state:   csMaker,
	}

	pkt := proto.NewPacket(proto.XbcTransactionHold, (&proto.HoldBody{}).Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, hubPriv); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		node.dispatchSwap(pkt, id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			panic("boom")
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchSwap did not return after panic (process may have crashed)")
	}
}

// seedHub registers pub as a running, version-matching servicenode so the
// STRICT hubRegistered gate (an empty registry refuses, C++ getSn null) lets
// the honest path through — mirroring a live registry that has seen the hub's
// SNPING.
func seedHub(node *Node, pub [33]byte) {
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: pub, Tier: servicenode.TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion,
	})
	node.snReg = reg
}

// TestDispatchSwapDropsForgedFinished is the STATE-F78 regression test: a Finished
// packet NOT signed by the session's trusted hub key must be dropped before it
// reaches OnFinished, so it can never set csFinished and disable the refund
// watcher (swap.go:473). A Finished signed by the real hub still processes.
func TestDispatchSwapDropsForgedFinished(t *testing.T) {
	node := newWalletTestCtx().Node
	node.sessions = map[string]*SwapSession{}
	id := [32]byte{8}

	hubPriv := make([]byte, 32)
	hubPriv[31] = 8
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	seedHub(node, hubPub)
	attackerPriv := make([]byte, 32)
	attackerPriv[31] = 9

	s := &SwapSession{
		n:       node,
		id:      id,
		isMaker: true,
		hubKey:  hubPub,
		state:   csConfirmedB,
	}
	node.sessions[hexEncode(id[:])] = s

	signed := func(priv []byte) *proto.Packet {
		pkt := proto.NewPacket(proto.XbcTransactionFinished, (&proto.FinishedBody{ID: id}).Marshal())
		if err := crypto.NewBtcSigner().Sign(pkt, priv); err != nil {
			t.Fatal(err)
		}
		return pkt
	}

	// Forged Finished from the attacker's key: dropped, session state untouched.
	node.dispatchSwap(signed(attackerPriv), id, [20]byte{}, "Finished", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		return s.OnFinished(&proto.FinishedBody{ID: id})
	})
	if s.state == csFinished {
		t.Fatal("forged Finished set csFinished — refund watcher would be disabled")
	}

	// Honest Finished from the pinned hub key: still processed.
	node.dispatchSwap(signed(hubPriv), id, [20]byte{}, "Finished", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		return s.OnFinished(&proto.FinishedBody{ID: id})
	})
	if s.state != csFinished {
		t.Fatalf("honest Finished did not process: state=%s", s.state.String())
	}
}

// TestDispatchSwapMakerRejectsNonPinnedHub verifies the maker's hub pin is
// strict and immutable: the session is pinned at creation to the servicenode
// chosen at make time. A hub-signed packet dispatches; a packet signed by any
// OTHER key (even a registered-looking forger) is dropped and can never claim
// the hub role.
func TestDispatchSwapMakerRejectsNonPinnedHub(t *testing.T) {
	node := newWalletTestCtx().Node
	node.sessions = map[string]*SwapSession{}
	id := [32]byte{9}

	hubPriv := make([]byte, 32)
	hubPriv[31] = 10
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	seedHub(node, hubPub)
	forgerPriv := make([]byte, 32)
	forgerPriv[31] = 11

	s := &SwapSession{
		n:       node,
		id:      id,
		isMaker: true,
		hubKey:  hubPub,
		hub:     coins.KeyID(hubPub[:]),
		state:   csMaker,
	}
	node.sessions[hexEncode(id[:])] = s

	mk := func(priv []byte) *proto.Packet {
		pkt := proto.NewPacket(proto.XbcTransactionHold, (&proto.HoldBody{}).Marshal())
		if err := crypto.NewBtcSigner().Sign(pkt, priv); err != nil {
			t.Fatal(err)
		}
		return pkt
	}

	// The trusted hub's Hold dispatches.
	dispatched := false
	node.dispatchSwap(mk(hubPriv), id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		dispatched = true
		return 0, nil, nil
	})
	if !dispatched {
		t.Fatal("trusted hub's Hold was not dispatched")
	}
	if s.hubKey != hubPub {
		t.Fatal("pinned hub key was overwritten")
	}

	// A forger's Hold is dropped and does NOT repin.
	node.dispatchSwap(mk(forgerPriv), id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		t.Fatal("forger Hold reached handler")
		return 0, nil, nil
	})
	if s.hubKey != hubPub {
		t.Fatalf("forger replaced the pinned hub key: got %x", s.hubKey)
	}

	// A forged Finished from the forger is rejected (hubKey stays pinned).
	node.dispatchSwap(mk(forgerPriv), id, [20]byte{}, "Finished", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		t.Fatal("forged Finished after pin reached handler")
		return 0, nil, nil
	})
}

// TestDispatchSwapMakerUnpinnedDropsAll verifies there is NO trust-on-first-use
// fallback for the maker: a session with no pinned hub key (zero) drops every
// hub packet, including one signed by a key that would otherwise look plausible.
// The hub must be chosen and pinned at make time (C++ xtx->sPubKey), never
// learned from network packets.
func TestDispatchSwapMakerUnpinnedDropsAll(t *testing.T) {
	node := newWalletTestCtx().Node
	node.sessions = map[string]*SwapSession{}
	id := [32]byte{11}

	s := &SwapSession{n: node, id: id, isMaker: true, state: csMaker}
	node.sessions[hexEncode(id[:])] = s

	hubPriv := make([]byte, 32)
	hubPriv[31] = 13
	mk := func(priv []byte) *proto.Packet {
		pkt := proto.NewPacket(proto.XbcTransactionHold, (&proto.HoldBody{}).Marshal())
		if err := crypto.NewBtcSigner().Sign(pkt, priv); err != nil {
			t.Fatal(err)
		}
		return pkt
	}

	// Even a validly-signed Hold must not pin an unpinned maker.
	node.dispatchSwap(mk(hubPriv), id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		t.Fatal("unpinned maker packet reached handler")
		return 0, nil, nil
	})
	if s.hubKey != [33]byte{} {
		t.Fatal("unpinned maker session was pinned by a hub packet")
	}
}

// TestDispatchSwapTakerRejectsUnpinnedHub verifies the taker side never falls
// back to TOFU: a taker session whose order had no SNodePubkey (unpinned) drops
// every hub packet — an unpinned session cannot be authenticated, so nothing
// (including a forged Finished) may progress it.
func TestDispatchSwapTakerRejectsUnpinnedHub(t *testing.T) {
	node := newWalletTestCtx().Node
	node.sessions = map[string]*SwapSession{}
	id := [32]byte{10}

	s := &SwapSession{n: node, id: id, isMaker: false, state: csTaker}
	node.sessions[hexEncode(id[:])] = s

	anyPriv := make([]byte, 32)
	anyPriv[31] = 12
	pkt := proto.NewPacket(proto.XbcTransactionHold, (&proto.HoldBody{}).Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, anyPriv); err != nil {
		t.Fatal(err)
	}

	node.dispatchSwap(pkt, id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		t.Fatal("unpinned taker packet reached handler")
		return 0, nil, nil
	})
	if s.hubKey != [33]byte{} {
		t.Fatal("unpinned taker session was pinned by a hub packet")
	}
}
