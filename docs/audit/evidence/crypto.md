# CRYPTO / FEES / UTXO Conformance Audit — go-xbridge vs Blocknet Core xBridge (C++)

Date: 2026-08-12
Scope: packet-signing primitives, address decoding, HTLC construction, tx signing/sighash,
UTXO selection, fee calculation, locktime, order-id hashing.
Ground truth: C++ source in `blocknet_core/src/xbridge/` (trust code, not comments).
Candidate: `go-xbridge/`.

Every claim cites `file:line` on both sides. Divergences are marked **DIFF**.

---

## Card 1 — PACKET SIGNING PRIMITIVES — **CONFORMANT (vector-verified)**

### C++ (reference)
- `XBridgePacket::sign` (xbridgepacket.cpp:59-89):
  - pubkey 33 bytes / privkey 32 bytes enforced (:62).
  - copies pubkey into header, zeroes the 64-byte signature field (:68-69).
  - digest = `CSHA256` over the **entire `m_body`** (header+body) with the sig field zeroed (:71-77).
  - `secp256k1_ecdsa_sign(secpContext, &sig, hash, &privkey[0], 0, 0)` (:80) — nonce function `0`
    (NULL) ⇒ libsecp256k1 default RFC6979, no extra entropy.
  - serialized with `secp256k1_ecdsa_signature_serialize_compact` ⇒ **64-byte r‖s** (:85).
- `XBridgePacket::verify` (xbridgepacket.cpp:94-149):
  - re-zeroes the sig field, hashes the whole body (:96-106), restores the sig (:109).
  - `ecdsa_signature_parse_compact` (:112), `ec_pubkey_parse` for the 33-byte compressed key (:119),
    `secp256k1_ecdsa_verify` (:125), then re-serializes the key compressed and `memcmp`s it against
    the header key (:132-145). High-S signatures are accepted (no normalize here).
- `verify(pubkey)` variant checks the header key against an explicit key first (xbridgepacket.cpp:154-162).
- `BtcCryptoProvider::sign` (xbridgecryptoproviderbtc.cpp:234-252) returns **DER** (72-byte buffer,
  `serialize_der`, :249-250) — this is the tx-signature path (refund/payment), not the packet path.
- `BtcCryptoProvider::verify` (xbridgecryptoproviderbtc.cpp:256-278) parses lax DER, **normalizes**
  high-S (:276), then verifies — used for signature checks elsewhere, not for packet verify.
- Key generation: `makeNewKey` = `GetStrongRandBytes` + `secp256k1_ec_seckey_verify` retry
  (xbridgecryptoproviderbtc.cpp:204-211).

### Go (candidate)
- `crypto/signer.go`:
  - `Sign` (:78-90): derives pubkey from priv via `btcec.PrivKeyFromBytes`, writes the 33-byte
    compressed key, computes `p.Digest()`, signs with `btcec/v2 ecdsa.Sign` (RFC6979), converts to
    compact via `compactSerialize` (:49-59: 32-byte big-endian R ‖ 32-byte big-endian S), writes the
    64-byte sig.
  - `Verify` (:95-108) / `VerifyAgainst` (:116-133): parse compact (:62-74), parse 33-byte pubkey,
    `sig.Verify(d, pub)`. Accepts both high-S and low-S (matches C++ packet verify).
- `proto/packet.go`:
  - Header layout: 8×u32 + 33-byte pubkey + 64-byte sig = 129 bytes; pubkey @20, sig @53
    (:12-44). Matches C++ `xbridgepacket.h:309,554-557`.
  - `Marshal` (:73-84): LE u32 fields, then pubkey, sig, body.
  - `Digest` (:88-93): SHA256 over `Marshal()` with the sig region zeroed. **This is the exact C++
    construction** (header+body, sig zeroed).
- `NewPrivateKey` (crypto/signer.go:138-147) clears the top bit (`b[0] &= 0x7f`) instead of
  C++'s full 32-byte random + retry. **MINOR** distribution bias, still a valid scalar.

### VECTOR (fixed key, cross-implementation)
Scratch module (`/tmp/opencode/scratch`, replace → go-xbridge) signed a fully deterministic packet
(body `"XBRIDGE_PACKET_SIGNING_VECTOR"`, cmd `XbcTransaction`, timestamp 1600000000, key
`00…01`):

```
digest    fafbf7fe9a58ba56cef3d81371af05449b08478d7946987d5790b5dc8646bb2a
pubkey    0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798
signature 63dfe856d466e7579c3a32b9b4987a5c11f52d1848f4634b1f4822bfb59f2959
          7c0cf7d2913fb5882240c4ff8d4229975398ea5d8db885d64514cb1f094a0c33
```

