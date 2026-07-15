# XBridge Wire Protocol — Canonical Reference

This document is the contract for the Go reimplementation in this module. It is
derived directly from the Blocknet C++ source (`src/xbridge/xbridgepacket.*`,
`src/xbridge/xbridgeapp.cpp`, `src/xbridge/xbridgesession.cpp`,
`src/xbridge/version.h`, `src/chainparams.cpp`). Where a value is inferred
rather than read verbatim, it is flagged **[VERIFY]**.

---

## 1. Transport

XBridge rides on top of the **Bitcoin P2P network**. A peer exchanges XBridge
data inside a Bitcoin P2P message whose command is `xbridge` (12-byte,
null-padded ASCII). There is no XBridge-specific transport — the Blocknet
`CNode`/`CConnman` carries it (`src/xbridge/xbridgeapp.cpp:585-589`).

```
g_connman->ForEachNode([&](CNode* pnode) {
    ...
    g_connman->PushMessage(pnode, msgMaker.Make(NetMsgType::XBRIDGE, msg));
});
```

This Go library is a **thin client**: it connects to the live Blocknet
service-node P2P network and speaks the `xbridge` message type. It does NOT
validate blocks and does NOT run a service node.

### 1.1 Network magics (`src/chainparams.cpp`)

| Network  | Magic (hex)        | Default port |
|----------|--------------------|--------------|
| mainnet  | `a1 a0 a2 a3`      | 41412        |
| testnet  | `45 76 65 bb`      | 41474        |
| staging  | `a1 cf 7e ac`      | 41489        |

### 1.2 Bitcoin P2P message framing

```
magic     : 4 bytes  (network-specific, see 1.1)
command   : 12 bytes (ASCII, null-padded)  -> "xbridge\0\0\0\0\0"
length    : 4 bytes  (little-endian uint32) = payload length
checksum  : 4 bytes  (first 4 bytes of double-SHA256(payload))
payload   : `length` bytes  == the XBridge transport envelope (below)
```

The `xbridge` payload is **not** the packet directly. It is wrapped in a
transport envelope (`src/xbridge/xbridgeapp.cpp` `onSend`; `src/net_processing.cpp`
XBRIDGE handling):

```
varint(28 + packetLen)        // Bitcoin CompactSize length of the rest
[ 20 bytes ] destination address (uint160); 20 zero bytes == broadcast
[  8 bytes ] uint64 LE timestamp (ms since Unix epoch, set by the sender)
[  packet  ] the XBridgePacket (129-byte header + body, section 2)
```

`varint` is a Bitcoin CompactSize integer. On send, `p2p/conn.go` builds this
envelope via `encodeXBridgePayload`; on receive, `DecodeXBridgePayload` strips
the varint + 28-byte envelope before handing the packet to `proto.Unmarshal`.

Implemented in `p2p/message.go` (`Message`, `Checksum`, `Marshal`,
`UnmarshalMessage`) and `p2p/envelope.go`.

### 1.3 Handshake

Before any `xbridge` traffic, a standard Bitcoin `version` / `verack` exchange
is required. The C++ node expects a well-formed `version` message
(version, services, timestamp, addr_recv, addr_from, nonce, user-agent,
start_height, relay). Implemented in `p2p/version.go` (`VersionMessage`,
`NetAddr`, `Marshal`, `NewVersion`) and driven by `p2p/conn.go`'s `handshake()`.

Field values (`p2p/version.go`):

- `version` = `70713` (`BitcoinProtocolVersion`, from `src/version.h:12`).
- `services` = `0` (thin client advertises no services).
- `addr_recv` = the peer's IP/port (IPv4 mapped into `::ffff:/96`); `addr_from`
  left zeroed. Port is **big-endian** in `net_addr` (network byte order).
- `nonce` = random `uint64`.
- `user_agent` = `"/xbridge-go:0.1.0/"`.
- `start_height` = `0` (thin client has no chain).
- `relay` = `false`.

The handshake sends `version`, then reads until it has seen both the peer's
`version` (to which it replies `verack`) and the peer's `verack`; unrelated
messages are ignored. A 30 s deadline bounds the exchange.

