# CONCURRENCY & ORDERING CONFORMANCE — C++ blocknet_core xBridge vs Go go-xbridge

Scope: threading/goroutine model, mutex/channel ordering, map-iteration order
affecting output, goroutine lifecycle/leaks, data races, and shutdown/staleness
semantics observable by a dApp.

Reference paths are relative to `blocknet_core/src/`; candidate paths to
`go-xbridge/`. Every claim carries `file:line` on both sides. Unknowns are
marked **TBD**.

---

## Card 1 — THREADING MODEL MAP

### C++ side (which thread runs what)

| Responsibility | C++ thread | Evidence |
|---|---|---|
| P2P message processing (incl. every inbound XBridge packet) | **single** `msghand` thread (`ThreadMessageHandler`), iterating all nodes serially | `net.cpp:1949-1996` (one loop, one thread), created at `net.cpp:2278` |
| XBridge packet → session dispatch | synchronous, on `msghand`; `ProcessMessage` → `xapp.onMessageReceived(addr, raw, state)` → `ptr->processPacket(packet)` inline | `net_processing.cpp:2868-2899`; `xbridgeapp.cpp:637-723` (`onMessageReceived`), `xbridgeapp.cpp:751-764` (`onBroadcastReceived` → `processPacket` inline) |
| Per-packet handler (the whole swap state machine: `processPendingTransaction` … `processTransactionFinished`) | `msghand` thread (serial), or a worker `io_service` thread for **deferred** packets | `xbridgesession.cpp:283-322` (`Session::processPacket`); deferred path `xbridgeapp.cpp:3712-3733` (`onTimer` reposts pending packets via `io->post(processPacket)`) |
| Timer / expiry / periodic sweep | dedicated timer thread, 15 s tick; posts work to a pool of `hardware_concurrency()` worker `io_service` threads | `xbridgeapp.cpp:269-271` (timer thread), `xbridgeapp.cpp:404-414` (worker thread pool + `m_timer.async_wait`), `xbridgeapp.cpp:3658-3752` (`onTimer` → `io->post(...)`) |
| `checkFinishedTransactions`, `updateActiveWallets`, `checkAndRelayPendingOrders`, `checkAndEraseExpiredTransactions`, deposit/trader watches, `sendPing` | worker `io_service` threads (concurrent with `msghand`) | `xbridgeapp.cpp:3667-3709`, `3241-3337`, `3348-3470` |
| Wallet RPC calls (`CallRPC` getunspent/createrawtransaction/sendrawtransaction/…) | **synchronous + blocking on the calling thread** (msghand, http, or worker/timer thread) | `xbridgewalletconnectorbtc.h:111-179` (`event_base_dispatch` blocks, timeout `-rpcxbridgetimeout` default 120 s); invoked from session handlers e.g. `xbridgesession.cpp:2094,2615` (`minTxFee`/create-tx) |
| JSON-RPC handlers (`dx*`) | **single** `http` thread (libevent event loop); handlers run inline, serially | `httpserver.cpp:436` (`threadHTTP`), `httprpc.cpp:189-201` (`JSONRPCExecOne`/`JSONRPCExecBatch` inline in `http_request_cb`) |
| `dxGetTokenBalances` per-connector balance RPC | spawns up to `cores/2` short-lived `blocknet-balance-check` threads **inside the handler** (bounded queue), each doing a blocking wallet `getWalletBalance` | `rpcxbridge.cpp:2530-2560` (`boost::thread_group`, `GetNumCores()/2`, `cv` backpressure, `tg.join_all()`) |
| `sendXBridgeTransaction`/`acceptXBridgeTransaction`/`cancelXBridgeTransaction` (RPC entry) | `http` thread; **state mutated without the map lock, wallet RPC blocking** | `rpcxbridge.cpp:337-473` (dxGetOrders reads `xapp.transactions()` copy, fields read unlocked); `xbridgeapp.cpp:2107-2163` (accept sets `ptr->state=trAccepting` unlocked at :2127 before RPCs) |
| Local BLOCK-wallet fee-tx construction (`createFeeTransaction`) | calling thread; global `cs_rpcBlockchainStore` + wallet lock held across it | `bitcoinrpcconnector.cpp:89` (`LOCK(cs_rpcBlockchainStore)`), `:62-65` (`LOCK2(cs_main, wallet->cs_wallet)`), `:218-233` (signing inside) |

### Go side (which goroutine runs what)

