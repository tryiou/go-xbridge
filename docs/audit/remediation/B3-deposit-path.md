# Diff brief — deposit path (B3, CRYPTO-F85/F86/F87/F78/F90/F97 + STATE-F71 + SEC-F03)

Session-scratch notes for branch `fix/deposit-path`. Source of truth is the
C++ writers; the register rows (`CRYPTO-F85/F86/F87/F78/F90`, `STATE-F71`,
`SEC-F03`) carry the canonical status.

## Findings

- **CRYPTO-F85** (S2): no `checkDepositTransaction` in the Connector contract —
  the Go client builds its own deposit and never validates the counterparty's
  deposit (script, amount, confirmations, sequences, prevouts, fees) before
  responding CreatedB / redeeming at ConfirmA. SEC-F03's theft closes only when
  this lands.
- **CRYPTO-F86** (S2): `buildDeposit` broadcasts the deposit **before** building
  the pre-signed CLTV refund; a refund-build failure leaves a broadcast deposit
  with no escape hatch. C++ builds deposit → refund → then broadcasts
  (`processTransactionCreateA/B`).
- **CRYPTO-F87** (S2): the maker's make-time UTXO selection is never recorded as
  `Order.UsedCoins`, and `buildDeposit` re-runs `ListUnspent`, funding the
  deposit from **any** wallet UTXO instead of the exact locked/selected set
  (C++ `xtx->usedCoins`).
- **CRYPTO-F78** (S2): deposit network fee uses Go `estimateFee(nIn, 2)` vs C++
  `minTxFee1(nIn, 3)` = `(192·nIn + 34·nOut)·FeePerByte` with `nOut = 3`
  (`xbridgesession.cpp:1994, 2526`).
- **CRYPTO-F90** (S3): the claim (`redeemCounterparty`) spends hardcoded
  `vout 0` of `theirDepositTxID` at the **nominal** amount; C++ spends the
  **validated** `oBinTxVout` / `oBinTxP2SHAmount` and pays `outAmount +
  oOverpayment` (excess to the redeemer) (`redeemOrderCounterpartyDeposit`,
  `xbridgesession.cpp:3962-3971`).
- **STATE-F71** (S2): `OnHold`/`OnInit` never re-verify the hub's amounts,
  currency, price, or identity against the order (`processTransactionHold`,
  `:1404-1461`; `processTransactionInit`, `:1725-1760`).
- **SEC-F03** (S2): closed **by this branch** as composite acceptance — the
  taker must refuse an unvalidated A-deposit end-to-end (bad → wire-Cancel,
  no CreatedB); attacker model corrected from "lockup" to "theft".

## Session decisions (documented divergences)

1. **Init order-detail check is currently OR; C++ is `&&`.** C++
   `processTransactionInit` rejects only when **every** field mismatches
   (`xbridgesession.cpp:1750-1756`, `&&` of `!=`s); Go rejects on **any**
   single-field mismatch. Tracked as STATE-F84 (fix-queued, bug-for-bug `&&`
   required by the §0 identity standard); the old "security > fidelity"
   rationale is void.
2. **Wire-Cancel on bad deposit**: C++ sends `crBadADepositTx`/`crBadBDepositTx`
   on a definitively-bad deposit and **no response** on "wait" (`processLater` —
   the hub retransmits). Go mirrors both.
3. **W0 — unit-scale correction (new, promoted from planning):** `buildDeposit`
   fed XBridge-base order amounts (`c.srcAmt`, `estimateFee`) alongside
   native-base funding (`wallet.Utxo.Amount`) into `BuildDepositTx`
   (`swap/deposit.go:104-147`), where output `Amount+fee2` and change
   `total − Amount − fee − fee2` mix the two scales. C++ locks `outAmount+fee2`
   whole-coins → `× COIN` native (`xbridgewalletconnectorbtc.cpp:2094, 2442-2450`).
   For any conf with `COIN != 1e6` (BTC `1e8`) Go deposited 100× too little
   on-chain; BLOCK (`COIN = 1e6`) is unaffected, which is why fixtures and
   `make parity` never caught it. Fix: convert at the `api/swap.go` boundary
   with `fromXBridgeAmt` (`api/handlers.go:1602`), keeping the `swap` package
   consistently on-chain-native. **Prerequisite to F85/F90** (cannot validate
   counterparty amounts against a broken deposit scale).