Python `coincurve` (direct libsecp256k1 bindings = the C++ library) signing the same digest with the
same key and `hasher=None` (⇒ `secp256k1_ecdsa_sign` default RFC6979) produced **byte-identical**
DER and the identical 64-byte compact signature, and the identical compressed pubkey. ⇒
btcd/btcec RFC6979 ≡ libsecp256k1 RFC6979 for this key+digest; the C++ and Go packet signatures are
byte-equivalent given the same digest.

No **DIFF** in digest construction or encoding. Note: C++ `BtcCryptoProvider::sign` is DER and is
only used for tx signatures; Go mirrors that split (DER in `coins.signDigest`, compact for packets).

---

## Card 2 — ADDRESS DECODING — **CONFORMANT, with error-behavior DIFFs**

### base58check split
- C++ `CBase58Data::SetString` (xbitcoinaddress.cpp:36-51): `DecodeBase58Check` then splits the
  first `nVersionBytes` (default 1) as version, the rest as data. `XBitcoinAddress::IsValid`
  requires 20-byte data (xbitcoinaddress.h:72-76); `Get()` returns the `CKeyID` (:102-112).
- Go `base58CheckDecode` (coins/base58check.go:23-39): `prefix = raw[0]`, `payload = raw[1:len-4]`,
  checksum = first 4 bytes of double-SHA256 (verified, :41-47). Base58 codec (coins/base58.go:23-67)
  matches Bitcoin's big-integer method incl. leading '1' handling.
- Checksum scheme identical (SHA256d first-4) on both sides. **CONFORMANT.**
- Version byte: C++ takes the first byte without consulting any coin config; Go validates the
  prefix against `c.P2PKH` / `c.P2SH` (coins/address.go:107-115) and rejects a non-matching byte.
  - **DIFF (error behavior)**: C++ `toXAddr` (xbridgewalletconnectorbtc.cpp:1547-1555) and
    `hasValidAddressPrefix` (xbridgewalletconnectorbtc.cpp:1869-1881) tolerate *any* single-byte
    prefix (`toXAddr` erases it unconditionally; `hasValidAddressPrefix` just checks it equals
    addrPrefix or scriptPrefix). Go rejects a base58check string whose version byte matches neither
    configured prefix at decode time. Go is stricter; C++ is lenient.
- P2SH vs P2PKH: both sides accept both prefixes; neither distinguishes beyond the version byte.
  C++ `hasValidAddressPrefix` compares `decoded.size() - sizeof(uint160)` (=1) bytes
  (xbridgewalletconnectorbtc.cpp:1877-1878); Go compares the single prefix byte. Equivalent.
- C++ `isValidAddress` = `hasValidAddressPrefix && rpc::validateaddress`
  (xbridgewalletconnectorbtc.cpp:1892-1895) — wallet-RPC validation has no Go analogue inside
  `DecodeAddress` (it is a pure codec); the wallet leg lives in the RPC layer. **Noted, not a DIFF
  in the codec.**
- Prefixes come from `xbridge.conf` `.AddressPrefix/.ScriptPrefix/.SecretPrefix`, parsed as int
  strings → char (xbridgeapp.cpp:982-984, xbridgewalletconnectorbtc.cpp:1516-1518). Go reads the
  same conf keys into `Coin.P2PKH/P2SH` (coins/coin.go:94-95, config/conf.go). **CONFORMANT.**

### cashaddr (BCH)
- C++ `cashaddr::Decode` (cashaddr/cashaddr.cpp:222-298): enforces all-lower or all-upper, `:` not
  first, no digit in the prefix, **no `:` ⇒ default prefix used** (:267-268), charset via
  `CHARSET_REV`, PolyMod (:43-148), checksum `== 0` after the constant-carrying `PolyMod` (which
  returns `c ^ 1`, :147), `CreateChecksum` (:184-197). Generator constants 0x98f2bc8e61…0x1e4f43e470.
- Go `cashaddrDecode` (coins/cashaddr.go:143-189): length gate 8..110, mixed-case rejection, `:`
  **required** (`pos < 1 ⇒ error`, :153-155), checksum `cashaddrPolymod == 1` (:70-73) with the same
  five 40-bit generator constants (:18-24), HRP expand = low-5-bits + 0 (:45-52), convertBits 5→8.
  Version byte: `hashType = version>>3`, `sizeIdx = version&0x07`, hash lengths 20/24/28/32
  (:95-108, :176-189).
- C++ `cashaddrenc.cpp` `DecodeCashAddrContent` (:114-156): `version&0x80` reserved-bit rejection,
  `type = (version>>3)&0x1f`, `hash_size = 20 + 4*(version&0x03)`, and `version&0x04 ⇒ hash_size*2`
  (supports 40/48/56/64-byte hashes). Go supports only sizeIdx 0-3 and rejects hashType∉{0,1} at
  decode; C++ rejects the 0x80 reserved bit explicitly but effectively also rejects hashType>1 in
  `DecodeCashAddrDestination` (cashaddrenc.cpp:158-175). For XBridge (20-byte hashes) equivalent.
  - **DIFF**: C++ accepts a prefix-less CashAddr (`bitcoincash:…` optional, default prefix);
    Go requires the explicit `bitcoincash:` separator. **DIFF**: C++ supports 40–64-byte hashes,
    Go only 20/24/28/32.
