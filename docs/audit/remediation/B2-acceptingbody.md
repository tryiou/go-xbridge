# Diff brief — `pkg/api` accepting-body (B2, CRYPTO-F84)

Session-scratch notes for branch `fix/wire-acceptingbody`. Source of truth is
the C++ writers; keep this file up to date while working.

## Finding

CRYPTO-F84 (Blocker): `Node.TakeOrder` (`api/node.go:1260`) broadcasts
`xbcTransactionAccepting` with an `AcceptingBody` whose `ServiceNodeFeeTx` and
`Utxos` are empty. C++ always fills both (`acceptXBridgeTransaction` →
`sendAcceptingTransaction`). The hub receiver drops any accepting with
`packet->size() < 188` (`xbridgesession.cpp:855`) and any body whose fee tx
lacks a ≥ `serviceNodeFee·COIN` output to the SN payment address
(`:895-911`, `crBadFeeTx`). A Go take therefore cannot be accepted by a real
hub.

Second wire bug found during review: `blockContext` (`node.go:391`) stamps the
two 8-byte block hashes with the **internal LE bytes**, but C++ `memcpy`s the
first **8 ASCII chars of the display hex** from the `getblockhash` string
(`xbridgeapp.cpp:2420-2424`). Wrong bytes on the wire.

## C++ source of truth

- `acceptXBridgeTransaction` — `xbridgeapp.cpp:2107-2402`. Error order
  (all verified):
  - `:2133-2145` from/to connectors missing → `NO_SESSION` (`revertOrder`).
  - `:2147-2157` `connFrom->isDustAmount(fromAmount)` /
    `connTo->isDustAmount(toAmount)` → `DUST`.
  - `:2159-2163` `availableBalance() < xBridgeAmountFromReal(connTo->serviceNodeFee)`
    → `INSUFFICIENT_FUNDS_DX`. `availableBalance()` = Σ `CWallet::GetBalance()`
    over `GetWallets()` (`xbridgeapp.h:798-806`) = the wallets loaded in the
    running daemon — for blocknetd that is the **BLOCK wallet only**
    (`wallet/wallet.h:67`, `vpwallets` registry), NOT every XBridge coin
    connector. `serviceNodeFee = .015` is a hardcoded `WalletParam` ctor default
    (`xbridgewallet.h:119`) — **not a conf key** (Go must NOT add a conf key).
    The native-vs-xbridge-unit comparison (satoshi balance vs 15000 xbridge
    units) is a C++ quirk reproduced as-is.
  - `:2165-2204` `sPubKey` len must be 33 → `NO_SERVICE_NODE`; `getSn` +
    `Decompress` fallback → `NO_SERVICE_NODE`;
    `snodeCollateralAddress = snode.getPaymentAddress()` (**registry** payment
    addr — the fee DESTINATION; the body's `hubAddress` stays `ptr->hubAddress`
    = `GetID(sPubKey)` = Go `o.HubAddress`).
  - `:2206-2229` order-info JSON: `maxBytes = nMaxDatacarrierBytes-3 = 157`
    (`MAX_OP_RETURN_RELAY = 160`, `script/standard.h:34`); base
    `["", fromCur, fromAmt, toCur, toAmt]`; truncate `orderId` hex to fit;
    insert at front; `> 157` → `INVALID_ONCHAIN_HISTORY`. `json_spirit` compact
    (no spaces). Normal case `["<64hex>","BTC",1500000,"LTC",300000]` = **95
    bytes**, no truncation; max base for valid input = 68, so truncation is
    unreachable — implement for fidelity.
  - `:2231-2232` `destScript = GetScriptForDestination(snodeCollateralAddress)`;
    `data = ToByteVector(strInfo)`.
  - `:2236-2267` BLOCK fee (inside `m_utxosOrderLock`): `rpc::unspentP2PKH`
    (`bitcoinrpcconnector.cpp:273-302` — BLOCK wallet, **minconf 1**, **25-byte
    p2pkh `76a914…88ac` only**) → fail `INSUFFICIENT_FUNDS`; exclude
    `getAllLockedUtxos(connFrom->currency)`;
    `createFeeTransaction(destScript, connFrom->serviceNodeFee=.015,
    blockFeePerByte=40/COIN, data, feeOutputs, ptr->feeUtxos, ptr->rawFeeTx)`
    (`bitcoinrpcconnector.cpp:79-269`) → any fail `INSUFFICIENT_FUNDS`;
    `lockFeeUtxos`.
  - `:2269-2358` taker funding (same lock scope):
    `connFrom->getUnspent(outputs, excludedUtxos)` with `excludedUtxos =
    getAllLockedUtxos(connFrom->currency)` (fee utxos already locked at `:2267`
    → auto-excluded); `selectUtxos(from, outputs, minTxFee1, minTxFee2,
    fromAmount, TransactionDescr::COIN=1e6, …)` → fail `INSUFFICIENT_FUNDS`
    (+`unlockFeeUtxos`); sign each `entry` via
    `signMessage(entry.address, entry.toString())` (base64 → decode fail =
    `FUNDS_NOT_SIGNED`), `rawAddress = toXAddr` (≠20 → `INVALID_ADDRESS`), sig
    ≠ 65 → `INVALID_SIGNATURE` (any err → revert + `unlockFeeUtxos`);
    `ptr->usedCoins = outputsForUse`; `lockCoins(connFrom->currency, …)` → fail
    `INSUFFICIENT_FUNDS`.
  - `:2361-2374` block heights+hashes from **both** connectors; any failure →
    `revertOrder`, `unlockCoins` + `unlockFeeUtxos`, `clearUsedCoins`,
    `NO_SESSION`.
  - `:2376-2398` `role = 'B'`; `connTo->newKeyPair(mPubKey, mPrivKey)`;
    `sendAcceptingTransaction`.
