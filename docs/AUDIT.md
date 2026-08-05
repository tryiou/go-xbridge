# XBridge C++ ↔ Go Port — Consolidated Audit

Single authoritative audit register for the `go-xbridge` port of the Blocknet C++
XBridge engine. This document **supersedes** the former
`docs/audit-dx-equivalence.md` (merged here) and is the canonical record of
fidelity, divergences, and outstanding work. It is a **living register**: status
here can lag the code; the code is authoritative.

## Reference pins

- **C++ (reference):** `blocknet_core` @ `41bd02e45` (2023-10-25 era),
  `src/xbridge/` + `src/protocol.h`, `src/net_processing.cpp`, `src/chainparams.cpp`,
  `src/version.h` (`XBRIDGE_PROTOCOL_VERSION = 55`, `PROTOCOL_VERSION = 70713`).
- **Go (subject):** `go-xbridge` @ HEAD (`main`, module `go-xbridge`, Go 1.25+).
- **Method:** comparative read-only audit by 6 subagents (wire/transport, RPC
  surface, swap state machine, config/coins/crypto/wallet, code-quality/security,
  docs-claims) + orchestrator re-verification of **every S1 finding** against the
  C++ writers. Baseline: `go build`, `go vet`, `go test`, `go test -race`, gofmt —
  all clean; 212 tests, hermetic (no live-network dials).

## Severity taxonomy

- **S1** — Wire/interop-breaking: a C++ node rejects or misreads Go data (order,
  deposit, signature) → swap cannot complete. MUST FIX.
- **S2** — Protocol-semantic / behavioral: identical bytes but different meaning,
  ordering, validation, or a clear JSON-shape/behavior mismatch vs C++. MUST FIX.
- **S3** — Behavioral/config/thin-client divergence. FIX or document as
  deliberate.
- **S4** — Cosmetic / doc / strictness.

## Executive summary (2026-08 audit)

The **wired contract is byte-for-byte faithful**: Bitcoin P2P framing, the
`xbridge` transport envelope, the 129-byte packet header, all 21 command enums,
all 19 body layouts (validated against a live `DOGE→BLOCK` capture), the
121-byte UTXO-entry encoding, and the signing digest all match the C++ writers.
The `dx*` JSON-RPC surface is largely at parity (error codes, dates, amounts,
status strings, envelope) but the "all 23 remediated" claim is **overstated** —
several response/behavior divergences remain (see the matrix). The swap **state
machine** (join, two-confirmation gate, drift check, locktimes, HTLC script
bytes) is faithful, but the **deposit construction path has three S1 interop
breakers** that, as built, would cause a C++ counterparty/hub to reject a
Go-created order or deposit. Separately, production readiness is blocked by an
unauthenticated, all-interfaces RPC and two untested data races.

**Bottom line:** the port can **connect** to the live network (wire-faithful),
but **cannot yet trade** with C++ peers (S1 deposit defects), and is not
production-safe until the auth + concurrency issues are fixed.

## Per-area verdicts

| Area | Verdict | Worst severity |
|---|---|---|
| SA-1 Wire packet header & transport | FAITHFUL (byte-for-byte; live-capture-validated) | — |
| SA-2 Command & TxCancelReason enums | FAITHFUL | — |
| SA-3 Packet body serialization | FAITHFUL (all live commands) | — |
| SA-4 Swap state machine & lifecycle | FAITHFUL (machine) / DIVERGENT (deposit execution) | **S1** |
| SA-5 Config (`xbridge.conf`) contract | FAITHFUL (superset of C++ keys) | S3 |
| SA-6 Fee & dust math | MINOR DIVERGENCE (thin-client defaults) | S3 |
| SA-7 Crypto & signatures | FAITHFUL except base58check strictness + BCH forkid / nTime sighash | **S1** |
| SA-8 RPC/API surface | PARITY except S2 shape/behavior + S3 value-level items | S2 |
| SA-9 Code quality & security | NOT production-ready (auth, races, growth) | S1-security |

---

