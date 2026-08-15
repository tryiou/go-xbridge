package api

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// reloadMidTaskConf is the xbridge.conf the node reloads from — the same coin
// set (BTC/LTC) with which the test node was started, so the reload's coin
// registry re-init and fresh connector build are hermetic (CheckReachability
// is disabled, so no wallet probe runs).
const reloadMidTaskConf = `
[Main]
ExchangeWallets=BTC,LTC

[BTC]
Ip=127.0.0.1
Port=18443
COIN=100000000
AddressPrefix=0
CreateTxMethod=BTC
BlockTime=60
Confirmations=2

[LTC]
Ip=127.0.0.1
Port=19443
COIN=100000000
AddressPrefix=48
CreateTxMethod=BTC
BlockTime=60
Confirmations=2
`

// reloadMidTaskCoinConf is the same coin set but with BTC's AddressPrefix AND
// ScriptPrefix CHANGED (0 → 111 both), so the reload's coin-registry re-init
// yields a BTC coin whose version bytes no longer match the addresses built at
// make-time. A worker that re-resolved the coin from the live registry
// mid-task would fail to decode those addresses (version byte 0 matches
// neither 111); the snapshot coin must be what the build uses.
const reloadMidTaskCoinConf = `
[Main]
ExchangeWallets=BTC,LTC

[BTC]
Ip=127.0.0.1
Port=18443
COIN=100000000
AddressPrefix=111
ScriptPrefix=111
CreateTxMethod=BTC
BlockTime=60
Confirmations=2

[LTC]
Ip=127.0.0.1
Port=19443
COIN=100000000
AddressPrefix=48
CreateTxMethod=BTC
BlockTime=60
Confirmations=2
`

