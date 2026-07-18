package wallet

import (
	"testing"

	"go-xbridge/config"
)

// TestNewConnectorFromConfMissingHost asserts that a [TICKER] section without
// Ip/Port is rejected rather than producing a connector pointing at a blank
// endpoint (a malformed conf must fail closed).
func TestNewConnectorFromConfMissingHost(t *testing.T) {
	if _, err := NewConnectorFromConf(&config.CoinConf{
		Ticker: "BTC", CreateTxMethod: "BTC", Coin: 100000000,
	}); err == nil {
		t.Fatal("expected error for missing Ip/Port")
	}
}

// TestNewConnectorFromConfValid asserts a well-formed [TICKER] section yields
// an RPCConnector carrying the right ticker and a correctly-formed endpoint.
func TestNewConnectorFromConfValid(t *testing.T) {
	c, err := NewConnectorFromConf(&config.CoinConf{
		Ticker:         "BTC",
		CreateTxMethod: "BTC",
		Ip:             "127.0.0.1",
		Port:           8332,
		Username:       "u",
		Password:       "p",
		Coin:           100000000,
		AddressPrefix:  0,
		ScriptPrefix:   5,
	})
	if err != nil {
		t.Fatalf("NewConnectorFromConf: %v", err)
	}
	rc, ok := c.(*RPCConnector)
	if !ok {
		t.Fatalf("connector type = %T, want *RPCConnector", c)
	}
	if rc.Ticker() != "BTC" {
		t.Fatalf("ticker = %q, want BTC", rc.Ticker())
	}
	if ep := rc.Endpoint(); ep != "http://127.0.0.1:8332" {
		t.Fatalf("endpoint = %q, want http://127.0.0.1:8332", ep)
	}
}