## Ranked divergence list (consolidated, incl. fresh 2026-08 findings)

### S1 — Wire / interop-breaking (MUST FIX)

**S1-A. UTXO ownership-proof challenge string differs (base units vs whole-coin).** — **FIXED** (see C13). Go now formats the whole-coin `double` the wallet reports (listunspent `"value"`) with `strconv.FormatFloat(v, 'g', 6, 64)`, byte-identical to C++ `UtxoEntry::toString()`'s default `ostringstream` (defaultfloat, precision 6); golden vectors generated from the real g++ output are asserted in `TestWholeCoinOstreamMatchesCppStream`.

**S1-B. Deposit input sequence.** Go `swap/deposit.go:128` sets
`Sequence: 0xfffffffe` on deposit inputs (CLTV-enabling). C++ builds deposits
via `createRawTransaction(..., cltv=true)` which stamps
`sequence = SEQUENCE_FINAL (0xffffffff)` (`xbridgerpc.cpp:949-950`, called from
`xbridgewalletconnectorbtc.cpp:2379-2382`), and `checkDepositTransaction`
hard-rejects any deposit input with `sequence != 0xffffffff`
(`xbridgewalletconnectorbtc.cpp:2076-2080`). The non-final sequence belongs on
the *refund* tx, not the deposit. Go deposits are rejected before the swap begins.

**S1-C. Deposit value lacks the `fee2` redeem margin.** Go locks exactly
`d.Amount` (`swap/deposit.go:131`). C++ locks `outAmount + fee2`
(`xbridgesession.cpp:2094` maker, `:2615` taker) and `checkDepositTransaction`
requires `depositP2SHAmount >= amount + 0.95*fee2`
(`xbridgewalletconnectorbtc.cpp:2183`). Go deposits fail this check.

**S1-D. `nTime` omitted from the sighash preimage on `TxWithTimeField` coins.**
Go `coins/tx.go:353-392` `HashForSigning` never serializes `TxTime` even though
`Serialize()` stamps it (`tx.go:113-119`). C++
`CTransactionSignatureSerializer::Serialize` **includes** `nTime` when
`serializeWithTimeField` (`xbitcointransaction.h:265-269`). On any time-field
chain the refund/claim signature Go computes is over the wrong preimage and is
rejected. Default coins (BTC/LTC, flag off) unaffected.

> **S1-B…D together:** a Go participant cannot complete a swap against a C++
> peer as built — orders are rejected on the proof, deposits are rejected on
> sequence + value, and time-field chains additionally get invalid signatures.

### S2 — Protocol-semantic / behavioral (MUST FIX)

- **S2-A. `dxGetOrderBook` detail-4 nesting.** Go emits
  `[[price, amount, [ids…]]]` (`api/handlers.go:708-715`); C++ emits flat
  `[price, amount, [ids…]]` (`rpcxbridge.cpp:1902-1926`). JSON shape differs;
  per-side `max_orders` caps are correct.
- **S2-B. `dxGetMyPartialOrderChain` unknown/empty id.** Go returns
  `TRANSACTION_NOT_FOUND` (`api/handlers.go:825-827`); C++ returns `[]`
  (`rpcxbridge.cpp:2276-2324`). Malformed-id error also differs.
- **S2-C. `dxSplitInputs` utxo-parameter schema.** Go requires `amount` in each
  utxo object and does not resolve it from the wallet (`api/handlers.go:1154,
  1461-1480`); C++ utxo objects carry `txid`+`vout` only and resolve amounts via
  `getTxOut` (`rpcxbridge.cpp:3313-3315`). A C++-contract caller gets `Amount=0`
  → spurious "insufficient funds". (Param *count* parity — exact-7 — is correct.)
- **S2-D. Expiry sweep not wired into production.** `IsExpired`/
  `IsExpiredByBlockNumber` (`swap/transaction.go:219-255`) are implemented and
  unit-tested but **never invoked** by the daemon. C++ runs
  `eraseExpiredTransactions` on a timer (`xbridgeexchange.cpp:712-747`). Go only
  filters the store in `dxGetOrders` and relies on the hub to drop stale orders,
  so expired-status behavior is inert.