| Responsibility | Go goroutine | Evidence |
|---|---|---|
| P2P packet read + decode + signature verify | one **readerLoop** goroutine; blocking `ReadPacket`; verified packets forwarded on `n.packets` | `engine.go:107-152` |
| All XBridge packet handling, all session/swap handlers, engine commands, worker results, refund sweep, persist | one **engine goroutine** (single owner of `n.sessions`, `n.pendingRefunds`, persist) | `engine.go:158-192` (`engineLoop`), `engine.go:154-156` comment |
| Wallet RPC I/O (deposit build, claim, refund broadcast) | pool of **4 worker goroutines** (`engineWorkers`); `n.tasks`→worker→`n.results`→engine apply | `engine.go:15,227-250` (`startWorkers`) |
| BLOCK best-block anti-replay refresh | `blockLoop` goroutine (30 s ticker) + inline refresh on stale read | `node.go:494-528` (`refreshBlock`/`currentBlockHash`), `node.go:531-544` |
| Periodic status log | `statusLoop` goroutine (60 s ticker) | `engine.go:197-209` |
| JSON-RPC handlers (`dx*`) | **one goroutine per HTTP request** (net/http); handlers run concurrently | `server.go:113-188` (`ServeHTTP`), `main.go:236,243-247` |
| Peer maintenance / per-peer read loops (discovery mode) | `maintain` goroutine + one `readLoop` goroutine per peer | `p2p/discovery/peer_manager.go:123-129,161-179,249-349` |
| Dedup log sweeps | one goroutine per `Dedupe` (lazy) | `log/dedup.go:122-148` |

### Side-by-side responsibility table

| Responsibility | C++ thread | Go goroutine | dApp-observable delta |
|---|---|---|---|
| Inbound P2P packet → state machine | `msghand` (serial, single) | engine (serial, single) | **same serialization** — per-order packet ordering preserved in both |
| Swap-handshake wallet I/O | **synchronous on `msghand`** → one slow wallet stalls the entire P2P pipeline and every other swap (up to 120 s) | **4 workers run it concurrently**; engine keeps processing other packets/swaps | **FLAG.** Go runs swap on-chain work concurrently that C++ serializes behind the single `msghand` thread. dApp sees: in C++, a hung wallet freezes the whole order feed and all swaps; in Go, only that swap stalls. (Improvement, but a real divergence; e.g. `xbridgesession.cpp:2094`/`:2615` vs `engine.go:15` + `swap.go:337-354`.) |
| RPC handler execution | single `http` thread, strictly serial; a slow `dxMakeOrder` wallet call blocks **all** RPC for its duration | one goroutine per request; concurrent handlers | **FLAG.** Go runs RPC handlers concurrently that C++ serializes. dApp pipelining two dx* calls sees different interleavings; C++ also has the single-thread wallet-stall coupling. `httprpc.cpp:192` vs `server.go:180`/`main.go:243-247`. |
| Timer/expiry work | worker `io_service` pool, **concurrent with `msghand`** — two threads can run a session's code at once (see Card 5 C++ races) | engine only (never concurrent within a session; `await` guard drops retransmits) | Go strictly serializes per-session work that C++ may run concurrently on two threads. dApp sees fewer torn intermediate states in Go (safer, but a model difference). `xbridgeapp.cpp:3727-3729` vs `engine.go:158-192` + `node.go:715-761`. |
| Order-book mutations vs reads | copy map under `m_txLocker` then read **unlocked** fields | store mutex; snapshot copies | see Card 6 |

**Key asymmetry to remember:** C++ has *three* writers that can hit one order
concurrently — `msghand` (packets), the timer pool (`checkAndRelayPendingOrders`,
`checkAndEraseExpiredTransactions`, `checkWatchesOnDepositSpends`,
`watchTraderDeposits`), and `http` (dxTakeOrder/dxCancelOrder) — protected only
by the map-level `m_txLocker` and coarse `m_lock`. Go has *one* writer (engine)
plus mutex-guarded store reads. Go's model is the safer one; C++ carries
genuine data races (Card 5).

---

## Card 2 — MUTEX / CHANNEL ORDERING & DEADLOCK HAZARD

### C++ lock inventory and acquisition order

- `App`: `m_lock` (config/partial-orders, `xbridgeapp.h:745`), `m_txLocker`
  (transactions map, `xbridgeapp.cpp:244`), `m_connectorsLock` (:228),
  `m_sessionsLock` (:223), `m_ppLocker` (:250), `m_updatingWalletsLock`
  (`xbridgeapp.h:748`), `m_utxosLock` / `m_utxosOrderLock` (:755-756),
  `m_watchDepositsLocker` / `m_watchTradersLocker` (:254-261).