- `sendAcceptingTransaction` — `xbridgeapp.cpp:2406-2469`. Wire order (==
  `proto.AcceptingBody.Marshal`, `body_types.go:287`): `hub(20) ‖ id(32) ‖
  u32(len(ParseHex(rawFeeTx))) ‖ feeTxBytes ‖ from(20) ‖ fc(8) ‖
  fromAmount(u64) ‖ fromBlockHeight(u32) ‖ fromhash(8) ‖ to(20) ‖ tc(8) ‖
  toAmount(u64) ‖ toBlockHeight(u32) ‖ tohash(8) ‖ u32(utxoCount) ‖
  entries(121 each: txid32 ‖ vout4 ‖ rawAddr20 ‖ sig65)`. `fromhash`/`tohash`
  = first 8 ASCII chars of the `getblockhash` display hex (`:2420-2424`).
- `createFeeTransaction` — `bitcoinrpcconnector.cpp:79-269`:
  - `:95-100` `estFee(in,out) = (192·in + 34·out) · feePerByte`, with
    `feePerByte = 40/1e8` (**hardcoded — NOT the conf `FeePerByte` /
    `estimateFee` model**).
  - `:101-130` `selectFeeUtxos`: ideal window
    `[feeAmount(amt,1,3), feeAmount(amt,1,3) + estFee(1,3)·100)`; gt/lte split
    paths; **no address filter**; fail → empty.
  - `:132-160` change: `changeAddr = sel[0].address`;
    `changeAmt = (inputAmt − amt − estFee(n,3))·1e8`; change output **only if
    ≥ 5460**.
  - vouts: `[OP_RETURN data, amt→dstScript (p2pkh), change?]`; standard
    `CMutableTransaction` ⇒ **version 1, no nTime field, Sequence 0xffffffff,
    locktime 0**; wallet-signed; `rawFeeTx = EncodeHexTx`.
- Receiver — `xbridgesession.cpp:848-916`: `packet->size() < 188` → drop;
  `feeTxSize` at `+129`; `DecodeHexTx` fail → `crBadFeeTx`; some vout
  `nValue ≥ serviceNodeFee·COIN` **and** script ==
  `GetScriptForDestination(snodeEntry.address)` **or**
  `getSn(key).getPaymentAddress()`.
