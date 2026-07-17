package wallet

import (
	"encoding/hex"
	"errors"
	"fmt"

	xlog "xbridge-go/log"

	"xbridge-go/coins"
)

// LocalSigner signs individual inputs of an in-memory transaction, producing
// the full scriptSig (including any HTLC redeem logic) for each input. It lets
// a LocalConnector sign without an external wallet by holding the keys itself.
type LocalSigner interface {
	// SignInput returns the finalized scriptSig for input idx of tx, given the
	// previous output it spends. It must assemble any HTLC redeem logic plus
	// the signature (e.g. via coins.BuildRefundScriptSig / BuildPaymentScriptSig).
	SignInput(tx *coins.Tx, idx int, prev PrevTx) ([]byte, error)
}

// Broadcaster sends a fully-signed transaction to the network. The connected
// SPV wallet provides one (via RPC); a nil Broadcaster makes LocalConnector
// sign-only (useful for offline signing and tests).
type Broadcaster func(txHex string) (txid string, err error)

// LocalConnector implements Connector by signing locally (with a LocalSigner)
// and — optionally — broadcasting through a Broadcaster. It is used when the
// library holds the keys directly rather than delegating to a wallet RPC.
//
// Address/UTXO/fee queries have no local source, so those methods return an
// error; the deposit flow supplies inputs and prevTxs explicitly instead.
type LocalConnector struct {
	ticker    string
	signer    LocalSigner
	broadcast Broadcaster
}

// NewLocalConnector builds a local-signing connector. broadcast may be nil for
// sign-only use.
func NewLocalConnector(ticker string, signer LocalSigner, broadcast Broadcaster) *LocalConnector {
	return &LocalConnector{ticker: ticker, signer: signer, broadcast: broadcast}
}

func (l *LocalConnector) Ticker() string { return l.ticker }

func (l *LocalConnector) GetNewAddress() (string, error) {
	return "", errors.New("wallet: LocalConnector has no address pool")
}

func (l *LocalConnector) ListUnspent(minConf int) ([]Utxo, error) {
	return nil, errors.New("wallet: LocalConnector has no UTXO source")
}

// SignRawTransaction parses txHex, signs each input via the LocalSigner, and
// re-serializes. Returns the signed hex and complete=true (all inputs signed).
func (l *LocalConnector) SignRawTransaction(txHex string, prevTxs []PrevTx) (string, bool, error) {
	raw, err := hex.DecodeString(txHex)
	if err != nil {
		return "", false, fmt.Errorf("wallet: bad tx hex: %w", err)
	}
	tx, err := coins.Deserialize(raw)
	if err != nil {
		return "", false, err
	}
	if len(prevTxs) != len(tx.Inputs) {
		return "", false, fmt.Errorf("wallet: %d prevTxs for %d inputs", len(prevTxs), len(tx.Inputs))
	}
	for i := range tx.Inputs {
		sig, err := l.signer.SignInput(tx, i, prevTxs[i])
		if err != nil {
			return "", false, err
		}
		tx.Inputs[i].ScriptSig = sig
	}
	xlog.Debug("local sign", "ticker", l.ticker, "inputs", len(tx.Inputs), "complete", true)
	return hex.EncodeToString(tx.Serialize()), true, nil
}

// SendRawTransaction broadcasts via the Broadcaster, if configured.
func (l *LocalConnector) SendRawTransaction(txHex string) (string, error) {
	if l.broadcast == nil {
		return "", errors.New("wallet: LocalConnector has no Broadcaster")
	}
	xlog.Debug("local broadcast", "ticker", l.ticker, "txHexLen", len(txHex))
	return l.broadcast(txHex)
}

func (l *LocalConnector) EstimateFee(confTarget int) (uint64, error) {
	return 0, errors.New("wallet: LocalConnector has no fee source")
}

// GetBlockCount / GetBlockHash have no local blockchain source.
func (l *LocalConnector) GetBlockCount() (int64, error) {
	return 0, errors.New("wallet: LocalConnector has no block source")
}

func (l *LocalConnector) GetBlockHash(height int64) ([32]byte, error) {
	return [32]byte{}, errors.New("wallet: LocalConnector has no block source")
}

// GetRawTransaction has no local blockchain source.
func (l *LocalConnector) GetRawTransaction(txid string) (string, error) {
	return "", errors.New("wallet: LocalConnector has no block source")
}

// SignMessage is unsupported for LocalConnector: BIP137 signing over locally
// held keys (the Blockchain Message magic-prefixed double-SHA256) is a
// follow-up. The RPCConnector path covers live wallets.
func (l *LocalConnector) SignMessage(address, message string) ([]byte, error) {
	return nil, errors.New("wallet: LocalConnector does not support signmessage")
}

// VerifyMessage is unsupported for LocalConnector (see SignMessage).
func (l *LocalConnector) VerifyMessage(address string, sig []byte, message string) (bool, error) {
	return false, errors.New("wallet: LocalConnector does not support verifymessage")
}
