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
| regtest  | `a1 cf 7e ac`      | 41489        |

The third network is C++ **REGTEST** (`chainparams.cpp:373-421`); go-xbridge
exposes it as `-network regtest` (`RegtestMagic`). There is no Blocknet staging
network.

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
[  8 bytes ] uint64 LE timestamp (microseconds since Unix epoch, set by the sender)
[  packet  ] the XBridgePacket (129-byte header + body, section 2)
```

The destination field is how the hub is addressed: go-xbridge sends the order
make (`xbcTransaction`), the taker's accept (`xbcTransactionAccepting`) and every
handshake reply addressed to the order's hub servicenode, so the C++ hub can
route the trade. `20 zero bytes == broadcast` is used only where a true broadcast
is intended.

The 8-byte envelope timestamp is in **microseconds** (`time.Now().UnixMicro()`),
matching C++ `timeToInt` (`xutil.cpp:280` `total_microseconds()`); it is included
in the SHA256-signed envelope, so a millisecond value would be a 1000x wire
divergence that fails every counterparty signature check. (The packet header's
own 4-byte `timestamp`, §2.1, is **seconds**.)

`varint` is a Bitcoin CompactSize integer. On send, `p2p/conn.go` builds this
envelope via `encodeXBridgePayload`; on receive, `DecodeXBridgePayload` strips
the varint + 28-byte envelope before handing the packet to `proto.Unmarshal`.

**Transport hardening (B5):**
- **Frame cap 4,000,000 bytes** — the declared `length` is rejected above
  `MaxPayloadSize` (C++ `MAX_PROTOCOL_MESSAGE_LENGTH`, `net.h:55`), disconnecting
  the peer like C++ (`net.cpp:583-585`).
- **Magic validated on receive** — a frame whose magic differs from the
  configured network disconnects the peer (C++ `net_processing.cpp:3117-3121`).
- **Checksum mismatch is log-and-drop** — a bad-checksum frame is dropped and
  the connection kept, matching C++ (`net_processing.cpp:3138-3145`); the
  `ErrChecksum` sentinel distinguishes it from fatal decode errors.
- **Canonical CompactSize** — `readVarInt` rejects non-canonical extended
  encodings and values above `MAX_SIZE` (32 MiB), like C++ `ReadCompactSize`
  (`serialize.h:289-305`). This governs the envelope varint, `addr` counts, and
  servicenode varints uniformly.
- **`proto.Unmarshal` requires an exact-length body** — trailing bytes after the
  declared body are an error, matching `XBridgePacket::copyFrom`
  (`xbridgepacket.h:489-493`).
- **Inbound protocol-version gate** — a packet whose header `version` differs
  from `XBRIDGE_PROTOCOL_VERSION` (55) is rejected before its body is parsed or
  its signature verified, mirroring C++ `Session::checkXBridgePacketVersion`
  (`xbridgesession.cpp:343-368`), called at the top of
  `App::onMessageReceived` / `App::onBroadcastReceived`
  (`xbridgeapp.cpp:648,737`). The drop is silent (no misbehaviour penalty) and
  applies to every inbound path, since all wire bytes reach the engine through
  `proto.Unmarshal`.

Implemented in `p2p/message.go` (`Message`, `Checksum`, `Marshal`,
`UnmarshalMessage`), `p2p/envelope.go` (`readVarInt`), and `proto/packet.go`.

### 1.3 Handshake

Before any `xbridge` traffic, a standard Bitcoin `version` / `verack` exchange
is required. The C++ node expects a well-formed `version` message. Implemented
in `p2p/version.go` (`VersionMessage`, `NetAddr`, `Marshal`, `NewVersion`) and
driven by `p2p/conn.go`'s `handshake()`.

Wire layout (`p2p/version.go` `Marshal`):

```
version(4 LE) || services(8 LE) || timestamp(8 LE) ||
addr_recv(26) || addr_from(26) || nonce(8 LE) ||
user_agent(varstr) || start_height(4 LE) || relay(1) || fxrouter(1)
```

Field values (`p2p/version.go`):

- `version` = `70713` (`BitcoinProtocolVersion`, from `src/version.h:12`).
- `services` = `0` (thin client advertises no services).
- `timestamp` = current unix seconds (`int64`). This is the message-level
  time; the embedded `addr_recv`/`addr_from` carry **no** per-addr `nTime`.
- `addr_recv` / `addr_from` = 26-byte CAddress (see §1.3.1). `addr_recv` is the
  peer's IP/port (IPv4 mapped into `::ffff:/96`); `addr_from` is a CAddress with
  services=0 and IP/port left zeroed (thin client advertises no address of its own).
- `nonce` = random `uint64`.
- `user_agent` = `"/go-xbridge:0.1.0/"`.
- `start_height` = `0` (thin client has no chain).
- `relay` = `false`.
- `fxrouter` = `false` (thin client is not an XRouter hub). Sent explicitly so
  address gossip/discovery works against stock service nodes.

**Version gate (B5):** the peer's advertised `version` must be at least
`MinPeerProtoVersion` (70712, `src/version.h:27`); a lower version or a
duplicate `version` message disconnects the peer (C++
`net_processing.cpp:1617-1626,1574-1582`).

The handshake sends `version`, then reads until it has seen both the peer's
`version` (to which it replies `verack`) and the peer's `verack`; unrelated
messages are ignored. A 60 s deadline bounds the exchange (C++
`DEFAULT_PEER_CONNECT_TIMEOUT`, `net.h:83`).

#### 1.3.1 `net_addr` / CAddress layout

Each `version`-message addr field is the legacy 26-byte CAddress:

```
services(8 LE) || ip(16) || port(2 BE)
```

- `ip` is 16 bytes; IPv4 is mapped into `::ffff:/96` (matches
  `CNetAddr::Serialize`).
- `port` is **big-endian** (network byte order), unlike the rest of the frame.
- **No per-addr `nTime`.** The 4-byte `nTime` belongs only to `addr`/`getaddr`
  records (30-byte, see `p2p/addr.go`); the `version` message's time role
  is filled by the message-level `timestamp` field above. Writing `nTime` into
  the `version` addrs misaligns every following field and causes the peer to
  drop the handshake.

**VERIFIED:** the version/verack handshake was exercised against a live
Blocknet 4.4.1 service node (`coreproxy.airdns.org:42111`, magic `a1 a0 a2 a3`);
the peer returned `proto=70713`, `ua="/Blocknet:4.4.1/"`, `relay=true`, and
immediately began emitting `xbridge` messages (the `xbridge` command string is
confirmed). The 26-byte (no-nTime) `version` CAddress layout is the form
real service nodes accept.

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
| 2  | `xbcXChatMessage`       | relay envelope (no C++ writer; body type removed on B5) |
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
| 19 | `xbcTransactionConfirmedA` | A confirmed (hub→A): A's pay tx id |
| 20 | `xbcTransactionConfirmB`| B confirms A's deposit |
| 21 | `xbcTransactionConfirmedB` | B confirmed |
| 22 | `xbcTransactionCancel`  | cancel (uint256 id, uint32 reason) |
| 24 | `xbcTransactionFinished`| finished |
| 26 | `xbcTransactionReject`  | reject (uint256 id, uint32 reason) |
| 50 | `xbcServicesPing`       | supported-services ping (no C++ writer; body type removed on B5 — `DecodeBody` rejects) |

Implemented in `proto/command.go`. Commands 2 (`xbcXChatMessage`) and 50 have
no C++ writer on either side; their speculative body types were removed on B5
(WIRE-F67/F68) and `DecodeBody` returns an unsupported-command error for them.

---

## 4. Swap handshake (state machine)

From the order/accept/hold/init/create/confirm sequence documented in
`xbridgepacket.h:69-99` and the `Transaction::State` enum
(`src/xbridge/xbridgetransaction.h:36-50`):

```
trNew -> trJoined -> trHold -> trInitialized -> trCreated -> trSigned
     -> trCommited -> trFinished   (or trCancelled / trDropped)