- dxTakeOrder pre-accept order — `rpcxbridge.cpp:1191-1235`:
  amount/partial sizing → **`checkAcceptParams(toCurrency, fromSize)`
  (`:1204`)** → self-trade (`isLocal`) → connectors to-/from-currency
  (`:1216-1217`) → `isValidAddress` from/to (`:1219-1225`) → dryrun branch
  (`:1227`). `checkAcceptParams` (`xbridgeapp.cpp:2539-2541`) → `checkAmount`
  (`:2561-2580`): missing connector → `NO_SESSION`; wallet balance
  < `fromSize/COIN` → `INSUFFICIENT_FUNDS`. `getWalletBalance`
  (`xbridgewalletconnector.cpp:52-73`) = Σ `getUnspent` whole-coin values at
  minconf 1 (empty `listUnspent` params, p2pkh only, minus
  `getAllLockedUtxos(currency)`); `getUnspent` failure → `-1` → fails the check.
- Dryrun — `rpcxbridge.cpp:1227,1236-1252`: **preview rendered BEFORE
  `acceptXBridgeTransaction`**, so NO dust / NO availableBalance / NO hub / NO
  fee / NO funding work ever runs on a dryrun — only the gates above. The dust
  and `INSUFFICIENT_FUNDS_DX` checks live in the accept path
  (`xbridgeapp.cpp:2147-2163`) and are therefore skipped by dryruns.

**Size math (correction):** fixed body = **156**, not 152:
`20+32+4+20+8+8+4+8+20+8+8+4+8+4`. `len ≥ 188 ⇔ len(feeTx) + 121·N ≥ 32`; a
real fee tx is ~224 bytes (1-in/3-out), so a real body ≥ ~500 bytes.

## Go deltas

### `wallet/connector.go` — extend `Connector` interface
- Add `GetBalance() (uint64, error)` — port of `CWallet::GetBalance()`
  (`getbalance`, wallet-wide confirmed balance, native base units). Implement
  in:
  - `wallet/rpc.go`: `getbalance` RPC → `float64` →
    `uint64(v * float64(coin.Coin))`.
  - `wallet/local.go`: confirmed available balance from the local store.
  - all test fakes (`wallet_methods_test.go` `stubConn` etc.).

### `api/fee_tx.go` — NEW
- `feeOrderInfo(id [32]byte, fromCur string, fromAmt uint64, toCur string,
  toAmt uint64) ([]byte, error)` — C++ `:2209-2229`; `encoding/json.Marshal`
  of `["<hex>", fromCur, fromAmt, toCur, toAmt]`; truncation against 157; KAT
  `["<64hex>","BTC",1500000,"LTC",300000]` = 95 bytes.
- `estFeeBlock(in, out int) float64` = `(192·in + 34·out) · 40/1e8`
  (independent of `estimateFee`).
- `selectFeeUtxos(a []wallet.Utxo, amt float64) ([]wallet.Utxo, bool)` — C++
  `bitcoinrpcconnector.cpp:101-130`, whole-coin doubles.
