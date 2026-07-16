package api

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strings"
	"sync"
	"testing"

	btcec "github.com/btcsuite/btcd/btcec/v2"

	"xbridge-go/coins"
	"xbridge-go/config"
	"xbridge-go/crypto"
	"xbridge-go/proto"
	"xbridge-go/wallet"
)

// b58alphabet is the Bitcoin base58 alphabet used by base58check.
const b58alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	x := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	mod := new(big.Int)
	var out []byte
	for x.Sign() > 0 {
		x.DivMod(x, base, mod)
		out = append(out, b58alphabet[mod.Int64()])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	for i := 0; i < zeros; i++ {
		out = append([]byte{'1'}, out...)
	}
	return string(out)
}

func b58checkEncode(prefix byte, payload []byte) string {
	buf := append([]byte{prefix}, payload...)
	h1 := sha256.Sum256(buf)
	h2 := sha256.Sum256(h1[:])
	all := append(buf, h2[:4]...)
	return base58Encode(all)
}

func hash20(s string) [20]byte {
	h := sha256.Sum256([]byte(s))
	var o [20]byte
	copy(o[:], h[:20])
	return o
}

// addrFor base58check-encodes the 20-byte HASH160 of s under version prefix.
func addrFor(prefix byte, s string) string {
	h := hash20(s)
	return b58checkEncode(prefix, h[:])
}

// fakeConnector is an in-memory wallet.Connector used to drive the swap driver
// without a live wallet. It holds one P2PKH funding UTXO (signed locally with
// fundingPriv), records broadcasts, and answers GetRawTransaction from its own
// broadcast log so the taker can recover the secret from the maker's payTx.
type fakeConnector struct {
	ticker      string
	funding     wallet.Utxo
	fundingPriv []byte
	fundingPub  []byte
	changeAddr  string
	blockHeight int64

	mu         sync.Mutex
	broadcasts []string
	rawTx      map[string]string // display txid -> hex
}

func (f *fakeConnector) Ticker() string { return f.ticker }

func (f *fakeConnector) GetNewAddress() (string, error) { return f.changeAddr, nil }

func (f *fakeConnector) ListUnspent(minConf int) ([]wallet.Utxo, error) {
	return []wallet.Utxo{f.funding}, nil
}

func (f *fakeConnector) SignRawTransaction(txHex string, prevTxs []wallet.PrevTx) (string, bool, error) {
	raw, err := hex.DecodeString(txHex)
	if err != nil {
		return "", false, err
	}
	tx, err := coins.Deserialize(raw)
	if err != nil {
		return "", false, err
	}
	for i := range tx.Inputs {
		if i >= len(prevTxs) {
			return "", false, err
		}
		prevScript, _ := hex.DecodeString(prevTxs[i].ScriptPubKey)
		sig, serr := coins.SignTxInput(tx, i, prevScript, f.fundingPriv)
		if serr != nil {
			return "", false, serr
		}
		// P2PKH scriptSig: <sig> <pubkey>.
		ss := []byte{byte(len(sig))}
		ss = append(ss, sig...)
		ss = append(ss, byte(len(f.fundingPub)))
		ss = append(ss, f.fundingPub...)
		tx.Inputs[i].ScriptSig = ss
	}
	return hex.EncodeToString(tx.Serialize()), true, nil
}

func (f *fakeConnector) SendRawTransaction(txHex string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	txid, err := txIDFromHex(txHex)
	if err != nil {
		return "", err
	}
	f.rawTx[txid] = txHex
	f.broadcasts = append(f.broadcasts, txid)
	return txid, nil
}

func (f *fakeConnector) EstimateFee(confTarget int) (uint64, error) { return 1000, nil }

func (f *fakeConnector) GetBlockCount() (int64, error) { return f.blockHeight, nil }

func (f *fakeConnector) GetBlockHash(height int64) ([32]byte, error) { return [32]byte{}, nil }

func (f *fakeConnector) GetRawTransaction(txid string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.rawTx[txid]
	if !ok {
		return "", errNotFound
	}
	return h, nil
}

func (f *fakeConnector) SignMessage(address, message string) ([]byte, error) {
	// 65-byte placeholder compact signature for proof-attachment tests.
	return []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a,
		0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14,
		0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e,
		0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28,
		0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30, 0x31, 0x32,
		0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x3b, 0x3c,
		0x3d, 0x3e, 0x3f, 0x40, 0x41,
	}, nil
}

func (f *fakeConnector) VerifyMessage(address string, sig []byte, message string) (bool, error) {
	return len(sig) == 65, nil
}

var errNotFound = errorString("not found")

type errorString string

func (e errorString) Error() string { return string(e) }

// newKey returns a fresh 32-byte priv + compressed 33-byte pub.
func newKey(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := key.PubKey().SerializeCompressed()
	return key.Serialize(), pub
}

func newTestNode(t *testing.T, priv []byte, confs map[string]*config.CoinConf, conns map[string]wallet.Connector) *Node {
	t.Helper()
	pk, err := crypto.CompressedPubKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return &Node{
		cfg:      &Config{PrivKey: priv, Confs: confs, Connectors: conns},
		store:    NewStore(),
		signer:   crypto.NewBtcSigner(),
		pubkey:   pk,
		sessions: map[string]*SwapSession{},
		stop:     make(chan struct{}),
	}
}