// TestReloadMidSwapTaskKeepsConnectorSnapshot. A
// dxLoadXBridgeConf mid-swap-task must not swap which connector a two-phase
// deposit task builds against: the worker captured its connectors at enqueue
// time (the C++ session holds the connector pointer it captured, so a reload
// that replaces n.config.Connectors with fresh objects cannot redirect an
// in-flight task).
//
// Determinism: all engineWorkers are parked on the gated BTC connector so the
// CreateA deposit task waits in the queue — its snapshot is taken at enqueue,
// BEFORE the reload lands. When the workers are released, the task must still
// build against gated (the snapshot), not the fresh RPC connector; otherwise
// the RPC build fails and no CreatedA appears.
func TestReloadMidSwapTaskKeepsConnectorSnapshot(t *testing.T) {
	n, cc, gated, _ := setupTwoCoinNode(t)
	defer gated.open()

	// Wire the node for a real reloadConf (hermetic: no reachability probe).
	dir := t.TempDir()
	confPath := filepath.Join(dir, "xbridge.conf")
	if err := os.WriteFile(confPath, []byte(reloadMidTaskConf), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	n.cfgMu.Lock()
	n.config.ConfPath = confPath
	n.config.CheckReachability = false
	n.cfgMu.Unlock()

	// Park all engineWorkers with refund broadcasts on the gated BTC connector,
	// so the deposit task posted below waits in the queue. Each refund task
	// captures the connector at enqueue, so the parked tasks also
	// resume against gated after the reload.
	for i := 0; i < engineWorkers; i++ {
		var id [32]byte
		copy(id[:], fmt.Sprintf("park-worker-%d-%012d", i, i))
		rtx := &coins.Tx{Version: 1}
		rtx.Inputs = []coins.TxIn{{PrevOut: coins.OutPoint{Hash: mustHash(strings.Repeat("aa", 32)), Index: 0}, Sequence: 0xfffffffe}}
		rtx.Outputs = []coins.TxOut{{Value: 1, ScriptPubKey: []byte{0x51}}}
		orderID := hexEncode(id[:])
		refundHex := hex.EncodeToString(rtx.Serialize())
		n.submit(func() {
			n.postRefundTask(orderID, "BTC", refundHex, 0, false, nil)
		}, true)
	}
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() < engineWorkers && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() != engineWorkers {
		t.Fatalf("parked %d of %d workers", gated.callCount(), engineWorkers)
	}

	// Enqueue a CreateA deposit task; its snapshot is taken now (engine side),
	// then it waits behind the parked workers.
	mPriv, mPub := newKey(t)
	var aID [32]byte
	copy(aID[:], "reload-mid-task-order-00000")
	n.newMakerSession(withUsedCoins(t, n, &Order{ID: aID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6}, []wallet.Utxo{gated.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "maker-a"), TakerAddress: addrFor(0, "taker-a")}, arr32(mPriv), toArr33(mPub))
	n.submit(func() {
		if _, _, err := n.sessions[hexEncode(aID[:])].OnCreateA(&proto.CreateABody{ID: aID, BPubKey: to33(mPub)}); err != nil {
			t.Errorf("OnCreateA stage 1: %v", err)
		}
	}, true)

	// Mid-task reload: the live config now carries a FRESH BTC connector.
	if err := n.reloadConf(); err != nil {
		t.Fatalf("reloadConf: %v", err)
	}
	if newConn := n.cfg().Connectors["BTC"]; newConn == gated {
		t.Fatal("reload must swap the BTC connector to a fresh object")
	}

	// Release the parked workers; the queued deposit task must run against the
	// snapshot (gated), or the RPC-backed connector build fails and no CreatedA
	// is ever written.
	gated.open()
	pkt := waitForPacket(t, cc, proto.XbcTransactionCreatedA, 10*time.Second)
	if pkt == nil {
		t.Fatal("no CreatedA — deposit did not complete against the snapshot connector")
	}
}

// TestReloadMidSwapTaskKeepsCoinSnapshot closure. The worker
// captured the session's coin values at enqueue (swapCtx.coinsMap), so a
// dxLoadXBridgeConf mid-task that CHANGES a coin's P2PKH byte must not change
// what the in-flight deposit build decodes addresses with. If the worker
// re-resolved coins.Get from the live registry, the reloaded BTC coin
// (AddressPrefix=111) would fail to decode the make-time address (version byte
// 0) and no CreatedA would appear.
func TestReloadMidSwapTaskKeepsCoinSnapshot(t *testing.T) {
	n, cc, gated, _ := setupTwoCoinNode(t)
	defer gated.open()

	dir := t.TempDir()
	confPath := filepath.Join(dir, "xbridge.conf")
	if err := os.WriteFile(confPath, []byte(reloadMidTaskCoinConf), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	n.cfgMu.Lock()
	n.config.ConfPath = confPath
	n.config.CheckReachability = false
	n.cfgMu.Unlock()

	for i := 0; i < engineWorkers; i++ {
		var id [32]byte
		copy(id[:], fmt.Sprintf("park-worker-%d-%012d", i, i))
		rtx := &coins.Tx{Version: 1}
		rtx.Inputs = []coins.TxIn{{PrevOut: coins.OutPoint{Hash: mustHash(strings.Repeat("aa", 32)), Index: 0}, Sequence: 0xfffffffe}}
		rtx.Outputs = []coins.TxOut{{Value: 1, ScriptPubKey: []byte{0x51}}}
		orderID := hexEncode(id[:])
		refundHex := hex.EncodeToString(rtx.Serialize())
		n.submit(func() {
			n.postRefundTask(orderID, "BTC", refundHex, 0, false, nil)
		}, true)
	}
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() < engineWorkers && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() != engineWorkers {
		t.Fatalf("parked %d of %d workers", gated.callCount(), engineWorkers)
	}

	mPriv, mPub := newKey(t)
	var aID [32]byte
	copy(aID[:], "reload-mid-task-order-00000")
	n.newMakerSession(withUsedCoins(t, n, &Order{ID: aID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6}, []wallet.Utxo{gated.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "maker-a"), TakerAddress: addrFor(0, "taker-a")}, arr32(mPriv), toArr33(mPub))
	n.submit(func() {
		if _, _, err := n.sessions[hexEncode(aID[:])].OnCreateA(&proto.CreateABody{ID: aID, BPubKey: to33(mPub)}); err != nil {
			t.Errorf("OnCreateA stage 1: %v", err)
		}
	}, true)

	if err := n.reloadConf(); err != nil {
		t.Fatalf("reloadConf: %v", err)
	}
	if coin, _ := coins.Get("BTC"); coin.P2PKH != 111 || coin.P2SH != 111 {
		t.Fatalf("precondition: reload must change BTC P2PKH/P2SH to 111, got P2PKH=%d P2SH=%d", coin.P2PKH, coin.P2SH)
	}

	gated.open()
	pkt := waitForPacket(t, cc, proto.XbcTransactionCreatedA, 10*time.Second)
	if pkt == nil {
		t.Fatal("no CreatedA — deposit did not build against the snapshot coin (live coins.Get leaked the reloaded P2PKH)")
	}
}
