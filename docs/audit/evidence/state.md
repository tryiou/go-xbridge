# STATE MACHINE & LIFECYCLE — Blocknet Core xBridge (C++) vs go-xbridge (Go) conformance audit

Scope: Transaction/Order state machine, transition table, TryJoin/matching, TTL/expiry,
locktime computation, cancel/rollback/penalty, and handshake ordering.

Reference (source of truth): `blocknet_core/src/xbridge/`
Candidate: `go-xbridge/swap/`, `go-xbridge/api/`

Legend: **MATCH** = conformant, **DIVERGE** = behavioral or numeric difference,
**GAP** = present on one side only, **NOTE** = observation/architectural difference.
Every claim cites `file:line` on both sides.

---

## CARD 1 — STATE ENUM ORDINALS & STRING RENDERING

### 1.1 `Transaction::State` (0..10) vs `swap.State`

| Ordinal | C++ name | Go name | Verdict |
|---|---|---|---|
| 0 | `trInvalid` | `TrInvalid` | MATCH |
| 1 | `trNew` | `TrNew` | MATCH |
| 2 | `trJoined` | `TrJoined` | MATCH |
| 3 | `trHold` | `TrHold` | MATCH |
| 4 | `trInitialized` | `TrInitialized` | MATCH |
| 5 | `trCreated` | `TrCreated` | MATCH |
| 6 | `trSigned` | `TrSigned` | MATCH |
| 7 | `trCommited` | `TrCommited` | MATCH |
| 8 | `trFinished` | `TrFinished` | MATCH |
| 9 | `trCancelled` | `TrCancelled` | MATCH |
| 10 | `trDropped` | `TrDropped` | MATCH |

C++: `blocknet_core/src/xbridge/xbridgetransaction.h:36-50`.
Go: `go-xbridge/swap/state.go:20-32`. All 11 ordinals match.

**String rendering (strState):**
- C++ `Transaction::strState` table: `xbridgetransaction.cpp:197-202` →
  `"trInvalid","trNew","trJoined","trHold","trInitialized","trCreated","trSigned","trCommited","trFinished","trCancelled","trDropped"`.
- Go `strStates`: `swap/state.go:35-38` — byte-for-byte identical, including the C++
  one-`m` spelling **"trCommited"** (not "trCommitted"). MATCH.
- The task's example (`"Invalid"` vs `"trInvalid"`) does not occur: both sides use `trInvalid`.

**Out-of-range rendering — DIVERGE (minor):**
- C++: `return states[state];` with no bounds check (`xbridgetransaction.cpp:204`) — UB/out-of-bounds read.
- Go: `fmt.Sprintf("trUnknown(%d)", int(s))` for `s < 0 || s >= len(strStates)` (`swap/state.go:42-44`).
- Go is strictly safer; behavior on an illegal ordinal is intentionally divergent (no crash vs UB).

### 1.2 `TransactionDescr::State` (-1..14) vs `swap.DescrState`

| Ordinal | C++ name | C++ strState | Go name | Go String() | Verdict |
|---|---|---|---|---|---|
| -1 | `trExpired` | `"expired"` | `DescrExpired` | `"expired"` | MATCH |
| 0 | `trNew` | `"new"` | `DescrNew` | `"new"` | MATCH |
| 1 | `trOffline` | `"offline"` | `DescrOffline` | `"offline"` | MATCH |
| 2 | `trPending` | `"open"` | `DescrOpen` | `"open"` | MATCH (name differs) |
| 3 | `trAccepting` | `"accepting"` | `DescrAccepting` | `"accepting"` | MATCH |
| 4 | `trHold` | `"hold"` | `DescrHold` | `"hold"` | MATCH |
| 5 | `trInitialized` | `"initialized"` | `DescrInitialized` | `"initialized"` | MATCH |
| 6 | `trCreated` | `"created"` | `DescrCreated` | `"created"` | MATCH |
| 7 | `trSigned` | `"signed"` | `DescrSigned` | `"signed"` | MATCH |
| 8 | `trCommited` | `"commited"` | `DescrCommited` | `"commited"` | MATCH |
| 9 | `trFinished` | `"finished"` | `DescrFinished` | `"finished"` | MATCH |
| 10 | `trRollback` | `"rolled back"` | `DescrRollback` | `"rolled back"` | MATCH |
| 11 | `trRollbackFailed` | `"rollback failed"` | `DescrRollbackFailed` | `"rollback failed"` | MATCH |
| 12 | `trDropped` | `"dropped"` | `DescrDropped` | `"dropped"` | MATCH |
| 13 | `trCancelled` | `"canceled"` | `DescrCancelled` | `"canceled"` | MATCH |
| 14 | `trInvalid` | `"invalid"` | `DescrInvalid` | `"invalid"` | MATCH |

C++ enum: `xbridgetransactiondescr.h:43-61`; C++ strState: `xbridgetransactiondescr.h:665-688`.
Go enum: `swap/state.go:78-95`; Go names map: `swap/state.go:99-116`; String(): `swap/state.go:119-156`.

**Naming difference (ordinal 2):** Go renames `trPending` → `DescrOpen`. Ordinal and the rendered
string both match (`"open"`); the Go source documents the mapping at `swap/state.go:82`
(`DescrOpen DescrState = 2 // C++ trPending`). MATCH in behavior, NOTE in identifier.

**String fidelity details that DO match:** C++ renders `trCancelled` as **"canceled"** (one `l`,
`xbridgetransactiondescr.h:683`); Go uses `"canceled"` (`state.go:149-150`, `response.go:454`).
C++ renders `trCommited` as **"commited"** (`:678`); Go `"commited"` (`state.go:139`, `response.go:444`).

