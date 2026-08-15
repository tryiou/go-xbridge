# B11 — Concurrency & lifecycle (CONC-F92–F94, INV-F98)

Branch: `fix/concurrency` (off `main` @ B8 merge `bb783d0`).
Status: MERGED into `main` @ `46c3a19` (fast-forward).
C++ reference: Blocknet Core @ `e9ddbc2bd` (v4.4.1 era).
Go subject: `p2p/conn.go`, `api/persist.go`, `api/engine.go`, `api/node.go`,
`p2p/discovery/peer_manager.go`, `log/dedup.go`, `api/swap.go`,
`docs/protocol.md` (three CONC findings + the INV-F98 doc fix; per
`register.md` Owner B11).

The concurrency / lifecycle pass: the engine goroutine must never block on a
peer socket or on fsync (CONC-F92); every discovery goroutine must be joined
and every Dedupe sweeper stopped on Close (CONC-F93); and a conf reload
mid-swap-task must not swap which wallet the task builds against (CONC-F94).
Plus the order-`Created`-is-µs documentation correction (INV-F98).

## Findings resolved on this branch

| ID | Title | C++ source of truth | Go call sites → fix |
|---|---|---|---|
| F92 | Engine blocks on socket write / fsync (S2) | `App::Impl::onSend` only enqueues via `PushMessage` (`xbridgeapp.cpp:585-590`); send buffer paused at `nSendSize > nSendBufferMaxSize` (`net.cpp:2721-2722`); `saveOrders` runs on the timer/worker threads, never `msghand` (`xbridgeapp.cpp:3742-3747`) | `p2p.Conn` growing outbound queue (C++ `vSendMsg`) + writer goroutine started lazily on the first post-handshake write (read-only/handshake-failed conns spawn nothing); 1 MB cap disconnects the peer (no silent frame drops — a dropped CreatedA would make the hub retransmit and double-broadcast the deposit); 30 s write deadline; error surfaces on the next send; write-error teardown in both directions (`doneOnce`-guarded); `Close` joins the writer. Persist split into `snapshotSwaps` (engine) + `writeSwaps` (background `persistLoop`, coalesced latest-slot + buffered(1) signal; `Close` flushes the slot after `wg.Wait()`); inline/test mode stays synchronous. `TestConnWrite*`, `TestEngineWriteDoesNotBlockOnSlowPeer`, `TestPersistDoesNotBlockEngine`, `TestPersistCoalescesBurst`, `TestPersistFlushedOnClose` |
| F93 | Discovery goroutines not joined; Dedupe sweeper leaks (S3) | `App::stop` joins every thread (`m_timerThread.join`, `m_threads.join_all`, `xbridgeapp.cpp:530-544`) | `PeerManager` tracks maintain/connectOne/readLoop with a `WaitGroup` joined by `Close`, and drives an internal lifecycle context so in-flight dials abort immediately (`p2p.DialContext` = `net.Dialer.DialContext`; the version handshake aborts on ctx cancellation via `NewConnCtx`, so a handshake-stalling peer cannot hold the join — follow-up `fix/concurrency-followup`); `Options.Dialer` is now ctx-aware. `Dedupe.startSweepLocked` re-registers in the global registry so a sweeper restarted after a shutdown flush is still stopped by `FlushAll`; `Node.Close` calls `xlog.FlushAll()`. `TestPeerManagerCloseAbortsInflightDialAndJoins`, `TestPeerManagerCloseFastWithStalledHandshake`, `TestDedupe_RestartReRegisters`, `TestNodeCloseFlushesDedupeSweepers` |
| F94 | Reload mid-swap-task hazard (S3) | session holds the connector pointer it captured; reload replaces the pool under `m_connectorsLock` — C++ immune | `swapCtx` snapshots `Connectors`/`Confs` at enqueue (shallow copies under `cfg()`); all worker-path reads use the snapshot (`checkCounterpartyDeposit`, ConfirmA/B claim broadcasts, `buildDeposit`, `computeLockTimeFor`, `conf()`); `postRefundTask` captures the connector at enqueue. `TestReloadMidSwapTaskKeepsConnectorSnapshot` (real `reloadConf` mid-task; fails when `buildDeposit` reverts to live-config reads) |
| INV-F98 | `Created` doc says seconds (S4) | `total_microseconds()` (`xutil.cpp:280`) — µs in the cmd-3/4 wire body and the 8-byte envelope timestamp | `docs/protocol.md` §4.2 note corrected to µs; stale `findings.md:366` ref → `:395` |

## Notes

- **Write-error surfacing.** `WritePacket` returns the last recorded writer-side
  error once the conn is dead; a broken peer is otherwise observed via the read
  side (the reader loop sees the teardown), matching C++ surfacing send
  failures on the socket-handler thread rather than on `PushMessage`.
- **Handshake stall bound (closed).** The dial phase is ctx-cancellable and the
  version handshake aborts on cancellation (`NewConnCtx` via `p2p.DialContext`,
  the default `Dialer`), so a peer that accepts TCP but stalls the version
  exchange cannot hold a discovery `Close` past the cancellation.
  `Start` must not race `Close`.
- **Coin-registry snapshot (CONC-F94, closed).** `swapCtx` copies the session's
  two currencies from the registry under one atomic load (`coins.Snapshot`) at
  enqueue; workers resolve coins via `c.coin(cur)`, so a mid-task reload that
  changes a coin's decimals/prefix/codec (or drops it) cannot alter an
  in-flight deposit/refund/claim build. `LocalConnector` binds
  `TxWithTimeField` at construction. Tests: `TestReloadMidSwapTaskKeepsCoinSnapshot`,
  `coins.TestSnapshot`, `TestLocalConnectorSignAfterRegistryFlip`.
- **Gate-semantics change (CONC-F92).** `dxMakeOrder`/`dxCancelOrder`/swap
  responses now complete on enqueue (write errors surface asynchronously),
  matching C++ `PushMessage` semantics; the old synchronous-write abort is gone
  (a strict improvement in fidelity).
- **Persist coalescing.** A burst of engine persists collapses into the newest
  snapshot (intermediate states are superseded, safe because each record is a
  full snapshot); the final state is durable before `Close` returns.
- **Dedupe restart.** A later `Event` after a `Flush` restarts the sweeper on
  demand and re-registers it, so dedup keeps summarizing after a
  shutdown-triggered flush (unchanged documented behaviour).
