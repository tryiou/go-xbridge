# Verification report — go-xbridge vs Blocknet Core xBridge wire-protocol findings

Verifier: wire-conformance subagent. All paths relative to the workspace root.
Repos: REFERENCE = `blocknet_core/`, CANDIDATE = `go-xbridge/`.

---

## WIRE-MaxPayloadSize (Go 1<<26 vs C++ 4,000,000)

VERDICT: **CONFIRMED**

- Go: `go-xbridge/p2p/message.go:15` `const MaxPayloadSize = 1 << 26` (67,108,864 B, ~67 MiB);
  enforced at `message.go:70-72`. An additional read-path cap at `go-xbridge/p2p/conn.go:199`
  is `length > 64*1024*1024` (64 MiB).
- C++: `blocknet_core/src/net.h:55` `MAX_PROTOCOL_MESSAGE_LENGTH = 4 * 1000 * 1000`
  (4,000,000 B); enforced at `net.cpp:583-586` (oversized message → disconnect). A second
  cap, `serialize.h:27` `MAX_SIZE = 0x02000000` (33,554,432 B), is checked at `net.cpp:657`
  and `protocol.cpp:135`.

Corrected statement: Go accepts payloads up to 64 MiB (read cap) / 67 MiB (declared cap) so a
10 MiB message passes Go, while C++ disconnects any message > 4,000,000 B (`net.cpp:583`).

---

## WIRE-Magic validation (Go never checks inbound magic)

VERDICT: **CONFIRMED**

- Go: `go-xbridge/p2p/message.go:58` `copy(m.Magic[:], data[0:4])` copies the received magic
  into the struct but `UnmarshalMessage` never compares it to the configured network magic.
  `go-xbridge/p2p/conn.go:121,131,185,234` write `c.magic` on outbound frames only; there is
  no receive-side comparison anywhere (`PeerVersion`, handshake `conn.go:96-113`, and
  `readMessage` `conn.go:193-207` all skip magic validation).
- C++: `blocknet_core/src/net_processing.cpp:3117-3121` compares `msg.hdr.pchMessageStart`
  against `chainparams.MessageStart()` and sets `pfrom->fDisconnect = true` on mismatch.

Corrected statement: Go never validates the received magic against the configured network
magic (it only writes it outbound); C++ disconnects peers whose message start does not match
(`net_processing.cpp:3117-3121`).

---

## WIRE-Version gate (C++ MIN_PEER_PROTO_VERSION=70712)

VERDICT: **CONFIRMED**

- C++: `blocknet_core/src/version.h:27` `MIN_PEER_PROTO_VERSION = 70712`;
  `net_processing.cpp:1617-1626` disconnects (`pfrom->fDisconnect = true`) peers with
  `nVersion < MIN_PEER_PROTO_VERSION`, sending `REJECT` `REJECT_OBSOLETE`.
- Go: `go-xbridge/p2p/version.go:16` advertises `BitcoinProtocolVersion = 70713`; the handshake
  (`go-xbridge/p2p/conn.go:100-105`) only parses and stores `peerVersion`; a repo-wide grep for
  `70712`/`MIN_PEER`/min-version checks in Go returns nothing (only tests/comment mentions).
  The comment at `version.go:15-16` ("The node rejects peers advertising a lower version") is
  misleading — no such rejection exists in Go.

Corrected statement: C++ enforces `MIN_PEER_PROTO_VERSION = 70712` and disconnects below it
(`net_processing.cpp:1617-1626`, `version.h:27`); Go never enforces any minimum protocol
version.

---

## WIRE-"Staging" label is actually C++ REGTEST

VERDICT: **CONFIRMED**

- Go: `go-xbridge/p2p/params.go:7` `StagingMagic = [4]byte{0xa1, 0xcf, 0x7e, 0xac} // port 41489`,
  selected by `cmd/xbridged/main.go:151-152` and `p2p/seeds.go:51,65` for the "staging"
  network.
- C++: `blocknet_core/src/chainparams.cpp:417-421` — the **REGTEST** params define
  `pchMessageStart = a1 cf 7e ac` and `nDefaultPort = 41489`. Mainnet is `a1 a0 a2 a3` /
  41412 (`chainparams.cpp:127-131`), testnet `45 76 65 bb` / 41474 (`chainparams.cpp:293-296`).
  A search of `blocknet_core/src/` for "staging" returns nothing — no C++ staging network
  exists.

