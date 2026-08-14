package api

import "testing"

// TestTxCancelReasonEnumOrdinals pins the wire ordinals to the C++ enum
// (xbridgepacket.h:21-48) so a reorder/retag can never silently change the
// bytes that ride on a Cancel/Reject packet.
func TestTxCancelReasonEnumOrdinals(t *testing.T) {
	cases := []struct {
		val TxCancelReason
		ord uint32
	}{
		{crUnknown, 0}, {crBadSettings, 1}, {crUserRequest, 2}, {crNoMoney, 3},
		{crBadUtxo, 4}, {crDust, 5}, {crRpcError, 6}, {crNotSigned, 7},
		{crNotAccepted, 8}, {crRollback, 9}, {crRpcRequest, 10}, {crXbridgeRejected, 11},
		{crInvalidAddress, 12}, {crBlocknetError, 13}, {crBadADepositTx, 14}, {crBadBDepositTx, 15},
		{crTimeout, 16}, {crBadLockTime, 17}, {crBadALockTime, 18}, {crBadBLockTime, 19},
		{crBadAUtxo, 20}, {crBadBUtxo, 21}, {crBadARefundTx, 22}, {crBadBRefundTx, 23},
		{crBadFeeTx, 24},
	}
	for _, c := range cases {
		if got := uint32(c.val); got != c.ord {
			t.Errorf("%v = %d, want %d", c.val, got, c.ord)
		}
	}
}

// TestTxCancelReasonText locks in the exact C++ text table (TxCancelReasonText,
// xbridgeapp.cpp:4052-4107) INCLUDING the two upstream rendering bugs:
// crBadSettings → "crUnknown" (:4055-4056) and crUnknown/out-of-enum →
// "crNone" (:4103-4105).
func TestTxCancelReasonText(t *testing.T) {
	cases := []struct {
		reason uint32
		want   string
	}{
		{0, "crNone"},             // crUnknown falls to default (C++ bug 2)
		{1, "crUnknown"},          // crBadSettings renders as crUnknown (C++ bug 1)
		{2, "crUserRequest"},      // crUserRequest
		{3, "crNoMoney"},          // crNoMoney
		{4, "crBadUtxo"},          // crBadUtxo
		{5, "crDust"},             // crDust
		{6, "crRpcError"},         // crRpcError
		{7, "crNotSigned"},        // crNotSigned
		{8, "crNotAccepted"},      // crNotAccepted
		{9, "crRollback"},         // crRollback
		{10, "crRpcRequest"},      // crRpcRequest
		{11, "crXbridgeRejected"}, // crXbridgeRejected
		{12, "crInvalidAddress"},  // crInvalidAddress
		{13, "crBlocknetError"},   // crBlocknetError
		{14, "crBadADepositTx"},   // crBadADepositTx
		{15, "crBadBDepositTx"},   // crBadBDepositTx
		{16, "crTimeout"},         // crTimeout
		{17, "crBadLockTime"},     // crBadLockTime
		{18, "crBadALockTime"},    // crBadALockTime
		{19, "crBadBLockTime"},    // crBadBLockTime
		{20, "crBadAUtxo"},        // crBadAUtxo
		{21, "crBadBUtxo"},        // crBadBUtxo
		{22, "crBadARefundTx"},    // crBadARefundTx
		{23, "crBadBRefundTx"},    // crBadBRefundTx
		{24, "crBadFeeTx"},        // crBadFeeTx
		{25, "crNone"},            // out-of-enum falls to default
		{0xFFFFFFFF, "crNone"},    // out-of-enum falls to default
	}
	for _, c := range cases {
		if got := TxCancelReasonText(c.reason); got != c.want {
			t.Errorf("TxCancelReasonText(%d) = %q, want %q", c.reason, got, c.want)
		}
	}
}
