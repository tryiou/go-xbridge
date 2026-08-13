# Remediation plan — audit findings

Authoritative register: [`register.md`](register.md) — the
single canonical list of axis-prefixed IDs (`RPC-F01..RPC-F59`,
`WIRE-F57..F71`, `STATE-F71..F79`, `CRYPTO-F77..F96`, `CFG-F84..F91`,
`CONC-F92..F102`, `INV-F97..F100`, `SEC-F01..F04`, each with status and owner).
This file is the **todo list**; status there is the source of truth, status
here can lag the code.

## Principles (normative)

1. **Self-contained branches.** Each branch's finding(s) are fully resolved on
   that branch; nothing is "deferred", "pulled from", or "closed by" another
   in-flight branch. Every branch leaves `main` green: it compiles, passes
   `go build/vet/test/race`, and passes `make parity` + `make canary`.
2. **Dependencies are expressed by ORDER, never by partial work.** The only
   cross-branch seams are data/API, and each is owned by the branch that
   introduces it: `Order.UsedCoins` (B2), registry `PaymentAddress()` (B1).
   Downstream branches consume completed APIs already on `main`.
3. **Disjoint files between parallel branches.** Branches that can land in any
   order touch disjoint files.
4. **Fidelity over shortcuts.** Wire bytes and validation mirror the C++
   writers, never comments or stale header enums. Golden vectors come from the
   C++ writers, never from guesses.
5. **One branch off `main`, merged back after each.** No direct work on `main`.
   No pushes/PRs unless explicitly asked. Commit logic / gofmt / docs
   separately.

## Context

- **C++ reference:** Blocknet Core @ `ac930b7f8` (4.4.1 era), `src/xbridge/`.
- **Go subject:** `go-xbridge` @ `main`.
- **Audit home:** `docs/audit/` — `register.md` (canonical), `findings.md`
  (finding cards), `evidence/` (per-axis), `verify/` (subagent reports),
  `conformance/` (build-tagged Go suite, stage F of `make parity`).
- **Numbering:** all findings use axis-prefixed IDs (`RPC-F01…`). The
  ID-history appendix in `register.md` maps every prior-audit ID to its
  canonical home; nothing else references the old numbering.
- **Attacker model correction:** the hostile outcome of the never-established
  trust basis is **theft**, not recoverable lockup. Corrected in
  `register.md` (SEC-F03) — applied on B3.

## Branch set and ordering

| | Branch | Findings (register IDs) | Files (disjoint) | Depends on |
|---|---|---|---|---|
| B1 | `fix/servicenode-registry` | **WIRE-F71** | `p2p/servicenode/*` | — |
| B2 | `fix/wire-acceptingbody` | **CRYPTO-F84** (+ A1–A7 closeout, per-token D4) | `api/fee_tx.go` (new), `api/node.go`, `api/order.go`, `proto/body_test.go`, `api/hub_gate_test.go`, `api/store.go`, `api/persist.go` | B1 |
| B3 | `fix/deposit-path` | **CRYPTO-F85, CRYPTO-F86, CRYPTO-F87, SEC-F03**, **STATE-F71**, **CRYPTO-F78** (+ folded **CRYPTO-F90**, promoted **CRYPTO-F97** unit-scale) | `wallet/connector.go`, `wallet/rpc.go`, `wallet/local.go`, `api/swap.go`, `api/node.go`, `api/order.go`, `api/persist.go` | B2, B6 |
| B4 | `fix/http-hardening` | **RPC-F58**, **RPC-F01, F02, F47–F52** | `api/server.go`, `api/dispatch.go`, `cmd/main.go` | — |
| B5 | `fix/wire-hardening` | **WIRE-F57–F71** | `p2p/addr.go`, `p2p/envelope.go`, `p2p/conn.go`, `p2p/message.go`, `p2p/params.go`, `p2p/version.go`, `p2p/discovery/peer_manager.go`, `proto/packet.go`, `proto/body_types.go`, `p2p/servicenode/servicenode.go` | — |
| B6 | `fix/secrets-hygiene` | **SEC-F04** | `api/persist.go`, `wallet/rpc.go`, `api/swap.go` (log lines only) | — |
| B7 | `fix/rpc-surface` | **RPC-F03–F59** (+ re-decide F37) | `api/handlers.go`, `api/response.go`, `api/order.go`, `api/utxo_select.go`, `api/store.go`, `coins/amount.go`, `api/node.go` | B3, B4 |
| B8 | `fix/state-machinery` | **STATE-F72–F75** | `api/engine.go`, `api/store.go`, `api/response.go`, `api/node.go`, `swap/transaction.go` | B3 |
| B9 | `fix/crypto-connectors` | **CRYPTO-F77 (S1), F82, F83, F88, F91, F92** | `coins/tx.go`, `coins/cashaddr.go`, `coins/base58check.go`, `crypto/signer.go`, `api/handlers.go`, `api/utxo_select.go` | B3 |
| B10 | `fix/config-parity` | **CFG-F84–F91** | `config/conf.go`, `cmd/main.go`, `coins/coin.go`, `wallet/conf.go`, `api/node.go`, `api/handlers.go` | B4 |
| B11 | `fix/concurrency` | **CONC-F92–F94** | `api/node.go`, `api/engine.go`, `p2p/conn.go`, `p2p/discovery/peer_manager.go`, `log/dedup.go`, `api/persist.go` | B5 |

