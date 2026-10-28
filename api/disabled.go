package api

import (
	"go-xbridge/config"

	xlog "go-xbridge/log"
)

// ReportDisabled logs every coin config.Load disabled, once per call, at Error
// so an operator sees exactly which market is missing and why. Disabled coins
// are absent from the parsed Coins map, so they are never admitted, get no
// connector, and never appear in dxGetLocalTokens (C++ xbridgeapp.cpp:1015-1025).
//
// The two callers are the daemon's only load points — startup and
// dxLoadXBridgeConf — and both go through here so the message and its keys
// cannot drift apart (that string is the grep target in either engine's log).
func ReportDisabled(conf *config.Conf) {
	if conf == nil {
		return
	}
	for _, d := range conf.Disabled {
		xlog.Error("coin disabled by config", "coin", d.Coin, "reason", d.Reason)
	}
}
