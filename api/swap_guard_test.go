package api

import (
	"testing"

	"go-xbridge/proto"
)

// guardSession returns a session pre-positioned at the given client state so a
// guard test can drive a POST-COMPLETION retransmit through a handler without
// any coin/node setup: the STATE-F77 state guards fire before any wallet or store
// work, so no connector, registry, or store is needed.
func guardSession(isMaker bool, state clientState) *SwapSession {
	var id [32]byte
	copy(id[:], "order-id-order-id-order-id-0")
	return &SwapSession{isMaker: isMaker, id: id, state: state}
}

// assertRetransmitDropped checks the handler's retransmit contract: it returns
// no response command/body, no error, and leaves the session state and the
// seeded counterparty fields untouched (a retransmit must not re-run stage 1).
func assertRetransmitDropped(t *testing.T, s *SwapSession, cmd proto.XBridgeCommand, body responseBody, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("retransmit errored: %v", err)
	}
	if cmd != 0 || body != nil {
		t.Fatalf("retransmit produced a response (%d, %v), want (0, nil)", cmd, body)
	}
}

// TestCreateAStateGuardDropsPostCompletionRetransmit mirrors C++
// processTransactionCreateA's `state >= trCreated` guard
// (xbridgesession.cpp:1947): a CreateA arriving after the maker's deposit was
// already created must be dropped without re-broadcasting a second deposit or
// overwriting the recorded counterparty key.
func TestCreateAStateGuardDropsPostCompletionRetransmit(t *testing.T) {
	s := guardSession(true, csCreatedA)
	s.theirPub = [33]byte{7}

	cmd, body, err := s.OnCreateA(&proto.CreateABody{ID: s.id, BPubKey: [33]byte{9}})
	assertRetransmitDropped(t, s, cmd, body, err)
	if s.state != csCreatedA {
		t.Errorf("state regressed to %v", s.state)
	}
	if s.theirPub != [33]byte{7} {
		t.Errorf("counterparty pubkey overwritten by retransmit: %x", s.theirPub)
	}
}

// TestCreateBStateGuardDropsPostCompletionRetransmit mirrors
// processTransactionCreateB's `state >= trCreated` guard (xbridgesession.cpp:2424).
func TestCreateBStateGuardDropsPostCompletionRetransmit(t *testing.T) {
	s := guardSession(false, csCreatedB)
	s.theirPub = [33]byte{7}
	s.theirDepositTxID = "cc"

	cmd, body, err := s.OnCreateB(&proto.CreateBBody{ID: s.id, APubKey: [33]byte{9}, ADepositTxID: "dd"})
	assertRetransmitDropped(t, s, cmd, body, err)
	if s.state != csCreatedB {
		t.Errorf("state regressed to %v", s.state)
	}
	if s.theirPub != [33]byte{7} {
		t.Errorf("counterparty pubkey overwritten by retransmit: %x", s.theirPub)
	}
	if s.theirDepositTxID != "cc" {
		t.Errorf("counterparty deposit overwritten by retransmit: %q", s.theirDepositTxID)
	}
}

// TestConfirmAStateGuardDropsPostCompletionRetransmit mirrors
// processTransactionConfirmA's `state >= trCommited` guard
// (xbridgesession.cpp:2897): a ConfirmA arriving after the maker already
// redeemed must not re-broadcast a second claim payTx.
func TestConfirmAStateGuardDropsPostCompletionRetransmit(t *testing.T) {
	s := guardSession(true, csConfirmedA)
	s.theirDepositTxID = "cc"

	cmd, body, err := s.OnConfirmA(&proto.ConfirmABody{ID: s.id, BDepositTxID: "dd"})
	assertRetransmitDropped(t, s, cmd, body, err)
	if s.state != csConfirmedA {
		t.Errorf("state regressed to %v", s.state)
	}
	if s.theirDepositTxID != "cc" {
		t.Errorf("counterparty deposit overwritten by retransmit: %q", s.theirDepositTxID)
	}
}

// TestConfirmBStateGuardDropsPostCompletionRetransmit mirrors
// processTransactionConfirmB's `state >= trCommited` guard
// (xbridgesession.cpp:3152): a ConfirmB arriving after the taker already
// redeemed must not re-broadcast a second claim payTx.
func TestConfirmBStateGuardDropsPostCompletionRetransmit(t *testing.T) {
	s := guardSession(false, csConfirmedB)

	cmd, body, err := s.OnConfirmB(&proto.ConfirmBBody{ID: s.id, APayTxID: "dd"})
	assertRetransmitDropped(t, s, cmd, body, err)
	if s.state != csConfirmedB {
		t.Errorf("state regressed to %v", s.state)
	}
}
