// Package wallet defines the Connector contract that swap deposits drive to
// fund, sign, and broadcast through the user's wallet; RPCConnector and LocalConnector implement it.
package wallet

import (
	"errors"
	"time"
)

var (
	// ErrDepositNotReady means the deposit cannot be judged yet: retry later; it is never a bad deposit.
	ErrDepositNotReady = errors.New("wallet: deposit not ready")
	// ErrNoChainSource reports a connector that cannot read chain data.
	ErrNoChainSource = errors.New("wallet: no chain data source")
)

// NotReadyError wraps ErrDepositNotReady with a visibility verdict: Seen reports whether the watched transaction's bytes were actually observed.
type NotReadyError struct {
	Seen bool
	Err  error
}

// Error returns the wrapped cause's message, the package default when unset.
func (e *NotReadyError) Error() string {
	if e == nil || e.Err == nil {
		return "wallet: deposit not ready"
	}
	return e.Err.Error()
}

// Unwrap exposes ErrDepositNotReady (and any inner cause) to errors.Is/As.
func (e *NotReadyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NotReadySeen reports whether a not-ready failure observed the watched transaction; an unclassifiable error reports true, so polling slows only on proven progress-free.
func NotReadySeen(err error) bool {
	var nre *NotReadyError
	if errors.As(err, &nre) {
		return nre.Seen
	}
	return true
}

// Chain identifies a coin wallet endpoint: the connected SPV wallet (or full node) exposing Blocknet-core-compatible RPC for that ticker.
type Chain struct {
	Ticker   string
	Endpoint string // http(s)://host:port
	User     string
	Pass     string
	Decimals int

	// CreateTxMethod selects the transaction-construction path.
	CreateTxMethod string
	// SegWit reports whether the chain has native segwit addresses.
	SegWit bool
	// JSONVersion is the request's "jsonrpc" field value; ContentType is the request's Content-Type header.
	JSONVersion string
	ContentType string
	// OmitJSONVersion drops the "jsonrpc" field from RPC requests; XLite-style wallets reject requests that carry it.
	OmitJSONVersion bool
	// Confirmations is the min confirmations for spendable UTXOs.
	Confirmations int
	// FeePerByte is the coin's tx fee rate in native base units per byte.
	FeePerByte uint64
	// MinTxFee floors computed fees in native base units.
	MinTxFee uint64
	// TxWithTimeField reports that txs on such chains carry a 4-byte nTime after nVersion and must be deserialized accordingly.
	TxWithTimeField bool
	// Timeout bounds each JSON-RPC call to the wallet/node; zero applies the client's 30s default.
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

// PrevTx describes a previous output being signed, in the shape Bitcoin Core's signrawtransaction expects.
type PrevTx struct {
	TxID         string
	Vout         uint32
	ScriptPubKey string
	Amount       uint64 // base units
}

// DepositCheck is the verdict of Connector.CheckDepositTransaction; a non-good result with no error means the deposit is definitively bad.
type DepositCheck struct {
	IsGood      bool
	P2SHAmount  uint64 // matched p2sh output value, XBridge 1e6 base
	P2SHNative  uint64 // the SAME matched output's exact value in the coin's native base (BTC sat). Exact, so the claim spend cannot round-trip past the deposit.
	DepositVout uint32 // matched p2sh output index
	Excess      uint64 // value over the expected amount+fee2, XBridge 1e6 base
}

// Connector is the wallet contract the swap deposit layer drives.
type Connector interface {
	// Ticker returns the coin this connector drives.
	Ticker() string
	// GetBalance returns the wallet-wide confirmed available balance in native base units.
	GetBalance() (uint64, error)
	// GetNewAddress returns a fresh receive address.
	GetNewAddress() (string, error)
	// ListUnspent returns spendable UTXOs with at least minConf confirmations; unconfirmed outputs are dropped even at minConf 0.
	ListUnspent(minConf int) ([]Utxo, error)
	// ListUnspentWithZeroConf returns the spendable set including unconfirmed outputs; it is enumeration only: callers that fund must still require at least one confirmation themselves.
	ListUnspentWithZeroConf() ([]Utxo, error)
	// SignRawTransaction signs txHex with prevTxs supplying each input's script and amount; complete reports whether every input was signed.
	SignRawTransaction(txHex string, prevTxs []PrevTx) (signedHex string, complete bool, err error)
	// SendRawTransaction broadcasts txHex, returning the network txid.
	SendRawTransaction(txHex string) (txid string, err error)
	// GetBlockCount returns the best block height of the coin's chain.
	GetBlockCount() (int64, error)
	// GetBlockHash returns the block hash in internal (little-endian) byte order, the order the XBridge wire uses.
	GetBlockHash(height int64) ([32]byte, error)
	// GetBlockTxs returns a block's decoded transactions; a backend that cannot decode block bodies errors rather than returning an empty list.
	GetBlockTxs(blockHash [32]byte) ([]BlockTx, error)
	// GetRawTransaction returns the full serialized tx hex; the taker reads the counterparty's payTx with it.
	GetRawTransaction(txid string) (string, error)
	// CheckDepositTransaction judges a counterparty's HTLC deposit: an error means retry later, a non-error IsGood=false means definitively bad.
	CheckDepositTransaction(depositTxID, expectedScriptHex string, expectedAmount uint64, requiredConfirmations int) (DepositCheck, error)
	// SignMessage produces a BIP137 ownership proof: a compact 65-byte signature over message for the given address.
	SignMessage(address, message string) ([]byte, error)
	// VerifyMessage checks a BIP137 proof (address, sig, message) against this wallet's chain.
	VerifyMessage(address string, sig []byte, message string) (bool, error)
	// GetTxOut fetches an unspent output's chain data (whole-coin Value, display-order TxID/Vout); ok is false when the output is unknown or spent.
	GetTxOut(txid string, vout uint32) (Utxo, bool, error)
	// GetRawTransactionVerbose is the existence check where gettxout is blind to non-wallet txs: spentness reads opaque, which is fund-safe because a claim on a spent output cannot confirm.
	GetRawTransactionVerbose(txid string) (VerboseTx, error)
	// GetRawMempool returns the mempool's transaction ids; an unsupported backend errors rather than returning an empty list.
	GetRawMempool() ([]string, error)
}

// BlockTx is one decoded transaction in a verbose-getblock response; coinbase inputs carry zero prevouts and never match a real deposit outpoint.
type BlockTx struct {
	TxID string
	Vin  []BlockVin
}

// BlockVin is one transaction input's spent outpoint (display txid + vout).
type BlockVin struct {
	TxID string
	Vout uint32
}

// VerboseTxOut is one decoded transaction output: native base-unit value and raw script hex, keyed by output index in VerboseTx.Outputs.
type VerboseTxOut struct {
	Value     uint64
	ScriptHex string
}

// VerboseTx is a decoded chain transaction with confirmation context.
type VerboseTx struct {
	TxID          string
	Confirmations int
	// HasConfirmations reports the backend actually asserted a depth (omitted for mempool/unknown txs); readers gate on it, never on Confirmations alone.
	HasConfirmations bool
	Outputs          map[uint32]VerboseTxOut
}