- Encoding: C++ `PackAddrData` version byte = `(type<<3) | sizeIdx` with sizeIdx from a
  bits-switch on 160/192/224/256/320/384/448/512 (cashaddrenc.cpp:19-55); Go `cashaddrEncode`
  version = `(hashType<<3) | sizeIdx` for 20/24/28/32 (cashaddr.go:117-139). For 20-byte P2KH (q…)
  and P2SH (p…) outputs **byte-identical**. The Go comment records that an earlier port packed
  `len>>2` and was byte-incompatible; the current one matches Bitcoin ABC.
- BCH connector glue: C++ `BchWalletConnector::init/hasValidAddressPrefix/fromXAddr/toXAddr/
  scriptIdToString` prefer CashAddr when `cashAddrPrefix` set, fall back to legacy base58check
  (xbridgewalletconnectorbch.cpp:302-359); Go routes BCH-family decode exclusively through CashAddr
  and rejects base58check for that family (coins/address.go:95-105). **DIFF**: C++ BCH accepts legacy
  base58check too; Go BCH accepts only CashAddr.

---

## Card 3 — HTLC CONSTRUCTION (deposit) — **CONFORMANT (byte-identical structure)**

### C++ (reference)
- `BtcWalletConnector::createDepositUnlockScript` (xbridgewalletconnectorbtc.cpp:2350-2368):
  ```
  OP_IF
      <lockTime> OP_CHECKLOCKTIMEVERIFY OP_DROP
      OP_DUP OP_HASH160 <getKeyId(myPubKey)> OP_EQUALVERIFY OP_CHECKSIG
  OP_ELSE
      OP_DUP OP_HASH160 <getKeyId(otherPubKey)> OP_EQUALVERIFY OP_CHECKSIGVERIFY
      OP_SIZE 33 OP_EQUALVERIFY OP_HASH160 <secretHash> OP_EQUAL
  OP_ENDIF
  ```
- `getKeyId` = `Hash160(pubkey)` = RIPEMD160(SHA256(pubkey)) (xbridgewalletconnectorbtc.cpp:1919-1923).
- Secret-hash: maker's deposit uses `hx = connTo->getKeyId(xtx->xPubKey)` (xbridgesession.cpp:2053),
  i.e. the 33-byte per-order secret **public key** is the preimage; `HASH160(secret) == secretHash`.
- P2SH address: `scriptIdToString(getScriptId(lockScript))` = base58check(scriptPrefix ‖
  HASH160(script)) (xbridgewalletconnectorbtc.cpp:1928-1942).
- Refund scriptSig: `<signature> <myPubKey> OP_1 <inner>` (createRefundTransaction :2477-2495).
- Payment scriptSig: `<xPubKey> <signature> <myPubKey> OP_0 <inner>` (createPaymentTransaction
  :2558-2561).

### Go (candidate)
- `coins.BuildDepositUnlockScript` (coins/htlc.go:33-58) emits the **same opcode sequence**:
  `OP_IF ⟨lockTime⟩ CLTV DROP DUP HASH160 KeyID(my) EQUALVERIFY CHECKSIG OP_ELSE DUP HASH160
  KeyID(other) EQUALVERIFY CHECKSIGVERIFY OP_SIZE ⟨33⟩ EQUALVERIFY HASH160 secretHash EQUAL OP_ENDIF`.
- `coins.KeyID` = RIPEMD160(SHA256(pubkey)) (htlc.go:11-18) ≡ C++ `getKeyId`.
- `swap.DepositSpec.SecretHash()` = RIPEMD160(SHA256(Secret)) over the 33-byte secret
  (swap/deposit.go:65-75) ≡ C++ `getKeyId(xPubKey)` — **the hashed-secret ordering is
  RIPEMD160(SHA256(x)) on both sides**.
- `coins.BuildP2SHScript` = `OP_HASH160 <HASH160(redeem)> OP_EQUAL` (htlc.go:99-104); deposit output
  built via `d.P2SHScript()` (swap/deposit.go:84-86,142).
- `coins.BuildRefundScriptSig` / `BuildPaymentScriptSig` (htlc.go:62-78) mirror the C++ refund/payment
  scriptSig order exactly.
- Script-number / push encoding: `coins.pushData` (script.go:31-52) and `pushNum` (script.go:57-88)
  implement Bitcoin minimal push and minimal CScriptNum encoding (sign-byte rule), matching C++
  `CScript::operator<<` semantics for `<lockTime>` (32-bit → minimal LE) and `<33>` (0x01 0x21).