- `isP2PKH25(scriptHex string) bool` — 25-byte `76a914…88ac`.
- `buildServiceNodeFeeTx(conn wallet.Connector, cc config.CoinConf,
  dest [20]byte, data []byte, avail []wallet.Utxo) (rawHex string,
  inputs []wallet.Utxo, rerr *rpcError)`:
  1. `selectFeeUtxos(avail, .015)` → fail ⇒ `INSUFFICIENT_FUNDS`.
  2. `tx := coins.Tx{Version: int32(cc.TxVersion), WithTime: cc.TxWithTimeField}`
     (BLOCK conf → **v1, no time** — mirrors C++ stock `CMutableTransaction`),
     Sequence `0xffffffff`, prevTxs from `inputs`
     (`coins.BuildP2PKHScript` of each input's `ScriptPubKey`/amount).
  3. vouts: `TxOut{0, OP_RETURN data}`,
     `TxOut{uint64(.015·float64(cc.Coin)), coins.BuildP2PKHScript(dest)}`,
     change `TxOut{changeAmt, BuildP2PKHScript(decoded sel[0].Address)}` only
     if `≥ 5460`; changeAmt in native units.
  4. `SignRawTransaction(tx.Hex(), prevTxs)`; not complete ⇒
     `INSUFFICIENT_FUNDS`. **Every failure → `INSUFFICIENT_FUNDS`** (matches
     C++ `:2240-2264`).

### `api/node.go` — TakeOrder restructure (mirror `rpcxbridge.cpp:1191-1235` +
`acceptXBridgeTransaction :2107-2402`)
Final gate order (verified against the C++ writers, 2026-08-11 — this supersedes
the earlier scratch ordering that put dust before dryrun and summed all
connectors in `availableBalance`):
1. `requireWrite`, same-address check, order lookup, amount sizing — **unchanged**.
2. **NEW checkAcceptParams** (C++ `rpcxbridge.cpp:1204`): `n.checkAcceptParams
   (o.ToCurrency, fromSize)` — missing connector ⇒ `NO_SESSION`; wallet balance
   (Σ `ListUnspent(1)` whole-coin, p2pkh-only, minus `LockedUtxoInfo()`) <
   `fromSize/COIN` ⇒ `INSUFFICIENT_FUNDS`; `ListUnspent` error ⇒
   `INSUFFICIENT_FUNDS` (C++ `getWalletBalance` → `-1`). Runs BEFORE self-trade.
3. Self-trade check — **unchanged**.
4. Connector presence (pre-dryrun, C++ `:1216-1217`): to-currency then
   from-currency ⇒ `NO_SESSION`.
5. **MOVED decodeAddr from/to** (C++ `isValidAddress` `:1219-1225`): runs AFTER
   the amount/checkAcceptParams/self-trade/connector gates ⇒ `INVALID_ADDRESS`.
6. **Dryrun return** (C++ `:1227`): `if p.DryRun { return
   o.toTakeDryrunResult(fromSize, toSize) }` here — BEFORE the accept-path dust
   and funds checks.
7. **Dust** (accept path only, C++ `:2147-2157`): `isDustNative(ccTo,
   fromSize)` / `isDustNative(ccFrom, toSize)` ⇒ `DUST`.
8. **Funds pre-check** (C++ `:2159-2163`): `availableBalance()` = **BLOCK
   connector only** (Σ `GetWallets()` in the daemon = the BLOCK wallet); `blk ==
   nil` ⇒ 0; any error ⇒ `NO_SESSION`; `< xBridgeIntFromReal(.015)` ⇒
   `INSUFFICIENT_FUNDS_DX`.
9. **Hub gate** (unchanged): `decodePub33` + `hubRegistered` ⇒ `NO_SERVICE_NODE`;
   fee dest = `PaymentAddress(hubKey)`.
10. **Fee** (C++ `:2236-2267`): `blk.ListUnspent(1)` → filter `isP2PKH25` →
    exclude `LockedUtxoInfo()` → `feeOrderInfo` → `buildServiceNodeFeeTx`
    ⇒ `INSUFFICIENT_FUNDS`; **feeOrderInfo overflow (C++ `:2226-2228`
    `INVALID_ONCHAIN_HISTORY`) maps to `errInvalidOnchainHist` (1033), not the
    generic 1019**; keep `feeInputs`.
11. **Funding** (C++ `:2269-2358`): `conn.ListUnspent(1)` — **minconf 1, NOT
    `cc.Confirmations`** (C++ `getUnspent` calls `rpc::listUnspent` with an
    empty params array = wallet default minconf 1,
    `xbridgewalletconnectorbtc.cpp:1604-1612`); exclusion = `LockedUtxoInfo()`
    ∪ feeInput keys; `selectUtxos(…)` ⇒ `INSUFFICIENT_FUNDS`;
    `buildUtxoProofs` mapping `errBadSigLen`→`INVALID_SIGNATURE`,
    `errBadAddr`→`INVALID_ADDRESS`, else→`FUNDS_NOT_SIGNED`; set `acc.Utxos`,
    `usedCoins`.
12. **blockContext** (`:375-393`): `GetBlockHash(h)` → reverse internal LE →
    display hex → first 8 **ASCII chars** → `[8]byte`; error ⇒ `NO_SESSION`.
