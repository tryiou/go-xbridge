# XBridge C++ ↔ Go Port — Consolidated Comparative Audit

Mode: read-only comparative audit (findings phase). No code edits were made during
audit collection. This document consolidates 8 independent subagent audits.

C++ root: `blocknet_core/src/xbridge/`
Go root:  `go-xbridge/`

## Executive summary

The Go port is **byte-for-byte faithful** on the core wire contract (packet header, body
serialization, crypto/signatures, command enums). The only **S1 wire-breaking** defect is
in the Bitcoin `version` handshake envelope. Remaining findings are S2 (lenient address
decode) and S3 (config/RPC/param-rule thin-client divergences, mostly documented or
deliberate).

## Severity taxonomy

- **S1** — Wire-breaking: byte/field layout differs → packets rejected or silent decode corruption. MUST FIX.
- **S2** — Protocol-semantic: identical bytes but different state-machine meaning / ordering / validation. MUST FIX.
- **S3** — Behavioral deviation: conf-key handling, fee/dust math, RPC surface mismatch, wallet-RPC differences. FIX or document as deliberate thin-client choice.
- **S4** — Cosmetic/doc.

## Per-area verdicts

| Area | Verdict | Worst severity |
|---|---|---|
| SA-1 Wire packet header & transport | FAITHFUL except `version` addr timestamp | **S1** |
| SA-2 Command & TxCancelReason enums | FAITHFUL | — |
| SA-3 Packet body serialization | FAITHFUL (all live commands) | S2 (asymmetric stubs) |
| SA-4 Swap state machine & lifecycle | FAITHFUL except finished-expiry guard | S3 |
| SA-5 Config (xbridge.conf) contract | DIVERGENCE | S3 |
| SA-6 Fee & dust math | DIVERGENCE | S3 |
| SA-7 Crypto & signatures | FAITHFUL except base58check decode | S2 |
| SA-8 RPC/API surface | FAITHFUL except param-rule/wallet-RPC diffs | S3 |

---

## Ranked divergence list (fix recommendations grouped by severity)

### S1 — Wire-breaking (MUST FIX)

**S1-A. Bitcoin `version` message drops 4-byte `nTime` in each `addrYou`/`addrMe`**
- C++: `protocol.h:372–386` writes nTime(4)+services(8)+IP(16)+port(2) = **30 bytes/addr** on network streams for `nVersion >= 31402`.
- Go: `p2p/version.go:72–79` `marshalNetAddr` writes services(8)+IP(16)+port(2) = **26 bytes/addr**, omitting nTime. `UnmarshalVersion` (`:181–192`) also reads 26.
- Impact: Go-sent `version` is 8 bytes short; a stock Blocknet servicenode mis-aligns nonce/subver/startheight/relay/fxrouter → handshake rejection or silent mis-parse.
- Fix: add a 4-byte `nTime` (uint32 LE, e.g. `uint32(time.Now().Unix())`) at the start of `marshalNetAddr`/`unmarshalNetAddr`, making each address 30 bytes.

### S2 — Protocol-semantic

**S2-A. base58check address decode strictness differs**
- C++ `toXAddr` (`xbridgewalletconnectorbtc.cpp:1547–1555`) strips the version byte blindly — accepts any prefix.
- Go `DecodeAddress` (`coins/address.go:95–104`) hard-rejects mismatched prefix.
- Impact: For valid inputs both yield the same 20-byte id; divergence only on malformed/foreign-chain addresses (Go stricter). Not a byte-fidelity bug, but a behavioral divergence vs the authoritative C++ leniency.
- Fix/decision: document as deliberate hardening, or relax to match C++ if cross-chain prefix-blindness is required by the swap flow.

**S2-B. `IsExpiredByBlockNumber` finished-state guard (SA-4)**
- C++ `xbridgetransaction.cpp:288–311`: for finished states (gtNew && isFinished) falls through to the **block-height** check (`lastHeight - trBlockHeight > blocksTTL`).
- Go `swap/transaction.go:229–239`: uses the **time-based** TTL for finished states, not the block-height check.
- Impact: likely dead code in both (C++ only calls it on pending txs), but the contract differs. Low real-world risk.
- Fix: align Go to use block-height check for finished states, or confirm dead-code and document.