// TestSwapHandshake drives a full maker⇄hub⇄taker swap client-side, asserting
// the HTLC deposits are built/broadcast with the correct redeem scripts, the
// maker reveals its secret on-chain, and the taker recovers it and claims the
// maker's deposit.
func TestSwapHandshake(t *testing.T) {
	// Register BTC/LTC from conf (decimals 8, prefixes 0/48).
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}

	btcHash := hash20("maker-btc-dest")
	ltcHash := hash20("taker-ltc-source")

	// Shared connectors (so the taker can read the maker's payTx).
	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{
		TxID:         strings.Repeat("aa", 32),
		Vout:         0,
		Amount:       5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub))),
	}
	btcConn := &fakeConnector{
		ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv, fundingPub: btcFundingPub,
		changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{},
	}
	ltcFundingPriv, ltcFundingPub := newKey(t)
	ltcFunding := wallet.Utxo{
		TxID:         strings.Repeat("bb", 32),
		Vout:         0,
		Amount:       5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(ltcFundingPub))),
	}
	ltcConn := &fakeConnector{
		ticker: "LTC", funding: ltcFunding, fundingPriv: ltcFundingPriv, fundingPub: ltcFundingPub,
		changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000, rawTx: map[string]string{},
	}
	conns := map[string]wallet.Connector{"BTC": btcConn, "LTC": ltcConn}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}

	mkPriv, _ := newKey(t)
	tkPriv, tkPub := newKey(t)
	makerNode := newTestNode(t, mkPriv, confs, conns)
	takerNode := newTestNode(t, tkPriv, confs, conns)

	var orderID [32]byte
	oid := hash20("order-id")
	copy(orderID[:], oid[:])

	mkAddr := addrFor(0, "maker-btc-dest")     // maker deposits BTC, receives LTC
	ltcAddr := addrFor(48, "taker-ltc-source") // taker deposits LTC, receives BTC

	makerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e8, ToAmount: 2e8}
	takerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e8, ToAmount: 2e8}

	makerNode.newMakerSession(makerOrder, MakeOrderParams{MakerAddress: mkAddr, TakerAddress: ltcAddr})
	takerNode.newTakerSession(takerOrder, TakeOrderParams{FromAddress: ltcAddr, ToAddress: mkAddr})

	var hub [20]byte
	hb := hash20("hub")
	copy(hub[:], hb[:])

	makerSession := makerNode.sessions[hexEncode(orderID[:])]
	takerSession := takerNode.sessions[hexEncode(orderID[:])]
	makerSession.hub = hub
	takerSession.hub = hub
	makerSecret := makerSession.secret

	// 1) Hold (hub→both) → HoldApply.
	if _, _, err := makerSession.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID, FromAmount: 1e8, ToAmount: 2e8}); err != nil {
		t.Fatalf("maker OnHold: %v", err)
	}
	if _, _, err := takerSession.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID, FromAmount: 1e8, ToAmount: 2e8}); err != nil {
		t.Fatalf("taker OnHold: %v", err)
	}

	// 2) Init (hub→each) → Initialized. ClientAddress is the destination address.
	mkInit := &proto.InitBody{ClientAddress: ltcHash, HubAddress: hub, ID: orderID}
	if _, _, err := makerSession.OnInit(mkInit); err != nil {
		t.Fatalf("maker OnInit: %v", err)
	}
	tkInit := &proto.InitBody{ClientAddress: btcHash, HubAddress: hub, ID: orderID}
	if _, _, err := takerSession.OnInit(tkInit); err != nil {
		t.Fatalf("taker OnInit: %v", err)
	}

	// 3) CreateA (hub→maker): maker builds + broadcasts deposit A (BTC).
	_, bodyA, err := makerSession.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkPub)})
	if err != nil {
		t.Fatalf("maker OnCreateA: %v", err)
	}
	createdA := bodyA.(*proto.CreatedABody)
	makerDepositTxID := createdA.ADepositTxID
	if makerDepositTxID == "" {
		t.Fatal("empty maker deposit txid")
	}
	if createdA.HashedSecret != makerSecretHash(t, makerSecret) {
		t.Error("CreatedA HashedSecret != HASH160(maker secret)")
	}

	// 4) CreateB (hub→taker): taker builds + broadcasts deposit B (LTC).
	_, bodyB, err := takerSession.OnCreateB(&proto.CreateBBody{
		HubAddress: hub, ID: orderID, APubKey: makerSession.pubkey(),
		ADepositTxID: makerDepositTxID, HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime,
	})
	if err != nil {
		t.Fatalf("taker OnCreateB: %v", err)
	}
	createdB := bodyB.(*proto.CreatedBBody)
	takerDepositTxID := createdB.BDepositTxID
	if takerDepositTxID == "" {
		t.Fatal("empty taker deposit txid")
	}

	// Verify both deposits lock funds into the correct P2SH HTLC redeem scripts.
	assertDepositHTLC(t, btcConn, makerDepositTxID, makerSession.pubkey(), to33(tkPub), createdA.HashedSecret, createdA.ALockTime)
	assertDepositHTLC(t, ltcConn, takerDepositTxID, to33(tkPub), makerSession.pubkey(), createdA.HashedSecret, createdB.BLockTime)

	// 5) ConfirmA (hub→maker's dest): maker redeems taker's deposit B, revealing
	// its secret on-chain.
	_, bodyCA, err := makerSession.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID, BDepositTxID: takerDepositTxID, BLockTime: createdB.BLockTime})
	if err != nil {
		t.Fatalf("maker OnConfirmA: %v", err)
	}
	confirmedA := bodyCA.(*proto.ConfirmedABody)
	makerPayTxID := confirmedA.APayTxID
	if makerPayTxID == "" {
		t.Fatal("empty maker payTx id")
	}
	// The maker's payTx must spend taker's deposit and reveal the secret.
	assertRevealsSecret(t, ltcConn, makerPayTxID, takerDepositTxID, makerSecret)

	// 6) ConfirmB (hub→taker's dest): taker recovers the secret from the maker's
	// payTx, then redeems maker's deposit A.
	_, bodyCB, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID})
	if err != nil {
		t.Fatalf("taker OnConfirmB: %v", err)
	}
	confirmedB := bodyCB.(*proto.ConfirmedBBody)
	takerPayTxID := confirmedB.BPayTxID
	if takerPayTxID == "" {
		t.Fatal("empty taker payTx id")
	}
	if takerSession.secret != makerSecret {
		t.Error("taker did not recover the maker's secret from the payTx")
	}
	// The taker's payTx must spend maker's deposit and reveal the (same) secret.
	assertRevealsSecret(t, btcConn, takerPayTxID, makerDepositTxID, makerSecret)

	// 7) Finished (hub→both).
	if _, _, err := makerSession.OnFinished(&proto.FinishedBody{ID: orderID}); err != nil {
		t.Fatalf("maker OnFinished: %v", err)
	}
	if _, _, err := takerSession.OnFinished(&proto.FinishedBody{ID: orderID}); err != nil {
		t.Fatalf("taker OnFinished: %v", err)
	}
	if makerSession.state != csFinished || takerSession.state != csFinished {
		t.Error("sessions did not reach csFinished")
	}
}