- **VECTOR/assert**: the redeem-script opcode skeleton is structurally identical; the only inputs
  are KeyID(myPub)/KeyID(otherPub)/secretHash (20 bytes each) and the minimal-encoded lockTime, all
  pushed with minimal opcodes — so the produced byte strings are identical for identical inputs.

**DIFFs**: none in script construction. (Maker vs taker both call the same builder;
xbridgesession.cpp:2069 (maker), :2592 (taker) vs Go single builder + role-parameterized keys.)

---

## Card 4 — TX SIGNING / SIGHASH — **CONFORMANT for legacy; BCH forkid MISSING (DIFF)**

### C++ (reference)
- Deposit tx: **NOT signed locally** — built via `rpc::createRawTransaction` and signed via
  `rpc::signRawTransaction` (wallet RPC) (xbridgewalletconnectorbtc.cpp:2379-2389). Also
  `createPartialTransaction` (:2594-2623).
- Refund / payment txs: **signed locally** with a private key:
  - `uint256 hash = SignatureHash(inner, txUnsigned, 0, SIGHASH_ALL)` (createRefundTransaction
    :2483; createPaymentTransaction :2547).
  - `m_cp.sign(...)` produces DER, then `signature.push_back(SIGHASH_ALL)` (:2492, :2556).
  - scriptSig assemblies at :2494-2495 and :2558-2561 (see Card 3).
- The local `SignatureHash` (xbridgewalletconnectorbtc.cpp:1434-1505) is the **legacy** sighash
  (CTransactionSignatureSerializer + trailing 4-byte nHashType, `GetHash` = double-SHA256); the
  BIP143 witness branch is **commented out** (:1440-1483).
- Sequence for CLTV spends: `SEQUENCE_FINAL-1` when lockTime>0 else `SEQUENCE_FINAL`
  (createRefundTransaction :2472).
- BCH connector: signs locally with the **BCH forkid sighash**:
  `SigHashType sigHashType = SigHashType(SIGHASH_ALL).withForkId();` (i.e. 0x41),
  `SignatureHash(inner, tx, 0, sigHashType, inputs[0].amount*COIN, rpe)` with replay protection
  (xbridgewalletconnectorbch.cpp:396-398; implementation :191-257 — BIP143 preimage, forkid byte,
  `0xdead` replay-protection fork-value mutation at :203-209).

### Go (candidate)
- `coins.SigHashAll = 0x01` (coins/tx.go:15). Legacy digest `HashForSigning` (tx.go:356-400):
  version [‖ nTime if WithTime] ‖ vin (scriptSig of index replaced by prevScript, others blanked)
  ‖ vout ‖ locktime ‖ sighash(4B LE), double-SHA256. Mirrors the C++ legacy serializer including
  the `nTime` quirk.
- BIP143 digest `HashForSigningSegwit` (tx.go:414-478): standard BIP143 preimage.
- `SignTxInput` (tx.go:494-500) / `signDigest` (tx.go:517-523): DER + 0x01 (≡ C++ `m_cp.sign` +
  `push_back(SIGHASH_ALL)`).
- Deposit tx signed via the **wallet RPC** `conn.SignRawTransaction` (api/swap.go:1097) — matches
  C++'s deposit path. Refund (api/swap.go:1135-1156) and claim (:1193-1212) are signed **locally**
  with `coins.SignTxInput` (legacy SIGHASH_ALL) — matches C++'s refund/payment local signing.
- Refund sequence `0xfffffffe` (swap.go:1146), claim `0xffffffff` (swap.go:1205) — match C++.
- Input/output ordering: C++ deposit vouts `[P2SH, change]` (xbridgesession.cpp:2094-2099);
  Go `BuildDepositTx` `[P2SH, change]` (swap/deposit.go:142-145). **CONFORMANT.**

### DIFFS
- **BCH forkid sighash is absent in Go.** The BCH family affects only the address codec
  (coins/coin.go:107-113, address.go:54/95). There is no 0x41/forkid digest anywhere in
  `coins/tx.go`. A Go-built BCH refund/claim signed via the local path would commit with the legacy
  0x01 digest — a signature the BCH chain rejects. (If the BCH deposit/refund goes through an RPC
  wallet's signrawtransaction, the wallet applies the forkid sighash correctly — so the gap is
  confined to the LocalConnector path, but the codec has no forkid support at all.) **FLAG.**
- Go's `HashForSigningSegwit` is never used by the C++ BTC path (its BIP143 branch is commented
  out); it exists for the segwit/bech32 LocalConnector path — a Go extension, not a port.

---

## Card 5 — UTXO SELECTION ALGORITHM — **CONFORMANT (both fully traced)**