(`trSigned` / `trCommited` are vestigial — C++ never assigns them during the
two-confirmation gate; the progression walks trJoined → trHold → trInitialized
→ trCreated → trFinished via `increaseStateCounter`. This is the HUB-side
machine: the go-xbridge thin client never runs it — its client progression is
`api.SwapSession`/`clientState`, see `docs/architecture.md` "swap/".)
```

High-level flow (hub = the **service node**; note this is *not* the maker — the
maker is client A and the taker is client B. The service node brokers the
handshake. clients A and B are the two traders):

```
A (maker)                       hub                     B (taker)
xbcTransaction --------------------->  (hub relays + broadcasts xbcPendingTransaction)
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

Addressing and trust model (client side):

- Every outbound packet (make `xbcTransaction`, accept
  `xbcTransactionAccepting`, and each handshake reply) is envelope-addressed to
  the order's hub — the 20-byte `destination` (§1.2). Clients pick the hub at
  `MakeOrder` (`findNodeWithService`) and record it as `Order.SNodePubkey` +
  `Order.HubAddress`; a taker adopts the hub from the order it takes.
- Clients bind handlers for **only** `xbcPendingTransaction` and the hub-driven
  handshake commands; an inbound `xbcTransaction` (cmd-3, server-side command)
  has no client handler and is ignored (C++ `xbridgesession.cpp:184-198`).