**Default (unknown) rendering — DIVERGE (minor):**
- C++: `return std::string("unknown");` (`xbridgetransactiondescr.h:686`).
- Go: `fmt.Sprintf("descrState(%d)", int(s))` (`swap/state.go:153-155`).
- The dx* surface normalizes through `statusString()` (`api/response.go:424-461`), whose `default`
  branch returns `"unknown"` — so the wire/JSON rendering still agrees; only the Go enum's
  direct `String()` differs.

**statusString / stateOrdinal bridge:** Go maps status strings → ordinals via
`statusString` (`api/response.go:424-461`) and `stateOrdinal` (`api/response.go:468-470`,
delegating to `swap.DescrStateOrdinal`, `state.go:167-172`). Ordinal map is verified
1:1 against the C++ enum in `go-xbridge/api/divergence_test.go:47`.

---

## CARD 2 — TRANSITION TABLE

Both machines drive the same 9 reachable states via the same two-confirmation gate.

### C++ reference transitions

| # | from | to | trigger | precondition | WHO confirms | C++ cite |
|---|---|---|---|---|---|---|
| T1 | trInvalid | trNew | order constructor (maker broadcast → hub `createTransaction`) | — | Maker (single) | `xbridgetransaction.cpp:63`; `xbridgeexchange.cpp:300-412` |
| T2 | trNew | trJoined | taker Accepting → hub `acceptTransaction` → `tryJoin` | both orders trNew; currencies flip; partial flags equal; amounts/drift OK | Hub (single, after taker) | `xbridgeexchange.cpp:416-519`; `xbridgetransaction.cpp:490-545` |
| T3 | trJoined | trHold | both `HoldApply` packets → `increaseStateCounter(trJoined)` | state==trJoined; **both** `a.source()` and `b.source()` marked | **BOTH A and B** (Source addrs) | `xbridgetransaction.cpp:120-135`; `xbridgesession.cpp:1620` |
| T4 | trHold | trInitialized | both `Initialized` packets → `increaseStateCounter(trHold)` | state==trHold; **both** `a.dest()` and `b.dest()` marked | **BOTH A and B** (Dest addrs) | `xbridgetransaction.cpp:137-153`; `xbridgesession.cpp:1850` |
| T5 | trInitialized | trCreated | `CreatedA`+`CreatedB` → `increaseStateCounter(trInitialized)` | state==trInitialized; **both** `a.source()` and `b.source()` marked | **BOTH A and B** (Source addrs) | `xbridgetransaction.cpp:154-170`; `xbridgesession.cpp:2343`/`:2821` |
| T6 | trCreated | trFinished | `ConfirmedA`+`ConfirmedB` → `increaseStateCounter(trCreated)` | state==trCreated; **both** `a.dest()` and `b.dest()` marked | **BOTH A and B** (Dest addrs) | `xbridgetransaction.cpp:171-187`; `xbridgesession.cpp:3082`/`:3264` |
| T7 | any | trCancelled | `cancel()` — cancel packet / reject / timeout | — | either party / hub | `xbridgetransaction.cpp:321-327` |
| T8 | trCancelled | trDropped | `drop()` from `checkFinishedTransactions` | state==trCancelled (or invalid) | hub | `xbridgetransaction.cpp:331-336`; `xbridgesession.cpp:3712-3715` |
| T9 | any | trFinished | `finish()` + `xbcTransactionFinished` broadcast | — | hub | `xbridgetransaction.cpp:340-345`; `xbridgesession.cpp:3489-3514` |

### Go candidate transitions

| # | from | to | trigger | precondition | WHO confirms | Go cite |
|---|---|---|---|---|---|---|
| T1 | TrInvalid | TrNew | `NewTransaction` | — | Maker (single) | `swap/transaction.go:61-77` |
| T2 | TrNew | TrJoined | `TryJoin` (via `tryJoinMatches`) | both TrNew; currencies flip; partial flags equal; drift/amounts OK | Hub (single, after taker) | `swap/transaction.go:85-128` |
| T3 | TrJoined | TrHold | `IncreaseStateCounter(TrJoined)` — both `A.Source`+`B.Source` marked | state==TrJoined | **BOTH** | `swap/transaction.go:147-152` |
| T4 | TrHold | TrInitialized | `IncreaseStateCounter(TrHold)` — both `A.Dest`+`B.Dest` marked | state==TrHold | **BOTH** | `swap/transaction.go:153-158` |
| T5 | TrInitialized | TrCreated | `IncreaseStateCounter(TrInitialized)` — both `A.Source`+`B.Source` marked | state==TrInitialized | **BOTH** | `swap/transaction.go:159-164` |
| T6 | TrCreated | TrFinished | `IncreaseStateCounter(TrCreated)` — both `A.Dest`+`B.Dest` marked | state==TrCreated | **BOTH** | `swap/transaction.go:165-170` |
| T7 | any | TrCancelled | `Cancel()` | — | either party / hub | `swap/transaction.go:194-197` |
| T8 | TrCancelled | TrDropped | `Drop()` | — | hub | `swap/transaction.go:200-203` |
| T9 | any | TrFinished | `Finish()` | — | hub | `swap/transaction.go:206-209` |

