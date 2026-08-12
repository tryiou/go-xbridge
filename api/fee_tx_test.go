package api

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/wallet"
)

// TestFeeOrderInfo locks the OP_RETURN order-info payload generated for the
// service-node fee tx (C++ xbridgeapp.cpp:2209-2229). The canonical vector is
// ["<64hex>","BTC",1500000,"LTC",300000] = 95 bytes — well under the 157-byte
// datacarrier cap, so no truncation.
func TestFeeOrderInfo(t *testing.T) {
	var id [32]byte
	out, err := feeOrderInfo(id, "BTC", 1500000, "LTC", 300000)
	if err != nil {
		t.Fatalf("feeOrderInfo: %v", err)
	}
	want := `["0000000000000000000000000000000000000000000000000000000000000000","BTC",1500000,"LTC",300000]`
	if string(out) != want {
		t.Errorf("feeOrderInfo = %q, want %q", out, want)
	}
	if len(out) != 95 {
		t.Errorf("feeOrderInfo length = %d, want 95 (C++ canonical)", len(out))
	}
	if len(out) > maxOrderInfoBytes {
		t.Errorf("feeOrderInfo exceeds %d-byte datacarrier cap", maxOrderInfoBytes)
	}
	// A maximal sanitized payload must still fit after truncation (never
	// triggers in practice, but must not return garbage).
	long := "X"
	id[0] = 1
	_, err = feeOrderInfo(id, long, 1, long, 1)
	if err != nil {
		t.Errorf("feeOrderInfo(oversize) = %v, want no error (truncated to fit)", err)
	}
	// A base that already exceeds the datacarrier cap cannot be rescued: the id
	// stays full (C++ size_t underflow in orderId.erase) and the payload is
	// reported as an overflow sentinel — the C++ path reverts the order with
	// INVALID_ONCHAIN_HISTORY (xbridgeapp.cpp:2226-2228), which the caller maps
	// separately from the generic fee-prep errors. This must not panic.
	huge := ""
	for i := 0; i < 100; i++ {
		huge += "X"
	}
	if _, err := feeOrderInfo(id, huge, 1, huge, 1); !errors.Is(err, errOrderInfoOverflow) {
		t.Errorf("feeOrderInfo(overflow) = %v, want errOrderInfoOverflow", err)
	}
}

// TestEstFeeBlock locks C++ createFeeTransaction's hardcoded fee estimator
// ((192*in + 34*out) * 40/1e8, bitcoinrpcconnector.cpp:96-98).
func TestEstFeeBlock(t *testing.T) {
	if got, want := estFeeBlock(1, 3), 0.0001176; got != want {
		t.Errorf("estFeeBlock(1,3) = %v, want %v", got, want)
	}
	if got, want := estFeeBlock(3, 3), 0.0002712; got != want {
		t.Errorf("estFeeBlock(3,3) = %v, want %v", got, want)
	}
}

// TestSelectFeeUtxos exercises the three selection branches of C++
// selectFeeUtxos (bitcoinrpcconnector.cpp:101-130): the ideal single input
// window, the smallest-above-minimum path, the sum-below path, and the fail.
func TestSelectFeeUtxos(t *testing.T) {
	u := func(v float64) wallet.Utxo {
		return wallet.Utxo{Value: v, Address: btcAddr}
	}
	t.Run("ideal single", func(t *testing.T) {
		sel, ok := selectFeeUtxos([]wallet.Utxo{u(0.02), u(0.04)}, serviceNodeFeeReal)
		if !ok || len(sel) != 1 || sel[0].Value != 0.02 {
			t.Fatalf("selectFeeUtxos(ideal) = %+v, %v; want [0.02], true", sel, ok)
		}
	})
	t.Run("smallest above min", func(t *testing.T) {
		sel, ok := selectFeeUtxos([]wallet.Utxo{u(0.05), u(1.0), u(0.04)}, serviceNodeFeeReal)
		if !ok || len(sel) != 1 || sel[0].Value != 0.04 {
			t.Fatalf("selectFeeUtxos(gt) = %+v, %v; want [0.04], true", sel, ok)
		}
	})
	t.Run("sum below min", func(t *testing.T) {
		sel, ok := selectFeeUtxos([]wallet.Utxo{u(0.01), u(0.01)}, serviceNodeFeeReal)
		if !ok || len(sel) != 2 {
			t.Fatalf("selectFeeUtxos(lt-sum) = %+v, %v; want 2 inputs, true", sel, ok)
		}
	})
	t.Run("fail", func(t *testing.T) {
		if sel, ok := selectFeeUtxos([]wallet.Utxo{u(0.01)}, serviceNodeFeeReal); ok || sel != nil {
			t.Fatalf("selectFeeUtxos(single-lt) = %+v, %v; want nil, false", sel, ok)
		}
	})
}