- Inbound handshake packets are re-verified against the order's trusted hub key
  (C++ `packet->verify(xtx->sPubKey)`, `xbridgesession.cpp:1364`) plus the
  registry `getSn` check (`:1384`), so a forged `Finished` can never disable the
  refund watcher. `xbcPendingTransaction` (cmd-4) is authenticated by the packet
  signature against its header pubkey (C++ `packet->verify(spubkey)`,
  `:736`) — the 20-byte "hub" field is the broadcaster's per-session id
  (`m_myid`, `xbridgesession.cpp:182-183`), a routing handle stored verbatim,
  **not** `GetID` of the signing key — and a relayed copy never re-orders a
  known order (`processPendingTransaction`, `xbridgesession.cpp:725,753-788`).
  Orders for any currency pair are ingested so the client can watch them;
  taking is gated on the order's `SNodePubkey` being a known, running
  servicenode in the local registry (C++ `acceptXBridgeTransaction` getSn,
  `xbridgeapp.cpp:2165-2197`).

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

### 4.1 Swap command body layouts (authoritative — CORRECTED 2026-07-15)

These are the exact on-the-wire field orders for the 13 swap commands. They were
read from the **real C++ writers** (`xbridgeapp.cpp` / `xbridgesession.cpp`),
**not** from the `xbridgepacket.h` enum comments, which are STALE and must NOT
be trusted. The most important correction: the `Create*` / `Created*` /
`Confirm*` / `Confirmed*` bodies carry **no `ClientAddress`** — only `HoldApply`
(7), `Init` (8) and `Initialized` (9) do. (See the note at the top of
`proto/body_types.go`.)

| Cmd | Name | Field order |
|-----|------|-------------|
| 6  | `xbcTransactionHold`       | Hub ‖ ID ‖ FromAmount ‖ ToAmount |
| 7  | `xbcTransactionHoldApply`  | Hub ‖ Client ‖ ID |
| 8  | `xbcTransactionInit`       | Client ‖ Hub ‖ ID ‖ FromAddr ‖ FromCur ‖ FromAmt ‖ ToAddr ‖ ToCur ‖ ToAmt |
| 9  | `xbcTransactionInitialized`| Hub ‖ Client ‖ ID |
| 10 | `xbcTransactionCreateA`    | Hub ‖ ID ‖ BPubKey |
| 11 | `xbcTransactionCreatedA`   | Hub ‖ ID ‖ ADepositTxID ‖ HashedSecret ‖ ALockTime ‖ RefTxID ‖ RefTx |
| 12 | `xbcTransactionCreateB`    | Hub ‖ ID ‖ APubKey ‖ ADepositTxID ‖ HashedSecret ‖ ALockTime |
| 13 | `xbcTransactionCreatedB`   | Hub ‖ ID ‖ BDepositTxID ‖ BLockTime ‖ RefTxID ‖ RefTx |
| 18 | `xbcTransactionConfirmA`   | Hub ‖ ID ‖ BDepositTxID ‖ BLockTime |
| 19 | `xbcTransactionConfirmedA` | Hub ‖ ID ‖ APayTxID |
| 20 | `xbcTransactionConfirmB`   | Hub ‖ ID ‖ APayTxID |
| 21 | `xbcTransactionConfirmedB` | Hub ‖ ID ‖ BPayTxID |
| 24 | `xbcTransactionFinished`   | ID |

Notes:

- **`HashedSecret`** is the 20-byte HASH160 of the maker's secret preimage (the
  maker's 33-byte compressed xPubKey). Both deposits share it.
- **`RefTx`** (11, 13) is the pre-signed IF-branch CLTV refund (hex), spendable
  only after the deposit's `ALockTime` / `BLockTime`. `RefTxID` is its txid.
- **Locktimes** are absolute block heights: `currentBlock + target / blockTime`,
  with target 7200 s (maker, A) / 1800 s (taker, B) per `xbridgewallet.h`. The
  deposit tx itself carries `LockTime = 0`; the CLTV lives on the refund/payment
  *spend*.
