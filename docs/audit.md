# go-xbridge ↔ C++ — Parity Audit Register

Single authoritative record of C++↔Go fidelity, divergences, security findings,
and outstanding work for the `go-xbridge` port. It is a **living register**:
status here can lag the code; the code is authoritative.

## Reference pins

- **C++ (reference):** Blocknet Core @ `ac930b7f8` (4.4.1 era), `src/xbridge/`
  + `src/protocol.h`, `src/net_processing.cpp`, `src/chainparams.cpp`,
  `src/version.h` (`XBRIDGE_PROTOCOL_VERSION = 55`, `PROTOCOL_VERSION = 70713`).
- **Go (subject):** `go-xbridge` @ HEAD (`main`, module `go-xbridge`, Go 1.25+).
- **Method:** comparative read-only audit by 6 subagents + orchestrator
  re-verification of every S1 finding against the C++ writers. Baseline:
  `go build`, `go vet`, `go test`, `go test -race`, gofmt — all clean; hermetic
  (no live-network dials). The C++ header-comment enums are **stale**; the
  actual C++ writers are the contract.

## Severity taxonomy

- **S1** — Wire/interop-breaking: a C++ node rejects or misreads Go data (order,
  deposit, signature) → swap cannot complete. MUST FIX.
- **S2** — Protocol-semantic / behavioral: identical bytes but different meaning,
  ordering, validation, or a clear JSON-shape/behavior mismatch vs C++. MUST FIX.
- **S3** — Behavioral/config/thin-client divergence. FIX or document as
  deliberate.
- **S4** — Cosmetic / doc / strictness.

## Executive summary

The **wired contract is byte-for-byte faithful**: Bitcoin P2P framing, the
`xbridge` transport envelope, the 129-byte packet header, all 21 command enums,
all 19 body layouts (validated against a live `DOGE→BLOCK` capture), the
121-byte UTXO-entry encoding, and the signing digest all match the C++ writers.
The swap **state machine** (join, two-confirmation gate, drift check, locktimes,
HTLC script bytes) is faithful, and the **deposit construction path** matches the
C++ writers: the UTXO ownership proof (S1-A), deposit input sequence (S1-B),
`fee2` redeem margin (S1-C), and time-field sighash (S1-D) are fixed, so a
Go-created order/deposit is accepted by a C++ hub.

The `dx*` JSON-RPC surface is largely at parity (error codes, dates, amounts,
status strings, envelope); several response/behavior divergences remain (see the
matrix). The swap handshake inbound packets are re-verified against the trusted
hub key pinned at session creation (S2-E fixed). The two untested data races
(F3/F4) are remediated and `-race`-covered by the concurrency tests; on the
`concurrency/engine` branch the make-order live-pointer escape (F15), the
post-completion handshake retransmit gap (F16), and the force-refund
double-broadcast window (F18) are additionally fixed, leaving one documented
engine-I/O limitation (F17). Production readiness is now blocked only by the
remaining open divergence set.

**Bottom line:** the port can **connect** to the live network (wire-faithful)
and can **trade** with C++ peers at the wire level (S1-A…D, S2-B, S2-E, F3/F4
fixed); it is not yet production-safe until the remaining divergence items
below are fixed.

## Per-area verdicts

| Area | Verdict | Worst severity |
|---|---|---|
| SA-1 Wire packet header & transport | FAITHFUL (byte-for-byte; live-capture-validated) | — |
| SA-2 Command & TxCancelReason enums | FAITHFUL | — |
| SA-3 Packet body serialization | FAITHFUL (all live commands) | — |
| SA-4 Swap state machine & lifecycle | FAITHFUL (machine + deposit execution) | S2 |
| SA-5 Config (`xbridge.conf`) contract | FAITHFUL (superset of C++ keys) | S3 |
| SA-6 Fee & dust math | MINOR DIVERGENCE (thin-client defaults) | S3 |
| SA-7 Crypto & signatures | FAITHFUL except base58check strictness + BCH forkid | S2 |
| SA-8 RPC/API surface | PARITY except S2 shape/behavior + S3 value-level items | S2 |
| SA-9 Code quality & security | IMPROVED (races + growth fixed; open S2 divergences) | S2 |

