# XBridge Swap State Machine — Go port

This is the canonical spec for `swap/` (Go port of
`src/xbridge/xbridgetransaction.{h,cpp}`). It is the **highest-risk** part of
the reimplementation, so the transition logic is mirrored field-for-field from
C++ and covered by `swap/transaction_test.go`.

## 1. Roles

A swap has exactly two members:

- **A (maker)** — created the order (`xbcTransaction`).
- **B (taker)** — created the complementary order that joins A's
  (`xbcTransactionAccepting` → `tryJoin`).

Each `Member` has a `Source` address (sends FROM) and a `Dest` address
(receives TO), both 20-byte uint160 values.

The maker order describes the trade from A's perspective: A gives
`SourceCurrency:SourceAmount` and receives `DestCurrency:DestAmount`.

## 2. States

```
trInvalid trNew trJoined trHold trInitialized trCreated trSigned trCommited
trFinished trCancelled trDropped
```

`trSigned` / `trCommited` exist in the enum but are **not** part of the
`increaseStateCounter` progression (signing/commit happen in the
`xbridgesession*` deposit/refund layer, not the two-confirmation gate). The
gate walks:

```
trNew --TryJoin--> trJoined --(both Source)--> trHold
   --(both Dest)--> trInitialized --(both Source)--> trCreated
   --(both Dest)--> trFinished
```

`trCancelled`, `trDropped`, `trFinished` are terminal (`State.IsTerminal`).

## 3. Join (`TryJoin`)

A taker order `o` joins maker `t` iff:

- both are `trNew`;
- `t.SourceCurrency == o.DestCurrency` and `t.DestCurrency == o.SourceCurrency`;
- `t.PartialAllowed == o.PartialAllowed`;
- **non-partial:** `t.SourceAmount == o.DestAmount` and `t.DestAmount == o.SourceAmount`
  (exact amounts);
- **partial:** `t.SourceAmount >= o.DestAmount`, `t.DestAmount >= o.SourceAmount`,
  and `o.DestAmount >= t.MinFromAmount`; plus `xBridgePartialOrderDriftCheck`
  (price-integrity / satoshi-level drift band, ported in `swap/price.go`).

On success `t.B = o.A` and state → `trJoined`.

## 4. Progression (`IncreaseStateCounter(state, from)`)

Each phase requires **both** members to confirm before advancing. The confirming
address `from` is matched against a participant's `Source` or `Dest` address
depending on the phase:

| Current state | `from` must match | → next |
|---------------|-------------------|--------|
| `trJoined`    | `A.Source` & `B.Source` | `trHold` |
| `trHold`      | `A.Dest` & `B.Dest`     | `trInitialized` |
| `trInitialized` | `A.Source` & `B.Source` | `trCreated` |
| `trCreated`   | `A.Dest` & `B.Dest`     | `trFinished` |

The two confirmation flags are **reused across phases** (reset after each
transition), matching C++'s single `m_a_stateChanged`/`m_b_stateChanged` pair.

- An unrecognized `from` is a **no-op** (state unchanged, returns current state).
- A `state` argument not equal to the current state returns `trInvalid` and
  leaves the state unchanged.
- Phases outside the four handled above (incl. `trSigned`/`trCommited`) return
  `trInvalid`.

## 5. Timing

| Constant | Value | Meaning |
|----------|-------|---------|
| `LockTime` | 600 s | deposit lock time |
| `PendingTTL` | 360 s | `trNew` idle expiry |
| `TTL` | 3600 s | post-`trNew` idle expiry |
| `DeadlineTTL` | 604800 s | `trNew` creation deadline |
| `BlocksTTL` | 10080 | block-height TTL (7 d) |

`IsExpired(now)`:

- `trNew` && (age-from-creation > `DeadlineTTL` || age-since-last > `PendingTTL`)
  → expired;
- state > `trNew` && age-since-last > `TTL` → expired.

The block-height variant (`isExpiredByBlockNumber`) needs chain context and is
**not** ported yet.

## 6. Not yet ported

- `isExpiredByBlockNumber` (requires block-index lookup).
- The full `xbridgesession*` coordination loop: the lockTime exchange
  (xbcTransactionInit, command 8), broadcasting/observing deposit txids, and the
  claim/refund *spending* of the HTLC outputs (the ELSE payment branch reveals
  the secret; the IF branch is the CLTV refund). `swap/deposit.go` already
  builds the deposit tx + HTLC scripts and `swap/session.go` advances the gate
  when both deposits confirm; the P2P packet exchange + output spending remain.

## 7. Deposit layer (`swap/deposit.go`, `swap/session.go`)

The deposit/refund (`xbridgesession*`) layer is ported at the construction +
gating level:

- `DepositSpec` describes one side's HTLC deposit: `RedeemScript()` builds the
  script via `coins.BuildDepositUnlockScript`; `P2SHScript()` wraps it;
  `BuildDepositTx` locks `Amount` into the P2SH, spending funding UTXOs with a
  CLTV-enabled input sequence; `SignInput`/`RefundScriptSig` sign + assemble the
  refund. The depositor generates a 33-byte `Secret`; `SecretHash()` is
  HASH160(Secret). The counterparty adopts only the `Hash` (it never learns the
  secret).
- `Session` wraps a joined `Transaction` for the local `Role`. `CreateLocalDeposit`
  generates the secret + builds the local `DepositSpec` (amount/currency derived
  from the role: maker locks SourceAmount/SourceCurrency, taker locks
  DestAmount/DestCurrency). `AdoptCounterparty` records the revealed `Hash` +
  lockTime. `ConfirmLocalDeposit`/`ConfirmOtherDeposit` advance the progression
  gate (trJoined → trHold) once both sides' deposits confirm.

**Fidelity note:** C++ never assigns `trSigned`/`trCommited` to the transaction
state (grep `xbridgetransaction.cpp` / `xbridgesession.cpp` — only trHold /
trInitialized / trCreated / trFinished / trCancelled / trDropped are set). Those
two enum values are vestigial; deposits instead *gate* the progression. This port
mirrors that: the `Session` tracks the deposit lifecycle on its own fields and
calls `IncreaseStateCounter`, and does not set `trSigned`/`trCommited`.
