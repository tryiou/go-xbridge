package api

import (
	"encoding/hex"
	"errors"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/wallet"
)

// orderIDParts returns the fixed inputs used by TestSha256dOrderID: from = 0x11..0x24,
// to = 0x21..0x34, blockHash = 0x30..0x4f, sig = 0x40..0x80 (65 bytes).
func orderIDParts() (from, to [20]byte, bh [32]byte, sig []byte) {
	for i := 0; i < 20; i++ {
		from[i] = byte(0x11 + i)
		to[i] = byte(0x21 + i)
	}
	for i := 0; i < 32; i++ {
		bh[i] = byte(0x30 + i)
	}
	for i := 0; i < 65; i++ {
		sig = append(sig, byte(0x40+i))
	}
	return from, to, bh, sig
}

// TestSha256dOrderID locks the deterministic order id against an independent
// golden: double-SHA256 over varstr(from20)∥varstr("BTC")∥u64LE(1500000)∥
// varstr(to20)∥varstr("SYS")∥u64LE(300000)∥u64LE(ts)∥blockHash[32]∥
// varstr(sig65), computed with python3 hashlib (not via the port function).
func TestSha256dOrderID(t *testing.T) {
	from, to, bh, sig := orderIDParts()
	id := sha256dOrderID(from, "BTC", 1500000, to, "SYS", 300000, 1700000000123456, bh, sig)
	const want = "f799926c961c8caca1a86f322ef4dc60ef780a5c750ee1a6d79c25d935fcd09f"
	if got := hex.EncodeToString(id[:]); got != want {
		t.Fatalf("sha256dOrderID = %s, want %s", got, want)
	}
}

// vecUtxos builds a wallet utxo set for selector vector tests: whole-coin Value
// doubles with matching Amount base units and distinct valid 64-hex txids.
func vecUtxos(addr string, vals ...float64) []wallet.Utxo {
	out := make([]wallet.Utxo, len(vals))
	for i, v := range vals {
		var h [32]byte
		h[31] = byte(i + 1)
		out[i] = wallet.Utxo{
			TxID: hex.EncodeToString(h[:]), Vout: uint32(i),
			Amount: uint64(v * 1e6), Value: v, Address: addr,
		}
	}
	return out
}

// selectVecCC is the vector-test CoinConf: MinTxFee=1000 floors every small-tx
// estimateFee below it, so minTxFeeWhole == 1e-5 and xBridgeIntFromReal(...) ==
// 10 for all in/out counts used here (C++ minTxFee1/minTxFee2, :1948-1969).
func selectVecCC() *config.CoinConf {
	return &config.CoinConf{Coin: 100000000, FeePerByte: 2, MinTxFee: 1000}
}

// TestSelectUtxosVectors locks App::selectUtxos (xbridgeapp.cpp:2972-3077) with
// hand-traced vectors. requiredAmount=1_500_000 -> amt=1.5, minAmount=1.50002,
// ideal upper bound 1.52002; fee1 is always 10 (MinTxFee floor).
func TestSelectUtxosVectors(t *testing.T) {
	cc := selectVecCC()
	cases := []struct {
		name    string
		wallet  []float64 // utxo Values (unsorted)
		wantOut []float64 // selected Values in returned order
		amount  uint64
		fee1    uint64
		ok      bool
	}{
		{name: "ideal", wallet: []float64{0.9, 1.51, 0.5}, wantOut: []float64{1.51}, amount: 1510000, fee1: 10, ok: true},
		{name: "gt-single", wallet: []float64{2.0}, wantOut: []float64{2.0}, amount: 2000000, fee1: 10, ok: true},
		{name: "gt-multi", wallet: []float64{5.0, 3.0, 1.2}, wantOut: []float64{3.0}, amount: 3000000, fee1: 10, ok: true},
		{name: "lt-accumulate", wallet: []float64{0.9, 0.8, 0.1}, wantOut: []float64{0.9, 0.8}, amount: 1700000, fee1: 10, ok: true},
		{name: "lt-fewer-than-two", wallet: []float64{0.9}, wantOut: nil, amount: 0, fee1: 0, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, amount, fee1, ok := selectUtxos(btcAddr, vecUtxos(btcAddr, tc.wallet...), cc, 1500000)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if len(out) != len(tc.wantOut) {
				t.Fatalf("selected %d utxos, want %d (%+v)", len(out), len(tc.wantOut), out)
			}
			for i, want := range tc.wantOut {
				if out[i].Value != want {
					t.Fatalf("selected[%d].Value = %v, want %v", i, out[i].Value, want)
				}
			}
			if amount != tc.amount {
				t.Fatalf("utxoAmount = %d, want %d", amount, tc.amount)
			}
			if fee1 != tc.fee1 {
				t.Fatalf("fee1 = %d, want %d", fee1, tc.fee1)
			}
		})
	}
}