## C++ reference (parity baseline)

- `BtcWalletConnector::checkDepositTransaction` —
  `xbridgewalletconnectorbtc.cpp:1981-2194`:
  - `getrawtransaction <txid>` (raw hex) missing → `"no tx found …waiting"` →
    caller's `processLater`.
  - `decoderawtransaction <hex>` fails → `"bad counterparty deposit, decode
    transaction failed"` → done (**bad**, not wait).
  - Confirmation gate `:2018-2045`: `decoderawtransaction` output has no
    `confirmations`, so C++ falls to `gettxout <txid> 0`; unknown → wait, `confs
    < requiredConfirmations` → wait.
  - Vin scan `:2049-2127`: `sequence != SEQUENCE_FINAL(0xffffffff)` → bad;
    prevout looked up via verbose `getrawtransaction <vintxid> 1`; missing vin
    tx / JSON parse fail → wait; bad prevout structure → bad; sum `totalVinAmount`.
  - Vout scan `:2129-2170`: non-real value / missing `n` / negative → bad; match
    `scriptPubKey.hex == expectedScript` **and** `amount ≤ value + ε` → record
    `p2shAmount`(whole) + `depositTxVout`; no match → `"no valid p2sh"` → bad.
  - Fee checks `:2172-2193`: `counterpartyFees = totalVin − totalVout`;
    `fee1 = minTxFee1(vins, vouts)`, `fee2 = minTxFee2(1,1)`;
    `counterpartyFees < fee1·0.95` → bad; `p2shAmount < amount + fee2·0.95` → bad;
    `excess = p2shAmount − amount − fee2` when positive;
    `p2shAmount *= COIN` → **XBridge base** out-param.
  - Out-params: `isGood`, `p2shAmount`, `depositTxVout`, `excessAmount`.
