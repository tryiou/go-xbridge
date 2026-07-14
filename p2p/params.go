package p2p

// Network message magics from src/chainparams.cpp.
var (
	MainnetMagic = [4]byte{0xa1, 0xa0, 0xa2, 0xa3} // port 41412
	TestnetMagic = [4]byte{0x45, 0x76, 0x65, 0xbb} // port 41474
	StagingMagic = [4]byte{0xa1, 0xcf, 0x7e, 0xac} // port 41489
)

// XBridgeNetCommand is the Bitcoin P2P message command under which XBridge
// packets are exchanged.
//
// VERIFY against src/net.h NetMsgType::XBRIDGE — the C++ code uses
// msgMaker.Make(NetMsgType::XBRIDGE, msg). Assumed to be the 12-byte,
// null-padded ASCII string "xbridge".
const XBridgeNetCommand = "xbridge"
