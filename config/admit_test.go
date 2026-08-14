package config

import "testing"

// baseAdmitted is a coin that passes every admission gate; each test mutates
// one field to hit a specific gate.
func baseAdmitted() *CoinConf {
	return &CoinConf{
		Ticker:         "BTC",
		Ip:             "127.0.0.1",
		Port:           8332,
		CreateTxMethod: "BTC",
		Coin:           100000000,
		BlockTime:      600,
		Confirmations:  2,
	}
}

func TestAdmitConstantsMatchCpp(t *testing.T) {
	// xbridgewallet.h:96-102 constexprs.
	if XMinLockTimeBlocks != 6 {
		t.Errorf("XMinLockTimeBlocks = %d, want 6", XMinLockTimeBlocks)
	}
	if XMaxLockTimeDriftBlocks != 4 {
		t.Errorf("XMaxLockTimeDriftBlocks = %d, want 4", XMaxLockTimeDriftBlocks)
	}
	if XMakerLocktimeTargetSeconds != 7200 {
		t.Errorf("XMakerLocktimeTargetSeconds = %d, want 7200", XMakerLocktimeTargetSeconds)
	}
	if XTakerLocktimeTargetSeconds != 1800 {
		t.Errorf("XTakerLocktimeTargetSeconds = %d, want 1800", XTakerLocktimeTargetSeconds)
	}
	if XSlowTakerLocktimeTargetSeconds != 3600 {
		t.Errorf("XSlowTakerLocktimeTargetSeconds = %d, want 3600", XSlowTakerLocktimeTargetSeconds)
	}
	if XSlowBlockTimeSeconds != 600 {
		t.Errorf("XSlowBlockTimeSeconds = %d, want 600", XSlowBlockTimeSeconds)
	}
	if XLocktimeDriftSeconds != 900 {
		t.Errorf("XLocktimeDriftSeconds = %d, want 900", XLocktimeDriftSeconds)
	}
}

// TestAdmitGates locks CFG-F85: the static wallet-admission gates ported from
// xbridgeapp.cpp:1002-1040, using the exact C++ boundary values.
func TestAdmitGates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CoinConf)
		wantOK bool
	}{
		{"base passes", func(*CoinConf) {}, true},
		{"empty Ip", func(c *CoinConf) { c.Ip = "" }, false},
		{"zero Port", func(c *CoinConf) { c.Port = 0 }, false},
		{"zero Coin", func(c *CoinConf) { c.Coin = 0 }, false},
		{"zero BlockTime", func(c *CoinConf) { c.BlockTime = 0 }, false},

		// Negative values: C++ stores BlockTime/Confirmations as uint32_t, so a
		// negative conf value wraps huge there and fails the gates; Go rejects
		// them explicitly instead of wrapping.
		{"negative BlockTime", func(c *CoinConf) { c.BlockTime = -100 }, false},
		{"negative Confirmations", func(c *CoinConf) { c.Confirmations = -2 }, false},

		// Maker: blockTime*6 > 7200. Only reachable for blockTime > 1200
		// (smaller values pass the maker gate but fail a taker gate, which C++
		// checks after — the maker gate simply never fires first for them).
		{"maker 1201", func(c *CoinConf) { c.BlockTime = 1201 }, false},
		{"maker 1300", func(c *CoinConf) { c.BlockTime = 1300 }, false},

		// Taker non-slow: blockTime < 600 && blockTime*6 > 1800.
		{"taker non-slow 300", func(c *CoinConf) { c.BlockTime = 300 }, true},
		{"taker non-slow 301", func(c *CoinConf) { c.BlockTime = 301 }, false},
		{"taker non-slow 599", func(c *CoinConf) { c.BlockTime = 599 }, false},
		{"taker non-slow 150", func(c *CoinConf) { c.BlockTime = 150 }, true},

		// Taker slow: blockTime >= 600 && blockTime*6 > 3600.
		{"taker slow 600", func(c *CoinConf) { c.BlockTime = 600 }, true},
		{"taker slow 601", func(c *CoinConf) { c.BlockTime = 601 }, false},

		// Confirmation: requiredConfirmations > max(900/blockTime, 4).
		// Only blockTimes that pass every prior gate are reachable:
		// BlockTime 600 -> max(1,4)=4; 300 -> max(3,4)=4; 60 -> max(15,4)=15;
		// 120 -> max(7,4)=7.
		{"conf 600 at 4", func(c *CoinConf) { c.BlockTime = 600; c.Confirmations = 4 }, true},
		{"conf 600 at 5", func(c *CoinConf) { c.BlockTime = 600; c.Confirmations = 5 }, false},
		{"conf 300 at 4", func(c *CoinConf) { c.BlockTime = 300; c.Confirmations = 4 }, true},
		{"conf 300 at 5", func(c *CoinConf) { c.BlockTime = 300; c.Confirmations = 5 }, false},
		{"conf 60 at 15", func(c *CoinConf) { c.BlockTime = 60; c.Confirmations = 15 }, true},
		{"conf 60 at 16", func(c *CoinConf) { c.BlockTime = 60; c.Confirmations = 16 }, false},
		{"conf 120 at 7", func(c *CoinConf) { c.BlockTime = 120; c.Confirmations = 7 }, true},
		{"conf 120 at 8", func(c *CoinConf) { c.BlockTime = 120; c.Confirmations = 8 }, false},
	}
	for _, tc := range cases {
		c := baseAdmitted()
		tc.mutate(c)
		err := Admit(c)
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: Admit = %v, wantOK %v", tc.name, err, tc.wantOK)
		}
	}
}

