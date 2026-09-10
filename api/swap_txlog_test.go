package api

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	xlog "go-xbridge/log"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// txlogTestDir enables the swap transcript under a temp dir for the test and
// disables it afterward so transcript state never leaks between tests.
func txlogTestDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "log-tx")
	if err := xlog.SetTxLogDir(dir); err != nil {
		t.Fatalf("SetTxLogDir: %v", err)
	}
	t.Cleanup(func() {
		if err := xlog.SetTxLogDir(""); err != nil {
			t.Fatalf("SetTxLogDir reset: %v", err)
		}
	})
	return dir
}

func txlogToday(t *testing.T, dir string) string {
	t.Helper()
	// Glob (not wall-clock date): immune to a midnight rollover between the
	// transcript write and this read.
	files, err := filepath.Glob(filepath.Join(dir, "xbridgep2p_*.log"))
	if err != nil {
		t.Fatalf("glob transcript: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("transcript files = %d (%v), want exactly 1", len(files), files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	return string(data)
}

// TestTxLogDepositTranscript drives a real maker deposit and proves the
// dedicated transcript (not the general log) captures the complete manual
// refund record: display order id, deposit hex, locktime, and pre-signed
// refund hex — while the per-trade private key and HTLC secret stay out.
func TestTxLogDepositTranscript(t *testing.T) {
	dir := txlogTestDir(t)
	_, s, conn := setupSwapPair(t)

	_, bodyA, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])})
	if err != nil {
		t.Fatalf("OnCreateA: %v", err)
	}
	createdA := bodyA.(*proto.CreatedABody)
	depositHex, ok := conn.rawTx[createdA.ADepositTxID]
	if !ok {
		t.Fatal("maker deposit not broadcast")
	}

	got := txlogToday(t, dir)
	disp := orderIDString(s.id)
	for _, want := range []string{
		"order " + disp + " deposit transaction for order " + disp,
		"submit manually using sendrawtransaction",
		"using locktime",
		depositHex,
		"refund transaction for order " + disp,
		createdA.RefTx,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q, got:\n%s", want, got)
		}
	}
	// Private trade material must never reach the transcript.
	for _, secret := range []string{hexEncode(s.privKey[:]), hexEncode(s.secret[:])} {
		if strings.Contains(got, secret) {
			t.Errorf("transcript leaks private material %q", secret)
		}
	}
}

// TestTxLogHelpersFormat pins the taker-deposit, claim, and refund entry
// formats (same file, Core TXLOG shapes) without needing live counterparty
// fixtures — the maker integration above covers the hook wiring end to end.
func TestTxLogHelpersFormat(t *testing.T) {
	dir := txlogTestDir(t)
	var id [32]byte
	copy(id[:], []byte("txlog-helper-format-order000"))
	disp := orderIDString(id)

	txLogDeposit(id, "B", "PIVX", 2331000, "BLOCK", 500000, 812345, "deposithex", "refundhex")
	txLogClaim(id, "A", "PIVX", "paytxid", "payhex")
	txLogRefund(id, "BLOCK", 812000, "refundtxid")

	got := txlogToday(t, dir)
	for _, want := range []string{
		"deposit transaction for order " + disp + " (submit manually using sendrawtransaction) 2331000 PIVX -> 500000 BLOCK using locktime 812345",
		"deposithex",
		"refund transaction for order " + disp + " role B locktime 812345",
		"refundhex",
		"redeem counterparty deposit for order " + disp + " role A PIVX (submit manually using sendrawtransaction) paytx paytxid",
		"payhex",
		"refund broadcast for order " + disp + " BLOCK txid refundtxid locktime 812000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q, got:\n%s", want, got)
		}
	}
}

// setupTxLogPair builds a two-party maker(BTC)/taker(LTC) rig through Init,
// mirroring TestSwapHandshake's fixture so both deposits and both claims can
// be driven for transcript coverage.
func setupTxLogPair(t *testing.T) (*SwapSession, *SwapSession, *fakeConnector, *fakeConnector, [20]byte, [32]byte) {
	t.Helper()
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	btcHash := hash20("maker-btc-dest")
	ltcHash := hash20("taker-ltc-source")

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

	mkMPriv, mkMPub := newKey(t)
	var orderID [32]byte
	oid := hash20("order-id")
	copy(orderID[:], oid[:])

	mkAddr := addrFor(0, "maker-btc-dest")
	ltcAddr := addrFor(48, "taker-ltc-source")
	makerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	takerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	makerNode.newMakerSession(withUsedCoins(t, makerNode, makerOrder, []wallet.Utxo{btcFunding}), MakeOrderParams{MakerAddress: mkAddr, TakerAddress: ltcAddr}, arr32(mkMPriv), toArr33(mkMPub))
	takerNode.newTakerSession(withUsedCoins(t, takerNode, takerOrder, []wallet.Utxo{ltcFunding}), TakeOrderParams{FromAddress: ltcAddr, ToAddress: mkAddr}, arr32(tkPriv), toArr33(tkPub))

	var hub [20]byte
	hb := hash20("hub")
	copy(hub[:], hb[:])
	makerSession := makerNode.sessions[hexEncode(orderID[:])]
	takerSession := takerNode.sessions[hexEncode(orderID[:])]
	makerSession.hub = hub
	takerSession.hub = hub

	if _, _, err := makerSession.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID, FromAmount: 2e6, ToAmount: 2.5e6}); err != nil {
		t.Fatalf("maker OnHold: %v", err)
	}
	if _, _, err := takerSession.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID, FromAmount: 2e6, ToAmount: 2.5e6}); err != nil {
		t.Fatalf("taker OnHold: %v", err)
	}
	mkInit := &proto.InitBody{
		ClientAddress: ltcHash, HubAddress: hub, ID: orderID,
		FromAddress: hash20("maker-btc-dest"), FromCurrency: "BTC", FromAmount: 2.5e6,
		ToAddress: hash20("taker-ltc-source"), ToCurrency: "LTC", ToAmount: 2e6,
	}
	if _, _, err := makerSession.OnInit(mkInit); err != nil {
		t.Fatalf("maker OnInit: %v", err)
	}
	tkInit := &proto.InitBody{
		ClientAddress: btcHash, HubAddress: hub, ID: orderID,
		FromAddress: hash20("taker-ltc-source"), FromCurrency: "LTC", FromAmount: 2e6,
		ToAddress: hash20("maker-btc-dest"), ToCurrency: "BTC", ToAmount: 2.5e6,
	}
	if _, _, err := takerSession.OnInit(tkInit); err != nil {
		t.Fatalf("taker OnInit: %v", err)
	}
	return makerSession, takerSession, btcConn, ltcConn, hub, orderID
}