### Gate comparison
- **Two-party confirmation gate — MATCH.** C++ advances a phase only when
  `m_a_stateChanged && m_b_stateChanged` (`xbridgetransaction.cpp:130-131,147-148,164-165,181-182`).
  Go requires the identical `t.changedA && t.changedB` (`swap/transaction.go:149,155,161,167`),
  with the same per-phase address set (Source for T3/T5, Dest for T4/T6) and the same
  reset-after-transition (`mark`/`reset`, `swap/transaction.go:180-188`; C++
  `m_a_stateChanged = m_b_stateChanged = false`, `xbridgetransaction.cpp:133,150,167,184`).
  A mismatch (state != current) returns `trInvalid` on both (`xbridgetransaction.cpp:189`;
  `swap/transaction.go:142-145,171-174`).
- **Transitions present in one but not the other — none.** `trSigned`/`trCommited` (6,7) are
  declared on both sides and assigned by neither. The Go port documents this:
  `swap/session.go:17-25` (FIDELITY NOTE: vestigial, deposits gate the progression instead).
- **NOTE (architecture):** the live Go thin-client handshake (`api/swap.go`) does NOT drive
  `swap.Transaction`/`swap.Session`. The hub owns the authoritative machine; the client tracks
  only a local `clientState` (`api/swap.go:40-53`, `csIdle..csFinished`) and the authoritative
  `swap.State` machine is exercised through `swap.Session.advance()` (`swap/session.go:127-163`)
  and unit tests only. `Session.advance` mirrors the C++ hub gate sequence (Source→Dest→Source→Dest)
  and is not on the production path. `swap.Transaction.IncreaseStateCounter` itself is a faithful
  1:1 port and is correct wherever used.

---

## CARD 3 — TRYJOIN / MATCHING

### 3.1 Hub-side join (`Transaction::tryJoin` vs `swap.Transaction.TryJoin`)

| check | C++ | Go | Verdict |
|---|---|---|---|
| both trNew | `xbridgetransaction.cpp:495-499` | `swap/transaction.go:86-88` | MATCH |
| currency pair (source==other.dest && dest==other.source) | `xbridgetransaction.cpp:501-506` | `swap/transaction.go:89-91` | MATCH |
| partial-allowed flags equal | `xbridgetransaction.cpp:508-511` | `swap/transaction.go:92-94` | MATCH |
| partial price drift check | `xBridgePartialOrderDriftCheck(m_sourceAmount, m_destAmount, other->m_sourceAmount, other->m_destAmount)` `xbridgetransaction.cpp:515-522` | `PartialOrderDriftCheck(t.SourceAmount, t.DestAmount, o.SourceAmount, o.DestAmount)` `swap/transaction.go:97-99` | MATCH |
| taker not too large (`source < other.dest || dest < other.source`) | `xbridgetransaction.cpp:524-526` | `swap/transaction.go:103-105` | MATCH |
| minFromAmount (`other.destAmount < m_minPartialAmount`) | `xbridgetransaction.cpp:527-530` | `swap/transaction.go:106-108` | MATCH |
| exact-order equality | `xbridgetransaction.cpp:533-537` | `swap/transaction.go:111-113` | MATCH |
| join member + state | `m_b = other->m_a; m_state = trJoined` `xbridgetransaction.cpp:540-544` | `t.B = o.A; t.State = TrJoined` `swap/transaction.go:124-126` | MATCH |

Resulting state on success: `trJoined` on both. MATCH.

### 3.2 Price / drift tolerance
- C++ `xBridgePartialOrderDriftCheck`: `xutil.cpp:338-379`. Exact match short-circuits
  (`:341-342`); otherwise derives expected amounts via `xBridgeSourceAmountFromPrice` /
  `xBridgeDestAmountFromPrice` (`xutil.cpp:314-336`) and, when amounts are not evenly
  divisible, applies a ±1 amount drift band (`:362-378`).
- Go `PartialOrderDriftCheck`: `swap/price.go:78-117`; `priceSource`/`priceDest`:
  `swap/price.go:33-55` (same `*c`, `+1`, `/c`, `<1→1` integer arithmetic as `xutil.cpp:314-336`).
- Drift tolerance is **satoshi-level, not a price-equality requirement** — the derived-price
  band is identical on both sides (exact match when divisible, ±1-unit band otherwise).
  MATCH. (C++ comment at `xutil.cpp:354-355` documents the "100 sats to 1000 sats or more"
  forgiveness; Go port note `swap/price.go:71-77`.)

### 3.3 Partial-order re-checks and where they live
- **Hub-side size gates in `processTransactionAccepting`** (C++ only, not in `tryJoin`):
  `samount < b_amount` → `crNoMoney`; `damount < a_amount` on exact → `crNotAccepted`;
  partial `damount < min_partial_amount` → `crNoMoney`; `damount > a_amount` → `crNotAccepted`;
  `samount > b_amount` → `crNotAccepted` (`xbridgesession.cpp:1140-1214`).
  Go (thin client, no hub role) covers the taker-side mirrors in `TakeOrder`: min/max amount
  gates `a < o.MinFromAmount` / `a > o.FromAmount` (`api/node.go:1364-1369`) and partial
  recompute via `xBridgeSourceAmountFromPrice` (`api/node.go:1372`). The C++ **client-side**
  re-verification of the Hub Hold packet (`processTransactionHold`, `xbridgesession.cpp:1401-1471`:
  role-B exact from/to match; role-A bounds + partial drift vs `origFromAmount`) has **no
  counterpart**: Go `OnHold` parses `proto.HoldBody` (which carries `FromAmount`/`ToAmount`,
  `go-xbridge/proto/body_types.go:367-372`) but never validates them
  (`api/swap.go:283-299`). **GAP — candidate finding (hold no re-verify).**
