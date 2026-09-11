# B8 — State-machinery (STATE-F72–F75, STATE-F79, SEC-F02)

Branch: `fix/state-machinery` (off `main` @ B10 merge `b3a29af`).
Status: MERGED into `main` @ `2f48c98` (fast-forward).
C++ reference: Blocknet Core @ `e9ddbc2bd` (v4.4.1 era).
Go subject: `api/engine.go`, `api/store.go`, `api/node.go`, `api/swap.go`,
`api/order.go`, `api/persist.go`, `api/response.go`, `api/cancel_reason.go`,
`swap/transaction.go`, `wallet/connector.go`, `wallet/rpc.go`,
`p2p/conn.go`, `p2p/discovery/peer_manager.go` (five STATE findings, plus
`STATE-F79` and `SEC-F02` reassigned from B7; per `register.md` Owner B8).

The state-machine / lifecycle pass. The headline items: the order-book expiry
sweep C++ drives on its 15 s timer is now wired (STATE-F72) — open orders are
pruned by time and block-height TTLs, persist aligns to 60 s, and the maker's
in-swap orders are protected by session state; the cancel-reason enum and its
text table (including the two upstream C++ rendering bugs) are ported
(STATE-F73); a failed refund broadcast now marks the order "rollback failed"
and a later success restores it (STATE-F74); peers (and the direct hub) are
scored and banned like C++ Misbehaving (STATE-F75); the partial-order
minimum-size guard is confirmed against `Transaction::tryJoin` (STATE-F79);
and inbound maker UTXO ownership proofs are verified before booking (SEC-F02).

## Findings resolved on this branch

| ID | Title | C++ source of truth | Go call sites → fix |
|---|---|---|---|
| F72 | Expiry pruning never wired (S2) | `App::Impl::checkAndEraseExpiredTransactions` every 15 s timer (`xbridgeapp.cpp:90,3573-3654`) → `Exchange::eraseExpiredTransactions` (`xbridgeexchange.cpp:712-747`); `Transaction::isExpired`/`isExpiredByBlockNumber` (`xbridgetransaction.cpp:268-311`); `saveOrders` every 4th tick (`:3744`) | `Store.PruneExpired` (open book `"created"`/`"open"`; strict `>` time + block-height TTLs, `PrepTx` pending-partial guard, erase-without-history); `Order.BlockNumber` stamp at ingest/make + cached BLOCK height; 15 s `expirySweepInterval` ticker; persist 240 s → 60 s; maker in-swap protection via the session `inSwap` guard (`s.state > csMaker`, C++ advances the descriptor to trHold, `xbridgesession.cpp:1529`); `TestPruneExpired*`, `TestNodePruneExpired` |
| F73 | TxCancelReason enum + text not ported (S3) | enum 0–24 (`xbridgepacket.h:21-48`); `TxCancelReasonText` incl. two bugs: `crBadSettings`→`"crUnknown"`, `crUnknown`/default→`"crNone"` (`xbridgeapp.cpp:4052-4107`); `cancel_reason` log field (`xbridgesession.cpp:3312,3536`) | `api/cancel_reason.go` (`TxCancelReason` + `TxCancelReasonText` byte-exact); `selfCancelErr`/`sendSelfCancel` retyped (wire boundary back to uint32); `cancel_reason`/`reasonText` log fields; `TestTxCancelReasonEnumOrdinals`, `TestTxCancelReasonText` |
| F74 | `trRollbackFailed` never set (S3) | `redeemOrderDeposit` — broadcast failure → `trRollbackFailed` (state ≥ trCreated guard, `xbridgesession.cpp:3852-3908`), success → `trRollback` (`:3911`); reached from cancel rollback (`:3415`) and the fund-safety watch (`xbridgeapp.cpp:3441`) | `postRefundTask` apply: `rollbackGate` (session `csCreatedA+`, `RefundTx` fallback) writes `"rollback failed"` on failure (never clobbering terminal/canceled), restores `"rolled back"` on a later success; `TestRollbackFailedStatusOnRefundBroadcastFailure`, `TestRefundFailureStateGate` |
| F75 | No peer penalty / Misbehaving (S3) | `Misbehaving(id,10)` undersized xbridge (`net_processing.cpp:2874-2878`); `+20` addr >1000 (`:1825-1830`); `-banscore` 100; malformed bodies → `DoS(0)` (`xbridgesession.cpp:312`) | per-peer score in `discovery.PeerManager` (+10 envelope / +20 addr, ban 100 = disconnect + exclude, per-connection reset, expired bans pruned); direct-hub score gated on the new `p2p.ErrMalformedXBridge` sentinel (`ReadPacket` wraps only the envelope decode); bodies dropped unscored; `TestPeerManagerMisbehave*`, `TestReaderLoopHub*` |
| F79 | `tryJoinMatches` min-size guards unconfirmed (S3) | `Transaction::tryJoin` (`xbridgetransaction.cpp:490-545`) — drift → bounds → `other->m_destAmount < m_minPartialAmount` (`:527`, strict `<`) | confirmed 1:1 (`swap/transaction.go:85-115`), no code change; `TestTryJoinPartialMinSizeGuard` isolates the guard with a 2:1-price fixture |
| SEC-F02 | Inbound UTXO proofs unverified (S3) | snode `processTransaction`/`processTransactionAccepting` (`xbridgesession.cpp:535-575,1100-1140`) — per entry `getTxOut` + `verifyMessage` vs the chain amount; skip bad entries; reject when no survivor covers the amount (`:566-577`); `rpc::gettxout` (`xbridgewalletconnectorbtc.cpp:659-712`) | `wallet.Connector.GetTxOut` (gettxout) + `verifyAndBook`/`verifyOrderUtxos` verify each maker UTXO before booking, offloaded so the engine never blocks; cmd-4 broadcasts carry no utxos, so the gate fires for any utxo-bearing order body (faithful port of the snode gate); `TestVerifyOrderUtxos*`, `TestVerifyAndBook*` |

## Notes

- The trader wire (`xbcPendingTransaction`, cmd-4) carries no UTXO entries
  (C++ `processPendingTransaction` parses only the order summary), so SEC-F02's
  gate is exercised on the live path only when a utxo-bearing order body is
  ingested; it is the exact C++ snode gate, unit-tested.
- STATE-F72's time predicates blend the C++ app-side inactivity TTL (1 h) with
  the exchange-side block/deadline predicates; the created-age deadline for
  trNew is kept because the port has no trNew→trOffline/trPending flip.
- The persist cadence change (240 s → 60 s) matches C++ `saveOrders` every 4th
  15 s tick (a deliberate parity alignment; `tickCount` gating removed).
- Peer bans: the direct-hub score is local to the readerLoop goroutine and
  drops the connection at 100; the discovery pool resets a peer's score on
  reconnect (C++ `nMisbehavior` is per-connection).
- A pre-existing divergence surfaced by F74: `dxCancelOrder` writes `"canceled"`
  for deposit-sent orders where C++ would route through the rollback path
  (`trRollback` → `trRollbackFailed`/`trRollback`). Tracked as STATE-F85
  (fix-queued); not fixed on the F74 branch.
