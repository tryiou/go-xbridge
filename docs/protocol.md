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
is required. The C++ node expects a well-formed `version` message. Implemented
in `p2p/version.go` (`VersionMessage`, `NetAddr`, `Marshal`, `NewVersion`) and
driven by `p2p/conn.go`'s `handshake()`.

Wire layout (`p2p/version.go` `Marshal`):

```
version(4 LE) || services(8 LE) || timestamp(8 LE) ||
addr_recv(30) || addr_from(30) || nonce(8 LE) ||
user_agent(varstr) || start_height(4 LE) || relay(1) || fxrouter(1)
```

Field values (`p2p/version.go`):

- `version` = `70713` (`BitcoinProtocolVersion`, from `src/version.h:12`).
- `services` = `0` (thin client advertises no services).
- `timestamp` = current unix seconds (`int64`).
- `addr_recv` / `addr_from` = 30-byte CAddress (see §1.3.1). `addr_recv` is the
  peer's IP/port (IPv4 mapped into `::ffff:/96`); `addr_from` is a CAddress with
  `nTime` stamped and services=0, but IP/port left zeroed (thin client advertises
  no address of its own).
- `nonce` = random `uint64`.
- `user_agent` = `"/go-xbridge:0.1.0/"`.
- `start_height` = `0` (thin client has no chain).
- `relay` = `false`.
- `fxrouter` = `false` (thin client is not an XRouter hub). Sent explicitly so
  address gossip/discovery works against stock service nodes.

The handshake sends `version`, then reads until it has seen both the peer's
`version` (to which it replies `verack`) and the peer's `verack`; unrelated
messages are ignored. A 30 s deadline bounds the exchange.

#### 1.3.1 `net_addr` / CAddress layout

Each addr field is a Bitcoin `CAddress`, 30 bytes:

```
nTime(4 LE) || services(8 LE) || ip(16) || port(2 BE)
```

- `nTime` is always written for `PROTOCOL_VERSION >= CADDR_TIME_VERSION (31402)`;
  Blocknet's `PROTOCOL_VERSION` is 70713, so it is always present
  (`src/protocol.h:379-381`, `src/net_processing.cpp:207-211`). `NewVersion`
  stamps both addrs with the current unix seconds (`p2p/version.go`).
- `ip` is 16 bytes; IPv4 is mapped into `::ffff:/96` (matches
  `CNetAddr::Serialize`).
- `port` is **big-endian** (network byte order), unlike the rest of the frame.

**VERIFIED:** the version/verack handshake was exercised against a live Blocknet
4.4.1 service node (`coreproxy.airdns.org:42111`, magic `a1 a0 a2 a3`); the peer
returned `proto=70713`, `ua="/Blocknet:4.4.1/"`, `relay=true`, and immediately
began emitting `xbridge` messages (the `xbridge` command string is confirmed).
The addr encoding was corrected to the 30-byte CAddress (incl. `nTime`) on
2026-07-15 — see §1.3.1.

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

(`trSigned` / `trCommited` are vestigial — C++ never assigns them during the
two-confirmation gate; the progression walks trJoined → trHold → trInitialized
→ trCreated → trFinished via `IncreaseStateCounter`. See `docs/swap.md` §2.)
```

High-level flow (hub = the **service node**; note this is *not* the maker — the
maker is client A and the taker is client B. The service node brokers the
handshake. clients A and B are the two traders):

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

### 4.1 Swap command body layouts (authoritative — CORRECTED 2026-07-15)

These are the exact on-the-wire field orders for the 8 swap commands. They were
read from the **real C++ writers** (`xbridgeapp.cpp` / `xbridgesession.cpp`),
**not** from the `xbridgepacket.h` enum comments, which are STALE and must NOT
be trusted. The most important correction: the `Create*` / `Created*` /
`Confirm*` / `Confirmed*` bodies carry **no `ClientAddress`** — only `HoldApply`
(7), `Init` (8) and `Initialized` (9) do. (See the note at the top of
`proto/body_types.go`.)

| Cmd | Name | Field order |
|-----|------|-------------|
| 6  | `xbcTransactionHold`       | Hub ‖ ID |
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

1. **`version` handshake payload** (§1.3, §1.3.1) — ✅ DONE.
2. **`NetMsgType::XBRIDGE` command string** — ✅ confirmed `"xbridge"` live.
3. **uint256 byte order** — ✅ VERIFIED. Bitcoin internal LE order is preserved
   verbatim on the wire (no reversal): C++ appends `blockHash.begin()` for 32
   bytes (xbridgeapp.cpp:2082), and Go's `body_types.go` carries uint256 fields
   byte-for-byte. Matches.
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
5. **Wallet connector RPC** — ✅ DONE. `wallet/` implements the `Connector`
   interface with `RPCConnector` (JSON-RPC to a Blocknet-core-compatible wallet,
   incl. `signmessage`/`getrawtransaction` for BIP137 proofs and `OnConfirm*`
   refund/payment) and `LocalConnector` (local signing). See `docs/wallet.md`.
6. **API surface** — ✅ DONE. All 23 `dx*` commands from `rpcxbridge.cpp` are
   ported 1:1 in `api/`; the three-party client driver (`api/swap.go`) runs the
   Maker ⇄ Hub ⇄ Taker handshake. See `docs/api.md`.