Corrected statement: Go's `StagingMagic` (a1cf7eac, port 41489) is byte-for-byte the C++ REGTEST
netmagic (`chainparams.cpp:417-421`); no Blocknet network uses a1cf7eac — there is no staging
chain in C++.

---

## WIRE-SNLIST handling (C++ answers; Go has no receive/use path)

VERDICT: **PARTIAL**

- C++: `blocknet_core/src/net_processing.cpp:2992-2998` — on `SNLIST`, iterates `smgr.list()`
  and replies to the requester with one `SNLISTPING` per known snode (`smgr.getPing(...)`),
  i.e. C++ actively uses the command and responds with a list of pings.
- Go: `go-xbridge/p2p/discovery/peer_manager.go:341-344` explicitly **ignores** incoming
  `snl` (`"servicenode: ignoring SNLIST (stock client does not answer)"`); `go-xbridge/p2p/servicenode/servicenode.go:39`
  defines `CmdSNList = "snl"` only as a request Go never sends. There is **no** `snl` payload
  parser — no `ParseServiceNodeList`-style function exists in the package.

Corrected statement: Go does NOT parse `snl` — it merely declares the constant and drops
incoming SNLIST (`peer_manager.go:341-344`); C++ actively answers SNLIST with SNLISTPING
replies (`net_processing.cpp:2992-2998`). The "Go parses snl" sub-claim is wrong; the
"no snl receive/use path" gap is confirmed.

---

## WIRE-Trailing bytes after declared size

VERDICT: **CONFIRMED**

- Go: `go-xbridge/proto/packet.go:117-123` rejects only `len(data) < HeaderSize + Size`;
  if the buffer is longer than declared, extra trailing bytes are silently ignored
  (`copy(p.Body, data[BodyOffset:BodyOffset+Size])`).
- C++: `blocknet_core/src/xbridge/xbridgepacket.h:479-497` `XBridgePacket::copyFrom` requires
  `sizeField() == data.size() - headerSize` (line 489) and returns false
  ("incorrect data size") on any mismatch, so declared body length must exactly equal the
  actual payload length.

Corrected statement: Go silently ignores trailing bytes past the declared packet body
(`packet.go:117-123`); C++ `copyFrom` rejects any size mismatch (`xbridgepacket.h:489-493`).

---

## WIRE-F70 (REFUTED) Receiver timestamp rewrite

VERDICT: **REFUTED**

- C++: the header `timestamp` field is set to `time(0)` **only** in the XBridgePacket
  constructors for newly created (outbound) packets (`xbridgepacket.h:502,507,519`). The
  receive path builds `new XBridgePacket` then `copyFrom(message)` (`xbridgeapp.cpp:654-659`
  and `744-749`), and `copyFrom` does `m_body = data` (`xbridgepacket.h:487`), which
  **preserves** the wire timestamp. `Session::Impl::decryptPacket` is a no-op
  (`xbridgesession.cpp:234-239`); `Session::processPacket` (`xbridgesession.cpp:283-322`)
  performs no timestamp manipulation. No code reads the header timestamp on either side
  (C++ exposes no accessor; Go only carries `Packet.Timestamp` in marshal/unmarshal,
  `proto/packet.go:77,104`).
- Go: `go-xbridge/proto/packet.go:104` preserves the wire timestamp on receive (set to
  `time.Now().Unix()` only in `NewPacket`, `packet.go:65`).

Corrected statement: C++ does NOT reset an incoming packet's header timestamp to `time(0)` —
the constructors' `time(0)` stamp applies only to newly created outbound packets and
`copyFrom` preserves the wire value, exactly as Go does; nothing on either side consumes the
header timestamp.

---

## WIRE-Non-canonical varint

VERDICT: **CONFIRMED**

- Go: `go-xbridge/p2p/envelope.go:101-129` `readVarInt` decodes `0xfd`/`0xfe`/`0xff` forms
  without checking canonicality (a `0xfd` prefix with value < 0xfd, `0xfe` with value ≤ 0xffff,
  or `0xff` with value ≤ 0xffffffff is accepted) and without any `MAX_SIZE` bound.
- C++: `blocknet_core/src/serialize.h:289-303` `ReadCompactSize` throws
  `"non-canonical ReadCompactSize()"` for each non-canonical form, and additionally rejects
  `nSizeRet > MAX_SIZE` (`serialize.h:304`).