\* SEC-F03 (taker-trust) has no standalone code — the HTLC composition is sound
(`coins/htlc.go:45-54` matches C++). It is closed by **B3** as composite
acceptance: B1's verified hub + B3/CRYPTO-F85's validated deposit kill the
theft. B3 carries the end-to-end refusal test and corrects the attacker model
in the register.

**Deliberate/documented (no branch):** RPC-F19/F39 (Tier-3 data source),
WIRE-F65/F66 (hardening/doc), STATE-F76, CONC-F95/F96, CRYPTO-F79/F80/F81
(thin-client deliberate), INV-F97–F100, STATE-F77/F78, CONC-F97–F102,
SEC-F01, RPC-F59, CRYPTO-F84/F93–F96 (fixed; regression-covered). Each is
marked `FIXED`/`DOCUMENTED` in `register.md`.

**Status (2026-08-13):** B1 and B2 merged to `main` (WIRE-F71, CRYPTO-F84 +
A1–A7 + per-token D4). **B6 merged** (SEC-F04: `-persistsecrets` gate,
log-site removals, corrupt-file severity parity). **B3 merged** (deposit path:
CRYPTO-F85/F86/F87/F78/F90/F97, STATE-F71, SEC-F03 — validated-deposit gate,
native-unit scale, wire-Cancel). B4/B5, B7–B11 pending.

**Order:** `B1 → B2 → B3` sequential (real data dependencies). `B4 ∥ B5 ∥ B6`
anytime, but **B6 must merge before B3** (both touch `api/swap.go`). B2/B3 also
both touch `api/node.go`, hence sequential. B7 after B3/B4; B8/B9 after B3;
B10 after B4; B11 after B5 (p2p files) and B6 (persist).

```
B1 → B2 → B3 → B7 → B8 → B9
B4 ∥ B5 ∥ B6 (B6 before B3)
B10 after B4   B11 after B5+B6
```

## Per-branch done criteria (each branch, one session, until green)

1. **Diff brief first:** `remediation/<pkg>.md` — C++ source-of-truth lines
   → Go call sites → existing tests → golden-vector strategy. Durable
   cross-session memory.
2. Fix + tests; golden vectors from the C++ **writers**, never comments.
3. `gofmt` → `go build ./...`, `go vet ./...`, `go test ./...` → `go test -race`.
4. `make parity` + `make canary` from `tools/` (includes conformance stage F).
5. **Update `register.md` in the same branch:** move the IDs to
   `FIXED` (name the branch/test), update `README.md` ratings where
   needed, fix the attacker model where needed.
6. Commit logic / gofmt / docs separately. Merge to `main`.

## Per-branch spec

### B1 — `fix/servicenode-registry` — WIRE-F71 (critical) — MERGED

Retain + gate registration fields; `PaymentAddress()`; thin-client collateral
limit documented in `register.md`/`evidence/`. See
`remediation/B1-servicenode.md`.

### B2 — `fix/wire-acceptingbody` — CRYPTO-F84 (blocker) — MERGED

Funded `AcceptingBody`: atomic per-order reservation, p2pkh-25 funding filter,
same-order gate, A1–A7 closeout, per-token lock exclusion (D4). See
`remediation/B2-acceptingbody.md`.

### B3 — `fix/deposit-path` — CRYPTO-F85 + CRYPTO-F86 + CRYPTO-F87 + STATE-F71 + CRYPTO-F78 (+ CRYPTO-F90, CRYPTO-F97) — MERGED

- **C++ source:** `xbridgesession.cpp:2490-2515` (deposit validation),
  `:2077-2194` (maker), `:2625/2669/2711` (taker), `:1975, 2515-2530`
  (`xtx->usedCoins`), `:1993` (`minTxFee1(nIn,3)`), `:1401-1471,1750-1760`
  (Hold/Init re-verify), `:3920-4016` (redeem payout), `:2134/2149` (refund
  payout), `:3525-3576` (sendCancelTransaction).
