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

import (
	"errors"
	"time"
)

// Deposit-check sentinels returned by Connector.CheckDepositTransaction.
//
// ErrDepositNotReady mirrors C++ checkDepositTransaction returning false: the
// deposit exists but cannot be judged yet (tx not found, too few confirmations,
// missing prevout, gettxout unavailable) — the caller defers (C++ processLater)
// and the hub retransmits; it is NOT a bad deposit.
var (
	ErrDepositNotReady = errors.New("wallet: deposit not ready")
	// ErrNoChainSource reports a connector that cannot read chain data (the
	// LocalConnector has no getrawtransaction/gettxout source to validate
	// against).
	ErrNoChainSource = errors.New("wallet: no chain data source")
)

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
	// OmitJSONVersion, when true, drops the "jsonrpc" field from RPC requests.
	// XLite-style wallets reject requests that carry it; Bitcoin Core and
	// blocknetd expect {"jsonrpc":"1.0",...}. Off by default.
	OmitJSONVersion bool
	// Confirmations is the min confirmations for spendable UTXOs (from conf).
	Confirmations int
	// FeePerByte is the coin's tx fee rate in native base units per byte (the
	// [TICKER].FeePerByte conf key; C++ xbridgewalletconnectorbtc.cpp:1951).
	// Used by CheckDepositTransaction to bound the counterparty's deposit fee.
	FeePerByte uint64
	// MinTxFee floors computed fees (the [TICKER].MinTxFee conf key; C++
	// :1952-1954).
	MinTxFee uint64
	// TxWithTimeField mirrors the coin's serializeWithTimeField quirk
	// (config.CoinConf.TxWithTimeField): counterparty txs on such chains carry
	// a 4-byte nTime after nVersion and must be deserialized accordingly.
	TxWithTimeField bool
	// Timeout bounds each JSON-RPC call to the wallet/node. A hung wallet must
	// not wedge the swap feed goroutine indefinitely. Zero applies the client's
	// default (30s).
	Timeout time.Duration
}

// Utxo is a spendable output usable to fund a deposit transaction.
type Utxo struct {
	TxID          string // prevout txid (hex, display order)
	Vout          uint32
	Address       string
	Amount        uint64  // base units
	Value         float64 // whole-coin value exactly as the wallet returned it (listunspent "value"); used to reproduce C++ UtxoEntry::toString() in ownership proofs
	ScriptPubKey  string  // hex of the output's script
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

// DepositCheck is the verdict of a Connector.CheckDepositTransaction call
// (C++ BtcWalletConnector::checkDepositTransaction out-params, xbridgewallet
// connectorbtc.cpp:1981-2194). IsGood reports a deposit that passed every C++
// check; a non-good result is a definitively bad deposit (cancel), never a
// retry — not-ready is signalled by the ErrDepositNotReady sentinel instead.
type DepositCheck struct {
	IsGood      bool
	P2SHAmount  uint64 // matched p2sh output value, XBridge 1e6 base (C++ p2shAmount = whole × COIN)
	DepositVout uint32 // matched p2sh output index (C++ depositTxVout)
	Excess      uint64 // value over the expected amount+fee2, XBridge 1e6 base (C++ excessAmount = oOverpayment)
}

// Connector is the wallet contract the swap deposit layer drives. A connected
// SPV wallet (RPC) or a local keystore can both satisfy it.
type Connector interface {
	// Ticker returns the coin this connector drives.
	Ticker() string
	// GetBalance returns the wallet-wide confirmed available balance in native
	// base units (getbalance). Mirrors C++ CWallet::GetBalance(), which C++
	// acceptXBridgeTransaction sums over all wallets for the
	// INSUFFICIENT_FUNDS_DX pre-check (xbridgeapp.h:798-806, xbridgeapp.cpp:2159).
	GetBalance() (uint64, error)
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
	// GetRelayFee returns the per-coin relay fee (BTC per kB) from the wallet's
	// getinfo (C++ xbridgewalletconnectorbtc.cpp:74-76), used for dust (C++ :1526).
	GetRelayFee() (float64, error)
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
	// CheckDepositTransaction validates a counterparty's HTLC deposit against the
	// expected p2sh script and amount, mirroring C++
	// BtcWalletConnector::checkDepositTransaction (xbridgewalletconnectorbtc.cpp:
	// 1981-2194): raw-hex getrawtransaction + local decode, gettxout
	// confirmation gate, SEQUENCE_FINAL + prevout-amount vin scan, expected-script
	// p2sh scan, fee1/fee2 5% tolerance, excess. expectedAmount and the returned
	// P2SHAmount/Excess are XBridge 1e6 base units. Returns ErrDepositNotReady
	// when the deposit cannot be judged yet (C++ return false → processLater) —
	// a non-error result with IsGood=false means the deposit is definitively bad.
	CheckDepositTransaction(depositTxID, expectedScriptHex string, expectedAmount uint64, requiredConfirmations int) (DepositCheck, error)
	// SignMessage produces a BIP137 ownership proof (compact 65-byte signature)
	// over message for the given address. XBridge embeds a SignMessage proof for
	// each order UTXO so counterparties can verify the order creator owns the
	// coins (src/xbridge/xbridgeapp.cpp createOrder →
	// CXBridgeWalletConnector::signMessage(address, txid:vout)).
	SignMessage(address, message string) ([]byte, error)
	// VerifyMessage checks a BIP137 proof (address, sig, message) against this
	// wallet's chain. Used by the taker to validate the maker's UTXO proofs.
	VerifyMessage(address string, sig []byte, message string) (bool, error)
}
