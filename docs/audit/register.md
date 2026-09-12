# Audit register — open work + fidelity standard

Tracks the divergences still open between Blocknet Core XBridge (C++) and
`go-xbridge`, and the identity standard every row is judged against.
Closed work is summarized, not listed: branches B1–B11 are all merged to
`main` (113 `FIXED`, 5 `IDENTICAL`, 4 `WAIVER` rows; per-branch detail lives
in git history — merge/closeout commits — not here). Rulings and standing
waivers with their rationale live in [`decisions.md`](decisions.md).

## §0 Fidelity standard (binding)

A swap sequence on go-xbridge must be identical to C++ in every observable:
wire bytes, state transitions, RPC shapes, error codes/messages, help texts,
fee/locktime math. Crash-restart durability ordering is excluded: a
disk-write-before-send is a local robustness addition with no wire/RPC effect.
No other deviation is accepted on that path.

Each open row resolves to exactly one terminal verdict:
- **fix queued** — real divergence with a remediation work item (owner: B12);
- **ruling pending** — open divergence awaiting an explicit ruling; the interim
  stance in `decisions.md` holds meanwhile;
- **waiver required** — unreachable for a thin client; decided explicitly,
  never by default. Standing waivers live in `decisions.md`: the trading-data
  family and the `dxGetTokenBalances` `Wallet` key.

## Status legend

| Status | Meaning |
|---|---|
| `FIX-QUEUED` | Real divergence with a remediation work item |
| `FIX-QUEUED + WAIVER` | Split row: part fix-queued, part waived |
| `RULING-PENDING` | Open divergence awaiting an explicit ruling; interim stance in `decisions.md` |
| `WAIVER` | Explicitly waived; reason in `decisions.md` |

Severities: **S1** wire/interop-breaking (MUST FIX) · **S2** observable
behavioral (MUST FIX) · **S3** edge-input divergence (FIX or rule) ·
**S4** cosmetic.

## Open rows (owner: B12)

