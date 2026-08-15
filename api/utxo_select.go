package api

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	"go-xbridge/config"
	"go-xbridge/p2p"
	"go-xbridge/wallet"
)

// maxPartialOrderUtxos caps how many utxos a partial order may be split across
// (C++ xBridgePartialOrderMaxUtxos, src/xbridge/util/xutil.h:81).
const maxPartialOrderUtxos = 10

// xBridgeValueFromAmount converts XBridge base units (COIN=1e6) into a whole
// coin, mirroring C++ xBridgeValueFromAmount (xutil.cpp:223-227):
// a/COIN + 1.0/::COIN. The trailing 1e-8 term matches C++ exactly and is what
// makes amounts at the coin-scale boundary round up instead of truncating down.
func xBridgeValueFromAmount(a uint64) float64 {
	return float64(a)/float64(coinScale) + 1.0/1e8
}

// xBridgeIntFromReal converts a whole-coin double into XBridge base units the
// way C++ does (xutil.cpp:232-236): trunc(v*COIN + 1/::COIN). The +1/1e8 term
// nudges representable doubles up by one satoshi before truncation.
func xBridgeIntFromReal(v float64) uint64 {
	return uint64(v*float64(coinScale) + 1e-8)
}

// camount returns the XBridge base-unit value of a wallet utxo, computed from
// the whole-coin double exactly as C++ UtxoEntry::camount()
// (xbridgewallet.h:65): xBridgeIntFromReal(amount). Selection math in
// selectPartialUtxos is done on camount only, never on the native Value.
func camount(u wallet.Utxo) uint64 {
	return xBridgeIntFromReal(u.Value)
}

// minTxFeeWhole returns the per-tx fee as a whole-coin double, mirroring C++
// minTxFee1/minTxFee2 (xbridgewalletconnectorbtc.cpp:1948-1972). Both C++
// functions are identical (each floored at minTxFee), so the Go port uses a
// single helper: estimateFee(nIn,nOut)/COIN. A nil CoinConf (no config for the
// coin) falls back to the XBridge scale defensively; the selection call sites
// always pass the maker's configured coin.
func minTxFeeWhole(cc *config.CoinConf, nIn, nOut int) float64 {
	if cc == nil {
		return float64(estimateFee(nil, nIn, nOut)) / float64(coinScale)
	}
	return float64(estimateFee(cc, nIn, nOut)) / float64(cc.Coin)
}

// isDustNative mirrors C++ isDustAmount(double)
// (xbridgewalletconnectorbtc.cpp:1900-1904): a whole-coin double v is dust when
// int64(v * COIN_native) < int64(dustAmount). dustAmount follows effectiveDust
// (relay-fee-derived, conf MinimumAmount, or the C++ 5460 fallback), nil-safe so a
// missing CoinConf cannot panic the make path.
func isDustNative(v float64, cc *config.CoinConf, relayFee float64, nativeCoin uint64) bool {
	var dust int64
	if relayFee > 0 && cc != nil {
		dust = int64(0.546 * relayFee * float64(cc.Coin))
	} else if cc != nil && cc.MinimumAmount > 0 {
		dust = int64(cc.MinimumAmount)
	} else {
		dust = cppDustFallback
	}
	return int64(v*float64(nativeCoin)) < dust
}