13. **acc**, **M key**, **pkt build + sign + WritePacket** — unchanged.
14. **submit closure**: set `stored.UsedCoins`, `stored.FeeUtxos` (runtime-only —
    see settled D2), `stored.Utxos = acc.Utxos`.

### `api/order.go` / `api/store.go`
- `Order` (`order.go:25`) gains `UsedCoins []wallet.Utxo` (C++
  `xtx->usedCoins`; B3 seam) and `FeeUtxos []wallet.Utxo`. `clearUsedCoins`
  (`:274`) clears both.
- `store.go` `LockedUtxoInfo` (`:350`) additionally scans `o.FeeUtxos` (key
  `TxID+":"+Vout`, display hex).

### Test harness
- `newHubNode`/`newWalletTestCtx` gain a **BLOCK connector** with ≥1 p2pkh
  utxo (BLOCK `Coin: 100000000`, `TxVersion: 1`); `stubConn` implements
  `GetBalance`; registry fixtures set a non-zero `PaymentAddress`. Touches
  `hub_gate_test.go`, `wallet_methods_test.go`, `divergence_test.go`.

## Tests

- `proto/body_test.go`: byte-exact `AcceptingBody` golden (fixed fee bytes + 1
  fixed utxo entry → exact hex, asserts `len ≥ 188` for a realistic body).
- `api/fee_tx_test.go` (NEW): `feeOrderInfo` 95-byte KAT + truncation math;
  `estFeeBlock`/`selectFeeUtxos` vectors (ideal / gt / lt-sum / fail);
  `buildServiceNodeFeeTx` golden (deterministic utxos → exact signed-hex
  shape: OP_RETURN, 0.015 output to registry payment addr, change ≥ 5460,
  v1/no-time/Seq ffffffff); `isP2PKH25` accept/reject.
- `api/hub_gate_test.go`: extend `TestTakeOrderPinnedAccepting` — fee tx +
  utxos non-empty, fee output pays the **registry payment address**, body ≥
  188; unfunded BLOCK → `INSUFFICIENT_FUNDS`; unfunded from-currency →
  `INSUFFICIENT_FUNDS`; no-BLOCK-connector → **`INSUFFICIENT_FUNDS_DX`** (the
  BLOCK-only `availableBalance()` = 0 fires before the fee-prep step, C++
  `:2161`); configured-but-empty-BLOCK-wallet → `INSUFFICIENT_FUNDS_DX`
  (`TestTakeOrderEmptyBlockWallet` — A1 gate, distinct from
  `TestTakeOrderUnfundedBlock`'s fee-prep `INSUFFICIENT_FUNDS`); dust → `DUST`; dryrun-under-dust → filled preview (dryrun precedes
  the accept-path dust); dryrun-under-funded → `INSUFFICIENT_FUNDS`
  (checkAcceptParams runs pre-dryrun); dryrun-no-session →
  `NO_SESSION`; bad-address + under-funded → `INSUFFICIENT_FUNDS` (address
  validation is deferred past checkAcceptParams).
- `api/parity_fee_test.go`: update `TestNodeBlockContext` — stub
  `[32]byte{0xab}` hash → display `…ab` → first 8 = `"00000000"` (8×`0x30`).
- `api/store_test.go`: `LockedUtxoInfo` includes `FeeUtxos`; terminal order
  releases.
- `api/handlers_test.go:792` (no-conn → `errNoSession`) must stay green.

## Corrections to `remediation-plan.md` B2 spec

- "`152 + len(fee) + 121·N ≥ 188`" → **156** (fixed body).
- "coins.Tx (v2, Sequence=0xffffffff)" → **v1** (C++ stock `CMutableTransaction`,
  no nTime; BLOCK conf `TxVersion=1`).
- "fee output 0.015 BLOCK to the hub payment addr" → **registry
  `PaymentAddress`** (`snode.getPaymentAddress()`, `:2196`); body `hubAddress`
  = `o.HubAddress`.
- File list expands: also `api/store.go`, `wallet/connector.go` (+
  `wallet/rpc.go`, `wallet/local.go`, fakes), `api/parity_fee_test.go`,
  `api/divergence_test.go`, `api/wallet_methods_test.go`.
