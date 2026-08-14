package config

import "fmt"

// XBridge locktime / drift constants — faithful mirrors of the C++ constexprs
// in src/xbridge/xbridgewallet.h:96-102. They are the single source of truth
// for the admission gates (Admit) and the swap locktime math (api package);
// nothing here is conf-driven.
const (
	// XMinLockTimeBlocks is XMIN_LOCKTIME_BLOCKS.
	XMinLockTimeBlocks = 6
	// XMaxLockTimeDriftBlocks is XMAX_LOCKTIME_DRIFT_BLOCKS (must be less
	// than XMinLockTimeBlocks).
	XMaxLockTimeDriftBlocks = 4
	// XMakerLocktimeTargetSeconds is XMAKER_LOCKTIME_TARGET_SECONDS (2 hours).
	XMakerLocktimeTargetSeconds = 2 * 60 * 60
	// XTakerLocktimeTargetSeconds is XTAKER_LOCKTIME_TARGET_SECONDS (30 mins;
	// must be less than the maker target).
	XTakerLocktimeTargetSeconds = 30 * 60
	// XSlowTakerLocktimeTargetSeconds is XSLOW_TAKER_LOCKTIME_TARGET_SECONDS
	// (1 hour; must be less than the maker target).
	XSlowTakerLocktimeTargetSeconds = 60 * 60
	// XSlowBlockTimeSeconds is XSLOW_BLOCKTIME_SECONDS.
	XSlowBlockTimeSeconds = 600
	// XLocktimeDriftSeconds is XLOCKTIME_DRIFT_SECONDS
	// (= XTakerLocktimeTargetSeconds / 2, 15 minutes).
	XLocktimeDriftSeconds = XTakerLocktimeTargetSeconds / 2
)

// supportedCreateTxMethods is the method set go-xbridge genuinely implements.
// C++ also dispatches BCD/PART/STEALTH/XST (xbridgeapp.cpp:1052-1086), but
// those transaction formats are not portable to a thin client and are deferred
// (CRYPTO-F98 for PART, CRYPTO-F99 for BCD, documented; STEALTH/XST appear in
// no live manifest conf); admitting them would build BTC-format transactions
// for a chain that rejects them. Go therefore refuses them at admission instead
// of broadcasting malformed transactions. LTC is accepted as a deliberate Go
// extension: C++ has no LTC connector dispatch (method "LTC" would be
// "unknown" there), but the Go codec supports it (coins/coin.go) and it is
// harmless — no live conf uses the LTC method (LTC confs use CreateTxMethod=BTC).
var supportedCreateTxMethods = map[string]bool{
	"BTC":     true,
	"SYS":     true,
	"LTC":     true,
	"DGB":     true,
	"BCH":     true,
	"BTG":     true,
	"DEVAULT": true,
}

// Admit applies the per-wallet admission gates a coin must pass before its
// connector is activated, mirroring App::updateActiveWallets
// (src/xbridge/xbridgeapp.cpp:1002-1090): empty Ip/Port/COIN/BlockTime, maker
// and taker locktime targets, confirmation drift, and the CreateTxMethod
// dispatch. A failing coin is not activated; the error text keeps C++'s
// greppable ERR/LOG fragments (with the ticker in place of the quoted title).
func Admit(c *CoinConf) error {
	if c == nil {
		return fmt.Errorf("wallet admission: nil coin config")
	}
	// C++ stores BlockTime/Confirmations as uint32_t: a negative conf value
	// wraps to a huge number there and fails the gates below. Reject negatives
	// explicitly here instead of wrapping.
	if c.BlockTime < 0 || c.Confirmations < 0 {
		return fmt.Errorf("%s: negative BlockTime/Confirmations", c.Ticker)
	}
	// Go's Port is an int: Port==0 conflates a missing key and an explicit 0
	// (C++ compares the empty string, so "0" would pass admission and fail the
	// live connection instead). Same net outcome — the coin is not activated —
	// but Go rejects earlier and more strictly.
	if c.Ip == "" || c.Port == 0 || c.Coin == 0 || c.BlockTime == 0 {
		return fmt.Errorf("%s: Failed to connect, check the config", c.Ticker)
	}
	// Maker locktime requirements.
	if c.BlockTime*XMinLockTimeBlocks > XMakerLocktimeTargetSeconds {
		return fmt.Errorf("%s: Failed maker locktime requirements", c.Ticker)
	}
	// Taker locktime requirements (non-slow chains).
	if c.BlockTime < XSlowBlockTimeSeconds && c.BlockTime*XMinLockTimeBlocks > XTakerLocktimeTargetSeconds {
		return fmt.Errorf("%s: Failed taker locktime requirements", c.Ticker)
	}
	// Taker locktime requirements (slow chains).
	if c.BlockTime >= XSlowBlockTimeSeconds && c.BlockTime*XMinLockTimeBlocks > XSlowTakerLocktimeTargetSeconds {
		return fmt.Errorf("%s: Failed taker locktime requirements", c.Ticker)
	}
	// Confirmation compatibility: max(900/blockTime, 4) (xbridgeapp.cpp:1030).
	maxConfirmations := XLocktimeDriftSeconds / c.BlockTime
	if XMaxLockTimeDriftBlocks > maxConfirmations {
		maxConfirmations = XMaxLockTimeDriftBlocks
	}
	if c.Confirmations > maxConfirmations {
		return fmt.Errorf("%s: Failed confirmation check, max allowed for this token is %d", c.Ticker, maxConfirmations)
	}
	switch c.CreateTxMethod {
	case "ETH", "ETHER", "ETHEREUM":
		return fmt.Errorf("%s: ETH connector is not supported on XBridge at this time", c.Ticker)
	case "BCD", "PART", "STEALTH", "XST":
		return fmt.Errorf("%s: %s connector is not supported by go-xbridge (deferred)", c.Ticker, c.CreateTxMethod)
	}
	if !supportedCreateTxMethods[c.CreateTxMethod] {
		return fmt.Errorf("%s: unknown session type %s", c.Ticker, c.CreateTxMethod)
	}
	return nil
}

// Admitted returns the subset of confs whose coin passes the static admission
// gates (Admit), mirroring C++ skipping wallets that fail updateActiveWallets.
// Gate-failed coins never reach the coin registry or the connector set.
func Admitted(confs map[string]*CoinConf) map[string]*CoinConf {
	out := make(map[string]*CoinConf, len(confs))
	for ticker, c := range confs {
		if Admit(c) == nil {
			out[ticker] = c
		}
	}
	return out
}
