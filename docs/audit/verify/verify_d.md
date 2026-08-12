# Verification report — candidate findings STATE-F76–CONC-F93

Reference: `blocknet_core/src/xbridge/` (C++). Candidate: `go-xbridge/` (Go).
Each finding re-read on both sides. Line numbers verified against the working tree.

---

## STATE-F76 Go live handshake uses clientState, not the ported swap.State

VERDICT: **CONFIRMED** (structural fact; NOT a behavioral divergence — same observable transitions).

- Go live handshake: `api/swap.go:40-53` defines a separate `clientState` enum
  (`csIdle..csFinished`); the On* handlers drive it (`swap.go:294,303,380,482,580,687,712`).
- Go `swap/state.go:18-32` defines `Transaction::State` (`TrInvalid..TrDropped`) and
  `swap/transaction.go:141-175` `IncreaseStateCounter`; `swap/session.go:133-157` calls it.
  The api package never uses `swap.Transaction`/`swap.Session` — the only non-test imports
  of `go-xbridge/swap` are `api/{swap,response,handlers}.go`, which use only
  `swap.DepositSpec` (swap.go:1078,1122,1167) and `swap.DescrStateOrdinal` (response.go:469,
  handlers.go:1101). `swap.Transaction`/`Session` have no non-test callers outside the swap
  package (grep `NewTransaction|NewSession|swap.Transaction`).
- C++ live machine: session handlers gate on `tr->state()` (`xbridgesession.cpp:1832`
  `tr->state() != Transaction::trHold`) and on `TransactionDescr::State` (`xbridgesession.cpp:1494`
  `xtx->state >= TransactionDescr::trHold`, `:1947` trCreated, `:2897` trCommited); the hub side
  advances via `Transaction::increaseStateCounter` (`xbridgeexchange.cpp:557,578,599,613`).
- Determination: Go's `clientState` reproduces the same handshake progression (Hold→Init→
  Created→Confirmed→Finished) and the same "already past N" guards as C++ (`swap.go:324,412,515,614`
  mirror `xbridgesession.cpp:1947,2424,2897,3152`). Internal-implementation choice, not an
  observable divergence.

Corrected statement: the live Go handshake is driven by `api/swap.go:40` `clientState`, not the
`swap/state.go` Transaction machine (which is exercised only inside the swap package and its
tests), but it reproduces the same guards/transitions as C++, so it is an implementation choice
rather than a behavioral divergence.

---

## STATE-F71 OnHold/OnInit skip verification