- Call sites: taker checks the maker's A deposit
  `xbridgesession.cpp:2459-2515` (expected script `createDepositUnlockScript(
  mPubKey, xtx->mPubKey, oHashedSecret, opponentLockTime)`, amount
  `checkAmount = xtx->toAmount/COIN`, `connTo` = connector(toCurrency) — the
  taker's local copy is mirrored, so `to` = the maker's `from` = the A deposit);
  maker checks the taker's B deposit `:2921-2981` (script `createDepositUnlockScript(
  oPubKey, mPubKey, hx, opponentLockTime)`, amount `toAmount/COIN`). Both:
  bad → `sendCancelTransaction(crBadADepositTx|crBadBDepositTx)`; wait →
  `processLater` (no reply, hub retransmits); good → `xtx->oBinTxP2SHAmount =
  p2shAmount`, `oBinTxVout = counterPartyVoutN`, `oOverpayment = excess`.
- Locktime pre-checks `:2464-2473` (A) / `:2926-2935` (B):
  `acceptableLockTimeDrift` fail → `sendCancelTransaction(crBadALockTime|
  crBadBLockTime)`. Go already ports this check (`acceptableLockTimeDrift`,
  `api/swap.go:442-444, 536-538`) but the C++ **also** sends the cancel packet;
  Go currently just returns an error.
- `sendCancelTransaction` — `xbridgesession.cpp:3525-3576`: builds a
  `xbcTransactionCancel` packet, signs with `mPrivKey`, and immediately
  `processTransactionCancel(reply)` locally (self-rollback), then broadcasts.
- Deposit fee / amount math:
  - `minTxFee1(nIn, nOut) = (192·nIn + 34·nOut)·FeePerByte`
    (`xbridgewalletconnectorbtc.cpp:1949-1962`), deposit loop uses `nOut = 3`
    (`xbridgesession.cpp:1994, 2526`).
  - Deposit locks `outAmount + fee2` (`:2094, 2615`), change `rest` back to the
    largest input's address when `> dust` (`:2097-2102, 2618-2623`).
  - `checkDepositTransaction` amount conversion: `whole = xtx->{to}Amount/COIN`
    (integer divide then cast — `:2459-2460, 2921-2922`).
- Claim (redeem) — `redeemOrderCounterpartyDeposit` `:3920-4016`: input
  `(oBinTxId, oBinTxVout, oBinTxP2SHAmount/connTo->COIN)`, output
  `(toAddr, outAmount + oOverpayment)`, `outAmount = toAmount/COIN`.
- Hold re-verify — `processTransactionHold` `:1404-1461`:
  - taker (role B): `samount == xtx->fromAmount && damount == xtx->toAmount`;
    `failPrice = !acceptibleOrderPrice(origFrom, origTo, fromAmount, toAmount)`.
  - maker (role A): `damount <= fromAmt`; `samount <= toAmt`; partial:
    `damount >= fromAmount_ceil/COIN`; `failPrice = !acceptibleOrderPrice(from,
    to, samount, damount)`. Any fail → drop, no reply.
- Init re-verify — `processTransactionInit` `:1675-1760`: state gate
  `xtx->state >= trInitialized` → drop (`:1725-1732`); field checks `:1750-1756`
  (**OR in Go**), address checks; fail → drop, no reply.

## Go deltas (this branch)

1. **`wallet.Connector` (`wallet/connector.go:73`)** gains
   `CheckDepositTransaction(depositTxID, expectedScriptHex string,
   expectedAmount uint64, requiredConfirmations int) (DepositCheck, error)` with
   sentinels `ErrDepositNotReady` (wait → `processLater`) and `ErrNoChainSource`
   (`LocalConnector`). `DepositCheck{IsGood bool, P2SHAmount uint64,
   DepositVout uint32, Excess uint64}` — P2SHAmount/Excess in XBridge base
   (`×1e6`), matching C++ `× COIN`. Mirror of `xbridgewalletconnectorbtc.cpp:
   1981-2194` via `getrawtransaction <txid> 0` + local `coins.Deserialize`
   (= `decoderawtransaction`; no wallet dependency on verbosity) +
   `gettxout <txid> 0` for the confirmation gate. `expectedAmount` is XBridge
   base; whole = `float64(expectedAmount/1e6)` (C++ integer-divide-then-cast).
2. **`api/swap.go`**:
   - **F85 call sites** — taker `OnCreateB` task (before `buildDeposit`):
     connector `c.dstCur`, `expectedAmount = c.dstAmt`,
     `expectedScriptHex = DepositSpec{DepositorPub: c.theirPub,
     CounterpartyPub: c.pubKey, Hash: c.theirSecretHash,
     LockTime: c.theirLockTime}.P2SHScript()` hex; `IsGood=false` →
     `sendSelfCancel(crBadADepositTx)`; `ErrDepositNotReady` → error (no reply).
     Maker `OnConfirmA` task (before redeem): connector `c.dstCur`,
     `expectedAmount = c.dstAmt`, script `Hash: c.secretHash`,
     `LockTime: c.theirLockTime`; `IsGood=false` → `sendSelfCancel(
     crBadBDepositTx)`. `DepositCheck` rides the task result into the resume,
     which persists `OBinTxVout`/`OBinTxP2SHAmount`/`OOverpayment` on the order.
   - **F86 reorder** in `buildDeposit` (`:1104-1116`): sign → compute txid
     locally (`txIDFromHex`), set `c.ourDepositTxID/ourLockTime` → build refund →
     broadcast (log the RPC `sentid` only, C++ `:2187-2191`).
   - **F87**: `buildDeposit` consumes `c.funding` (snapshot from
     `store.Get(orderID).UsedCoins`) instead of `conn.ListUnspent`
     (`:1047`). Maker `MakeOrder` records `o.UsedCoins` before `store.Add`
     (`api/node.go`), in both the autoSplit-rebuild and direct branches.
   - **F78**: `estimateFee(cc, len(funding), 3)` at `:1064` (C++
     `minTxFee1(nIn,3)`, `xbridgesession.cpp:1994/:2526`).
   - **F90**: `redeemCounterparty` (`:1166-1218`) spends the VALIDATED deposit —
     exact native `P2SHNative` at `DepositVout` — paying `p2sh − fee2` (the
     redeemer keeps any excess; C++ output = outAmount + oOverpayment =
     depositP2SH − fee2, `xbridgesession.cpp:3971`); `buildRefundTx` pays the
     full nominal (fee2 is the implicit miner fee, C++ `:2134/2149`); order
     fields `OBinTxVout/OBinTxP2SHAmount/OOverpayment` persisted.
   - **W0 → CRYPTO-F97 (promoted)**: convert `c.srcAmt`, `fee`, `fee2`
     (deposit), `fee`, `spec.Amount` (refund `:1127-1148`), and claim output
     (F90) to **native** via `fromXBridgeAmt` before `BuildDepositTx`/builders;
     `swap` package stays on-chain-native. Register promotion: CRYPTO-F97.
   - **STATE-F71**: `OnHold` (`:283-299`) + `OnInit` (`:302-308`) re-verify
     amounts/currency/price/identity per C++ (intended OR for Init; state gate
     `csInitialized`).
   - **Wire-Cancel helper** `sendSelfCancel(idHex, reason)`: build CancelBody,
     sign with session privkey, local rollback via `handleRemoteCancel`
     (self-signed ⇒ verifies ⇒ `iCanceled` ⇒ rollback), broadcast via
     `WritePacket(pkt, [20]byte{})` (C++ `sendCancelTransaction` +
     `processTransactionCancel`). Reason consts added for B3 only:
     `crBadALockTime=18, crBadBLockTime=19, crBadADepositTx=14,
     crBadBDepositTx=15` (`xbridgepacket.h:37-42`); full enum is B8/STATE-F73.
3. **`api/order.go` / `api/persist.go`**: `Order` + `persistedSwap` gain
   `OBinTxVout uint32`, `OBinTxP2SHAmount uint64`, `OOverpayment uint64`
   (XBridge base); deep-copied in `Copy()`; persisted/restored.
4. **Test mocks** (all four `Connector` implementers): `fakeConnector`
   (`api/swap_test.go:72` — implements from its `rawTx` hex store via
   `coins.Deserialize`, plus a confirmations hook), `stubConn`
   (`api/wallet_methods_test.go:38`, canned result),
   parity harness `stubConn` (`tools/parity/go/harness_test.go`, `IsGood:true`).

## Golden-vector strategy

- `checkDepositTransaction` goldens from the C++ **writers** (not comments),
  driven by the `mockRPC` httptest harness (`wallet/rpc_test.go:32`): good
  deposit; `requiredConfirmations` wait; bad sequence; no-p2sh; fee1 shortfall;
  fee2 shortfall; excess computed; XBridge-base `p2shAmount`.
- `buildDeposit`/refund/claim byte goldens at **native** scale: BTC fixture
  (`COIN=1e8`) proves `output = fromXBridgeAmt(srcAmt+fee2)`, change =
  `total − native(srcAmt+fee+fee2)`, refund = `native(srcAmt − fee)`, claim
  input `theirP2SHAmount`, output `native(dstAmt+excess) − fee`.
- F78: `parity_fee_test.go` `(nIn,3)` vector.
- Parity gate (`make parity` + `make canary`) stays green — the harness stub
  returns `IsGood:true`, and no deposit golden exists there.

## Tests

- `wallet/rpc_test.go`: `TestCheckDepositTransaction*` goldens (above).
- `api/swap_test.go`: `TestSwapHandshake` keeps green; new
  `TestBuildDepositNativeScale` (BTC), `TestRefundNativeScale`,
  `TestRedeemCounterpartyPayout` (vout + p2shAmount + excess).
- `api/swap_two_phase_test.go`: refund-before-broadcast order; refund-build
  failure ⇒ no broadcast; bad-deposit ⇒ Cancel broadcast + rollback + no
  CreatedB; not-ready ⇒ no response.
- `api/swap_security_test.go` (SEC-F03): composite refusal — taker refuses an
  unvalidated A-deposit; mismatched Hold/Init dropped with no response.
- `api/persist_test.go`: `OBinTxVout`/`OBinTxP2SHAmount`/`OOverpayment`
  round-trip.