| ID | Sev | Finding (one line) | Fix direction |
|---|---|---|---|
| RPC-F19 | S2 | `dxGetOrderHistory` source: fills store never written → always empty | FIX-QUEUED + WAIVER: record own fills at swap completion. Non-local fills need chain view: waived (trading-data family) |
| RPC-F24 | S3 | `dxGetTokenBalances` ticker order | RULING-PENDING: `Wallet`-first dropped by ruling (`decisions.md`: never synthesized); remaining question is whether Go sorted output is within C++'s observable range, since C++ connector insertion order is not a fixed cross-call contract |
| RPC-F27 | S2 | `dxGetMyPartialOrderChain` membership can differ | Residual: full-lineage walk must become the C++ utxo-count child-walk |
| RPC-F35 | S2 | `dxFlushCancelledOrders` age clock restarts at cancel | Residual: Go sets `Updated`=now at cancel; C++ leaves txtime — age-gated flushes diverge past age 0 |
| RPC-F63 | S3 | Business-vs-envelope classification parity per command unaudited | Command-by-command audit for B12: for each `dx*` method, classify C++ failure paths as throw (transport envelope) vs `makeError` vs `businessResponse` from the writers in `rpcxbridge.cpp` (not the header comments), then confirm Go returns the same shape on the same trigger |
| RPC-F60 | S3 | Bare `help` lists only `dx*` vs Core full command list | RULING-PENDING: mirror list vs waiver (`decisions.md`); thin-client-only list meanwhile |
| RPC-F61 | S3 | `getnetworkinfo` `localservices` bits reflect thin client, not full node | RULING-PENDING: bits report the reporting node by construction; waiver likely (`decisions.md`) |
| RPC-F62 | S3 | `dxFlushCancelledOrders` `use_count` always 1 vs shared_ptr refcount | RULING-PENDING: debug-only field with no Go equivalent; constant 1 meanwhile (`decisions.md`) |
| STATE-F80 | S2 | Never `finished` at claim; waits for hub `Finished` | Adopt C++ claim-completion locally; exclude claim-confirmed sessions from `scanRefunds` |
| STATE-F81 | S2 | Taker cannot complete without hub `ConfirmB`; no chain-watch claim | Port taker watch (spend-scan own deposit, learn payTx, derive secret, build claim) |
| STATE-F82 | S2 | No `processLater` retry queue; depends on hub retransmit | Bounded local retry queue for not-ready/build/broadcast failures |
| STATE-F83 | S3 | Claim broadcast lacks `ALREADY_IN_CHAIN`-as-success tolerance | Treat wallet already-in-chain as success, proceed to `Confirmed` (xbridgesession.cpp:4012-4024) |
| STATE-F84 | S2 | Init check is OR (reject any mismatch) vs C++ `&&` bug | Bug-for-bug `&&` required (xbridgesession.cpp:1750-1756). Risk: re-admits single-field mismatches C++ accepts — required for interop; amounts re-verified at deposit/claim. Needs explicit security sign-off (`decisions.md`) |
| STATE-F85 | S2 | Cancel writes `canceled` for deposit-sent orders vs C++ `trRollback` path | Route deposit-sent cancels through the rollback path (xbridgesession.cpp:3384-3420) |
| STATE-F86 | S3 | Refund sweep 60 s vs C++ 15 s timer | Tighten `refundCheckInterval` toward C++ `TIMER_INTERVAL` (xbridgeapp.cpp:90) |
| CRYPTO-F79 | S3 | Fee fallback: C++ 0 vs Go 2 sat/vB when FeePerByte unset | Fallback must be C++ 0 exactly (deposit fee math is on-chain visible) |
| CRYPTO-F81 | S3 | Address decoding strictness differs (base58check, cashaddr) | Match C++ `toXAddr` leniency exactly (swap-visible at make/take) |
| CRYPTO-F98 | S3 | PART connector non-portable (confidential outputs, amount-committing digest) | RULING-PENDING: `[PART]` refused at admission meanwhile (`decisions.md`) |
| CRYPTO-F99 | S3 | BCD connector non-portable (fork-version `preBlockHash` field) | RULING-PENDING: `[BCD]` refused at admission meanwhile (`decisions.md`) |
| CRYPTO-F102 | S3 | Dust source: live relay-derived (C++) vs 5460 fallback (Go) | RULING-PENDING: thin client has no relay feed; keep amounts well above dust meanwhile (`decisions.md`) |
| CFG-F92 | S3 | Wallet RPC timeout 30 s vs C++ 120 s `-rpcxbridgetimeout` | Honor the C++ timeout on slow-wallet paths (timeout error vs success is swap-visible) |
| CONC-F95 | S3 | Hides C++'s transient `accepting` window | `dxGetOrder` during a take must show `accepting`; preserve commit atomicity |
| WIRE-F65 | S3 | Go-only 1 MiB XBridge body cap (C++ has none) | RULING-PENDING: hardening stands; no legit swap body approaches it (`decisions.md`) |
| WIRE-F66 | S3 | cmd-4 dual writer (126 B vs 134 B trailing-`minFromAmount` forms) | RULING-PENDING: reader tolerates both; rejecting live C++ broadcasts is worse (`decisions.md`) |
| WIRE-F72 | S3 | Outbound SENDHEADERS/SENDCMPCT/pings never sent | RULING-PENDING: engine never acts on them; inbound pings answered (`decisions.md`) |

## Closed work (summary)