- `Exchange`: `m_lock`, `m_walletsLock`, `m_pendingTransactionsLock`,
  `m_transactionsLock`, `m_utxoLocker` (`xbridgeexchange.cpp:52-62`), plus a
  **per-transaction** `m_lock`.
- Per-object `Transaction`/`TransactionDescr` `m_lock`
  (`xbridgetransaction.cpp:92+`; `xbridgetransactiondescr.h:35` — the descr
  `_lock` is only taken in serialization).

**Ordering chains (all single-direction):**
1. `m_utxosOrderLock` → (`rpc::unspentP2PKH`/`createFeeTransaction`, holding
   `cs_rpcBlockchainStore` + `cs_main`+`wallet->cs_wallet`) → `m_utxosLock`
   (`lockFeeUtxos`/`lockCoins`) — accept path holds the outer order-lock across
   blocking wallet RPCs: `xbridgeapp.cpp:2236-2267` → `bitcoinrpcconnector.cpp:89,62-65,218-233`.
2. `m_pendingTransactionsLock` → `tx->m_lock` (Exchange pending-tx ops):
   `xbridgeexchange.cpp:375-406,459-513,804-823`. Reverse (`m_lock` then map
   lock) never occurs.
3. `m_txLocker` alone for map copy/insert/erase: `xbridgeapp.cpp:1273-1419`.
   Field reads/writes of the pointed-to `TransactionDescr` are **outside** the
   map lock (`xbridgeapp.cpp:2126-2129`, `rpcxbridge.cpp:434-467`).

**Blocking-wallet-RPC-under-lock in C++:** yes — `m_utxosOrderLock` is held
across `createFeeTransaction` (wallet sign + `cs_rpcBlockchainStore`) in the
accept path (`xbridgeapp.cpp:2236-2267`). C++ also blocks `msghand` on wallet
RPC with no lock held (the session handlers take no App lock while calling the
connector: `xbridgesession.cpp:2094-2130`). No C++ lock inversion found.

### Go lock/channel inventory and ordering

- `store.mu` (RWMutex, single book lock): `store.go:25,103-176`.
- `cfgMu` (RW): `node.go:181,274-278,329-331`.
- `blockMu` (RW): `node.go:173-175,507-527`.
- `servicenode.Registry.mu` (RW): `p2p/servicenode/servicenode.go:546`.
- `p2p.Conn.writeMu` (write serialization): `p2p/conn.go:46,140-145`.
- `log.Dedupe.mu`: `log/dedup.go:26`.
- Channels: `packets` (drop-on-full), `cmds` (backpressure), `tasks`
  (drop-on-full), `results` (== worker count), `stop` (shutdown):
  `engine.go:87-99,152-156`.

**Lock ordering in Go:** no multi-mutex nesting anywhere — `store.mu` is never
held while taking `cfgMu`/`blockMu`; handlers call store methods and `cfg()`
sequentially. `store.Update` runs `fn` under the lock, but every `fn` in the
codebase only assigns fields (e.g. `swap.go:334-336,373-379,583-585`); no `fn`
calls a wallet RPC. `ReserveForTake`/`ReleaseReserve` hold `store.mu` only for
map ops (`store.go:448-476`), unlike C++ which holds `m_utxosOrderLock` across
the whole fee-tx build.

**FLAG — Go has NO lock-order inversion and never calls a blocking wallet RPC
under a mutex** (wallet RPCs run on handler goroutines or workers, `swap.go:725-740`,
`engine.go:227-250`). C++ *does* run blocking wallet RPC under
`m_utxosOrderLock`. Net: Go is strictly safer here; no deadlock hazard found in
Go's mutex graph.

### Go stall hazards (not deadlocks) that C++ handles differently

- **Engine blocks on socket write.** `n.send`/`WritePacket` run on the engine
  goroutine (`node.go:802-814`); `PeerManager.WritePacket` loops all peers and
  each `p2p.Conn.write` takes `writeMu` and does a **blocking `net.Conn.Write`**
  (`p2p/conn.go:140-145`, `p2p/discovery/peer_manager.go:371-390`). A peer with
  a full TCP window stalls the *entire engine* (all packets, all swap applies,
  all RPC awaits) until the socket drains. C++ `onSend` only enqueues into the
  node's send buffer (`g_connman->PushMessage`, `xbridgeapp.cpp:585-590`), so
  the message thread never blocks on a peer. **FLAG.**
