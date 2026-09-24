package api

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/version"
	"go-xbridge/wallet"
)

// TestOnConfirmAMissingConnectorIsError verifies the swap handler returns an
// error (not a nil-interface panic) when the destination currency's wallet
// connector is absent — the nil-interface panic at api/swap.go:225.
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
// crash the process: processSwap must recover and return. The session is a
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
		node.processSwap(pkt, id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			panic("boom")
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processSwap did not return after panic (process may have crashed)")
	}
}

// seedHub registers pub as a running, version-matching servicenode so the
// STRICT hubRegistered gate (an empty registry refuses, C++ getSn null) lets
// the honest path through — mirroring a live registry that has seen the hub's
// SNPING.
func seedHub(node *Node, pub [33]byte) {
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: pub, Tier: servicenode.TierSPV, Services: []string{"BTC"}, XBridgeVersion: version.XBridgeProtocolVersion,
	})
	node.snReg = reg
}

// TestDispatchSwapDropsForgedFinished is the trusted-hub-key regression test: a Finished
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
	node.processSwap(signed(attackerPriv), id, [20]byte{}, "Finished", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		return s.OnFinished(&proto.FinishedBody{ID: id})
	})
	if s.state == csFinished {
		t.Fatal("forged Finished set csFinished — refund watcher would be disabled")
	}

	// Honest Finished from the pinned hub key: still processed.
	node.processSwap(signed(hubPriv), id, [20]byte{}, "Finished", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
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
	node.processSwap(mk(hubPriv), id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
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
	node.processSwap(mk(forgerPriv), id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		t.Fatal("forger Hold reached handler")
		return 0, nil, nil
	})
	if s.hubKey != hubPub {
		t.Fatalf("forger replaced the pinned hub key: got %x", s.hubKey)
	}

	// A forged Finished from the forger is rejected (hubKey stays pinned).
	node.processSwap(mk(forgerPriv), id, [20]byte{}, "Finished", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
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
	node.processSwap(mk(hubPriv), id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
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

	node.processSwap(pkt, id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		t.Fatal("unpinned taker packet reached handler")
		return 0, nil, nil
	})
	if s.hubKey != [33]byte{} {
		t.Fatal("unpinned taker session was pinned by a hub packet")
	}
}

// TestUnvalidatedDepositRefusedBothLegs is the composite acceptance: the HTLC
// composition is sound, and with the validated-deposit gate the taker AND the
// maker refuse an unvalidated counterparty deposit end-to-end — the hostile
// outcome (theft, not recoverable lockup) is killed on both legs.
func TestUnvalidatedDepositRefusedBothLegs(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	mkPriv, mkPub := newKey(t)
	tkPriv, tkPub := newKey(t)
	mkAddr := addrFor(0, "maker-btc-dest")
	ltcAddr := addrFor(48, "taker-ltc-source")
	mkBtc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8, ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(mkPub)))}, fundingPriv: mkPriv, fundingPub: mkPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{}}
	tkLtc := &fakeConnector{ticker: "LTC", funding: wallet.Utxo{TxID: strings.Repeat("bb", 32), Vout: 0, Amount: 5e8, ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(tkPub)))}, fundingPriv: tkPriv, fundingPub: tkPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000, rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	makerNode := newTestNode(t, confs, map[string]wallet.Connector{"BTC": mkBtc, "LTC": tkLtc})
	takerNode := newTestNode(t, confs, map[string]wallet.Connector{"BTC": mkBtc, "LTC": tkLtc})
	makerCC := &captureXConn{}
	takerCC := &captureXConn{}
	makerNode.conn = makerCC
	takerNode.conn = takerCC
	var orderID [32]byte
	s3h := hash20("unvalidated-deposit-order")
	copy(orderID[:], s3h[:])
	mkOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6, Status: "created"}
	tkOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6, Status: "created"}
	mkMPriv, mkMPub := newKey(t)
	makerNode.newMakerSession(withUsedCoins(t, makerNode, mkOrder, []wallet.Utxo{mkBtc.funding}), MakeOrderParams{MakerAddress: mkAddr, TakerAddress: ltcAddr}, arr32(mkMPriv), toArr33(mkMPub))
	takerNode.newTakerSession(withUsedCoins(t, takerNode, tkOrder, []wallet.Utxo{tkLtc.funding}), TakeOrderParams{FromAddress: ltcAddr, ToAddress: mkAddr}, arr32(tkPriv), to33(tkPub))
	// The store orders must carry OUR per-trade M pubkey (Order.MakerKey,
	// order.go:96) so the self-signed Cancel packets pass handleRemoteCancel's
	// iCanceled check and the local rollback actually runs.
	makerNode.store.Update(hexEncode(orderID[:]), func(o *Order) { o.MakerKey = hexEncode(mkMPub[:]) })
	takerNode.store.Update(hexEncode(orderID[:]), func(o *Order) { o.MakerKey = hexEncode(tkPub[:]) })
	var hub [20]byte
	hubH := hash20("hub")
	copy(hub[:], hubH[:])
	makerSession := makerNode.sessions[hexEncode(orderID[:])]
	takerSession := takerNode.sessions[hexEncode(orderID[:])]
	makerSession.hub = hub
	takerSession.hub = hub

	// --- Leg 1 (taker): a maker A-deposit that is not a valid HTLC p2sh must
	// be refused: wire-Cancel crBadADepositTx, NO CreatedB, NO B deposit of ours.
	fundingInternal, _ := reverseTxidHex(strings.Repeat("aa", 32))
	fakeA := &coins.Tx{Version: 1}
	fakeA.Inputs = append(fakeA.Inputs, coins.TxIn{PrevOut: coins.OutPoint{Hash: fundingInternal, Index: 0}, Sequence: seqFinal})
	fakeA.Outputs = append(fakeA.Outputs, coins.TxOut{Value: 250000000, ScriptPubKey: coins.BuildP2PKHScript(hash20("evil"))}) // P2PKH, not the HTLC p2sh
	fakeATxID := strings.Repeat("ef", 32)
	tkLtc.setRawTx(fakeATxID, hex.EncodeToString(fakeA.Serialize()))
	// The taker checks the maker's A deposit on its DST currency (BTC).
	mkBtc.setRawTx(fakeATxID, hex.EncodeToString(fakeA.Serialize()))

	_, _, err := takerSession.OnCreateB(&proto.CreateBBody{
		HubAddress: hub, ID: orderID, APubKey: to33(mkMPub),
		ADepositTxID: fakeATxID, HashedSecret: [20]byte{0x11}, ALockTime: 2000,
	})
	var sce *selfCancelErr
	if !errors.As(err, &sce) || sce.reason != crBadADepositTx {
		t.Fatalf("taker CreateB err = %v, want crBadADepositTx self-cancel", err)
	}
	if got := len(tkLtc.broadcasts); got != 0 {
		t.Fatalf("taker broadcast %d deposit(s) despite an unvalidated A-deposit, want 0 (theft killed)", got)
	}
	// A Cancel (crBadADepositTx) must be on the wire, and NO CreatedB.
	pkts := takerCC.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("taker wrote %d packets (cmd %v), want exactly one Cancel", len(pkts), pkts[0].Command)
	}
	// The taker's pre-deposit order is canceled locally (status "canceled").
	if o := takerNode.store.Get(hexEncode(orderID[:])); o == nil || o.Status != "canceled" {
		t.Fatalf("taker order status = %+v, want canceled", o)
	}

	// --- Leg 2 (maker): a taker B-deposit that is not the valid HTLC p2sh must
	// be refused at ConfirmA: wire-Cancel crBadBDepositTx, NO ConfirmedA, and the
	// maker's own deposit rolls back via its pre-signed refund.
	_, bodyA, err := makerSession.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkPub)})
	if err != nil {
		t.Fatalf("maker OnCreateA: %v", err)
	}
	createdA := bodyA.(*proto.CreatedABody)
	fakeB := &coins.Tx{Version: 1}
	fundingLtc, _ := reverseTxidHex(strings.Repeat("bb", 32))
	fakeB.Inputs = append(fakeB.Inputs, coins.TxIn{PrevOut: coins.OutPoint{Hash: fundingLtc, Index: 0}, Sequence: seqFinal})
	fakeB.Outputs = append(fakeB.Outputs, coins.TxOut{Value: 200000000, ScriptPubKey: coins.BuildP2PKHScript(hash20("evil-b"))})
	fakeBTxID := strings.Repeat("fe", 32)
	tkLtc.setRawTx(fakeBTxID, hex.EncodeToString(fakeB.Serialize()))

	_, _, err = makerSession.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID, BDepositTxID: fakeBTxID, BLockTime: createdA.ALockTime})
	if !errors.As(err, &sce) || sce.reason != crBadBDepositTx {
		t.Fatalf("maker ConfirmA err = %v, want crBadBDepositTx self-cancel", err)
	}
	// The maker's A deposit rolls back: status "rolled back" and the pre-signed
	// refund is enqueued (C++ processLater; in production the wallet rejects the
	// pre-lockTime CLTV spend and the background sweep retries once the
	// deposit's lockTime passes — never a second deposit).
	if o := makerNode.store.Get(hexEncode(orderID[:])); o == nil || o.Status != "rolled back" {
		t.Fatalf("maker order status = %+v, want rolled back", o)
	}
	if got := len(mkBtc.broadcasts); got != 2 {
		t.Fatalf("maker BTC broadcasts = %d, want 2 (deposit A + its enqueued refund)", got)
	}
	// The maker's Cancel (crBadBDepositTx) is on the wire; NO ConfirmedA.
	mkPkts := makerCC.snapshot()
	if len(mkPkts) != 1 || mkPkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("maker wrote %d packets (cmd %v), want exactly one Cancel", len(mkPkts), mkPkts[0].Command)
	}
}
