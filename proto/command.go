package proto

// XBridgeCommand enumerates the XBridge P2P message types.
// Values mirror src/xbridge/xbridgepacket.h (enum XBridgeCommand).
type XBridgeCommand uint32

const (
	XbcInvalid                XBridgeCommand = 0
	XbcXChatMessage           XBridgeCommand = 2
	XbcTransaction            XBridgeCommand = 3
	XbcPendingTransaction     XBridgeCommand = 4
	XbcTransactionAccepting   XBridgeCommand = 5
	XbcTransactionHold        XBridgeCommand = 6
	XbcTransactionHoldApply   XBridgeCommand = 7
	XbcTransactionInit        XBridgeCommand = 8
	XbcTransactionInitialized XBridgeCommand = 9
	XbcTransactionCreateA     XBridgeCommand = 10
	XbcTransactionCreatedA    XBridgeCommand = 11
	XbcTransactionCreateB     XBridgeCommand = 12
	XbcTransactionCreatedB    XBridgeCommand = 13
	XbcTransactionConfirmA    XBridgeCommand = 18
	XbcTransactionConfirmedA  XBridgeCommand = 19
	XbcTransactionConfirmB    XBridgeCommand = 20
	XbcTransactionConfirmedB  XBridgeCommand = 21
	XbcTransactionCancel      XBridgeCommand = 22
	XbcTransactionFinished    XBridgeCommand = 24
	XbcTransactionReject      XBridgeCommand = 26
	XbcServicesPing           XBridgeCommand = 50
)

var commandNames = map[XBridgeCommand]string{
	XbcInvalid:                "xbcInvalid",
	XbcXChatMessage:           "xbcXChatMessage",
	XbcTransaction:            "xbcTransaction",
	XbcPendingTransaction:     "xbcPendingTransaction",
	XbcTransactionAccepting:   "xbcTransactionAccepting",
	XbcTransactionHold:        "xbcTransactionHold",
	XbcTransactionHoldApply:   "xbcTransactionHoldApply",
	XbcTransactionInit:        "xbcTransactionInit",
	XbcTransactionInitialized: "xbcTransactionInitialized",
	XbcTransactionCreateA:     "xbcTransactionCreateA",
	XbcTransactionCreatedA:    "xbcTransactionCreatedA",
	XbcTransactionCreateB:     "xbcTransactionCreateB",
	XbcTransactionCreatedB:    "xbcTransactionCreatedB",
	XbcTransactionConfirmA:    "xbcTransactionConfirmA",
	XbcTransactionConfirmedA:  "xbcTransactionConfirmedA",
	XbcTransactionConfirmB:    "xbcTransactionConfirmB",
	XbcTransactionConfirmedB:  "xbcTransactionConfirmedB",
	XbcTransactionCancel:      "xbcTransactionCancel",
	XbcTransactionFinished:    "xbcTransactionFinished",
	XbcTransactionReject:      "xbcTransactionReject",
	XbcServicesPing:           "xbcServicesPing",
}

func (c XBridgeCommand) String() string {
	if s, ok := commandNames[c]; ok {
		return s
	}
	return "xbcUnknown"
}
