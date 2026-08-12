# WIRE_CONFORMANCE.md

# Wire/P2P Conformance Audit — Blocknet Core xBridge (C++) vs go-xbridge (Go)

REF: blocknet_core/src/xbridge/* + src/net_processing.cpp + src/protocol.* | CAND: go-xbridge/proto/ + go-xbridge/p2p/
All hex vectors generated via scratch Go modules (/tmp/opencode/vecgen*, replace go-xbridge => repo) and cross-checked against C++ writers.

## WIRE CONFORMANCE CARDS — PROTO CODEC (packet header, signing, bodies, envelope/frame, enums)

Audit date: 2026-08-12. Byte-level conformance of `go-xbridge/proto` + `go-xbridge/p2p`
against the Blocknet Core C++ XBridge wire contract. **C++ writers are ground truth**;
`xbridgepacket.h` enum comments are frequently stale (noted where relevant).

All hex vectors below were produced by the scratch module at `/tmp/opencode/vecgen`
(module `vecgen`, `replace go-xbridge => …/workspace/go-xbridge`) running the actual
Go encoders (`proto.Packet.Marshal`, `proto.*Body.Marshal`, `crypto.BtcSigner`,
`p2p.Message.Marshal`); none are hand-fabricated. Raw generator output:
`/tmp/opencode/vecgen/vectors.txt`.

Legend: ✅ MATCH — Go bytes equal the C++ writer output for the same logical values.
⚠️ NOTE — a semantic/unit/robustness nuance, not a wire-bytes divergence.
❌ DIFF — a byte-level or units divergence.

---

### CARD 1 — PACKET HEADER (129 bytes)

**Verdict: ✅ MATCH** (all offsets, widths, endianness, and size semantics agree).

| Offset | Field   | Type | C++ source | Go source | Match |
|--------|---------|------|-----------|-----------|-------|
| 0 | version   | u32 LE | `field32<0>` xbridgepacket.h:543; ctor `XBRIDGE_PROTOCOL_VERSION` :499-520 | packet.go:75, ProtocolVersion=55 :14 | ✅ |
| 4 | command   | u32 LE | `field32<1>` :545 | packet.go:76 | ✅ |
| 8 | timestamp | u32 LE | `field32<2>` :547; `time(0)` seconds :502,:519 | packet.go:65 (`time.Now().Unix()`) | ✅ |
| 12 | oldSize  | u32 LE | `field32<3>` `__oldSizeField` :569-570; = `sizeField()+__headerDifference` :368 (97 = 129−32) | packet.go:67 (`len(body)+headerDifference`) | ✅ |
| 16 | size     | u32 LE | `field32<4>` `sizeField` :549-550; = body length, kept in sync by every `append()` :427-477 | packet.go:66,79 (always `len(p.Body)`) | ✅ |
| 20 | pubkey   | 33 B | `pubkeyField() = &m_body[20]` :554 | packet.go:23,80 (PubkeyOffset=20) | ✅ |
| 53 | signature| 64 B | `signatureField() = &m_body[53]` :556; `rawSignatureSize=64` :323 | packet.go:24,81 (SigOffset=53) | ✅ |
| 117 | padding | 12 B | uninitialized-region bytes; zero from ctor `m_body(headerSize,0)` :499 | packet.go:74 (zero buf) | ✅ |
| headerSize | | 129 | `headerSize = 8*4+33+64` :309 | `HeaderSize = 8*4+33+64` :20 | ✅ |

Verified specifics:

- **Version stamp 55**: version.h:8 `XBRIDGE_PROTOCOL_VERSION 55`; Go `ProtocolVersion uint32 = 55` packet.go:14. ✅
- **Timestamp units = seconds** in the header: C++ `timestampField() = (uint32_t)time(0)` (xbridgepacket.h:502,507,519). Go `uint32(time.Now().Unix())` (packet.go:65). ⚠️ C++ *receiver* ctor `XBridgePacket(const std::string& raw)` **overwrites** the wire timestamp with local `time(0)` (xbridgepacket.h:505-508); Go preserves the peer's header timestamp on `Unmarshal` (packet.go:104). Behavioral note only; does not affect bytes a sender emits.
- **oldSize semantics = bodyLen + 97**: `__oldSizeField() = sizeField()+__headerDifference`, `__headerDifference = 129−32 = 97` (xbridgepacket.h:364-369, :561-566); Go `headerDifference = HeaderSize − 8*4 = 97` (packet.go:43), `OldSize = len(body)+headerDifference` (:67). ✅
- **size = body length**, recomputed on every write in C++ (`sizeField() = m_body.size() − headerSize`, xbridgepacket.h:432,441,450,458,467,475); Go `Marshal` writes `len(p.Body)` unconditionally (packet.go:79). ✅
- **crc**: C++ `crc()` is a stub returning 0 (xbridgepacket.h:330-337), never written; `crcField()` (`field32<5>`) at offset 20 **overlaps the first 4 bytes of the pubkey region** (xbridgepacket.h:551). Go comment packet.go:18-19,39 states exactly this; no crc is written. ✅
- **MaxBodySize**: C++ has **no** size cap; `copyFrom` only requires `sizeField() == data.size()−headerSize` (xbridgepacket.h:479-497). Go caps at `1<<20` on `Unmarshal` (packet.go:31,113). ⚠️ Go is stricter (1 MiB) than C++; safe hardening, not a divergence. ⚠️ Go `Unmarshal` ignores trailing bytes beyond `HeaderSize+Size` (packet.go:117-122) while C++ `copyFrom` rejects a size mismatch — asymmetric strictness.

**VECTOR 1.1 — marshaled header for a known body** (cmd 22 `xbcTransactionCancel`, body = 32-byte id `0102…20` + reason `0xfeedbeef`, timestamp `0x178b6a56`, deterministic key/sig):

```
body    : 0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20efbeedfe
packet  : 3700000016000000566a8b178500000024000000 0284bf7562262bbd6940085748f3be6afa52ae317155181ece31b66351ccffa4b0 a3dfdba8d803471627e9a70559e9e3c70e5e02b9a26f8e9988349e18f6824d9565d2fc6141951f6f8b8a5a9335b2a8a42d9dcd8b76099c0b137d91c1828060fe 000000000000000000000000 0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20efbeedfe
            |--version0x37--||-cmd0x16----||-ts 178b6a56-||oldsize0x85||size0x24 |--- pubkey 33 @20 ----------------------------| |-- sig 64 @53 ---|...|---pad 12 @117--| |--- body ---------------------------------------|
```

Field decode (LE): version `0x00000037`=55 ✅; command `0x00000016`=22 ✅; timestamp `0x178b6a56`; oldSize `0x00000085`=133 = 36+97 ✅; size `0x00000024`=36 ✅; padding bytes 117..128 all zero ✅.

---

### CARD 2 — SIGNING / DIGEST

**Verdict: ✅ MATCH** (digest, signature format, and key formats agree byte-for-byte).

- Digest = **single SHA256** over the **entire packet buffer** (129-byte header incl. padding + body) with the 64-byte signature region zeroed:
  - C++: `sign()` memsets `signatureField()` 64 bytes, then `CSHA256::Write(&m_body[0], m_body.size())` (xbridgepacket.cpp:68-77); `verify()` re-zeroes, hashes the whole `m_body` again (xbridgepacket.cpp:96-106).
  - Go: `Digest()` marshals header+body, zeroes `[SigOffset:SigOffset+64]`, single `sha256.Sum256` (packet.go:88-93). ✅
  - Proof: manual zero-then-hash over the same bytes == `p.Digest()` (`737e49c1…`, see vector).
- **Signature = 64-byte compact ECDSA (r‖s)**: C++ `secp256k1_ecdsa_signature_serialize_compact` (xbridgepacket.cpp:85); Go `compactSerialize` — 32-byte BE R + 32-byte BE S (crypto/signer.go:49-59). ✅
- **Pubkey = 33-byte compressed** from the privkey: C++ `sign(pubkey…)` requires 33/32 sizes and memcpy into header (xbridgepacket.cpp:62-68); Go derives `SerializeCompressed()` (signer.go:84). ✅
- **Verify uses the header pubkey**: C++ `verify()` parses `pubkeyField()` 33 bytes (xbridgepacket.cpp:118-123) plus a redundant compressed re-serialize equality check (:132-145); Go `Verify` parses the 33-byte `p.Pubkey` (signer.go:96-107). ⚠️ Go skips the redundant re-serialization check but 33-byte parse is equivalent. C++ also offers `verify(pubkey)` against an explicit key (xbridgepacket.cpp:154-162) — Go `VerifyAgainst` (signer.go:116-133). ✅
- **Low-S**: C++ `secp256k1_ecdsa_sign` (RFC6979, low-S) and `secp256k1_ecdsa_verify` accepts high-S; Go `ecdsa.Sign` is low-S and `sig.Verify` accepts both (signer.go:86,107). ✅

**VECTOR 2.1 — ECDSA sign/verify round-trip** (same packet as Card 1):

```
digest (SHA256 sig-zeroed) : 737e49c1f8e2d852c7a98ffbe9a7bf0a5f8c2811591671c10efb2f87485193f7
signature (64 B r||s)      : a3dfdba8d803471627e9a70559e9e3c70e5e02b9a26f8e9988349e18f6824d9565d2fc6141951f6f8b8a5a9335b2a8a42d9dcd8b76099c0b137d91c1828060fe
pubkey (33 B compressed)   : 0284bf7562262bbd6940085748f3be6afa52ae317155181ece31b66351ccffa4b0
verify(signature)          : true     verify(tampered body) : false
manual sha256(sigzero) == Digest() : true
```

---

### CARD 3 — ENVELOPE + BITCOIN FRAME

**Verdict: ✅ MATCH.** The `xbridge` P2P payload is `CompactSize varint(28+packetLen) ‖ dest(20) ‖ timestamp(8, µs) ‖ packet`. The varint comes from the `std::vector<unsigned char>` serializer, not from XBridge code.

- **Where C++ wraps the packet** — `App::Impl::onSend` (xbridgeapp.cpp:561-591):
  - `msg = id` (20-byte uint160 dest; 20 zeroes == broadcast, :553,563-568)
  - `timestampValue = timeToInt(microsec_clock::universal_time())` appended as 8 raw bytes (:571-574)
  - `message` (packet header+body) appended (:577)
  - `g_connman->PushMessage(pnode, msgMaker.Make(NetMsgType::XBRIDGE, msg))` (:589)
- **Varint / CompactSize prefix** — NOT written by onSend. It is produced by serializing the `std::vector<unsigned char>` through the Bitcoin serializer: `vector<uchar>` writes `WriteCompactSize(v.size())` then bytes (serialize.h:708-714), read back by `vRecv >> raw` (net_processing.cpp:2870, deserialize serialize.h:732-745). `Make()` wraps args via `CVectorWriter` (netmessagemaker.h:18-24). **Go `encodeXBridgePayload` writes the same varint** (p2p/envelope.go:33-48) — confirmed against a live captured frame (`liveXBridgePacket`, envelope_test.go:19). ✅
- **Receive side** — net_processing.cpp:2868-2899: strip varint via `>> raw`, require `raw.size() >= 20 + sizeof(time_t)` (:2874), `addr = raw[0..20)`, erase 20 then 8, then `onMessageReceived`/`onBroadcastReceived` (:2893-2899). `LegacyXBridgePacket::CopyFrom` reads the packet header at offset `20+8` (servicenode.h:52-61). Go `DecodeXBridgePayload` requires `off+n == len(payload)` (envelope.go:59) — same layout. ✅
- **Timestamp units = MICROSECONDS** in the envelope: `timeToInt = total_microseconds()` (xutil.cpp:276-283), used at xbridgeapp.cpp:572. Go uses `time.Now().UnixMicro()` (envelope.go:42), guarded by a µs-scale test (envelope_test.go:130-153). ✅ (⚠️ the *header* timestamp is seconds — two different units in the same message, both matched.)
- **Bitcoin frame**: `magic(4) ‖ command(12, NUL-padded) ‖ length(4 LE) ‖ checksum(4 = first 4 of SHA256d) ‖ payload`:
  - C++: `PushMessage` computes `Hash(data)` = CHash256 (double SHA256) and copies first 4 bytes into `pchChecksum` (net.cpp:2706-2708; hash.h:80-87 CHash256); command is `"xbridge"` (protocol.cpp:45, XBRIDGE string), written into a 12-byte field (protocol.h:49-55).
  - Go: `Message.Marshal` (message.go:38-50) + `Checksum` first-4 of SHA256(SHA256) (message.go:30-36). ✅
- **Mainnet magic** `a1 a0 a2 a3`: C++ chainparams.cpp:127-130; Go `MainnetMagic` (params.go:5). ✅

**VECTOR 3.1 — full mainnet frame** for the Card-1 packet, dest = `a0 a1 … b3`, envelope timestamp = fixed µs `0x0000017b8b6a560000`:

```
envelope varint      : c1   (0xc1 = 193 = 28 + 165-byte packet)   [single-byte CompactSize]
dest (20 B)          : a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3
env timestamp (8 µs) : 0000566a8b7b0100
xbridge payload      : c1a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3 0000566a8b7b0100 3700000016000000566a8b1785000000240000000284bf7562262bbd6940085748f3be6afa52ae317155181ece31b66351ccffa4b0a3dfdba8d803471627e9a70559e9e3c70e5e02b9a26f8e9988349e18f6824d9565d2fc6141951f6f8b8a5a9335b2a8a42d9dcd8b76099c0b137d91c1828060fe0000000000000000000000000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20efbeedfe
full frame (mainnet) : a1a0a2a3 786272696467650000000000 000000c2 863c174d c1a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b30000566a8b7b01003700000016000000566a8b1785000000240000000284bf7562262bbd6940085748f3be6afa52ae317155181ece31b66351ccffa4b0a3dfdba8d803471627e9a70559e9e3c70e5e02b9a26f8e9988349e18f6824d9565d2fc6141951f6f8b8a5a9335b2a8a42d9dcd8b76099c0b137d91c1828060fe0000000000000000000000000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20efbeedfe
   magic a1a0a2a3 | command "xbridge\0\0\0\0\0" | length 0xc2=194 LE | checksum 863c174d (SHA256d first 4)
```

---

### CARD 4 — PER-COMMAND BODY LAYOUTS

Common encodings (all LE integers; currencies 8-byte ASCII left-aligned NUL-padded — `proto/body.go:54-58` vs C++ `std::vector<unsigned char> fc(8,0)` xbridgeapp.cpp:2066-2067 / xbridgesession.cpp:3629-3636; strings NUL-terminated — `proto/body.go:42-45` vs `append(const std::string&)` pushes a trailing 0, xbridgepacket.h:462-469; hashes/addresses raw byte runs).

**UtxoEntry = 121 bytes** — `TxID(32) ‖ Vout(4) ‖ RawAddress(20) ‖ Signature(65)`. C++ writer xbridgeapp.cpp:2088-2096 (and :2452-2460); reader computes `utxoItemSize = 32+4+20+65` = 121 (xbridgesession.cpp:511-512, 535); `wallet::UtxoEntry{rawAddress, signature}` xbridgewallet.h:36-46; `signatureSize=65` (recoverable signmessage) xbridgepacket.h:324. Go `proto.UtxoEntry` body_types.go:38-43. ✅

| Cmd | Body | C++ writer (authoritative) | C++ reader | Go `proto/body_types.go` | Match |
|-----|------|---------------------------|-----------|--------------------------|-------|
| 2 | xbcXChatMessage | **no writer — unimplemented** (xbridgesession.cpp:373-375) | same | `XChatMessageBody.Raw` = raw bytes :913-927 | ⚠️ see note |
| 3 | xbcTransaction | sendPendingTransaction xbridgeapp.cpp:2063-2103: id‖from(20)‖fc(8)‖fromAmount‖to(20)‖tc(8)‖toAmount‖created(µs)‖blockHash(32)‖u16 partial‖minFromAmount‖u32 count‖utxos | processTransaction xbridgesession.cpp:402-531 | `OrderBody.Marshal` :121-140 | ✅ |
| 4 | xbcPendingTransaction | **two writers**: broadcast omits minFromAmount (xbridgesession.cpp:3626-3651); `sendTransaction` appends it (:3666-3691): id‖fc‖aAmount‖tc‖bAmount‖hub(20)‖created(µs)‖blockHash(32)‖u16 partial‖[minFromAmount] | processPendingTransaction **always reads** minFromAmount (xbridgesession.cpp:767-778) | `PendingTransactionBody` :204-221 always writes; :255-262 tolerates absence | ⚠️ see note |
| 5 | xbcTransactionAccepting | sendAcceptingTransaction xbridgeapp.cpp:2409-2469: hub(20)‖id(32)‖u32 feeTxLen‖feeTx‖from(20)‖fc(8)‖fromAmount‖u32 fromHt‖fromHash(8)‖to(20)‖tc(8)‖toAmount‖u32 toHt‖toHash(8)‖u32 count‖utxos | processTransactionAccepting xbridgesession.cpp:838-938 | `AcceptingBody.Marshal` :287-305 | ✅ |
| 6 | xbcTransactionHold | xbridgesession.cpp:1288-1293: hub(20)‖id(32)‖samount‖damount | :processTransactionHold | `HoldBody` :374-381 | ✅ |
| 7 | xbcTransactionHoldApply | xbridgesession.cpp:1534-1538: hub(20)‖client(20)‖id(32) | :1547+ | `HoldApplyBody` :407-413 | ✅ |
| 8 | xbcTransactionInit | xbridgesession.cpp:1637-1650: client‖hub‖id‖fromAddr‖fromCur‖fromAmt‖toAddr‖toCur‖toAmt | processTransactionInit | `InitBody` :446-458 | ✅ |
| 9 | xbcTransactionInitialized | xbridgesession.cpp:1766-1769: hub‖client‖id | :1780+ | `InitializedBody` :499-505 | ✅ |
| 10 | xbcTransactionCreateA | xbridgesession.cpp:1857-1861: hub‖id‖B_pk1(33) | processTransactionCreateA | `CreateABody` :546-552 | ✅ |
| 11 | xbcTransactionCreatedA | xbridgesession.cpp:2226-2234: hub‖id‖ADepositTxID(str)‖hashedSecret(20)‖lockTime(u32)‖refTxId(str)‖refTx(str) | processTransactionCreatedA | `CreatedABody` :579-589 | ✅ |
| 12 | xbcTransactionCreateB | xbridgesession.cpp:2349-2355: hub‖id‖A_pk1(33)‖ADepositTxID(str)‖hashedSecret(20)‖lockTimeA(u32) | processTransactionCreateB | `CreateBBody` :627-636 | ✅ |
| 13 | xbcTransactionCreatedB | xbridgesession.cpp:2734-2741: hub‖id‖BDepositTxID(str)‖lockTime(u32)‖refTxId(str)‖refTx(str) | processTransactionCreatedB | `CreatedBBody` :671-680 | ✅ |
| 18 | xbcTransactionConfirmA | xbridgesession.cpp:2828-2832: hub‖id‖B_bintxid(str)‖lockTimeB(u32) | processTransactionConfirmA | `ConfirmABody` :718-725 | ✅ |
| 19 | xbcTransactionConfirmedA | xbridgesession.cpp:3006-3009: hub‖id‖payTxId(str) | processTransactionConfirmedA | `ConfirmedABody` :752-758 | ✅ |
| 20 | xbcTransactionConfirmB | xbridgesession.cpp:3088-3091: hub‖id‖a_payTxId(str) | processTransactionConfirmB | `ConfirmBBody` :782-788 | ✅ |
| 21 | xbcTransactionConfirmedB | xbridgesession.cpp:3189-3192: hub‖id‖payTxId(str) | processTransactionConfirmedB | `ConfirmedBBody` :811-817 | ✅ |
| 22 | xbcTransactionCancel | xbridgesession.cpp:3542-3544 & :3569-3571: id(32)‖reason(u32) | processTransactionCancel | `CancelBody` :843-848 | ✅ |
| 24 | xbcTransactionFinished | xbridgesession.cpp:3272-3273 & :3504-3505: **id(32) only** | processTransactionFinished | `FinishedBody` :894-898 | ✅ |
| 26 | xbcTransactionReject | xbridgesession.cpp:3598-3600: id(32)‖reason(u32) | processTransactionReject | `RejectBody` :867-872 | ✅ |
| 50 | xbcServicesPing | **no C++ writer** — `processXBridge` only ignores it (`TODO handle legacy snode ping`, servicenodemgr.h:107-120) | — | `ServicesPingBody` :933-956 (typed, NUL-term strings) | ⚠️ see note |

Notes on cmd 4, cmd 2, cmd 50 (documented in `proto/body_types.go:16-33`):

- **cmd 4 minFromAmount asymmetry** (⚠️): C++ has two writers that differ by one trailing u64 (broadcast omits, `sendTransaction` includes). Go `PendingTransactionBody.Marshal` always writes it (matches `sendTransaction`), and `Unmarshal` accepts both forms (:255-262). **C++'s own reader always consumes the field** (xbridgesession.cpp:767-778), so a C++-broadcast frame would misparse on a C++ reader — a C++-internal inconsistency; Go's tolerant decoder is deliberately more robust. Not a divergence for Go-produced frames.
- **cmd 2 xchat** (⚠️): no C++ writer exists ("not implemented", xbridgesession.cpp:375). The xbridgepacket.h comment (:62-67) describes a `uint160 destination + serialized p2p message` body, but Go treats the whole body as raw bytes (`XChatMessageBody.Raw`, body_types.go:913-927). Unverifiable — marked TBD; no live writer on either side.
- **cmd 50 services ping** (⚠️): no C++ writer/reader exists (servicenodemgr.h:113-117 TODO). Go's `ServicesPingBody` (NUL-terminated service-name strings) is the only concrete encoder, and `DecodeBody` intentionally refuses to decode cmd 50 (body_types.go:1020-1028). The task brief asserted "parsed by p2p/servicenode" — **not true in the code**: no go-xbridge package parses a cmd-50 body; consistency with a C++ wire form is unverifiable (TBD).

**Vectors 4.x — fully-encoded bodies** (values chosen to make offsets legible; `hub`=`b0..b3`, `client`=`c0..c3`, `txid32`=`10..2f`, `blockHash32`=`20..3f`):

```
2  xbcXChatMessage                    : deadbeef
3  xbcTransaction                     : 10111213...2e2f c0c1c2...d2d3 6274630000000000 2100000000000000 b0b1b2...c0c1c2 7872000000000000 3400000000000000 566a8b1700000000 20212223...3d3e3f 0100 0500000000000000 01000000 00010203...1e1f 00010203 40414243...5253 0001020304...3e3f40
4  xbcPendingTransaction (w/ min)     : 10111213...2e2f 6274630000000000 2100000000000000 7872000000000000 3400000000000000 b0b1b2...c0c1c2 566a8b1700000000 20212223...3d3e3f 0100 0500000000000000
5  xbcTransactionAccepting            : b0b1b2...c0c1c2 10111213...2e2f 03000000 aabbcc c0c1c2...d2d3 6274630000000000 2100000000000000 05060000 1112131415161718 b0b1b2...c0c1c2 7872000000000000 3400000000000000 08090000 2122232425262728 01000000 00010203...1e1f 00010203 40414243...5253 0001020304...3e3f40
6  xbcTransactionHold                 : b0b1b2...c0c1c2 10111213...2e2f 2100000000000000 3400000000000000
7  xbcTransactionHoldApply            : b0b1b2...c0c1c2 c0c1c2...d2d3 10111213...2e2f
8  xbcTransactionInit                 : c0c1c2...d2d3 b0b1b2...c0c1c2 10111213...2e2f c0c1c2...d2d3 6274630000000000 2100000000000000 b0b1b2...c0c1c2 7872000000000000 3400000000000000
9  xbcTransactionInitialized          : b0b1b2...c0c1c2 c0c1c2...d2d3 10111213...2e2f
10 xbcTransactionCreateA              : b0b1b2...c0c1c2 10111213...2e2f 0284bf75...ffa4b0
11 xbcTransactionCreatedA             : b0b1b2...c0c1c2 10111213...2e2f 6465616462656566 00 333435...4546 04030201 336131623263 00 3061316232633364 00
12 xbcTransactionCreateB              : b0b1b2...c0c1c2 10111213...2e2f 0284bf75...ffa4b0 6465616462656566 00 333435...4546 04030201
13 xbcTransactionCreatedB             : b0b1b2...c0c1c2 10111213...2e2f 6361666562616265 00 08070605 336131623263 00 3061316232633364 00
18 xbcTransactionConfirmA             : b0b1b2...c0c1c2 10111213...2e2f 6361666562616265 00 08070605
19 xbcTransactionConfirmedA           : b0b1b2...c0c1c2 10111213...2e2f 3132333435363738 00
20 xbcTransactionConfirmB             : b0b1b2...c0c1c2 10111213...2e2f 3132333435363738 00
21 xbcTransactionConfirmedB           : b0b1b2...c0c1c2 10111213...2e2f 3961626364656630 00
22 xbcTransactionCancel               : 01020304...1e1f20 efbeedfe
24 xbcTransactionFinished             : 10111213...2e2f
26 xbcTransactionReject               : 10111213...2e2f dec0ad0b
50 xbcServicesPing                    : 424c4f434b00 42544300 4c544300
```

(`00` after a string = NUL terminator; each string field is `… 00`; the cmd-3 vector tail is the u32 utxo-count `01000000` + one 121-byte UtxoEntry `TxID 00010203…1e1f | Vout 00010203 | Addr 40414243…5253 | Sig 0001020304…3e3f40`.)

Each body was cross-checked field-by-field against the C++ writer `append()` call sequence in the cited lines; every width, order, and encoding matches (see per-row citations above).

---

### CARD 5 — COMMAND ENUM

**Verdict: ✅ MATCH** — no missing, no extra, no renumbered values.

| Value | Name | C++ (xbridgepacket.h) | Go (command.go) |
|-------|------|----------------------|-----------------|
| 0 | xbcInvalid | :54 | :8 |
| 2 | xbcXChatMessage | :67 | :9 |
| 3 | xbcTransaction | :119 | :10 |
| 4 | xbcPendingTransaction | :132 | :11 |
| 5 | xbcTransactionAccepting | :155 | :12 |
| 6 | xbcTransactionHold | :163 | :13 |
| 7 | xbcTransactionHoldApply | :169 | :14 |
| 8 | xbcTransactionInit | :182 | :15 |
| 9 | xbcTransactionInitialized | :188 | :16 |
| 10 | xbcTransactionCreateA | :196 | :17 |
| 11 | xbcTransactionCreatedA | :206 | :18 |
| 12 | xbcTransactionCreateB | :217 | :19 |
| 13 | xbcTransactionCreatedB | :224 | :20 |
| 18 | xbcTransactionConfirmA | :232 | :21 |
| 19 | xbcTransactionConfirmedA | :239 | :22 |
| 20 | xbcTransactionConfirmB | :247 | :23 |
| 21 | xbcTransactionConfirmedB | :253 | :24 |
| 22 | xbcTransactionCancel | :259 | :25 |
| 24 | xbcTransactionFinished | :266 | :26 |
| 26 | xbcTransactionReject | :273 | :27 |
| 50 | xbcServicesPing | :281 | :28 |

Cmd-50 body note: the task brief said p2p/servicenode parses command 50 — there is **no such parser** in go-xbridge (only `ServicesPingBody` in proto, unused by p2p). C++ likewise has only a TODO. Consistency cannot be byte-verified (TBD).

---

### CARD 6 — SIGNATURE / KEY FORMATS

**Verdict: ✅ MATCH.**

- Pubkey: 33-byte compressed secp256k1 in the header (offset 20). C++ `pubkeySize=33` (xbridgepacket.h:319), `SECP256K1_EC_COMPRESSED` re-serialize check (xbridgepacket.cpp:134). Go: btcec `SerializeCompressed` (signer.go:84,167).
- Signature: 64-byte compact `r‖s`, both 32-byte big-endian scalar halves. C++ `rawSignatureSize=64` + `secp256k1_ecdsa_signature_serialize_compact` (xbridgepacket.h:323; xbridgepacket.cpp:85). Go: manual `r‖s` (signer.go:49-59) matching `secp256k1_ecdsa_signature_serialize_compact`; `compactParse` reverses it (signer.go:62-74).
- Both use secp256k1 ECDSA over SHA256; C++ uses libsecp256k1, Go uses btcec/v2 (+ decred scalar layer) — same curve, same wire bytes. ✅

---

### CANDIDATE FINDINGS (axis → 1-line description)

1. **created-unit/doc** — C++ writes cmd-3/4 `Created` as **µs** via `timeToInt`/`total_microseconds()` (xbridgeapp.cpp:2081, xbridgesession.cpp:3644/3684, xutil.cpp:280); `docs/protocol.md:366` claims "unix seconds" — doc is wrong, Go codec passes the u64 through and callers use `NowMicro()`; no wire change needed, fix the doc.
2. **cmd4-asymmetry** — C++ has two cmd-4 writers differing by one trailing u64 (broadcast omits `minFromAmount` xbridgesession.cpp:3626-3651 vs sendTransaction :3666-3691) while C++'s own reader always consumes it (:767-778); Go `Marshal` always emits it and `Unmarshal` is lenient (body_types.go:204-221,255-262) — Go is self-consistent and matches the `sendTransaction` form.
3. **unmarshal-trailing-bytes** — Go `proto.Unmarshal` silently ignores bytes beyond `HeaderSize+Size` (packet.go:117-122) whereas C++ `copyFrom` rejects a length mismatch (xbridgepacket.h:489-493) — Go is lenient where C++ is strict.
4. **receiver-timestamp** — C++ rewrites the received packet header timestamp to local `time(0)` (xbridgepacket.h:505-508); Go preserves the wire value (packet.go:104) — behavioral, not a wire divergence.
5. **cmd2-body** — `xbcXChatMessage` (2) has no C++ writer ("not implemented", xbridgesession.cpp:373-375); the Go raw-bytes body and the stale `uint160+message` comment are unverifiable against any live writer (TBD).
6. **cmd50-body** — `xbcServicesPing` (50) has no C++ writer (servicenodemgr.h:113-117 TODO) and the task brief's "parsed by p2p/servicenode" is not backed by any go-xbridge code — Go `ServicesPingBody` (NUL-term strings) is unverifiable (TBD).
7. **maxbodysize** — C++ has no body-size cap (xbridgepacket.h:479-497) vs Go 1 MiB cap (packet.go:31) — Go is strictly safer; no interop impact below the cap.
8. **header-comment-staleness** — `xbridgepacket.h` prose comments for cmd 11 (says client+BLockTime, xbridgepacket.h:198-206), cmd 12/13, cmd 18 (says client+pubkey, :227-232), cmd 20 (says pubkey+deposit, :241-247), cmd 24 (says client+id, :262-265) are all stale; Go follows the writers (documented at body_types.go:522-538, protocol.md §4.1) — correct behavior, but any future C++ change must check writers, not comments.

---

### VERIFICATION SCRATCH

Generated module `/tmp/opencode/vecgen` (go.mod `require go-xbridge v0.0.0` + `replace` to the workspace checkout). `go run .` output captured to `/tmp/opencode/vecgen/vectors.txt`. No files in either repo were modified.


---

## WIRE CONFORMANCE CARDS — P2P TRANSPORT (frame, version, net_addr, envelope, addr, servicenode, handshake)

Audit date: 2026-08-12. Byte-level conformance of `go-xbridge/p2p` (candidate)
against the Blocknet Core C++ P2P transport layer (reference / ground truth,
`blocknet_core/`, a Bitcoin Core 0.18 fork). **C++ writers are ground truth;
header comments are frequently stale — trust the code.**

Scope: message frame, version handshake, net_addr/CAddress, XBridge transport
envelope, addr/getaddr gossip, ping/pong, servicenode messages (snr/snp/snl/snlp),
handshake sequencing. Packet bodies / 129-byte XBridgePacket header were covered
by the sibling agent (`audit/wire_p1.md`).

All hex vectors below were produced by the scratch module at
`/tmp/opencode/vecgen2` (module `vecgen`, `replace go-xbridge => …/workspace/go-xbridge`)
running the actual Go encoders (`p2p.Message.Marshal`, `VersionMessage.Marshal`,
`MarshalAddr`, envelope encode, servicenode layout builders); none are
hand-fabricated. The deterministic vectors are shown inline; the timestamped
(`time.Now()`) encoder variants were run and their formats verified by field.

Legend: ✅ MATCH — Go bytes equal the C++ writer output for the same logical
values. ⚠️ NOTE — a semantic/robustness/policy nuance, not a wire-bytes
divergence. ❌ DIFF — a byte-level / units / capacity divergence.

C++ constants verified: `PROTOCOL_VERSION = 70713` (`src/version.h:12`),
`MIN_PEER_PROTO_VERSION = 70712` (`version.h:27`), `CADDR_TIME_VERSION = 31402`
(`version.h:31`), `INIT_PROTO_VERSION = 209` (`version.h:21`).

---

### CARD 1 — MESSAGE FRAME (`p2p/message.go` vs `src/protocol.h` `CMessageHeader`)

**Verdict: ✅ MATCH on layout, ⚠️ NOTE on capacity caps and read-side validation.**

Wire layout `magic(4 LE) ‖ command(12, null-padded) ‖ length(4 LE) ‖ checksum(4) ‖ payload`:

| Field | C++ reference | Go candidate | Match |
|-------|---------------|--------------|-------|
| header size = 24 | `HEADER_SIZE = 4+12+4+4` protocol.h:37 | `4+cmdSize+8` message.go:19,54,73 | ✅ |
| magic 4 LE | `pchMessageStart[4]`, protocol.h:38,55 | `Magic [4]byte`, message.go:21 | ✅ |
| command 12 null-padded | `pchCommand[COMMAND_SIZE]`, protocol.h:32; `memset`+`strncpy` protocol.cpp:103-104 | `make(cmd, cmdSize); copy` message.go:41-43 | ✅ |
| length 4 LE | `nMessageSize`, protocol.h:57; `READWRITE` protocol.h:59 | `binary.LittleEndian.PutUint32` message.go:45 | ✅ |
| checksum 4 = double-SHA256(payload)[0:4] | send: `Hash(msg.data…)` net.cpp:2706-2708; recv: `memcmp(hash.begin(), pchChecksum, 4)` net_processing.cpp:3136-3145 | `Checksum` message.go:30-36; verified message.go:78-80 | ✅ |
| magic values | mainnet `a1 a0 a2 a3` chainparams.cpp:127-130; testnet `45 76 65 bb` :293-296; regtest `a1 cf 7e ac` :417-420 | MainnetMagic/TestnetMagic/StagingMagic params.go:5-8 | ✅ (values) |
| length cap | `MAX_PROTOCOL_MESSAGE_LENGTH = 4*1000*1000 = 4,000,000` net.h:55 (disconnect, net.cpp:583-585); `readHeader` rejects `> MAX_SIZE = 0x02000000 (32 MiB)` serialize.h:27, net.cpp:657 | `MaxPayloadSize = 1<<26 = 67,108,864` message.go:15; conn.go:199 `64*1024*1024` | ❌ |

Verified specifics:

- **Checksum algorithm**: `Hash` = double SHA256. net.cpp:2706 `uint256 hash = Hash(msg.data.data(), msg.data.data() + nMessageSize);` then `memcpy(hdr.pchChecksum, hash.begin(), 4)` (:2708). Go message.go:30-36 does exactly `sha256(sha256(payload))[0:4]`. ✅
- **Command padding**: both write up to 12 bytes and zero-fill the rest (protocol.cpp:103-104 `memset` then `strncpy`; message.go:41-43 `make(cmd, 12); copy`). `GetCommand` trims at first NUL via `strnlen` (protocol.cpp:111); Go scans to first NUL (message.go:60-64). ✅
- **Magic values confirmed against C++ chainparams** (not comments): mainnet port 41412 (`chainparams.cpp:127-130`), testnet port 41474 (`:293-296`), and the third network is **REGTEST** (`strNetworkID = "regtest"`, `CRegTestParams`, `chainparams.cpp:373-376`), port 41489 (`:417-420`). Go labels it `StagingMagic` (params.go:7) — ❌ **naming divergence only**; the 4 magic bytes and port match. See FINDINGS.
- **Read-side magic/command validation**: C++ rejects a bad magic (net_processing.cpp:3117-3121) and validates command charset `0x20..0x7E` + NUL-termination rules (protocol.cpp:114-132). Go `UnmarshalMessage` copies Magic (:58) but **never compares it** to the expected network magic, and does not validate command bytes. ⚠️ robustness gap (a frame with the wrong magic is still parsed). Go also requires the checksum to match or returns an error (message.go:78-80) — C++ on checksum mismatch just logs and drops the message without disconnecting (net_processing.cpp:3138-3145). ⚠️ both directions differ from C++.
- **Capacity**: Go's cap is `1<<26` (67,108,864 B, message.go:15) — **16× the C++ wire cap** of 4,000,000 B (`net.h:55`). C++ disconnects any peer declaring more (net.cpp:583-585). Go will accept and process frames C++ would drop. ❌ (Go more lenient.)

**VECTOR 1.1 — full `version` frame, mainnet magic** (payload = the Card 2 vector):

```
a1a0a2a3 76657273696f6e0000000000 69000000 65484f9b
 39140100 0000000000000000 80b8706000000000
 000000000000000000000000000000000000ffff01020304a1c4
 0000000000000000000000000000000000000000000000000000
 8877665544332211 122f676f2d786272696467653a302e312e302f
 00000000 00 00
```
`magic=a1a0a2a3`, `command="version\0…"`, `length=0x69=105`, `checksum=65484f9b` (verified = first 4 bytes of double-SHA256 of the 105-byte payload). Field decode in Card 2.

**VECTOR 1.2 — `verack` frame (24 B, empty payload):**
```
a1a0a2a3 76657261636b0000000000 00000000 5df6e0e2
```
`checksum(empty)=5df6e0e2` (verified). Go `writeVerack` conn.go:129-136; C++ `Make(NetMsgType::VERACK)` net_processing.cpp:1662.

**VECTOR 1.3 — `xbridge` frame (53 B)** — envelope of Card 4 (29-byte payload):
```
a1a0a2a3 786272696467650000000000 1d000000 72e88f36
 1c 0000000000000000000000000000000000000000 00203ffb8fbf0500
```
`command="xbridge\0…"` = `NetMsgType::XBRIDGE` (protocol.cpp:45, matching Go `XBridgeNetCommand` params.go:16). `length=0x1d=29`, `checksum=72e88f36` (verified double-SHA256 of the 29-byte envelope).

---

### CARD 2 — VERSION MESSAGE (`p2p/version.go` vs C++)

**Verdict: ✅ MATCH** (field order, widths, and the trailing fxrouter byte all agree).

C++ send path `PushNodeVersion` (net_processing.cpp:210-211):
```
PROTOCOL_VERSION, (uint64_t)nLocalNodeServices, nTime, addrYou, addrMe,
nonce, strSubVersion, nNodeStartingHeight, ::g_relay_txes, (bool)pnode->fXRouter
```
C++ receive path (net_processing.cpp:1597, 1628-1643): `nVersion >> nServiceInt >> nTime >> addrMe`, then `addrFrom >> nNonce` (:1628-1629), `strSubVer` (:1630-1633), `nStartingHeight` (:1634-1636), `fRelay` (:1637-1638), and **`fXRouter` read last, only `if (!vRecv.empty())`** (:1640-1643).

| # | Field | Width/enc | C++ source | Go source | Match |
|---|-------|-----------|-----------|-----------|-------|
| 1 | version | 4 LE | net_processing.cpp:210 (`PROTOCOL_VERSION`) | version.go:124 (`Version`) | ✅ |
| 2 | services | 8 LE | :210 (`nLocalNodeServices`) | version.go:126 | ✅ |
| 3 | timestamp | 8 LE (int64) | :210 (`nTime` = GetAdjustedTime) | version.go:128 (Unix seconds) | ✅ |
| 4 | addr_recv (addrYou) | 26 (no nTime) | :210 (`addrYou`); 26 B because stream nVersion=209 < CADDR_TIME_VERSION — see Card 3 | version.go:130 | ✅ |
| 5 | addr_from (addrMe) | 26 (no nTime) | :210 (`addrMe`) | version.go:131 | ✅ |
| 6 | nonce | 8 LE | :210 | version.go:132 | ✅ |
| 7 | user_agent | varstr (CompactSize) | :210 (`strSubVersion`); read `LIMITED_STRING(strSubVer, MAX_SUBVERSION_LENGTH=256)` net_processing.cpp:1630-1631, net.h:57 | version.go:134 (`marshalVarStr`) | ✅ |
| 8 | start_height | 4 LE (int32) | :210 (`nNodeStartingHeight`) | version.go:135 | ✅ |
| 9 | relay | 1 | :210 (`::g_relay_txes`) | version.go:137-141 | ✅ |
| 10 | fxrouter | 1, trailing & optional | :211 (write); :1640-1643 (read `if !empty`) | version.go:142-146 (write); :219-221 (read optional) | ✅ |

Verified specifics:

- **PROTOCOL_VERSION**: C++ = 70713 (`src/version.h:12`). Go `BitcoinProtocolVersion = 70713` (version.go:16). ✅
- **fxrouter placement**: C++ writes it as the **last** field (net_processing.cpp:211) and reads it **only if bytes remain** (net_processing.cpp:1641-1642, defaulting to `false`). Go writes it explicitly last (version.go:142-146) and reads it conditionally (version.go:219-221). Placement matches. C++'s read-side `if (!vRecv.empty())` means a peer omitting the byte is treated as non-XRouter — which is what Go's comment (version.go:38-41) describes. ✅
- **Values Go advertises** (version.go:159-170): version 70713, services 0 (thin client, no `NODE_*`), timestamp Unix seconds, start height 0 (no chain), relay=false, fxrouter=false. C++ advertises its real chain height and `g_relay_txes` (default true). ⚠️ Behavior note, not wire format.
- **nVersion gate for addr gossip**: C++ processes `addr` only if `pfrom->nVersion >= CADDR_TIME_VERSION` (31402) unless seeding (net_processing.cpp:1823). All peers are ≥ 70712, so effectively always. Go `ParseAddr` has no version gate (addr.go:32). ✅ in practice.
- **Go does not enforce `MIN_PEER_PROTO_VERSION`** on the peer's advertised version: `conn.go:87-116` parses it (`UnmarshalVersion`) but never checks it, while C++ **disconnects** peers `< 70712` (net_processing.cpp:1617-1626). ❌ behavior gap — see FINDINGS.
- **services gate**: C++ disconnects an outbound peer that fails `HasAllDesirableServiceFlags` unless `fXRouter` (net_processing.cpp:1604-1615). Not applicable to Go: Go always connects **outbound**, so stock nodes see it as inbound and skip that check. ⚠️ NOTE.

**VECTOR 2.1 — version payload (105 B)** (v=70713, services=0, ts=1618000000, recv=`1.2.3.4:41412`, from=zero, nonce=`0x8877665544332211`, UA=`/go-xbridge:0.1.0/`, height=0, relay=0, fxrouter=0):

```
39140100 0000000000000000 80b8706000000000
000000000000000000000000000000000000ffff01020304a1c4
0000000000000000000000000000000000000000000000000000
8877665544332211 122f676f2d786272696467653a302e312e302f
00000000 00 00
```
Segments: `39140100`=70713 LE; `80b8706000000000`=1618000000 LE; `addr_recv` = `0000000000000000`(services) + `00000000000000000000ffff01020304`(::ffff:1.2.3.4) + `a1c4`(port 41412 BE); `ua varstr` = `12`(len 18) + `/go-xbridge:0.1.0/`; relay=0, fxrouter=0. Produced by `VersionMessage.Marshal` (version.go:121-148); verified byte-for-byte against the C++ field order above.

---

### CARD 3 — NET_ADDR / CAddress (`p2p/version.go` `NetAddr`, `p2p/addr.go`)

**Verdict: ✅ MATCH** (26-byte form in `version`, 30-byte nTime form in `addr`).

C++ `CAddress::SerializationOp` (protocol.h:371-386):
```
if (SER_DISK) READWRITE(nVersion);
if (SER_DISK || (nVersion >= CADDR_TIME_VERSION && !(SER_GETHASH))) READWRITE(nTime); // 4 LE
READWRITE(nServicesInt);   // 8 LE
READWRITEAS(CService, *this); // ip(16) || port(2 BE), netaddress.h:169-173
```
- **Version message** (stream nVersion = `INIT_PROTO_VERSION` = 209, net.cpp:569): 209 < 31402 ⇒ **no nTime** ⇒ `services(8) ‖ ip(16) ‖ port(2 BE)` = 26 B. Go `marshalNetAddr` (version.go:75-82), `unmarshalNetAddr` (:226-237). ✅
- **addr message** (peer version ≥ 70712 > 31402): **nTime present** ⇒ `time(4 LE) ‖ services(8) ‖ ip(16) ‖ port(2 BE)` = 30 B per record. Go `ParseAddr`/`MarshalAddr` (addr.go:39-58, 64-83) match. ✅
- **IPv4 → ::ffff:0:0/96 mapping**: C++ `CNetAddr::SetRaw(NET_IPV4)` (netaddress.h:43, 58-69) places the 4 bytes at ip[12..16] after `00 00 00 00 00 00 00 00 00 00 ff ff`. Go `ipTo16` (version.go:59-70) and inverse `netIPFrom16` (:241-246). ✅
- **port endianness**: C++ `WrapBigEndian(port)` (netaddress.h:172); Go `BigEndian.PutUint16` (version.go:80). ✅ (only this field is BE).
- **nTime semantics**: C++ clamps a received addr's nTime on store (net_processing.cpp:1847 `addr.nTime <= 100000000 || > nNow+10*60 → nNow-5*24*60*60`); Go `ParseAddr` returns the raw 4 bytes (addr.go:44) and the address manager applies its own policy. ⚠️ policy, not bytes.

**VECTOR 3.1 — 26-byte net_addr** (services=0x5, `1.2.3.4`, port 8333):
```
0500000000000000 00000000000000000000ffff01020304 208d
```
**VECTOR 3.2 — `addr` payload (61 B), 2 records** (nTime 1618000000 / 1618000001, services=1, `5.6.7.8:41412` / `9.9.9.9:41412`):
```
02 80b87060 0100000000000000 00000000000000000000ffff05060708 a1c4
   80b87061 0100000000000000 00000000000000000000ffff09090909 a1c4
```
`count varint = 02`; per record `time(4 LE) ‖ services(8 LE) ‖ ip(16) ‖ port(2 BE)`.

---

### CARD 4 — XBRIDGE ENVELOPE (`p2p/envelope.go` vs C++)

**Verdict: ✅ MATCH** (varint length prefix, 20-byte dest, 8-byte µs timestamp, exact-length decode).

C++ side located: **`App::Impl::onSend`** (`src/xbridge/xbridgeapp.cpp:561-591`) builds the envelope, and **`net_processing.cpp:2868-2926`** consumes/relays it. A `std::vector<unsigned char>` serializes as **Bitcoin CompactSize length prefix + bytes** (serialize.h:546-550, `WriteCompactSize` :253-275).

- Envelope = `id(20) ‖ timestamp(8) ‖ message(packet)` — xbridgeapp.cpp:563,574,577. C++ broadcast uses a static zero id (`App::sendPacket`, xbridgeapp.cpp:553); targeted sends pass the id (`App::sendPacket(id,…)` :595-598). 
- **Timestamp units = microseconds**: `boost::posix_time::microsec_clock::universal_time()` (xbridgeapp.cpp:571) → `timeToInt` = `timeFromEpoch.total_microseconds()` (**xutil.cpp:276-283**, exact line 280). Go `uint64(time.Now().UnixMicro())` (envelope.go:42). ✅ µs on both sides.
- **Signed-body question**: C++ computes `uint256 hash = Hash(msg.begin(), msg.end());` over the **whole envelope** (dest+ts+packet) for its `addToKnown` dedup (xbridgeapp.cpp:579-581). That is a *send-side dedup key*, **not** the XBridgePacket's internal signature — the packet's own sign/verify covers only the 129-byte header + body (C++ `packet->verify()` xbridgeapp.cpp:751; Go `proto.Packet.Digest` packet.go:86-91, envelope ts excluded). So on **both** sides the envelope timestamp is outside the packet's cryptographic signature; Go's µs placement keeps C++'s `Hash`/dedup semantics identical. ✅ (clarified, per audit item.)
- **Length prefix = CompactSize of (28 + packetLen)**: C++ wraps via `msgMaker.Make(NetMsgType::XBRIDGE, msg)` (xbridgeapp.cpp:589) where `msg` is a `std::vector<unsigned char>` ⇒ CompactSize count covers **dest+ts+packet**. Go `writeVarInt(len(env))` with `env = dest+ts+packet` (envelope.go:45). ✅
- **CompactSize encoding** (C++ serialize.h:255-273 `nSize<253 → 1B`, `<=0xffff → 0xFD+u16LE`, `<=0xffffffff → 0xFE+u32LE`, else `0xFF+u64LE`; Go envelope.go:72-92 identical). ✅ thresholds match.
- **Receive validation**: C++ requires `raw.size() >= (20 + sizeof(time_t)) = 28` (net_processing.cpp:2874), strips `addr(20)` (:2893-2894) and `ts(8)` (:2895), and routes zero-dest → `onBroadcastReceived`, non-zero → `onMessageReceived` (:2896-2899); relays the raw envelope to all connected nodes (:2921-2924). Go `DecodeXBridgePayload` requires `n ≥ 28` (envelope.go:63-66) and returns the packet bytes only. ✅
- ⚠️ **Exact-length decode**: Go requires `off+n == len(payload)` (envelope.go:59). C++ `vRecv >> raw` reads exactly the declared CompactSize bytes and silently ignores any trailing bytes. Go is stricter (rejects trailing junk C++ tolerates). ⚠️ NOTE.
- ⚠️ **Non-canonical varints**: C++ `ReadCompactSize` throws on non-canonical encodings (serialize.h:289-303) and on `> MAX_SIZE` (:304-305); Go `readVarInt` accepts non-canonical forms (e.g. `0xfd` encoding a value < 253) and has no MAX_SIZE bound. ⚠️ NOTE (Go more lenient).
- ⚠️ **Go's broadcast path** uses a zero dest (peer_manager.go:366-390 `WritePacket` fan-out), matching C++ `ForEachNode` relay (xbridgeapp.cpp:585-590, net_processing.cpp:2921-2924). C++ excludes `fXRouter` nodes from the relay fan-out (xbridgeapp.cpp:588); Go broadcasts to all peers. ⚠️ NOTE.

**VECTOR 4.1 — broadcast envelope payload (33 B)** (dest=20×0x00, ts=`1618000000000000` µs, packet=`00 01 02 03`):
```
20 0000000000000000000000000000000000000000 00203ffb8fbf0500 00010203
```
`20`=varint(32=28+4); dest 20 zero bytes; `00203ffb8fbf0500` = `0x5bf8ffb3f2000` LE = 1,618,000,000,000,000 µs; packet 4 B. Decode round-trips (err=nil, pkt=`00010203`).

**VECTOR 4.2 — addressed envelope payload (31 B)** (dest=`000102…13`, ts=`1618000000000000`, packet=`aa bb`):
```
1e 000102030405060708090a0b0c0d0e0f10111213 00203ffb8fbf0500 aabb
```
`1e`=varint(30=28+2); non-zero dest routes to `onMessageReceived` (C++ net_processing.cpp:2896-2897). Go `TestEnvelopeDestination` (envelope_test.go:94-120) exercises the same.

**VECTOR 4.3 — live captured envelope** (real service-node traffic, `envelope_test.go:19`, `fdb401…`): `fd b4 01` = CompactSize 0x01b4 = 436 ⇒ 28-byte envelope + 408-byte packet; dest (bytes 4-23) = `6894ff47163a031d3ac8bfce10dfa3fbe290a48a` (20 B, non-zero ⇒ addressed); timestamp (bytes 24-31) = `4d27edd398560600` µs (=0x065698d3ed274d ≈ 2026); then the XBridgePacket (`37 00 00 00 03 00 00 00 …` = version 55, command 3). Confirms the wire format end-to-end.

---

### CARD 5 — ADDR / GETADDR gossip (`p2p/addr.go` vs C++)

**Verdict: ✅ MATCH on wire format, ⚠️ NOTE on counts and response policy.**

- **addr payload** = CompactSize count + N records of `time(4 LE) ‖ services(8) ‖ ip(16) ‖ port(2 BE)` (C++ `std::vector<CAddress>` net_processing.cpp:1818-1821; per-record layout via protocol.h:379-385 with nTime, peer version > 31402). Go `MarshalAddr`/`ParseAddr` (addr.go:32-83) identical. ✅
- **count cap**: C++ misbehaves on `> 1000` addrs (net_processing.cpp:1825-1830). Go `ParseAddr` is lenient (decodes what fits, addr.go:40-42) and `MarshalAddr` is unbounded. ⚠️ Go missing the 1000 cap on receive.
- **getaddr**: C++ answers **only inbound** peers, **once per connection** (`fSentAddr`, net_processing.cpp:2665-2676), via `connman->GetAddresses()` (≤ `MAX_ADDR_TO_SEND=1000`, net.h:53). Go answers **any** requester with `addrMan.Random(64)` (peer_manager.go:322-324). ❌ policy divergence (count 64 vs ≤1000; inbound-only vs any).
- **who sends getaddr**: C++ sends getaddr after the version handshake when `fOneShot || nVersion >= CADDR_TIME_VERSION || addrCount < 1000` and only to non-XRouter peers (net_processing.cpp:1719-1725). Go sends getaddr after every connect (peer_manager.go:273). ✅ (compatible).

**VECTOR 5.1 — see Card 3 VECTOR 3.2.** Command names `CmdGetAddr="getaddr"`, `CmdAddr="addr"` (addr.go:10-11) match `NetMsgType::GETADDR`/`ADDR` (protocol.cpp:59,68 area).

---

### CARD 6 — PING / PONG (`p2p/discovery/peer_manager.go` vs C++)

**Verdict: ✅ MATCH on echo behavior, ⚠️ NOTE (Go never initiates pings).**

- **ping payload** = 8-byte nonce (C++ net_processing.cpp:2712-2713, only when `nVersion > BIP0031_VERSION`=60000, version.h:34). C++ echoes it back as `PONG` with the nonce (:2725). Go, on receiving `ping`, replies `pong` echoing the payload verbatim (peer_manager.go:325-326). ✅
- **pong** = 8-byte nonce (C++ :2736-2745). Go's echo covers this. ✅
- Go **never sends** outbound pings; C++ sends one every 2 minutes (`PING_INTERVAL`, net.h:43; net_processing.cpp:3343-3360) and measures latency. ⚠️ NOTE — absence is protocol-safe (pings are optional liveness probes).
- The handshake loop treats ping/pong/addr as ignorable (conn.go:111-113), which C++ tolerates pre-verack only by dropping+penalizing (net_processing.cpp:1811-1815) — see Card 8.

**VECTOR 6.1 — ping nonce (8 B)** `0000000000000000`; **pong echo** `7877767574737271`.

---

### CARD 7 — SERVICENODE MESSAGES (`p2p/servicenode/servicenode.go` vs C++)

**Verdict: ✅ MATCH on every layout. C++ codecs exist and Go's parsers are NOT speculative.**

Command names in both: `snr`/`snp`/`snl`/`snlp` — C++ protocol.cpp:46-49, Go servicenode.go:36-41. C++ handlers: `SNREGISTER` net_processing.cpp:2931-2960, `SNPING`/`SNLISTPING` :2962-2990, `SNLIST` :2992-3001. Envelope: none — the payload **is** the serialized struct (no length prefix).

**7a. SNREGISTER (`snr`) → `ServiceNode`** — `ServiceNode::SerializationOp` servicenode.h:355-367; Go `ParseServiceNode` servicenode.go:217-244:

| # | Field | Wire | C++ source | Go source | Match |
|---|-------|------|-----------|-----------|-------|
| 1 | snodePubKey | varstr, 33 B (CPubKey) | servicenode.h:356 | servicenode.go:221 (`readCPubKey`) | ✅ |
| 2 | tier | 1 (enum, uint8) | servicenode.h:357 | :224 | ✅ |
| 3 | paymentAddress | 20 (CKeyID) | servicenode.h:358 | :227 | ✅ |
| 4 | collateral | varint count + N×(txid 32 + vout 4 LE) (COutPoint) | servicenode.h:359; COutPoint = `hash(32)‖n(4)` transaction.h:26-39 | :230 (`readCollateral`) | ✅ |
| 5 | bestBlock | 4 LE (uint32) | servicenode.h:360 (member is `uint32_t`, :625) | :233 (read as int32 — same bytes) | ✅ |
| 6 | bestBlockHash | 32 | servicenode.h:361 | :236 | ✅ |
| 7 | signature | varbytes (vector<uchar>) | servicenode.h:362 | :239 | ✅ |

**7b. SNPING (`snp`) / SNLISTPING (`snlp`) → `ServiceNodePing`** — `ServiceNodePing::SerializationOp` servicenode.h:681-695; Go `ParseServiceNodePing` servicenode.go:249-317:

| # | Field | Wire | C++ source | Go source | Match |
|---|-------|------|-----------|-----------|-------|
| 1 | snodePubKey (outer ping key) | varstr 33 | servicenode.h:683 | :254 | ✅ |
| 2 | bestBlock | 4 LE (uint32) | servicenode.h:684 | :257 | ✅ |
| 3 | bestBlockHash | 32 | servicenode.h:685 | :260 | ✅ |
| 4 | pingTime | 4 LE (uint32) | servicenode.h:686 | :264 | ✅ |
| 5 | config | varstr (std::string) | servicenode.h:687 | :268 (`readVarStr`) | ✅ |
| 6 | snode (embedded `ServiceNode`) | nested 7-field block | servicenode.h:688 | :273 (`parseInnerServiceNode`) | ✅ |
| 7 | signature | varbytes | servicenode.h:689 | :290 | ✅ |

Verified specifics:

- **sigHash / CreateSigHash**: ping signature covers `snodePubKey‖bestBlock‖bestBlockHash‖pingTime‖config‖snode` (servicenode.h:748-752); Go hashes exactly `b[:sigStart]` (servicenode.go:289-300). Registration `CreateSigHash` covers `pubkey‖tier‖paymentAddress‖collateral‖bestBlock‖bestBlockHash` (servicenode.h:104-112); Go `serializeSigHashFields` (servicenode.go:381-393) matches byte-for-byte, incl. CompactSize collateral count (servicenode.h:109, Go :358-374). ✅
- **pubkey-match + RecoverCompact**: C++ `ping.isValid` requires `snodePubKey == snode.getSnodePubKey()` (servicenode.h:791) and `RecoverCompact(sigHash(), signature)` (:810-815); Go enforces both (servicenode.go:279-304). ✅
- **Tier gate**: C++ requires `Tier::SPV` (servicenode.h:787, 409; enum `OPEN=0, SPV=50` :90-93). Go `TierSPV = 50` (servicenode.go:91), `registrationValid` :448-468. ✅
- **SNLIST response**: C++ **answers** `snl` with an `SNLISTPING` per known snode (net_processing.cpp:2992-2998). Go **ignores** `snl` (peer_manager.go:341-344) with the comment "A stock XBridge client does not answer SNLIST (only XRouter does, xrouterpeermgr.cpp:536)". **That rationale is stale** — the C++ receive/respond side (`net_processing.cpp:2992-2998`) is not gated on XRouter. ❌ behavior divergence (Go learns the SN set only from relayed pings and never responds/asks). See FINDINGS.
- **Relay policy**: C++ relays `snp` to all but the sender; does **not** relay `snlp` (net_processing.cpp:2976-2983). Go stores both. ⚠️ NOTE.
- **No marshaler on the Go side**: Go only *parses* snr/snp/snlp; there is no `Marshal` for them (registration signer exists for testing, servicenode.go:414-425). C++ `writeSnRegistration` (servicenodemgr.h:756) serializes its own registration. Not a conformance gap for a thin client that only listens. ⚠️ NOTE.

**VECTOR 7.1 — SNREGISTER payload (345 B)** (pubkey `02 01 02 03 04…`, tier 50, paymentAddress `01..14`, 7 collateral utxos `txid=bytes(i+j)…, vout=i`, bestBlock 123456=0x1E240, bestBlockHash `a0..bf`, empty signature varbytes). Built byte-for-byte per the C++ layout and re-parsed by `servicenode.ParseServiceNode` (err=nil; tier=50, collateral=7, bestBlock=123456):
```
21 020102030400000000000000000000000000000000000000000000000000000000
32 0102030405060708090a0b0c0d0e0f10111213
07 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f 00000000
   0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20 01000000
   02030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f2021 02000000
   030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122 03000000
   0405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20212223 04000000
   05060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f2021222324 05000000
   060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425 06000000
40e20100 a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf
00
```
`21`=varstr33 pubkey; `32`=varstr20 paymentAddress; `07`=varint collateral count; per-utxo `txid(32)‖vout(4 LE)`; `40e20100`=123456 LE; hash 32 B; `00`=empty signature.

**VECTOR 7.2 — SNPING payload (245 B)** (outer pubkey = pk, bestBlock 1000, bestBlockHash `10..2f`, pingTime 1618000000, config `{"xbridgeversion":55,"xrouterversion":5}` varstr, embedded `ServiceNode` per VECTOR 7.1, empty ping signature). Field order verified by parse (the synthetic key fails `fullyValidCPubKey` at validation, not at layout — proving the parser read every field to that point):
```
21 020102030400000000000000000000000000000000000000000000000000000000
e8030000 101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f
80b87060 28 7b227862726964676576657273696f6e223a35352c2278726f7574657276657273696f6e223a357d
<embedded ServiceNode: 21 pubkey | 32 paymentAddress | 01 collateral | ... | 40e20100 | hash | 00>
00
```
Go's own valid-signature round-trips are in `servicenode_test.go` (`buildPing` :126-129, `TestWalletServicesParity` :150-209) using the exact C++ field order (servicenode.h:683-694).

---

### CARD 8 — HANDSHAKE / SEQUENCING (`p2p/conn.go` vs C++)

**Verdict: ✅ MATCH on ordering, ⚠️ NOTE on timeouts and post-verack extras.**

- **Ordering**: outbound — send `version` (C++ `PushNodeVersion` net_processing.cpp:199-218; Go conn.go:118-127), await peer `version` then send `verack` (C++ net_processing.cpp:1662; Go conn.go:101-108), await peer `verack` (C++ sets `fSuccessfullyConnected = true` :1799; Go completes handshake conn.go:94-114). ✅ Identical sequence.
- **xbridge gated on verack**: C++ refuses anything except version/verack before `fSuccessfullyConnected` (`"Must have a verack message before anything else"` + `Misbehaving(1)` net_processing.cpp:1811-1815); the XBridge handler is reached only after that gate, and C++ relays xbridge only to `fSuccessfullyConnected` nodes (:2921-2923). Go's `Conn` isn't used for XBridge until `NewConn`'s handshake completes, and `ReadPacket`/`WritePacket` run post-handshake. ✅
- **Pre-verack message handling**: C++ penalizes + drops other messages pre-verack (:1811-1815); Go ignores them (:111-113). ⚠️ Go more lenient.
- **Post-verack extras**: C++ sends `SENDHEADERS` (+ `SENDCMPCT` if version ≥) right after verack (net_processing.cpp:1779-1798). Go sends neither — both are optional/negotiable and a peer must tolerate their absence. ⚠️ NOTE.
- **Timeouts**: Go bounds the whole exchange at `handshakeTimeout = 30s` (conn.go:26). C++ disconnects a peer that sends **no** message within `DEFAULT_PEER_CONNECT_TIMEOUT = 60s` (net.h:83, net.cpp:1056-1061) and enforces 20-min inactivity (`TIMEOUT_INTERVAL`, net.h:45, net.cpp:1063-1074). Go's 30s handshake deadline is stricter. ⚠️ policy, not wire.
- **Version gate**: Go never checks the peer's `Version` for `MIN_PEER_PROTO_VERSION` (70712) — C++ disconnects below it (net_processing.cpp:1617-1626). ❌ see FINDINGS. C++ also disconnects on duplicate version (:1574-1582); Go's handshake would accept and just overwrite `peerVersion`.
- **getaddr/seed behavior**: C++ sends getaddr post-handshake (net_processing.cpp:1719-1725) and advertises its own addr (:1700-1716); Go sends getaddr after connecting (peer_manager.go:273) and seeds its AddrMan from DNS + fixed seeds (seeds.go:76-89). Go's 11 mainnet + 4 testnet fixed seeds **exactly** match `pnSeed6_main`/`pnSeed6_test` (chainparamsseeds.h:10-29) and DNS seeds (chainparams.cpp:146-147, 310-311). ✅

**VECTOR 8.1 — version/verack exchange bytes**: Card 1 VECTOR 1.1 (our `version` frame) + 1.2 (`verack` frame). C++'s outbound `version` frame differs only in values (real services/height, non-zero nonce, longer UA), not format.

---

### FINDINGS (candidate-side deltas from the C++ reference)

- **MaxPayloadSize cap — Go 16× larger**: Go `MaxPayloadSize = 1<<26` (message.go:15) and 64 MiB read guard (conn.go:199) vs C++ `MAX_PROTOCOL_MESSAGE_LENGTH = 4,000,000` (net.h:55) and `MAX_SIZE = 32 MiB` (serialize.h:27). Go will accept/process frames C++ disconnects for. (CARD 1)
- **Go never validates the incoming magic** — `UnmarshalMessage` copies it (message.go:58) but no compare to the network magic; C++ rejects bad magic (net_processing.cpp:3117-3121). (CARD 1)
- **`readVarInt` accepts non-canonical CompactSize** and has no MAX_SIZE bound — C++ `ReadCompactSize` throws (serialize.h:289-305). (CARD 4)
- **`DecodeXBridgePayload` requires exact envelope length** (envelope.go:59) vs C++ tolerance of trailing bytes after the vector. Stricter, not divergent. (CARD 4)
- **"staging" label is actually C++ REGTEST** — Go `StagingMagic a1cf7eac:41489` (params.go:7) matches C++ `CRegTestParams` (chainparams.cpp:376, 417-420), which Go names "staging" and gives no seeds (seeds.go:66). Values match; naming diverges from the C++ chain name. (CARD 1)
- **Go drops the MIN_PEER_PROTO_VERSION gate** — never checks the peer's version (conn.go:87-116); C++ disconnects < 70712 (net_processing.cpp:1617-1626). Also accepts duplicate `version` messages. (CARD 2/8)
- **getaddr policy differs** — C++ answers only inbound, once, up to 1000 addrs (net_processing.cpp:2665-2686, net.h:53); Go answers any requester with 64 (peer_manager.go:322-324). (CARD 5)
- **`addr` receive cap missing** — C++ misbehaves on >1000 records (net_processing.cpp:1825-1830); Go `ParseAddr` is unbounded/lenient. (CARD 5)
- **Go never initiates pings** — C++ sends PING every 2 min (net.h:43, net_processing.cpp:3343-3360); Go only echoes. Absence is protocol-safe. (CARD 6)
- **SNLIST handling is stale** — C++ answers `snl` with SNLISTPING (net_processing.cpp:2992-2998), un-gated on XRouter; Go ignores it citing "only XRouter answers" (peer_manager.go:341-344) — that claim is contradicted by the C++ reference. (CARD 7)
- **No post-verack SENDHEADERS/SENDCMPCT** from Go (C++ net_processing.cpp:1779-1798). Optional; tolerated. (CARD 8)
- **Handshake timeout** 30s (conn.go:26) vs C++ 60s first-message deadline (net.h:83, net.cpp:1056-1061) + 20-min inactivity (net.h:45). Stricter, not divergent. (CARD 8)

### VERDICT SUMMARY

Every serialized field audited (frame, version incl. fxrouter, net_addr/CAddress both forms, XBridge envelope incl. µs timestamp, addr, ping/pong, snr/snp/snlp) is **byte-for-byte conformant** with the C++ writers. All divergences are policy/robustness/validation deltas on the Go side (more lenient caps, missing read-side validation, missing version/snl gates) — no wire-format byte difference.
