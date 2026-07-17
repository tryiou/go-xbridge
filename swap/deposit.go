package swap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"golang.org/x/crypto/ripemd160"

	"xbridge-go/coins"
	"xbridge-go/wallet"
)

// Role identifies which side of a swap the local node plays.
type Role int

const (
	RoleMaker Role = iota // A — created the order
	RoleTaker             // B — joined it
)

// DepositSpec describes one participant's HTLC deposit, ported from the C++
// deposit blob carried on xbcTransactionInit (command 8) and built by
// XBridgeWalletConnector::createDepositUnlockScript. It locks Amount of
// Currency into a P2SH HTLC:
//   - the counterparty claims it via the ELSE branch by revealing the 33-byte
//     Secret preimage (HASH160(Secret) == SecretHash);
//   - the depositor refunds it via the IF branch (CLTV lockTime paying the
//     depositor's key) if the swap does not complete.
//
// The depositor chooses Secret; revealing SecretHash to the counterparty (in
// the init packet) lets them build the matching claim.
type DepositSpec struct {
	Currency        string
	Amount          uint64
	DepositorPub    [33]byte
	CounterpartyPub [33]byte
	Secret          [33]byte // depositor-chosen 33-byte preimage (local side only)
	Hash            [20]byte // HASH160(Secret); set directly for the counterparty (whose Secret is unknown)
	LockTime        uint32
	// TxVersion is the transaction version to stamp on the deposit (and, by
	// extension, its refund/claim spends). In C++ this is the per-coin
	// <COIN>.TxVersion read from xbridge.conf (default 1), NOT a hardcoded
	// constant — so it must come from the coin config, not be fixed here.
	TxVersion int
}

// SecretHash returns the HTLC hashlock target. The depositor computes it from
// Secret; the counterparty adopts it directly (they only learn the hash).
func (d *DepositSpec) SecretHash() [20]byte {
	if d.Hash != [20]byte{} {
		return d.Hash
	}
	h1 := sha256.Sum256(d.Secret[:])
	r := ripemd160.New()
	r.Write(h1[:])
	var out [20]byte
	copy(out[:], r.Sum(nil))
	return out
}

// RedeemScript builds the HTLC redeem script via coins.BuildDepositUnlockScript.
func (d *DepositSpec) RedeemScript() []byte {
	sh := d.SecretHash()
	return coins.BuildDepositUnlockScript(d.DepositorPub[:], d.CounterpartyPub[:], sh[:], d.LockTime)
}

// P2SHScript returns the deposit output script OP_HASH160 <HASH160(redeem)> OP_EQUAL.
func (d *DepositSpec) P2SHScript() []byte {
	return coins.BuildP2SHScript(coins.KeyID(d.RedeemScript()))
}

// BuildDepositTx builds the unsigned deposit transaction that locks Amount into
// the P2SH HTLC, spending funding UTXOs and returning change to changeAddr.
// Input sequence is set below 0xffffffff so the CLTV refund branch is
// spendable. The deposit tx itself has LockTime 0 so it confirms immediately;
// the CLTV (d.LockTime) is enforced on the *refund spend*, not the deposit.
// Legacy (P2PKH) change only — native segwit change is a follow-up.
func (d *DepositSpec) BuildDepositTx(c coins.Coin, funding []wallet.Utxo, changeAddr [20]byte, fee uint64) (*coins.Tx, error) {
	if len(funding) == 0 {
		return nil, errors.New("swap: no funding UTXOs for deposit")
	}
	var total uint64
	for _, u := range funding {
		total += u.Amount
	}
	if total < d.Amount+fee {
		return nil, errors.New("swap: funding insufficient for deposit + fee")
	}
	// Transaction version is the per-coin <COIN>.TxVersion (C++ default 1). A
	// zero/unset value falls back to 1 to match C++'s config default.
	ver := d.TxVersion
	if ver <= 0 {
		ver = 1
	}
	tx := &coins.Tx{Version: int32(ver), LockTime: 0}
	for _, u := range funding {
		h, err := reverseHashHex(u.TxID)
		if err != nil {
			return nil, err
		}
		tx.Inputs = append(tx.Inputs, coins.TxIn{
			PrevOut:  coins.OutPoint{Hash: h, Index: u.Vout},
			Sequence: 0xfffffffe, // < 0xffffffff to enable CLTV
		})
	}
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: d.Amount, ScriptPubKey: d.P2SHScript()})
	if change := total - d.Amount - fee; change > 0 {
		tx.Outputs = append(tx.Outputs, coins.TxOut{Value: change, ScriptPubKey: coins.BuildP2PKHScript(changeAddr)})
	}
	return tx, nil
}

// SignInput signs funding input idx of the deposit tx with priv (the funding
// UTXO is the depositor's own P2PKH output), returning the DER+SIGHASH_ALL sig.
// prevScript is that funding output's scriptPubKey.
func (d *DepositSpec) SignInput(tx *coins.Tx, idx int, prevScript []byte, priv []byte) ([]byte, error) {
	return coins.SignTxInput(tx, idx, prevScript, priv)
}

// RefundScriptSig assembles the scriptSig to claim the deposit's refund (IF)
// branch: <sig> <DepositorPub> OP_1 <redeemScript>.
func (d *DepositSpec) RefundScriptSig(sig []byte) []byte {
	return coins.BuildRefundScriptSig(sig, d.DepositorPub[:], d.RedeemScript())
}

// reverseHashHex converts a display-order txid hex into the 32-byte internal
// (little-endian) outpoint form, matching api.reverseTxidHex / wallet.revHashHex.
func reverseHashHex(s string) ([32]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return [32]byte{}, errors.New("swap: bad txid " + s)
	}
	var out [32]byte
	for i := 0; i < 32; i++ {
		out[i] = b[31-i]
	}
	return out, nil
}