// TestSelectPartialUtxosVectors locks App::selectPartialUtxos
// (xbridgeapp.cpp:3079-3237) with hand-traced vectors. All comparisons run on
// camount() (Value/1e6, +1/1e8); per-utxo fee and prep-tx fees are 10 except
// minTxFeeWhole(3,4)=14.
func TestSelectPartialUtxosVectors(t *testing.T) {
	cc := selectVecCC()
	cases := []struct {
		name    string
		wallet  []float64
		require []uint64 // requiredAmount, requiredUtxoCount, requiredFeePerUtxo
		prep    int      // requiredPrepTxVouts
		split   uint64   // requiredSplitSize
		rem     uint64   // requiredRemainder
		wantOut []float64
		amount  uint64
		fees    uint64
		exact   bool
		ok      bool
	}{
		{
			name: "ideal-exact", wallet: []float64{1.00001, 1.00001},
			require: []uint64{2000000, 2, 10}, prep: 4, split: 1000000, rem: 0,
			wantOut: []float64{1.00001, 1.00001}, amount: 2000020, fees: 20, exact: true, ok: true,
		},
		{
			name: "exact-remainder-scan", wallet: []float64{1.00001, 1.00001, 0.10001},
			require: []uint64{2100000, 2, 10}, prep: 4, split: 1000000, rem: 500000,
			wantOut: []float64{1.00001, 1.00001, 0.10001}, amount: 2100030, fees: 30, exact: true, ok: true,
		},
		{
			name: "autosplit-pick", wallet: []float64{2.0, 1.0, 0.2, 0.1},
			require: []uint64{2000000, 2, 10}, prep: 4, split: 1000000, rem: 600000,
			wantOut: []float64{2.0, 1.0}, amount: 3000000, fees: 20, exact: false, ok: true,
		},
		{
			name: "insufficient", wallet: []float64{0.5},
			require: []uint64{2000000, 2, 10}, prep: 4, split: 1000000, rem: 0,
			wantOut: nil, amount: 0, fees: 0, exact: false, ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, amount, fees, exact, ok := selectPartialUtxos(
				vecUtxos(btcAddr, tc.wallet...), cc,
				tc.require[0], tc.require[1], tc.require[2], tc.prep, tc.split, tc.rem)
			if ok != tc.ok || exact != tc.exact {
				t.Fatalf("ok = %v (want %v), exact = %v (want %v)", ok, tc.ok, exact, tc.exact)
			}
			if len(out) != len(tc.wantOut) {
				t.Fatalf("selected %d utxos, want %d (%+v)", len(out), len(tc.wantOut), out)
			}
			for i, want := range tc.wantOut {
				if out[i].Value != want {
					t.Fatalf("selected[%d].Value = %v, want %v", i, out[i].Value, want)
				}
			}
			if amount != tc.amount {
				t.Fatalf("utxoAmount = %d, want %d", amount, tc.amount)
			}
			if fees != tc.fees {
				t.Fatalf("fees = %d, want %d", fees, tc.fees)
			}
		})
	}
}

// shortSigConn wraps the stub connector but always returns a non-65-byte
// signmessage proof.
type shortSigConn struct{ *stubConn }

func (s *shortSigConn) SignMessage(address, message string) ([]byte, error) {
	return []byte{0x01}, nil
}

// TestBuildUtxoProofsRejectsShortSig verifies the strict 65-byte signature
// guard (C++ INVALID_SIGNATURE, xbridgeapp.cpp:1705): a wallet returning a
// short proof fails the order build instead of silently padding/truncating.
func TestBuildUtxoProofsRejectsShortSig(t *testing.T) {
	coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	})
	c, ok := coins.Get("BTC")
	if !ok {
		t.Fatal("BTC coin not registered")
	}
	conn := &shortSigConn{&stubConn{ticker: "BTC", addr: btcAddr}}
	utxos := []wallet.Utxo{{
		TxID:    "0000000000000000000000000000000000000000000000000000000000000001",
		Vout:    0,
		Value:   1.0,
		Address: btcAddr,
	}}
	if _, err := buildUtxoProofs(conn, utxos, c); !errors.Is(err, errBadSigLen) {
		t.Fatalf("buildUtxoProofs = %v, want errBadSigLen", err)
	}
}
