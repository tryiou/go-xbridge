package api

import (
	"bytes"
	"testing"
	"time"

	"xbridge-go/crypto"
	"xbridge-go/proto"
)

// TestOnConfirmAMissingConnectorIsError verifies the swap handler returns an
// error (not a nil-interface panic) when the destination currency's wallet
// connector is absent — the exact crash the audit flagged at api/swap.go:225.
// Before the fix, s.n.cfg.Connectors[cur] returned a nil interface and the
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
	delete(node.cfg.Connectors, "BTC")

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
// crash the process: dispatchSwap must recover and return.
func TestDispatchSwapRecoversFromPanic(t *testing.T) {
	node := newWalletTestCtx().Node
	node.sessions = map[string]*SwapSession{}
	id := [32]byte{7}
	node.sessions[hexEncode(id[:])] = &SwapSession{n: node, id: id, state: csMaker}

	done := make(chan struct{})
	go func() {
		node.dispatchSwap(id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
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