Branches B1 (servicenode-registry), B2 (wire-acceptingbody), B3
(deposit-path), B4 (http-hardening), B5 (wire-hardening), B6
(secrets-hygiene), B7 (rpc-surface), B8 (state-machinery), B9
(crypto-connectors), B10 (config-parity), B11 (concurrency) are merged to
`main`. Every row they owned is `FIXED` with its closing branch and test named
in the merge/closeout commits — `git log main --grep='B[0-9]'` is the index
(B1 predates the closeout-message convention: `d704ab4 fix(servicenode)`).
The pre-collapse per-row table (closing test per canonical ID) is preserved
in git history, not here; the ID-history appendix below maps legacy IDs to
canonical IDs with their closing branches. RPC-F23 (`Wallet` key) closed by
ruling instead of code (`decisions.md`). The
byte-level parity proof lives in the test suite (`conformance/` stage F plus
per-package goldens: BIP143/forkid vectors, sighash oracles, dust/fee
goldens, TTL/state-enum vectors), not in prose.

## How to maintain

- **Open a new divergence?** Add a row above with the next free
  axis-prefixed ID (`RPC-F..`, `WIRE-F..`, `STATE-F..`, `CRYPTO-F..`,
  `CFG-F..`, `CONC-F..`, `INV-F..`, `SEC-F..`).
- **Fix one?** Delete the row in the same branch that fixes the code. No
  `FIXED` section is kept here — green tests are the record.
- **Think it's acceptable as-is?** It isn't — §0 forbids accepted gaps. Either
  prove it identical (name the proof test) or file it fix-queued; only rulings
  recorded in `decisions.md` are exempt, and new rulings need an explicit entry
  recorded there. (Deliberate exception: DCR/STEALTH/XST refusals stay rowless
  per `decisions.md`.)

## ID-history appendix

Every prior-audit ID (`F1–F27`, `S2/S3/S4` series, `S1-A…D`) resolves to
exactly one canonical ID. This appendix exists for archaeology; the live
namespace is the open-rows table above.