// selectUtxos picks the maker's spendable utxos funding an exact order,
// mirroring C++ App::selectUtxos (src/xbridge/xbridgeapp.cpp:2972-3077). It
// returns the chosen outputs, their total value in XBridge base units
// (utxoAmount), and the prep fee fee1 (XBridge base units).
//
// The address filter applies ONLY in the ideal pass, exactly like C++: utxos
// bucket into gt/lt regardless of address afterward. coinDenomination in C++ is
// TransactionDescr::COIN (1e6), so utxoAmount/fee1 are computed on the XBridge
// scale, not the native chain scale.
func selectUtxos(addr string, outputs []wallet.Utxo, cc *config.CoinConf, requiredAmount uint64) (out []wallet.Utxo, utxoAmount uint64, fee1 uint64, ok bool) {
	feeAmount := func(amt float64, inputs, o int) float64 {
		return amt + minTxFeeWhole(cc, inputs, o) + minTxFeeWhole(cc, 1, 1)
	}

	// selUtxos is C++'s inner fee-utxo selector lambda (:3003-3066).
	selUtxos := func(a []wallet.Utxo, o *[]wallet.Utxo, amt float64) {
		var gt, lt []wallet.Utxo
		minAmount := feeAmount(amt, 1, 3)

		for _, u := range a {
			if u.Value >= minAmount && u.Value < minAmount+(minTxFeeWhole(cc, 1, 3)+minTxFeeWhole(cc, 1, 1))*1000 && (u.Address == addr || addr == "") {
				*o = append(*o, u)
				return
			} else if u.Value >= minAmount {
				gt = append(gt, u)
			} else if u.Value < minAmount {
				lt = append(lt, u)
			}
		}

		// Smallest input > min amount; else the biggest inputs < min amount
		// whose sum is >= min amount; else fail.
		switch {
		case len(gt) == 1:
			*o = append(*o, gt[0])
		case len(gt) > 1:
			sort.Slice(gt, func(i, j int) bool { return gt[i].Value < gt[j].Value })
			*o = append(*o, gt[0])
		case len(lt) < 2:
			return // fail: not enough inputs
		default:
			sort.Slice(lt, func(i, j int) bool { return lt[i].Value > lt[j].Value })
			var sel []wallet.Utxo
			for _, u := range lt {
				sel = append(sel, u)
				running := (minTxFeeWhole(cc, len(sel), 3) + minTxFeeWhole(cc, 1, 1)) * -1
				for _, s := range sel {
					running += s.Value
				}
				if running >= minAmount {
					*o = append(*o, sel...)
					break
				}
			}
		}
	}

	utxos := make([]wallet.Utxo, len(outputs))
	copy(utxos, outputs)
	// Sort available utxos by amount (descending).
	sort.Slice(utxos, func(i, j int) bool { return utxos[i].Value > utxos[j].Value })

	var o []wallet.Utxo
	selUtxos(utxos, &o, float64(requiredAmount)/float64(coinScale))
	if len(o) == 0 {
		return nil, 0, 0, false
	}

	// Sum the selected utxos in XBridge base units.
	for _, u := range o {
		utxoAmount += uint64(u.Value * float64(coinScale))
	}
	fee1 = uint64(minTxFeeWhole(cc, len(o), 3) * float64(coinScale))
	return o, utxoAmount, fee1, true
}

