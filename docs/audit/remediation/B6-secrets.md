# Diff brief — persist + logging secrets hygiene (B6, SEC-F04)

> Amendment (recovery hardening, 2026-09-10): the `-persistsecrets=false`
> opt-out described below was **removed**. Secrets are now always persisted
> (strict C++ `orders.dat` parity); `Config.PersistSecrets`, the CLI flag, the
> write-time zeroing, and `TestPersistSecretsOptOut` are gone, replaced by
> `TestPersistSecretsAlwaysOnDisk`. The log-hygiene removals (§3 below) stand
> unchanged; refund/claim hex logging is a separate pending recovery item
> (Core TXLOG-hatch parity) — privkeys/preimages stay out of logs.

Session-scratch notes for branch `fix/secrets-hygiene`. Source of truth is the
C++ writers; the register row (`SEC-F04`) carries the canonical status.

## Finding

SEC-F04 (S2): the swap-state file persists each trade's per-trade M keypair
(`PrivKey`), HTLC preimage (`Secret`), and pre-signed refund (`RefundHex`) in
plaintext, and debug logs emit refund/claim tx hex and full RPC bodies.

## C++ reference (parity baseline)

- `App::loadOrders` — `xbridgeapp.cpp:3828-3866`: a failed `xdb.Read` logs
  `LogOrderMsg(erro, "Failed to load existing orders database")` and **returns
  with an empty set** — the node never refuses to start.
- `App::saveOrders` — `xbridgeapp.cpp:3868-3893`: calls `xdb.Write(orders,
  force)` and **ignores the bool return**; write failures are surfaced only by
  `SerializeFileDB`'s internal `error(...)` logs.
- `SerializeFileDB` / `DeserializeFileDB` — `xbridgedb.cpp:36-107`: `error(...)`
  (ERROR severity) on open/flush/rename/checksum failure.
- C++ persists the per-trade M keypair and swap secrets in `orders.dat`
  plaintext; there is **no** log line like Go's refund/claim `refundHex` /
  `payHex` debug output anywhere in `xbridgesession.cpp`.

## Go deltas (this branch, as merged — historical; the opt-out in §1/§Config
below was removed 2026-09-10, see header amendment)

1. **`api/persist.go` `persistFromSession`** — when `Config.PersistSecrets` was
   false, zeroed `PrivKey`/`Secret`/`RefundHex` at write time. `PubKey`,
   `SecretHash`, `OurDepositTxID`, `OurLockTime` always survived (hashes /
   derived, not signing material). Order-only and history records already wrote
   zero secrets (`persistFromOrder` / `persistFromHistoryEntry`), so only live
   session records were gated.
   - **No restore-path change.** `restoreSwap` / `hasSessionData` were untouched:
     a secret-less restored session was carried by `State > csIdle` and
     `OurDepositTxID != ""`, and the existing guards (`swap.go:823` refund
     sweep skipping `refundHex == ""`, `persist.go:325` terminal routing)
     governed it exactly as they govern order-only / refund-done records. This
     was the one **deliberate, write-only** divergence from C++ (SEC-F04
     hardening); with the flag on (default) the on-disk output was
     byte-identical to C++.
2. **`api/node.go`** — `NewNode` restore block extracted to
   `restoreLocalSwaps`; a corrupt swap file logs at **Error** (C++ `erro`
   parity; was `Warn`) and continues with an empty set — it never refuses to
   start, matching `loadOrders`.
3. **Log removals** (pure deletions; no C++ analogue exists for any of them):
   - `api/swap.go:383,485` — drop `refundHex` from the deposit refund
     pre-signed debug lines.
   - `api/swap.go:545,650` — drop `payHex` from the claim-tx debug lines (the
     claim scriptSig carries the HTLC preimage).
   - `wallet/rpc.go:129-130,138-140` — drop the full RPC response `body` /
     `Result` from decode-failure logs and error text.
   - `swap.go:643` (logs txid + secretHash, not the preimage) and
     `swap.go:1142/1269/1274/1278` (params/err/ok only) verified not leaking.

## Config (historical — flag and field removed 2026-09-10)

- `api/node.go` `Config.PersistSecrets bool` — defaulted true (C++ parity).
- `cmd/xbridged/main.go` `-persistsecrets` flag (defaulted true). CLI-flag only;
  `config/conf.go` conf-key alignment was B10's (`fix/config-parity`) concern —
  files stayed disjoint.

## Opt-out consequence (historical — opt-out removed 2026-09-10; kept here as
the record of what the merged branch documented)

With `-persistsecrets=false`, a swap that was mid-flight at restart had no M
keypair / refund, so the engine could not auto-refund or re-sign a cancel after
restart (`coins` signing uses the per-trade privkey, `api/swap.go:1151,1210`).
This was inherent to the operator's opt-out choice and was documented in the
flag help, `README.md`, and `register.md` — it was **not** a new runtime branch.

## Tests

- `newPersistNode` set `PersistSecrets: true` (existing round-trip /
  cancel-after-restart tests kept their parity meaning).
- `TestPersistSecretsAlwaysOnDisk` (recovery hardening, replaces the removed
  `TestPersistSecretsOptOut` below) — secrets always on disk, restore keeps the
  session sign-capable (M pubkey re-derivation).
- `TestCorruptSwapFileContinuesLikeCpp` — bad-checksum file: `loadSwaps`
  errors, `restoreLocalSwaps` logs at Error and restores nothing.
