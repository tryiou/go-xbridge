package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
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

// CheckDepositTransaction test-twin constants (wallet package versions are
// unexported): seqFinal is the deposit-input sequence requirement (C++
// xbridge::SEQUENCE_FINAL) and dblEps is std::numeric_limits<double>::epsilon().
const (
	seqFinal = 0xffffffff
	dblEps   = 2.220446049250313e-16
)

// hashToDisplayHex converts a 32-byte internal (little-endian) hash into its
// display-order txid hex.
func hashToDisplayHex(b []byte) string {
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		out[i] = b[31-i]
	}
	return hex.EncodeToString(out)
}

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

	// funders, when non-nil, is the UTXO set reported by ListUnspent, used by
	// tests where multiple concurrent takes must each reserve a distinct
	// funding utxo (the B2 funding path locks take #1's selection, so a
	// single-utxo fixture starves later takes).
	funders []wallet.Utxo

	// CheckDepositTransaction knobs: confirmations maps a broadcast txid to its
	// confirmations (a known tx without an entry fails the required-confirmations
	// gate → ErrDepositNotReady); feePerByte/minTxFee drive the C++ fee-band
	// checks (default 0 → no fee rejection).
	confirmations map[string]int
	feePerByte    uint64
	minTxFee      uint64

	mu         sync.Mutex
	broadcasts []string
	rawTx      map[string]string // display txid -> hex
}

func (f *fakeConnector) Ticker() string { return f.ticker }

func (f *fakeConnector) GetBalance() (uint64, error) {
	if len(f.funders) > 0 {
		var total uint64
		for _, u := range f.funders {
			total += u.Amount
		}
		return total, nil
	}
	return f.funding.Amount, nil
}

func (f *fakeConnector) GetNewAddress() (string, error) { return f.changeAddr, nil }

func (f *fakeConnector) ListUnspent(minConf int) ([]wallet.Utxo, error) {
	if len(f.funders) > 0 {
		return append([]wallet.Utxo(nil), f.funders...), nil
	}
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

// broadcastSnapshot returns a copy of the recorded broadcasts under the lock,
// so a started-engine test can observe worker RPCs from the test goroutine.
func (f *fakeConnector) broadcastSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.broadcasts))
	copy(out, f.broadcasts)
	return out
}

// setRawTx seeds the connector's rawTx map under the lock (used to stand in for
// the maker's payTx when driving ConfirmB in started mode).
func (f *fakeConnector) setRawTx(txid, hexStr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawTx[txid] = hexStr
}

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

// chainScale returns the coin's native base scale (10^Decimals), falling back
// to 1e8 when the coin registry is not initialized (fixtures that never call
// coins.InitFromConf).
func (f *fakeConnector) chainScale() float64 {
	dec := 8
	if c, ok := coins.Get(f.ticker); ok && c.Decimals > 0 {
		dec = c.Decimals
	}
	s := 1.0
	for i := 0; i < dec; i++ {
		s *= 10
	}
	return s
}

// minTxFeeWhole mirrors C++ minTxFee1/minTxFee2 in whole coins (the fake's
// in-memory twin of RPCConnector.minTxFeeWhole).
func (f *fakeConnector) minTxFeeWhole(nIn, nOut int) float64 {
	fee := uint64(192*nIn+34*nOut) * f.feePerByte
	if fee < f.minTxFee {
		fee = f.minTxFee
	}
	return float64(fee) / f.chainScale()
}