- Add: `GetBalance` interface method (B2-owned seam, like B1's
  `PaymentAddress`); funds pre-check; dust; `NO_SESSION` block-context;
  `blockContext` hash-byte fix; `INVALID_ONCHAIN_HISTORY`.

## Verification

```bash
gofmt && go build ./... && go vet ./... && go test ./... && go test -race ./...
# from tools/: make parity && make canary
```

Then update `../register.md` (CRYPTO-F84 → FIXED) + the register. Commits:
logic / gofmt / docs separate.

## Done checklist (2026-08-12)

- [x] `go build` / `go vet` / `go test ./...` / `go test -race ./...` green.
- [x] `make parity` + `make canary` green on the final tree.
- [x] A1–A7 + per-token exclusion documented here.
- [x] `../register.md` CRYPTO-F84 row updated (A1–A7 + per-token split).
- [x] Merge `fix/wire-acceptingbody` → `main`.
- [ ] Optional add-on: real captured `AcceptingBody` golden in `tools/parity`.

## Decisions (settled — full C++ parity, no documented deviations)

- **D1:** `GetBalance` added to the `Connector` contract; the pre-check reads
  **only the BLOCK connector** (C++ `GetWallets()` = daemon wallets = the BLOCK
  wallet), reproduced exactly — including the xbridge-unit-vs-native-unit quirk.
- **D2:** `UsedCoins`/`FeeUtxos` runtime-only; locks derive from stored orders,
  released on terminal state (C++ in-memory locks die with the daemon —
  persistence would deviate).
- **D3:** block-context RPC failure → `NO_SESSION` (C++ `:2373`); Go's
  store-on-success makes the C++ unlock a no-op.