Corrected statement: Go's `readVarInt` accepts non-canonical CompactSize encodings (and is
unbounded); C++ `ReadCompactSize` throws on non-canonical encodings (`serialize.h:289-303`)
and on sizes > MAX_SIZE.

---

## WIRE-F62 (PARTIAL) Envelope exact-length strictness

VERDICT: **PARTIAL**

- Go: `go-xbridge/p2p/envelope.go:53-68` `DecodeXBridgePayload` requires
  `off+n == len(payload)` (line 59-62) and returns "xbridge envelope length mismatch" on any
  trailing/extra bytes — strict exact-length envelope decode.
- C++: the net_processing read path performs **no** envelope-length validation: the payload is
  read wholesale (`vRecv >> raw`, `net_processing.cpp:2870`) and only the first 20+8 bytes are
  stripped (`net_processing.cpp:2893-2895`). However, the inner packet is then parsed by
  `XBridgePacket::copyFrom` which rejects a size mismatch (`xbridgepacket.h:489-493`) — so any
  trailing bytes after the packet cause C++ to drop it too. The `LegacyXBridgePacket::CopyFrom`
  sniff (`servicenode.h:52-61`) tolerates trailing bytes but only to identify legacy
  command-50 pings.

Corrected statement: Go rejects trailing bytes via the envelope varint exact-match
(`envelope.go:59-62`); C++ has no envelope-length check but effectively rejects trailing bytes
through `copyFrom`'s exact size check (`xbridgepacket.h:489-493`) — the claimed
"C++ tolerates extra trailing bytes" asymmetry does not hold at the packet level.

---

## WIRE-Checksum mismatch behavior

VERDICT: **CONFIRMED**

- C++: `blocknet_core/src/net_processing.cpp:3138-3145` logs
  `"CHECKSUM ERROR expected %s was %s"` and `return fMoreWork` — the message is dropped, the
  connection is kept (no `fDisconnect`).
- Go: `go-xbridge/p2p/message.go:78-79` `UnmarshalMessage` returns
  `"p2p: checksum mismatch"`; the error propagates out of `readMessage`/`ReadPacket`
  (`conn.go:193-207`) and terminates the read loop, which closes the connection
  (`go-xbridge/p2p/discovery/peer_manager.go:281-299`). During the handshake the error closes
  the conn as well (`conn.go:70-74`).

Corrected statement: C++ logs-and-drops bad-checksum messages (`net_processing.cpp:3138-3145`);
Go treats a checksum mismatch as a fatal error that disconnects the peer (`message.go:78-79` →
`peer_manager.go:281-299`).

---

## WIRE-MaxBodySize (Go 1 MiB, C++ none)

VERDICT: **CONFIRMED**

- Go: `go-xbridge/proto/packet.go:31` `MaxBodySize = 1 << 20` (1,048,576 B); enforced at
  `packet.go:113-115` ("declared body size too large").
- C++: no per-packet body cap exists — `XBridgePacket::copyFrom` only enforces an *equality*
  check (`xbridgepacket.h:489-493`); the only upper bound on an XBridge packet is the P2P
  4 MB message cap (`net.h:55`, enforced `net.cpp:583-586`).

Corrected statement: Go caps the declared XBridge body at 1 MiB (`packet.go:31`); C++ has no
per-packet body cap and relies solely on the P2P 4,000,000-byte message limit.

---

## WIRE-Command 4 (xbcPendingTransaction) writer asymmetry

VERDICT: **CONFIRMED** (citation corrected)

- C++ writer WITHOUT minFromAmount: `Session::Impl::sendListOfTransactions`,
  `blocknet_core/src/xbridge/xbridgesession.cpp:3626-3650` — appends id/currencies/amounts/hub
  addr/created/blockHash/`uint16` partial flag (lines 3638-3646) then signs; no minFromAmount.
  Body = 126 bytes.
- C++ writer WITH minFromAmount: `Session::Impl::sendTransaction`,
  `xbridgesession.cpp:3666-3691` — appends the same fields plus
  `packet->append(tr->min_partial_amount())` (line 3687). Body = 134 bytes.
- C++ reader: `Session::Impl::processPendingTransaction`, `xbridgesession.cpp:694-700` —
  **requires `packet->size() == 134`** (rejects the 126-byte broadcast variant) and always
  consumes the trailing `uint64 minFromAmount` (`xbridgesession.cpp:776-778`).
