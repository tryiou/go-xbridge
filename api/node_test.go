package api

import (
	"testing"
	"time"

	"xbridge-go/crypto"
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