- Order-id integrity: C++ hashes the order body and compares to the packet id
  (`xbridgesession.cpp:600-618`); Go derives the id at make time
  (`sha256dOrderID`, `api/node.go:1065-1067`). Out of card scope but consistent.

---

## CARD 4 — TIMEOUT / EXPIRY SEMANTICS

### 4.1 TTL constants — all match numerically

| constant | C++ value | C++ cite | Go value | Go cite | Verdict |
|---|---|---|---|---|---|
| lockTime | 600 (=60*10) | `xbridgetransaction.h:54-55` | `LockTime = 60*10` = 600 | `swap/state.go:176-177` | MATCH |
| pendingTTL | 360 (=60*6) | `xbridgetransaction.h:57-58` | `PendingTTL = 60*6` = 360 | `swap/state.go:178-179` | MATCH |
| TTL | 3600 (=60*60) | `xbridgetransaction.h:60-61` | `TTL = 60*60` = 3600 | `swap/state.go:180-181` | MATCH |
| deadlineTTL | 604800 (=60*60*24*7) | `xbridgetransaction.h:63-64` | `DeadlineTTL = 60*60*24*7` = 604800 | `swap/state.go:182-183` | MATCH |
| blocksTTL | 10080 (=1440*7) | `xbridgetransaction.h:66-67` | `BlocksTTL = 1440*7` = 10080 | `swap/state.go:184-185` | MATCH |

### 4.2 `isExpired` comparison semantics — MATCH

C++ `Transaction::isExpired` (`xbridgetransaction.cpp:268-284`):
- `trNew && tdCreated.total_seconds() > deadlineTTL` → expired
- `trNew && tdLast.total_seconds() > pendingTTL` → expired
- `state > trNew && tdLast.total_seconds() > TTL` → expired

Go `Transaction.IsExpired` (`swap/transaction.go:219-227`):
- `TrNew: ageCreated > DeadlineTTL || ageLast > PendingTTL`
- `State > TrNew && ageLast > TTL`

Both use **strict `>`** (a transaction exactly at the TTL is NOT expired). Units: C++
`posix_time::time_duration.total_seconds()` vs Go `time.Now().Unix()` int64 seconds — equivalent
magnitude. The `updateTooSoon()` rate-limit (`xbridgetransaction.cpp:225-230`,
`< pendingTTL/2` = 180s) is mirrored only conceptually; the Go engine has no
`updateTimestampOrRemoveExpired` rate-limit gate (see 4.4).

### 4.3 `isExpiredByBlockNumber` — MATCH with a thin-client delta

C++ (`xbridgetransaction.cpp:288-311`):
- short-circuit `if (gtNew && !isFinished()) return false;` (`:290-296`)
- `LookupBlockIndex(m_blockHash)` missing → expired (`:300-302`)
- `lastBlockHeight - trBlockHeight > blocksTTL` → expired (`:307-308`)

Go (`swap/transaction.go:241-255`):
- same short-circuit `if t.State > TrNew && !t.IsFinished() { return false }` (`:245-247`)
- `int64(currentBlock)-int64(t.BlockNumber) > BlocksTTL` (`:255`)

MATCH. The "block hash not in our chain → expired" branch is inapplicable to a thin client
(`BlockNumber` tracked directly); the Go comment documents this (`swap/transaction.go:250-254`).

### 4.4 Sweep / prune triggers — DIVERGE (structural GAP)

C++ periodic driver (`App::Impl::onTimer`, `TIMER_INTERVAL = 15s`, `xbridgeapp.cpp:90,271,3750`):
- `checkFinishedTransactions` every tick (`:3670`, impl `xbridgesession.cpp:3696-3742`; timeout →
  `sendCancelTransaction(ptr, crTimeout)` at `:3739`)
- `checkAndEraseExpiredTransactions` every tick (`:3684`, impl `xbridgeapp.cpp:3573-3654`;
  calls `Exchange::eraseExpiredTransactions`, `xbridgeexchange.cpp:712-747`, which prunes the
  pending book by `isExpiredByBlockNumber()` then `isExpired()` and unlocks UTXOs)
- client-side descriptor sweep: trNew→trOffline past pendingTTL, trPending→trExpired, reverse
  when refreshed, TTL/deadline erase (`xbridgeapp.cpp:3604-3634`)
- `saveOrders` every 4th tick (~60s, `:3744-3746`)

Go periodic driver (`engineLoop`, `api/engine.go:158-192`, ticker = `refundCheckInterval` 60s):
- `scanRefunds()` refund sweep (`engine.go:179`, impl `api/swap.go:821-836`)
- `pruneSessions()` terminal-session sweep (`engine.go:182`, impl `api/swap.go:871-879`)
- `persist()` every 4th tick (`engine.go:185-187`) → ~240s cadence
- **NO order-book expiry sweep.** `IsExpired`/`IsExpiredByBlockNumber` exist
  (`swap/transaction.go:219-255`) and are unit-tested (`swap/transaction_test.go:194-306`) but
  are **never called** from the api/engine layer (grep: no caller outside the swap package and
  its tests). The `Store` never prunes open orders by TTL/block-height; nothing ever writes a
  status of `"expired"` or `"offline"` in Go. `dxGetOrders` only *filters* `"expired"` strings
  that are never produced (`api/handlers.go:108-114`).
  **GAP — candidate finding (no live expiry sweep).**
- Interval mismatch: C++ 15s timer vs Go 60s ticker; C++ persist ~60s vs Go ~240s.
  Minor timing DIVERGE (refund sweep cadence is 60s in both semantics: C++ deposit-spend
  watch runs every ~15s `:3689-3691`, Go refund sweep every 60s).

