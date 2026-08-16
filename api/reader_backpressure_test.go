package api

import (
	"io"
	"testing"
	"time"

	"go-xbridge/crypto"
	"go-xbridge/proto"
)

// floodConn is a fake XConn that returns a bounded stream of packets then
// blocks (the reader's ReadPacket stays parked). release gates the FIRST
// ReadPacket so a test can hold the reader until the engine channel is provably
// full; Close unblocks a parked ReadPacket so readerLoop can observe n.stop.
type floodConn struct {
	pkts    []*proto.Packet
	done    chan struct{}
	release chan struct{}
}

func newFloodConn(pkts []*proto.Packet) *floodConn {
	return &floodConn{pkts: pkts, done: make(chan struct{}), release: make(chan struct{})}
}

// unblock allows the first ReadPacket to return its packet.
func (c *floodConn) unblock() { close(c.release) }

func (c *floodConn) ReadPacket() (*proto.Packet, string, error) {
	if len(c.pkts) == 0 {
		select {
		case <-c.done:
			return nil, "", io.EOF
		case <-time.After(5 * time.Second):
			return nil, "", io.EOF
		}
	}
	select {
	case <-c.release:
	case <-c.done:
		return nil, "", io.EOF
	}
	p := c.pkts[0]
	c.pkts = c.pkts[1:]
	return p, "peer", nil
}
func (c *floodConn) WritePacket(*proto.Packet, [20]byte) error { return nil }
func (c *floodConn) Close() error                              { close(c.done); return nil }

func f8SignedCancel(t *testing.T) *proto.Packet {
	t.Helper()
	priv, err := crypto.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	// XbcTransactionCancel with a 36-byte body: decodable (DecodeBody) and
	// signed (signer.Verify) so it passes every pre-channel gate and reaches
	// the n.packets send — the only point under test.
	p := proto.NewPacket(proto.XbcTransactionCancel, make([]byte, 36))
	if err := crypto.NewBtcSigner().Sign(p, priv); err != nil {
		t.Fatalf("sign packet: %v", err)
	}
	return p
}

// TestReaderFloodBackpressure proves a reader with a full engine channel
// BLOCKS on the send (backpressure) rather than dropping the packet — the
// C++ net-thread behavior (synchronous processPacket) and the discovery
// readLoop contract.
//
// Sequence: the channel is pre-filled to capacity, the reader is held at
// ReadPacket via the release gate, and only then is the reader's single packet
// released into processing. The send must therefore happen with the channel
// FULL and the test NOT draining: a blocking reader parks the packet and
// delivers it as soon as a slot frees (5 total deliveries); a drop-on-full
// reader discards it permanently (only the 4 markers arrive).
func TestReaderFloodBackpressure(t *testing.T) {
	n := newEngineNode()
	n.packets = make(chan inboundPacket, 4)
	marker := f8SignedCancel(t)
	for i := 0; i < 4; i++ {
		n.packets <- inboundPacket{pkt: marker}
	}
	fc := newFloodConn([]*proto.Packet{f8SignedCancel(t)})
	n.conn = fc
	n.wg.Add(1)
	go n.readerLoop()

	// Release the reader's single packet. It now decodes/verifies and attempts
	// the send against a full, undrained channel.
	fc.unblock()

	// Poll until the reader has attempted the send (parked) or dropped. The
	// channel can never grow past 4 while full; the deterministic signal is the
	// eventual delivery count, so give the send a settle window before draining.
	time.Sleep(100 * time.Millisecond)

	// Drain: the first read frees a slot, which lets a PARKED reader complete
	// its send (delivering packet 5 as the final read). A DROPPED packet is
	// gone and the fifth read times out.
	got := 0
drain:
	for i := 0; i < 5; i++ {
		select {
		case <-n.packets:
			got++
		case <-time.After(2 * time.Second):
			break drain
		}
	}
	_ = fc.Close()
	close(n.stop)
	n.wg.Wait()

	if got != 5 {
		t.Fatalf("delivered %d/5 packets (4 buffer markers + 1 reader packet); want 5 — reader dropped the surplus on a full channel", got)
	}
}

// TestReaderBackpressureNoDrops exercises the shutdown-loss path exactly: the
// reader must PARK at the send on a full channel, and when the node stops the
// in-flight packet is lost and counted (packetsDropped == 1) — the ONLY loss
// path after the backpressure fix. The reader is released past ReadPacket so
// it reaches the blocked send, then n.stop closes with the packet in flight.
func TestReaderBackpressureNoDrops(t *testing.T) {
	n := newEngineNode()
	n.packets = make(chan inboundPacket, 4)
	for i := 0; i < 4; i++ {
		n.packets <- inboundPacket{pkt: f8SignedCancel(t)}
	}
	fc := newFloodConn([]*proto.Packet{f8SignedCancel(t)})
	n.conn = fc
	n.wg.Add(1)
	go n.readerLoop()

	// Release the reader's packet: it decodes/verifies and parks at the send
	// (the channel is full and nothing drains). Wait for the park, then stop
	// the node with the packet in flight.
	fc.unblock()
	time.Sleep(50 * time.Millisecond)
	_ = fc.Close()
	close(n.stop)
	n.wg.Wait()

	if got := n.packetsDropped.Load(); got != 1 {
		t.Fatalf("packetsDropped = %d, want 1 (the in-flight packet lost at shutdown)", got)
	}
}