// CheckDepositTransaction is the in-memory twin of RPCConnector.
// CheckDepositTransaction: it validates a counterparty deposit against its own
// broadcast log (plus the known funding UTXOs as prevout sources), so the F85
// wiring tests can drive the full C++ tri-state (ready / bad / wait) without a
// wallet.
func (f *fakeConnector) CheckDepositTransaction(depositTxID, expectedScriptHex string, expectedAmount uint64, requiredConfirmations int) (wallet.DepositCheck, error) {
	dc := wallet.DepositCheck{}

	f.mu.Lock()
	rawHex, ok := f.rawTx[depositTxID]
	confsOK := true
	if ok && requiredConfirmations > 0 {
		c, okc := f.confirmations[depositTxID]
		confsOK = okc && c >= requiredConfirmations
	}
	known := make(map[string]wallet.Utxo, len(f.funders)+1)
	if len(f.funders) > 0 {
		for _, u := range f.funders {
			known[u.TxID] = u
		}
	} else {
		known[f.funding.TxID] = f.funding
	}
	rawLog := make(map[string]string, len(f.rawTx))
	for k, v := range f.rawTx {
		rawLog[k] = v
	}
	f.mu.Unlock()

	if !ok || !confsOK {
		return dc, wallet.ErrDepositNotReady
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return dc, nil // done: bad
	}
	tx, err := coins.Deserialize(raw)
	if err != nil {
		return dc, nil // done: bad
	}
	if len(tx.Inputs) == 0 || len(tx.Outputs) == 0 {
		return dc, nil // done: bad
	}

	scale := f.chainScale()
	// Vin scan: sequence + prevout amounts.
	var totalVinAmount float64
	for i := range tx.Inputs {
		vin := &tx.Inputs[i]
		if vin.Sequence != seqFinal {
			return dc, nil // bad sequence
		}
		vinTxID := hashToDisplayHex(vin.PrevOut.Hash[:])
		vinAmount, found := 0.0, false
		if rawHex2, ok := rawLog[vinTxID]; ok {
			if b, err := hex.DecodeString(rawHex2); err == nil {
				if vtx, err := coins.Deserialize(b); err == nil && int(vin.PrevOut.Index) < len(vtx.Outputs) {
					vinAmount, found = float64(vtx.Outputs[vin.PrevOut.Index].Value)/scale, true
				}
			}
		} else if u, ok := known[vinTxID]; ok && u.Vout == vin.PrevOut.Index {
			vinAmount, found = float64(u.Amount)/scale, true
		}
		if !found {
			return dc, wallet.ErrDepositNotReady // vin tx not found ...waiting
		}
		totalVinAmount += vinAmount
	}

	// Vout scan for the expected p2sh.
	var totalVoutAmount, depositP2SHAmount float64
	var depositTxVout uint32
	for i := range tx.Outputs {
		out := &tx.Outputs[i]
		whole := float64(out.Value) / scale
		totalVoutAmount += whole
		if hex.EncodeToString(out.ScriptPubKey) != expectedScriptHex {
			continue
		}
		if float64(expectedAmount)/coinScale <= whole+dblEps {
			depositP2SHAmount = whole
			depositTxVout = uint32(i)
		}
		break // done searching
	}
	if depositP2SHAmount == 0 {
		return dc, nil // no valid p2sh
	}

	// Fee checks.
	counterpartyFees := totalVinAmount - totalVoutAmount
	fee1 := f.minTxFeeWhole(len(tx.Inputs), len(tx.Outputs))
	fee2 := f.minTxFeeWhole(1, 1)
	if counterpartyFees < 0 || counterpartyFees < fee1*0.95 {
		return dc, nil // not enough to cover deposit fees
	}
	wholeAmount := float64(expectedAmount) / coinScale
	if depositP2SHAmount < wholeAmount+fee2*0.95 {
		return dc, nil // not enough to cover redeem fees
	}
	if depositP2SHAmount > wholeAmount+fee2 {
		dc.Excess = uint64((depositP2SHAmount - wholeAmount - fee2) * coinScale)
	}
	dc.P2SHAmount = uint64(depositP2SHAmount * coinScale)
	dc.DepositVout = depositTxVout
	dc.IsGood = true
	return dc, nil
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

	makerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	takerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}

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
	if _, _, err := makerSession.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID, FromAmount: 2.5e6, ToAmount: 2e6}); err != nil {
		t.Fatalf("maker OnHold: %v", err)
	}
	if _, _, err := takerSession.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID, FromAmount: 2.5e6, ToAmount: 2e6}); err != nil {
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

// TestDepositNativeScale (W0) pins the deposit-path unit-scale fix: on-chain
// output values must be in the coin's NATIVE base (10^Decimals), never the
// XBridge 1e6 base of the order amounts. C++ locks outAmount+fee2 whole coins
// and createDepositTransaction emits out.second*COIN(native)
// (xbridgewalletconnectorbtc.cpp:2094, 2442-2450); for BTC (1e8) the pre-fix Go
// deposit locked 100x too little. The order is 2.5e6→2e6 XBridge units (2.5 BTC
// / 2 LTC), funded by 5e8-sat UTXOs; fees default to 2 sat/vB
// (192/34 vsize: fee=(192*1+34*2)*2=520, fee2=(192+34)*2=452).
func TestDepositNativeScale(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	btcCoin, _ := coins.Get("BTC")
	ltcCoin, _ := coins.Get("LTC")

	mkPriv, mkPub := newKey(t)
	tkPriv, tkPub := newKey(t)
	mkAddr := addrFor(0, "maker-btc-dest")
	ltcAddr := addrFor(48, "taker-ltc-source")

	mkBtc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8, ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(mkPub)))}, fundingPriv: mkPriv, fundingPub: mkPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{}}
	tkLtc := &fakeConnector{ticker: "LTC", funding: wallet.Utxo{TxID: strings.Repeat("bb", 32), Vout: 0, Amount: 5e8, ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(tkPub)))}, fundingPriv: tkPriv, fundingPub: tkPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000, rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	makerNode := newTestNode(t, confs, map[string]wallet.Connector{"BTC": mkBtc, "LTC": tkLtc})
	takerNode := newTestNode(t, confs, map[string]wallet.Connector{"BTC": mkBtc, "LTC": tkLtc})

	var orderID [32]byte
	nsHash := hash20("native-scale-order")
	copy(orderID[:], nsHash[:])
	mkOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	tkOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	mkMPriv, mkMPub := newKey(t)
	tkMPriv := tkPriv
	tkMPub := tkPub
	makerNode.newMakerSession(mkOrder, MakeOrderParams{MakerAddress: mkAddr, TakerAddress: ltcAddr}, arr32(mkMPriv), toArr33(mkMPub))
	takerNode.newTakerSession(tkOrder, TakeOrderParams{FromAddress: ltcAddr, ToAddress: mkAddr}, arr32(tkMPriv), to33(tkMPub))
	var hub [20]byte
	hubHash := hash20("hub")
	copy(hub[:], hubHash[:])
	makerSession := makerNode.sessions[hexEncode(orderID[:])]
	takerSession := takerNode.sessions[hexEncode(orderID[:])]
	makerSession.hub = hub
	takerSession.hub = hub
	makerSecret := makerSession.secret

	const fee = 520  // (192*1 + 34*2) * 2 sat/vB
	const fee2 = 452 // (192*1 + 34*1) * 2 sat/vB

	// Maker deposit A (BTC): locks native(fromXBridgeAmt(2.5e6)) + fee2.
	_, bodyA, err := makerSession.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkPub)})
	if err != nil {
		t.Fatalf("maker OnCreateA: %v", err)
	}
	createdA := bodyA.(*proto.CreatedABody)
	depA := deserializeBroadcast(t, mkBtc, createdA.ADepositTxID)
	nativeAmt := fromXBridgeAmt(btcCoin, 2.5e6)
	if got := depA.Outputs[0].Value; got != nativeAmt+fee2 {
		t.Fatalf("deposit A p2sh value = %d, want native %d + fee2 %d = %d (pre-fix: 100x under-lock)", got, nativeAmt, fee2, nativeAmt+fee2)
	}
	if got := depA.Outputs[1].Value; got != 5e8-nativeAmt-fee-fee2 {
		t.Fatalf("deposit A change = %d, want %d", got, 5e8-nativeAmt-fee-fee2)
	}

	// Pre-signed refund pays the full native amount (fee2 is the miner fee) and
	// spends the deposit via its LOCALLY-derived txid (CRYPTO-F86: the refund
	// is built from the local txid before the broadcast).
	refund := deserializeHex(t, createdA.RefTx)
	if got := refund.Outputs[0].Value; got != nativeAmt-fee2 {
		t.Fatalf("refund output = %d, want native %d - fee2 %d = %d", got, nativeAmt, fee2, nativeAmt-fee2)
	}
	if rev, err := reverseTxidHex(createdA.ADepositTxID); err != nil || refund.Inputs[0].PrevOut.Hash != rev {
		t.Fatalf("refund prevout does not reference the deposit txid %s (err %v)", createdA.ADepositTxID, err)
	}

	// Taker deposit B (LTC): locks native(fromXBridgeAmt(2e6)) + fee2.
	_, bodyB, err := takerSession.OnCreateB(&proto.CreateBBody{
		HubAddress: hub, ID: orderID, APubKey: makerSession.pubkey(),
		ADepositTxID: createdA.ADepositTxID, HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime,
	})
	if err != nil {
		t.Fatalf("taker OnCreateB: %v", err)
	}
	createdB := bodyB.(*proto.CreatedBBody)
	depB := deserializeBroadcast(t, tkLtc, createdB.BDepositTxID)
	nativeTaker := fromXBridgeAmt(ltcCoin, 2e6)
	if got := depB.Outputs[0].Value; got != nativeTaker+fee2 {
		t.Fatalf("deposit B p2sh value = %d, want %d", got, nativeTaker+fee2)
	}

	// Maker redeems deposit B: output = native(toAmount) − fee2.
	_, bodyCA, err := makerSession.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID, BDepositTxID: createdB.BDepositTxID, BLockTime: createdB.BLockTime})
	if err != nil {
		t.Fatalf("maker OnConfirmA: %v", err)
	}
	payA := deserializeBroadcast(t, tkLtc, bodyCA.(*proto.ConfirmedABody).APayTxID)
	if got := payA.Outputs[0].Value; got != nativeTaker-fee2 {
		t.Fatalf("maker redeem output = %d, want native %d - fee2 %d = %d", got, nativeTaker, fee2, nativeTaker-fee2)
	}

	// Taker redeems deposit A: output = native(fromAmount) − fee2.
	_, bodyCB, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: bodyCA.(*proto.ConfirmedABody).APayTxID})
	if err != nil {
		t.Fatalf("taker OnConfirmB: %v", err)
	}
	payB := deserializeBroadcast(t, mkBtc, bodyCB.(*proto.ConfirmedBBody).BPayTxID)
	if got := payB.Outputs[0].Value; got != nativeAmt-fee2 {
		t.Fatalf("taker redeem output = %d, want native %d - fee2 %d = %d", got, nativeAmt, fee2, nativeAmt-fee2)
	}
	_ = makerSecret
}