---

## CARD 5 — LOCKTIME COMPUTATION

### 5.1 Constants — all match

| constant | C++ value | C++ cite | Go value | Go cite | Verdict |
|---|---|---|---|---|---|
| XMIN_LOCKTIME_BLOCKS | 6 | `xbridgewallet.h:96` | `xMinLockTimeBlocks = 6` | `api/swap.go:28` | MATCH |
| XMAX_LOCKTIME_DRIFT_BLOCKS | 4 | `xbridgewallet.h:97` | `xMaxLockTimeDriftBlocks = 4` | `api/locktime.go:14` | MATCH |
| XMAKER_LOCKTIME_TARGET_SECONDS | 7200 (2h) | `xbridgewallet.h:98` | `makerLockTimeSec = 7200` | `api/swap.go:24` | MATCH |
| XTAKER_LOCKTIME_TARGET_SECONDS | 1800 (30m) | `xbridgewallet.h:99` | `takerLockTimeSec = 1800` | `api/swap.go:25` | MATCH |
| XSLOW_TAKER_LOCKTIME_TARGET_SECONDS | 3600 (1h) | `xbridgewallet.h:100` | `xSlowTakerLockTimeSec = 3600` | `api/swap.go:29` | MATCH |
| XSLOW_BLOCKTIME_SECONDS | 600 | `xbridgewallet.h:101` | `xSlowBlockTimeSec = 600` | `api/swap.go:30` | MATCH |
| XLOCKTIME_DRIFT_SECONDS | 900 (=1800/2) | `xbridgewallet.h:102` | `xLockTimeDriftSeconds = 900` | `api/locktime.go:13` | MATCH |
| LOCKTIME_THRESHOLD | 500000000 | `script/script.h:39` | `lockTimeThreshold = 500_000_000` | `api/locktime.go:12` | MATCH |

(Header-comment staleness: `xbridgesession.cpp:55-56` and `xbridgepacket.h:55` carry a
commented-out `LOCKTIME_THRESHOLD`; the live definition is `script/script.h:39`. The Go port
used the live value — correct.)

### 5.2 Formula — MATCH

C++ `BtcWalletConnector::lockTime(role)` (`xbridgewalletconnectorbtc.cpp:2286-2326`):
- role 'A': `blocks = XMAKER_LOCKTIME_TARGET_SECONDS / blockTime`, `if (blocks < XMIN) blocks = XMIN`,
  `lt = info.blocks + blocks` (`:2308-2314`)
- role 'B': `takerTime = XTAKER...; if (blockTime >= XSLOW_BLOCKTIME_SECONDS) takerTime = XSLOW_TAKER...`,
  clamp, `lt = info.blocks + blocks` (`:2315-2323`)

Go `swapCtx.computeLockTimeFor(cur, isMaker)` (`api/swap.go:993-1021`):
- `target = makerLockTimeSec`; `if !isMaker { target = takerLockTimeSec; if bt >= xSlowBlockTimeSec { target = xSlowTakerLockTimeSec } }`
  (`:1007-1015`)
- `blocks := target / bt; if blocks < xMinLockTimeBlocks { blocks = xMinLockTimeBlocks }`
  (`:1016-1019`)
- `return uint32(n) + uint32(blocks)` (`:1020`)

Identical: `currentBlock + clamp(target/blockTime, min 6)`. Slow-chain (blockTime≥600s) taker
escalation identical. MATCH.

### 5.3 Drift check — MATCH (one-sided on both)

C++ `acceptableLockTimeDrift` (`xbridgewalletconnectorbtc.cpp:2331-2345`):
```
lt = lockTime(role)
if (lt == 0 || lt >= LOCKTIME_THRESHOLD || lckTime >= LOCKTIME_THRESHOLD) return false;
diff = lt - lckTime;                 // blocks
drift = max(XLOCKTIME_DRIFT_SECONDS, XMAX_LOCKTIME_DRIFT_BLOCKS * blockTime);
return diff * blockTime <= drift;
```
Go `acceptableLockTimeDrift` (`api/locktime.go:29-39`): byte-for-byte the same arithmetic and
the same strict one-sided direction — only `ourLT - theirLT` is checked; a counterparty
lockTime *below* ours fails, *above* ours passes (Go doc `api/locktime.go:25-28`). MATCH.

### 5.4 Call sites (drift enforced where C++ enforces it)

| phase | C++ | Go | verdict |
|---|---|---|---|
| taker checks maker A locktime (CreateB) | `xbridgesession.cpp:2462-2474` (`connTo->acceptableLockTimeDrift('A', lockTimeA)`, fail → `crBadALockTime` cancel) | `api/swap.go:442-444` (expectation `computeLockTimeFor(dstCur, true)`, fail → error) | MATCH |
| maker checks taker B locktime (ConfirmA) | `xbridgesession.cpp:2924-2936` (`connTo->acceptableLockTimeDrift('B', lockTimeB)`, fail → `crBadBLockTime` cancel) | `api/swap.go:536-538` (expectation `computeLockTimeFor(dstCur, false)`, fail → error) | MATCH |

Threshold constants involved (500000000 / 900s / 4 blocks) match on both sides (5.1).
C++ computes the expectation with the connector's own `blockTime` (conf-driven,
`xbridgewallet.h` WalletParam.blockTime); Go uses per-coin `cc.BlockTime` (default 60)
`api/swap.go:1003-1006` — same source of truth (xbridge.conf `[TICKER].BlockTime`).

---

## CARD 6 — CANCEL / ROLLBACK / PENALTY