- **D4 — per-token lock exclusion:** the lock exclusion set is per-token, not
  store-wide, mirroring C++ `getAllLockedUtxos(token)`
  (`xbridgeapp.cpp:2827-2834`) / `m_utxosDict[token]` + `m_feeUtxos`:
  - `Order.UtxoCurrency` tags the chain an order's locked `Utxos` live on: the
    maker's `FromCurrency` (`dxMakeOrder`, a remote maker body) or the taker's
    funding `ToCurrency` (`TakeOrder`). Persisted in `persistedSwap`
    (`utxoCurrency`); pre-tag records fall back to a Role-derived rule (`'B'`
    taker → `ToCurrency`, else `FromCurrency`).
  - `Store.LockedUtxoInfoFor(ticker)` returns the global fee set (every order's
    `FeeUtxos` + every reservation's fee inputs) plus the coins locked on that
    ticker. Make/fund/balance paths use it so a check excludes only coins
    locked on the checked chain. `LockedUtxoInfo()` (the RPC display view)
    still reports the union.
  - `Store.reserved` splits each take's claim into `fee` (BLOCK, global) and
    `fund` (`fundCurrency`) key sets, mirroring C++ `lockFeeUtxos`/`lockCoins`;
    `ReserveForTake(key, feeKeys, fundKeys, fundCurrency)`.
  - `checkAcceptParams` balance excludes only `LockedUtxoInfoFor(currency)`
    (C++ `checkAmount` `getAllLockedUtxos(currency)`, `xbridgeapp.cpp:2574`);
    fee prep and funding exclusions read `LockedUtxoInfoFor(o.ToCurrency)`
    (C++ `:2247`/`:2270`).
  - Tests: `TestLockedUtxoInfoFor`, `TestStoreReserveForTake`.
- **D5 — same-order reservation gate:** `Store.ReserveForTake` returns a reason
  enum (`takeReserve`): an order with an in-flight reservation is refused
  `reserveOrderBusy` → `BAD_REQUEST` "not accepting, order already accepted"
  (C++ state gate `xbridgeapp.cpp:2122-2125`), checked BEFORE any utxo work.
  This closes the reservation-overwrite hole: two concurrent takes of the same
  order selecting disjoint keys could both pass the collision scan and the
  second would overwrite the first's `s.reserved[key]`, dropping the first
  take's keys out of `LockedUtxoInfo` before it submits. Scope is the in-flight
  window only (C++'s persistent `trAccepting` gate is a documented Go
  divergence): `ReleaseReserve` on submit clears the entry, so sequential
  re-takes of a settled order stay legal (`TestDxTakeOrderFullTake`). Loser
  mapping in the concurrency test accepts `errBadRequest` (same-order gate) or
  `errInsufficientFunds` (distinct-order key collision, or a same-order loser
  whose stale `LockedUtxoInfo` snapshot routes it to the collision scan).

## Zero-deviation audit follow-ons (2026-08-11, A1-A7)

Re-checked every gate of `TakeOrder` against the C++ writers during the CRYPTO-F84
closeout; seven genuine deviations were found and fixed. Labels A1-A7 avoid the
settled D1-D4 above.

- **A1 — `availableBalance()` wallet set** (was: Σ every conf connector; C++:
  Σ `GetWallets()` = the daemon's BLOCK wallet only, `xbridgeapp.h:798-806`,
  `wallet/wallet.h:67`). Fixed: BLOCK connector only; nil ⇒ 0 (empty-wallet
  semantics ⇒ `INSUFFICIENT_FUNDS_DX`). This changes
  `TestTakeOrderNoBlockConnector` from `INSUFFICIENT_FUNDS` (fee-prep) to
  `INSUFFICIENT_FUNDS_DX` (`:2161`).
- **A2 — `xBridgeValueFromAmount` round-up** (`utxo_select.go`): C++
  `a/COIN + 1.0/::COIN` (`xutil.cpp:223-227`); Go omitted the `+1e-8` term,
  losing a satoshi at the coin-scale boundary. Fixed; KAT added.
- **A3 — funding `ListUnspent` minconf** (was `cc.Confirmations`; C++
  `getUnspent`→`rpc::listUnspent` empty params = wallet default minconf 1,
  `xbridgewalletconnectorbtc.cpp:1604-1612`). Fixed: hardcode `ListUnspent(1)`
  (fee path already used 1).
- **A4 — missing `checkAcceptParams`** (`rpcxbridge.cpp:1204` →
  `checkAmount` `xbridgeapp.cpp:2561-2580`): the pre-dryrun balance gate did
  not exist in Go. Added `Node.checkAcceptParams(currency, fromSize,
  fromAddress)`; NO_SESSION / INSUFFICIENT_FUNDS, positioned before the
  self-trade check. Error messages carry C++'s exact args — bare
  `toCurrency` for NO_SESSION (`rpcxbridge.cpp:1263`), `fromAddress` for
  INSUFFICIENT_FUNDS (`:1267`).
- **A5 — fee order-info overflow** (was `errInsufficientFunds`; C++:
  `strInfo.size() > maxBytes` ⇒ `INVALID_ONCHAIN_HISTORY`, `xbridgeapp.cpp:
  2226-2228`). Fixed: `errOrderInfoOverflow` sentinel → `errInvalidOnchainHist`
  (1033). Also fixed a latent Go panic: `orderID[:157-len]` with `len > 157`
  sliced   out of range, where C++'s size_t `leftOver` underflows and
  `std::string::erase(pos > size)` throws `std::out_of_range` (terminating
  the daemon); Go deliberately keeps the full id and lets the size check
  report the overflow.
- **A6 — dryrun precedes dust** (was: dust checked before the dryrun branch;
  C++: dryrun branch `rpcxbridge.cpp:1227` is before `acceptXBridgeTransaction`
  and its dust checks `xbridgeapp.cpp:2147-2157`). Fixed: dryrun returns first;
  dryruns of under-dust orders now render a filled preview.
- **A7 — address validation precedence** (was: `decodeAddr` first; C++:
  `isValidAddress` `rpcxbridge.cpp:1219-1225` runs after the
  amount/checkAcceptParams/self-trade/connector gates). Fixed: `decodeAddr`
  moved after the connector checks. An invalid address on a
  `checkAcceptParams`-failing order now reports `INSUFFICIENT_FUNDS`, not
  `INVALID_ADDRESS`.