func deserializeBroadcast(t *testing.T, conn *fakeConnector, txid string) *coins.Tx {
	t.Helper()
	raw, ok := conn.rawTx[txid]
	if !ok {
		t.Fatalf("tx %s not broadcast on %s", txid, conn.ticker)
	}
	return deserializeHex(t, raw)
}

func deserializeHex(t *testing.T, hexStr string) *coins.Tx {
	t.Helper()
	tx, err := coins.Deserialize(mustHex(hexStr))
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	return tx
}

// TestDepositNotBroadcastWhenRefundFails (CRYPTO-F86) proves the build order:
// the deposit must be signed and the CLTV refund pre-built BEFORE the broadcast,
// so a refund-build failure never strands a broadcast deposit without an escape
// hatch (C++ builds deposit → refund → then broadcasts). An undecodable
// refund destination makes buildRefundTx fail after signing; the connector must
// show zero broadcasts.
func TestDepositNotBroadcastWhenRefundFails(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	priv, pub := newKey(t)
	_, tkPub := newKey(t)
	fundingPriv, fundingPub := newKey(t)
	btc := &fakeConnector{
		ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8, ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(fundingPub)))},
		fundingPriv: fundingPriv, fundingPub: fundingPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{},
	}
	confs := map[string]*config.CoinConf{"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60}}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": btc})

	var orderID [32]byte
	rfoHash := hash20("refund-fails-order")
	copy(orderID[:], rfoHash[:])
	n.newMakerSession(&Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6},
		MakeOrderParams{MakerAddress: "not-a-valid-address", TakerAddress: addrFor(0, "taker-dest")}, arr32(priv), toArr33(pub))
	s := n.sessions[hexEncode(orderID[:])]

	_, _, err := s.OnCreateA(&proto.CreateABody{ID: orderID, BPubKey: to33(tkPub)})
	if err == nil {
		t.Fatal("expected refund-build failure (bad refund destination)")
	}
	if got := len(btc.broadcasts); got != 0 {
		t.Fatalf("deposit broadcast %d time(s) despite refund-build failure; the refund must be pre-built before broadcasting (CRYPTO-F86)", got)
	}
}

