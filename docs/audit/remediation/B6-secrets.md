# Diff brief — persist + logging secrets hygiene (B6, SEC-F04)

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

## Go deltas (this branch)

1. **`api/persist.go` `persistFromSession`** — when `Config.PersistSecrets` is
   false, zero `PrivKey`/`Secret`/`RefundHex` at write time. `PubKey`,
   `SecretHash`, `OurDepositTxID`, `OurLockTime` always survive (hashes /
   derived, not signing material). Order-only and history records already write
   zero secrets (`persistFromOrder` / `persistFromHistoryEntry`), so only live
   session records are gated.
   - **No restore-path change.** `restoreSwap` / `hasSessionData` are untouched:
     a secret-less restored session is carried by `State > csIdle` and
     `OurDepositTxID != ""`, and the existing guards (`swap.go:823` refund
     sweep skips `refundHex == ""`, `persist.go:325` terminal routing) govern it
     exactly as they govern order-only / refund-done records today. This is the
     one **deliberate, write-only** divergence from C++ (SEC-F04 hardening);
     with the flag on (default) the on-disk output is byte-identical to C++.
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

## Config

- `api/node.go` `Config.PersistSecrets bool` — default true (C++ parity).
- `cmd/xbridged/main.go` `-persistsecrets` flag (default true). CLI-flag only;
  `config/conf.go` conf-key alignment is B10's (`fix/config-parity`) concern —
  files stay disjoint.

## Opt-out consequence (documented, not coded)

With `-persistsecrets=false`, a swap that is mid-flight at restart has no M
keypair / refund, so the engine cannot auto-refund or re-sign a cancel after
restart (`coins` signing uses the per-trade privkey, `api/swap.go:1151,1210`).
This is inherent to the operator's opt-out choice and is documented in the flag
help, `README.md`, and `register.md` — it is **not** a new runtime branch.

## Tests

- `newPersistNode` sets `PersistSecrets: true` (existing round-trip /
  cancel-after-restart tests keep their parity meaning).
- `TestPersistSecretsOptOut` — write with the flag off: `PrivKey`/`Secret`/
  `RefundHex` zeroed on disk, `PubKey`/`SecretHash`/`OurDepositTxID`/
  `OurLockTime`/`State` survive; `restoreSwap` re-adds the order and restores
  the live session with zero signing material.
- `TestCorruptSwapFileContinuesLikeCpp` — bad-checksum file: `loadSwaps`
  errors, `restoreLocalSwaps` logs at Error and restores nothing.
