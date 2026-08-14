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
| B5 | `fix/wire-hardening` | **WIRE-F57–F64, F67–F70** (F65/F66 DOCUMENTED; F71 on B1) | `p2p/addr.go`, `p2p/envelope.go`, `p2p/conn.go`, `p2p/message.go`, `p2p/params.go`, `p2p/version.go`, `p2p/seeds.go`, `p2p/discovery/peer_manager.go`, `proto/packet.go`, `proto/body_types.go`, `p2p/servicenode/servicenode.go`, `cmd/xbridged/main.go` | — |
| B6 | `fix/secrets-hygiene` | **SEC-F04** | `api/persist.go`, `wallet/rpc.go`, `api/swap.go` (log lines only) | — |
| B7 | `fix/rpc-surface` | **RPC-F03–F59** (+ re-decide F37) | `api/handlers.go`, `api/response.go`, `api/order.go`, `api/utxo_select.go`, `api/store.go`, `coins/amount.go`, `api/node.go` | B3, B4 |
| B8 | `fix/state-machinery` | **STATE-F72–F75** | `api/engine.go`, `api/store.go`, `api/response.go`, `api/node.go`, `swap/transaction.go` | B3 |
| B9 | `fix/crypto-connectors` | **CRYPTO-F77 (S1), F82, F83, F88, F89, F91, F92** (F98/F99 DOCUMENTED residual: PART/BCD non-portable) | `coins/tx.go`, `coins/coin.go`, `coins/cashaddr.go`, `coins/base58check.go`, `crypto/signer.go`, `api/swap.go`, `api/handlers.go`, `api/utxo_select.go`, `wallet/rpc.go`, `swap/deposit.go` | B3 |
| B10 | `fix/config-parity` | **CFG-F84–F91** (F86 DOCUMENTED; F80 folded) | `config/conf.go`, `config/admit.go`, `cmd/xbridged/main.go`, `coins/coin.go`, `wallet/conf.go`, `wallet/activate.go`, `api/node.go`, `api/store.go`, `api/handlers.go`, `api/utxo_select.go`, `api/engine.go` | B4 |
| B11 | `fix/concurrency` | **CONC-F92–F94** | `api/node.go`, `api/engine.go`, `p2p/conn.go`, `p2p/discovery/peer_manager.go`, `log/dedup.go`, `api/persist.go` | B5 |

\* SEC-F03 (taker-trust) has no standalone code — the HTLC composition is sound
(`coins/htlc.go:45-54` matches C++). It is closed by **B3** as composite
acceptance: B1's verified hub + B3/CRYPTO-F85's validated deposit kill the
theft. B3 carries the end-to-end refusal test and corrects the attacker model
in the register.

**Deliberate/documented (no branch):** RPC-F19/F39 (Tier-3 data source),
WIRE-F65/F66 (hardening/doc), STATE-F76, CONC-F95/F96, CRYPTO-F79 (fee
fallback; CRYPTO-F80 FIXED on B10), CRYPTO-F81 (address hardening),
INV-F97–F100, STATE-F77/F78, CONC-F97–F102, SEC-F01, RPC-F59,
CRYPTO-F84/F93–F96 (fixed; regression-covered), CFG-F86 (never-creates). Each
is marked `FIXED`/`DOCUMENTED` in `register.md`.

**Status (2026-08-13):** B1 and B2 merged to `main` (WIRE-F71, CRYPTO-F84 +
A1–A7 + per-token D4). **B6 merged** (SEC-F04: `-persistsecrets` gate,
log-site removals, corrupt-file severity parity). **B3 merged** (deposit path:
CRYPTO-F85/F86/F87/F78/F90/F97, STATE-F71, SEC-F03 — validated-deposit gate,
native-unit scale, wire-Cancel). **B5 merged** (wire hardening: WIRE-F57–F64,
F67–F70 — caps, magic/version/checksum gates, canonical varint, snl echo,
regtest rename, cmd2/50 removal). **B4 merged** (http hardening: RPC-F01/F02,
F47–F52/F58 — transport status routing, bare method-not-found, 32 MiB cap,
always-auth + rpcauth, batch/named, strict params + NO_SESSION name, arity
gates). **B7 merged** (RPC response-surface: RPC-F03–F57, F59 — order/filter,
cancel codes, make/take shape, order-history OHLCV, order-book bump/nesting,
split fee model, utxos fixed-8, tokens/conf, getnetworkinfo, gettradingdata
documented). **B9 merged** (crypto-connectors: CRYPTO-F77 S1 forkid sighash —
BCH 0xffdead/BTG 79/DEVAULT 0 — F82 RNG, F83 block-hash byte order, F88 BIP143
live, F89 BTG/DEVAULT classification; F91/F92 RPC payloads; F98/F99 PART/BCD
documented non-portable). **B10 merged** (config-parity: CFG-F84–F91 — `[Rpc]`
whitelist, admission gates `config.Admit`, `ExchangeWallets` parsing, exact-case
keys, CLI flags, `MinimumAmount`/`CashAddrPrefix`/`CreateTxMethod` alignment,
ExchangeWallets-keyed activation + reachability probe + 30 s sweep + order
clearing; CFG-F86 documented never-creates). B8, B11 pending.

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