- **S2-E. Swap-handshake inbound packets not authenticated against a trusted hub
  key.** `api/node.go:544` verifies inbound packets only against the self-asserted
  header pubkey; `dispatchSwap` (`api/node.go:663-701`) routes Hold/Init/CreateA/B/
  ConfirmA/B/Finished to live sessions without re-verification. A peer can forge
  `CreateA/B` (triggers a real deposit broadcast) and a forged `Finished` sets
  `csFinished`, **disabling the auto-refund safety-net** (`api/swap.go:473`).
  Cancel/reject are correctly re-verified against trusted keys
  (`api/node.go:1172-1206`).
- **S2-F. Data race on live `SwapSession` fields.** Handlers mutate `state`,
  `ourDepositTxID`, `refundHex`, etc. on the feed goroutine **without** `sessMu`
  (`api/swap.go:545-636`, lock released at `api/node.go:664-666`) while the
  refund-watcher (`api/swap.go:469-494`) and persist read under it. Untested by
  `-race` (watcher never runs in tests).
- **S2-G. Data race on the package-global `coins.Coins` map on hot-reload.**
  `coins.InitFromConf` reassigns the map (`coins/coin.go:62-72`) without
  synchronization, invoked by `dxLoadXBridgeConf → reloadConf` (`api/node.go:280`)
  while other goroutines call `coins.Get` (`coins/coin.go:162`) — possible
  "concurrent map read and map write" daemon crash.
- **S2-H. base58check decode strictness** (prior S2-A). Go hard-rejects a
  mismatched version byte (`coins/address.go:107-114`); C++ `toXAddr` strips the
  version byte blindly (`xbridgewalletconnectorbtc.cpp:1547-1555`). Valid-input
  payloads are identical; kept as deliberate hardening.
- **S2-I. BCH `SIGHASH_ALL|FORKID` missing in Go local signing.** C++ signs BCH
  refunds with forkid (`xbridgewalletconnectorbch.cpp:396`) and payments
  conditionally (`:454`); Go local signing lacks forkid. (Devault payments would
  match Go but Devault is separately broken by S2-J.)
- **S2-J. Coin-family misclassification / missing connectors.** DEVAULT is
  misclassified as the BTC family; DCR, PART, BTG adapters are missing. DCR/PART
  are core network coins → trading on them is impossible.

### S3 — Behavioral / config / thin-client (FIX or document)

- **S3-A. `dxMakeOrder` partial fields.** C++ emits literal `"0"`
  (`rpcxbridge.cpp:1061-1063`); Go emits `"0.000000"` (`api/order.go:204-206`).
- **S3-B. `dxGetUtxos` `amount` format.** C++ renders fixed **8-decimal**
  whole-coin (`rpcxbridge.cpp:3483`, e.g. `"3.26211780"`); Go trims trailing
  zeros (`api/handlers.go:1536`, `coins/amount.go:79-80`, `"3.2621178"`).
- **S3-C. `dxGetLockedUtxos`.** Per-order key: C++ uses `a_and_b_currency` for
  accepted orders (`rpcxbridge.cpp:2674-2677`); Go always uses `FromCurrency`
  (`api/handlers.go:1042`). Per-UTXO amount strings also differ (whole-coin
  double at 6-sig-fig vs Go formatted).
- **S3-D. `dxPartialOrderChainDetails` `p2sh_deposits` alignment.** C++ emits one
  entry per order incl. `""` (`rpcxbridge.cpp:2455-2457`); Go omits empty entries
  (`api/handlers.go:938-943`) → element misalignment with `orders`.
- **S3-E. `dxGetNewTokenAddress` wallet failure.** C++ silently returns `[]`
  (`rpcxbridge.cpp:184-192`); Go returns an `UNKNOWN` error
  (`api/handlers.go:206-208`).
