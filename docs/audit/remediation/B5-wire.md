# B5 — wire hardening (WIRE-F57..F70)

Branch: `fix/wire-hardening` (off `main` @ B3 merge).
C++ reference: Blocknet Core @ `ac930b7f8` (v4.4.1 era).
Go subject: `p2p/`, `proto/`, `cmd/xbridged/` (12 OPEN findings; F65/F66 stay
`DOCUMENTED`; WIRE-F71 was fixed on B1).

## Findings resolved on this branch

| ID | Title | C++ source of truth | Go call sites → fix |
|---|---|---|---|
| F57 | P2P max payload 67 MiB vs 4,000,000 | `net.h:55` `MAX_PROTOCOL_MESSAGE_LENGTH = 4*1000*1000`; disconnect `net.cpp:583-585` | `p2p/message.go:15` `MaxPayloadSize`; `p2p/conn.go:199` `readMessage` guard |
| F58 | Received magic never validated | `net_processing.cpp:3117-3121` (`fDisconnect` on bad `pchMessageStart`) | `p2p/conn.go` `readMessage` → compare `msg.Magic != c.magic` ⇒ error (disconnect) |
| F59 | No `MIN_PEER_PROTO_VERSION` gate | `version.h:27` `=70712`; disconnect `net_processing.cpp:1617-1626`; duplicate `version` disconnect `:1574-1582` | `p2p/version.go` const; `p2p/conn.go` `handshake` |
| F60 | "staging" is C++ REGTEST | `chainparams.cpp:417-421` `a1cf7eac:41489`; `strNetworkID="regtest"` | `p2p/params.go:7`, `p2p/seeds.go`, `cmd/xbridged/main.go`, `api/node.go:85` |
| F61 | `snl` (SNLIST) responses ignored | `net_processing.cpp:2992-3001` answers with one SNLISTPING per known ping | `p2p/discovery/peer_manager.go:341-344`, `p2p/servicenode/servicenode.go` |
| F62 | `proto.Unmarshal` ignores trailing body bytes | `xbridgepacket.h:489-493` `copyFrom` rejects `sizeField() != size()-headerSize` | `proto/packet.go:117-123` |
| F63 | Non-canonical CompactSize accepted | `serialize.h:289-305` `ReadCompactSize` throws on non-canonical and on `> MAX_SIZE` (32 MiB) | `p2p/envelope.go:101-129` `readVarInt` (shared by envelope, addr, servicenode) |
| F64 | Bad checksum disconnects instead of log-and-drop | `net_processing.cpp:3138-3145` logs + drops, keeps the conn | `p2p/message.go:78-80` → sentinel `ErrChecksum`; `p2p/conn.go` `readMessage` loops |
| F67 | cmd-2 `xbcXChatMessage` body speculative | no C++ writer ("not implemented", `xbridgesession.cpp:373-375`) | `proto/body_types.go:913-927` → DELETE the type |
| F68 | cmd-50 `xbcServicesPing` claim unbacked | no C++ writer (TODO, `servicenodemgr.h:113-117`); DecodeBody already refuses | `proto/body_types.go:933-956` → DELETE the type |
| F69 | getaddr policy + addr cap differ | outbound-ignore `net_processing.cpp:2665-2668`; `Misbehaving(20)` on `>1000` `:1825-1830` | `p2p/discovery/peer_manager.go:420-424`; `p2p/addr.go:32-43` |
| F70 | Handshake deadline 30 s vs C++ 60 s | `net.h:83` `DEFAULT_PEER_CONNECT_TIMEOUT = 60` | `p2p/conn.go:43-46` `handshakeTimeout` |

Stays DOCUMENTED (rulings pending, not accepted): WIRE-F65 (Go 1 MiB XBridge body cap —
hardening; waiver ruling pending), WIRE-F66 (cmd-4 dual-writer — Go matches the
authoritative writer and tolerates the second form; ruling pending whether
tolerance or strictness is the §0-conformant reader).

## Design notes / decisions

- **Magic validation lives in `conn.readMessage`**, not `UnmarshalMessage`:
  the codec stays pure; the network layer owns the network-magic check. Every
  receive path (handshake + peer_manager read loop + api) goes through it.
- **Checksum mismatch is non-fatal**: `UnmarshalMessage` returns the exported
  `ErrChecksum` sentinel; `readMessage` logs and re-loops to the next frame.
  Only structural errors (short frame, oversized length, bad magic) are fatal
  (disconnect), matching C++ `fDisconnect` vs log-and-drop.
- **`readVarInt` canonicality**: one shared reader for the envelope length,
  `addr` count, and servicenode varint/varstr fields, so the C++
  `ReadCompactSize` rules (non-canonical ⇒ error; `> 0x02000000` ⇒ error) apply
  uniformly. `writeVarInt` is already canonical.
- **`getaddr` never answered**: go-xbridge's PeerManager only makes outbound
  connections, and C++ ignores `getaddr` from outbound peers — the faithful
  behavior is to drop the request (removes the 64-vs-1000 response divergence).
- **`snl` answered with a raw byte-echo** of the last ACCEPTED ping payload per
  pubkey (`Registry.AddPing` returns whether it accepted, mirroring the
  strict-newer gate). C++ re-serializes the stored `ServiceNodePing`, which is
  byte-identical to the received payload, so echoing the raw accepted payload is
  exact and avoids a full marshaler.
- **cmd 2 / cmd 50 deleted** (decision): neither has a C++ writer on either side,
  neither is ever sent, and the formats are unverifiable. Removing the
  speculative types + `DecodeBody` cases makes a received cmd 2/50 an explicit
  unsupported-command error. Conformance `// DIVERGENT` skips for both are
  dropped. (F65/F66 documentation remains.)
- **`-network regtest`**: Blocknet has no real "staging" network; the magic
  `a1cf7eac:41489` is C++ REGTEST, so the network name is renamed `staging`→
  `regtest` (flag, seeds, magic var, comments).

## Tests

- `p2p/message_test.go` — cap 4 MB rejection; `ErrChecksum` sentinel.
- `p2p/conn_test.go` — (net.Pipe) wrong-magic ⇒ handshake error; version 70711
  ⇒ error; duplicate `version` ⇒ error; bad-checksum frame dropped then next
  frame delivered; declared length > 4 MB ⇒ error.
- `p2p/envelope_test.go` — non-canonical `0xfd`/`0xfe`/`0xff` varints rejected;
  `> 32 MiB` rejected; existing canonical vectors still round-trip.
- `p2p/addr_test.go` — 1001-record payload ⇒ error.
- `p2p/servicenode/servicenode_test.go` — `AddPing` accepted-bool; (raw echo).
- `p2p/discovery/peer_manager_test.go` — getaddr unanswered; snl → SNLISTPING
  raw echo; addr > 1000 dropped.
- `proto/packet_test.go` — trailing body bytes rejected; exact-length ok.
- `proto/body_test.go` + `conformance` — cmd 2/50 rows removed (reject).
- Existing golden vectors in `evidence/wire*.md` are canonical and unchanged.

## Verify

`gofmt` → `go build` / `go vet` / `go test ./...` → `go test -race
./p2p/... ./proto/...` → from `tools/`: `make parity` + `make canary`.
Register + remediation-plan closeout on this branch, then merge approval.
