package api

import (
	"strconv"
	"testing"

	"go-xbridge/wallet"
)

// TestSelectTakeFunding pins the take-side funding-before-fee order (C++
// acceptXBridgeTransaction, xbridgeapp.cpp:2254-2321; go TakeOrder,
// node.go:1738-1824). The service-node fee selector's single-gt rule
// (fee_tx.go:118-123, C++ bitcoinrpcconnector.cpp:131-132) eats a
// dominating utxo when the fee is built first, starving funding selection
// afterward. Selecting funding first from the full set, then the fee from
// the remainder, fixes the starvation with the selectors themselves
// untouched. Live case: wallet {9.2233} + 40x{0.0095} BLOCK, take need
// 9.10 — fee-first deterministically refused with 1019 at node.go:1795.
func TestSelectTakeFunding(t *testing.T) {
	cc := selectVecCC() // MinTxFee floor: fee1 provision 10 XB, funding ideal window +0.02
	const needXB = uint64(9100000)

	t.Run("dominated wallet succeeds funding-first", func(t *testing.T) {
		vals := append([]float64{9.2233}, repVals(0.0095, 40)...)
		avail := vecUtxos(btcAddr, vals...)
		funding, feeRest, ok := selectTakeFunding(btcAddr, avail, avail, cc, needXB)
		if !ok {
			t.Fatalf("selectTakeFunding(dominated) ok = false, want true")
		}
		if len(funding) != 1 || funding[0].Value != 9.2233 {
			t.Fatalf("funding = %+v, want the single 9.2233 giant", funding)
		}
		fee, ok := selectFeeUtxos(feeRest, serviceNodeFeeReal)
		if !ok {
			t.Fatalf("fee from remainder ok = false, want true (smalls combine)")
		}
		if len(fee) == 0 {
			t.Fatalf("fee = empty, want 1-2 smalls covering 0.0151176")
		}
		seen := keySet(funding)
		for _, u := range fee {
			if seen[takeKey(u)] {
				t.Fatalf("fee input %+v reuses a funding input", u)
			}
		}
	})

	t.Run("fee-first order starves on the same wallet", func(t *testing.T) {
		vals := append([]float64{9.2233}, repVals(0.0095, 40)...)
		avail := vecUtxos(btcAddr, vals...)
		// Old order, replicated with the production selectors: fee first.
		feeFirst, ok := selectFeeUtxos(avail, serviceNodeFeeReal)
		if !ok {
			t.Fatalf("fee-first fee select ok = false, want true (it eats the giant)")
		}
		if len(feeFirst) != 1 || feeFirst[0].Value != 9.2233 {
			t.Fatalf("fee-first fee = %+v, want [9.2233]", feeFirst)
		}
		rest := excludeKeys(avail, keySet(feeFirst))
		if _, _, _, ok := selectUtxos(btcAddr, rest, cc, needXB); ok {
			t.Fatalf("fee-first funding ok = true, want false (starved remainder)")
		}
	})

	t.Run("genuinely short wallet fails either order", func(t *testing.T) {
		avail := vecUtxos(btcAddr, []float64{9.15}...)
		// Funding-first covers funding (the giant) but the fee stage finds
		// an empty remainder: the take still fails, at fee build.
		_, feeRest, ok := selectTakeFunding(btcAddr, avail, avail, cc, needXB)
		if !ok {
			t.Fatalf("selectTakeFunding(single-9.15) funding ok = false, want true")
		}
		if _, ok := selectFeeUtxos(feeRest, serviceNodeFeeReal); ok {
			t.Fatalf("fee from empty remainder ok = true, want false")
		}
		// Fee-first eats the giant for the fee, then funding starves.
		feeFirst, ok := selectFeeUtxos(avail, serviceNodeFeeReal)
		if !ok {
			t.Fatalf("fee-first fee select ok = false, want true")
		}
		rest := excludeKeys(avail, keySet(feeFirst))
		if _, _, _, ok := selectUtxos(btcAddr, rest, cc, needXB); ok {
			t.Fatalf("fee-first funding ok = true, want false")
		}
	})

	t.Run("small take leaves fee intact", func(t *testing.T) {
		avail := vecUtxos(btcAddr, []float64{0.05, 0.02}...)
		funding, feeRest, ok := selectTakeFunding(btcAddr, avail, avail, cc, 15000)
		if !ok {
			t.Fatalf("selectTakeFunding(small) ok = false, want true")
		}
		fee, ok := selectFeeUtxos(feeRest, serviceNodeFeeReal)
		if !ok {
			t.Fatalf("fee from remainder ok = false, want true")
		}
		seen := keySet(funding)
		for _, u := range fee {
			if seen[takeKey(u)] {
				t.Fatalf("fee input %+v reuses a funding input", u)
			}
		}
	})
}

func repVals(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func takeKey(u wallet.Utxo) string {
	return u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
}

func keySet(us []wallet.Utxo) map[string]bool {
	m := make(map[string]bool, len(us))
	for _, u := range us {
		m[takeKey(u)] = true
	}
	return m
}

func excludeKeys(us []wallet.Utxo, skip map[string]bool) []wallet.Utxo {
	out := make([]wallet.Utxo, 0, len(us))
	for _, u := range us {
		if !skip[takeKey(u)] {
			out = append(out, u)
		}
	}
	return out
}