### 6.1 TxCancelReason enum — **GAP (not ported)**

C++ 25-value enum, `xbridgepacket.h:21-48`:

| ord | name | ord | name |
|---|---|---|---|
| 0 | crUnknown | 13 | crBlocknetError |
| 1 | crBadSettings | 14 | crBadADepositTx |
| 2 | crUserRequest | 15 | crBadBDepositTx |
| 3 | crNoMoney | 16 | crTimeout |
| 4 | crBadUtxo | 17 | crBadLockTime |
| 5 | crDust | 18 | crBadALockTime |
| 6 | crRpcError | 19 | crBadBLockTime |
| 7 | crNotSigned | 20 | crBadAUtxo |
| 8 | crNotAccepted | 21 | crBadBUtxo |
| 9 | crRollback | 22 | crBadARefundTx |
| 10 | crRpcRequest | 23 | crBadBRefundTx |
| 11 | crXbridgeRejected | 24 | crBadFeeTx |
| 12 | crInvalidAddress | | |

Go: **no TxCancelReason enum, no ordinal constants, no reason→string table anywhere.**
The Go client always broadcasts `reason = 0` (`api/node.go:1702,1781` — the same ordinal as
`crUnknown`) and stores inbound reasons as opaque `uint32` (`api/order.go:90-91`,
`api/store.go:211-214`). **GAP — candidate finding (cancel reasons not ported).**

### 6.2 Reason strings (`TxCancelReasonText`) — **GAP (not ported) + C++ bugs**

C++ `TxCancelReasonText` (`xbridgeapp.cpp:4052-4107`):
- Known C++ bug 1: `case crBadSettings: return "crUnknown";` (`:4055-4056`) — renders as
  "crUnknown", not "crBadSettings".
- Known C++ bug 2: `case crUnknown: default: return "crNone";` (`:4103-4105`) — `crUnknown`(0)
  and any out-of-enum reason render as "crNone".
- Every other case returns `"cr" + name` (`crUserRequest`, `crNoMoney`, … `crBadFeeTx`).

Go: no equivalent function; cancel reason is never rendered to a string (status carries
`"canceled"`/`"rolled back"`, reason field is raw `uint32`). **GAP.** The C++ rendering bugs
above therefore have no Go counterpart to be conformant (or non-) with — flag as unported.

### 6.3 State on cancel

| path | C++ | Go | verdict |
|---|---|---|---|
| exchange (hub) cancel | `tx->cancel()` → trCancelled; `deletePendingTransaction`; broadcast `xbcTransactionCancel` | `sendCancelTransaction` builds+signs+broadcasts Cancel (`api/node.go:1773-1790`); `CancelOrder` sets status `"canceled"` + `RecordCancelled` + `enqueueRefund` (`api/node.go:1716-1739`) | MATCH structure; Go sets order status (Descr layer), C++ sets Transaction layer |
| client, state < trCreated | `moveTransactionToHistory` + trCancelled (`xbridgesession.cpp:3384-3388`) | `MoveToHistory("canceled")` (`api/node.go:1884-1887`) | MATCH |
| client, already canceled | ignore (`:3389-3391`) | `if o.Status == "canceled" { return }` (`api/node.go:1888-1890`) | MATCH |
| client, deposit not sent | cancel → trCancelled (`:3392-3394`) | status `"canceled"` (`api/node.go:1891-1899`) | MATCH |
| client, counterparty redeemed | ignore (`:3395-3397`) | `if o.CounterpartyRedeemed { return }` (`api/node.go:1900-1903`) | MATCH |
| client, no refund tx | cancel → trCancelled (`:3400-3404`) | status `"canceled"` (`api/node.go:1906-1915`) | MATCH |
| client, refund available | trRollback; `redeemOrderDeposit` (CLTV refund after locktime); failure → trRollbackFailed (`:3406-3420`, `:3843-3918`) | status `"rolled back"` + `enqueueRefund` (`api/node.go:1917-1929`); **failure never writes "rollback failed"** (fire-and-forget) | MATCH core; **minor DIVERGE: no trRollbackFailed write** |
| open/pending rebroadcast on another SN (local order, not self-cancelled) | `setUpdateTime(now-241s)` then rebroadcast (`:3379-3383`) | `markStale` sets `Updated = now - 241_000_000` (`api/node.go:1795-1797`, used `:1880-1883`) | MATCH |
| reject (taker-side) | restore to trPending, clear role/keys, unlock coins (`:3432-3485`) | `handleRemoteReject`: status `"open"`, `clearUsedCoins`, unlock stubs (`api/node.go:1942-1975`) | MATCH |

Rollback refund sweep trigger: C++ `redeemOrderDeposit` gates on `blockCount < xtx->lockTime`
then returns false and retries via `processLater` (`xbridgesession.cpp:3875-3888`); Go
`scanRefunds` gates on `uint32(h) < lockTime` and retries next tick (`api/swap.go:725-740,
821-836`). Semantics MATCH; C++ also has `refundTraderDeposit` + `watchTraderDeposits` and the
`redeemOrderCounterpartyDeposit` retry machinery (out of state-machine scope).

### 6.4 Penalty / banning

C++:
- `Misbehaving(pfrom->GetId(), 10)` for any undersized xbridge packet
  (`net_processing.cpp:2874-2878`).
- `Misbehaving(pfrom->GetId(), dos)` when a processed packet marks `CValidationState` invalid
  with `dos > 0` (`net_processing.cpp:2901-2908`). The only xbridge session DoS call is
  `state->DoS(0, error("Xbridge packet processing error"), ...)` (`xbridgesession.cpp:312`) —
  a **0** penalty, so no actual ban derives from malformed-but-well-sized packets.