// TestAdmitCreateTxMethod locks CFG-F91's method-dispatch fix: ETH and unknown
// methods are rejected (C++ xbridgeapp.cpp:1043-1090), and the non-portable
// connectors deferred under CRYPTO-F98 (PART) / CRYPTO-F99 (BCD), plus
// STEALTH/XST (in no live manifest conf), are refused rather than built as
// BTC-format connectors.
func TestAdmitCreateTxMethod(t *testing.T) {
	supported := []string{"BTC", "SYS", "LTC", "DGB", "BCH", "BTG", "DEVAULT"}
	rejected := []string{"ETH", "ETHER", "ETHEREUM", "BCD", "PART", "STEALTH", "XST", "BLOCK", "", "FOO"}
	for _, m := range supported {
		c := baseAdmitted()
		c.CreateTxMethod = m
		if err := Admit(c); err != nil {
			t.Errorf("method %q: Admit = %v, want nil", m, err)
		}
	}
	for _, m := range rejected {
		c := baseAdmitted()
		c.CreateTxMethod = m
		if err := Admit(c); err == nil {
			t.Errorf("method %q: Admit accepted, want rejection", m)
		}
	}
}

// TestAdmittedFilters lock CFG-F85's registry consequence: Admitted returns
// exactly the coins passing the static gates.
func TestAdmittedFilters(t *testing.T) {
	good := baseAdmitted()
	badLocktime := baseAdmitted()
	badLocktime.Ticker = "BAD"
	badLocktime.BlockTime = 1201
	badMethod := baseAdmitted()
	badMethod.Ticker = "ETHX"
	badMethod.CreateTxMethod = "ETH"

	confs := map[string]*CoinConf{
		"BTC":  good,
		"BAD":  badLocktime,
		"ETHX": badMethod,
	}
	got := Admitted(confs)
	if len(got) != 1 || got["BTC"] != good {
		t.Errorf("Admitted = %v, want only BTC", got)
	}
	if _, ok := got["BAD"]; ok {
		t.Error("gate-failed coin BAD survived Admitted")
	}
	if _, ok := got["ETHX"]; ok {
		t.Error("ETH coin ETHX survived Admitted")
	}
}