### C++ (reference)
- `App::selectUtxos` (xbridgeapp.cpp:2972-3077):
  - Input utxos copied and sorted by amount **descending** (:3056-3062).
  - Inner `selUtxos` (:2984-3053): ideal pass picks the first utxo with
    `amount ≥ minAmount(=amt+minTxFee1(1,3)+minTxFee2(1,1))` and
    `amount < minAmount + (minTxFee1(1,3)+minTxFee2(1,1))*1000` and `(address==addr || addr.empty())`
    (:2993-3007). Else buckets into gt/lt; `gt.size()==1` ⇒ take it, `gt.size()>1` ⇒ sort ascending,
    take smallest > min (:3017-3025); else sort lt **descending**, accumulate until
    `Σsel − (minTxFee1(|sel|,3)+minTxFee2(1,1)) ≥ minAmount` (:3026-3051).
  - `utxoAmount = Σ amount*coinDenomination` (1e6 scale) (:3069-3070);
    `fee1 = minTxFee1(|out|,3)*1e6`, `fee2 = minTxFee2(1,1)*1e6` (:3073-3074).
  - Call site (makeOrder): pre-filter `if (!useAllFunds) remove utxos whose address != from`
    (xbridgeapp.cpp:1621-1627); lock-exclusion via `getAllLockedUtxos` (:1614).
- `App::selectPartialUtxos` (xbridgeapp.cpp:3079-3237): ideal utxo = `camount == requiredSplitSize +
  requiredFeePerUtxo` (first loop, :3098-3118); exact-match checks (:3125, :3153); ascending sort
  (:3131-3134) then exact-remainder search (:3136-3156) or "enough to cover" with prep fees
  (:3158-3182); largest-utxo remainder loops (:3190-3226); final
  `fees = |out|*requiredFeePerUtxo` (:3231); success iff `|out|>0 && utxoAmount − totalNeeded > 0`
  (:3233-3234). No address argument is used inside the function.