- **CRYPTO-F85:** `CheckDepositTransaction` on `wallet.Connector` (interface +
  RPCConnector 1:1 port of `xbridgewalletconnectorbtc.cpp:1981-2194` +
  LocalConnector `ErrNoChainSource`); wired into `OnCreateB` (taker checks the
  maker's A deposit) and `OnConfirmA` (maker checks the taker's B deposit) with
  the C++ tri-state — wait → no reply (hub retransmits), bad → wire-Cancel
  (`crBadADepositTx`/`crBadBDepositTx`) + local rollback, good → record the
  validated out-params. MockRPC goldens + `TestCreateBBadDepositCancels` /
  `TestCreateBWaitsOnNotReadyDeposit`.
- **CRYPTO-F86:** `buildDeposit` signs → derives the local txid → pre-builds the
  CLTV refund → only then broadcasts (`TestDepositNotBroadcastWhenRefundFails`).
- **CRYPTO-F87:** maker `MakeOrder` records `Order.UsedCoins` (incl. autoSplit);
  `buildDeposit` consumes the `swapCtx.funding` snapshot, never `ListUnspent`
  (`TestDepositSpendsUsedCoins`).
- **CRYPTO-F78:** deposit fee `estimateFee(nIn, 3)`.
- **CRYPTO-F90:** the claim spends the VALIDATED deposit (exact `P2SHNative`
  value at `DepositVout`), paying `p2sh − fee2` so the redeemer keeps any excess;
  the refund pays the full nominal (fee2 implicit); `OBinTxVout/OBinTxP2SHAmount/
  OOverpayment` persisted (`TestRedeemCounterpartyPayout`, round-trip).
- **CRYPTO-F97 (promoted from planning):** deposit path now operates in native
  base units via `fromXBridgeAmt` at the boundary — for COIN≠1e6 (BTC) the
  on-chain deposit previously locked 100× too little (`TestDepositNativeScale`).
- **STATE-F71:** `OnHold`/`OnInit` re-verify amounts/price/identity against the
  order (intended-OR for Init — documented divergence from the C++ `&&` bug);
  state gate on duplicate Init (`TestHoldInitVerification`).
- **SEC-F03 closure:** composite acceptance — taker and maker refuse an
  unvalidated counterparty deposit end-to-end (`TestSecF03CompositeRefusal`);
  attacker model corrected in the register (theft, not lockup).
- **Verify:** as B1; `make parity` + `make canary`. Commits: one per ID + docs.

### B4 — `fix/http-hardening` — RPC-F58, RPC-F01/F02/F47–F52

- `authorized` returns true with no user configured (`api/server.go:78-80`);
  `SetAuth` silently disables on partial creds (`:62-68`); no
  method/content-type/host checks; `http.Server` has no timeouts
  (`cmd/main.go:236`). C++ is POST-only + always-authenticated
  (`src/xbridge/httprpc.cpp:148-234`).
- **RPC-F58:** add `http.Server` read/write/idle timeouts + hardening.
- **RPC-F01:** error-channel policy — param-count/type errors as envelope error
  (code −1) on the throw methods; strict parsers (`api/dispatch.go`).
- **RPC-F02:** `NO_SESSION` `name` = the handler's `__FUNCTION__`, not `"dx"`
  (`api/handlers.go:200,204`).
- **RPC-F47–F52:** HTTP status 500/404 vs 200; method-not-found text; 32 MiB
  body; always-auth model; batch/named params + `-32600`; arity gates → 1025.

### B5 — `fix/wire-hardening` — WIRE-F57–F71

- `ParseAddr` pre-bounds `make(…, n)` (`p2p/addr.go:33-43`); `readVarInt`
  uint64→int cast (`p2p/envelope.go:126`); no `recover` in discovery read loop
  (`peer_manager.go:280`); deadline cleared post-handshake (`p2p/conn.go:89`);
  payload caps (WIRE-F57 4,000,000; WIRE-F65 body cap policy); magic validation
  (WIRE-F58); `MIN_PEER_PROTO_VERSION` (WIRE-F59); regtest rename (WIRE-F60);
  `snl` receive (WIRE-F61); trailing-bytes reject (WIRE-F62); canonical varint
  (WIRE-F63); checksum log-and-drop (WIRE-F64); cmd2/cmd50 disposition
  (WIRE-F67/F68); getaddr policy + addr cap (WIRE-F69); handshake
  deadline/negotiation (WIRE-F70); WIRE-F71 already fixed on B1.