## Open divergences

### S2 — Protocol-semantic / behavioral (MUST FIX)

- **S2-A. `dxGetOrderBook` detail-4 nesting.** Go emits `[[price, amount,
  [ids…]]]` (`api/handlers.go:738-747`); C++ emits flat `[price, amount,
  [ids…]]` (`rpcxbridge.cpp:1902-1926`). JSON shape differs; per-side
  `max_orders` caps are correct.
- **S2-C. `dxSplitInputs` utxo-parameter schema.** Go requires `amount` in each
  utxo object and does not resolve it from the wallet (`api/handlers.go:1594-1614`);
  C++ utxo objects carry `txid`+`vout` only and resolve amounts via
  `getTxOut` (`rpcxbridge.cpp:3313-3315`). A C++-contract caller gets `Amount=0`
  → spurious "insufficient funds". (Param *count* parity — exact-7 at
  `api/handlers.go:1221` — is correct.)
- **S2-D. Expiry sweep not wired into production.** `IsExpired`/
  `IsExpiredByBlockNumber` (`swap/transaction.go:219-255`) are implemented and
  unit-tested but **never invoked** by the daemon. C++ runs
  `eraseExpiredTransactions` on a timer (`xbridgeexchange.cpp:712-747`). Go only
  filters the store in `dxGetOrders` and relies on the hub to drop stale orders.
- **S2-H. base58check decode strictness.** Go hard-rejects a mismatched version
  byte (`coins/address.go:107-114`); C++ `toXAddr` strips the version byte
  blindly (`xbridgewalletconnectorbtc.cpp:1547-1555`). Valid-input payloads are
  identical; kept as **deliberate hardening**.
- **S2-I. BCH `SIGHASH_ALL|FORKID` missing in Go local signing.** C++ signs BCH
  refunds with forkid (`xbridgewalletconnectorbch.cpp:396`) and payments
  conditionally (`:454`); Go local signing lacks forkid. (Devault payments would
  match Go but Devault is separately broken by S2-J.)
- **S2-J. Coin-family misclassification / missing connectors.** DEVAULT is
  misclassified as the BTC family; DCR, PART, BTG adapters are missing. DCR/PART
  are core network coins → trading on them is impossible.

### S3 — Behavioral / config / thin-client (FIX or document)

- **S3-A. `dxMakeOrder` partial fields.** C++ emits literal `"0"`
  (`rpcxbridge.cpp:1061-1063`); Go emits `"0.000000"` (`api/order.go:219-221`).
- **S3-B. `dxGetUtxos` `amount` format.** C++ renders fixed **8-decimal**
  whole-coin (`rpcxbridge.cpp:3483`, e.g. `"3.26211780"`); Go trims trailing
  zeros (`api/handlers.go:1669`, `coins/amount.go:79-80`, `"3.2621178"`).
- **S3-C. `dxGetLockedUtxos` per-UTXO `amount` format.** Per-order key now
  matches C++ (`api/handlers.go:1067-1070` uses `<from>_and_<to>` for accepted
  orders). The per-UTXO amount strings still differ: C++ `UtxoEntry::toString()`
  streams the raw amount (`xbridgewalletconnector.cpp:25-31`), Go renders it via
  `coins.FormatAmount` (`api/handlers.go:1080-1084`).
- **S3-D. `dxPartialOrderChainDetails` `p2sh_deposits` alignment.** C++ emits one
  entry per order incl. `""` (`rpcxbridge.cpp:2455-2457`); Go omits empty entries
  (`api/handlers.go:975-977`) → element misalignment with `orders`.
