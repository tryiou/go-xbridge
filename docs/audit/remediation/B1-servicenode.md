# Diff brief — `pkg/servicenode` (B1, WIRE-F71)

Session-scratch notes for branch `fix/servicenode-registry`. Source of truth is
the C++ writers; keep this file up to date while working.

## Finding

WIRE-F71 (Critical): `ParseServiceNode`/`ParseServiceNodePing`
read-then-discard paymentAddress, collateral, bestBlock, bestBlockHash, and the
registration signature. `AddRegistration`/`AddPing` gate only on
tier/pubkey/services/ping-sig. C++ `isValid` requires chain ancestry, collateral
count/dups/ownership/total, payment address, and both signatures
(`servicenode.h:398-483, 782-820`).

## C++ source of truth

- `ServiceNode::CreateSigHash` — `servicenode.h:104-111`:
  `CHashWriter(SER_GETHASH,0) << snodePubKey << uint8(tier) << paymentAddress <<
  collateral << bestBlock << bestBlockHash` → SHA256d.
  Wire serialization of each operand (matches Go reader):
  - CPubKey = varstr of 33 raw bytes
  - tier = 1 byte
  - CKeyID paymentAddress = 20 raw bytes
  - collateral = CompactSize count + count × (txid 32 bytes, vout uint32 LE)
  - bestBlock = uint32 LE (registration wire parses it as int32 — same 4 bytes)
  - bestBlockHash = 32 raw bytes
- `ServiceNode::isValid` — `servicenode.h:398-484`:
  - `:405` `snodePubKey.IsFullyValid()` (length + point-on-curve)
  - `:409` tier == SPV only
  - `:426` paymentAddress not null
  - `:430` collateral non-empty, `<= snMaxCollateralCount` (10, params.h)
  - `:434-436` no duplicate COutPoints (thin-client checkable)
  - `:438-441` `RecoverCompact(sigHash, signature)` must succeed; the recovered
    pubkey is then matched against on-chain collateral utxos `:447-476`
    (needs chain index in C++; Go enforces the SPV-checkable subset — FIXED,
    `register.md` WIRE-F71 / `evidence/inventory.md`)
  - `:478` SPV total >= `COLLATERAL_SPV` = 5000 * COIN (same: chain-index tier
    out of thin-client scope; SPV-tier subset enforced, WIRE-F71 FIXED)
- `ServiceNodePing::isValid` — `servicenode.h:782-821`: `:818` runs
  `snode.isValid(...)` (registration checks) unless `skipBlockchainValidation`.
  Wire pings: `processPing(vRecv, ping)` with default `skipValidation=false`
  (`net_processing.cpp:2976-2981`), so the embedded registration must be valid.
- `servicenodemgr.h`: `processPing:177-194` (whole ping rejected on invalid
  `isValid`), `addPing:843-852` (strict-newer gate — already ported),
  `addSn:861-871` (registration path `addSn(sn, true, true)` at `:162`).

## Go deltas

- `ServiceNode` struct gains `PaymentAddress [20]byte`, `Collateral []CollateralUTXO`,
  `BestBlock int32`, `BestBlockHash [32]byte`, `Signature []byte`.
- `skipCollateral` → `readCollateral` (retains). `ParseServiceNode` +
  `parseInnerServiceNode` retain all fields.
- New `crypto.FullyValidPubKey` (`secp256k1.ParsePubKey` error check) for
  `IsFullyValid`.
- New registration-sig verification: rebuild `CreateSigHash` bytes from the
  parsed fields → `DoubleSHA256` → `RecoverCompact` must succeed (recovered
  key's identity is the on-chain RESIDUAL).
- `AddRegistration` gates on the thin-client `isValid` subset (SPV tier, fully
  valid pubkey, non-null payment address, collateral 1..10 no dups, valid sig).
- `ParseServiceNodePing` runs the embedded-registration subset (C++
  `ping.isValid:818`); invalid embedded SN → parse error → ping dropped.
- Registry `entry` gains `paymentAddress`; new `PaymentAddress(key) ([20]byte,
  bool)` for B2 (hub fee destination).

## Tests

- Rework `buildPingParts` to emit a VALID embedded registration (non-zero
  paymentAddress, 1 collateral outpoint, embedded SN signed over
  `CreateSigHash`). Existing ping tests exercised zeros/empty → must be updated.
- New: parse-retains-fields; AddRegistration reject matrix; AddPing rejects
  invalid embedded registration; PaymentAddress resolution; CreateSigHash
  known-answer golden.
