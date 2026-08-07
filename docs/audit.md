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
hub key pinned at session creation (S2-E fixed). Separately, production
readiness is still blocked by two untested data races and an open divergence set.

**Bottom line:** the port can **connect** to the live network (wire-faithful)
and can **trade** with C++ peers at the wire level (S1-A…D, S2-B, S2-E fixed); it
is not yet production-safe until the data-race + divergence items below are
fixed.

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
| SA-9 Code quality & security | NOT production-ready (races, growth) | S2 |

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
- **S2-F. Data race on live `SwapSession` fields.** Handlers mutate `state`
  (`api/swap.go:218,227,252,297,345,399,417`), `ourDepositTxID` (`:636`),
  `ourLockTime` (`:637`), `refundHex` (`:649`) on the feed goroutine **without**
  `sessMu` while the refund-watcher `checkRefunds` (`api/swap.go:487-512`) and
  persist read under it. Untested by `-race` (watcher never runs in tests).
  `ingestPending` (`api/node.go:641`) mutates `ex.Updated` on the same goroutine
  via `Store.Get` — same accepted pattern.
- **S2-G. Data race on the package-global `coins.Coins` map on hot-reload.**
  `coins.InitFromConf` reassigns the map (`coins/coin.go:62-72`) without
  synchronization, invoked by `dxLoadXBridgeConf → reloadConf` (`api/node.go:287`)
  while other goroutines call `coins.Get` (`coins/coin.go:162`) — possible
  "concurrent map read and map write" daemon crash.
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
- `dxGetMyOrders` `Mine()` omits history-only local finished/cancelled orders
  (`api/store.go:134` vs C++ `rpcxbridge.cpp:2111-2120`).
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
| dxGetMyOrders | PARITY-NOTE | S4: omits history-only finished/cancelled |
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
| F3 | Data race on `SwapSession` fields (S2-F). | S2 |
| F4 | Data race on global `coins.Coins` map on hot-reload (S2-G). | S2 |
| F5/F9 | `sessions` / `Store.fills` / `Store.history` grow without bound (memory + persisted JSON). | S3 |
| F6 | `sessMu` held across wallet RPC I/O in `checkRefunds` (stalls packet dispatch up to 30 s). | S3 |
| F7 | Inbound order UTXO ownership proofs are never verified before the order is put on the book. | S3 |
| F8 | Segwit/BIP143 signing is dead code w.r.t. the daemon; bech32 destinations re-encoded as legacy P2PKH. | S3 |
| F11–F14 | Vestigial `Server.verify`, unused `coins.MustGet`, tested-but-unreferenced `swap` package, `LocalConnector.SignMessage`/`VerifyMessage` unsupported (test-only). | S4 |

**Attacker model:** inbound signature verification *is* enforced before state
mutation against a trusted hub key, and automatic peer discovery is rate-unbounded.
An attacker can pollute the book and trigger real deposits (fund lockup,
recoverable via refund). HTLC semantics (ELSE-branch needs the secret; refunds
pay the depositor's own address) prevent direct **theft** — worst case is lockup +
fee-burn + state corruption + DoS. `-race` green does **not** cover F3/F4 (the
watcher/reload paths are untested).

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
| Code quality & security (production-readiness) | 7 / 10 (F1/F2/F10 fixed) |
| Docs & prior-audit accuracy | 7 / 10 |
| **Readiness to trade live vs C++ network** | **6 / 10** |

## Priority remediation order

1. **F3/F4** session-lock scope + coin-registry `sync.RWMutex`/atomic pointer;
   add tests that run the watcher and hot-reload paths.
2. **S2-A/C** order-book detail-4 nesting, `dxSplitInputs` utxo schema.
3. **S2-D** wire the expiry sweep to a timer using the correct
   `IsExpiredByBlockNumber`.
4. **S2-I/J** BCH forkid signing; DCR/PART/DEVAULT coin-family connectors.
5. **S3-G** refund/payment payout model (`fee2` margin, `oOverpayment`).