- NOTE: the finding's citation `xbridgeapp.cpp:2063-2098` (`App::Impl::sendPendingTransaction`)
  is the **xbcTransaction (command 3)** writer — it is NOT a command-4 writer. Both command-4
  writers live in `xbridgesession.cpp`. Because the reader demands exactly 134 body bytes, the
  with-minFromAmount writer (`sendTransaction`) is the authoritative wire contract.
- Go: `go-xbridge/proto/body_types.go:204-221` always writes minFromAmount (line 219), and the
  decoder tolerates both variants (`body_types.go:255-262`, reads it only if ≥ 8 bytes remain).

Corrected statement: the two command-4 writers are `sendListOfTransactions` (omits
minFromAmount, 126 B) and `sendTransaction` (appends it, 134 B) in `xbridgesession.cpp`
(not `xbridgeapp.cpp:2063-2098`, which is command 3); the reader requires exactly 134 B so the
minFromAmount-bearing writer is authoritative, and Go matches it.

---

## WIRE-getaddr policy + addr cap

VERDICT: **CONFIRMED**

- C++: `net_processing.cpp:2659-2687` — `getaddr` is answered only for `pfrom->fInbound`
  peers (line 2665-2668) and only once per connection (`fSentAddr`, lines 2672-2676); the
  outbound addr queue is capped at `MAX_ADDR_TO_SEND = 1000` (`net.h:53`, used `net.h:837-846`).
  Received `addr` messages > 1000 entries are rejected with Misbehaving
  (`net_processing.cpp:1825-1830`).
- Go: `go-xbridge/p2p/discovery/peer_manager.go:322-324` answers **any** `getaddr` requester
  with `m.addrMan.Random(64)` addresses — no inbound check, no once-per-connection flag.
  Received `addr` has no count cap: `go-xbridge/p2p/addr.go:32-61` `ParseAddr` reads up to the
  declared count with no 1000 limit and `AddSlice` stores them all.

Corrected statement: C++ answers getaddr only from inbound peers, once, capped at 1000 addrs,
and rejects inbound addr > 1000; Go answers any requester with 64 addrs and imposes no cap on
received addr messages.

---

## WIRE-ServicesPing (command 50) body

VERDICT: **CONFIRMED**

- C++: no writer ever constructs a command-50 packet — `xbcServicesPing = 50` exists only in
  the enum (`xbridgepacket.h:276,281`), and `processServicesPing` is declared
  (`xbridgesession.cpp:99`) but **never bound** in the handler table (`xbridgesession.cpp:186-220`).
  The only C++ handling is the legacy snode sniff (`servicenodemgr.h:113-116`, via
  `servicenode.h:52-61`).
- Go: `proto/body_types.go:933-956` defines `ServicesPingBody` (Marshal/Unmarshal) but
  `DecodeBody` explicitly refuses command 50 (`body_types.go:1021-1027`); the type is exercised
  only by a unit test (`body_test.go:293-295`). `p2p/servicenode` parses only
  SNREGISTER/SNPING/SNLISTPING (`peer_manager.go:327-344`); the network-token set is derived
  from ping records' service lists (`api/node.go:185`, `servicenode.go:695-...`). The comment
  at `api/handlers.go:180-181` ("XbcServicesPing advertisements") is misleading — no
  go-xbridge code parses a services-ping body.

Corrected statement: C++ never sends command 50 (no writer, handler unbound); Go has a
standalone `ServicesPingBody` type but neither `DecodeBody` nor `p2p/servicenode` parses a
command-50 body, so the "p2p/servicenode parses it" claim is unbacked.

---

## Summary of corrected claims

- WIRE-F61: "Go parses `snl`" is false — Go only defines the constant and ignores SNLIST.
- WIRE-F70: "C++ resets an incoming packet's header timestamp to time(0)" is false — C++ preserves
  it exactly like Go; nothing consumes the header timestamp on either side.
- WIRE-F62: "C++ tolerates extra trailing bytes" does not hold at the packet level — `copyFrom` is
  strict; only the legacy snode sniff is lenient.
- WIRE-F66: the command-4 writers are both in `xbridgesession.cpp`, not `xbridgeapp.cpp:2063-2098`
  (that is command 3); the 134-byte reader makes the with-minFromAmount writer authoritative.