### B4 — `fix/http-hardening` — RPC-F58, RPC-F01/F02/F47–F52 — MERGED

- Original gap: `authorized` returned true with no user configured; `SetAuth`
  silently disabled on partial creds; no method/content-type/host checks;
  `http.Server` had no timeouts. C++ is POST-only + always-authenticated
  (`src/xbridge/httprpc.cpp:148-234`).
- **RPC-F58 (delivered):** `http.Server` read/write/header/idle timeouts +
  `-rpcservertimeout` (default 30 s, C++ `DEFAULT_HTTP_SERVER_TIMEOUT`).
- **RPC-F47 (delivered):** `httpStatusForCode` — 400 for −32600, 404 for
  −32601, 500 otherwise (was HTTP 200 everywhere).
- **RPC-F48 (delivered):** bare `"Method not found"` (no method-name suffix);
  id parsed before dispatch and echoed on pre-dispatch errors.
- **RPC-F49 (delivered):** 32 MiB `rpcMaxBodyBytes` via `MaxBytesReader`,
  non-envelope 413 (was 4 MiB + JSON −32700).
- **RPC-F50 (delivered):** always-auth when ANY credential configured
  (`-rpcuser/-rpcpassword` or multi-user `-rpcauth` HMAC-SHA256), 401 empty
  body + `WWW-Authenticate`, 250 ms failure delay. Residual (documented):
  no auto-cookie file; loopback-open when zero credentials configured.
- **RPC-F51 (delivered):** batch (`execOne` per element, always HTTP 200);
  named params rejected with −8 `"Unknown named parameter <key>"`; −32600
  emitted for request-shape errors.
- **RPC-F01 (delivered):** error-channel policy — strict parsers
  (`spStr/spBool/spInt/spInt64`, `uvStr/uvBool/uvBoolOpt/uvArr`,
  `rtcStr/rtcNum/rtcBool`); param/type errors as envelope −1 (json_spirit
  family) or −3 (RPCTypeCheck family); C++ read-order + `limit` validation.
- **RPC-F02 (delivered):** `NO_SESSION` `name` = the handler method name
  (not `"dx"`).
- **RPC-F52 (delivered):** central arity registry (`api/dispatch.go`) with
  byte-for-byte C++ help-text errors (business methods → 1025; throw methods →
  envelope −1 with the `RPCHelpMan` text); per-handler gates removed.
- **Delivered (B4):** commits `859b7aa` (brief), `4a0e422` (transport),
  `e5995d2` (auth), `6304ea9` (strict params/NO_SESSION), `e1bc23f` (arity),
  `7c0157c` (conformance promote). Diff brief: `remediation/B4-http.md`.
  Contract corrections verified against C++: json_spirit `get_value< T >`
  messages, UniValue `JSON value is not a X as expected`, `RPCTypeCheck`
  `-3 "Expected type t, got n"`, business 1025 vs throw-envelope split.
- **Verify:** full suite + race green; `make parity` + `make canary` green.

### B5 — `fix/wire-hardening` — WIRE-F57–F64, WIRE-F67–F70 — MERGED