- `smgr.processXBridge(raw)` gate runs first (`net_processing.cpp:2885`).

Go: **no banning/penalty counter anywhere.** The reader drops packets with bad signatures or
undecodable bodies with a Warn/Debug log (`api/engine.go:130-141`); no peer misbehaviour score,
no `Misbehaving` analog, no disconnect threshold. **DIVERGE — candidate finding (no peer
penalty; per-packet DoS of 0 in C++ means the *effective* outcome is nearly identical —
bad content is dropped on both sides, but C++ additionally applies +10 misbehavior to
sub-min-size packets).**

---

## CARD 7 — HANDSHAKE ORDERING

### 7.1 Successful swap — packet sequence (hub-relayed, three-party)

C++ wire order (hub `xbridgesession.cpp` handlers; command enum `xbridgepacket.h:52-282`):

```
 Maker ─────────── Hub (service node) ─────────── Taker
   │  (3) xbcTransaction          │                  │
   │ ───────────────────────────> │                  │
   │                              │ (4) xbcPendingTransaction
   │                              │ ─────────────────────────────────> │
   │                              │ (5) xbcTransactionAccepting        │
   │                              │ <───────────────────────────────── │
   │                              │  tryJoin -> trJoined (:485)
   │                              │ (6) xbcTransactionHold  (broadcast, :1288-1295)
   │ <─────────────────────────── │ ─────────────────────────────────> │
   │  (7) xbcTransactionHoldApply │                  (7) HoldApply
   │ ───────────────────────────> │ <───────────────────────────────── │
   │                              │  increaseStateCounter(trJoined)x2 -> trHold (:1620)
   │                              │ (8) xbcTransactionInit (:1637-1666)  each side
   │ <─────────────────────────── │ ─────────────────────────────────> │
   │  (9) xbcTransactionInitialized│                  (9) Initialized
   │ ───────────────────────────> │ <───────────────────────────────── │
   │                              │  increaseStateCounter(trHold)x2 -> trInitialized (:1850)
   │                              │ (10) xbcTransactionCreateA (:1857-1863)
   │ <─────────────────────────── │
   │  (11) xbcTransactionCreatedA │  (deposit A built/broadcast by maker)
   │ ───────────────────────────> │  mark a.source -> setBinTxId (:2343)
   │                              │ (12) xbcTransactionCreateB (:2349-2359)
   │                              │ ─────────────────────────────────> │
   │                              │  (13) xbcTransactionCreatedB  (deposit B built/broadcast)
   │                              │ <───────────────────────────────── │
   │                              │  mark b.source -> trCreated (:2821-2826)
   │                              │ (18) xbcTransactionConfirmA (:2828-2836)
   │ <─────────────────────────── │
   │  (19) xbcTransactionConfirmedA   maker redeems B (reveals secret), (:3002-3013)
   │ ───────────────────────────> │  mark a.dest (:3082)
   │                              │ (20) xbcTransactionConfirmB (:3088-3095)
   │                              │ ─────────────────────────────────> │
   │                              │  (21) xbcTransactionConfirmedB   taker recovers secret, redeems A (:3185-3196)
   │                              │ <───────────────────────────────── │
   │                              │  mark b.dest -> trFinished (:3264-3267)
   │                              │ (24) xbcTransactionFinished (broadcast, :3272-3277)
   │ <─────────────────────────── │ ─────────────────────────────────> │
```

Go client side (`api/swap.go` handlers; packet types `go-xbridge/proto/body_types.go`):
1. `OnHold` → reply `HoldApply` (`api/swap.go:283-299`)
2. `OnInit` → reply `Initialized` (`api/swap.go:302-308`)
3. `OnCreateA` (maker) → build+broadcast deposit A → reply `CreatedA` (`api/swap.go:315-397`)
4. `OnCreateB` (taker) → drift-check A locktime → build+broadcast deposit B → reply `CreatedB`
   (`api/swap.go:404-499`)
5. `OnConfirmA` (maker) → drift-check B locktime → redeem B (reveal secret) → reply `ConfirmedA`
   (`api/swap.go:507-597`)
6. `OnConfirmB` (taker) → recover secret from A payTx → redeem A → reply `ConfirmedB`
   (`api/swap.go:606-704`)
7. `OnFinished` → `MoveToHistory("finished")` (`api/swap.go:710-717`)

**Sequence order MATCHES 1:1 with the C++ client-side handler sequence**
(C++ client handlers: `processTransactionHold` → `processTransactionInit` →
`processTransactionCreateA`/`CreateB` → `processTransactionConfirmA`/`ConfirmB` →
`processTransactionFinished`). Dispatch in `api/node.go:559-599`.

### 7.2 Two-confirmation gate per phase

- C++: the hub advances each phase only on **both** sides' confirmations (Card 2 T3–T6), and
  the order is strict: CreateA must be answered before CreateB is sent
  (`xbridgesession.cpp:2343-2359`), ConfirmA before ConfirmB (`:3082-3095`).
- Go (client): the client itself is passive — it responds to whichever packet the hub sends and
  has no per-phase gate of its own; the authoritative two-confirmation gate is hub-side. The
  ported gate exists in `swap.Transaction.IncreaseStateCounter`/`swap.Session.advance`
  (`swap/session.go:127-163`) but is not on the live path (see Card 2 NOTE). Client-side
  idempotence guards (STATE-F77) prevent re-broadcasting a second deposit/claim on retransmit:
  `OnCreateA` `s.state >= csCreatedA` (`api/swap.go:324-327`), `OnCreateB`
  `>= csCreatedB` (`:412-415`), `OnConfirmA` `>= csConfirmedA` (`:515-518`), `OnConfirmB`
  `>= csConfirmedB` (`:614-617`) — mirroring C++ `xtx->state >= trCreated` / `>= trCommited`
  drops (`xbridgesession.cpp:1947-1954, 2424-2431, 2897-2904, 3152-3159`). MATCH.

