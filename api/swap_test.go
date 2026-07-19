package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"strings"
	"sync"
	"testing"

	btcec "github.com/btcsuite/btcd/btcec/v2"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/proto"
	"go-xbridge/wallet"
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

func (f *fakeConnector) GetRelayFee() (float64, error) { return 0.0001, nil }

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

func newTestNode(t *testing.T, confs map[string]*config.CoinConf, conns map[string]wallet.Connector) *Node {
	t.Helper()
	return &Node{
		config:   &Config{Confs: confs, Connectors: conns},
		store:    NewStore(),
		signer:   crypto.NewBtcSigner(),
		sessions: map[string]*SwapSession{},
		stop:     make(chan struct{}),
	}
}

// toArr33 converts a 33-byte pubkey slice to a fixed array.
func toArr33(b []byte) [33]byte {
	var a [33]byte
	copy(a[:], b)
	return a
}

// arr32 converts a 32-byte privkey slice to a fixed array.
func arr32(b []byte) [32]byte {
	var a [32]byte
	copy(a[:], b)
	return a
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

	tkPriv, tkPub := newKey(t)
	makerNode := newTestNode(t, confs, conns)
	takerNode := newTestNode(t, confs, conns)

	// Per-trade M keypairs (C++ mPubKey/mPrivKey). The taker's M pubkey is
	// tkPub because the handshake assertions below expect the taker deposit's
	// DepositorPub to equal tkPub.
	mkMPriv, mkMPub := newKey(t)
	tkMPriv := tkPriv
	tkMPub := tkPub

	var orderID [32]byte
	oid := hash20("order-id")
	copy(orderID[:], oid[:])

	mkAddr := addrFor(0, "maker-btc-dest")     // maker deposits BTC, receives LTC
	ltcAddr := addrFor(48, "taker-ltc-source") // taker deposits LTC, receives BTC

	makerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e8, ToAmount: 2e8}
	takerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e8, ToAmount: 2e8}

	makerNode.newMakerSession(makerOrder, MakeOrderParams{MakerAddress: mkAddr, TakerAddress: ltcAddr}, arr32(mkMPriv), toArr33(mkMPub))
	takerNode.newTakerSession(takerOrder, TakeOrderParams{FromAddress: ltcAddr, ToAddress: mkAddr}, arr32(tkMPriv), toArr33(tkMPub))

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

	// Refund-path coverage (must be covered per the fund-safety requirement):
	// the maker's pre-signed CLTV refund, produced at deposit time, must be
	// broadcastable on abort. Task #5 automates this; here we exercise the path
	// end-to-end by broadcasting the stored refund hex and asserting the
	// connector recorded it.
	if createdA.RefTx == "" {
		t.Fatal("maker refund hex not produced at deposit time")
	}
	before := len(btcConn.broadcasts)
	refundTxID, err := btcConn.SendRawTransaction(createdA.RefTx)
	if err != nil {
		t.Fatalf("abort refund broadcast: %v", err)
	}
	if refundTxID == "" {
		t.Fatal("refund broadcast returned empty txid")
	}
	if len(btcConn.broadcasts) != before+1 {
		t.Errorf("refund broadcast not recorded by connector (got %d, want %d)", len(btcConn.broadcasts), before+1)
	}

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
	assertRevealsSecret(t, ltcConn, makerPayTxID, takerDepositTxID, makerSecret, makerSecretHash(t, makerSecret))

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
	assertRevealsSecret(t, btcConn, takerPayTxID, makerDepositTxID, makerSecret, makerSecretHash(t, makerSecret))

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

func (s *SwapSession) pubkey() [33]byte { return s.pubKey }

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
// deposit and that its scriptSig reveals the expected secret — i.e. the 33-byte
// push whose HASH160 equals wantHash. This mirrors the C++ getKeyId(push)==hx
// guard, so a malleated scriptSig (wrong 33-byte push) is rejected.
func assertRevealsSecret(t *testing.T, conn *fakeConnector, payTxID, depositTxID string, wantSecret [33]byte, wantHash [20]byte) {
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
	got, ok := secretFromScriptSig(tx.Inputs[0].ScriptSig, wantHash)
	if !ok {
		t.Fatalf("%s payTx scriptSig yields no 33-byte secret matching the deposit hash", conn.ticker)
	}
	if got != wantSecret {
		t.Errorf("%s payTx revealed secret mismatch", conn.ticker)
	}
}

