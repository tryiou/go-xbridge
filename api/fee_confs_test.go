package api

import (
	"encoding/hex"
	"fmt"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/wallet"
)

// TestTakeFeeRejectsZeroConfChange pins the C++ parity floor: service-node
// fee inputs need ≥1 confirmation (C++ unspentP2PKH via
// availableCoins(true,1); a 0-conf fee parent the hub's mempool lacks fails
// hub broadcast with crBadFeeTx, live 2026-09-24). Fee funds consisting only
// of 0-conf change (plus confirmed dust that cannot cover the fee alone)
// must fail with errInsufficientFunds.
func TestTakeFeeRejectsZeroConfChange(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Fee-covering 0-conf change: Confirmations unset reads 0, mirroring
	// the production absent-wire-field population (wallet/rpc.go).
	change := blkUtxo()
	change.TxID = fmt.Sprintf("%064x", 0x910)
	change.Amount = 93400000
	change.Value = 0.934
	// Confirmed dust in the mature pool (stub stamps Confirmations): covers
	// nothing alone, so the take must fail rather than spend the 0-conf.
	dust := blkUtxo()
	dust.TxID = fmt.Sprintf("%064x", 0x911)
	dust.Amount = 990000
	dust.Value = 0.0099
	funding := wallet.Utxo{
		TxID: fmt.Sprintf("%064x", 0x912), Vout: 0,
		Amount: 500000000, Value: 5.0,
		ScriptPubKey: "76a914000000000000000000000000000000000000000088ac",
		Address:      btcAddr,
	}
	n, _ := newStartedNode(t, map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}, map[string]wallet.Connector{
		"BTC":   &stubConn{ticker: "BTC", addr: btcAddr, utxos: []wallet.Utxo{funding}},
		"BLOCK": &stubConn{ticker: "BLOCK", addr: btcAddr, utxos: []wallet.Utxo{change}, matureUtxos: []wallet.Utxo{dust}},
	})
	hubPriv := make([]byte, 32)
	hubPriv[31] = 7
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	registerHub(t, n, hubPriv)
	var oid [32]byte
	copy(oid[:], []byte("take-zero-conf-reject-0000000000"))
	n.store.Add(&Order{
		ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		Status: "open", SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	})
	if _, rerr := n.TakeOrder(TakeOrderParams{
		ID:          orderIDString(oid),
		FromAddress: addrFor(0, "take-from-zc-reject"),
		ToAddress:   addrFor(0, "take-to-zc-reject"),
	}); rerr == nil {
		t.Fatal("take with only 0-conf fee funds succeeded: want errInsufficientFunds")
	} else if rerr.Code != errInsufficientFunds {
		t.Fatalf("rerr.Code = %d, want errInsufficientFunds (%d)", rerr.Code, errInsufficientFunds)
	}
}
