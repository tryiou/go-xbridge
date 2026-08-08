package api

// Locktime-drift validation for the accept/take path.
//
// This mirrors C++ BtcWalletConnector::acceptableLockTimeDrift
// (xbridgewalletconnectorbtc.cpp:2331) and its xbridgesession.cpp:2464 call
// site. C++ computes locktimes as BLOCK HEIGHTS (lockTime() == currentBlock +
// target/blockTime), which is exactly what Go's swapCtx.computeLockTimeFor
// does, so the arithmetic ports directly. The constants mirror
// xbridgewallet.h / script.h.
const (
	lockTimeThreshold       = 500_000_000 // LOCKTIME_THRESHOLD (script.h:39)
	xLockTimeDriftSeconds   = 900         // XLOCKTIME_DRIFT_SECONDS = XTAKER_LOCKTIME_TARGET_SECONDS/2 (1800/2)
	xMaxLockTimeDriftBlocks = 4           // XMAX_LOCKTIME_DRIFT_BLOCKS (xbridgewallet.h:97)
)

// acceptableLockTimeDrift reports whether the counterparty's deposit lockTime
// (block height, theirLT) is acceptable relative to our own expectation
// (ourLT), both for the SAME deposit role on the SAME coin.
//
//	C++: lt == 0 || lt >= LOCKTIME_THRESHOLD || lckTime >= LOCKTIME_THRESHOLD -> false
//	    diff  = int64(lt) - int64(lckTime)                               // blocks
//	    drift = max(XLOCKTIME_DRIFT_SECONDS, XMAX_LOCKTIME_DRIFT_BLOCKS*blockTime)  // seconds
//	    diff*blockTime <= drift                                         // blocks*sec <= sec
//
// The check is one-sided on purpose: a counterparty lockTime far BELOW ours
// would let them refund before us, so we reject when theirs is too small;
// theirs being larger is allowed.
func acceptableLockTimeDrift(ourLT, theirLT, blockTime uint32) bool {
	if ourLT == 0 || ourLT >= lockTimeThreshold || theirLT >= lockTimeThreshold {
		return false
	}
	diff := int64(ourLT) - int64(theirLT)
	drift := int64(xLockTimeDriftSeconds)
	if mb := int64(xMaxLockTimeDriftBlocks) * int64(blockTime); mb > drift {
		drift = mb
	}
	return diff*int64(blockTime) <= drift
}

// blockTimeFor returns the configured seconds-per-block for a coin (default 60).
// It runs on a worker in the two-phase handshake, so it reads config only.
func (c swapCtx) blockTimeFor(cur string) uint32 {
	bt := uint32(60)
	if cc := c.conf(cur); cc != nil && cc.BlockTime > 0 {
		bt = uint32(cc.BlockTime)
	}
	return bt
}