// selectPartialUtxos picks the maker's spendable utxos funding a partial order,
// mirroring C++ App::selectPartialUtxos (src/xbridge/xbridgeapp.cpp:3079-3237).
// All comparisons run on camount() (XBridge base units); the addr argument is
// dropped because C++ never reads it. It returns the chosen outputs, their
// total value (utxoAmount), the accumulated fees, and whether the selection
// exactly matches the required utxo set (exactUtxoMatch) — which determines
// whether a prep split tx is required downstream.
func selectPartialUtxos(outputs []wallet.Utxo, cc *config.CoinConf,
	requiredAmount, requiredUtxoCount, requiredFeePerUtxo uint64,
	requiredPrepTxVouts int, requiredSplitSize, requiredRemainder uint64) (
	out []wallet.Utxo, utxoAmount, fees uint64, exactUtxoMatch, ok bool) {

	utxos := make([]wallet.Utxo, len(outputs))
	copy(utxos, outputs)

	var totalAmountNeeded = int64(requiredAmount) // fees == 0 at this point
	totalExactSplitSizeNeeded := int64((requiredSplitSize + requiredFeePerUtxo) * requiredUtxoCount)
	var totalRemainderNeeded int64
	if requiredRemainder > 0 {
		totalRemainderNeeded = int64(requiredRemainder + requiredFeePerUtxo)
	}
	var requiredPrepTxFees int64
	var usedAmount int64
	idealUtxoCount := 0
	var outputsForUse []wallet.Utxo

	// Find all ideal utxos (those matching split size and fees).
	requiredSplitSizeAmt := int64(requiredSplitSize + requiredFeePerUtxo)
	for i := 0; i < len(utxos); {
		utxo := utxos[i]
		camt := int64(camount(utxo))
		if camt == requiredSplitSizeAmt && usedAmount < totalExactSplitSizeNeeded {
			usedAmount += camt
			fees += requiredFeePerUtxo
			outputsForUse = append(outputsForUse, utxo)
			utxos = append(utxos[:i], utxos[i+1:]...)
			idealUtxoCount++
			continue
		}
		if requiredRemainder > 0 && camt == int64(requiredRemainder) {
			usedAmount += camt
			fees += requiredFeePerUtxo
			outputsForUse = append(outputsForUse, utxo)
			utxos = append(utxos[:i], utxos[i+1:]...)
			continue
		}
		if totalAmountNeeded <= usedAmount {
			break
		}
		i++
	}

	// Exact match of the required utxos; no prep-tx fees needed here.
	totalAmountNeeded = int64(requiredAmount) + int64(fees)
	if (len(outputsForUse) == int(requiredUtxoCount) || (requiredRemainder > 0 && len(outputsForUse) == int(requiredUtxoCount)+1)) && totalAmountNeeded-usedAmount <= 0 {
		return outputsForUse, uint64(usedAmount), fees, true, true
	}

	// Sort available utxos by amount (ascending).
	sort.Slice(utxos, func(i, j int) bool { return camount(utxos[i]) < camount(utxos[j]) })

	if len(outputsForUse) == int(requiredUtxoCount) {
		// Find a utxo matching the exact remainder amount.
		for i := 0; i < len(utxos); {
			utxo := utxos[i]
			totalAmountNeeded = int64(requiredAmount) + int64(fees) + int64(requiredFeePerUtxo)
			if int64(camount(utxo)) == totalAmountNeeded-usedAmount {
				usedAmount += int64(camount(utxo))
				fees += requiredFeePerUtxo
				outputsForUse = append(outputsForUse, utxo)
				utxos = append(utxos[:i], utxos[i+1:]...)
				break
			}
			i++
		}

		totalAmountNeeded = int64(requiredAmount) + int64(fees)
		if len(outputsForUse) >= int(requiredUtxoCount) && totalAmountNeeded-usedAmount <= 0 {
			return outputsForUse, uint64(usedAmount), fees, true, true
		}
	} else {
		// Find enough utxos to cover the remaining partial order amount. A
		// prep tx will be required, so fold those fees in now.
		count := len(outputsForUse) - idealUtxoCount + 1
		for i := 0; i < len(utxos); {
			utxo := utxos[i]
			requiredPrepTxFees = int64(xBridgeIntFromReal(minTxFeeWhole(cc, count, requiredPrepTxVouts)))
			totalAmountNeeded = totalExactSplitSizeNeeded + totalRemainderNeeded + requiredPrepTxFees
			if totalAmountNeeded-usedAmount <= 0 {
				break // reached the required amount
			}
			// Prefer utxos >= the required split size to limit the total count;
			// once the splits are covered the extra inputs cover change.
			if camount(utxo) >= requiredSplitSize+requiredFeePerUtxo {
				usedAmount += int64(camount(utxo))
				outputsForUse = append(outputsForUse, utxo)
				utxos = append(utxos[:i], utxos[i+1:]...)
				count++
				continue
			}
			i++
		}
	}

	// Incorporate prep fees. Ideal utxos (matching size+fees) are not resent,
	// so they don't count toward the prep-tx input fee estimate; assume at least
	// one more utxo is added (+1) to cover the remainder.
	requiredPrepTxFees = int64(xBridgeIntFromReal(minTxFeeWhole(cc, len(outputsForUse)-idealUtxoCount+1, requiredPrepTxVouts)))
	totalAmountNeeded = totalExactSplitSizeNeeded + totalRemainderNeeded + requiredPrepTxFees

	// Find the largest utxo to cover the remainder.
	if usedAmount < totalAmountNeeded {
		for i := 0; i < len(utxos); {
			utxo := utxos[i]
			if int64(camount(utxo))+usedAmount >= totalAmountNeeded {
				usedAmount += int64(camount(utxo))
				outputsForUse = append(outputsForUse, utxo)
				utxos = append(utxos[:i], utxos[i+1:]...)
				break
			}
			i++
		}
	}

	// Find the largest utxos to cover the remainder (descending).
	if usedAmount < totalAmountNeeded {
		sort.Slice(utxos, func(i, j int) bool { return camount(utxos[i]) > camount(utxos[j]) })
		count := len(outputsForUse) - idealUtxoCount + 1
		for i := 0; i < len(utxos); {
			utxo := utxos[i]
			requiredPrepTxFees = int64(xBridgeIntFromReal(minTxFeeWhole(cc, count, requiredPrepTxVouts)))
			totalAmountNeeded = totalExactSplitSizeNeeded + totalRemainderNeeded + requiredPrepTxFees
			if totalAmountNeeded-usedAmount <= 0 {
				break
			}
			usedAmount += int64(camount(utxo))
			outputsForUse = append(outputsForUse, utxo)
			utxos = append(utxos[:i], utxos[i+1:]...)
			count++
		}
	}

	// Final update.
	requiredPrepTxFees = int64(xBridgeIntFromReal(minTxFeeWhole(cc, len(outputsForUse)-idealUtxoCount, requiredPrepTxVouts)))
	totalAmountNeeded = totalExactSplitSizeNeeded + totalRemainderNeeded + requiredPrepTxFees
	fees = uint64(len(outputsForUse)) * requiredFeePerUtxo

	if len(outputsForUse) == 0 || usedAmount-totalAmountNeeded <= 0 {
		return nil, 0, 0, false, false
	}
	return outputsForUse, uint64(usedAmount), fees, false, true
}