- **S3-F. Fee/dust thin-client substitutions.** `FeePerByte==0` → 2 sat/vB
  (`api/handlers.go:1370-1374`; C++ `xbridgewallet.h:114` default 0 → fee 0); a
  Go-only `DustAmount` conf tier (`api/handlers.go:1349-1357`) where C++ always
  uses `0.546*relayFee*COIN` else 5460 (`xbridgewalletconnectorbtc.cpp:1526`).
  Both collapse to C++ values for real conf inputs; documented deliberate.
- **S3-G. Refund/payment payout model.** Go pays `amount − fee`
  (`api/swap.go:666,723`); C++ refund pays the full deposit outAmount funded by
  `+fee2` (`xbridgesession.cpp:2149`) and payment pays `amount + oOverpayment`
  (`:3971`). Go leaves the `fee2` redeem margin on the table (economic, not
  correctness).
- **S3-H. `signrawtransaction` param payload.** Go passes `[txHex, prevTxs,
  "ALL"]` (`wallet/rpc.go:286`); C++ passes `[rawtx, prevtxs, privkeys]` with no
  sighash-type (`xbridgerpc.cpp:539-570`). "ALL" lands in the privkeys slot of
  the legacy RPC (→ throws, then the modern fallback succeeds); net effect
  equivalent on modern wallets.
- **S3-I. `secretFromScriptSig` requires a 33-byte push** (`api/swap.go:834`);
  C++ `getSecretFromPaymentTransaction` matches any push (`xbridgewalletconnectorbtc.cpp:2263-2270`).
- **S3-J. `tryJoinMatches` adds partial-order min-size guards**
  (`swap/transaction.go:103-108`) not yet confirmed against C++ `tryJoin`.

### S4 — Cosmetic / strictness / docs

- JSON key emission order differs from C++ (documented, non-semantic).
- `formatXAmount` omits C++'s `+1/::COIN` (1e-8) round-up — below the 6-decimal
  render floor, unobservable.
- Param-leniency on commands C++ rejects with error: `dxGetLocalTokens`,
  `dxGetNetworkTokens`, `dxGetTokenBalances`, `dxGetMyOrders`, `dxLoadXBridgeConf`,
  `dxGetNewTokenAddress`, extra params on `dxGetOrderFills`.
- `dxGetMyOrders` `Mine()` omits history-only local finished/cancelled orders
  (`api/store.go:134` vs C++ `rpcxbridge.cpp:2111-2120`).
- `MaxPayloadSize` 64 MiB vs C++ `MAX_SIZE` 32 MiB; Go does not enforce C++'s
  per-command min-size checks on read (receive-side leniency); Go doesn't re-stamp
  the header timestamp or run `addToKnown`/hash dedup (application-level).