// TestCreateBBadLocktimeCancels proves the wire-Cancel path for a rejected
// counterparty: a CreateB whose ALockTime fails the drift check must broadcast
// a signed Cancel packet (reason crBadALockTime=18, C++ :2464-2472) AND roll
// the order back locally (no deposit was sent → status "canceled"), and must
// NOT respond with CreatedB.
func TestCreateBBadLocktimeCancels(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	tkPriv, tkPub := newKey(t)
	_, makerPub := newKey(t)
	fundingPriv, fundingPub := newKey(t)
	btc := &fakeConnector{
		ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8, ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(fundingPub)))},
		fundingPriv: fundingPriv, fundingPub: fundingPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{},
	}
	confs := map[string]*config.CoinConf{"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60}}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": btc})
	cc := &captureXConn{}
	n.conn = cc

	var orderID [32]byte
	bloHash := hash20("bad-locktime-order")
	copy(orderID[:], bloHash[:])
	n.newTakerSession(&Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6},
		TakeOrderParams{FromAddress: addrFor(0, "from"), ToAddress: addrFor(0, "to")}, arr32(tkPriv), to33(tkPub))
	// Seed the store order (the taker's copy) so the self-cancel's local rollback
	// can find it and cancel it. MakerKey = OUR per-trade M pubkey (order.go:96).
	n.store.Add(&Order{
		ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		Status: "open", MakerKey: hexEncode(tkPub[:]),
	})
	s := n.sessions[hexEncode(orderID[:])]

	_, _, err := s.OnCreateB(&proto.CreateBBody{
		HubAddress: [20]byte{}, ID: orderID, APubKey: to33(makerPub),
		ADepositTxID: strings.Repeat("bb", 32), HashedSecret: [20]byte{0x11},
		ALockTime: 0, // fails the drift check (expected ~blockTime+2h)
	})
	if err == nil {
		t.Fatal("expected locktime-drift rejection")
	}
	var sce *selfCancelErr
	if !errors.As(err, &sce) || sce.reason != crBadALockTime {
		t.Fatalf("err = %v, want selfCancelErr reason 18 (crBadALockTime)", err)
	}
	// A Cancel packet must have been broadcast, signed with our M key.
	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("broadcast packets = %d (cmd %v), want exactly one Cancel", len(pkts), pkts[0].Command)
	}
	var cancel proto.CancelBody
	if err := cancel.Unmarshal(pkts[0].Body); err != nil {
		t.Fatalf("cancel body: %v", err)
	}
	if cancel.Reason != crBadALockTime {
		t.Fatalf("cancel reason = %d, want 18", cancel.Reason)
	}
	if ok, _ := n.signer.VerifyAgainst(pkts[0], hexEncode(tkPub[:])); !ok {
		t.Fatal("cancel packet not signed with our per-trade M key")
	}
	// Local rollback: the pre-deposit order is canceled (moved to history with
	// the cancel reason), never a CreatedB.
	if h := n.store.HistoryOrder(hexEncode(orderID[:])); h == nil || h.Status != "canceled" {
		t.Fatalf("history order = %+v, want canceled", h)
	}
	if n.store.Get(hexEncode(orderID[:])) != nil {
		t.Fatal("order must be removed from the live store after the self-cancel")
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
	n.newMakerSession(&Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6}, MakeOrderParams{MakerAddress: mkAddr, TakerAddress: mkAddr}, arr32(mPriv), toArr33(mPub))
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