- The **HTLC redeem script** (`coins.BuildDepositUnlockScript`) is:
  - IF branch: `CLTV refund to DepositorPub` (enabled by input sequence
    `< 0xffffffff` on the refund tx);
  - ELSE branch: `pay CounterpartyPub if HASH160(preimage) == secretHash`.
  - The maker's deposit A has `DepositorPub = makerPubKey`,
    `CounterpartyPub = takerPubKey`; the taker's deposit B is mirrored. Both use
    the same `secretHash`.
- The maker reveals the secret by broadcasting the ELSE-branch payTx that spends
  B's deposit; the taker recovers the 33-byte preimage from A's payTx
  (`GetRawTransaction(APayTxID)`, first 33-byte push of input[0].ScriptSig) and
  spends A's deposit. This is implemented in `api/swap.go` (`TestSwapHandshake`
  in `api/swap_test.go` drives it end-to-end with fake connectors).

### 4.2 Non-swap body layouts (order broadcast, accept, cancel)

The order/accept lifecycle and cancellation use their own bodies (read from the
same real C++ writers as §4.1). Field encoding is per §2.2.

| Cmd | Name | Field order |
|-----|------|-------------|
| 3  | `xbcTransaction`          | ID ‖ From(addr) ‖ FromCurrency ‖ FromAmount ‖ To(addr) ‖ ToCurrency ‖ ToAmount ‖ Created ‖ BlockHash ‖ PartialAllowed(uint16 0/1) ‖ MinFromAmount ‖ Utxos |
| 4  | `xbcPendingTransaction`   | ID ‖ FromCurrency ‖ FromAmount ‖ ToCurrency ‖ ToAmount ‖ HubAddress ‖ Created ‖ BlockHash ‖ PartialAllowed(uint16 0/1) ‖ MinFromAmount* |
| 5  | `xbcTransactionAccepting` | HubAddress ‖ ID ‖ ServiceNodeFeeTx(uint32-len ‖ bytes) ‖ From ‖ FromCurrency ‖ FromAmount ‖ FromBlockHeight ‖ FromBlockHash(8) ‖ To ‖ ToCurrency ‖ ToAmount ‖ ToBlockHeight ‖ ToBlockHash(8) ‖ Utxos |
| 22 | `xbcTransactionCancel`    | ID ‖ Reason |
| 26 | `xbcTransactionReject`    | ID ‖ Reason |

Notes:

- **`Utxos`** is a `uint32` LE count followed by that many 121-byte entries. Each
  `UtxoEntry` is `TxID(32) ‖ Vout(4) ‖ RawAddress(20) ‖ Signature(65)` — the
  signature is 65 bytes (1 recovery byte + 64) in `signmessage` format, and
  `RawAddress` is the 20-byte `toXAddr` payload (**not** the base58 address; the
  version byte is stripped, matching C++). See `proto/body_types.go:36-69`.
- **`BlockHash`** (3, 4) is the full 32-byte block hash, Bitcoin internal LE
  order; `FromBlockHash`/`ToBlockHash` (5) are only the **first 8 bytes**.
- **`ServiceNodeFeeTx`** (5) is a variable-length byte array **prefixed by its
  own `uint32` length** (the one field in the protocol that is length-prefixed
  rather than fixed-width/terminated).
- **`MinFromAmount`\*** (4) is **optional**: present only in writers that
  advertise a minimum partial amount. Of C++'s two cmd-4 writers, the broadcast
  writer omits it (`xbridgesession.cpp:3626`) while `sendTransaction` always
  includes it (`:3666`). Go's `Marshal` always writes it (`proto/body_types.go:204`)
  but Go only receives cmd 4, never sends it, so readers tolerate its absence
  (`proto/body_types.go:255-262`).
- **`Created`** (3, 4) is the order creation time in **microseconds since the
  Unix epoch** — the wire writers use `total_microseconds()` (`xutil.cpp:280`),
  and Go passes the u64 through unchanged (`api/store.go` `NowMicro`), so an
  encoder must emit µs, not seconds. (The 8-byte P2P *envelope* timestamp is
  also µs; only the packet *header* `timestamp` is seconds — §2.1.)
- `xbcXChatMessage` (2) carries a raw serialized Bitcoin P2P message as its
  entire body (no inner structure); `xbcServicesPing` (50) is a sequence of
  null-terminated service-name strings (typed in Go but no C++ live writer — see
  `docs/architecture.md`).

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
