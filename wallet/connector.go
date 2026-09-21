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
	P2SHNative  uint64 // the SAME matched output's exact value in the coin's native base (BTC sat). Exact, so the claim spend cannot round-trip past the deposit.
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
	// GetBlockTxs returns a block's decoded transactions via verbose getblock
	// (verbosity 2 requested explicitly — stock Core defaults bare hashes
	// to verbosity 1 — with one bare-hash probe before giving up),
	// mirroring C++ getTransactionsInBlock + isUTXOSpentInTx
	// (xbridgewalletconnectorbtc.cpp:1838-1869,1760-1798): the
	// confirmed-spend leg of the deposit watch pages block bodies hunting
	// the spender of our own deposit when the hub is dead. blockHash is the
	// internal (little-endian) form GetBlockHash returns; the wallet
	// converts to display order for the wire. Backends without decoded
	// output fail here (callers hold their cursor, mempool leg only) —
	// never a silent empty list, never per-tx fan-out.
	GetBlockTxs(blockHash [32]byte) ([]BlockTx, error)
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
	// At requiredConfirmations=0 (no gettxout gate) the deposit must still be
	// visible in the verbose chain/mempool view: wallet-local bytes alone are
	// not proof the network saw the broadcast, and unknown/conflicted there is
	// NotReady, never proceed.
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
	// GetTxOut fetches an unspent output's chain data (whole-coin value in
	// Value, display-order TxID/Vout) via gettxout, mirroring C++
	// rpc::gettxout (xbridgewalletconnectorbtc.cpp:659-712). ok=false when the
	// output is unknown or spent (gettxout result null / RPC error), which the
	// inbound-proof verifier treats as "the entry cannot hold". Used to validate
	// counterparty order UTXOs before booking (C++ processTransaction,
	// xbridgesession.cpp:535-575).
	GetTxOut(txid string, vout uint32) (Utxo, bool, error)
	// GetRawTransactionVerbose returns the decoded transaction with chain
	// context (confirmations, per-output native value + script hex) via
	// verbose getrawtransaction. Used ONLY as a degraded deposit-existence
	// check where gettxout is backend-blind: non-Core backends answer -5
	// "unknown/non-wallet transaction" for any tx outside their wallet, so a
	// confirmed counterparty deposit can never read unspent there. Spent
	// status stays opaque in this path (a claim broadcast on a spent output
	// cannot confirm, so proceeding is fund-safe); confirmation depth itself
	// was established at deposit-validation time.
	GetRawTransactionVerbose(txid string) (VerboseTx, error)
	// GetRawMempool returns the mempool's transaction ids (getrawmempool).
	// Mirrors C++ App::Impl::checkWatchesOnDepositSpends' mempool scan
	// (xbridgeapp.cpp:3378-3413): the taker sweeps pending transactions for
	// the spender of its own deposit to recover the HTLC secret when the
	// hub's ConfirmB never arrives. Unsupported backends return an error
	// (the watch degrades to the refund sweep, fund-safe).
	GetRawMempool() ([]string, error)
}

// BlockTx is one decoded transaction in a verbose-getblock response: only
// the fields the spend scan needs (identity + spent outpoints). Vin entries
// without a prevout (coinbase) carry zero values and never match a real
// deposit outpoint.
type BlockTx struct {
	TxID string
	Vin  []BlockVin
}

// BlockVin is one transaction input's spent outpoint (display txid + vout).
type BlockVin struct {
	TxID string
	Vout uint32
}

// VerboseTxOut is one decoded transaction output: native base-unit value and
// raw script hex, keyed by output index in VerboseTx.Outputs.
type VerboseTxOut struct {
	Value     uint64
	ScriptHex string
}

// VerboseTx is a decoded chain transaction with confirmation context.
type VerboseTx struct {
	TxID          string
	Confirmations int
	// HasConfirmations reports the backend actually asserted a depth.
	// Backends omit the field for mempool/unknown transactions; readers
	// that must distinguish "0-conf" from "no data" (spend classifiers,
	// scan seeds) gate on this — never on Confirmations alone.
	HasConfirmations bool
	Outputs          map[uint32]VerboseTxOut
}