**VERIFIED (2026-07-14):** the version/verack handshake was exercised against a
live Blocknet 4.4.1 service node (`coreproxy.airdns.org:42111`, magic
`a1 a0 a2 a3`). The peer returned `proto=70713`, `ua="/Blocknet:4.4.1/"`,
`relay=true`, and immediately began emitting `xbridge` messages. The `xbridge`
command string is confirmed to be `"xbridge"`.

---

## 2. XBridge packet

Defined by `class XBridgePacket` in `src/xbridge/xbridgepacket.h`.

### 2.1 Header (129 bytes)

All multi-byte integers are **little-endian**.

| Offset | Field        | Type        | Notes |
|--------|--------------|-------------|-------|
| 0      | version      | uint32 LE   | `XBRIDGE_PROTOCOL_VERSION = 55` (`src/xbridge/version.h`) |
| 4      | command      | uint32 LE   | `XBridgeCommand` (section 3) |
| 8      | timestamp    | uint32 LE   | seconds (C++ uses `time(0)`) |
| 12     | oldSize      | uint32 LE   | backward-compat size = bodyLen + 97 |
| 16     | size         | uint32 LE   | body length |
| 20     | pubkey       | 33 bytes    | compressed secp256k1 pubkey |
| 53     | signature    | 64 bytes    | compact ECDSA sig (r‖s) |
| 117    | (padding)    | 12 bytes    | unused; fills header to 129 |

> The C++ `crc` field (`field32<5>`) sits at offset 20 — i.e. it **overlaps the
> start of the pubkey** and is unused (`crc()` always returns 0). Do not write a
> separate crc field. `headerSize = 8*4 + 33 + 64 = 129`.

Implemented in `proto/packet.go` (`Packet`, `Marshal`, `Unmarshal`).

### 2.2 Body

A variable-length sequence of typed fields appended in order. Field encodings
(`proto/body.go`):

| Type       | Encoding |
|------------|----------|
| uint32     | 4 bytes LE |
| uint64     | 8 bytes LE |
| string     | raw bytes + terminating `0x00` |
| 32-byte hash (uint256) | 32 raw bytes, **Bitcoin internal LE order** (reverse of display hex) |
| 20-byte address (uint160) | 20 raw bytes |
| 8-byte currency | ASCII ticker, left-aligned, null-padded (e.g. `"BTC"` → `42 54 43 00 00 00 00 00`) |

Per-command body layouts are listed in section 4.

---

## 3. Commands (`XBridgeCommand`)

From `src/xbridge/xbridgepacket.h:52-282`. (Gaps in the numbering are
intentional — historical.)

| Value | Name | Direction / purpose |
|-------|------|---------------------|
| 0  | `xbcInvalid`            | — |
| 2  | `xbcXChatMessage`       | relay envelope (carries a serialized Bitcoin p2p msg) |
| 3  | `xbcTransaction`        | make / broadcast an order |
| 4  | `xbcPendingTransaction` | list of open orders broadcast |
| 5  | `xbcTransactionAccepting` | accept an open order |
| 6  | `xbcTransactionHold`    | holder announces hold |
| 7  | `xbcTransactionHoldApply` | apply hold |
| 8  | `xbcTransactionInit`    | init (client→hub) |
| 9  | `xbcTransactionInitialized` | init ack (hub→client) |
| 10 | `xbcTransactionCreateA` | A creates deposit tx |
| 11 | `xbcTransactionCreatedA`| A created (hub→B): deposit id, hashed secret, locktimes |
| 12 | `xbcTransactionCreateB` | B creates deposit tx |
| 13 | `xbcTransactionCreatedB`| B created (hub→A): deposit id |
| 18 | `xbcTransactionConfirmA`| A confirms B's deposit |
| 19 | `xbcTransactionConfirmedA` | A confirmed: x pubkey |
| 20 | `xbcTransactionConfirmB`| B confirms A's deposit |
| 21 | `xbcTransactionConfirmedB` | B confirmed |
| 22 | `xbcTransactionCancel`  | cancel (uint256 id, uint32 reason) |
| 24 | `xbcTransactionFinished`| finished |
| 26 | `xbcTransactionReject`  | reject (uint256 id, uint32 reason) |
| 50 | `xbcServicesPing`       | supported-services ping |

Implemented in `proto/command.go`.

---

## 4. Swap handshake (state machine)