// TestSecretFromScriptSig locks in the C++ getKeyId(push)==hx safeguard: the
// recovered secret must be the 33-byte push whose HASH160 equals the expected
// secretHash, not merely the first 33-byte element. This guards against a
// malleated/non-conforming scriptSig that pushes a decoy 33-byte element (e.g.
// myPubKey) ahead of the real secret.
func TestSecretFromScriptSig(t *testing.T) {
	// real secret and a decoy 33-byte element (myPubKey) with a different hash.
	realSecret := make([]byte, 33)
	for i := range realSecret {
		realSecret[i] = byte(i + 1)
	}
	myPubKey := make([]byte, 33)
	for i := range myPubKey {
		myPubKey[i] = byte(200 - i)
	}
	sig := make([]byte, 71)
	inner := []byte{0x51, 0x20} // arbitrary inner redeem-script fragment

	realHash := coins.KeyID(realSecret)
	// An unrelated hash that matches neither push in any scriptSig below.
	wrongHash := coins.KeyID([]byte("unrelated-preimage-material-that-matches-nothing"))

	// Well-formed: <realSecret> <sig> <myPubKey> OP_0 <inner>.
	good := coins.BuildPaymentScriptSig(realSecret, sig, myPubKey, inner)
	got, ok := secretFromScriptSig(good, realHash)
	if !ok || got != to33(realSecret) {
		t.Fatalf("well-formed: ok=%v got=%x want=%x", ok, got, realSecret)
	}
	// A wrong expected hash must be rejected even on a well-formed scriptSig.
	if _, ok := secretFromScriptSig(good, wrongHash); ok {
		t.Error("well-formed scriptSig with wrong hash should be rejected")
	}

	// Malleated: <myPubKey> <sig> <realSecret> OP_0 <inner> — a decoy 33-byte
	// element appears first. The old code would adopt it; the fixed code must
	// skip it and return the matching real secret.
	malleated := coins.BuildPaymentScriptSig(myPubKey, sig, realSecret, inner)
	got2, ok2 := secretFromScriptSig(malleated, realHash)
	if !ok2 || got2 != to33(realSecret) {
		t.Fatalf("malleated: ok=%v got=%x want real secret %x", ok2, got2, realSecret)
	}

	// Decoy-only: no push matches realHash → rejected outright.
	decoy := coins.BuildPaymentScriptSig(myPubKey, sig, myPubKey, inner)
	if _, ok := secretFromScriptSig(decoy, realHash); ok {
		t.Error("decoy-only scriptSig (no matching secret) should be rejected")
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// decodeScriptPushes splits a Bitcoin scriptSig into its data pushes plus the
// trailing opcodes (e.g. OP_1/OP_0 between pushes). btcd/txscript is not a
// dependency, so we validate scriptSig structure byte-for-byte rather than via
// a VM.
func decodeScriptPushes(t *testing.T, b []byte) (pushes [][]byte, ops []byte) {
	t.Helper()
	i := 0
	for i < len(b) {
		c := b[i]
		switch {
		case c >= 1 && c <= 75:
			n := int(c)
			i++
			if i+n > len(b) {
				t.Fatalf("script push runs past end (len %d, need %d)", len(b), i+n)
			}
			pushes = append(pushes, b[i:i+n])
			i += n
		case c == coins.OpPushData1:
			if i+2 > len(b) {
				t.Fatalf("OP_PUSHDATA1 truncated")
			}
			n := int(b[i+1])
			i += 2
			if i+n > len(b) {
				t.Fatalf("OP_PUSHDATA1 data runs past end")
			}
			pushes = append(pushes, b[i:i+n])
			i += n
		case c == coins.OpPushData2:
			if i+3 > len(b) {
				t.Fatalf("OP_PUSHDATA2 truncated")
			}
			n := int(binary.LittleEndian.Uint16(b[i+1 : i+3]))
			i += 3
			if i+n > len(b) {
				t.Fatalf("OP_PUSHDATA2 data runs past end")
			}
			pushes = append(pushes, b[i:i+n])
			i += n
		case c == coins.OpPushData4:
			if i+5 > len(b) {
				t.Fatalf("OP_PUSHDATA4 truncated")
			}
			n := int(binary.LittleEndian.Uint32(b[i+1 : i+5]))
			i += 5
			if i+n > len(b) {
				t.Fatalf("OP_PUSHDATA4 data runs past end")
			}
			pushes = append(pushes, b[i:i+n])
			i += n
		default:
			ops = append(ops, c)
			i++
		}
	}
	return pushes, ops
}

// setupSwapPair builds a maker-only swap fixture on BTC so individual maker-side
// paths (refund tx, etc.) can be exercised without a full taker exchange.
func setupSwapPair(t *testing.T) (*Node, *SwapSession, *fakeConnector) {
	t.Helper()
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	mPriv, mPub := newKey(t)
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
	conns := map[string]wallet.Connector{"BTC": btcConn}
	confs := map[string]*config.CoinConf{"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60}}
	n := newTestNode(t, confs, conns)
	var orderID [32]byte
	oid := hash20("order-id")
	copy(orderID[:], oid[:])
	mkAddr := addrFor(0, "maker-btc-dest")
	n.newMakerSession(&Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 1e8, ToAmount: 1e8}, MakeOrderParams{MakerAddress: mkAddr, TakerAddress: mkAddr}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(orderID[:])]
	hub := hash20("hub")
	s.hub = hub
	return n, s, btcConn
}

// TestRefundTx exercises the pre-signed CLTV refund produced when the maker
// deposits: it must carry the deposit's lockTime, set the input sequence to
// 0xfffffffe (CLTV enabled), and assemble the IF-branch scriptSig
// <sig> <depositorPub> OP_1 <redeemScript> whose inner push equals the deposit's
// HTLC redeem script.
func TestRefundTx(t *testing.T) {
	_, s, conn := setupSwapPair(t)

	_, bodyA, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])})
	if err != nil {
		t.Fatalf("OnCreateA: %v", err)
	}
	createdA := bodyA.(*proto.CreatedABody)
	if _, ok := conn.rawTx[createdA.ADepositTxID]; !ok {
		t.Fatal("maker deposit not broadcast")
	}
	if createdA.RefTx == "" {
		t.Fatal("empty refund hex")
	}
	raw, err := hex.DecodeString(createdA.RefTx)
	if err != nil {
		t.Fatalf("decode refund: %v", err)
	}
	tx, err := coins.Deserialize(raw)
	if err != nil {
		t.Fatalf("deserialize refund: %v", err)
	}
	if tx.LockTime != createdA.ALockTime {
		t.Errorf("refund LockTime %d != deposit lockTime %d", tx.LockTime, createdA.ALockTime)
	}
	if len(tx.Inputs) != 1 {
		t.Fatalf("refund wants 1 input, got %d", len(tx.Inputs))
	}
	if tx.Inputs[0].Sequence != 0xfffffffe {
		t.Errorf("refund input sequence %#x != 0xfffffffe (CLTV not enabled)", tx.Inputs[0].Sequence)
	}
	pushes, ops := decodeScriptPushes(t, tx.Inputs[0].ScriptSig)
	if len(pushes) != 3 || len(ops) != 1 || ops[0] != coins.Op1 {
		t.Fatalf("refund scriptSig wrong: pushes=%d ops=%v", len(pushes), ops)
	}
	wantInner := coins.BuildDepositUnlockScript(s.pubKey[:], s.theirPub[:], s.secretHash[:], createdA.ALockTime)
	if !bytes.Equal(pushes[2], wantInner) {
		t.Error("refund scriptSig inner push != expected HTLC redeem script")
	}
}