**S2-C. BIP143 / bech32 in Go have no live C++ writer** (deliberate-thin-client, unverified) — `coins/tx.go:397–461`, `coins/bech32.go`. BTC deposit flow is legacy P2SH in both; Go's segwit path is currently unreferenced. Flag for future segwit work.

### S3 — Behavioral / config / thin-client (FIX or document)

**S3-A. Config defaults: `JSONVersion` / `ContentType`**
- C++ `xbridgeapp.cpp:995–996` defaults empty → RPC request omits `jsonrpc` field and Content-Type header.
- Go `config/conf.go:274–275` defaults `"1.0"` and `"application/json"`.
- Fix: default to empty to match C++ (or rely on Go-only `OmitJSONVersion` key — `conf.go:276`, `wallet/conf.go:32` — which is itself a Go invention not in the C++ contract).

**S3-B. Fee formula `FeePerByte==0`**
- C++ `xbridgewallet.h:114` default 0 → fee 0.
- Go `api/handlers.go:1365–1368` substitutes **2 sat/vB** thin-client default.
- Decision: documented deliberate-thin-client. Operator should set `FeePerByte` explicitly.

**S3-C. Dust floor `DustAmount` conf tier**
- C++ never reads `DustAmount` from conf (`xbridgeexchange.cpp:145` overwritten by `init()` at `xbridgewalletconnectorbtc.cpp:1526`); dust = `0.546*relayFee*COIN` else `5460`.
- Go `api/handlers.go:1348–1350` inserts a `DustAmount` conf tier C++ lacks.
- Decision: document as deliberate thin-client substitution (for the real conf `DustAmount=0`, Go collapses to 5460, matching C++).

**S3-D. Display `+1/COIN` round-up**
- C++ `xBridgeValueFromAmount` (`util/xutil.cpp:223–227`) adds 1 sat round-up on display.
- Go `formatXAmount` (`api/response.go:237–241`) omits it.
- Impact: ≤1e-6 coin display mismatch on fractional amounts.

**S3-E. `dxSplitInputs` param-count rule**
- Go requires exactly 7 (`handlers.go:1148`); C++ accepts 3–7 (`rpcxbridge.cpp:3295`).
- Fix: relax to match, or document.

**S3-F. `dxGetOrderBook` max_orders**
- C++ doc says combined cap; Go caps per-side (`handlers.go:625,631`). Confirm C++ behaviour.

**S3-G. Wallet-RPC param payload diffs** (all S3):
- `listunspent`: Go sends `[minConf,9999999,nil,true]` (include_unsafe=true) vs C++ no params (`wallet/rpc.go:197` vs `xbridgewalletconnectorbtc.cpp:512`).
- `sendrawtransaction`: Go adds `false` (allowhighfees) (`wallet/rpc.go:258`).
- `getrawtransaction`: Go sends verbosity `0` (`wallet/rpc.go:342`).
- relay-fee probe order differs (`getinfo`↔`getnetworkinfo`).
- `signrawtransactionwithwallet` only in Go (drops legacy alias).
- Fix: align params to match C++ for strict parity, or accept as modern-wallet compatibility.

---

## Deliberate thin-client items (explicitly NOT bugs)
- `gettradingdata` alias removed (matches `dispatch.go:37`).
- `getnetworkinfo` Go-only command (BLOCK-DX version gate).
- `dxGetOrderHistory` / `dxGetTradingData` local-only data sources, schema-faithful.
- `LocalConnector` local-key signing (no C++ counterpart).
- `FeePerByte==0 → 2 sat/vB` default; `DustAmount` conf tier.
- `xbcServicesPing` / `xbcXChatMessage` typed bodies (C++ has no live writer).

## Open questions worth resolving before implementation phase
1. S1-A: confirm stock servicenode rejects a 26-byte-addr `version` (live capture).
2. SA-2: confirm go-xbridge never needs to *emit* ConfirmA/ConfirmB (hub-originated by design).
3. SA-4: trace `isExpiredByBlockNumber` call sites against finished txs — is the block-height branch reachable?
4. SA-6: does the live `xbridge.conf` set `FeePerByte` for BTC-like coins (gates S3-B impact)?
5. SA-8: confirm BLOCK-DX's exact `getnetworkinfo` field requirements.

## Bottom line
One real S1 fix needed (the `version` nTime omission). Everything else is faithful or a
low-risk, mostly-documented thin-client divergence.