// sha256dOrderID builds the XBridge order id exactly as C++ does when writing
// the make body (xbridgeapp.cpp:1729-1763): CHashWriter(SER_GETHASH,0) over
// varstr(from) ∥ varstr(fromCurrency) ∥ u64LE(fromAmount) ∥ varstr(to) ∥
// varstr(toCurrency) ∥ u64LE(toAmount) ∥ u64LE(created) ∥ blockHash ∥
// varstr(signature), then double-SHA256. All component lengths are < 253 bytes,
// so p2p.MarshalVarStr (a Bitcoin VarInt length prefix) reproduces C++
// WriteCompactSize+bytes byte-for-byte.
func sha256dOrderID(from [20]byte, fromCur string, fromAmt uint64,
	to [20]byte, toCur string, toAmt uint64, ts uint64, blockHash [32]byte, sig []byte) [32]byte {
	var b []byte
	b = append(b, p2p.MarshalVarStr(string(from[:]))...)
	b = append(b, p2p.MarshalVarStr(fromCur)...)
	b = binary.LittleEndian.AppendUint64(b, fromAmt)
	b = append(b, p2p.MarshalVarStr(string(to[:]))...)
	b = append(b, p2p.MarshalVarStr(toCur)...)
	b = binary.LittleEndian.AppendUint64(b, toAmt)
	b = binary.LittleEndian.AppendUint64(b, ts)
	b = append(b, blockHash[:]...)
	b = append(b, p2p.MarshalVarStr(string(sig))...)
	h1 := sha256.Sum256(b)
	return sha256.Sum256(h1[:])
}
