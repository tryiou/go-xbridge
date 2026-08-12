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
