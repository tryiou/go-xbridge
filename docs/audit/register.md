# Audit register — canonical finding list

Single source of truth for every known C++↔Go divergence and its status. The
detail behind each row lives in `findings.md` (per-finding cards) and
`evidence/` (per-axis deep dives); the remediation todo list lives in
[`remediation-plan.md`](remediation-plan.md). This file only tracks
**what is wrong and where it stands** — nothing else.

## Numbering & status legend

- **Canonical IDs** — every finding has exactly one axis-prefixed ID:
  `RPC-F01..F59`, `WIRE-F57..F71`, `STATE-F71..F79`, `CRYPTO-F77..F97`,
  `CFG-F84..F91`, `CONC-F92..F102`, `INV-F97..F100`, `SEC-F01..F04`. The 2026
  full audit (`F01–F99`) supplied the core; prior audits' `F1–F27`, `S2/S3/S4`
  and `S1-A…D` findings were folded in — duplicates mapped onto the surviving
  row, orphans promoted to new IDs. The one-to-one mapping lives in the
  [ID-history appendix](#id-history-appendix) at the bottom.

Status values:

| Status | Meaning |
|---|---|
| `OPEN` | Divergence present; assigned to a remediation branch |
| `FIXED` | Remediated at HEAD; branch or test that closed it is named |
| `DOCUMENTED` | Deliberate divergence, decided (not a bug); reference where decided |
| `RE-DECIDE` | Prior register documented it deliberate; the new audit disagrees — re-open the decision |

Detail for every Current finding lives in [`findings.md`](findings.md)
(per-finding REF/CAND/IMPACT/FIX cards) and the per-axis deep dives in
[`evidence/`](evidence/); the register tables below carry only the status.

---

## Current register

### RPC axis (`RPC-F01`–`RPC-F59`) — evidence: `evidence/rpc.md`, `evidence/rpc-groups/`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| RPC-F01 | S2 | Error-channel policy: C++ *throws* envelope errors for param/type errors; Go returns business results | FIXED | B4 `fix/http-hardening` (strict parsers, envelope −1/−3, read-order, `limit` param; `TestStrictParamEnvelopeErrors`, `TestRpcTypeCheckErrors`) — no string coercion |
| RPC-F02 | S2 | `NO_SESSION` error `name` hardcoded `"dx"` instead of the method name | FIXED | B4 `fix/http-hardening` (`TestNoSessionNameIsMethodName`) |
| RPC-F03 | S2 | `dxGetOrders` array order random (Go map) vs id-ascending (C++ std::map) | FIXED | B7 `fix/rpc-surface` (`orderIDLess` LSB-first byte sort, C++ `std::map<uint256>` order — `TestDxGetOrdersSortedById`, `TestOrderIDLess`) |
| RPC-F04 | S3 | `dxGetOrders` 60 s filter boundary (µs-exact vs second-truncated) | FIXED | B7 `fix/rpc-surface` (second-truncated on creation time — `TestDxGetOrdersSixtySecondBoundary`) |
| RPC-F05 | S2 | Exactly-64-hex id gate vs C++ `uint256S` left-pad/truncate tolerance | FIXED | B7 `fix/rpc-surface` (tolerant `parseOrderIDS` — `TestParseOrderIDS`, `TestDxCancelOrderShortId`) |
| RPC-F06 | S3 | `dxGetOrder` not-found message renders id raw vs zero-padded | FIXED | B7 `fix/rpc-surface` (zero-padded 64-hex not-found id — `TestDxGetOrderNotFoundPadded`) |
| RPC-F07 | S3 | `dxCancelOrder` cancels *before* validating connectors (side-effect order) | FIXED | B7 `fix/rpc-surface` (connectors validated before cancel side effects — `TestDxCancelOrderSideEffectBeforeValidation`) |
| RPC-F08 | S3 | `dxCancelOrder` cancel-failure error codes/text set differs (incl. isLocal→1021) | FIXED | B7 `fix/rpc-surface` (C++ codes/texts incl. isLocal→1021 — `TestDxCancelOrderFromConnectorGate`, `TestDxCancelOrderNonLocal`, `TestDxCancelOrderNotFoundPadded`) |
| RPC-F09 | S2 | `dxMakeOrder` response field ORDER differs (updated/created swapped; addresses/block_id last) | FIXED | B7 `fix/rpc-surface` (flat Layout B make-order struct, C++ insertion order — `TestMakeOrderResponseLayoutB`) |
| RPC-F10 | S2 | `dxMakeOrder`/`dxMakePartialOrder` dryrun returns real id + extra fields vs C++ zero id | FIXED | B7 `fix/rpc-surface` (dryrun zero-id 14-field — `TestMakeDryrunResponse`) |
| RPC-F11 | S2 | `dxMakeOrder` non-partial `partial_*` values: literal `"0"` vs `"0.000000"` | FIXED | B7 `fix/rpc-surface` (Layout B `"0.000000"` literals — `TestMakeOrderResponseLayoutB`) |
| RPC-F12 | S3 | `dxMakeOrder` `NO_SERVICE_NODE` message includes the pair; C++ bare | FIXED | B7 `fix/rpc-surface` (bare `NO_SERVICE_NODE` — `TestMakeOrderResponseLayoutB`) |
| RPC-F13 | S2 | `dxMakePartialOrder` dust gate: 1e6-scale minFrom vs native 1e8-scale dust | FIXED | B7 `fix/rpc-surface` (native-scale dust gate — `TestMakePartialDustNativeScale`) |
| RPC-F14 | S2 | `dxTakeOrder` explicit amount `"0"` is a full take in Go, error 1025 in C++ | FIXED | B7 `fix/rpc-surface` (explicit 0 → 1025 — `TestDxTakeOrderFullTake`) |
| RPC-F15 | S3 | `dxTakeOrder` error texts/names leak (`INVALID_ADDRESS` text, `name` "dxMakeOrder") | FIXED | B7 `fix/rpc-surface` (C++ address text + `name` `"dxTakeOrder"` — `TestTakeOrderBadAddressMessage`, `TestTakeOrderNotFoundBare`) |
| RPC-F16 | S2 | `dxGetOrderHistory` sums the WRONG asset volume (taker vs from/maker) | FIXED | B7 `fix/rpc-surface` (from/maker volume side — `TestDxGetOrderHistoryInverse`) |
| RPC-F17 | S2 | `dxGetOrderHistory` encoding: shortest-roundtrip floats vs C++ fixed-8; raw ratio vs 1e-6-quantized price | FIXED | B7 `fix/rpc-surface` (fixed-8 OHLCV + 1e-6-quantized price; row time = bucket START per default `interval_timestamp=at_start` — `TestQuantizePrice`, `TestXFloat`, `TestDxGetOrderHistory*`) |
| RPC-F18 | S2 | `dxGetOrderHistory` validation missing (granularity whitelist, end≤start, limit) | FIXED | B7 `fix/rpc-surface` (granularity whitelist, end≤start, limit — `TestDxGetOrderHistoryValidations`, `TestDxGetOrderHistoryLimitTail`) |
| RPC-F19 | S2 | `dxGetOrderHistory` data source: fills store never written → always empty | DOCUMENTED | Tier-3 (session-local fills, `api.md`) |
| RPC-F20 | S3 | `dxGetOrderBook` price formula omits C++ +1/COIN bump | FIXED | B7 `fix/rpc-surface` (+1/COIN price bump — `TestDxGetOrderBookPriceBump`) |
| RPC-F21 | S3 | `dxGetOrderBook` equal-best-price tie-break nondeterministic | FIXED | B7 `fix/rpc-surface` (deterministic `orderIDLess` tie-break — `TestDxGetOrderBookTieBreak`) |
| RPC-F22 | S3 | `dxGetOrderBook` int-width (Go `int` vs C++ int64_t) | FIXED | B7 `fix/rpc-surface` (int64 width, `TestDxGetOrderBook*`) |
| RPC-F23 | S2 | `dxGetTokenBalances` `"Wallet"` key derivation/presence differ | DOCUMENTED | no synthesized `Wallet` key — thin client exposes the BLOCK connector balance under its ticker (deliberate, `B7-rpc.md`) |
| RPC-F24 | S3 | `dxGetTokenBalances` key order (Go map-sorted, Wallet last) | DOCUMENTED | C++ ticker order is race-dependent thread-completion order (non-conformable; same decision as F23) |
| RPC-F25 | S3 | `dxGetTokenBalances` precision (C++ per-UTXO double sum vs Go exact integer) | FIXED | B7 `fix/rpc-surface` (golden `TestDxGetTokenBalancesSum`; agreement to the 6th decimal) |
| RPC-F26 | S3 | `dxGetMyOrders` field order, param rejection, dedup, sort | FIXED | B7 `fix/rpc-surface` (`orderDetailResult` addresses at 3/6; `seen` dedup; µs sort — `TestDxGetMyOrdersDedupAndSort`, `TestDxGetMyOrdersFieldOrder`; arity already gated params) |
| RPC-F27 | S2 | `dxGetMyPartialOrderChain` chain membership differs (filters/sort) | FIXED | B7 `fix/rpc-surface` (ported `getPartialOrderChain` filter: local partial/partial-child in live+history; created-time sort; full-lineage walk as documented deviation from the C++ utxo-count child-walk — `TestDxPartialOrderChainDetailsAggregate`, `TestPartialOrderChainResolvesHistoryParent`) |
| RPC-F28 | S2 | `dxPartialOrderChainDetails` `p2sh_deposits` array length ≠ chain length | FIXED | B7 `fix/rpc-surface` (one entry per chain order incl. empty strings — `TestDxPartialOrderChainDetailsAggregate`) |
| RPC-F29 | S3 | `dxPartialOrderChainDetails` bad-id error text differs | FIXED | B7 `fix/rpc-surface` (`"bad order id"` — `TestDxPartialOrderChainDetailsBadId`) |
| RPC-F30 | S3 | `dxPartialOrderChainDetails` key order alphabetized (Go map) | FIXED | B7 `fix/rpc-surface` (ordered `partialChainDetailsResult` struct, C++ 2460-2480 — `TestDxPartialOrderChainDetailsKeyOrder`) |
| RPC-F31 | S3 | `dxGetLockedUtxos` amount encoding (default-float vs fixed-6/native) | FIXED | B7 `fix/rpc-surface` (`nativeAmountString`: C++ default-double prec 6 via `UtxoEntry::toString` — `TestDxGetLockedUtxosAmountDefaultDouble`, `TestDxLockedUtxoNativeAmount`) |
| RPC-F32 | S2 | `dxGetLockedUtxos` 1021 trigger too narrow in Go | FIXED | B7 `fix/rpc-surface` (1021 on no-reserved-utxos `getUtxoItems` miss + expired-state validity — `TestDxGetLockedUtxosNoReserved`) |
| RPC-F33 | S3 | `dxGetLockedUtxos` per-order key selection (status ordinal vs map membership) | FIXED | B7 `fix/rpc-surface` (pending/accepted map membership: `o.Mine || st >= accepting` → maker_and_taker — `TestDxGetLockedUtxosKeyByState`) |
| RPC-F34 | S3 | `dxGetLockedUtxos` id echo un-normalized | FIXED | B7 `fix/rpc-surface` (echo `orderIDString(parseOrderIDS(id))` — `TestDxGetLockedUtxosIdEchoNormalized`) |
| RPC-F35 | S2 | `dxFlushCancelledOrders` flushes only the cancelled ledger, not book/history | FIXED | B7 `fix/rpc-surface` (scans + erases cancelled from live book AND history, `xbridgeapp.cpp:1331-1354` — `TestFlushCancelledPrunesBookAndHistory`, `TestDxGetLockedAndFlush`). Residual: Go sets `Updated`=now at cancel (restarts the age clock), C++ leaves txtime at the last pre-cancel update — identical under age 0 |
| RPC-F36 | S3 | `dxFlushCancelledOrders` use_count/ordering/key order | FIXED | B7 `fix/rpc-surface` (per-map uint256 id ordering, ordered struct keys `ageMillis/now/durationMicrosec/flushedOrders` — `TestFlushCancelledPrunesBookAndHistory`; use_count = owning-map ref 1, Go has no shared_ptr — debug-only residual) |
| RPC-F37 | S2 | `gettradingdata` (lowercase) missing from Go dispatch | DOCUMENTED | B7 decision: deliberate removal — the lowercase command is a blocknetd-internal registration with a different schema + duplicated `to` key (rpcxbridge.cpp:3520, 2761-2770); `xbridged` surfaces only `dxGetTradingData`, callers get envelope `-32601` (`api.md:8`) |
| RPC-F38 | S2 | `dxGetTradingData` `fee_txid`/`nodepubkey` always `""` | DOCUMENTED | Tier-3 thin-client limit: the fields come from the on-chain BLOCK scan (RPC-F39) a thin client cannot replay; always `""` (`api.md` Tier 3) |
| RPC-F39 | S2 | `dxGetTradingData` data source: local fills vs on-chain scan | DOCUMENTED | Tier-3 (`api.md`) |
| RPC-F40 | S2 | `dxSplitInputs` requires amount/scriptPubKey; C++ needs only txid/vout | FIXED | B7 `fix/rpc-surface` (txid/vout-only entries resolved against the wallet's unspent by txid/vout; locked-utxo guard — `TestDxSplitInputsTxidVoutOnly`, `TestDxSplitInputsLockedUtxo`) |
| RPC-F41 | S2 | `dxSplit` fee formula differs (520·fpb per output + claw-back) | FIXED | B7 `fix/rpc-surface` (`feesPerUtxo = minTxFee1(1,3)+minTxFee2(1,1)` in XBridge units; real tx fee deducted from change, dust claw-back incl. C++ `outputCount -= 1` quirk — `TestDxSplitFeesPerUtxo`; parity oracle `main.cpp` + `TestValueMatchesCppSplit`) |
| RPC-F42 | S2 | `dxSplit` change destination differs (requested address vs fresh address) | FIXED | B7 `fix/rpc-surface` (change output uses the requested address script — `TestDxSplitChangeToRequestedAddress`) |
| RPC-F43 | S3 | `dxSplit` submit-failure code and error names | FIXED | B7 `fix/rpc-surface` (1004 BAD_REQUEST named after the actual method — `TestDxSplitSubmitFailure`) |
| RPC-F44 | S2 | `dxGetUtxos` amounts trimmed vs C++ fixed-8 | FIXED | B7 `fix/rpc-surface` (`coins.FormatAmountFixed` — fixed `c.Decimals` places, no trimming, matching C++ `xBridgeStringValueFromPrice(amount, conn->COIN)` (xutil.cpp:216-221) — `TestFormatAmountFixed`, `TestDxGetUtxos`) |
| RPC-F45 | S3 | `dxGetUtxos` listunspent failure code/text (1004 vs 1002) | FIXED | B7 `fix/rpc-surface` (1004 BAD_REQUEST named `dxGetUtxos` with C++ text `"failed to get unspent transaction outputs"` — `TestDxGetUtxosListUnspentError`) |
| RPC-F46 | S2 | `getnetworkinfo` shim diverges from real blocknetd (protocolversion, fees, subversion, fields) | FIXED | B7 `fix/rpc-surface` (protocolversion 70713, xbridgeprotocolversion 55, xrouterprotocolversion 50, subversion `/Blocknet:4.4.1/`, relayfee/incrementalfee 8-decimal strings `"0.00010000"`/`"0.00001000"`, networks `proxy_randomize_credentials:false` — `TestGetNetworkInfo`) |
| RPC-F47 | S2 | JSON-RPC HTTP status for parse/method-not-found (C++ 500/404 vs Go 200) | FIXED | B4 `fix/http-hardening` (status routing −32600→400, −32601→404, else 500; `TestServerEnvelopeStatusCodes`, `TestServerParseErrorStatus500`) |
| RPC-F48 | S3 | method-not-found message appends the method name | FIXED | B4 `fix/http-hardening` (bare `"Method not found"`; `TestServerInvalidRequest`, `TestServerEnvelopeStatusCodes`) |
| RPC-F49 | S3 | request body limit 4 MiB vs C++ 32 MiB | FIXED | B4 `fix/http-hardening` (32 MiB `rpcMaxBodyBytes`, non-envelope 413; `TestServerMaxBodyBytes`) |
| RPC-F50 | S2 | auth model: Go open-by-default vs C++ always-auth | FIXED | B4 `fix/http-hardening` (always-auth when creds configured, 401 empty body, 250 ms delay; `TestServerRPCAuth`, `TestServerRpcAuthMultiUser`, `TestServerRpcAuthDelay`) — residual: no auto-cookie file, loopback-open when unconfigured (documented) |
| RPC-F51 | S3 | batch / named params / -32600 unsupported | FIXED | B4 `fix/http-hardening` (batch supported, named → −8; `TestServerBatch`, `TestServerNamedParamsRejected`) |
| RPC-F52 | S3 | extra positional params accepted where C++ errors (business 1025) | FIXED | B4 `fix/http-hardening` (arity registry + C++ help-text 1025; `TestArityBusinessMethods`, `TestArityThrowMethods`) |
| RPC-F53 | S3 | `dxGetLocalTokens` returns unconnected/duplicate tickers | FIXED | B7 `fix/rpc-surface` (returns the loaded connector map keys, deduplicated — C++ `availableCurrencies()` (xbridgeapp.cpp:808-821) — `TestDxGetLocalTokensConnectedOnly`) |
| RPC-F54 | S3 | `dxGetNetworkTokens` membership: Go unions config; C++ pure SN service union | FIXED | B7 `fix/rpc-surface` (pure SN service union via `Registry.WalletServices()`; config `NetworkTokens`/`ExchangeWallets` no longer contribute — `TestDxTokenListsFromConf`, `TestDxGetNetworkTokensLive`) |
| RPC-F55 | S3 | `dxGetNewTokenAddress` error path returns `[]` in C++, business 1002 in Go | FIXED | B7 `fix/rpc-surface` (GetNewAddress failure -> empty array, C++ `getNewTokenAddress()` empty-string (rpcxbridge.cpp:186-190) — `TestDxGetNewTokenAddressGetNewAddrError`) |
| RPC-F56 | S3 | `dxLoadXBridgeConf` reload failure shape and side effects differ | FIXED | B7 `fix/rpc-surface` (reload failure returns `false` result, not a business error — C++ `uret(success)` (rpcxbridge.cpp:229-234) — `TestDxLoadConfFailureFalse`, `TestDxLoadConfHotReloadMissingPath`); coin/connector rebuild side effects in `Node.reloadConf`; non-local-order clearing (`clearNonLocalOrders`, :232-233) tracked under B10 `fix/config-parity` |
| RPC-F57 | S2 | `dxGetOrderBook` detail-4 nesting `[[…]]` vs flat | FIXED | B7 `fix/rpc-surface` (detail-4 `[[…]]` nesting — `TestDxGetOrderBookDetail`) |
| RPC-F58 | S2 | HTTP auth/timeout hardening missing | FIXED | B4 `fix/http-hardening` (`-rpcservertimeout` + `http.Server` read/write/header/idle timeouts) |
| RPC-F59 | S3 | `dxGetMyPartialOrderChain` unknown/malformed id handling | FIXED | B7 (bad-order-id) |

### WIRE axis (`WIRE-F57`–`WIRE-F71`) — evidence: `evidence/wire.md`, `evidence/wire_p1.md`, `evidence/wire_p2.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| WIRE-F57 | S2 | P2P max payload 67 MiB vs C++ 4,000,000 | FIXED | B5 `fix/wire-hardening` (`MaxPayloadSize`, conn cap test) |
| WIRE-F58 | S2 | Received magic never validated | FIXED | B5 `fix/wire-hardening` (`conn.readMessage`, `TestConnWrongMagicRejected`) |
| WIRE-F59 | S2 | No `MIN_PEER_PROTO_VERSION` gate | FIXED | B5 `fix/wire-hardening` (`MinPeerProtoVersion`, `TestConnVersionBelowMinimum`) |
| WIRE-F60 | S2 | `"staging"` network magic is actually C++ REGTEST | FIXED | B5 `fix/wire-hardening` (`RegtestMagic`, `-network regtest`) |
| WIRE-F61 | S2 | `snl` (SNLIST) responses ignored | FIXED | B5 `fix/wire-hardening` (raw accepted-ping echo, `TestPeerManagerSNListEcho`) |
| WIRE-F62 | S3 | `proto.Unmarshal` silently ignores trailing body bytes | FIXED | B5 `fix/wire-hardening` (`TestUnmarshalTrailingBytes`) |
| WIRE-F63 | S3 | Non-canonical CompactSize varint accepted | FIXED | B5 `fix/wire-hardening` (`readVarInt` canonicality, `TestReadVarIntCanonical`) |
| WIRE-F64 | S3 | Bad checksum disconnects instead of log-and-drop | FIXED | B5 `fix/wire-hardening` (`ErrChecksum` sentinel, `TestConnChecksumFrameDropped`) |
| WIRE-F65 | S3 | Go-only 1 MiB XBridge body cap (C++ has none) | DOCUMENTED | hardening limit (`protocol.md`) |
| WIRE-F66 | S3 | Command 4 has two C++ writers differing by trailing minFromAmount | DOCUMENTED | Go is correct; doc'd in `findings.md` |
| WIRE-F67 | S2 | Command 2 (xbcXChatMessage) body is speculative (no C++ writer) | FIXED | B5 `fix/wire-hardening` (type deleted; `DecodeBody` rejects) |
| WIRE-F68 | S2 | Command 50 (xbcServicesPing) body claim is unbacked | FIXED | B5 `fix/wire-hardening` (type deleted; `DecodeBody` rejects) |
| WIRE-F69 | S3 | getaddr policy and addr cap differ | FIXED | B5 `fix/wire-hardening` (outbound-ignore; 1000-cap, `TestPeerManagerAddrCapDropped`) |
| WIRE-F70 | S3 | Version handshake differences (deadline, SENDHEADERS/SENDCMPCT, pings) | FIXED | B5 `fix/wire-hardening` (60 s deadline; SENDHEADERS/SENDCMPCT + pings remain DOCUMENTED) |
| WIRE-F71 | S2 | Servicenode registration integrity: fields read-then-discarded; gates miss `isValid` subset | FIXED | B1 `fix/servicenode-registry` |

### STATE axis (`STATE-F71`–`STATE-F79`) — evidence: `evidence/state.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| STATE-F71 | S2 | OnHold/OnInit skip the C++ amount/identity/price verification | FIXED | B3 `fix/deposit-path`: `verifyHold` (C++ `processTransactionHold` :1404-1471) + `verifyInit` with the intended-OR order-detail check + state gate; `TestHoldInitVerification` |
| STATE-F72 | S2 | Expiry pruning never wired in Go (IsExpired has no production caller) | OPEN | B8 |
| STATE-F73 | S3 | TxCancelReason enum + text table not ported (incl. C++ bugs) | OPEN | B8 |
| STATE-F74 | S3 | `trRollbackFailed` never set (refund broadcast failure) | OPEN | B8 |
| STATE-F75 | S3 | No peer penalty/Misbehaving analogue | OPEN | B8 |
| STATE-F76 | S4 | Live handshake uses separate `clientState`, not ported `swap.State` | DOCUMENTED | internal choice (`findings.md`) |
| STATE-F77 | S2 | Post-completion handshake retransmit re-broadcasts deposit/claim | FIXED | state guards (`swap_guard_test.go`) |
| STATE-F78 | S2 | Handshake inbound packets re-verified against pinned hub key, no TOFU | FIXED | hub-key pinning (`swap.go`) |
| STATE-F79 | S3 | `tryJoinMatches` partial-order min-size guards unconfirmed | OPEN | B8 |

### CRYPTO axis (`CRYPTO-F77`–`CRYPTO-F97`) — evidence: `evidence/crypto.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| CRYPTO-F77 | S1 | BCH forkid `0x41` sighash missing in Go local signing | OPEN | B9 |
| CRYPTO-F78 | S2 | Deposit tx fee formula `minTxFee1(nIn,3)` vs Go `estimateFee(nIn,2)` | FIXED | B3 `fix/deposit-path`: `estimateFee(cc, nIn, 3)` at `api/swap.go` (`(192nIn+102)·FeePerByte`, C++ `xbridgesession.cpp:1994/:2526`); `TestEstimateFeeMatchesCppVsize` |
| CRYPTO-F79 | S3 | Fee fallback: C++ 0 vs Go 2 sat/vB when FeePerByte unset | DOCUMENTED | deliberate thin-client (`api.md` Tier-3) |
| CRYPTO-F80 | S3 | Go honors `DustAmount` conf key C++ never reads | DOCUMENTED | deliberate thin-client (`api.md` Tier-3) |
| CRYPTO-F81 | S3 | Address decoding strictness differs (base58check version byte, cashaddr) | DOCUMENTED | deliberate hardening (`api.md` Tier-3) |
| CRYPTO-F82 | S4 | RNG top-bit bias in Go private-key generation | OPEN | B9 |
| CRYPTO-F83 | S3 | Block-hash byte order assumption unverified end-to-end | OPEN | B9 |
| CRYPTO-F84 | S2 | `TakeOrder` emits `AcceptingBody` with empty fee/utxos (156 B < 188 B) | FIXED | B2 `fix/wire-acceptingbody` |
| CRYPTO-F85 | S2 | No `checkDepositTransaction` in the Connector contract | FIXED | B3 `fix/deposit-path`: `wallet.CheckDepositTransaction` (interface + RPCConnector 1:1 port of `xbridgewalletconnectorbtc.cpp:1981-2194` + LocalConnector `ErrNoChainSource`) wired into `OnCreateB`/`OnConfirmA` with tri-state (wait→no reply / bad→Cancel / good→record); `wallet/rpc_test.go` goldens + `TestCreateBBadDepositCancels`/`TestCreateBWaitsOnNotReadyDeposit` |
| CRYPTO-F86 | S2 | `buildDeposit` broadcasts before building the refund | FIXED | B3 `fix/deposit-path`: sign → local txid → refund → broadcast; `TestDepositNotBroadcastWhenRefundFails` |
| CRYPTO-F87 | S2 | Deposit re-runs `ListUnspent` instead of `xtx->usedCoins` | FIXED | B3 `fix/deposit-path`: `Order.UsedCoins` recorded at make/take, `swapCtx.funding` snapshot consumed by `buildDeposit`; `TestDepositSpendsUsedCoins` |
| CRYPTO-F88 | S3 | Segwit/BIP143 signing dead code; bech32 re-encoded legacy | OPEN | B9 |
| CRYPTO-F89 | S3 | Coin-family misclassification / missing connectors (DEVAULT, DCR, PART, BTG) | OPEN | B9 |
| CRYPTO-F90 | S3 | Refund/payment payout model (fee2 margin, oOverpayment) | FIXED | B3 `fix/deposit-path`: claim spends the validated deposit (exact `P2SHNative` at `DepositVout`), refund pays full nominal (fee2 implicit); `OBinTxVout/OBinTxP2SHAmount/OOverpayment` persisted; `TestRedeemCounterpartyPayout` |
| CRYPTO-F91 | S3 | `signrawtransaction` param payload ("ALL" in privkeys slot) | OPEN | B9 |
| CRYPTO-F92 | S3 | `secretFromScriptSig` requires 33-byte push | OPEN | B9 |
| CRYPTO-F93 | S3 | UTXO ownership-proof challenge stream format | FIXED | `TestWholeCoinOstreamMatchesCppStream` |
| CRYPTO-F94 | S3 | Deposit inputs `SEQUENCE_FINAL`; refund spend `SEQUENCE_FINAL-1` | FIXED | `checkDepositTransaction` parity |
| CRYPTO-F95 | S3 | Deposit locks `Amount + fee2` (`minTxFee2(1,1)`); change after fee+fee2 | FIXED | `TestDepositLocksAmountPlusFee2` |
| CRYPTO-F96 | S3 | `nTime` committed in sighash on `TxWithTimeField` coins | FIXED | `TestHashForSigningWithTimeField` |
| CRYPTO-F97 | S1 | Deposit path mixed XBridge 1e6 and native base units (locked 100× too little for COIN≠1e6; BLOCK masked it) | FIXED | B3 `fix/deposit-path` (promoted from planning): `fromXBridgeAmt` at the `api/swap.go` boundary; `TestDepositNativeScale` |

### CONFIG axis (`CFG-F84`–`CFG-F91`) — evidence: `evidence/config.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| CFG-F84 | S2 | `[Rpc]` section in xbridge.conf aborts xbridged at startup | OPEN | B10 |
| CFG-F85 | S2 | Wallet admission validation gates absent (locktime/confirmation drift) | OPEN | B10 |
| CFG-F86 | S2 | Missing conf: C++ creates template and runs; Go exits(1) | OPEN | B10 |
| CFG-F87 | S2 | Hot-reload semantics differ (ExchangeWallets keying, gates, order clearing) | OPEN | B10 |
| CFG-F88 | S3 | ExchangeWallets parsing differs (`,` `;` `:` + validation) | OPEN | B10 |
| CFG-F89 | S3 | Case-insensitive keys in Go vs case-sensitive C++ | OPEN | B10 |
| CFG-F90 | S3 | Missing CLI flags / flag differences (-enableexchange, -dxnowallets, version case) | OPEN | B10 |
| CFG-F91 | S3 | Go-only conf keys and ignored C++ keys (MinimumAmount, CashAddrPrefix, CreateTxMethod) | OPEN | B10 |

### CONCURRENCY axis (`CONC-F92`–`CONC-F102`) — evidence: `evidence/concurrency.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| CONC-F92 | S2 | Engine goroutine can block on socket write / fsync, stalling packet processing + RPC | OPEN | B11 |
| CONC-F93 | S3 | Discovery peer goroutines never joined; Dedupe sweeper leaks without Flush | OPEN | B11 |
| CONC-F94 | S3 | Conf reload mid-swap-task hazard (untested) | OPEN | B11 |
| CONC-F95 | S3 | Go hides C++'s transient "accepting" window | DOCUMENTED | Go-stricter; `findings.md` |
| CONC-F96 | S4 | Go runs swap wallet I/O + RPC concurrently where C++ serializes | DOCUMENTED | model note; `findings.md` |
| CONC-F97 | S2 | `SwapSession` fields single-owner, engine-only | FIXED | race tests |
| CONC-F98 | S2 | Coin registry `atomic.Pointer` hot-reload safe | FIXED | race tests |
| CONC-F99 | S2 | Unbounded growth bounded (pruneSessions, trimOldest, fills/history caps) | FIXED | `TestPruneSessions*` |
| CONC-F100 | S2 | Book/session maps never held across wallet I/O | FIXED | lock discipline |
| CONC-F101 | S2 | `dxMakeOrder` returned the store's LIVE `*Order` (data race) | FIXED | `TestMakeOrderReturnsStoreCopy` |
| CONC-F102 | S2 | Force-refund double-broadcast window | FIXED | `TestForceRefundTakesSweepGuard` |

### INVENTORY / DOC axis (`INV-F97`–`INV-F100`) — evidence: `evidence/inventory.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| INV-F97 | S4 | Unported C++ internal helpers (not dApp-facing) | DOCUMENTED | gap list in `evidence/inventory.md` |
| INV-F98 | S4 | `docs/protocol.md` says order `Created` is unix seconds; wire carries µs | OPEN | doc fix |
| INV-F99 | S4 | Stale C++ header-comment enums (commands 11/12/13/18/20/24) | DOCUMENTED | C++ side; writers authoritative |
| INV-F100 | S4 | Vestigial `Server.verify`, `coins.MustGet`, unreferenced `swap`, LocalConnector sign/verify | FIXED | documented |

### SECURITY axis (`SEC-F01`–`SEC-F04`) — security/robustness findings outside the RPC/wire/config axes

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| SEC-F01 | S2 | RPC binds to loopback by default (auth only when both creds set) | FIXED | loopback bind |
| SEC-F02 | S3 | Inbound order UTXO ownership proofs never verified before booking | OPEN | B8 |
| SEC-F03 | S2 | HTLC ELSE branch + CreateB-derived taker deposit composition sound; no standalone code | FIXED | B3 `fix/deposit-path` (composite): validated-deposit refusal kills the theft end-to-end — `TestSecF03CompositeRefusal` (taker refuses a bad A-deposit → Cancel + no B deposit; maker refuses a bad B-deposit at ConfirmA → Cancel + refund rollback). Attacker model corrected: the hostile outcome is **theft**, not recoverable lockup. |
| SEC-F04 | S2 | Plaintext secrets + debug-log leakage | FIXED | B6 `fix/secrets-hygiene`: `-persistsecrets` gate (default ON = C++ orders.dat parity; OFF zeroes `PrivKey`/`Secret`/`RefundHex` on write); refund/claim hex + RPC bodies dropped from logs; corrupt swap file logs at Error like C++ `loadOrders`. Tests: `TestPersistSecretsOptOut`, `TestCorruptSwapFileContinuesLikeCpp`. |

---

## ID-history appendix

Every prior-audit ID (`F1–F27`, `S2/S3/S4` series, `S1-A…D`) resolves to
exactly one canonical ID. This appendix exists for archaeology; the live
namespace is the Current register above.

| Legacy | Canonical | Notes |
|---|---|---|
| F1 | SEC-F01 | RPC loopback bind — FIXED |
| F3 | CONC-F97 | `SwapSession` single-owner — FIXED |
| F4 | CONC-F98 | coin registry atomic reload — FIXED |
| F5/F9 | CONC-F99 | bounded growth — FIXED |
| F6 | CONC-F100 | no wallet I/O under lock — FIXED |
| F7 | SEC-F02 | inbound UTXO proofs unverified — OPEN |
| F8 | CRYPTO-F88 | segwit dead code — OPEN |
| F10 | RPC-F49 | 4 MiB body cap — FIXED (B4 `fix/http-hardening`: 32 MiB `rpcMaxBodyBytes`, non-envelope 413) |
| F11–F14 | INV-F100 | vestigial helpers — FIXED |
| F15 | CONC-F101 | live `*Order` race — FIXED |
| F16 | STATE-F77 | post-completion retransmit — FIXED |
| F17 | CONC-F92 | blocking I/O on engine goroutine — OPEN |
| F18 | CONC-F102 | force-refund double-broadcast — FIXED |
| F19 | CRYPTO-F84 | AcceptingBody empty fee/utxos — FIXED (B2) |
| F20 | WIRE-F71 | registration integrity — FIXED (B1) |
| F21 | CRYPTO-F85 | `checkDepositTransaction` absent — FIXED (B3 `fix/deposit-path`) |
| F22 | SEC-F03 | taker-trust composite — FIXED (B3 `fix/deposit-path`) |
| F23 | CRYPTO-F86 | `buildDeposit` broadcast order — FIXED (B3 `fix/deposit-path`) |
| F24 | RPC-F58 | HTTP auth/timeout hardening — FIXED (B4 `fix/http-hardening`) |
| F25 | WIRE-F57/F63 | P2P addr/varint DoS — FIXED (B5 `fix/wire-hardening`) |
| F26 | SEC-F04 | plaintext secrets — FIXED (B6 `fix/secrets-hygiene`) |
| F27 | CRYPTO-F87 | `usedCoins` vs `ListUnspent` — FIXED (B3 `fix/deposit-path`) |
| S1-A | CRYPTO-F93 | ownership-proof challenge stream — FIXED |
| S1-B | CRYPTO-F94 | deposit `SEQUENCE_FINAL` — FIXED |
| S1-C | CRYPTO-F95 | deposit locks `Amount + fee2` — FIXED |
| S1-D | CRYPTO-F96 | `nTime` sighash on time-field coins — FIXED |
| S2-A | RPC-F57 | order-book detail-4 nesting — FIXED (B7) |
| S2-B | RPC-F59 | partial-chain unknown/malformed id — FIXED |
| S2-C | RPC-F40 | `dxSplitInputs` utxo schema — FIXED (B7) |
| S2-D | STATE-F72 | expiry sweep unwired — OPEN (B8) |
| S2-E | STATE-F78 | hub-key pinning, no TOFU — FIXED |
| S2-H | CRYPTO-F81 | base58check strictness — DOCUMENTED |
| S2-I | CRYPTO-F77 | BCH forkid sighash — OPEN (B9) |
| S2-J | CRYPTO-F89 | coin-family misclassification — OPEN (B9) |
| S3-A | RPC-F11 | partial fields `"0"` literal — FIXED (B7) |
| S3-B | RPC-F44 | `dxGetUtxos` amounts trimmed — FIXED (B7) |
| S3-C | RPC-F31 | locked-utxos amount format — FIXED (B7) |
| S3-D | RPC-F28 | `p2sh_deposits` alignment — FIXED (B7) |
| S3-E | RPC-F55 | new-token-address `[]` vs error — FIXED (B7) |
| S3-F | CRYPTO-F79/F80 | fee/dust thin-client substitutions — DOCUMENTED |
| S3-G | CRYPTO-F90 | payout model (fee2 margin) — FIXED (B3 `fix/deposit-path`) |
| S3-H | CRYPTO-F91 | `signrawtransaction` payload — OPEN (B9) |
| S3-I | CRYPTO-F92 | `secretFromScriptSig` 33-byte push — OPEN (B9) |
| S3-J | STATE-F79 | `tryJoinMatches` min-size guards — OPEN (B8) |
| S4 | RPC-F24/F30/F36, WIRE-F57 | key order, +1/COIN, help text, 64 MiB cap — DOCUMENTED (RPC-F52 leniency FIXED on B4) |

---

## How to maintain

- **Open a new divergence?** Add a Current-register row with the next free
  axis-prefixed ID; add its card to `findings.md` and its axis deep dive to
  `evidence/`.
- **Fix one?** Move the row's status to `FIXED` and name the branch/test that
  closed it, in the same branch that fixes the code (remediation-plan §done
  criteria step 5).
- **Decide it's deliberate?** Set `DOCUMENTED` and name where the decision lives
  (`api.md` Tier-3 / deliberate list).
- Never duplicate a finding's *status* in `findings.md` or `evidence/` — those
  files describe the divergence, this file tracks its disposition.

## Remediated at HEAD (detail)

The following prior findings are fixed and `-race`/golden-vector covered. Their
row status above is `FIXED`; this section records the closure detail so the
regression tests are locatable:

- **CRYPTO-F93.** UTXO ownership-proof challenge = C++ `UtxoEntry::toString()`:
  whole-coin `listunspent` `"value"` double streamed with
  `strconv.FormatFloat(v, 'g', 6, 64)`; golden vectors from real g++ output in
  `TestWholeCoinOstreamMatchesCppStream`.
- **CRYPTO-F94.** Deposit inputs `SEQUENCE_FINAL` (0xffffffff); refund spend keeps
  `SEQUENCE_FINAL-1` (C++ `checkDepositTransaction` hard-rejects non-final).
- **CRYPTO-F95.** Deposit locks `Amount + fee2` (`fee2 = minTxFee2(1,1)`), satisfying
  C++ `depositP2SHAmount >= amount + 0.95*fee2`; change = total − Amount − fee − fee2.
- **CRYPTO-F96.** `nTime` committed in the sighash on `TxWithTimeField` coins (4-byte
  LE `TxTime` after `nVersion` in `HashForSigning`); golden digests from a C++
  oracle in `TestHashForSigningWithTimeField`.
- **RPC-F59.** `dxGetMyPartialOrderChain` unknown id → `[]`, malformed id →
  `bad order id` (`api/handlers.go:863-865,858-861`).
- **STATE-F78.** Handshake inbound packets (Hold/Init/CreateA/B/ConfirmA/B/Finished)
  re-verified against the pinned hub key + registry membership — no TOFU; a
  forged `Finished` is dropped before `OnFinished`; non-registered hub →
  `NO_SERVICE_NODE`.
- **SEC-F01.** RPC binds loopback by default; Basic auth when both `-rpcuser` +
  `-rpcpassword` set (constant-time compare); non-loopback bind without auth
  warns.
- **CONC-F97/F98.** `SwapSession` single-owner (engine-only) +
  `atomic.Pointer[map]` coin registry; race-covered by
  `TestConcurrentRefundSweepAndDepositTask`, `TestConcurrentInitFromConfGet`.
- **CONC-F99.** Unbounded growth bounded: `pruneSessions` + `trimOldest` (1000
  each); `TestPruneSessionsRemovesTerminal`, `TestStoreHistoryBounded`.
- **CONC-F101.** MakeOrder returns a store snapshot copy, not the live pointer;
  `TestMakeOrderReturnsStoreCopy`.
- **STATE-F77.** Post-completion retransmit guards (`state >= csCreatedA/B`,
  `>= csConfirmedA/B`); `*StateGuard*` tests + `TestCreateAStateGuard*`.
- **CONC-F102.** Force-refund takes the `pendingRefunds` sweep guard;
  `TestForceRefundTakesSweepGuard`.
- **CRYPTO-F84.** AcceptingBody funded — B2 `fix/wire-acceptingbody`
  (atomic per-order reservation `Store.ReserveForTake`, p2pkh-25 funding filter,
  same-order `BAD_REQUEST` gate, A1–A7 gate closeout, per-token lock exclusion
  D4). Tests: `TestStoreReserveForTake`, `TestConcurrentTakeOrder*`,
  `TestTakeOrderFundingRejectsNonP2PKH`, `TestTakeOrderEmptyBlockWallet`,
  `TestLockedUtxoInfoFor` + `make parity`.
- **WIRE-F71.** Servicenode registration integrity — B1
  `fix/servicenode-registry` (retain + gate registration fields, `CreateSigHash`
  golden, `PaymentAddress()`); `TestParseServiceNode*`, `TestCreateSigHashGolden`,
  `TestAddRegistrationRejectMatrix`, `TestPaymentAddress*`.