// TestTxLogClaimTranscript drives both claims through the real worker path
// and proves each broadcast payTx hex lands in the dedicated transcript (a
// broadcast payTx is public chain data — it reveals the secret on-chain by
// design — while both M privkeys stay out).
func TestTxLogClaimTranscript(t *testing.T) {
	dir := txlogTestDir(t)
	makerSession, takerSession, btcConn, ltcConn, hub, orderID := setupTxLogPair(t)
	disp := orderIDString(orderID)

	_, bodyA, err := makerSession.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: takerSession.pubKey})
	if err != nil {
		t.Fatalf("maker OnCreateA: %v", err)
	}
	createdA := bodyA.(*proto.CreatedABody)
	_, bodyB, err := takerSession.OnCreateB(&proto.CreateBBody{
		HubAddress: hub, ID: orderID, APubKey: makerSession.pubkey(),
		ADepositTxID: createdA.ADepositTxID, HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime,
	})
	if err != nil {
		t.Fatalf("taker OnCreateB: %v", err)
	}
	createdB := bodyB.(*proto.CreatedBBody)

	_, bodyCA, err := makerSession.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID, BDepositTxID: createdB.BDepositTxID, BLockTime: createdB.BLockTime})
	if err != nil {
		t.Fatalf("maker OnConfirmA: %v", err)
	}
	makerPayTxID := bodyCA.(*proto.ConfirmedABody).APayTxID
	_, bodyCB, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID})
	if err != nil {
		t.Fatalf("taker OnConfirmB: %v", err)
	}
	takerPayTxID := bodyCB.(*proto.ConfirmedBBody).BPayTxID

	makerPayHex, ok := ltcConn.rawTx[makerPayTxID]
	if !ok {
		t.Fatal("maker payTx not broadcast")
	}
	takerPayHex, ok := btcConn.rawTx[takerPayTxID]
	if !ok {
		t.Fatal("taker payTx not broadcast")
	}
	got := txlogToday(t, dir)
	for _, want := range []string{
		"redeem counterparty deposit for order " + disp + " role A LTC (submit manually using sendrawtransaction) paytx " + makerPayTxID,
		makerPayHex,
		"redeem counterparty deposit for order " + disp + " role B BTC (submit manually using sendrawtransaction) paytx " + takerPayTxID,
		takerPayHex,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q, got:\n%s", want, got)
		}
	}
	for _, secret := range []string{hexEncode(makerSession.privKey[:]), hexEncode(takerSession.privKey[:])} {
		if strings.Contains(got, secret) {
			t.Errorf("transcript leaks M privkey %q", secret)
		}
	}
}

// TestTxLogRefundTranscript drives a deposit then the automatic refund sweep
// and proves the broadcast refund is tied to its on-chain txid in the
// transcript — the entry an operator greps for after a stall.
func TestTxLogRefundTranscript(t *testing.T) {
	dir := txlogTestDir(t)
	n, s, conn := setupSwapPair(t)

	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
		t.Fatalf("OnCreateA: %v", err)
	}
	// Past the deposit lockTime: the sweep must auto-broadcast the pre-signed
	// refund exactly once.
	conn.blockHeight = 1200
	before := len(conn.broadcasts)
	n.scanRefunds()
	if len(conn.broadcasts) != before+1 {
		t.Fatalf("refund not auto-broadcast (broadcasts %d, want %d)", len(conn.broadcasts), before+1)
	}
	got := txlogToday(t, dir)
	disp := orderIDString(s.id)
	refundTxID := conn.broadcasts[len(conn.broadcasts)-1]
	if !strings.Contains(got, "refund broadcast for order "+disp+" BTC txid "+refundTxID) {
		t.Errorf("transcript missing refund broadcast line, got:\n%s", got)
	}
}