- Stale doc line refs and the wrong prior S1-A/S3-D notes (see "Resolution
  status" below).

---

## `dx*` RPC equivalence matrix

`DONE` = behaviorally 1:1 with the C++ writer. `DIVERGENCE` = real remaining
mismatch (row note). `TIER3` = intentionally divergent thin-client limit
(document [`api.md`](api.md)). `GO-ONLY` = no C++ `dx*` counterpart.

| Command | Verdict | Note |
|---|---|---|
| dxGetOrders | DONE | conf/connector filter + id-sorted |
| dxGetOrder | DONE | `NO_SESSION` gate; case-insensitive id |
| dxGetMyOrders | PARITY-NOTE | S4: omits history-only finished/cancelled |
| dxGetOrderBook | **DIVERGENCE** | S2-A detail-4 nesting; per-side caps OK |
| dxGetOrderFills | PARITY-NOTE | S4: param leniency (C++ rejects size∉{2,3}) |
| dxGetMyPartialOrderChain | **DIVERGENCE** | S2-B unknown-id → error vs `[]` |
| dxPartialOrderChainDetails | PARITY-NOTE | S3-D `p2sh_deposits` alignment |
| dxGetLockedUtxos | **DIVERGENCE** | S3-C per-order key + amount format |
| dxFlushCancelledOrders | DONE | age 0/1/else-`-1`; underflow clamped |
| dxGetLocalTokens | PARITY-NOTE | S4: accepts params C++ rejects |
| dxGetNetworkTokens | TIER3 | servicenode-registry-driven (C10); P2P-bounded |
| dxMakeOrder | PARITY-NOTE | S3-A partial fields `"0"` vs `"0.000000"` |
| dxMakePartialOrder | DONE | `order_type="partial"`; trailing params |
| dxTakeOrder | DONE | full-take on amount=0; self-trade guard; dryrun |
| dxCancelOrder | DONE | `state>=trCreated` guard; `refund_tx` |
| dxLoadXBridgeConf | PARITY-NOTE | S4: hot-reload; last-good on failure |
| dxGetNewTokenAddress | PARITY-NOTE | S3-E wallet-failure → error vs `[]` |
| dxGetTokenBalances | PARITY-NOTE | S4 params; `Wallet` key present only when a connector loaded |
| dxGetUtxos | **DIVERGENCE** | S3-B amount trailing-zero trim; locked set Tier-3 |
| dxSplitAddress | DONE | 8-field object; 1e6-scale amounts |
| dxSplitInputs | **DIVERGENCE** | S2-C utxo schema (param-count parity OK) |
| dxGetOrderHistory | TIER3 | OHLCV buckets from session-local fills |
| dxGetTradingData | TIER3 | 8-field local record (`fee_txid`/`nodepubkey` empty); C++ `gettradingdata` not exposed |
| getNetworkInfo | GO-ONLY | Go-only extension |

## Cross-cutting substrate (remediated — verified at HEAD)

- **C1 — decimals.** `formatXAmount`/`formatXPrice` render **6 decimals**
  (`xBridgeSignificantDigits(1_000_000)==6`), not 7.
- **C2 — error `code`.** C++ 1000-range enum (`xbridgeerror.h`): `UNKNOWN=1002`,
  `BAD_REQUEST=1004`, `NO_SESSION=1018`, `INSUFFICIENT_FUNDS=1019`,
  `TRANSACTION_NOT_FOUND=1021`, `INVALID_PARAMETERS=1025`, `INVALID_ADDRESS=1026`,
  `INVALID_STATE=1028`, `NOT_EXCHANGE_NODE=1029`.
- **C3 — error `error` text.** `xbridgeErrorText(code, arg)` per-code prefix.
- **C4 — param coercion.** Present-but-unparseable required params error with the
  correct code.
- **C5 — envelope.** JSON-RPC 1.0 (`result`/`error`/`id` only, compact);
  business errors in `result`; panic → `-32603`.
- **C6 — inbound packet signature verification** before any state mutation
  (`api/node.go:544`; mirrors `xbridgesession.cpp:736`).
- **C7 — remote cancel/reject wire plumbing** (`onRemoteCancel`/`onRemoteReject`,
  `xbridgesession.cpp:3288-3485`) with signature gates against trusted keys.
- **C8 — lockTime drift check** (`api/locktime.go`; `xbridgewalletconnectorbtc.cpp:2331`,
  call site `xbridgesession.cpp:2464`).
- **C9 — dust/fee fidelity** — conf `DustAmount` tier + `5460` constant + `MinTxFee`
  floor; collapses to C++ for real conf.
- **C10 — network-token source** — `p2p/servicenode.Registry` from
  `SNREGISTER`/`SNPING`/`SNLISTPING` (still Tier-3 bounded).
- **C11 — empty order-book arrays** as `[]` (default-constructed `Array`).
- **C12 — server robustness** — panic-safe RPC envelope, rotating file logging,
  datadir pre-create.
- **C13 — UTXO ownership-proof challenge = C++ `UtxoEntry::toString()`**
  (S1-A). `wallet.Utxo.Value` preserves the whole-coin `listunspent` `"value"`
  double; `api.utxoChallenge` streams it via `wholeCoinOstream`
  (`strconv.FormatFloat(v, 'g', 6, 64)` = default `ostringstream`, precision 6),
  byte-identical to the C++ writers; golden vectors from real g++ output are
  asserted in `TestWholeCoinOstreamMatchesCppStream`.

## Tier 3 — architectural limits (thin-client, cannot fully match C++)

Documented once, not silently divergent — see [`api.md`](api.md) "Tier 3":
`dxGetOrderHistory`/`dxGetTradingData` session-local fills,
`dxGetNetworkTokens` P2P-bounded, and the locked-set limits. The TIER3 rows in
the `dx*` matrix above point at the same section.

---

## Resolution status of the prior audit (re-verified at HEAD)

### Fixed / verified

- **S3-A — empty `JSONVersion`/`ContentType` defaults.** VERIFIED.
  `config/conf.go:278-279` defaults `""`/`""`; `wallet/rpc.go` omits the
  `jsonrpc` field (`omitempty`) and Content-Type when empty; C++ `xbridgeapp.cpp:995-996`.
- **S3-G — wallet RPC shapes.** VERIFIED for `listunspent` (empty `[]` params +
  client-side filter, `wallet/rpc.go:212`) and the legacy-first
  `signrawtransaction` → `signrawtransactionwithwallet` fallback
  (`wallet/rpc.go:289-293`). Note: the 3rd-param `"ALL"` payload drift remains
  (S3-H).
- **S3-E — `dxSplitInputs` param count.** VERIFIED NO-DEVIATION. C++ help says
  3–7 but the body reads `params[3..6]` unconditionally and throws on missing —
  only 7 works; Go's exact-7 matches.
- **S3-F — per-side order-book caps.** VERIFIED NO-DEVIATION (C++ caps bids/asks
  independently). Detail-4 nesting is still wrong (S2-A).
- **S2-B (old) — finished-state block-height rule.** VERIFIED in code
  (`swap/transaction.go:241-256` ⇄ `xbridgetransaction.cpp:288-311`) — but **dead
  in production** (see S2-D: the expiry sweep is never invoked).
- **S3-D (old) — display round-up.** NO-DEVIATION conclusion correct, but the
  doc's `setprecision(7)` number was wrong: true precision is **6**
  (`xBridgeSignificantDigits(1_000_000)==6`); the `+1/::COIN` (1e-8) bump never
  renders.

### Corrected (the prior record was wrong)

- **S1-A (old) — `version` net_addr size.** The original severity-1 "finding"
  (26-byte version addrs are wire-breaking) was a **false positive**, and the
  recorded "fix" to **30 bytes** (`88c2d30`) was itself a **regression**, since
  reverted (`a2f0ceb`). Current code and tests are **26-byte**, which is **correct**:
  C++ serializes the `version` addrs at `INIT_PROTO_VERSION` (209 <
  `CADDR_TIME_VERSION` 31402) on both send (`net_processing.cpp:210`) and receive
  (`net.cpp:569`), so no per-addr `nTime` is written. `protocol.md` §1.3.1 is the
  accurate record; the 30-byte form applies only to `addr`/`getaddr` records.
- **P1 (old) — UTXO ownership-proof challenge string "fixed".** The prior
  "fixed" claim was **REFUTED** by S1-A (Go signed base units where C++ signs
  whole-coin); S1-A is now **actually fixed** — see C13.

### Deliberate thin-client items (explicitly NOT bugs)

- `gettradingdata` alias removed; `getnetworkinfo` Go-only extension.
- `dxGetOrderHistory`/`dxGetTradingData`/`dxGetNetworkTokens`/locked-set limits
  (Tier 3).
- `LocalConnector` local-key signing (no C++ counterpart).
- `FeePerByte==0 → 2 sat/vB`; `DustAmount` conf tier (S3-F).
- base58check strictness (S2-H, kept).
- `xbcServicesPing`/`xbcXChatMessage` typed bodies (C++ has no live writer;
  servicenode messages parsed by `p2p/servicenode`).

---

## Security & production-readiness findings

| # | Finding | Severity |
|---|---|---|
| F1 | **Unauthenticated, network-exposed RPC.** `cmd/xbridged/main.go:67` defaults `-rpcaddr` to `:41414` (all interfaces); `api/server.go:47-97` has no auth. Any host can call `dxMakeOrder`/`dxTakeOrder`/`dxCancelOrder` and drive connected wallets. Upstream blocknetd is auth-gated. | S1-security |
| F2 | Swap-handshake packets verified only against self-asserted pubkey, not a trusted hub key → forged `CreateA/B` deposits and forged `Finished` disables the auto-refund watcher (S2-E). | S2 |
| F3 | Data race on `SwapSession` fields (S2-F). | S2 |
| F4 | Data race on global `coins.Coins` map on hot-reload (S2-G). | S2 |
| F5/F9 | `sessions` / `Store.fills` / `Store.history` grow without bound (memory + persisted JSON). | S3 |
| F6 | `sessMu` held across wallet RPC I/O in `checkRefunds` (stalls packet dispatch up to 30 s). | S3 |
| F7 | Inbound order UTXO ownership proofs are never verified before the order is put on the book. | S3 |
| F8 | Segwit/BIP143 signing is dead code w.r.t. the daemon; bech32 destinations re-encoded as legacy P2PKH. | S3 |
| F10 | HTTP RPC has no request-body size limit. | S3 |
| F11–F14 | Vestigial `Server.verify`, unused `coins.MustGet`, tested-but-unreferenced `swap` package, `LocalConnector.SignMessage`/`VerifyMessage` unsupported (test-only). | S4 |

**Attacker model:** inbound signature verification *is* enforced before state
mutation, but against a self-declared key. Combined with automatic peer
discovery and no rate limiting, an attacker can pollute the book, trigger real
deposits (fund lockup, recoverable via refund), and forge `Finished` to strand a
deposit. HTLC semantics (ELSE-branch needs the secret; refunds pay the
depositor's own address) prevent direct **theft** — worst case is lockup +
fee-burn + state corruption + DoS. `-race` green does **not** cover F3/F4 (the
watcher/reload paths are untested).

---

## Verification gaps still open

- Live-hub verification of the swap-handshake claim/refund spends (in-memory
  connectors only today; `cmd/liveprobe` dials + handshakes but does not drive a
  swap).
- Byte-level capture of `OrderBody`/`AcceptingBody`/`CancelBody` UTXO-entry
  encoding vs a live C++ node (currently validated on one captured packet).
- Cross-check `p2p/seeds.go` fixed-IP/DNS seeds against `chainparamsseeds.h`.
- Confirm S1-B/C against a real C++ hub (`checkDepositTransaction` behavior
  with a live deposit).

## Ratings (2026-08 audit)

| Area | Rating |
|---|---|
| Wire protocol & transport fidelity | 9 / 10 |
| RPC / `dx*` surface parity | 7 / 10 |
| Swap state machine (machine/scripts) | 8 / 10 |
| Swap deposit/execution path | 6 / 10 (S1-B…D) |
| Config / coins / crypto / wallet | 7 / 10 |
| Code quality & security (production-readiness) | 6 / 10 |
| Docs & prior-audit accuracy | 5 / 10 |
| **Readiness to trade live vs C++ network** | **2 / 10** |

## Priority remediation order

1. **S1-B/C** deposit sequence `0xffffffff`, deposit value `Amount + fee2`,
   collect the redeem margin on claim/refund.
2. **S1-D** include `nTime` in `HashForSigning` when `WithTime`.
3. **F1/F2** bind RPC to loopback + auth; authenticate hub-originated swap
   packets; never let a forged `Finished` disable the refund watcher.
4. **F3/F4** session-lock scope + coin-registry `sync.RWMutex`/atomic pointer;
   add tests that run the watcher and hot-reload paths.
5. **S2-A/B/C** order-book detail-4 nesting, partial-chain not-found,
   `dxSplitInputs` utxo schema.
6. **S2-D** wire the expiry sweep to a timer using the correct
   `IsExpiredByBlockNumber`.
7. **Docs** — this register + `usage.md`/`wallet.md`/`swap.md` resync, and
   `p2p/seeds.go` cross-check.
