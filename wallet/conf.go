package wallet

import (
	"fmt"

	"xbridge-go/coins"
	"xbridge-go/config"
)

// NewConnectorFromConf builds a Connector for a coin from its [TICKER] section
// in xbridge.conf. The endpoint, credentials, decimals, segwit support, RPC
// version and content-type all come from conf — nothing is hardcoded. BLOCK is
// configured exactly like any other coin.
func NewConnectorFromConf(c *config.CoinConf) (Connector, error) {
	if c.Ip == "" || c.Port == 0 {
		return nil, fmt.Errorf("wallet: %s: Ip/Port missing in xbridge.conf", c.Ticker)
	}
	coin, err := coins.FromConf(c)
	if err != nil {
		return nil, err
	}
	chain := Chain{
		Ticker:        c.Ticker,
		Endpoint:      fmt.Sprintf("http://%s:%d", c.Ip, c.Port),
		User:          c.Username,
		Pass:          c.Password,
		Decimals:      coin.Decimals,
		CreateTxMethod: c.CreateTxMethod,
		SegWit:        coin.SegWit,
		JSONVersion:   c.JSONVersion,
		ContentType:   c.ContentType,
		Confirmations: c.Confirmations,
	}
	return NewRPCConnector(chain), nil
}