### 7.3 Client-side handshake fidelity — GAP

- **Hold re-verification absent (Go).** C++ `processTransactionHold` re-checks the taker's
  amounts against the order before sending HoldApply — role-B exact `samount == fromAmount &&
  damount == toAmount`, role-A size bounds + partial drift against `origFromAmount`
  (`xbridgesession.cpp:1401-1471`) — and fails with a log (no cancel). Go `OnHold` reads
  `b.FromAmount`/`b.ToAmount` but never validates them (`api/swap.go:283-299`). The Go wire
  layer decodes them (`proto/body_types.go:367-399`). **GAP — candidate finding.**
- **Init amount/counterparty verification absent (Go).** C++ `processTransactionInit` verifies
  the packet's from/to identity against the stored order before accepting
  (`xbridgesession.cpp:1750-1760`). Go `OnInit` performs no such comparison
  (`api/swap.go:302-308`).
- Stale header-comment sequence (C++ `xbridgepacket.h:69-98`) references
  `xbcTransactionCommitA`/`CommitB` and `xbcTransactionCommitedA/B`, which do not exist in the
  enum (commands jump 13→18; `xbridgepacket.h:219-253`). The live flow uses
  ConfirmA=18/ConfirmedA=19/ConfirmB=20/ConfirmedB=21. Not a functional issue; flagged as
  header-comment staleness.

---

## CANDIDATE FINDINGS SUMMARY

| axis | finding |
|---|---|
| enum ordinals | all 11 `Transaction::State` and all 16 `TransactionDescr::State` ordinals match; only Go identifier differs at ordinal 2 (`DescrOpen` vs `trPending`), string `"open"` matches |
| strState strings | all strings match byte-for-byte, incl. `"trCommited"`, `"commited"`, `"canceled"`; only unknown-ordinal render differs (Go `trUnknown(N)`/`descrState(N)` vs C++ `"unknown"`) |
| transition table | all 9 transitions present on both sides; two-party gate identical; `trSigned`/`trCommited` vestigial on both |
| swap.State not on live path | the live Go handshake drives a local `clientState`; `swap.Transaction`/`swap.Session.advance` (the faithful gate port) run only in tests/helpers — architectural |
| TryJoin/matching | partial-order price drift, exact-order equality, minFromAmount, taker-size bounds all port 1:1; drift is the C++ derived-price ±1-band check |
| Hold/Init re-verify | `OnHold`/`OnInit` never validate the hub's amounts/order identity that C++ `processTransactionHold`/`processTransactionInit` check (price/amount re-verification absent) |
| TTL constants | LockTime/PendingTTL/TTL/DeadlineTTL/BlocksTTL all match; strict `>` comparison and unix-seconds vs ptime semantics equivalent |
| no live expiry sweep | `IsExpired`/`IsExpiredByBlockNumber` implemented+tested but never wired: the Store never prunes open orders by TTL or block height (C++ `eraseExpiredTransactions` runs every 15s) |
| sweep intervals | C++ timer 15s / persist ~60s vs Go ticker 60s / persist ~240s |
| locktime | all 8 constants (incl. 500000000/900s/4-blocks) and the formula `currentBlock + clamp(target/blockTime, 6)` match; one-sided drift identical; enforced at the same CreateB/ConfirmA sites |
| cancel reasons | TxCancelReason enum (0..24) and TxCancelReasonText NOT ported — Go sends/receives opaque `uint32` (default 0); C++ bugs (crBadSettings→"crUnknown", default→"crNone") therefore unmirrored |
| rollback-failed | Go never writes a "rollback failed" descriptor state (C++ sets trRollbackFailed on refund broadcast failure); fire-and-forget refund instead |
| peer penalty | no banning/misbehavior analog in Go (C++ Misbehaving(+10) on undersized packets, DoS(0) on content errors) |

---

## VERDICTS (one line per card)

1. STATE ENUM ORDINALS — **MATCH** (all 11+16 ordinals and all strings identical; only unknown-ordinal rendering and the `DescrOpen`/`trPending` identifier differ)
2. TRANSITION TABLE — **MATCH** (all 9 transitions and the two-party gate are 1:1; trSigned/trCommited vestigial on both; live Go handshake uses clientState, not swap.State)
3. TRYJOIN/MATCHING — **MATCH** (currency/partial/drift/minFrom/exact checks port 1:1 with the same derived-price drift band)
4. TIMEOUT/EXPIRY — **DIVERGE** (constants/semantics match, but Go never wires an order-book expiry sweep; 15s vs 60s / 60s vs 240s cadence)
5. LOCKTIME COMPUTATION — **MATCH** (all 8 constants, the formula, the 6-block floor, and one-sided drift are identical and enforced at the same phases)
6. CANCEL/ROLLBACK/PENALTY — **DIVERGE** (TxCancelReason enum + reason strings unported; no trRollbackFailed write; no peer-penalty mechanism; C++ TxCancelReasonText bugs unmirrored)
7. HANDSHAKE ORDERING — **MATCH** (packet sequence 1:1 and phase gating same; Hold/Init re-verification missing on the Go client side)

Output written to: `docs/audit/evidence/state.md`