### B6 — `fix/secrets-hygiene` — SEC-F04 (high) — MERGED

Gate `PrivKey`/`Secret`/`RefundHex` behind `-persistsecrets` (default ON = C++
`orders.dat` parity; OFF zeroes them at write — the sole deliberate
divergence). Corrupt swap file logs at **Error** like C++ `loadOrders`'s `erro`
(continue-empty, never refuse to start) — this also satisfies "treat a corrupt
file as an error". Persist failures already log at Error + continue, matching
`saveOrders`'s ignored `xdb.Write` return. Dropped refund/claim hex and full
RPC bodies from debug logs (`api/swap.go:383,485,545,650`,
`wallet/rpc.go:129,138`). See `remediation/B6-secrets.md`. Tests:
`TestPersistSecretsOptOut`, `TestCorruptSwapFileContinuesLikeCpp`.

### B7 — `fix/rpc-surface` — RPC-F03–F59 (RPC S2/S3 set)

The RPC semantic pass: ordering (RPC-F03), boundary (RPC-F04), `uint256S`
parser (RPC-F05/RPC-F06), cancel ordering/codes (RPC-F07/RPC-F08), make-order
shape/dryrun/partial (RPC-F09–F11), message/name leaks (RPC-F12–F15),
order-history (RPC-F16–F19), order-book (RPC-F20–F22), balances
(RPC-F23–F25), my-orders/chain (RPC-F26–F30), locked-utxos (RPC-F31–F34),
flush-cancelled (RPC-F35/F36), trading-data (RPC-F38/F39), split
(RPC-F40–F43), getutxos (RPC-F44/F45), getnetworkinfo (RPC-F46 — re-decide),
tokens (RPC-F53/F54), new-address (RPC-F55), loadconf (RPC-F56), order-book
nesting (RPC-F57). Full per-finding detail: `register.md` + `findings.md`.

### B8 — `fix/state-machinery` — STATE-F72–F75

Wire the expiry sweep (`IsExpired`/`IsExpiredByBlockNumber`, `swap/
transaction.go:219-255`); port `TxCancelReason` enum + `TxCancelReasonText`
incl. the C++ rendering bugs; set `trRollbackFailed` on refund-broadcast
failure; add a peer penalty/ban analogue.

### B9 — `fix/crypto-connectors` — CRYPTO-F77, F82, F83, F88, F91, F92

BCH forkid `0x41` sighash + replay protection for the BCH connector local
signing path (`coins/tx.go`); full-range RNG retry (`crypto/signer.go`); block
hash byte-order conformance test.

### B10 — `fix/config-parity` — CFG-F84–F91

Whitelist `[Main]`/`[Rpc]` sections (startup kill); wallet-admission gates
(locktime/confirmation drift); missing-conf template; hot-reload semantics
(`ExchangeWallets` keying, gates, non-local order clearing); `ExchangeWallets`
separator set + validation; case sensitivity; CLI flags (`-enableexchange`,
`-dxnowallets`, version case); conf-key set alignment (`MinimumAmount`,
`CashAddrPrefix`, `CreateTxMethod`).

### B11 — `fix/concurrency` — CONC-F92–F94

Bounded async socket writes / background persist so the engine never blocks on
a peer or fsync; join discovery goroutines + stop Dedupe on Close; snapshot
connectors at task start (reload-mid-task race test).

## Register + audit.md hygiene

- `register.md` is the single status source; every branch moves its
  own IDs to `FIXED` and names the closing branch/test.
- `README.md` holds ratings + attacker model; B3 corrects the
  attacker model and closes SEC-F03 there.
- New divergences get a Current-register row (next free axis-prefixed ID) +
  a card in `findings.md`, never a duplicate status elsewhere.

## tools/ add-on (landed with the audit re-housing)

- `make conformance` runs the `go-xbridge/conformance` suite; `make parity`
  includes it as stage F. The suite's expected-fail `// DIVERGENT` rows turn
  red the moment a documented divergence is remediated (promote to strict).
- Replace the hardcoded parity fallback golden
  (`tools/parity/go/parity_value_test.go:568-575`) with a real captured
  `AcceptingBody` golden so the gate catches CRYPTO-F84-class regressions.

## Notes

- `make parity` is the objective "done" signal; `make canary` proves the gate
  is not silently green.
- No pushes or PRs unless explicitly asked; branch/merge locally.
- Carry-forward rule: when each branch lands, fold its diff-brief conclusions
  into `register.md` and check it off here.