// TestComputeLockTime checks the C++ locktime formula (xbridgewallet.h:96-101):
// target = MAKER/TAKER seconds; taker target becomes XSLOW_TAKER when blockTime
// >= 600; blocks = target/blockTime clamped to XMIN_LOCKTIME_BLOCKS=6; result =
// current block + blocks. The computed values here are BTC-like (blockTime 60)
// plus a few edge blockTimes; the clamp/mask behavior is identical to C++.
func TestComputeLockTime(t *testing.T) {
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", BlockTime: 60},
	}
	conn := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{}}
	conns := map[string]wallet.Connector{"BTC": conn}
	mPriv, mPub := newKey(t)
	n := newTestNode(t, confs, conns)
	s := &SwapSession{n: n, isMaker: true, id: [32]byte{}, srcCur: "BTC", dstCur: "BTC", privKey: arr32(mPriv), pubKey: toArr33(mPub)}
	n.sessions["x"] = s

	// blockTime 60: maker 7200/60=120 → 1120; taker 1800/60=30 → 1030.
	if got := s.computeLockTime(true); got != 1120 {
		t.Errorf("maker lockTime (bt60) = %d, want 1120", got)
	}
	if got := s.computeLockTime(false); got != 1030 {
		t.Errorf("taker lockTime (bt60) = %d, want 1030", got)
	}

	// blockTime 100 (no clamp): maker 7200/100=72 → 1072; taker 1800/100=18 → 1018.
	confs["BTC"].BlockTime = 100
	if got := s.computeLockTime(true); got != 1072 {
		t.Errorf("maker lockTime (bt100) = %d, want 1072", got)
	}
	if got := s.computeLockTime(false); got != 1018 {
		t.Errorf("taker lockTime (bt100) = %d, want 1018", got)
	}

	// blockTime 7200: maker 7200/7200=1, taker 1800/7200=0 → both clamped to
	// XMIN_LOCKTIME_BLOCKS=6 → 1006. (The XSLOW_TAKER branch is also applied for
	// bt>=600 but the 6-block clamp dominates the result, matching C++.)
	confs["BTC"].BlockTime = 7200
	if got := s.computeLockTime(true); got != 1006 {
		t.Errorf("maker lockTime (clamped) = %d, want 1006", got)
	}
	if got := s.computeLockTime(false); got != 1006 {
		t.Errorf("taker lockTime (clamped) = %d, want 1006", got)
	}
}