| Legacy | Canonical | Notes |
|---|---|---|
| F1 | SEC-F01 | RPC loopback bind — FIXED |
| F3 | CONC-F97 | `SwapSession` single-owner — FIXED |
| F4 | CONC-F98 | coin registry atomic reload — FIXED |
| F5/F9 | CONC-F99 | bounded growth — FIXED |
| F6 | CONC-F100 | no wallet I/O under lock — FIXED |
| F7 | SEC-F02 | inbound UTXO proofs unverified — FIXED (B8) |
| F8 | CRYPTO-F88 | segwit dead code — FIXED (B9) |
| F10 | RPC-F49 | 4 MiB body cap — FIXED (B4 `fix/http-hardening`: 32 MiB `rpcMaxBodyBytes`, non-envelope 413) |
| F11–F14 | INV-F100 | vestigial helpers — FIXED |
| F15 | CONC-F101 | live `*Order` race — FIXED |
| F16 | STATE-F77 | post-completion retransmit — FIXED |
| F17 | CONC-F92 | blocking I/O on engine goroutine — FIXED (B11) |
| F18 | CONC-F102 | force-refund double-broadcast — FIXED |
| F19 | CRYPTO-F84 | AcceptingBody empty fee/utxos — FIXED (B2) |
| F20 | WIRE-F71 | registration integrity — FIXED (B1) |
| F21 | CRYPTO-F85 | `checkDepositTransaction` absent — FIXED (B3 `fix/deposit-path`) |
| F22 | SEC-F03 | taker-trust composite — FIXED (B3 `fix/deposit-path`) |
| F23 | CRYPTO-F86 | `buildDeposit` broadcast order — FIXED (B3 `fix/deposit-path`) |
| F24 | RPC-F58 | HTTP auth/timeout hardening — FIXED (B4 `fix/http-hardening`) |
| F25 | WIRE-F57/F63 | P2P addr/varint DoS — FIXED (B5 `fix/wire-hardening`) |
| F26 | SEC-F04 | plaintext secrets — FIXED (B6 `fix/secrets-hygiene`) |
| F27 | CRYPTO-F87 | `usedCoins` vs `ListUnspent` — FIXED (B3 `fix/deposit-path`) |
| S1-A | CRYPTO-F93 | ownership-proof challenge stream — FIXED |
| S1-B | CRYPTO-F94 | deposit `SEQUENCE_FINAL` — FIXED |
| S1-C | CRYPTO-F95 | deposit locks `Amount + fee2` — FIXED |
| S1-D | CRYPTO-F96 | `nTime` sighash on time-field coins — FIXED |
| S2-A | RPC-F57 | order-book detail-4 nesting — FIXED (B7) |
| S2-B | RPC-F59 | partial-chain unknown/malformed id — FIXED |
| S2-C | RPC-F40 | `dxSplitInputs` utxo schema — FIXED (B7) |
| S2-D | STATE-F72 | expiry sweep unwired — FIXED (B8: 15 s prune, persist 60 s, BlockNumber stamp) |
| S2-E | STATE-F78 | hub-key pinning, no TOFU — FIXED |
| S2-H | CRYPTO-F81 | base58check strictness — FIX-QUEUED (C++ leniency required, §0) |
| S2-I | CRYPTO-F77 | BCH forkid sighash — FIXED (B9, fork value 0xffdead) |
| S2-J | CRYPTO-F89 | coin-family misclassification — FIXED (B9: BTG/DEVAULT; PART/BCD deferred) |
| S2-K | CFG-F84 | `[Rpc]` aborts startup — FIXED (B10) |
| S2-L | CFG-F85 | admission gates absent — FIXED (B10: `config.Admit`) |
| S2-M | CFG-F86 | missing-conf template — IDENTICAL (startup posture, zero swap effect; never-creates stance) |
| S2-N | CFG-F87 | hot-reload semantics — FIXED (B10) |
| S3-A | RPC-F11 | partial fields `"0"` literal — FIXED (B7) |
| S3-B | RPC-F44 | `dxGetUtxos` amounts trimmed — FIXED (B7) |
| S3-C | RPC-F31 | locked-utxos amount format — FIXED (B7) |
| S3-D | RPC-F28 | `p2sh_deposits` alignment — FIXED (B7) |
| S3-E | RPC-F55 | new-token-address `[]` vs error — FIXED (B7) |
| S3-F | CRYPTO-F79 | fee fallback 0 vs 2 sat/vB — FIX-QUEUED (fallback must be C++ 0); CRYPTO-F80 dust key — FIXED (B10: `MinimumAmount`) |
| S3-G | CRYPTO-F90 | payout model (fee2 margin) — FIXED (B3 `fix/deposit-path`) |
| S3-H | CRYPTO-F91 | `signrawtransaction` payload — FIXED (B9) |
| S3-I | CRYPTO-F92 | secret-from-payTx input scan — FIXED (B9) |
| S3-J | STATE-F79 | `tryJoinMatches` min-size guards — FIXED (B8) |
| S3-K | CFG-F88 | `ExchangeWallets` parsing — FIXED (B10) |
| S3-L | CFG-F89 | case-insensitive keys — FIXED (B10: exact-case) |
| S3-M | CFG-F90 | CLI flags — FIXED (B10: `-dxnowallets`/`-enableexchange`) |
| S3-N | CFG-F91 | conf-key set alignment — FIXED (B10) |
| S3-O | STATE-F73 | cancel-reason enum/text — FIXED (B8) |
| S3-P | STATE-F74 | `trRollbackFailed` unset — FIXED (B8) |
| S3-Q | STATE-F75 | no peer penalty — FIXED (B8) |
| S3-R | SEC-F02 | inbound UTXO proofs unverified — FIXED (B8) |
| S2-S | CONC-F92 | blocking I/O on engine goroutine — FIXED (B11) |
| S3-S | CONC-F93 | goroutine joins / Dedupe sweeper leak — FIXED (B11) |
| S3-T | CONC-F94 | reload mid-swap-task — FIXED (B11) |
| S4-D | INV-F98 | `Created` doc µs — FIXED (B11 doc) |
| S4 | RPC-F30/F36, WIRE-F57 | +1/COIN, help text, 64 MiB cap — re-triaged under §0 |