- `ParseAddr` pre-bounds `make(…, n)` (`p2p/addr.go:33-43`); `readVarInt`
  uint64→int cast (`p2p/envelope.go:126`); no `recover` in discovery read loop
  (`peer_manager.go:280`); deadline cleared post-handshake (`p2p/conn.go:89`);
  payload caps (WIRE-F57 4,000,000; WIRE-F65 body cap policy); magic validation
  (WIRE-F58); `MIN_PEER_PROTO_VERSION` (WIRE-F59); regtest rename (WIRE-F60);
  `snl` receive (WIRE-F61); trailing-bytes reject (WIRE-F62); canonical varint
  (WIRE-F63); checksum log-and-drop (WIRE-F64); cmd2/cmd50 disposition
  (WIRE-F67/F68); getaddr policy + addr cap (WIRE-F69); handshake
  deadline/negotiation (WIRE-F70); WIRE-F71 already fixed on B1.
- **Delivered (B5):** `MaxPayloadSize` 4,000,000 + conn cap; `conn.readMessage`
  magic validation (disconnect) and bad-checksum log-and-drop (`ErrChecksum` sentinel);
  handshake gates on `MinPeerProtoVersion` (70712) + duplicate `version`; 60 s
  handshake deadline; `staging`→`regtest` network rename (`RegtestMagic`,
  `-network regtest`); `snl` answered with a raw accepted-ping echo;
  `proto.Unmarshal` exact-length body; `readVarInt` rejects non-canonical
  CompactSize + `> MAX_SIZE` (32 MiB); cmd-2/cmd-50 speculative body types
  deleted (`DecodeBody` rejects); `getaddr` never answered (outbound-only) +
  1000-record `addr` cap. F65 (1 MiB body cap) and F66 (cmd-4 dual writer) stay
  DOCUMENTED; SENDHEADERS/SENDCMPCT + periodic pings remain DOCUMENTED under
  F70. See `remediation/B5-wire.md`.
- **Verify:** as B1; `make parity` + `make canary`.

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

### B7 — `fix/rpc-surface` — RPC-F03–F59 (RPC S2/S3 set) — **DONE, merged to `main`**

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

### B9 — `fix/crypto-connectors` — CRYPTO-F77, F82, F83, F88, F89, F91, F92 — **DONE, merged to `main`**

BCH forkid sighash + replay protection for the connector local-signing path:
parameterized BIP143 digest + forkid signing (`HashForSigningBIP143`,
`SignTxInputForkID`, `SignTxInputForCoin`); per-coin `SignatureKind`/`ForkValue`
derived from `CreateTxMethod` (BCH `0xffdead` — live mainnet replay protection,
DEVAULT 0, BTG 79); wired into `buildRefundTx` (commits the recorded deposit
P2SH value) / `redeemCounterparty` (validated deposit amount) /
`DepositSpec.SignInput`; DER byte `0x41`. Parity oracle transcribes the C++
forkid `SignatureHash` (bch.cpp:191-256 / btg.cpp:118-207). Coin-agnostic:
`signrawtransaction` payload (F91), all-inputs secret scan (F92), full-range
RNG retry (F82), block-hash byte-order pin (F83, oracle transcribes
`base_blob::SetHex`). BTG/DEVAULT classification + address codecs (F89).
PART/BCD tx formats are non-portable → `CRYPTO-F98`/`CRYPTO-F99` DOCUMENTED
(deferred); DCR is not in the live manifest (dropped). Branch doc:
`B9-crypto.md`.

### B10 — `fix/config-parity` — CFG-F84–F91 — **DONE, merged to `main`**

Conf-parity pass: `[Rpc]`/`[Main]` whitelisted in `config.Load` (F84, a stock
conf no longer aborts startup); exact-case key/section lookups (F89, boost
property_tree semantics); `ExchangeWallets` split on `,;:` with
`ccy::Symbol::validate` (F88); wallet admission gates ported to `config.Admit`
and applied at startup/reload/sweep via `config.Admitted` (F85); `Title`
defaults to `""`, `MinimumAmount` is the dust source, the `CashAddrPrefix` conf
value wins over the method table, ETH/unknown `CreateTxMethod` rejected (F91);
`-dxnowallets`/`-enableexchange` flags + startup honors `Main.ShowAllOrders`
(F90, version-case sub-item already fixed on B7). `wallet.Activator` connects
exactly `[Main].ExchangeWallets` ∩ gates ∩ a live reachability probe (C++
`updateActiveWallets`, 300 s bad-wallet retry), reload preserves the daemon
flags + `PersistSecrets`, clears non-local orders unless ShowAllOrders, and a
30 s sweep re-probes in-memory settings (F87). CFG-F86 is DOCUMENTED — the
never-creates hard rule stands; the daemon requires an existing conf. Branch
doc: `B10-config.md`.

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
