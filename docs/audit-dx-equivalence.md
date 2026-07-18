# xbridge-go `dx*` RPC Equivalence Audit

**Source of truth:** the original C++ implementation in
`blocknet_core/src/xbridge/` (mainly `rpcxbridge.cpp`, plus `xbridgeapp.cpp`,
`util/xutil.cpp`, `util/xbridgeerror.{h,cpp}`, `xbridgetransactiondescr.h`).
The Go port must match C++ behavior byte-for-byte on the wire. Header enum
values and RPC help text in the C++ are frequently **stale** — trust the actual
C++ writers, not the comments.

> This document is a **living register of the current divergence state**, not a
> change log. The dated remediation history has been retired to git history
> (`git log -- docs/api.md api/`). For per-package implementation status see
> [`STATUS.md`](STATUS.md); for the stable contract see [`api.md`](api.md).

## Current state (2026-07-18)

All 23 `dx*` commands (plus the `gettradingdata` alias) have been remediated
against the C++ wire contract. The **Tier 1** code bugs and **Tier 2** achievable
backing gaps are **fixed**, and the subsequent pass below closed the remaining
cancel/reject, packet-signature, lockTime-drift, dust/fee-fidelity, and
network-token-source gaps (C6–C12). The only remaining deltas are **Tier 3 —
documented thin-client architectural limits** that require a BLOCK block index /
`blocknetd` to fully resolve; these are documented (not silently divergent) under
"Tier 3 — architectural limits" in [`api.md`](api.md).

## Equivalence matrix

`DONE` = behaviorally 1:1 with the C++ writer. `TIER3` = intentionally divergent,
bounded by the thin-client design. `GO-ONLY` = no C++ `dx*` counterpart.

| Command | Verdict | Note |
|---|---|---|
| dxGetOrders | DONE | conf/connector filter + id-sorted |
| dxGetOrder | DONE | `NO_SESSION` gate; case-insensitive id |
| dxGetMyOrders | DONE | includes finished/cancelled locals; sorted by `txtime` |
| dxGetOrderBook | DONE | ask=`to/from`, bid=`from/to`; detail levels 1–4 |
| dxGetOrderFills | DONE | full 12-field record |
| dxGetMyPartialOrderChain | DONE | ancestors + descendants |
| dxPartialOrderChainDetails | DONE | `stateOrdinal<=trPending` totals; empty `{}`; id validated; `p2sh_deposits` |
| dxGetLockedUtxos | DONE | per-UTXO `txid:vout` locked set; nil-guarded |
| dxFlushCancelledOrders | DONE | prunes by age; uint64 underflow clamped |
| dxGetLocalTokens | DONE | conf/connector token set |
| dxGetNetworkTokens | TIER3 | live servicenode registry (SNREGISTER/SNPING, C10); still bounded by P2P ping coverage |
| dxMakeOrder | DONE | `dryrun` simulates; precision/limit/address/`NO_SESSION` checks |
| dxMakePartialOrder | DONE | `order_type="partial"`; trailing params; min/dust checks |
| dxTakeOrder | DONE | maker/taker correct; `dryrun` simulates; self-trade guard; full-take on amount=0 |
| dxCancelOrder | DONE | `state>=trCreated` guard; `refund_tx` from swap refund |
| dxLoadXBridgeConf | OK | hot-reloads xbridge.conf from the daemon's ConfPath (Load -> InitFromConf -> rebuild connectors) under cfgMu; keeps last-good config on failure |
| dxGetNewTokenAddress | DONE | `[]` on no-wallet; segwit addr type |
| dxGetTokenBalances | DONE | `Wallet` key from BLOCK connector; locked UTXOs subtracted |
| dxGetUtxos | DONE | `orderid` field; locked UTXOs excluded (`include_used`) |
| dxSplitAddress | DONE | 8-field object; 1e6-scale amounts |
| dxSplitInputs | DONE | 8-field object; C++ 7-param contract |
| dxGetOrderHistory | TIER3 | OHLCV buckets from local fills only |
| dxGetTradingData / gettradingdata | TIER3 | 8-field record from local fills (`fee_txid`/`nodepubkey` empty) |
| getNetworkInfo | GO-ONLY | Go-only extension; not one of the 23 `dx*` |

## Cross-cutting substrate (fixed)

These once hit every command; all are corrected and covered by tests.

- **C1 — decimals.** `formatXAmount`/`formatXPrice` render **6 decimals**
  (`xBridgeSignificantDigits(1_000_000)==6`), not 7.
- **C2 — error `code`.** The C++ 1000-range enum (`xbridgeerror.h`):
  `UNKNOWN=1002`, `BAD_REQUEST=1004`, `NO_SESSION=1018`,
  `INSUFFICIENT_FUNDS=1019`, `TRANSACTION_NOT_FOUND=1021`,
  `INVALID_PARAMETERS=1025`, `INVALID_ADDRESS=1026`, `INVALID_STATE=1028`,
  `NOT_EXCHANGE_NODE=1029`.
