package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/wallet"
)

// Service-node fee the taker pays on the Blocknet chain, mirroring C++
// WalletParam::serviceNodeFee (.015, xbridgewallet.h:119). It is a hardcoded
// connector default in C++ — NOT an xbridge.conf key — so the port hardcodes the
// same value.
const serviceNodeFeeReal = 0.015

// feeTxRelayPerByte is the hardcoded BLOCK fee rate C++ createFeeTransaction
// uses (blockFeePerByte = 40/COIN, xbridgeapp.cpp:2257). It is deliberately
// independent of the conf FeePerByte / estimateFee model.
const feeTxRelayPerByte = 40.0 / 100000000

// minFeeChangeDust is the smallest native change output C++ keeps; smaller
// change is folded back into the fee (bitcoinrpcconnector.cpp:206).
const minFeeChangeDust = 5460

// maxOrderInfoBytes mirrors C++ maxBytes = nMaxDatacarrierBytes-3 = 157
// (xbridgeapp.cpp:2207; MAX_OP_RETURN_RELAY = 160, script/standard.h:34).
const maxOrderInfoBytes = 160 - 3

// errOrderInfoOverflow is returned by feeOrderInfo when the assembled order-info
// payload exceeds maxOrderInfoBytes. C++ reverts the take and reports
// INVALID_ONCHAIN_HISTORY on this overflow (xbridgeapp.cpp:2226-2228), so the
// caller must not fold it into the generic INSUFFICIENT_FUNDS fee-prep errors.
var errOrderInfoOverflow = errors.New("fee order info exceeds max bytes")

// feeOrderInfo builds the OP_RETURN order-info JSON carried by the service-node
// fee tx, mirroring C++ xbridgeapp.cpp:2209-2229: the base list
// ["", fromCur, fromAmt, toCur, toAmt] is sized, the 64-hex order id truncated
// only if it would overflow maxBytes, then prepended. json_spirit's compact
// write_string is byte-identical to encoding/json for these scalar types, so
// the result is the exact wire payload. For valid input the id never truncates
// (max base size is 68 < 93 = 157-64); the truncation path is implemented for
// fidelity. When the base already exceeds maxBytes the leftover underflows: Go
// keeps the full id and lets the final size check report errOrderInfoOverflow
// (C++ INVALID_ONCHAIN_HISTORY, :2226-2228). This is a deliberate safe
// deviation — C++ computes leftOver as a size_t and passes it to
// std::string::erase(pos > size), which throws std::out_of_range (libstdc++
// basic_string::_M_check) and terminates the daemon. Unreachable for valid
// input (max base size is 68 < 93 = 157-64).
func feeOrderInfo(id [32]byte, fromCur string, fromAmt uint64, toCur string, toAmt uint64) ([]byte, error) {
	base := []any{"", fromCur, fromAmt, toCur, toAmt}
	strInfo, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	orderID := orderIDString(id)
	if len(strInfo)+len(orderID) > maxOrderInfoBytes {
		switch leftOver := maxOrderInfoBytes - len(strInfo); {
		case leftOver > 0:
			// leftOver < len(orderID) is guaranteed: the enclosing guard
			// len(strInfo)+len(orderID) > maxOrderInfoBytes means
			// len(orderID) > maxOrderInfoBytes-len(strInfo) = leftOver.
			orderID = orderID[:leftOver]
		case leftOver == 0:
			orderID = ""
		case leftOver < 0:
			// Base already over maxBytes: C++'s size_t leftover underflows and
			// std::string::erase(pos > size) throws out_of_range, killing the
			// daemon. Go deliberately keeps the full id so the size check below
			// reports errOrderInfoOverflow instead.
		}
	}
	full := []any{orderID, fromCur, fromAmt, toCur, toAmt}
	out, err := json.Marshal(full)
	if err != nil {
		return nil, err
	}
	if len(out) > maxOrderInfoBytes {
		return nil, errOrderInfoOverflow
	}
	return out, nil
}

// estFeeBlock returns the fee estimate C++ createFeeTransaction uses
// (bitcoinrpcconnector.cpp:96-98): (192*inputs + 34*outputs) * feePerByte with
// feePerByte hardcoded to 40/1e8.
func estFeeBlock(inputs, outputs int) float64 {
	return float64(192*inputs+34*outputs) * feeTxRelayPerByte
}

