package api

// TxCancelReason is the XBridge wire cancel/reject reason, a 1:1 port of the
// C++ enum (xbridgepacket.h:21-48). It rides as a bare uint32 on the wire; the
// ordinals below mirror the C++ enum exactly.
type TxCancelReason uint32

const (
	crUnknown         TxCancelReason = 0
	crBadSettings     TxCancelReason = 1
	crUserRequest     TxCancelReason = 2
	crNoMoney         TxCancelReason = 3
	crBadUtxo         TxCancelReason = 4
	crDust            TxCancelReason = 5
	crRpcError        TxCancelReason = 6
	crNotSigned       TxCancelReason = 7
	crNotAccepted     TxCancelReason = 8
	crRollback        TxCancelReason = 9
	crRpcRequest      TxCancelReason = 10
	crXbridgeRejected TxCancelReason = 11
	crInvalidAddress  TxCancelReason = 12
	crBlocknetError   TxCancelReason = 13
	crBadADepositTx   TxCancelReason = 14
	crBadBDepositTx   TxCancelReason = 15
	crTimeout         TxCancelReason = 16
	crBadLockTime     TxCancelReason = 17
	crBadALockTime    TxCancelReason = 18
	crBadBLockTime    TxCancelReason = 19
	crBadAUtxo        TxCancelReason = 20
	crBadBUtxo        TxCancelReason = 21
	crBadARefundTx    TxCancelReason = 22
	crBadBRefundTx    TxCancelReason = 23
	crBadFeeTx        TxCancelReason = 24
)

// TxCancelReasonText renders a cancel/reject reason exactly the way C++ does
// (TxCancelReasonText, xbridgeapp.cpp:4052-4107) — INCLUDING the two upstream
// rendering bugs, reproduced for parity:
//   - crBadSettings (1) renders as "crUnknown" (C++ :4055-4056);
//   - crUnknown (0) and every out-of-enum value render as "crNone"
//     (C++ :4103-4105 — the crUnknown case falls through to default).
//
// Every other value renders as "cr" + its name. C++ uses it for the
// cancel_reason order-log field (xbridgesession.cpp:3312,3536;
// xbridgeapp.cpp:4041); the on-wire value remains the raw uint32.
func TxCancelReasonText(reason uint32) string {
	switch TxCancelReason(reason) {
	case crBadSettings:
		return "crUnknown"
	case crUserRequest:
		return "crUserRequest"
	case crNoMoney:
		return "crNoMoney"
	case crBadUtxo:
		return "crBadUtxo"
	case crDust:
		return "crDust"
	case crRpcError:
		return "crRpcError"
	case crNotSigned:
		return "crNotSigned"
	case crNotAccepted:
		return "crNotAccepted"
	case crRollback:
		return "crRollback"
	case crRpcRequest:
		return "crRpcRequest"
	case crXbridgeRejected:
		return "crXbridgeRejected"
	case crInvalidAddress:
		return "crInvalidAddress"
	case crBlocknetError:
		return "crBlocknetError"
	case crBadADepositTx:
		return "crBadADepositTx"
	case crBadBDepositTx:
		return "crBadBDepositTx"
	case crTimeout:
		return "crTimeout"
	case crBadLockTime:
		return "crBadLockTime"
	case crBadALockTime:
		return "crBadALockTime"
	case crBadBLockTime:
		return "crBadBLockTime"
	case crBadAUtxo:
		return "crBadAUtxo"
	case crBadBUtxo:
		return "crBadBUtxo"
	case crBadARefundTx:
		return "crBadARefundTx"
	case crBadBRefundTx:
		return "crBadBRefundTx"
	case crBadFeeTx:
		return "crBadFeeTx"
	case crUnknown:
		fallthrough
	default:
		return "crNone"
	}
}