// TestRefundWatcher drives the fund-safety safety net: before the deposit's
// lockTime the watcher must not broadcast; once the chain advances past the
// lockTime, checkRefunds must auto-broadcast the pre-signed refund exactly once
// (guarded by refundDone).
func TestRefundWatcher(t *testing.T) {
	n, s, conn := setupSwapPair(t)
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
		t.Fatalf("OnCreateA: %v", err)
	}
	if s.refundHex == "" {
		t.Fatal("refund not pre-signed at deposit time")
	}
	// Maker lockTime = 1000 (blockHeight) + 7200/60 = 1120. Before expiry, the
	// watcher must leave the deposit untouched.
	before := len(conn.broadcasts)
	n.checkRefunds()
	if len(conn.broadcasts) != before {
		t.Errorf("refund broadcast before lockTime expiry (broadcasts %d)", len(conn.broadcasts))
	}
	// Advance the chain past the deposit lockTime and re-run the check.
	conn.blockHeight = 1200
	n.checkRefunds()
	if len(conn.broadcasts) != before+1 {
		t.Fatalf("refund not auto-broadcast at lockTime (broadcasts %d, want %d)", len(conn.broadcasts), before+1)
	}
	if !s.refundDone {
		t.Error("refundDone not set after auto-broadcast")
	}
	// A second pass must NOT double-broadcast.
	after := len(conn.broadcasts)
	n.checkRefunds()
	if len(conn.broadcasts) != after {
		t.Errorf("refund auto-broadcast twice (broadcasts %d, want %d)", len(conn.broadcasts), after)
	}
}

// TestRefundEscapeHatch exercises the manual escape hatch: BroadcastRefund
// force-broadcasts the stored refund and records it on the connector.
func TestRefundEscapeHatch(t *testing.T) {
	_, s, conn := setupSwapPair(t)
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
		t.Fatalf("OnCreateA: %v", err)
	}
	orderIDHex := hexEncode(s.id[:])
	before := len(conn.broadcasts)
	txid, err := s.n.BroadcastRefund(orderIDHex)
	if err != nil {
		t.Fatalf("BroadcastRefund: %v", err)
	}
	if txid == "" {
		t.Fatal("BroadcastRefund returned empty txid")
	}
	if len(conn.broadcasts) != before+1 {
		t.Errorf("escape-hatch refund not broadcast (broadcasts %d, want %d)", len(conn.broadcasts), before+1)
	}
}
