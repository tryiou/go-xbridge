// Command liveprobe dials a Blocknet service node over the Bitcoin P2P
// transport, performs the version/verack handshake, prints the peer's
// advertised version, and then reads raw P2P messages for a short window to
// (a) confirm the stream flows and (b) verify the XBridge command name used on
// the wire.
//
// Usage:
//
//	go run ./cmd/liveprobe -addr coreproxy.airdns.org:42111 -magic a1a0a2a3
//
// If the handshake fails, it falls back to a raw diagnostic that dumps the
// first bytes the peer emits, so the actual network magic / protocol can be
// discovered.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"go-xbridge/p2p"
	"go-xbridge/proto"
)

func main() {
	addr := flag.String("addr", "coreproxy.airdns.org:42111", "host:port of the blocknet node (P2P)")
	magicHex := flag.String("magic", "a1a0a2a3", "4-byte network magic (hex)")
	timeout := flag.Duration("timeout", 20*time.Second, "handshake dial/connect timeout")
	listen := flag.Duration("listen", 20*time.Second, "how long to read messages after handshake")
	dump := flag.String("dump", "", "file to append raw xbridge packet payloads (hex) to")
	flag.Parse()

	var magic [4]byte
	mhb, err := hex.DecodeString(*magicHex)
	if err != nil || len(mhb) != 4 {
		fatal("bad -magic %q: %v", *magicHex, err)
	}
	copy(magic[:], mhb)

	fmt.Printf("dialing %s (magic %s)…\n", *addr, *magicHex)
	c, err := p2p.Dial(*addr, magic, *timeout)
	if err != nil {
		fmt.Printf("handshake failed: %v\n", err)
		rawDiag(*addr, magic)
		return
	}
	defer c.Close()
	fmt.Println("handshake OK")

	if pv := c.PeerVersion(); pv != nil {
		fmt.Printf("peer version:  proto=%d services=%d ua=%q startHeight=%d relay=%v\n",
			pv.Version, pv.Services, pv.UserAgent, pv.StartHeight, pv.Relay)
		fmt.Printf("peer addr_recv: %s:%d  addr_from: %s:%d\n",
			pv.AddrRecv.IP, pv.AddrRecv.Port, pv.AddrFrom.IP, pv.AddrFrom.Port)
	} else {
		fmt.Println("peer version: (not captured)")
	}

	deadline := time.Now().Add(*listen)
	_ = c.NetConn().SetReadDeadline(deadline)
	fmt.Printf("reading raw messages for up to %s (xbridge command = %q)…\n", *listen, p2p.XBridgeNetCommand)
	var dumpF *os.File
	if *dump != "" {
		var derr error
		dumpF, derr = os.OpenFile(*dump, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if derr != nil {
			fatal("open dump: %v", derr)
		}
		defer dumpF.Close()
	}
	count := 0
	var parsed, parseErrs int
	var bodyOK, bodyErr int
	cmdCounts := map[uint32]int{}
	for {
		if time.Now().After(deadline) {
			break
		}
		m, err := c.ReadMessage()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			fmt.Printf("read: %v\n", err)
			break
		}
		count++
		tag := ""
		if m.Command == p2p.XBridgeNetCommand {
			tag = "  <-- XBRIDGE"
		}
		fmt.Printf("  #%d cmd=%-12q len=%-8d chksum=%s%s\n",
			count, m.Command, len(m.Payload), hex.EncodeToString(m.Checksum[:]), tag)
		if m.Command == p2p.XBridgeNetCommand {
			if dumpF != nil {
				fmt.Fprintf(dumpF, "%s\n", hex.EncodeToString(m.Payload))
			}
			// Verify the proto codec parses the live packet.
			pktBytes, derr := p2p.DecodeXBridgePayload(m.Payload)
			if derr != nil {
				parseErrs++
				fmt.Printf("       decode envelope: %v\n", derr)
				continue
			}
			p, perr := proto.Unmarshal(pktBytes)
			if perr != nil {
				parseErrs++
				fmt.Printf("       proto.Unmarshal: %v\n", perr)
				continue
			}
			parsed++
			fmt.Printf("       proto: version=%d command=%d(0x%x) size=%d body=%d bytes\n",
				p.Version, uint32(p.Command), uint32(p.Command), p.Size, len(p.Body))
			// Live-verify the per-command body layout against the real struct.
			cmdCounts[uint32(p.Command)]++
			b, berr := proto.DecodeBody(p.Command, p.Body)
			if berr != nil {
				bodyErr++
				fmt.Printf("       body DECODE ERROR: %v\n", berr)
			} else {
				bodyOK++
				fmt.Printf("       body OK: %+v\n", b)
			}
		}
	}
	fmt.Printf("read %d message(s); xbridge parsed=%d errors=%d; body decoded OK=%d err=%d\n",
		count, parsed, parseErrs, bodyOK, bodyErr)
	if len(cmdCounts) > 0 {
		fmt.Println("commands seen (cmd=value count):")
		for cmd, n := range cmdCounts {
			fmt.Printf("  %d(0x%x): %d\n", cmd, cmd, n)
		}
	}
}

// rawDiag connects, sends a version message, and dumps the first bytes the
// peer emits so the actual network magic / command can be discovered.
func rawDiag(addr string, magic [4]byte) {
	nc, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		fmt.Printf("raw dial: %v\n", err)
		return
	}
	defer nc.Close()
	vm := p2p.NewVersion(nc.RemoteAddr())
	payload := vm.Marshal()
	msg := p2p.Message{Magic: magic, Command: "version", Payload: payload, Checksum: p2p.Checksum(payload)}
	if _, err := nc.Write(msg.Marshal()); err != nil {
		fmt.Printf("raw write: %v\n", err)
		return
	}
	hdr := make([]byte, 24)
	_ = nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := io.ReadFull(nc, hdr)
	if err != nil {
		fmt.Printf("raw read first bytes: %v\n", err)
		return
	}
	fmt.Printf("raw first %d bytes: %s\n", n, hex.EncodeToString(hdr))
	fmt.Printf("  magic=%s cmd=%q length=%d checksum=%s\n",
		hex.EncodeToString(hdr[0:4]), string(hdr[4:16]),
		binary.LittleEndian.Uint32(hdr[16:20]), hex.EncodeToString(hdr[20:24]))
}

func fatal(format string, args ...interface{}) {
	fmt.Printf(format+"\n", args...)
}