// selectFeeUtxos picks the p2pkh utxos funding the service-node fee tx,
// mirroring the C++ fee-utxo selector inside createFeeTransaction
// (bitcoinrpcconnector.cpp:104-168): an ideal single input in
// [amt+estFee(1,3), amt+estFee(1,3)+estFee(1,3)*100), else the smallest input
// above the minimum, else the largest inputs below it that sum (minus fees) to
// the minimum. No address filter. amt is the whole-coin fee amount (.015).
func selectFeeUtxos(a []wallet.Utxo, amt float64) ([]wallet.Utxo, bool) {
	utxos := make([]wallet.Utxo, len(a))
	copy(utxos, a)
	sort.Slice(utxos, func(i, j int) bool { return utxos[i].Value > utxos[j].Value })

	minAmount := amt + estFeeBlock(1, 3)
	var gt, lt []wallet.Utxo
	for _, u := range utxos {
		switch {
		case u.Value >= minAmount && u.Value < minAmount+estFeeBlock(1, 3)*100:
			return []wallet.Utxo{u}, true
		case u.Value >= minAmount:
			gt = append(gt, u)
		default:
			lt = append(lt, u)
		}
	}

	switch {
	case len(gt) == 1:
		return gt, true
	case len(gt) > 1:
		sort.Slice(gt, func(i, j int) bool { return gt[i].Value < gt[j].Value })
		return gt[:1], true
	case len(lt) < 2:
		return nil, false
	default:
		sort.Slice(lt, func(i, j int) bool { return lt[i].Value > lt[j].Value })
		var sel []wallet.Utxo
		for _, u := range lt {
			sel = append(sel, u)
			running := 0.0
			for _, s := range sel {
				running += s.Value
			}
			running -= estFeeBlock(len(sel), 3)
			if running >= minAmount {
				out := make([]wallet.Utxo, len(sel))
				copy(out, sel)
				return out, true
			}
		}
		return nil, false
	}
}

// isP2PKH25 reports whether scriptHex is a 25-byte P2PKH output script
// (76a914…88ac), mirroring rpc::unspentP2PKH's only-supported-script filter
// (bitcoinrpcconnector.cpp:282-284).
func isP2PKH25(scriptHex string) bool {
	b, err := hex.DecodeString(scriptHex)
	if err != nil || len(b) != 25 {
		return false
	}
	return b[0] == 0x76 && b[1] == 0xa9 && b[2] == 0x14 && b[23] == 0x88 && b[24] == 0xac
}

// buildServiceNodeFeeTx constructs, signs, and returns the BLOCK service-node
// fee transaction plus its selected inputs, mirroring C++
// rpc::createFeeTransaction (bitcoinrpcconnector.cpp:79-269). dest is the
// service node's registry payment address; data is the order-info OP_RETURN
// payload; avail is the spendable p2pkh BLOCK utxo set (already filtered by
// unspentP2PKH and the lock exclusion). Every failure maps to INSUFFICIENT_FUNDS
// (C++ returns INSUFFICIENT_FUNDS for any fee-prep failure, xbridgeapp.cpp:2240-2264).
func buildServiceNodeFeeTx(conn wallet.Connector, blkCoin coins.Coin, blkConf *config.CoinConf, dest [20]byte, data []byte, avail []wallet.Utxo) (rawHex string, inputs []wallet.Utxo, rerr *rpcError) {
	sel, ok := selectFeeUtxos(avail, serviceNodeFeeReal)
	if !ok {
		return "", nil, makeError(errInsufficientFunds, "dxTakeOrder", "not accepting order, insufficient BLOCK funds for service node fee payment")
	}
	if blkConf == nil {
		// A configured BLOCK connector implies its [BLOCK] conf exists, but a
		// conf reload or a manually-constructed connector map could drop it;
		// the fee amount scales by Coin so refuse rather than nil-deref
		// (C++ maps any fee-prep failure to INSUFFICIENT_FUNDS).
		return "", nil, makeError(errInsufficientFunds, "dxTakeOrder", "not accepting order, BLOCK conf missing")
	}

	native := float64(blkConf.Coin)
	inputAmt := 0.0
	for _, u := range sel {
		inputAmt += u.Value
	}
	feeAmt := estFeeBlock(len(sel), 3)
	changeAmt := uint64((inputAmt - serviceNodeFeeReal - feeAmt) * native)

	tx := &coins.Tx{Version: 1}
	if blkConf.TxVersion != 0 {
		tx.Version = int32(blkConf.TxVersion)
	}
	tx.WithTime = blkCoin.TxWithTimeField
	var prevTxs []wallet.PrevTx
	for _, u := range sel {
		hash, err := reverseTxidHex(u.TxID)
		if err != nil {
			return "", nil, makeError(errInsufficientFunds, "dxTakeOrder", "not accepting order, bad fee input txid: "+u.TxID)
		}
		tx.Inputs = append(tx.Inputs, coins.TxIn{PrevOut: coins.OutPoint{Hash: hash, Index: u.Vout}, Sequence: 0xffffffff})
		prevTxs = append(prevTxs, wallet.PrevTx{TxID: u.TxID, Vout: u.Vout, ScriptPubKey: u.ScriptPubKey, Amount: u.Amount})
	}

	// vouts: [OP_RETURN data, fee→registry payment addr, change?].
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: 0, ScriptPubKey: coins.BuildOpReturnScript(data)})
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: uint64(serviceNodeFeeReal * native), ScriptPubKey: coins.BuildP2PKHScript(dest)})
	if changeAmt >= minFeeChangeDust {
		changeScript, e := legacyOutputScript(blkCoin, sel[0].Address)
		if e != nil {
			return "", nil, e
		}
		tx.Outputs = append(tx.Outputs, coins.TxOut{Value: changeAmt, ScriptPubKey: changeScript})
	}

	unsigned := hex.EncodeToString(tx.Serialize())
	signed, complete, err := conn.SignRawTransaction(unsigned, prevTxs)
	if err != nil || !complete {
		return "", nil, makeError(errInsufficientFunds, "dxTakeOrder", "not accepting order, failed to prepare the service node fee")
	}
	return signed, sel, nil
}
