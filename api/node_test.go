package api

import (
	"encoding/hex"
	"io"
	"sync"
	"testing"
	"time"

	"xbridge-go/coins"
	"xbridge-go/config"
	"xbridge-go/crypto"
	"xbridge-go/proto"
	"xbridge-go/wallet"
)

// blockStub is a wallet.Connector that returns a fixed anti-replay block hash
// for height-1, used to exercise the order blockHash wiring (C1). It embeds
// stubConn to satisfy the rest of the Connector interface.
type blockStub struct {
	stubConn
	hash [32]byte
}

func (b *blockStub) GetBlockCount() (int64, error) { return 10, nil }
func (b *blockStub) GetBlockHash(height int64) ([32]byte, error) {
	return b.hash, nil
}

func newBlockNode(bs wallet.Connector) *Node {
	cfg := &Config{Connectors: map[string]wallet.Connector{"BLOCK": bs}}
	return &Node{cfg: cfg, signer: crypto.NewBtcSigner(), stop: make(chan struct{})}
}

func TestNodeRefreshBlock(t *testing.T) {
	var want [32]byte
	want[3] = 0x42
	want[31] = 0x99
	n := newBlockNode(&blockStub{hash: want})

	n.refreshBlock()
	if n.block != want {
		t.Fatalf("refreshBlock: got %x want %x", n.block, want)
	}
}

func TestNodeCurrentBlockHashFresh(t *testing.T) {
	var want [32]byte
	want[3] = 0x42
	n := newBlockNode(&blockStub{hash: want})

	// First call refreshes from the connector.
	got := n.currentBlockHash()
	if got != want {
		t.Fatalf("currentBlockHash (first): got %x want %x", got, want)
	}
	// Cached value (set within 60s) is returned unchanged.
	got = n.currentBlockHash()
	if got != want {
		t.Fatalf("currentBlockHash (cached): got %x want %x", got, want)
	}
}

func TestNodeCurrentBlockHashStale(t *testing.T) {
	var want [32]byte
	want[1] = 0x7f
	n := newBlockNode(&blockStub{hash: want})

	n.refreshBlock()
	// Force the cache stale; currentBlockHash must re-fetch from the connector.
	n.blockMu.Lock()
	n.blockAt = time.Now().Add(-10 * time.Minute)
	n.blockMu.Unlock()

	got := n.currentBlockHash()
	if got != want {
		t.Fatalf("currentBlockHash (stale): got %x want %x", got, want)
	}
}

func TestNodeRefreshBlockNoConnector(t *testing.T) {
	n := newBlockNode(nil) // no BLOCK connector
	n.refreshBlock()
	if n.block != [32]byte{} {
		t.Fatalf("expected zero block without connector, got %x", n.block)
	}
}

// captureXConn is a test XConn that records every packet written to it. Its
// ReadPacket blocks forever (returns io.EOF) so a dispatch test cannot
// accidentally consume real input; we only care about the outbound direction.
type captureXConn struct {
	mu      sync.Mutex
	written []*proto.Packet
}

func (c *captureXConn) ReadPacket() (*proto.Packet, string, error) {
	return nil, "", io.EOF
}
func (c *captureXConn) WritePacket(p *proto.Packet) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, p)
	return nil
}
func (c *captureXConn) Close() error { return nil }
func (c *captureXConn) snapshot() []*proto.Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*proto.Packet, len(c.written))
	copy(out, c.written)
	return out
}

// TestDispatchSwapSignsOutbound closes the untested outbound boundary: a hub
// packet dispatched through dispatchSwap must produce exactly one signed
// outbound packet, with the right command, whose signature verifies with our
// public key. This proves the trust boundary writes what the C++ hub expects.
func TestDispatchSwapSignsOutbound(t *testing.T) {
	coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	})

	// Private key 1 (a valid non-zero secp256k1 scalar).
	priv := make([]byte, 32)
	priv[31] = 1

	var id [32]byte
	copy(id[:], []byte("order-id-order-id-order-id-0")) // 32 bytes exactly
	var hub [20]byte
	hub[0] = 0xaa

	cfg := &Config{
		PrivKey:    priv,
		Connectors: map[string]wallet.Connector{"BTC": &stubConn{ticker: "BTC", addr: btcAddr}},
	}
	n := &Node{cfg: cfg, signer: crypto.NewBtcSigner(), stop: make(chan struct{}), sessions: map[string]*SwapSession{}}

	o := &Order{
		ID:           id,
		FromCurrency: "BTC",
		ToCurrency:   "LTC",
		FromAmount:   100000000,
		ToAmount:     200000000,
	}
	n.newMakerSession(o, MakeOrderParams{MakerAddress: btcAddr, TakerAddress: btcAddr})

	cc := &captureXConn{}
	n.conn = cc

	// OnHold drives the maker's response to a hub xbcTransactionHold.
	n.dispatchSwap(id, hub, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		return s.OnHold(&proto.HoldBody{})
	})

	pkts := cc.snapshot()
	if len(pkts) != 1 {
		t.Fatalf("wrote %d packets, want exactly 1", len(pkts))
	}
	p := pkts[0]
	if p.Command != proto.XbcTransactionHoldApply {
		t.Fatalf("command = %d, want XbcTransactionHoldApply (%d)", p.Command, proto.XbcTransactionHoldApply)
	}
	ok, err := crypto.NewBtcSigner().Verify(p)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if !ok {
		t.Fatal("outbound packet signature did not verify")
	}
}

// TestDecodeAddr exercises the per-currency address decoder used by every
// swap handshake: a valid P2PKH and a valid P2WPKH both resolve to the right
// 20-byte id; unknown coins and malformed addresses surface as *rpcError (no
// silent default, no panic).
func TestDecodeAddr(t *testing.T) {
	coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	})

	// Valid P2PKH for version 0x00 — must decode to a non-zero 20-byte id.
	id, e := decodeAddr("BTC", btcAddr)
	if e != nil {
		t.Fatalf("P2PKH decode: %v", e)
	}
	if id == ([20]byte{}) {
		t.Fatal("P2PKH decoded to an empty id")
	}

	// Valid P2WPKH (BIP173 test vector BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4);
	// its id equals the 20-byte witness program 751e76e8199196d454941c45d1b3a323f1433bd6.
	id2, e := decodeAddr("BTC", "BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4")
	if e != nil {
		t.Fatalf("P2WPKH decode: %v", e)
	}
	var want [20]byte
	if _, err := hex.Decode(want[:], []byte("751e76e8199196d454941c45d1b3a323f1433bd6")); err != nil {
		t.Fatalf("test vector decode: %v", err)
	}
	if id2 != want {
		t.Fatalf("P2WPKH id = %x, want %x", id2, want)
	}

	// Malformed address -> rpcError.
	if _, e := decodeAddr("BTC", "not-an-address"); e == nil {
		t.Fatal("expected rpcError for malformed address")
	}
	// Unknown coin -> rpcError.
	if _, e := decodeAddr("NOPE", btcAddr); e == nil {
		t.Fatal("expected rpcError for unknown coin")
	}
}
