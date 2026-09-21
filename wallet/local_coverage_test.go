package wallet

import (
	"errors"
	"testing"
)

// TestLocalConnectorNoSourceContract pins the documented LocalConnector
// contract: address/UTXO/fee/chain queries have no local source and must fail
// (local.go). Chain-data reads report ErrNoChainSource so the api layer can
// distinguish "cannot judge" from generic errors; the rest report plain
// errors. Previously only SignRawTransaction and the GetBlockTxs negative
// were covered — 16 exported methods sat at 0%.
func TestLocalConnectorNoSourceContract(t *testing.T) {
	c := NewLocalConnector("BTC", nil, nil)
	if got := c.Ticker(); got != "BTC" {
		t.Fatalf("Ticker = %q, want BTC", got)
	}

	if _, err := c.GetBalance(); err == nil {
		t.Error("GetBalance: want error, got nil")
	}
	if _, err := c.GetNewAddress(); err == nil {
		t.Error("GetNewAddress: want error, got nil")
	}
	if _, err := c.ListUnspent(1); err == nil {
		t.Error("ListUnspent: want error, got nil")
	}
	if _, err := c.GetRelayFee(); err == nil {
		t.Error("GetRelayFee: want error, got nil")
	}
	if _, err := c.GetBlockCount(); err == nil {
		t.Error("GetBlockCount: want error, got nil")
	}
	if _, err := c.GetBlockHash(100); err == nil {
		t.Error("GetBlockHash: want error, got nil")
	}
	if _, err := c.GetRawTransaction("aa"); err == nil {
		t.Error("GetRawTransaction: want error, got nil")
	}
	if _, err := c.SignMessage("addr", "msg"); err == nil {
		t.Error("SignMessage: want error, got nil")
	}
	if _, err := c.VerifyMessage("addr", []byte{1}, "msg"); err == nil {
		t.Error("VerifyMessage: want error, got nil")
	}

	for name, err := range map[string]error{
		"CheckDeposit": func() error {
			_, err := c.CheckDepositTransaction("aa", "bb", 1, 1)
			return err
		}(),
		"GetTxOut": func() error {
			_, _, err := c.GetTxOut("aa", 0)
			return err
		}(),
		"GetRawTransactionVerbose": func() error {
			_, err := c.GetRawTransactionVerbose("aa")
			return err
		}(),
		"GetRawMempool": func() error {
			_, err := c.GetRawMempool()
			return err
		}(),
	} {
		if !errors.Is(err, ErrNoChainSource) {
			t.Errorf("%s: err = %v, want ErrNoChainSource", name, err)
		}
	}
	// NOTE (contract gap, not fixed here): GetBlockTxs reports a plain
	// "no block source" error instead of ErrNoChainSource like its sibling
	// chain reads. The rescan holds its cursor on ANY GetBlockTxs error, so
	// behavior is identical today — but a future errors.Is branch would miss
	// it. Left as prod behavior; unify only with an api-owner decision.
	if _, err := c.GetBlockTxs([32]byte{}); err == nil {
		t.Error("GetBlockTxs: want error, got nil")
	}
}

// TestLocalConnectorBroadcastGate pins the Broadcaster gate: nil broadcasts
// fail closed, a configured one is invoked with the exact hex and its
// result (and error) propagate.
func TestLocalConnectorBroadcastGate(t *testing.T) {
	closed := NewLocalConnector("BTC", nil, nil)
	if _, err := closed.SendRawTransaction("deadbeef"); err == nil {
		t.Fatal("SendRawTransaction(nil broadcaster): want error, got nil")
	}

	const wantTxid = "txid-from-broadcaster"
	var gotHex string
	open := NewLocalConnector("BTC", nil, func(txHex string) (string, error) {
		gotHex = txHex
		return wantTxid, nil
	})
	txid, err := open.SendRawTransaction("deadbeef")
	if err != nil {
		t.Fatalf("SendRawTransaction: %v", err)
	}
	if txid != wantTxid || gotHex != "deadbeef" {
		t.Fatalf("broadcast = (%q, hex %q), want (%q, hex deadbeef)", txid, gotHex, wantTxid)
	}

	wantErr := errors.New("broadcast boom")
	failing := NewLocalConnector("BTC", nil, func(string) (string, error) {
		return "", wantErr
	})
	if _, err := failing.SendRawTransaction("deadbeef"); !errors.Is(err, wantErr) {
		t.Fatalf("SendRawTransaction error = %v, want %v", err, wantErr)
	}
}
