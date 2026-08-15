package p2p

import (
	"net"
	"sync"
	"testing"
	"time"

	"go-xbridge/proto"
)

// handshakePeer drives the far end of a net.Pipe through the version/verack
// exchange, then parks until release (a "stuck" peer that never reads, so the
// client's writer goroutine blocks on its first post-handshake frame).
func handshakePeer(t *testing.T, server net.Conn, release chan struct{}) {
	t.Helper()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		v := versionPayloadForTest(BitcoinProtocolVersion)
		writeFrame(server, Message{Magic: MainnetMagic, Command: "version", Payload: v, Checksum: Checksum(v)})
		if _, err := readFrame(server); err != nil { // our verack
			return
		}
		writeFrame(server, Message{Magic: MainnetMagic, Command: "verack", Checksum: Checksum(nil)})
		if release != nil {
			<-release // never read again
		}
	}()
}

// TestConnWriteSlowPeerNonBlocking. A peer that stops reading
// must not stall WritePacket: frames are buffered by the writer goroutine, and
// when the buffered queue exceeds the (test-lowered) cap the peer is
// disconnected exactly like C++ pauses-then-disconnects a peer whose send
// buffer cannot drain (net.cpp:2721-2722).
func TestConnWriteSlowPeerNonBlocking(t *testing.T) {
	orig := maxSendBufferSize
	maxSendBufferSize = 1 << 10 // small cap so the test stays fast
	t.Cleanup(func() { maxSendBufferSize = orig })

	client, server := net.Pipe()
	release := make(chan struct{})
	handshakePeer(t, server, release)
	conn, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		_ = conn.Close()
	})

	pkt := proto.NewPacket(proto.XbcTransactionCancel, make([]byte, 36))
	start := time.Now()
	var firstErr error
	for i := 0; i < 100; i++ {
		if err := conn.WritePacket(pkt, [20]byte{}); err != nil {
			firstErr = err
			break
		}
		if time.Since(start) > time.Second {
			t.Fatal("WritePacket blocked on a slow peer")
		}
	}
	if firstErr == nil {
		t.Fatal("expected the send-buffer cap to disconnect a stuck peer")
	}
	// The connection is torn down after the cap disconnect.
	if _, _, err := conn.ReadPacket(); err == nil {
		t.Fatal("conn must be closed after the cap disconnect")
	}
}

// TestConnWriteDeliversInOrderConcurrent — the buffered writer must not lose or
// reorder frames: two producers enqueue concurrently; the far end must receive
// exactly 20 frames with each producer's subsequence strictly increasing (-race
// exercises the concurrent producers).
func TestConnWriteDeliversInOrderConcurrent(t *testing.T) {
	client, server := net.Pipe()
	handshakePeer(t, server, nil) // far end reads forever
	conn, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	type frame struct{ id, seq int }
	got := make(chan frame, 20)
	go func() {
		for i := 0; i < 20; i++ {
			m, err := readFrame(server)
			if err != nil {
				return
			}
			pktBytes, derr := DecodeXBridgePayload(m.Payload)
			if derr != nil {
				return
			}
			pkt, perr := proto.Unmarshal(pktBytes)
			if perr != nil || len(pkt.Body) < 2 {
				return
			}
			got <- frame{id: int(pkt.Body[0]), seq: int(pkt.Body[1])}
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				body := []byte{byte(id), byte(i)}
				if err := conn.WritePacket(proto.NewPacket(proto.XbcTransactionCancel, body), [20]byte{}); err != nil {
					t.Errorf("WritePacket: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	seqs := map[int][]int{}
	for i := 0; i < 20; i++ {
		select {
		case f := <-got:
			seqs[f.id] = append(seqs[f.id], f.seq)
		case <-time.After(5 * time.Second):
			t.Fatal("far end did not receive all frames")
		}
	}
	for id, s := range seqs {
		for i := 1; i < len(s); i++ {
			if s[i] <= s[i-1] {
				t.Fatalf("producer %d frames reordered: %v", id, s)
			}
		}
	}
}

// TestConnWriterExitsOnClose — Close stops the writer goroutine and a later
// WritePacket fails, so no outbound goroutine leaks and no frame is accepted
// onto a dead connection.
func TestConnWriterExitsOnClose(t *testing.T) {
	client, server := net.Pipe()
	handshakePeer(t, server, nil)
	conn, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	defer server.Close()
	pkt := proto.NewPacket(proto.XbcTransactionCancel, make([]byte, 36))
	if err := conn.WritePacket(pkt, [20]byte{}); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.WritePacket(pkt, [20]byte{}); err == nil {
		t.Fatal("WritePacket after Close must fail")
	}
}

// TestConnWriteErrorSurfacesOnNextSend — a peer that closes the transport makes
// the writer record the socket error, which the next WritePacket returns (C++
// surfaces send failures on the socket-handler thread, never on PushMessage).
func TestConnWriteErrorSurfacesOnNextSend(t *testing.T) {
	client, server := net.Pipe()
	handshakePeer(t, server, nil)
	conn, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	server.Close() // abrupt peer close

	pkt := proto.NewPacket(proto.XbcTransactionCancel, make([]byte, 36))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := conn.WritePacket(pkt, [20]byte{}); err != nil {
			return // the broken peer surfaced
		}
		if time.Now().After(deadline) {
			t.Fatal("write error never surfaced on a closed peer")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestConnCloseConcurrentWithWrite — the writer tears the conn down on a socket
// error while other goroutines Close() and WritePacket() at the same time; a
// concurrent close of c.done must not panic (doneOnce) and the writer must not
// leak. -race is the pass criteria.
func TestConnCloseConcurrentWithWrite(t *testing.T) {
	client, server := net.Pipe()
	handshakePeer(t, server, nil)
	conn, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	server.Close() // a write will now error and make the writer tear down

	pkt := proto.NewPacket(proto.XbcTransactionCancel, make([]byte, 36))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = conn.WritePacket(pkt, [20]byte{}) // may error; must not panic
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = conn.Close() // concurrent with writer teardown; must not panic
		}
	}()
	wg.Wait()
}

// TestConnReadOnlyNoWriterGoroutine — a connection used for reads only (the
// common hub path before any outbound frame) must never spawn the writer
// goroutine, so read-only and handshake-failed conns leak nothing.
func TestConnReadOnlyNoWriterGoroutine(t *testing.T) {
	client, server := net.Pipe()
	handshakePeer(t, server, nil)
	conn, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	defer server.Close()
	if conn.writerUp.Load() {
		t.Fatal("writer goroutine must not start without a post-handshake write")
	}
	_ = conn.Close()
	if conn.writerUp.Load() {
		t.Fatal("writer goroutine must not exist after close of a read-only conn")
	}
}