- **There is no order-id→hash→selection-seed mechanism anywhere in C++ selection**; selection is
  purely amount-ordered. (The task's premise does not appear in source; neither side has it.)

### Go (candidate)
- `api/selectUtxos` (utxo_select.go:79-144) mirrors C++ exactly: descending sort (:130), ideal pass
  with the same window and address filter (:89-98), gt/lt fallback (:102-124), `utxoAmount`/`fee1`
  on the 1e6 scale (:140-142). `feeAmount` (:80-82) and `minTxFeeWhole` (:46-51) equal the C++ fee
  closures.
- `api/selectPartialUtxos` (utxo_select.go:153-299) mirrors C++ line-for-line incl. the final
  `fees = len(outputsForUse)*requiredFeePerUtxo` (:293) and the `utxoAmount − totalAmountNeeded <= 0`
  failure test (:295-298). Address argument dropped (C++ never reads it).
- Pre-filter for `use_all_funds=false` and locked-utxo exclusion in `dxMakeOrder`
  (api/node.go:1016-1030) match C++ :1614/:1621-1627.

### Worked example (traced on both sides)
Setup: 6-decimal coin (native COIN = 1e6 = XBridge scale so the C++ mixed-scale quirk is inert),
`FeePerByte=2`, `MinTxFee=1000` ⇒ `minTxFee1(1,3)=minTxFee2(1,1)=0.001` whole;
order `requiredAmount = 1_000_000` ⇒ `amt = 1.0`; `minAmount = 1.002`; ideal window `[1.002, 3.002)`.
Utxos (whole): `{0.3, 0.5, 0.9, 1.5, 2.5, 4.0}` all at maker addr.

- Case A (ideal hit): descending `[4.0, 2.5, 1.5, 0.9, 0.5, 0.3]` → 2.5 is ideal ⇒ select `[2.5]`,
  `utxoAmount=2_500_000`, `fee1=fee2=1000`. C++ and Go agree.
- Case B (no ideal, two gt): remove 2.5 ⇒ `[4.0,1.5,0.9,0.5,0.3]`; gt={4.0,1.5} → sort asc → `[1.5]`.
  Agree.
- Case C (all lt): set `{0.9,0.5,0.3}`; lt descending `[0.9,0.5,0.3]`; sel={0.9} running
  −0.002+0.9=0.898 < 1.002; sel={0.9,0.5} running −0.002+1.4=1.398 ≥ 1.002 ⇒ `[0.9,0.5]`;
  `utxoAmount=1_400_000`, `fee1=minTxFee1(2,3)=0.001*1e6=1000`. Agree.
- Partial ideal: `requiredSplitSize=1.0`, `feePerUtxo=0.001(1e6 units)` ⇒ ideal amount 1.000001; a
  utxo `{1.000001, 1.000001}` exactly matches ⇒ `exactUtxoMatch=true` on both (C++ :3125, Go
  :200). Agree.

Both sides were fully traced from source; nothing was left TBD.

---

## Card 6 — FEE CALCULATION — **MOSTLY CONFORMANT; 3 numeric DIFFs**

### Size model / floor (deposit, refund, payment, selection) — CONFORMANT
- C++: `minTxFee1 = (192*in + 34*out)*feePerByte`, floor `minTxFee`, `/COIN`
  (xbridgewalletconnectorbtc.cpp:1948-1957); `minTxFee2` identical shape (:1963-1972).
- Go: `estimateFee = (192*nIn+34*nOut)*FeePerByte`, floor `MinTxFee` (api/handlers.go:1463-1482);
  `minTxFeeWhole = estimateFee/cc.Coin` (utxo_select.go:46-51). **CONFORMANT.**

### Dust — CONFORMANT, plus a Go-only conf key
- C++: `dustAmount = relayFee>0 ? 0.546*relayFee*COIN : 5460` (xbridgewalletconnectorbtc.cpp:1526);
  `isDustAmount(amount) = int64(amount*COIN) < int64(dustAmount)` (:1900-1904).
- Go: `effectiveDust = relayFee>0 ? 0.546*relayFee*cc.Coin : (cc.DustAmount>0 ? cc.DustAmount : 5460)`
  (handlers.go:1449-1457); `isDustNative` same comparison (utxo_select.go:58-68).
- **DIFF**: C++ never reads a `DustAmount` conf key (xbridgeapp.cpp:978-997 has no such key);
  Go honors it (config/conf.go:268). A configured `DustAmount=` in xbridge.conf changes Go's dust
  threshold but is ignored by C++. Go documents this as a thin-client extension.

### BLOCK service-node fee tx — CONFORMANT
- C++: `serviceNodeFee = .015` (xbridgewallet.h:119), `blockFeePerByte = 40/COIN`
  (xbridgeapp.cpp:2257), `estFee = (192*in+34*out)*feePerByte` (bitcoinrpcconnector.cpp:96-98),
  fee-utxo selector ideal window `min + estFee(1,3)*100` (:113-115), change kept only if `≥ 5460`
  (:206), vouts `[OP_RETURN data, amount→registry, change?]` (:201-207), P2PKH-25-only inputs
  (`unspentP2PKH`, :273-302), order-info JSON `["", fromCur, fromAmt, toCur, toAmt]` with 64-hex
  order id prepended, cap `nMaxDatacarrierBytes-3 = 157` (xbridgeapp.cpp:2207-2229).
- Go: `serviceNodeFeeReal=0.015` (fee_tx.go:18), `feeTxRelayPerByte=40/1e8` (fee_tx.go:23),
  `estFeeBlock=(192*in+34*out)*40/1e8` (fee_tx.go:87-89), `selectFeeUtxos` window `estFeeBlock(1,3)*100`
  (fee_tx.go:102-107), change dust `minFeeChangeDust=5460` (fee_tx.go:27,193), vouts order
  `[OP_RETURN, fee→dest, change?]` (fee_tx.go:191-199), `isP2PKH25` filter (fee_tx.go:146-152),
  `feeOrderInfo` (fee_tx.go:53-82) mirrors the JSON construction incl. truncation path.
  **CONFORMANT.**

### DIFF 6a — deposit network fee output count
- C++ deposit fee: `minTxFee1(usedInTx.size(), 3)` (xbridgesession.cpp:1993) — assumes **3 outputs**.
- Go deposit fee: `estimateFee(cc, len(funding), 2)` (api/swap.go:1064) — assumes **2 outputs**.
- The actual deposit tx has 2 outputs (P2SH + change; xbridgesession.cpp:2094-2099 vs
  swap/deposit.go:142-145). C++ over-estimates by `34*feePerByte`; Go charges the exact count.
  Small but a real numeric divergence (relevant for the fee the deposit pays on-chain).

### DIFF 6b — split (dxSplitAddress/dxSplitInputs) fee — the "520 vs 192·nIn+68" divergence
- C++ `splitUtxos`: per-output fee when `includeFees` = `feesPerUtxo = minTxFee1(1,3) + minTxFee2(1,1)`
  = `(294+226)*feePerByte` = **520·feePerByte** (xbridgewalletconnectorbtc.cpp:2665-2668); and a
  separate tx fee `txFees = minTxFee1(vins.size(), vouts.size())` with the **actual** output count,
  subtracted from the remainder (`change = remainder − txFees`) with a claw-back loop when change is
  dust (:2702-2734).
- Go `splitTx`: per-output fee = `estimateFee(cc, len(utxos), 2)` = `(192·nIn + 68)·feePerByte`
  (api/handlers.go:1356-1362); no tx fee is subtracted from `change` at all (change = total − spent,
  dust-dropped, :1371-1379).
- Net effect for 1 input: C++ inflates each split by 520·fpb and additionally deducts a real tx fee;
  Go inflates each split by 260·fpb and deducts nothing from change. **The reported "C++ 520·fpb vs
  Go 192·nIn+68" is confirmed by source.** (They coincide only at nIn=1, nOut=2 for the *tx* fee
  term — 192+68=260 — but the C++ per-output fee is a different, larger quantity.)