// TestIsP2PKH25 locks the 25-byte P2PKH (`76a914…88ac`) output-script filter of
// rpc::unspentP2PKH (bitcoinrpcconnector.cpp:282-284).
func TestIsP2PKH25(t *testing.T) {
	good := "76a914000000000000000000000000000000000000000088ac"
	if !isP2PKH25(good) {
		t.Errorf("isP2PKH25(%s) = false, want true", good)
	}
	cases := []string{
		"", // empty
		"00000000000000000000000000000000000000000000000000", // 25 bytes but wrong template
		"76a914000000000000000000000000000000000000000000",   // 24 bytes script
		"not-hex", // unparseable
		"76a9140000000000000000000000000000000000000000", // wrong length (23)
	}
	for _, c := range cases {
		if isP2PKH25(c) {
			t.Errorf("isP2PKH25(%q) = true, want false", c)
		}
	}
}

// TestBuildServiceNodeFeeTx builds the BLOCK service-node fee tx from a single
// deterministic 1.0 BLOCK p2pkh utxo (stubConn passes the unsigned tx through
// as "signed", so the shape is fully deterministic) and asserts the C++
// createFeeTransaction layout (bitcoinrpcconnector.cpp:132-234): version 1, no
// nTime, Sequence 0xffffffff, [OP_RETURN data, 0.015 → registry payment
// address, change ≥ 5460].
func TestBuildServiceNodeFeeTx(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BLOCK": {Ticker: "BLOCK", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000, TxVersion: 1},
	}); err != nil {
		t.Fatal(err)
	}
	blkCoin, _ := coins.Get("BLOCK")
	blkConf := &config.CoinConf{Ticker: "BLOCK", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000, TxVersion: 1}
	dest := [20]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e}
	data := []byte(`["0000000000000000000000000000000000000000000000000000000000000000","BTC",300000,"BTC",1500000]`)
	avail := []wallet.Utxo{blkUtxo()}
	conn := &stubConn{ticker: "BLOCK", addr: btcAddr}

	rawHex, inputs, rerr := buildServiceNodeFeeTx(conn, blkCoin, blkConf, dest, data, avail)
	if rerr != nil {
		t.Fatalf("buildServiceNodeFeeTx: %v", rerr)
	}
	if rawHex == "" {
		t.Fatal("buildServiceNodeFeeTx returned empty hex")
	}
	if len(inputs) != 1 || inputs[0].TxID != blkUtxo().TxID {
		t.Fatalf("fee inputs = %+v, want the single p2pkh utxo", inputs)
	}

	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}
	tx, err := coins.Deserialize(raw)
	if err != nil {
		t.Fatalf("deserialize fee tx: %v", err)
	}
	if tx.Version != 1 || tx.WithTime {
		t.Errorf("fee tx version/time = %d/%v, want v1 no-time (CMutableTransaction)", tx.Version, tx.WithTime)
	}
	if len(tx.Inputs) != 1 || tx.Inputs[0].Sequence != 0xffffffff || tx.Inputs[0].PrevOut.Index != 0 {
		t.Fatalf("fee tx inputs = %+v, want 1 input Seq 0xffffffff vout 0", tx.Inputs)
	}
	if len(tx.Outputs) != 3 {
		t.Fatalf("fee tx outputs = %d, want 3 (OP_RETURN, fee, change)", len(tx.Outputs))
	}
	if tx.Outputs[0].Value != 0 || len(tx.Outputs[0].ScriptPubKey) == 0 || tx.Outputs[0].ScriptPubKey[0] != coins.OpReturn {
		t.Errorf("fee tx output[0] = %+v, want OP_RETURN data carrier", tx.Outputs[0])
	}
	wantFee := uint64(serviceNodeFeeReal * float64(blkConf.Coin))
	if tx.Outputs[1].Value != wantFee {
		t.Errorf("fee output value = %d, want %d (.015 BLOCK)", tx.Outputs[1].Value, wantFee)
	}
	if got := tx.Outputs[1].ScriptPubKey; !equalP2PKH(got, dest) {
		t.Errorf("fee output script = %x, want p2pkh(%x)", got, dest)
	}
	if tx.Outputs[2].Value < minFeeChangeDust {
		t.Errorf("change output = %d, want >= %d", tx.Outputs[2].Value, minFeeChangeDust)
	}
}

// equalP2PKH reports whether script is a 25-byte P2PKH paying dest.
func equalP2PKH(script []byte, dest [20]byte) bool {
	if len(script) != 25 {
		return false
	}
	return bytes.Equal(script, coins.BuildP2PKHScript(dest))
}
