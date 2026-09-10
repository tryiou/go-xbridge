package api

import (
	"fmt"

	xlog "go-xbridge/log"
)

// Swap transcript entries (the dedicated log-tx/xbridgep2p_YYYYMMDD.log,
// Core util/txlog.cpp analog). Every helper logs the DISPLAY order id
// (orderIDString, the uint256 hex the user pastes into RPCs — never the
// internal little-endian key) plus the full raw hex needed for a manual
// refund with the deposit currency's own wallet (sendrawtransaction).
//
// Amounts are logged in the coin's raw base units (exact; the same units the
// refund builder consumes) — scale follows the coin's COIN (1e8 for BTC-family,
// 1e6 for PIVX-class). Private keys and HTLC preimages are NEVER passed here
// (see log/txlog.go); only hashes, txids, locktimes, and hexes.

// txLogDeposit records a broadcast deposit and its pre-signed refund (Core
// xbridgesession.cpp:2121-2125 deposit + :2168-2171 refund analog). role is
// "A" (maker) or "B" (taker).
func txLogDeposit(id [32]byte, role, srcCur string, srcAmt uint64, dstCur string, dstAmt uint64, lockTime uint32, depositHex, refundHex string) {
	disp := orderIDString(id)
	xlog.TxLog(disp,
		fmt.Sprintf("deposit transaction for order %s (submit manually using sendrawtransaction) %d %s -> %d %s using locktime %d",
			disp, srcAmt, srcCur, dstAmt, dstCur, lockTime),
		depositHex)
	xlog.TxLog(disp,
		fmt.Sprintf("refund transaction for order %s role %s locktime %d", disp, role, lockTime),
		refundHex)
}

// txLogDepositBuilt records a BUILT but not yet broadcast deposit and its
// pre-signed refund (Core logs both hexes at build time, before broadcast:
// xbridgesession.cpp:2121-2125/:2168-2171). Labeled unbroadcast: if no
// broadcast line follows for the order, the deposit may never have reached
// the chain — check the deposit txid on chain before broadcasting the refund
// (a refund for a nonexistent deposit is a harmless wallet reject, but the
// check comes first).
func txLogDepositBuilt(id [32]byte, role, srcCur string, srcAmt uint64, dstCur string, dstAmt uint64, lockTime uint32, depositHex, refundHex string) {
	disp := orderIDString(id)
	xlog.TxLog(disp,
		fmt.Sprintf("deposit built (NOT YET BROADCAST) for order %s %d %s -> %d %s using locktime %d (submit manually using sendrawtransaction only if broadcast; check txid on chain first)",
			disp, srcAmt, srcCur, dstAmt, dstCur, lockTime),
		depositHex)
	xlog.TxLog(disp,
		fmt.Sprintf("refund pre-signed (unbroadcast deposit) for order %s role %s locktime %d", disp, role, lockTime),
		refundHex)
}

// txLogClaim records a broadcast claim payTx (Core xbridgesession.cpp:3992-3995
// analog). The claim reveals the HTLC secret on-chain by design (the taker
// recovers it from this very hex), so a broadcast payTx is public chain data,
// not private material — safe to record. Per-trade M privkeys never reach any
// txlog helper (see log/txlog.go).
func txLogClaim(id [32]byte, role, cur, payTxID, payHex string) {
	disp := orderIDString(id)
	xlog.TxLog(disp,
		fmt.Sprintf("redeem counterparty deposit for order %s role %s %s (submit manually using sendrawtransaction) paytx %s",
			disp, role, cur, payTxID),
		payHex)
}

// txLogClaimBuilt records a BUILT but not yet broadcast claim (same
// unbroadcast labeling as txLogDepositBuilt; the payTx reveals the secret
// only once broadcast).
func txLogClaimBuilt(id [32]byte, role, cur, payHex string) {
	disp := orderIDString(id)
	xlog.TxLog(disp,
		fmt.Sprintf("claim built (NOT YET BROADCAST) for order %s role %s %s (submit manually using sendrawtransaction only if broadcast)",
			disp, role, cur),
		payHex)
}

// txLogRefund records a broadcast refund with its chain txid (Core
// redeemOrderDeposit success analog). The refund hex itself was logged at
// pre-sign time by txLogDeposit; this entry ties it to the on-chain txid.
func txLogRefund(id [32]byte, cur string, lockTime uint32, txid string) {
	disp := orderIDString(id)
	xlog.TxLog(disp,
		fmt.Sprintf("refund broadcast for order %s %s txid %s locktime %d", disp, cur, txid, lockTime))
}