VERDICT: **CONFIRMED** (C++ verifies; Go's handlers do not; partial mitigation by hub-key pinning).

- C++ `processTransactionHold` (`xbridgesession.cpp:1322-1543`) verifies before accepting:
  snode signature `packet->verify(xtx->sPubKey)` (:1364); snode validity + registry membership
  (:1376-1397); taker amounts `samount==xtx->fromAmount && damount==xtx->toAmount` (:1407-1421);
  maker caps `damount<=fromAmount`, `samount<=toAmount`, `damount>=minFromAmount` (:1429-1457);
  price drift check (:1463-1471); state (:1494). For role 'A' the maker's order then ADOPTS the
  taker's requested amounts (:1524-1527).
- C++ `processTransactionInit` (`xbridgesession.cpp:1675-1774`) verifies order identity/details
  against the packet (`xtx->id/from/fromCurrency/fromAmount/to/toCurrency/toAmount`, :1750-1760 —
  note the `&&` bug makes it fire only when ALL differ).
- Go `OnHold` (`api/swap.go:283-299`) only checks the coin exists and decodes the source address;
  it never inspects `b.FromAmount/b.ToAmount`. Go `OnInit` (`swap.go:302-308`) only echoes
  `b.ClientAddress`. The HoldBody/InitBody amounts (proto/body_types.go:367-399,434-458) are
  carried but never validated against the session.
- Partial mitigation: hub packet signature + registry membership ARE re-checked per packet in
  `processSwap`/`verifyHubPacket` (`api/node.go:715-729,770-777`; C++ analogue
  `xbridgesession.cpp:1364,1384`), so only the pinned hub can forge; but a pinned-but-malicious/
  buggy hub can drive the swap without any amount/price check, and Go never implements the
  C++ partial-fill amount adoption (`xbridgesession.cpp:1524-1527`) — Go locks the full order amount.

Corrected statement: Go's OnHold/OnInit (`api/swap.go:283-308`) skip the amount/identity/currency
verification that C++ `processTransactionHold`/`processTransactionInit` perform
(`xbridgesession.cpp:1407-1471,1750-1760`); the hub-signature check (`api/node.go:770-777`) only
bounds the adversary to the pinned hub, and Go lacks C++'s Hold-time amount adoption.

---

## STATE-F72 Expiry sweep never wired

VERDICT: **CONFIRMED** (Go has the predicates but no production call site; cadence claim correct).

- C++ pruning: `App::Impl::onTimer` (`xbridgeapp.cpp:3658-3752`) on `TIMER_INTERVAL = 15`
  (`xbridgeapp.cpp:90`) posts `checkAndEraseExpiredTransactions` (:3684) → `Exchange::eraseExpiredTransactions`
  (`xbridgeapp.cpp:3573-3577`, `xbridgeexchange.cpp:712-745`, `isExpiredByBlockNumber`/`isExpired`
  erase + `unlockUtxos`). Persist: `saveOrders()` every 4th tick = 60s (:3745-3747).
- Go predicates: `swap/transaction.go:217-255` `IsExpired`/`IsExpiredByBlockNumber`; grep shows
  the ONLY call sites are `swap/transaction_test.go:194,237,289`. No production code prunes the
  Store by expiry: the 60s engine ticker (`api/engine.go:161`, `refundCheckInterval=60s`
  `api/swap.go:34`) runs `scanRefunds`+`pruneSessions` (engine.go:174-187) which only drop
  sessions/terminal orders; `Store.Remove` has no production caller; store pruning is only
  RPC-driven `FlushCancelled` (`api/handlers.go:1150`, `api/store.go:486-513`). Expired orders are
  merely hidden from `dxGetOrders` (`api/handlers.go:110-114`), never removed.
- Cadence: C++ 15s tick / 60s persist; Go 60s ticker / persist every 4th tick = 240s
  (`api/engine.go:185`). Confirmed.

Corrected statement: C++ erases expired pending orders on a 15s timer (`xbridgeapp.cpp:90,3684`,
`xbridgeexchange.cpp:712`) and persists every 60s, while Go never calls
`swap.Transaction.IsExpired/IsExpiredByBlockNumber` outside tests and never periodically prunes the
store (60s tick / 240s persist only run refund/prune-session sweeps).

---

## STATE-F73 TxCancelReason not ported

VERDICT: **CONFIRMED** (enum + text table absent in Go; both C++ string-table bugs reproduced).

- C++ enum: `xbridgepacket.h:21-48` `TxCancelReason` values `crUnknown=0 .. crBadFeeTx=24`.
- C++ text table: `TxCancelReasonText` `xbridgeapp.cpp:4052-4107`. Bugs confirmed:
  `case crBadSettings: return "crUnknown"` (:4055-4056); `case crUnknown: default: return "crNone"`
  (:4103-4105).
- Go: reason is a raw `uint32` end-to-end (`api/order.go:90-91`, `api/store.go:61`,
  `proto/body_types.go:840,864`, `api/node.go:1885`), with no `TxCancelReason` enum and no string
  table — grep for `crBadSettings|crTimeout|TxCancelReason|cr[A-Z]` finds nothing in Go.

Corrected statement: C++ carries a 0–24 `TxCancelReason` enum (`xbridgepacket.h:21-48`) rendered by
`TxCancelReasonText` (`xbridgeapp.cpp:4052-4107`, incl. the crBadSettings→"crUnknown" and
default→"crNone" bugs), while Go stores the reason as an untyped uint32 with no text mapping.

---

## STATE-F74 No rollback-failed state

VERDICT: **CONFIRMED**.

- C++: `TransactionDescr::State` includes `trRollback=10`, `trRollbackFailed=11`
  (`xbridgetransactiondescr.h:56-57`). `trRollbackFailed` is set in `redeemOrderDeposit` when the
  refund `sendRawTransaction` fails (`xbridgesession.cpp:3908`, "trying again later"); `trRollback`
  is set at `xbridgesession.cpp:3410` and `:3911`.
- Go: `swap/state.go:90-91` and `api/response.go:448-451` define/recognize the ordinal and the
  string "rollback failed", but nothing ever assigns it: the cancel rollback path sets only
  `o.Status = "rolled back"` (`api/node.go:1917-1929`); refund-broadcast failure is logged and
  retried by the sweep (`api/swap.go:760-762`) without ever marking the descriptor
  rollback-failed.

Corrected statement: C++ sets `TransactionDescr::trRollbackFailed` when a cancel-rollback refund
broadcast fails (`xbridgesession.cpp:3908`); Go maps the string (api/response.go:450-451) but never
sets a rollback-failed order status (api/node.go:1917-1929 only ever sets "rolled back").

---

## STATE-F75 No peer penalty / DoS analog

VERDICT: **CONFIRMED**.

- C++: inbound `XBRIDGE` messages are penalized/banned via `Misbehaving` —
  `net_processing.cpp:2877` (10 points for undersized payload, "bad packet, small penalty"), and
  `net_processing.cpp:2904-2907` (`Misbehaving(pfrom->GetId(), dos)` when
  `xapp.onMessageReceived` reports `state.IsInvalid(dos)` with `dos > 0`).
- Go: no score/penalty/ban mechanism anywhere — `p2p/discovery/peer_manager.go` and `p2p/conn.go`
  contain no ban/score concept; the reader drops invalid packets (`api/node.go:556-557`) or the
  conn errors out on an implausible message length (`p2p/conn.go:200`). A bad xbridge peer is
  never penalized or banned, only possibly disconnected.

Corrected statement: C++ scores/bans misbehaving xbridge peers (`net_processing.cpp:2877,2904-2907`);
Go has no penalty/ban analogue (only per-packet drop and connection-level errors).

---

## CRYPTO-F77 BCH sighash

VERDICT: **CONFIRMED** (Go signs with legacy 0x01 on the P2SH refund/claim spends even for BCH).

- C++ BCH connector: `createRefundTransaction`/`createPaymentTransaction` build
  `SigHashType(SIGHASH_ALL).withForkId()` = `0x01|0x40 = 0x41` and append it to the DER sig
  (`xbridgewalletconnectorbch.cpp:396,405` and `:454,463`). `SignatureHash` implements the
  forkid/BIP143 digest with replay protection (`xbridgewalletconnectorbch.cpp:191-215`, `0xdead`
  fork-value xor at :203-209; `checkReplayProtectionEnabled` :501). `SIGHASH_FORKID = 0x40`,
  `SIGHASH_ALL = 1` (:64-67).
- Go: `coins/tx.go:15` `SigHashAll = 0x01`; legacy `HashForSigning` (:349-400) and BIP143
  `HashForSigningSegwit` (:402-478) both stamp `SigHashAll` (0x01); `signDigest` appends the 0x01
  byte (:517-523). No forkid handling — grep for `forkid|0x41` finds nothing. Go signs the HTLC
  refund/claim scriptSigs via `coins.SignTxInput` with 0x01 for every family
  (`api/swap.go:1151,1210`); BCH is supported only at the address/registry layer
  (`coins/coin.go:107-114`, `coins/cashaddr.go`), never in the signing path. (The deposit funding
  inputs are signed by the wallet's `SignRawTransaction` RPC, `api/swap.go:1097`, so the divergent
  byte is specifically the refund/claim sighash.)

Corrected statement: C++ BCH connector signs refund/payment with the forkid sighash 0x41 plus
replay protection (`xbridgewalletconnectorbch.cpp:396,405,454,463`), while Go's `coins/tx.go`
(signing via `SignTxInput`, `api/swap.go:1151,1210`) always emits legacy SIGHASH_ALL 0x01 with no
forkid handling, so a Go-built BCH refund/claim would be rejected by BCH nodes.

---

## CRYPTO-F78 Deposit fee formula

VERDICT: **CONFIRMED** (formulas and the 2-input delta verified).

- C++ `minTxFee1(inputCount, outputCount) = (192*inputCount + 34*outputCount) * feePerByte`
  floored at minTxFee (`xbridgewalletconnectorbtc.cpp:1949-1957`). The deposit is constructed with
  `minTxFee1(usedInTx.size(), 3)` — `xbridgesession.cpp:1993` (maker/CreateA) and `:2526`
  (taker/CreateB).
- Go `estimateFee(cc, nIn, nOut) = (192*nIn + 34*nOut) * FeePerByte` floored at MinTxFee
  (`api/handlers.go:1463-1482`); the deposit calls `estimateFee(cc, len(funding), 2)`
  (`api/swap.go:1064`).
- 2-input deposit: C++ = (192·2 + 34·3)·fpb = 486·fpb; Go = (192·2 + 34·2)·fpb = 452·fpb —
  Go undercounts one output (34·fpb, e.g. 972 vs 904 sat at 2 sat/vB). (fee2 margin uses
  `minTxFee2(1,1)` vs Go `estimateFee(1,1)` = both 226·fpb; matches.)

Corrected statement: deposit fee is C++ `minTxFee1(nIn,3)`=`(192nIn+102)·fpb`
(`xbridgesession.cpp:1993,2526`) vs Go `estimateFee(nIn,2)`=`(192nIn+68)·fpb`
(`api/swap.go:1064`), i.e. Go models one fewer output and charges 34·fpb less (486 vs 452 units
for a 2-input deposit).

---

## RPC-F41 Split fee formula

VERDICT: **CONFIRMED**.

- C++ `splitUtxos` (`xbridgewalletconnectorbtc.cpp:2628-2767`): `fee1 = minTxFee1(1,3)` = 294·fpb
  (:2665), `fee2 = minTxFee2(1,1)` = 226·fpb (:2666), `feesPerUtxo = fee1+fee2` = **520·fpb**
  (:2667), `splitSize = splitAmount + (includeFees ? feesPerUtxo : 0)` (:2668); then the real
  per-tx fee `txFees = minTxFee1(vins.size(), vouts.size())` is deducted from the remainder, with
  a claw-back into the last outputs when the change is dust (:2707-2734).
- Go `splitTx` (`api/handlers.go:1297-1424`): `fee = estimateFee(cc, len(utxos), 2)` =
  (192·nIn + 68)·fpb (:1356), `splitSize = target + fee` when include_fees (:1359-1362),
  `nSplits = total / splitSize` (:1363), `change = total - spent` with dust-zeroing only
  (:1371-1379). No 520·fpb-per-output, and no real-tx-fee deduction from change.

Corrected statement: C++ charges 520·fpb per split output (`feesPerUtxo = minTxFee1(1,3)+minTxFee2(1,1)`,
`xbridgewalletconnectorbtc.cpp:2665-2668`) plus a real-tx-fee claw-back from the change
(:2707-2734); Go instead adds a single whole-tx `(192nIn+68)·fpb` fee to splitSize with no change
deduction (`api/handlers.go:1356-1379`).

---

## CRYPTO-F79 Fee fallback

VERDICT: **CONFIRMED**.

- C++: `feePerByte` defaults to 0 (`xbridgewallet.h:114`; read `s.get<uint64_t>(TICKER+".FeePerByte", 0)`
  `xbridgeapp.cpp:988`) and `minTxFee` defaults to 0 (`xbridgewallet.h:113`, `xbridgeapp.cpp:987`),
  so `minTxFee1 = (192i+34o)*0` = 0 with a 0 floor when FeePerByte is unset — no fallback rate.
- Go: `estimateFee` uses 2 sat/vB when `cc == nil || cc.FeePerByte == 0`
  (`api/handlers.go:1470-1472`).

Corrected statement: with `FeePerByte` unset C++ computes fee 0 (`xbridgeapp.cpp:988`,
`xbridgewallet.h:113-114`) while Go falls back to 2 sat/vB (`api/handlers.go:1470-1472`).

---

## CFG-F84 [Rpc] section misparse

VERDICT: **CONFIRMED**.

- C++: `Settings` exposes `Rpc.Enable/Port/UserName/Password/UseSSL/SertFile/PKeyFile/SslCiphers`
  getters (`util/settings.h:49-65`), but grep across `src/` finds **zero** callers — dead code,
  never consumed by xbridge.
- Go: `config.Load` treats every non-`[Main]` section as a coin (`config/conf.go:111-118`), so a
  real `[Rpc]` section becomes `CoinConf{Ticker:"Rpc", Coin:0}` → `coins.FromConf` fails with
  "COIN not set in xbridge.conf" (`coins/coin.go:87-89`) → `cmd/xbridged/main.go:163-165`
  `fatalf` → `os.Exit(1)` (`main.go:39-43`). (Stock C++ `createConf` template itself does not
  emit `[Rpc]` — `xbridgeapp.cpp:310-372`.)

Corrected statement: C++'s `Rpc.*` settings keys are defined but never read
(`util/settings.h:49-65`), while Go parses any non-Main INI section as a coin
(`config/conf.go:111-118`), so an `[Rpc]` section aborts `xbridged` at startup with the
"COIN not set" fatal error (`coins/coin.go:87-89`, `main.go:163-165`).

---

## CFG-F85 Admission validation gates absent

VERDICT: **CONFIRMED**.

- C++ `updateActiveWallets` (`xbridgeapp.cpp:917-1101`) drops a wallet that fails any gate:
  empty Ip/Port/COIN/blockTime (:1002-1006); maker locktime
  `blockTime*XMIN_LOCKTIME_BLOCKS > XMAKER_LOCKTIME_TARGET_SECONDS` (:1009-1013); taker locktime
  on non-slow chains (:1015-1019); slow-chain locktime
  `blockTime*6 > XSLOW_TAKER_LOCKTIME_TARGET_SECONDS` (:1023-1027); confirmation drift
  `requiredConfirmations > max(XLOCKTIME_DRIFT_SECONDS/blockTime, XMAX_LOCKTIME_DRIFT_BLOCKS)`
  (:1030-1035); blockSize clamped to ≥1024 (:1037-1040); and reachability via `conn->init()`
  (:1129-1136). Constants: `xbridgewallet.h:96-102`.
- Go: connector construction gates only `Ip`/`Port` presence (`wallet/conf.go:15-17`) plus
  `COIN != 0` (`coins/coin.go:87-89`); none of the maker/taker locktime, drift, blockSize, or
  reachability gates exist (`wallet/conf.go:14-36`, `api/node.go:299-307`). Go computes locktimes
  with the same constants for deposit math (`api/swap.go:28-30,1007-1019`) but never applies them
  as wallet admission gates.

Corrected statement: C++ admission gates in `updateActiveWallets`
(`xbridgeapp.cpp:1002-1040` + init-reachability) are absent in Go, which validates only
Ip/Port/COIN presence (`wallet/conf.go:15-17`, `coins/coin.go:87-89`).

---

## CFG-F86 Missing-conf behavior

VERDICT: **CONFIRMED**.

- C++: `init.cpp:1919-1922` calls `xbridge::App::createConf()` (writes the default template if
  absent — `xbridgeapp.cpp:310-372`) before `xapp.init()`/`start()`; `Settings::read` merely
  returns false on a missing/unparseable file (`util/settings.cpp:58-82`) and the daemon continues.
- Go: `config.Load` returns an error when the file cannot be opened (`config/conf.go:98-103`),
  and `cmd/xbridged/main.go:159-162` calls `fatalf` → `os.Exit(1)` (`main.go:39-43`). Go never
  creates the file.

Corrected statement: C++ creates the default xbridge.conf template and continues
(`init.cpp:1920`, `xbridgeapp.cpp:310-372`), while `xbridged` exits(1) when the conf file is
missing (`config/conf.go:98-103`, `cmd/xbridged/main.go:159-162`).

---

## CFG-F87 Reload semantics

VERDICT: **CONFIRMED** (all four claimed differences verified; "partial state on failure" is the
weakest claim — C++ retains prior state on reload failure).

- C++ `dxLoadXBridgeConf` (`rpcxbridge.cpp:195-235`): `loadSettings()` (:229), `clearBadWallets()`
  (:230), `updateActiveWallets()` (:231) — re-runs all admission gates
  (`xbridgeapp.cpp:1002-1040`) and **disconnects wallets not in the new ExchangeWallets list**
  (`xbridgeapp.cpp:931-948`); `clearNonLocalOrders()` when `!showAllOrders()` (:232-233) drops
  non-local orders whose legs lack a live connector (`xbridgeapp.cpp:3811-3826`). On load failure
  `loadSettings` returns false and the daemon keeps its prior state (`xbridgeapp.cpp:502-517`,
  `settings.cpp:73-79`); the RPC returns `false` rather than throwing (:234).
- Go `reloadConf` (`api/node.go:287-334`): on load/parse failure returns the error **before**
  touching `n.config` (last-good swap, :292-298); connectors are keyed off **every `[TICKER]`
  section** (`conf.Coins`), never `ExchangeWallets` (:299-307, startup `main.go:168`); no
  admission gates are applied; no `clearNonLocalOrders` equivalent exists. `ExchangeWallets` is
  used only for RPC display (`api/handlers.go:120,175,800,1062`).

Corrected statement: C++ reload re-applies wallet admission gates, drops wallets leaving
`ExchangeWallets`, and clears non-local orders (`rpcxbridge.cpp:229-233`,
`xbridgeapp.cpp:931-948,3811-3826`) keeping prior state on failure; Go `reloadConf`
(`api/node.go:287-334`) atomically keeps last-good on failure, builds connectors from all
`[TICKER]` sections (never ExchangeWallets), applies no gates, and never clears non-local orders.

---

## CFG-F88 ExchangeWallets parsing

VERDICT: **CONFIRMED**.

- C++ `Settings::exchangeWallets()` splits on the character set `",;:"` and validates/normalizes
  each symbol (`util/settings.cpp:143-166`).
- Go `parseMain` splits on `,` only, with no symbol validation (`config/conf.go:232-249`).

Corrected statement: C++ splits `Main.ExchangeWallets` on `,;:` and validates symbols
(`util/settings.cpp:143-166`); Go splits on `,` only with no validation
(`config/conf.go:232-249`).

---

## CONC-F93 Goroutine lifecycle

VERDICT: **CONFIRMED** for both sub-claims (with the note that the daemon flushes dedupe on
shutdown).

(a) Discovery goroutines signalled but never joined:
- `p2p/discovery/peer_manager.go`: `maintain` (:128, :161-179), `connectOne` (:201, :249-277) and
  `readLoop` (:270, :280-349) goroutines are all stopped via the `m.done` channel closed by
  `Close()` (:433-448); there is **no WaitGroup** in the package. `api/Node.Close`
  (`api/engine.go:268-281`) closes `n.stop` and `n.conn` (→ `pm.Close`) then `n.wg.Wait()`, where
  `n.wg` tracks only reader/engine/blockLoop/statusLoop/workers (`api/engine.go:93,227-250`), not
  the discovery goroutines. `pm.Start` is given `context.Background()` (`api/node.go:262`), so
  `ctx.Done` never fires.

(b) Dedupe sweep goroutine:
- `log/dedup.go:122-148`: `startSweepLocked` lazily spawns `sweep`, which returns only when
  `stopCh` closes; `stopCh` is closed only in `Flush` (:189-200). Any `*Dedupe` that never has
  `Flush`/`FlushAll` called leaks its sweep goroutine for process lifetime. In the daemon the leak
  is avoided on both the graceful path (`xlog.FlushAll()` at `cmd/xbridged/main.go:263`) and the
  fatal path (`fatalf`, `main.go:41`); a library consumer that never flushes leaks it.

Corrected statement: discovery peer goroutines (`maintain`/`connectOne`/`readLoop`) are signalled
via `m.done` but never joined (`p2p/discovery/peer_manager.go:128,201,270,433-448`; no WaitGroup),
and the Dedupe sweeper goroutine (`log/dedup.go:137-148`) terminates only on `Flush` — both leaks
are real at the library level, though the daemon calls `FlushAll` on shutdown
(`cmd/xbridged/main.go:41,263`).

---

## Findings whose claims were substantially corrected

- **STATE-F76** — claim "swap/state.go Transaction::State is only used by tests/helpers" is slightly
  imprecise: `IncreaseStateCounter` is also used by `swap/session.go` (itself dead from the live
  path); and the divergence is explicitly judged an internal-implementation choice with the same
  observable transitions, not a behavioral divergence.
- **STATE-F71** — Go partially compensates via per-packet hub-key verification (`api/node.go:770-777`),
  which the original finding omitted; the impact is bounded to a pinned-but-malicious hub, and Go
  additionally never implements C++'s Hold-time partial-fill amount adoption.
- **STATE-F72** — expired orders are hidden from `dxGetOrders` (`api/handlers.go:110-114`), so the gap
  is "no periodic prune" rather than "expiry entirely invisible".
- **CFG-F87** — C++ on reload failure keeps prior state (returns `false`), which is a last-good
  behavior too; the real divergences are the gates, the ExchangeWallets-vs-all-sections connector
  keying, and the missing clearNonLocalOrders.
- **CONC-F93(b)** — the daemon's graceful shutdown does call `xlog.FlushAll()`
  (`cmd/xbridged/main.go:263`), so the Dedupe leak is a library-level defect, not observed in the
  shipped daemon's normal shutdown.

---

Report written to: `docs/audit/verify/verify_d.md`