- **Engine blocks on fsync.** `persist()` (temp-write + `f.Sync` + rename,
  `persist.go:154-176`) runs on the engine and after every make/take/cancel
  apply and every 4th tick (`engine.go:185-187`, `node.go:1266,1678,1739`).
  C++ `saveOrders` runs on the timer/RPC thread, never on the message thread.
- **Worker pool exhaustion.** With 4 workers, 4 slow wallet calls saturate the
  pool; further swap tasks are dropped (fund-safe, `swap.go:798-814`), but a
  dApp's swap handshake stalls until a worker frees. C++ would just block the
  single `msghand` thread. Both stall; the failure modes differ.

---

## Card 3 — MAP ITERATION RANDOMNESS

### (a) JSON object serialization from `map[string]interface{}`

| Endpoint | C++ container + order | Go container + order | Impact |
|---|---|---|---|
| `dxPartialOrderChainDetails` | `UniValue` object built with `emplace_back` in fixed order (`rpcxbridge.cpp:2440-2457`) | `map[string]interface{}` → `encoding/json` **sorts keys alphabetically** (`handlers.go:1015-1037`) | **FLAG (observable).** Key order in the JSON object differs (`first_order_id` … `p2sh_deposits_counterparty` sorted in Go vs C++ insertion order). Key order is JSON-insignificant, but string-diffing dApps / golden-file tests observe it. |
| `dxGetLockedUtxos` (all) | object `{all_locked_utxo: [...]}` (`rpcxbridge.cpp:2655`) | same object, single key (`handlers.go:1084`) | no key-order issue |
| `dxGetLockedUtxos` (per-order) | object keyed by `key` state string (`rpcxbridge.cpp:2674-2677`) | `map[string]interface{}` with `id`+`key` (`handlers.go:1140-1143`) → sorted | minor key-order delta (id first in Go) |
| `dxGetTokenBalances` | object keyed by ticker; insertion order is **thread-dependent** — connectors iterate in vector order but each balance thread appends under `mu` on completion (`rpcxbridge.cpp:2530-2560`) | `map[string]string` → sorted keys (`handlers.go:763,823-833`) | Go emits deterministic (sorted) keys; C++ order is completion-race-dependent. Both valid JSON; only byte-for-byte diffs differ |
| `dxFlushCancelledOrders`, `dxGetTradingData`, `dxSplit*`, `dxGetOrderHistory` | `UniValue` object/array in fixed order | `map[string]interface{}` (sorted keys) or structs/slices (fixed order) (`handlers.go:1174-1185,1205-1215,1426-1436`) | same note as above |

Struct-typed responses (`orderBase`, `orderListResult`, `fillOut` …) preserve
field order, matching C++ `emplace_back` order: `response.go:24-88`,
`handlers.go:38-51`.

### (b) map iteration feeding SELECTION / OUTPUT ORDER

