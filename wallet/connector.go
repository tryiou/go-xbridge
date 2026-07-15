// Package wallet provides the connector the swap deposit layer drives to fund,
// sign, and broadcast XBridge transactions through a connected wallet.
//
// The connected wallet is the user's own (SPV) wallet, which holds the keys
// and — for BLOCK — pays the service-node fee via Blocknet core RPC. Two
// implementations satisfy the Connector contract:
//
//   - RPCConnector: talks JSON-RPC to a Blocknet-core-compatible wallet/node
//     (signrawtransactionwithwallet, sendrawtransaction, getnewaddress, …).
//   - LocalConnector: signs locally with caller-supplied keys (via a
//     LocalSigner) and optionally broadcasts through a Broadcaster. Useful
//     when the library holds the keys directly, and for offline/test signing.
//
// Transaction *construction* (deposit/refund/payment scripts, HTLC) lives in
// the coins package; wallet only signs and moves bytes.
package wallet

// Chain identifies a coin wallet endpoint: the connected SPV wallet (or full
// node) exposing Blocknet-core-compatible RPC for that ticker. Every field is
// derived from the coin's [TICKER] section in xbridge.conf (nothing hardcoded).
type Chain struct {
	Ticker   string // wire ticker, e.g. "BTC"
	Endpoint string // http(s)://host:port
	User     string // RPC auth user
	Pass     string // RPC auth pass
	Decimals int    // base-unit precision (from COIN in conf)

	// CreateTxMethod selects the transaction-construction path (from conf).
	CreateTxMethod string
	// SegWit is whether the chain has native segwit addresses (derived from
	// CreateTxMethod, mirroring C++'s connector classes).
	SegWit bool
	// JSONVersion / ContentType are the RPC client version and content-type the
	// wallet expects (from conf; defaults applied by the builder).
	JSONVersion string
	ContentType string
	// Confirmations is the min confirmations for spendable UTXOs (from conf).
	Confirmations int
}

// Utxo is a spendable output usable to fund a deposit transaction.
type Utxo struct {
	TxID          string // prevout txid (hex, display order)
	Vout          uint32
	Address       string
	Amount        uint64 // base units
	ScriptPubKey  string // hex of the output's script
	Confirmations int
}

// PrevTx describes a previous output being signed, in the shape Bitcoin Core
// signrawtransaction expects: txid, vout, scriptPubKey, amount.
type PrevTx struct {
	TxID         string
	Vout         uint32
	ScriptPubKey string
	Amount       uint64 // base units
}

// Connector is the wallet contract the swap deposit layer drives. A connected
// SPV wallet (RPC) or a local keystore can both satisfy it.
type Connector interface {
	// Ticker returns the coin this connector drives.
	Ticker() string
	// GetNewAddress returns a fresh receive address.
	GetNewAddress() (string, error)
	// ListUnspent returns spendable UTXOs with at least minConf confirmations.
	ListUnspent(minConf int) ([]Utxo, error)
	// SignRawTransaction signs txHex with the wallet's keys. prevTxs supplies
	// the previous outputs' scripts/amounts. It returns the signed hex and
	// whether signing completed (every input signed).
	SignRawTransaction(txHex string, prevTxs []PrevTx) (signedHex string, complete bool, err error)
	// SendRawTransaction broadcasts txHex, returning the network txid.
	SendRawTransaction(txHex string) (txid string, err error)
	// EstimateFee returns the fee rate in sat/vB for confTarget confirmations.
	EstimateFee(confTarget int) (uint64, error)
	// GetBlockCount returns the best block height of the coin's chain.
	GetBlockCount() (int64, error)
	// GetBlockHash returns the block hash at height as a 32-byte internal
	// (little-endian) hash, matching the XBridge wire order. Used to stamp
	// orders' anti-replay blockHash (C++ uses the BLOCK chain's
	// chainActive.Tip()->pprev).
	GetBlockHash(height int64) ([32]byte, error)
	// GetRawTransaction returns the full serialized (hex) transaction for txid.
	// The taker uses it to read the maker's payTx and recover the HTLC secret
	// preimage (C++ getSecretFromPaymentTransaction → getrawtransaction).
	GetRawTransaction(txid string) (string, error)
}