- Note: the **autoSplit partial-order prep path** is CONFORMANT — both use `fee1+fee2` (520·fpb):
  C++ xbridgeapp.cpp:1590-1592 vs Go node.go:985-987.

### DIFF 6c — missing FeePerByte fallback
- C++ with `FeePerByte=0` and `MinTxFee=0` ⇒ fee = 0 (xbridgewalletconnectorbtc.cpp:1951-1955).
- Go with `FeePerByte=0` falls back to **2 sat/vB** (api/handlers.go:1470-1475), so a nil/unset
  fee rate yields a positive fee where C++ yields zero. Deliberate Go fallback; a divergence from
  C++'s zero-fee behavior.

---

## Card 7 — LOCKTIME / BLOCK HEIGHT — **CONFORMANT**

### C++ (reference)
- `lockTime(role)` (xbridgewalletconnectorbtc.cpp:2286-2326):
  - role 'A': `blocks = XMAKER_LOCKTIME_TARGET_SECONDS(7200)/blockTime`, floor
    `XMIN_LOCKTIME_BLOCKS(6)`; `lt = info.blocks + blocks` (:2310-2313).
  - role 'B': `takerTime = XTAKER_LOCKTIME_TARGET_SECONDS(1800)`, or
    `XSLOW_TAKER_LOCKTIME_TARGET_SECONDS(3600)` when `blockTime >= XSLOW_BLOCKTIME_SECONDS(600)`;
    floor 6; `lt = info.blocks + blocks` (:2317-2322).
  - `info.blocks == 0 ⇒ return 0` (:2300-2304). Constants in xbridgewallet.h:96-102.
- `acceptableLockTimeDrift` (xbridgewalletconnectorbtc.cpp:2331-2345):
  `lt==0 || lt>=LOCKTIME_THRESHOLD || lckTime>=LOCKTIME_THRESHOLD ⇒ false`;
  `diff = lt − lckTime` (blocks); `drift = max(XLOCKTIME_DRIFT_SECONDS=900, XMAX_LOCKTIME_DRIFT_BLOCKS=4 * blockTime)`;
  accept iff `diff * blockTime <= drift` (seconds). Drift check is one-sided (rejects counterparty
  locktime far below ours).

### Go (candidate)
- `computeLockTimeFor` (api/swap.go:993-1021): `n = GetBlockCount()`, `n<1 ⇒ 0`; `bt` default 60
  (C++ reads conf `BlockTime`; C++ with `BlockTime=0` would divide by zero, so conf always sets it —
  Go's 60 default is a safe-guard); maker target 7200, taker 1800, slow-taker 3600 when `bt>=600`;
  `blocks = target/bt`, floor 6; `return n + blocks`. Constants (swap.go:24-30, locktime.go:11-15).
- `acceptableLockTimeDrift` (api/locktime.go:29-39): identical formula and one-sidedness.
- **Boundaries match**: `blocks < XMIN` uses strict `<` on both sides (so exactly 6 stays);
  `lt >= LOCKTIME_THRESHOLD` (500_000_000) on both; success test `diff*blockTime <= drift` on both.
- Units: both are absolute **block heights** (not seconds), and the drift is compared in seconds
  (`diff_blocks * seconds_per_block <= drift_seconds`). **CONFORMANT.**

---

## Card 8 — ORDER-ID / HASHING — **CONFORMANT**

### C++ (reference)
- Order id (xbridgeapp.cpp:1729-1763):
  ```
  CHashWriter ss(SER_GETHASH, 0);
  ss << ptr->from          // 20-byte vector → WriteCompactSize(20) + 20 bytes
     << ptr->fromCurrency  // std::string → WriteCompactSize + bytes
     << ptr->fromAmount    // uint64_t (xbridgetransactiondescr.h:240) → 8-byte LE
     << ptr->to            // 20-byte vector
     << ptr->toCurrency    // std::string
     << ptr->toAmount      // uint64_t → 8-byte LE
     << timestampValue     // uint64_t, timeToInt = microseconds (xutil.cpp:276-283)
     << ptr->blockHash     // uint256 → 32 raw bytes (internal LE order)
     << outputsForUse.at(0).signature; // vector → WriteCompactSize + bytes
  id = ss.GetHash();       // CHashWriter = double-SHA256
  ```
  Inputs: `from`/`to` = 20-byte hashes from `toXAddr`; `timestamp` = µs since epoch; `blockHash` =
  `chainActive.Tip()->pprev->GetBlockHash()` (the pre-tip block, :1731); signature = 65-byte
  signmessage decode of the first selected utxo (:1696-1697). Partial-order path re-hashes the id
  after the prep tx with the final proof signatures (:1936-1946).
- The make (xbcTransaction) wire body = `[id(32), from(20), fromCur(8,padded), fromAmount(u64),
  to(20), toCur(8), toAmount(u64), created(µs,u64), blockHash(32), partial(u16), minFromAmount(u64),
  utxoCount(u32), utxos…]` (xbridgeapp.cpp:2074-2096). Note: fromAmount/toAmount are the **same**
  uint64 base units on the wire.

### Go (candidate)
- `sha256dOrderID` (api/utxo_select.go:301-322): `MarshalVarStr(from) ∥ MarshalVarStr(fromCur) ∥
  u64LE(fromAmt) ∥ MarshalVarStr(to) ∥ MarshalVarStr(toCur) ∥ u64LE(toAmt) ∥ u64LE(ts) ∥ blockHash ∥
  MarshalVarStr(sig)`, then **double-SHA256**.
- `MarshalVarStr` = Bitcoin VarInt (p2p/version.go:92-111) ≡ C++ WriteCompactSize for these
  lengths. `fromAmt`/`toAmt` are base-unit uint64 from `parseXAmount` (api/node.go:870-877) —
  matching C++'s uint64 `fromAmount` serialization (the earlier worry that C++ serializes a
  `double` is unfounded: `TransactionDescr::fromAmount` is `uint64_t`, xbridgetransactiondescr.h:240).