| Site | C++ container + iteration | Go container + iteration | Behavioral impact |
|---|---|---|---|
| **`dxGetOrders` array order** | `std::map<uint256,...>` → **sorted by id** (`rpcxbridge.cpp:42,430,434`) | `h.Store.List()` over **Go map → random order** (`store.go:154-162`, `handlers.go:108`) | **FLAG (highest-impact).** dApps that iterate the returned array (e.g. pick the first matching order, or diff consecutive polls) see **non-deterministic order in Go vs stable id-sorted order in C++**. No sort applied. |
| `dxGetMyOrders` | collects `std::map` (id-sorted) then `std::sort` by `txtime` — **unstable** on ties (`rpcxbridge.cpp:2100-2131`) | `Store.Mine()`+`History()` (random) then `sort.SliceStable` by `UpdatedAt` (`handlers.go:840-863`, `store.go:279-289`) | both end up time-sorted; Go is **stable** where C++'s `std::sort` is unstable on equal `txtime` → tie order differs (rare; dApp-visible only if two orders share the same `updated_at`) |
| `dxGetOrderBook` | `std::map` source then explicit price sort + id-ordered equal-price walks (`rpcxbridge.cpp:1673-1926`) | random source then price sort + explicit `bytes.Compare` id sort for detail 4 (`handlers.go:654-755`, `602-618`) | Go re-derives the id ordering deterministically → matches C++; **no randomness** |
| UTXO selection `selectUtxos` | C++ sorts by `amount` desc; **stable?** `std::sort` on `UtxoEntry` copies, ties unspecified; address filter applied in ideal pass (`xbridgeapp.cpp:2972-3077`) | sorts `[]wallet.Utxo` by `Value` desc with **unstable** `sort.Slice` (`utxo_select.go:127-133`); address filter in ideal pass (`:90`) | tie-break among equal-amount utxos differs run-to-run **in both**; C++ starts from `listunspent` (wallet order) vs Go from `ListUnspent`. Equal-value tie picks can differ → different funding tx. Low risk, note only |
| `selectPartialUtxos` | C++ asc/desc sorts with `std::sort` (`xbridgeapp.cpp:3079-3237`) | unstable `sort.Slice` on `camount` (`utxo_select.go:205-206,274`) | same as above |
| `Registry.Pick` (hub choice) | `findShuffledNodesWithService`: collect into vector, **shuffle**, take first (`xbridgeapp.cpp:2901-2936`) | collect into slice, `rand.Shuffle`, first (`p2p/servicenode/servicenode.go:653-675`) | both explicitly shuffle; both non-deterministic by design. **Go uses `math/rand` global** — seed not set per-run → reproducible-ish across restarts unless `rand` seeded; C++ `default_random_engine` seed 0 → deterministic sequence. Choice has no wire effect (doc'd at `servicenode.go:651-652`); hub choice differs run-to-run in both. |
| Order iteration in `checkAndRelayPendingOrders` | `std::map` id-sorted (`xbridgeapp.cpp:3244-3248,3254`) | n/a in Go (not ported) | — |
| `partialOrderChain` | `std::map` walk (`rpcxbridge.cpp:2273-2457`) | Go builds `idx` from random `List()` then walks deterministic chain (`handlers.go:900-961`) | final chain order is deterministic in both; intermediate map order irrelevant |

`dxGetOrders` is the only output-ordering break that materially changes dApp
behavior.

---

## Card 4 — GOROUTINE LEAKS / LIFECYCLE

### Goroutines created

| Goroutine | Created | Stop signal | Shutdown path on Node.Close | Leak? |
|---|---|---|---|---|
| engine | `engine.go:95` | `n.stop` | `engineLoop` selects `<-n.stop` → return; `wg.Done` (`engine.go:188-189`) | No |
| reader | `engine.go:94` | `n.stop` | checks `<-n.stop` each iteration; read-error loop re-checks (`engine.go:110-128`) | No (but see Note A) |
| 4 workers | `engine.go:230` | `n.stop` or `tasks` closed | select `<-n.stop`/`<-n.tasks` closed → return; result send selects `n.stop` (`engine.go:233-247`) | No |
| blockLoop | `engine.go:96` | `n.stop` | `<-n.stop` → return; ticker `Stop()` deferred (`node.go:534-543`) | No |
| statusLoop | `engine.go:97` | `n.stop` | `<-n.stop` → return; ticker stopped (`engine.go:199-207`) | No |
| HTTP handler goroutine | net/http | — | `httpSrv.Shutdown` with 5 s timeout drains in-flight (`main.go:253-257`); any handler still blocked on a wallet RPC after the timeout is abandoned but the process `os.Exit`s (`main.go:270`) | No in production (process exits); see Note B |
| Dedupe sweeper (one per `Dedupe`) | `log/dedup.go:132` | `stopCh` closed by `Flush` | `xlog.FlushAll()` on shutdown (`main.go:263`); `Flush` closes `stopCh` (`log/dedup.go:197-200`) | **Leak in tests/long-lived libraries**: any `Dedupe` whose `Flush` is never called keeps its sweeper forever. Production flushes all. |
| PeerManager `maintain` | `peer_manager.go:128` | `ctx.Done`/`m.done` | `PeerManager.Close` closes `m.done` (`peer_manager.go:433-447`); Node.Close calls `n.conn.Close()` (`engine.go:276-278`) | No |
| Peer `readLoop` (one/peer) | `peer_manager.go:270` | `m.done` or conn error | `Close` closes all conns → `ReadMessage` errors → loop returns (`peer_manager.go:281-300,435-442`) | No (transiently outlives Close; not joined — see Note C) |
| `connectOne` dial goroutine | `peer_manager.go:201` | none mid-dial | dial has 30 s timeout (`peer_manager.go:256`); `Close` may not interrupt an in-flight `net.DialTimeout` | Transient (≤30 s) |
| Refund/BroadcastRefund await | `swap.go:964` | `n.stop` | select on `n.stop` (`swap.go:971-972`) | No |

**Note A — reader error spin:** on `ReadPacket` error without `n.stop`,
`readerLoop` retries with a 200 ms sleep (`engine.go:117-127`). If the conn is
closed and `n.stop` is never closed (Close always closes it), it would spin —
bounded and inert.

**Note B — shutdown ordering:** `main.go` drains HTTP (5 s) *then* `node.Close`.
A handler parked >5 s in `conn.ListUnspent` (wallet, 30 s client timeout,
`wallet/rpc.go:61,75`) is left running while `node.Close` tears the P2P conn
underneath it; its later `n.submit` bails on `n.stop` (`engine.go:72-80`). C++
orders RPC shutdown via `StopRPC`/http thread join (`httpserver.cpp:474`).

**Note C — no join on discovery goroutines:** `Node.wg` covers only api
goroutines (`engine.go:93`); `PeerManager` goroutines are signalled via `done`
but never waited. `Node.Close` returns while peer readLoops may still be
finishing. C++ joins all xbridge threads (`m_timerThread.join`,
`m_threads.join_all`, `xbridgeapp.cpp:530-544`) and the msghand/http threads
(`net.cpp`, `httpserver.cpp:474`). Go's is a bounded, signalled drain, not a
join — acceptable, but a lifecycle difference.

### Tickers with `Stop()`
`refundCheckInterval` (`engine.go:161-162`), status (`engine.go:199-200`),
block refresh (`node.go:534-535`), peer maintain (`peer_manager.go:162-163`),
Dedupe sweep (`log/dedup.go:138-139`) — **all call `t.Stop()`** or are stopped
by channel close. No ticker leaks. Compare C++: `m_timer.cancel()` +
`m_timerIo.stop()` + `m_timerThread.join()` (`xbridgeapp.cpp:530-533`).

### Channels that can block forever
- `n.cmds` (64): producer `submit` selects against `n.stop` (`engine.go:70-74`) → cannot block after Close.
- `n.results` (== 4): worker send selects against `n.stop` (`engine.go:239-243`) → cannot block after engine exit.
- `n.packets` (256, drop-on-full, `engine.go:144-150`): never blocks.
- `n.tasks` (16): `postSwapTask`/`postRefundTask` use non-blocking send with drop (`swap.go:782-789,804-813`).
- `xbridgeCh` (PeerManager): senders select `<-m.done` (`peer_manager.go:313-317`); `Close` intentionally leaves it open but no sender survives `m.done` close (`peer_manager.go:443-446`).
- `out` in `BroadcastRefund`: select on `n.stop` (`swap.go:968-972`).

No channel can block forever. C++ has no equivalent unbounded waits (the
`condMsgProc` wait is bounded, `net.cpp:1990-1993`).

---

## Card 5 — DATA RACES

### C++ races (present in reference; TSAN-visible)
- `Session::m_isWorking` is a plain `bool` (`xbridgesession.h:137`), written by
  `setWorking()/setNotWorking()` in `processPacket` without any lock
  (`xbridgesession.cpp:283-321`), while `getSession()` reads it under
  `m_sessionsLock` (`xbridgeapp.cpp:602-620`). Two threads (msghand + timer
  worker) can write/read it concurrently.
- `TransactionDescr` field writes on the `http` thread are unlocked:
  `ptr->state = trAccepting` at `xbridgeapp.cpp:2127` and the revert lambda at
  `:2131-2135` — while `msghand`/timer threads read the same fields
  (`rpcxbridge.cpp:436-466` reads after releasing `m_txLocker`).
- `checkAndRelayPendingOrders` mutates `order->sPubKey`/`excludedNodes` on a
  worker thread (`xbridgeapp.cpp:3288-3289,3324-3326`) concurrently with msghand
  handlers reading them.
- `m_partialOrders` guarded by `m_lock` inconsistently (`xbridgeapp.cpp:1951,1975,3737-3739,3757`).

### Go races — audit result
- `n.sessions`: **all** writers are the engine goroutine (`engine.go:154-156`;
  `swap.go:238,270,768,808,822,872-875`; `node.go:716,1765,1777`) or the
  single-threaded NewNode restore path before `start()` (`node.go:217-219`,
  `persist.go:361`). `persist()` reads it on the engine (`persist.go:121,127`).
  Tests read via `readOnEngine` (`concurrency_test.go:128-136`). **No race.**
- `n.config`: every read goes through `cfg()` RLock (`node.go:274-278`); reload
  swaps under `cfgMu.Lock` (`node.go:329-331`). **No race.**
- `n.block`/`blockAt`: `blockMu` RW (`node.go:507-527`); writers are
  `blockLoop` and the stale-refresh inside `currentBlockHash`, called from
  handler goroutines (`node.go:515-528`). **No race.**
- `n.tickCount`, `n.pendingRefunds`: engine-only. **No race.**
- `n.snReg`: Registry `mu` RW (`p2p/servicenode/servicenode.go:546`); written by
  reader/peer readLoops, read by engine + handlers. **No race.**
- `Store`: all access via mutex methods; `Copy()` deep-copies slices
  (`order.go:168-177`). **No race.**
- `p2p.Conn`: `peerVersion` written in handshake before any goroutine starts;
  `OnNonXBridge` callback invoked on the reader goroutine and, in discovery
  mode, on peer readLoops (writes go to `snReg` under its lock). `writeMu`
  serializes writers. **No race.**
- `coins` registry: `atomic.Pointer[map]` (`coins/coin.go:61`), reload-safe.
- `wallet.RPCClient`: `nextID` atomic; `http.Client` concurrent-safe
  (`wallet/rpc.go:29-39`). **No race.**

### Test coverage vs uncovered
Covered by `-race` (the whole suite runs `go test ./...`):
- Concurrent refund sweep vs deposit task: `concurrency_test.go:53-140`.
- Refund double-broadcast guard: `concurrency_test.go:148-199`.
- Engine liveness while one wallet parked: `concurrency_test.go:204-252, 664-729`.
- Concurrent takes (same order; distinct orders with shared scarce pool):
  `concurrency_test.go:303-415, 423-552`.
- Concurrent cancels: `concurrency_test.go:560-606`.
- Close-drains-in-flight-task: `concurrency_test.go:258-297, 612-658`.
- `store_test.go`, `engine_test.go`, `swap_two_phase_test.go` cover ordering
  invariants.

**Gaps the tests do NOT cover (FLAG):**
1. **Engine blocked on a slow socket write** (Card 2): no test parks
   `WritePacket`; if a peer's send window stalls the engine, packet processing
   and `submit` awaits stall. Not exercised.
2. **Concurrent `dxLoadXBridgeConf` vs in-flight swap** (reload while a worker
   task holds a `swapCtx` built from the old config): no test reloads conf
   mid-handshake. A worker task reads `c.n.cfg().Connectors` at run time
   (`swap.go:725-726,1038,1097`) — if reload swaps `Connectors` while a
   two-phase task is between stage-1 enqueue and worker execution, the task uses
   the NEW connector set (config pointer read lazily inside `run`). This is
   data-race-free (cfgMu) but behaviorally racy: **a mid-task reload can change
   which connector a deposit is built against.** C++ reload (`dxLoadXBridgeConf`)
   replaces connectors under `m_connectorsLock`; a session holds the connector
   pointer it captured, so C++ is immune to this mid-task switch.
3. **Reader-vs-engine config reads**: none needed (mutex), covered implicitly.

---

## Card 6 — SHUTDOWN / STALENESS SEMANTICS

### Lock granularity vs C++ `m_transactions` lock granularity

C++ granularity:
- Map-level `m_txLocker` protects insert/erase/copy of the pointer map
  (`xbridgeapp.cpp:1298-1302,1358-1377,1381-1419`), **not** the pointed-to
  `TransactionDescr` fields.
- `transaction()`/`transactions()`/`history()` return *copies of the map*, but
  the caller reads `tr->state`, `tr->fromAmount`, `tr->toAmount`, etc. **outside
  the lock** (`rpcxbridge.cpp:434-467`). Field writers (`http` accept path,
  `msghand` session handlers, timer workers) do not take `m_txLocker`.

Consequence: C++ RPC readers can observe **partially updated / torn orders**
(multiple fields updated non-atomically, e.g. `fromAmount`+`toAmount` changed at
`:2128-2129` then reverted at `:2131-2135`), and — importantly — can observe a
**transient `trAccepting` state with the take's sizes while the take is still
executing its blocking wallet RPCs and may yet fail and revert.**

Go granularity:
- A single `store.mu` covers the whole `Order` struct; every read returns a
  deep snapshot `Copy()` under the RLock (`store.go:110-118,154-162`),
  every write is a single `Update` under the Lock (`store.go:125-133`). No torn
  reads are possible.

### Half-applied updates — concrete diff

**C++ exposes a mid-operation state a dApp can read:**
`acceptXBridgeTransaction` sets `state=trAccepting` and rewrites
`fromAmount/toAmount` *before* the blocking fee/utxo selection and wallet RPCs
(`xbridgeapp.cpp:2126-2129`), only reverting on failure (`:2131-2135,
2241-2264`). The wallet window can last many seconds (CallRPC up to 120 s,
`xbridgewalletconnectorbtc.h:124`). During that window a concurrent
`dxGetOrders`/`dxGetOrder`/`dxGetMyOrders` on the `http` thread reads the order
as **"accepting" with modified sizes** — even though the take later fails and
the order returns to "open".

**Go does not expose that state:**
`TakeOrder` runs ALL wallet RPCs (balance check, fee-tx build, funding
selection, block context) before any store mutation; only on success does the
engine closure flip `Status="accepting"` in a single `store.Update`
(`node.go:1637-1671`). On any failure the reserve is released and the order
stays "open" (`node.go:1587-1631,1641-1643`). A concurrent reader sees either
the pre-take "open" or the post-success "accepting", never a transient "accepting".

So the direction of the gap is **reversed from the audit brief**: Go *cannot*
show a half-applied update that C++ *would* expose; instead C++ shows transient
accepting states that Go hides. The dApp-observable delta: polling
`dxGetOrders` during an in-flight take sees `status=accepting`+modified sizes in
C++ (then possibly back to `open`), while Go stays `open` until the take truly
lands.

### Multi-step updates within Go (still atomic per step)
`applyCreatedA` applies `BinTxId`/`DepositSent` and then `RefundTx` in two
separate `store.Update` calls (`swap.go:373-379`). A concurrent `dxGetOrder`
can read the order between the two and see `DepositSent=true` with `RefundTx=""`.
C++ has the same non-atomicity on the descr fields but with no lock at all —
Go's window is strictly narrower and always consistent field-by-field.

### Staleness semantics — reads
Both C++ and Go let a reader see a snapshot that predates a concurrent write
(that's normal). Differences:
- C++: a copied-map read can also see a **torn** combination (field A new,
  field B old). Go: impossible.
- Go: `n.submit(..., await=true)` gives an RPC a *happens-after* view of the
  state mutation it triggered (`node.go:1246-1272,1638-1680`) — the response is
  rendered from a post-mutation snapshot. C++ RPC handlers mutate inline so the
  response reflects the mutation by construction on the same thread
  (`rpcxbridge.cpp:1204-1262`). Both give self-consistent responses.
- Go `cancelDedup`/`unknownSwapDedup`/`cancelBadSigDedup` collapse repeated
  log lines only; they don't change state (`node.go:33-62`).

---

## CANDIDATE FINDINGS (Go-specific hazards / divergences)

1. **Threading (1):** Go offloads swap wallet I/O to 4 workers and runs RPC
   handlers on per-request goroutines, both of which C++ serializes on the
   single `msghand`/`http` thread — dApp sees concurrent progress (or concurrent
   RPC interleavings) where C++ would stall or serialize.
2. **Locking (2):** Go never holds a mutex across a blocking wallet RPC and has
   no lock-order nesting, while C++ holds `m_utxosOrderLock` across the fee-tx
   build; Go's mutex graph is deadlock-free, but the **engine goroutine can
   block on a peer socket write or on fsync**, stalling all packet processing
   and RPC awaits — C++'s `PushMessage`/`saveOrders` never block those paths.
3. **Map iteration (3):** `dxGetOrders` output array order is **random in Go**
   (Go map) vs **id-sorted in C++** (`std::map`); JSON key order in the
   `map[string]interface{}` responses (`dxPartialOrderChainDetails`,
   `dxGetLockedUtxos`, `dxGetTokenBalances`) is alphabetically sorted in Go vs
   insertion order in C++.
4. **Lifecycle (4):** no goroutine/ticker/channel leak in production (all
   signalled, all tickers `Stop()`ed, `FlushAll` on shutdown), but discovery
   peer goroutines are **not joined** by `Node.Close` and a **Dedupe sweeper
   leaks unless `Flush` runs** (only guaranteed on daemon shutdown).
5. **Data races (5):** no Go races found (single-owner engine, store/registry/
   cfg/block mutexes, atomic coins registry); C++ has genuine unlocked races
   (`m_isWorking`, descr fields). Uncovered by tests: engine blocked on socket
   write, and **conf reload mid-swap-task** which can swap the connector a
   two-phase deposit task builds against.
6. **Staleness (6):** Go's single-lock snapshot reads cannot observe torn
   orders, and Go hides the transient `trAccepting`+modified-sizes state C++
   exposes during an in-flight take — direction is *Go-stricter than C++*, so a
   dApp polling `dxGetOrders` during a take sees different (Go: fewer)
   intermediate states.