- **C3 — error `error` text.** Routed through `xbridgeErrorText(code, arg)`,
  which prepends the per-code prefix.
- **C4 — param coercion.** Present-but-unparseable required params error with the
  C2-correct code (strict-coercion for missing `mustBool`/`mustInt` still lenient).
- **C5 — envelope.** JSON-RPC 1.0: only `result`/`error`/`id`, compact; business
  errors live in the `result` object, envelope `error` stays null.

Equivalent cross-cutting areas (already faithful): timestamps (`iso8601` 3-digit
ms `Z`), `status` strings (`statusString` == `TransactionDescr::strState`), and
the dispatch set (23 `dx*` + the `gettradingdata` alias; `getnetworkinfo` is a
Go-only extension).

## Cross-cutting substrate (added in the 2026-07-18 pass)

These once diverged or were unimplemented; all are now corrected and covered by
tests (`api/node_test.go`, `api/divergence_test.go`, `api/parity_dustfee_test.go`,
`api/swap_locktime_test.go`).

- **C6 — inbound packet signature verification.** Every XBridge packet is now
  verified against `pkt.Pubkey` in `Node.feed` (`api/node.go`), dropping spoofed
  servicenode packets. Mirrors `xbridgesession.cpp:736` (verbatim).
- **C7 — cancel/reject wire plumbing.** New `onRemoteCancel` / `onRemoteReject`
  ports of `xbridgesession.cpp:3288-3485`: the Exchange branch, the state-machine
  switch (stale-rebroadcast, rollback, refund broadcast), the signature gate, and
  reject-restore-to-pending. `Order` gains `SNodePubkey`/`OtherPubkey`/`MakerKey`/
  `Role`/`Reason`/`DepositSent`/`CounterpartyRedeemed`/`Orig*` mirroring
  `TransactionDescr`; `Store` gains a `history` terminal record; `crypto` gains
  `VerifyAgainst` for non-header keys.
- **C8 — lockTime drift check.** `acceptableLockTimeDrift` + `computeLockTimeFor`
  validate the counterparty's deposit lockTime before we broadcast our own deposit
  and before redeem (`api/locktime.go`, `api/swap.go`), mirroring
  `BtcWalletConnector::acceptableLockTimeDrift` (`xbridgewalletconnectorbtc.cpp:2331`)
  and its `xbridgesession.cpp:2464` call site.
- **C9 — dust/fee fidelity.** `effectiveDust` mirrors
  `xbridgewalletconnectorbtc.cpp:1526` (`dust = relayFee>0 ? 0.546*relayFee*COIN
  : configured : 5460`); `estimateFee` applies the `MinTxFee` floor
  (`xbridgewalletconnectorbtc.cpp:1952/1968`). `RelayFee` added to `CoinConf`.
- **C10 — network-token source.** `dxGetNetworkTokens` now learns tokens from real
  `SNREGISTER`/`SNPING`/`SNLISTPING` P2P messages via `p2p/servicenode.Registry`
  (wallet-token regex `^[^:]+$`, `xr`/`xrs` exclusion, 5-minute running window)
  instead of the ad-hoc `ServicesPingBody`. Still **TIER3** (bounded by P2P
  coverage); see matrix row.
- **C11 — empty order-book arrays.** `dxGetOrderBook` now emits `[]` (non-nil) for
  empty sides, matching C++'s default-constructed `Array` (`rpcxbridge.cpp:1568-1576`).
- **C12 — server robustness.** `ServeHTTP` recovers handler panics into a valid
  JSON-RPC envelope (`-32603`) so the daemon never emits an empty HTTP body
  (matches blocknetd). `cmd/xbridged` gains `-logfile`, datadir pre-creation, and
  rotating file logging via the new dependency-free `log` package.

## Tier 3 — architectural limits (thin-client)

These require a BLOCK block index / `blocknetd` and are intentionally out of
scope; see the "Tier 3 — architectural limits" section in [`api.md`](api.md):

- **`dxGetOrderHistory` / `dxGetTradingData`** reflect **session-local fills
  only**; `fee_txid` / `nodepubkey` are empty. C++ derives these from the BLOCK
  chain index across all servicenode-confirmed trades.
- **`dxGetNetworkTokens`** completeness is bounded by the P2P servicenode-ping
  coverage the client currently sees (now learned via the servicenode registry,
  C10); it falls back to the config list when no servicenodes are connected.
- **`dxGetLockedUtxos` / `dxGetUtxos` locked set** is derived from orders *this
  client knows about*; a UTXO locked by an unseen order is not subtracted.

## Verification gaps to close before shipping

- Byte-level capture of `OrderBody`/`AcceptingBody`/`CancelBody` UTXO-entry
  encoding vs live C++ (Go uses BIP137 proofs; C++ embeds real funding UTXOs).
- Live P2P verification of the swap-handshake claim/refund spends (in-memory
  only today).