From the order/accept/hold/init/create/confirm sequence documented in
`xbridgepacket.h:69-99` and the `Transaction::State` enum
(`src/xbridge/xbridgetransaction.h:36-50`):

```
trNew -> trJoined -> trHold -> trInitialized -> trCreated -> trSigned
     -> trCommited -> trFinished   (or trCancelled / trDropped)
```

High-level flow (hub = the order maker / "exchange"; clients A and B are the
two traders):

```
A (maker)                       hub                     B (taker)
xbcTransaction --------------------->  (order broadcast via xbcPendingTransaction)
                                 xbcPendingTransaction ---->
xbcTransactionAccepting <--------- (B accepts) ----------------- xbcTransaction
xbcTransactionHold <-----------> xbcTransactionHoldApply
xbcTransactionInit ----------> xbcTransactionInitialized
xbcTransactionCreateA ------> xbcTransactionCreatedA ----> (B sees A deposit, secret, locktimes)
                              xbcTransactionCreateB <---- xbcTransactionCreatedB
xbcTransactionConfirmA ----> xbcTransactionConfirmedA
                              xbcTransactionConfirmB <---- xbcTransactionConfirmedB
xbcTransactionFinished
```

Timing constants (`xbridgetransaction.h:54-67`):

| Constant    | Value |
|-------------|-------|
| `lockTime`  | 600 s (10 min) |
| `pendingTTL`| 360 s (6 min) |
| `TTL`       | 3600 s (1 h) |
| `deadlineTTL` | 604800 s (7 d) |
| `blocksTTL` | 10080 blocks (7 d) |

**The swap state machine + edge cases (refunds, partial orders, expiry) are the
highest-risk part of the reimplementation.** Port `src/test/xbridge_tests.cpp`
and `src/test/bswap_tests.cpp` as the acceptance oracle.

---

## 5. Signing

From `src/xbridge/xbridgepacket.cpp:59-162`.

- `signature` = secp256k1 **ECDSA**, serialized with
  `secp256k1_ecdsa_signature_serialize_compact` → **64 bytes (r‖s)**,
  compressed pubkey (33 bytes).
- The signed digest is **SHA256 over the entire packet buffer (header + body)**
  with the 64-byte signature region zeroed first.
- Verification re-zeroes the signature region, hashes, parses the compact
  signature, parses the 33-byte pubkey, and verifies.

Implemented as `proto.Packet.Digest()` (stdlib SHA256) +
`crypto.Signer` (btcec/v2 — see `crypto/signer.go` for the exact recipe).

---

## 6. Open questions / TODO for next phases

1. **`version` handshake payload** (§1.3) — ✅ DONE + VERIFIED live (2026-07-14).
2. **`NetMsgType::XBRIDGE` command string** — ✅ confirmed `"xbridge"` live.
3. **uint256 byte order** — confirm Bitcoin internal LE order is preserved
   verbatim on the wire (no reversal) by inspecting how `xbridgeapp` appends
   uint256 fields.
4. **Per-coin transaction construction** — `src/xbridge/xbridgewalletconnector*`
   + `xbitcointransaction*`: build/serialize each coin's deposit/refund tx and
   parse addresses/amounts. UTXO coins share `xbitcointransaction`; Decred
   (`xbridgesessiondcr`) is special.
   - **Body field layouts: DONE (2026-07-15).** `proto/body_types.go` ports
     every `XBridgeCommand`'s body 1:1 from the C++ writers
     (`xbridgeapp.cpp` `sendPendingTransaction` for `xbcTransaction` (3);
     `xbridgesession.cpp` for `xbcPendingTransaction` (4) and the swap
     commands; `xbridgeapp.cpp` `acceptXBridgeTransaction` for
     `xbcTransactionAccepting` (5)). The C++ header enum comments are STALE and
     were deliberately NOT followed where they disagree with the real writers
     (see the note at the top of `body_types.go`). `xbcTransaction` is decoded
     end-to-end from a live captured packet in `proto/body_test.go`.
5. **Wallet connector RPC** — `src/xbridge/bitcoinrpcconnector*` exposes
   Blocknet core RPC; the connected SPV wallet (incl. BLOCK) signs + pays the
   service-node fee. Implement the connector interface in `wallet/`.
6. **API surface** — `src/xbridge/rpcxbridge.cpp` (`dx*` commands) as Go
   library calls in `api/`.