func (s *SwapSession) pubkey() [33]byte { return s.n.pubkey }

// to33 copies a (33-byte) compressed pubkey slice into a fixed [33]byte.
func to33(b []byte) [33]byte {
	var o [33]byte
	copy(o[:], b)
	return o
}

func makerSecretHash(t *testing.T, secret [33]byte) [20]byte {
	t.Helper()
	return coins.KeyID(secret[:])
}

// assertDepositHTLC decodes depositTxID from conn and checks its first output is
// the P2SH script for the expected HTLC redeem script.
func assertDepositHTLC(t *testing.T, conn *fakeConnector, depositTxID string, depositor, counterparty [33]byte, secretHash [20]byte, lockTime uint32) {
	t.Helper()
	raw, ok := conn.rawTx[depositTxID]
	if !ok {
		t.Fatalf("deposit %s not broadcast on %s", depositTxID, conn.ticker)
	}
	tx, err := coins.Deserialize(mustHex(raw))
	if err != nil {
		t.Fatalf("deserialize deposit: %v", err)
	}
	if len(tx.Outputs) == 0 {
		t.Fatal("deposit has no outputs")
	}
	scriptHash := coins.KeyID(coins.BuildDepositUnlockScript(depositor[:], counterparty[:], secretHash[:], lockTime))
	want := coins.BuildP2SHScript(scriptHash)
	if hex.EncodeToString(tx.Outputs[0].ScriptPubKey) != hex.EncodeToString(want) {
		t.Errorf("%s deposit P2SH output does not match expected HTLC redeem script", conn.ticker)
	}
}

// assertRevealsSecret decodes payTxID from conn and checks it spends the given
// deposit and that its scriptSig's first push is the 33-byte secret.
func assertRevealsSecret(t *testing.T, conn *fakeConnector, payTxID, depositTxID string, wantSecret [33]byte) {
	t.Helper()
	raw, ok := conn.rawTx[payTxID]
	if !ok {
		t.Fatalf("payTx %s not broadcast on %s", payTxID, conn.ticker)
	}
	tx, err := coins.Deserialize(mustHex(raw))
	if err != nil {
		t.Fatalf("deserialize payTx: %v", err)
	}
	if len(tx.Inputs) == 0 {
		t.Fatal("payTx has no inputs")
	}
	depHash, err := reverseTxidHex(depositTxID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Inputs[0].PrevOut.Hash != depHash {
		t.Errorf("%s payTx does not spend the expected deposit", conn.ticker)
	}
	got, ok := secretFromScriptSig(tx.Inputs[0].ScriptSig)
	if !ok {
		t.Fatalf("%s payTx scriptSig yields no 33-byte secret", conn.ticker)
	}
	if got != wantSecret {
		t.Errorf("%s payTx revealed secret mismatch", conn.ticker)
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