- **S3-E. `dxGetNewTokenAddress` wallet failure.** C++ silently returns `[]`
  (`rpcxbridge.cpp:184-192`); Go mirrors that for a missing connector
  (`api/handlers.go:234`) but returns `1002`/`UNKNOWN` ("Internal Server
  Error", `errUnknown`) when the wallet's `getNewAddress` RPC fails
  (`api/handlers.go:236-238`).
- **S3-F. Fee/dust thin-client substitutions.** `FeePerByte==0` → 2 sat/vB
  (`api/handlers.go:1438-1442`; C++ `xbridgewallet.h:114` default 0 → fee 0); a
  Go-only `DustAmount` conf tier (`api/handlers.go:1416-1428`) where C++ always
  uses `0.546*relayFee*COIN` else 5460 (`xbridgewalletconnectorbtc.cpp:1526`).
  Both collapse to C++ values for real conf inputs; documented deliberate.
- **S3-G. Refund/payment payout model.** Go pays `amount − fee`
  (`api/swap.go:688,745`); C++ refund pays the full deposit outAmount funded by
  `+fee2` (`xbridgesession.cpp:2149`) and payment pays `amount + oOverpayment`
  (`:3971`). Go leaves the `fee2` redeem margin on the table (economic, not
  correctness).
- **S3-H. `signrawtransaction` param payload.** Go passes `[txHex, prevTxs,
  "ALL"]` (`wallet/rpc.go:287`); C++ passes `[rawtx, prevtxs, privkeys]` with no
  sighash-type (`xbridgerpc.cpp:539-570`). "ALL" lands in the privkeys slot of
  the legacy RPC (→ throws, then the modern fallback succeeds); net effect
  equivalent on modern wallets.
- **S3-I. `secretFromScriptSig` requires a 33-byte push** (`api/swap.go:856`);
  C++ `getSecretFromPaymentTransaction` matches any push
  (`xbridgewalletconnectorbtc.cpp:2263-2270`).
- **S3-J. `tryJoinMatches` adds partial-order min-size guards**
  (`swap/transaction.go:103-108`) not yet confirmed against C++ `tryJoin`.

### S4 — Cosmetic / strictness / docs

- JSON key emission order differs from C++ (documented, non-semantic).
- `formatXAmount` omits C++'s `+1/::COIN` (1e-8) round-up — below the 6-decimal
  render floor, unobservable.
- RPC help-text examples are misleading: they show 7-decimal amounts and
  `.12345Z` timestamps; the code emits **6-decimal** amounts and **3-digit ms**
  timestamps (matching C++ `xBridgeSignificantDigits(COIN)` / `iso8601`).
- Param-leniency on commands C++ rejects with error: `dxGetLocalTokens`,
  `dxGetNetworkTokens`, `dxGetTokenBalances`, `dxGetMyOrders`, `dxLoadXBridgeConf`,
  `dxGetNewTokenAddress`, extra params on `dxGetOrderFills`.
- `MaxPayloadSize` 64 MiB vs C++ `MAX_SIZE` 32 MiB; Go does not enforce C++'s
  per-command min-size checks on read (receive-side leniency); Go doesn't
  re-stamp the header timestamp or run `addToKnown`/hash dedup
  (application-level).

## `dx*` RPC equivalence matrix

`DONE` = behaviorally 1:1 with the C++ writer. `DIVERGENCE` = real remaining
mismatch (row note). `TIER3` = intentionally divergent thin-client limit
(document [`api.md`](api.md)). `GO-ONLY` = no C++ `dx*` counterpart.

| Command | Verdict | Note |
|---|---|---|
| dxGetOrders | DONE | conf/connector filter + id-sorted |
| dxGetOrder | DONE | `NO_SESSION` gate; case-insensitive id |
| dxGetMyOrders | DONE | history-only finished/cancelled now merged (S4 resolved) |
| dxGetOrderBook | **DIVERGENCE** | S2-A detail-4 nesting; per-side caps OK |
| dxGetOrderFills | PARITY-NOTE | S4: param leniency (C++ rejects size∉{2,3}) |
| dxGetMyPartialOrderChain | DONE | S2-B resolved: unknown id → `[]`; malformed id → `bad order id` |
| dxPartialOrderChainDetails | PARITY-NOTE | S3-D `p2sh_deposits` alignment |
| dxGetLockedUtxos | **DIVERGENCE** | S3-C per-UTXO amount format (per-order key now matches) |
| dxFlushCancelledOrders | DONE | age 0/1/else-`-1`; underflow clamped |
| dxGetLocalTokens | PARITY-NOTE | S4: accepts params C++ rejects |
| dxGetNetworkTokens | TIER3 | servicenode-registry-driven; P2P-bounded |
| dxMakeOrder | PARITY-NOTE | S3-A partial fields `"0"` vs `"0.000000"` |
| dxMakePartialOrder | DONE | `order_type="partial"`; trailing params |
| dxTakeOrder | DONE | full-take on amount=0; self-trade guard; dryrun |
| dxCancelOrder | DONE | `state>=trCreated` guard; `refund_tx` |
| dxLoadXBridgeConf | PARITY-NOTE | S4: hot-reload; last-good on failure |
| dxGetNewTokenAddress | PARITY-NOTE | S3-E wallet-RPC-failure → error vs `[]` (missing connector → `[]`, matching) |
| dxGetTokenBalances | PARITY-NOTE | S4 params; `Wallet` key present only when a connector loaded |
| dxGetUtxos | **DIVERGENCE** | S3-B amount trailing-zero trim; locked set Tier-3 |
| dxSplitAddress | DONE | 8-field object; 1e6-scale amounts |
| dxSplitInputs | **DIVERGENCE** | S2-C utxo schema (param-count parity OK) |
| dxGetOrderHistory | TIER3 | OHLCV buckets from session-local fills |
| dxGetTradingData | TIER3 | 8-field local record (`fee_txid`/`nodepubkey` empty); C++ `gettradingdata` not exposed |
| getNetworkInfo | GO-ONLY | Go-only extension |

## Tier 3 — architectural limits (thin-client, cannot fully match C++)

Documented once, not silently divergent — see [`api.md`](api.md) "Tier 3":
`dxGetOrderHistory`/`dxGetTradingData` session-local fills,
`dxGetNetworkTokens` P2P-bounded, and the locked-set limits.

## Fixed / verified (remediated at HEAD)

- **S1-A.** UTXO ownership-proof challenge = C++ `UtxoEntry::toString()`: the
  whole-coin `listunspent` `"value"` double is streamed with
  `strconv.FormatFloat(v, 'g', 6, 64)` (= default `ostringstream`, precision 6);
  golden vectors from real g++ output asserted in
  `TestWholeCoinOstreamMatchesCppStream`.
- **S1-B.** Deposit inputs use `SEQUENCE_FINAL` (0xffffffff), matching C++
  `createRawTransaction(..., cltv=true)`; the refund spend keeps
  `SEQUENCE_FINAL-1`. C++ `checkDepositTransaction` hard-rejects non-final
  deposit sequences.
- **S1-C.** Deposit locks `Amount + fee2` (`fee2 = minTxFee2(1,1)`), satisfying
  C++ `depositP2SHAmount >= amount + 0.95*fee2`; funding check and change are
  `total − Amount − fee − fee2`.
- **S1-D.** `nTime` committed in the sighash on `TxWithTimeField` coins
  (4-byte LE `TxTime` after `nVersion` in `HashForSigning`); golden digests from
  a C++ oracle asserted in `TestHashForSigningWithTimeField`.
- **S2-B.** `dxGetMyPartialOrderChain` unknown id returns `[]`
  (`api/handlers.go:863-865`) and malformed ids return `bad order id`
  (`api/handlers.go:858-861`), both matching C++
  (`rpcxbridge.cpp:2273-2324`).
- **S2-E.** Swap-handshake inbound packets (Hold/Init/CreateA/B/ConfirmA/B/
  Finished) re-verified against the trusted hub key pinned at session creation
  (maker: the servicenode chosen by `findNodeWithService` at `MakeOrder`; taker:
  the order's `SNodePubkey`) plus its registry membership (C++ `packet->verify(
  xtx->sPubKey)` + `getSn`) — never learned from network packets, so **no TOFU**;
  the pinned key persists across restarts. A forged `Finished` is dropped before
  it reaches `OnFinished`, so `csFinished` cannot disable the auto-refund
  watcher; an order whose hub is not a known, running servicenode is refused
  (`NO_SERVICE_NODE`). **Residual:** `hubRegistered` is strict (C++ `getSn`
  null) — in explicit `-node` mode with no `SNPING` seen, takes/handshake
  dispatches are refused until the hub's `SNPING` lands; Go's dry-run previews
  (a Go-only extension) skip the gate.
- **F1.** RPC binds to loopback by default (`-rpcbind`, mirroring blocknetd's
  `httpserver.cpp:308`); HTTP Basic auth enforced when `-rpcuser` +
  `-rpcpassword` are both set (constant-time compare, no cookie fallback);
  non-loopback bind without auth logs a warning.
- **F10.** RPC request bodies capped at 4 MiB (`http.MaxBytesReader` → `413`).
- **Verified behavior (cross-cutting):** decimals = 6
  (`xBridgeSignificantDigits(1_000_000)==6`); error `code`/`error` text from
  `xbridgeerror.h`; JSON-RPC 1.0 envelope with business errors in `result`;
  inbound packet signature verification before state mutation; remote
  cancel/reject wire plumbing with signature gates; lockTime drift check;
  dust/fee fidelity (`DustAmount` tier + 5460 + `MinTxFee` floor); network-token
  source via `p2p/servicenode.Registry`; empty order-book arrays as `[]`;
  panic-safe RPC envelope + rotating file logging + datadir pre-create.
- **F3 (was S2-F).** `SwapSession` fields are single-owner: all handshake
  mutation runs on the engine goroutine via `submit`, including the two-phase
  task resumes; the refund sweep (`scanRefunds`), session prune
  (`pruneSessions`), and persist run on the same engine ticker. Race-covered by
  `TestConcurrentRefundSweepAndDepositTask` (sweep reading a session while its
  deposit resume lands) plus the slow-wallet liveness and shutdown-drain tests in
  `api/concurrency_test.go`. The `swap/` package documents its pure/ownership
  contract (`swap/state.go`): `Session`/`Transaction` values are never shared
  across goroutines; workers receive immutable `swapCtx` snapshots.
- **F4 (was S2-G).** The coin registry is an `atomic.Pointer[map[string]Coin]`
  published whole by `InitFromConf` (last-good on error); `Get`/`Has`/`MustGet`
  dereference the pointer lock-free, so a `dxLoadXBridgeConf` hot-reload can
  never race a concurrent lookup. Race-covered by `TestConcurrentInitFromConfGet`
  (`coins/coin_test.go`), which hot-reloads against concurrent readers.
- **F6.** The book/session maps are never held across wallet I/O: `scanRefunds`
  runs on the engine and posts worker refund tasks; the refund `apply` runs back
  on the engine. The refund sweep (auto-broadcast of due pre-signed refunds)
  therefore cannot stall packet dispatch.
- **F5/F9.** Unbounded growth bounded: `pruneSessions` drops terminal sessions on
  the engine ticker; `Store.fills`/`history`/`cancelled` are capped via
  `trimOldest` (1000 each). Covered by `TestPruneSessionsRemovesTerminal`,
  `TestPruneKeepsSessionWithInFlightDepositTask`, `TestStoreHistoryBounded`.
- **S4 (dxGetMyOrders).** Live local orders now merge with the local
  history (finished/cancelled) so `Mine()` matches C++
  `rpcxbridge.cpp:2111-2120`.
- **F15.** `dxMakeOrder`/`dxMakePartialOrder` returned the STORE'S LIVE
  `*Order` (`api/node.go` MakeOrder): the HTTP handler rendered
  `makeOrderResponse()` on it while the engine could concurrently write the
  same record (a relayed self-echo bumps `Updated` via `Store.Touch`; a remote
  cancel writes `Status`) — a real data race that escaped the store's own "no
  live pointer escapes" contract (`api/store.go:17-20`). Fixed: MakeOrder
  returns `n.store.Get(key)` (a snapshot copy) from inside the engine closure,
  mirroring TakeOrder/CancelOrder. Covered by `TestMakeOrderReturnsStoreCopy`
  (aliasing check + render-vs-`store.Touch` under `-race`).
- **F16.** The handshake handlers had no C++-style post-completion state guard:
  a retransmit arriving after the deposit/claim completed (`await` already
  cleared) re-ran stage 1 and re-broadcast the deposit/claim — a second on-chain
  broadcast attempt. Fixed with C++-faithful guards: `OnCreateA/B` drop once
  `state >= csCreatedA/B` (C++ `state >= trCreated`, `xbridgesession.cpp:1947,
  2424`); `OnConfirmA/B` drop once `state >= csConfirmedA/B` (C++ `state >=
  trCommited`, `:2897,3152`). The `await` guard covers the in-flight window;
  these cover post-completion. Covered by the `*StateGuard*` unit tests and
  `TestCreateAStateGuardDropsPostCompletionRetransmitE2E`.
- **F18.** A force-refund (`enqueueRefund`, from CancelOrder/BroadcastRefund)
  did not take the `pendingRefunds` guard, so it could overlap the sweep's
  auto-refund for the same order (two `SendRawTransaction` of the same hex;
  on-chain idempotent but noisy). Fixed: `enqueueRefund` sets the guard before
  posting, so `scanRefunds` skips while it is in flight; the apply still clears
  the guard on success AND error, preserving the sweep safety net. Covered by
  `TestForceRefundTakesSweepGuard`.
- **F20.** `p2p/servicenode` registration integrity. `ParseServiceNode` and
  `ParseServiceNodePing` retain paymentAddress, collateral, bestBlock,
  bestBlockHash, and the registration signature (servicenode.h:354-384) instead
  of discarding them. A ping whose embedded registration fails the
  thin-client-enforceable subset of `ServiceNode::isValid` — SPV tier, fully-valid
  curve pubkey (C++ `IsFullyValid`), non-null payment address, collateral 1..10
  with no duplicates, recoverable signature over `CreateSigHash`
  (servicenode.h:104-111, 398-484) — is rejected at parse, mirroring
  `ServiceNodePing::isValid` running `snode.isValid` (servicenode.h:817-818) and
  `processPing` dropping the ping (servicenodemgr.h:186-187). `AddRegistration`
  applies the same gate (C++ `addSn`, servicenodemgr.h:862). `Registry.
  PaymentAddress()` resolves the hub's fee destination for B2. Coverage:
  `TestParseServiceNodeRetainsRegistration`,
  `TestParseServiceNodePingRetainsEmbeddedRegistration`, `TestCreateSigHashGolden`
  (pins the `CreateSigHash` byte serialization), `TestAddRegistrationRejectMatrix`,
  `TestAddPingRejectsInvalidEmbeddedRegistration`, `TestPaymentAddress*`.
  **Residual (thin client, documented):** on-chain collateral ancestry/ownership/
  total (`>= COLLATERAL_SPV`) and block ancestry (servicenode.h:401,447-478) still
  require a full chain index, so a well-formed signature over different data is
  only rejected on-chain.

## Deliberate thin-client items (explicitly NOT bugs)

- `gettradingdata` alias removed; `getnetworkinfo` Go-only extension.
- `dxGetOrderHistory`/`dxGetTradingData`/`dxGetNetworkTokens`/locked-set limits
  (Tier 3).
- `LocalConnector` local-key signing (no C++ counterpart).
- `FeePerByte==0 → 2 sat/vB`; `DustAmount` conf tier (S3-F).
- base58check strictness (S2-H, kept).
- `xbcServicesPing`/`xbcXChatMessage` typed bodies (C++ has no live writer;
  servicenode messages parsed by `p2p/servicenode`).

## Security & production-readiness findings

| # | Finding | Severity |
|---|---|---|
| F7 | Inbound order UTXO ownership proofs are never verified before the order is put on the book. | S3 |
| F8 | Segwit/BIP143 signing is dead code w.r.t. the daemon; bech32 destinations re-encoded as legacy P2PKH. | S3 |
| F11–F14 | Vestigial `Server.verify`, unused `coins.MustGet`, tested-but-unreferenced `swap` package, `LocalConnector.SignMessage`/`VerifyMessage` unsupported (test-only). | S4 |
| F17 | Blocking I/O on the engine goroutine: `MakeOrder`/`CancelOrder` closures and the two-phase resumes run `conn.WritePacket` (blocking TCP write) and `persist()` (fsync) inline; a stalled peer or slow disk stalls all state processing. | S3 |

## 2026 audit findings — F19–F27 register

The 2026 re-audit reported findings F1–F9 that collide with the prior F-numbering
above, so they are registered as **F19–F27**. Fixes land branch-by-branch
(execution order in the remediation plan); each branch moves its IDs to
"Fixed / verified" and marks them here.

| # | Severity | Verdict | Status |
|---|---|---|---|
| F19 | Blocker | CONFIRMED — `TakeOrder` emits an `AcceptingBody` with empty `ServiceNodeFeeTx`/`Utxos` (156 bytes < C++ 188 minimum) | **FIXED — B2 `fix/wire-acceptingbody` (at HEAD)**: take inputs reserved atomically per order (`Store.ReserveForTake`, C++ lockCoins/lockFeeUtxos under m_utxosOrderLock) so no two concurrent takes can double-select a BLOCK fee utxo or taker funding utxo; funding inputs are p2pkh-25 filtered (C++ unspentP2PKH). The reservation is one-per-order: a concurrent second take of the same order is refused `BAD_REQUEST` "not accepting, order already accepted" (C++ state gate `xbridgeapp.cpp:2122-2125`), so it can never overwrite the first take's claimed keys; the gate only spans the in-flight window (ReleaseReserve on submit), keeping sequential re-takes legal. Both verified by `TestStoreReserveForTake`, `TestConcurrentTakeOrderDistinctOrdersExclusiveReservation`, `TestConcurrentTakeOrderSingleSession`, `TestTakeOrderFundingRejectsNonP2PKH` + `make parity`. **Follow-on closeout (A1-A7, `fix/wire-acceptingbody` HEAD+2, see `docs/remediation/B2-acceptingbody.md`)**: seven further gate deviations found and fixed — `availableBalance()` now reads the BLOCK wallet only (A1); `xBridgeValueFromAmount` adds C++'s `+1.0/::COIN` round-up (A2); funding enumerates unspent at minconf 1 (A3); missing `checkAcceptParams` balance gate added (A4); fee order-info overflow now maps to `INVALID_ONCHAIN_HISTORY` (1033) instead of 1019, and its truncation no longer panics on underflow (A5); dryrun returns before the accept-path dust checks (A6); address validation is deferred past the earlier gates (A7); pre-accept NO_SESSION/INSUFFICIENT_FUNDS messages now carry C++'s exact args (toCurrency/fromAddress, rpcxbridge.cpp:1262-1263); empty-BLOCK-wallet path covered by `TestTakeOrderEmptyBlockWallet`. **Lock exclusion made per-token (D4, `docs/remediation/B2-acceptingbody.md`)**: `Order.UtxoCurrency` tags the chain each order's locked Utxos live on; `Store.LockedUtxoInfoFor(ticker)` and a `fee`/`fund`-split reservation reproduce C++ `getAllLockedUtxos(token)` (`m_utxosDict[token]` + `m_feeUtxos`) so make/fund/balance checks exclude only coins locked on the checked chain — covered by `TestLockedUtxoInfoFor` + `make parity`. |
| F20 | Critical | CONFIRMED — registration fields read-then-discarded; gates miss the `isValid` subset | **FIXED — B1 `fix/servicenode-registry` (at HEAD)** |
| F21 | Critical | CONFIRMED — no `checkDepositTransaction` in the Connector contract | OPEN — B3 `fix/deposit-path` |
| F22 | Critical | CONFIRMED — HTLC ELSE branch + CreateB-derived taker deposit; composition is SOUND | OPEN — closed by B3 (composite acceptance; no standalone code) |
| F23 | High | CONFIRMED — `buildDeposit` broadcasts before building the refund | OPEN — B3 |
| F24 | High | CONFIRMED — HTTP auth/timeout hardening missing | OPEN — B4 `fix/http-hardening` |
| F25 | High | CONFIRMED — P2P addr/varint allocation DoS | OPEN — B5 `fix/p2p-dos` |
| F26 | High | CONFIRMED — plaintext secrets + debug-log leakage | OPEN — B6 `fix/secrets-hygiene` |
| F27 | High | CONFIRMED — deposit re-runs `ListUnspent` instead of `xtx->usedCoins` | OPEN — B3 |

**Attacker model:** inbound signature verification *is* enforced before state
mutation against a trusted hub key, and automatic peer discovery is rate-unbounded.
An attacker can pollute the book and trigger real deposits (fund lockup,
recoverable via refund). HTLC semantics (ELSE-branch needs the secret; refunds
pay the depositor's own address) prevent direct **theft** — worst case is lockup +
fee-burn + state corruption + DoS. `-race` green now covers F3/F4: the watcher
path runs in `TestConcurrentRefundSweepAndDepositTask` and the hot-reload path
in `TestConcurrentInitFromConfGet`. F15 (make-response vs engine `store.Touch`),
F16 (post-completion retransmit), and F18 (force-refund vs sweep) are covered by
`TestMakeOrderReturnsStoreCopy`, the `*StateGuard*` tests, and
`TestForceRefundTakesSweepGuard`. F17 (blocking engine I/O) is a documented
liveness hazard, not a memory-safety one.

## Verification gaps still open

- Live-hub verification of the swap-handshake claim/refund spends (in-memory
  connectors only today; `cmd/liveprobe` dials + handshakes but does not drive a
  swap).
- Byte-level capture of `OrderBody`/`AcceptingBody`/`CancelBody` UTXO-entry
  encoding vs a live C++ node (currently validated on one captured packet).
- Cross-check `p2p/seeds.go` fixed-IP/DNS seeds against `chainparamsseeds.h`.
- Live-hub confirm of S1-B/C (`checkDepositTransaction` behavior with a real
  deposit); the writers are matched but no C++ node has accepted a Go deposit
  on-chain yet.

## Ratings (2026-08 audit)

| Area | Rating |
|---|---|
| Wire protocol & transport fidelity | 9 / 10 |
| RPC / `dx*` surface parity | 7 / 10 |
| Swap state machine (machine/scripts) | 8 / 10 |
| Swap deposit/execution path | 9 / 10 (S1-A…D fixed) |
| Config / coins / crypto / wallet | 7 / 10 |
| Code quality & security (production-readiness) | 8 / 10 (F1/F2/F3/F4/F5/F6/F9/F10/F15/F16/F18/F20 fixed; F17 documented) |
| Docs & prior-audit accuracy | 7 / 10 |
| **Readiness to trade live vs C++ network** | **6 / 10** |

## Priority remediation order

1. **S2-A/C** order-book detail-4 nesting, `dxSplitInputs` utxo schema.
2. **S2-D** wire the expiry sweep to a timer using the correct
   `IsExpiredByBlockNumber`.
3. **S2-I/J** BCH forkid signing; DCR/PART/DEVAULT coin-family connectors.
4. **S3-G** refund/payment payout model (`fee2` margin, `oOverpayment`).