- `ts = NowMicro()` = `time.Now().UnixMicro()` (api/store.go:517-519) ≡ C++ `timeToInt` (µs).
- `blockHash` = the pre-tip block hash from the BLOCK connector (`GetBlockCount()-1`,
  api/node.go:494-511) ≡ C++ pre-tip hash (:1731). Byte order assumed identical internal LE on both
  (both feed the same bytes into the id hash and the wire body); **TBD** only if the RPC
  getblockhash path ever returns display order.
- Signature = `buildUtxoProofs(...).Signature` (65 bytes; api/node.go:1051,1067), re-hashed after an
  autoSplit prep tx (api/node.go:1160-1170) — matches C++ :1881-1946.
- Go `OrderBody.Marshal` (proto/body_types.go:121-140) matches the C++ make-body field order and
  widths (raw id/hash/addr, 8-byte currency, LE u64 amounts, µs created). **CONFORMANT.**
- **No divergence found.** Field order, varint encoding, amount width, µs timestamp, double-SHA256
  all match.

---

## CANDIDATE FINDINGS (summary)

1. **Card 4 / BCH sighash**: Go has no BCH forkid (0x41) sighash; a locally-signed BCH refund/claim
   would commit with legacy 0x01 and be rejected on-chain. C++ uses forkid + replay protection
   (xbridgewalletconnectorbch.cpp:191-257,396-398).
2. **Card 6a / deposit fee**: C++ uses `minTxFee1(nIn, 3)`, Go `estimateFee(nIn, 2)` — Go charges
   34·feePerByte less than C++ for the deposit network fee (xbridgesession.cpp:1993 vs swap.go:1064).
3. **Card 6b / split fee**: dxSplit per-output fee C++ = fee1+fee2 = **520·feePerByte**
   (xbridgewalletconnectorbtc.cpp:2665-2668) vs Go = `(192·nIn+68)·feePerByte`
   (handlers.go:1356-1362); C++ additionally deducts a real tx fee from change with a dust claw-back
   (…:2702-2734) which Go does not do. AutoSplit prep path is conformant (both 520·fpb).
4. **Card 6c / missing FeePerByte**: C++ with unset FeePerByte yields fee 0; Go falls back to 2
   sat/vB (handlers.go:1470-1475). Deliberate but divergent.
5. **Card 2 / cashaddr strictness**: Go rejects prefix-less CashAddr and base58check for BCH, and
   only supports 20/24/28/32-byte hashes; C++ accepts prefix-less (default "bitcoincash"), legacy
   base58check on BCH, and up to 64-byte hashes.
6. **Card 2 / base58check version byte**: Go rejects an address whose version byte matches neither
   configured prefix; C++ `toXAddr`/`hasValidAddressPrefix` are lenient (prefix byte erased / only
   two fixed comparisons).
7. **Card 6 / DustAmount conf key**: Go honors a `DustAmount` conf key C++ ignores (config/conf.go:268
   vs xbridgeapp.cpp:978-997).
8. **Card 1 / RNG**: Go `NewPrivateKey` clears the top bit (crypto/signer.go:145); C++ retries
   full-range randoms (xbridgecryptoproviderbtc.cpp:204-211). Valid keys either way; distribution
   bias, no wire impact.
9. **Card 8 / block-hash byte order**: id-hash byte order assumed identical on both sides; TBD only
   if the Go RPC getblockhash returns display order somewhere.

Report written to: `docs/audit/evidence/crypto.md`
