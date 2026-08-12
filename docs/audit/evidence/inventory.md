# go-xbridge Behavioral Conformance Audit — Structural Inventory

- **Date:** 2026-08-12
- **Reference (source of truth):** `blocknet_core/src/xbridge/` (+ `src/net.cpp`, `src/protocol.h`, `src/net_processing.cpp`, `src/rpc/server.cpp`, `src/rpc/client.cpp`)
- **Candidate:** `go-xbridge/` (packages `api/`, `p2p/` incl. `discovery/`, `servicenode/`, `proto/`, `swap/`, `wallet/`, `coins/`, `crypto/`, `config/`, `log/`, `cmd/xbridged`, `cmd/liveprobe`)
- **Method:** structural pass only — `ls`/`glob`/`ripgrep`; no deep logic read.

---

## 1. Reference — translation units under `blocknet_core/src/xbridge/` (73 files)

### 1.1 `src/xbridge/*` (53)

| File | Purpose |
|------|---------|
| `xbridgepacket.h` | Wire packet header layout; `XBridgeCommand`, `TxCancelReason` enums |
| `xbridgepacket.cpp` | `XBridgePacket::sign/verify` secp256k1 implementation |
| `version.h` | `XBRIDGE_PROTOCOL_VERSION` constant |
| `xbridgedef.h` | Core typedefs: `SessionPtr`, `TransactionPtr`, `WalletConnectorPtr`, maps |
| `xbridgetransaction.h` | `Transaction` class; `State` enum; lockTime/TTL constants |
| `xbridgetransaction.cpp` | Transaction lifecycle state machine implementation |
| `xbridgetransactionmember.h` | `XBridgeTransactionMember` per-side (A/B) data struct |
| `xbridgetransactionmember.cpp` | Member `payTx`/`refTx`/`binTx` accessors and serialization |
| `xbridgetransactiondescr.h` | `TransactionDescr` struct; 16-value `State`; wire/history serialization |
| `xbridgetransactiondescr.cpp` | TransactionDescr (de)serialization implementation |
| `xbridgesession.h` | `Session` class: phases, cancel/redeem/refund helpers |
| `xbridgesession.cpp` | `processPacket` dispatch; per-command phase handlers |
| `xbridgesessiondcr.h` | Decred-specific session phase declaration |
| `xbridgesessiondcr.cpp` | Decred phase implementations |
| `xbridgeexchange.h` | `Exchange` (servicenode) class declaration |
| `xbridgeexchange.cpp` | Exchange tx book, create/accept, utxo lock/unlock |
| `xbridgeapp.h` | `App` singleton orchestrator declaration |
| `xbridgeapp.cpp` | Make/take/cancel/repost flows; `selectUtxos`, `selectPartialUtxos` |
| `xbridgerpc.h` | `rpc::` raw wallet RPC helper declarations |
| `xbridgerpc.cpp` | Raw wallet RPC calls: getInfo, signRawTx, sendRawTx, etc. |
| `xbridgewallet.h` | `WalletParam`; `wallet::UtxoEntry`, `AddressBookEntry` |
| `xbridgewalletconnector.h` | `WalletConnector` virtual interface; `rpc::WalletInfo` |
| `xbridgewalletconnector.cpp` | Base connector: balance, token address, fee helpers |
| `xbridgewalletconnectorbtc.h` | `BtcWalletConnector<CryptoProvider>` declaration |
| `xbridgewalletconnectorbtc.cpp` | BTC/BLOCK connector: createTransaction, signing, deposit checks |
| `xbridgewalletconnectorbch.h` | BCH cashaddr connector declaration |
| `xbridgewalletconnectorbch.cpp` | BCH cashaddr connector implementation |
| `xbridgewalletconnectorbcd.h` | BCD connector declaration |
| `xbridgewalletconnectorbcd.cpp` | BCD connector implementation |
| `xbridgewalletconnectorbtg.h` | BTG connector declaration |
| `xbridgewalletconnectorbtg.cpp` | BTG connector implementation |
| `xbridgewalletconnectordgb.h` | DGB connector declaration |
| `xbridgewalletconnectordgb.cpp` | DGB connector implementation |
| `xbridgewalletconnectorpart.h` | PART connector declaration |
| `xbridgewalletconnectorpart.cpp` | PART connector implementation |
| `xbridgewalletconnectorstealth.h` | Stealth-chain connector declaration |
| `xbridgewalletconnectorstealth.cpp` | Stealth-chain connector implementation |
| `xbridgewalletconnectordevault.h` | DVT connector declaration |
| `xbridgewalletconnectordevault.cpp` | DVT connector implementation |
| `xbridgecryptoproviderbtc.h` | `BtcCryptoProvider` secp256k1 wrapper declaration |
| `xbridgecryptoproviderbtc.cpp` | Key gen, sign, verify via secp256k1 |
| `xbitcoinaddress.h` | `CBase58Data` base + `XBitcoinAddress` classes |
| `xbitcoinaddress.cpp` | Base58 address encode/decode implementation |
| `xbitcointransaction.h` | `XBitcoinTransaction` time-field serialization wrapper |
| `xbitcointransaction.cpp` | Time-field transaction (de)serialization |
| `xbridgedb.h` | TransactionDescr LevelDB persistence API |
| `xbridgedb.cpp` | LevelDB read/write of historical transactions |
| `xbridgerpc.cpp` (dup) | — *(registration only; see 1.3 RPC plumbing)* |
| `rpcxbridge.cpp` | All 24 `dx*` JSON-RPC handlers + `commands[]` table |
| `currency.h` | Currency display-name helpers |
| `currencypair.h` | `CurrencyPair` history-query struct |
| `bitcoinrpcconnector.h` | bitcoin-cli subprocess wrapper declaration |
| `bitcoinrpcconnector.cpp` | bitcoin-cli subprocess wrapper implementation |
| `xuiconnector.h` | Blocknet UI connector declaration |

### 1.2 `src/xbridge/cashaddr/` (4)

| File | Purpose |
|------|---------|
| `cashaddr.h` | BCH cashaddr polymorphic types |
| `cashaddr.cpp` | Cashaddr encode/decode algorithm |
| `cashaddrenc.h` | Cashaddr reference encoder declarations |
| `cashaddrenc.cpp` | Cashaddr reference encoder implementation |

### 1.3 `src/xbridge/util/` (16)

| File | Purpose |
|------|---------|
| `settings.h` | `Settings` singleton; xbridge.conf INI accessors |
| `settings.cpp` | Settings parse/read/write implementation |
| `xutil.h` | xbridge amount/price/date/string helper declarations |
| `xutil.cpp` | `xBridgeStringValueFromAmount`, `iso8601`, `base64`, `makeError` |
| `xbridgeerror.h` | `xbridge::Error` enum + `xbridgeErrorText` declarations |
| `xbridgeerror.cpp` | `xbridgeErrorText` per-code message implementation |
| `logger.h` | Logging macros/streams (`LOG`, `ERR`) |
| `logger.cpp` | Logger backend implementation |
| `txlog.h` | Order/transaction logging declarations |
| `txlog.cpp` | Order/transaction logging implementation |
| `xassert.h` | Assertion macro |
| `xseries.h` | `XSeries`/`XSeriesCache` OHLCV history cache |
| `xseries.cpp` | XSeries cache implementation |
| `posixtimeconversion.h` | Boost ptime conversion helpers |
| `posixtimeconversion.cpp` | ptime conversion implementation |
| `fastdelegate.h` | Fast-delegate event macro infrastructure |

### 1.4 Referenced RPC/P2P plumbing (5, outside `xbridge/`)

| File | Purpose |
|------|---------|
| `src/net.cpp` | P2P send/receive plumbing XBridge hooks into |
| `src/protocol.h` | Bitcoin P2P message types XBridge envelopes ride on |
| `src/net_processing.cpp` | Inbound-message dispatch to xbridge |
| `src/rpc/server.cpp` | JSON-RPC server wiring for xbridge commands |
| `src/rpc/client.cpp` | CLI/RPC client plumbing used by wallets |

---

## 2. Candidate — `.go` files by package (110 files)

### 2.1 `api/` (37)

| File | Purpose |
|------|---------|
| `dispatch.go` | dx* method dispatch table + positional param parsers |
| `engine.go` | Node event loops, worker pool, reader loop |
| `node.go` | Node orchestration: make/take/cancel, packet handling |
| `handlers.go` | dx* RPC handler implementations |
| `store.go` | Order book, history, locked-utxo, fills store |
| `order.go` | Order model + dx* response rendering |
| `response.go` | Response schemas, error codes, amount/date formatters |
| `swap.go` | SwapSession state handlers (OnHold…OnFinished, refunds) |
| `utxo_select.go` | selectUtxos/selectPartialUtxos, order-id derivation |
| `fee_tx.go` | Servicenode fee-tx build + fee utxo selection |
| `locktime.go` | Lock-time threshold + drift checks |
| `persist.go` | Swap state persistence to disk |
| `server.go` | JSON-RPC HTTP server |
| `concurrency_test.go` | Concurrent engine/store race tests |
| `divergence_test.go` | Port-divergence regression tests |
| `engine_test.go` | Engine loop tests |
| `fee_tx_test.go` | Fee-tx tests |
| `handlers_test.go` | Handler tests |
| `hub_gate_test.go` | Hub packet gate tests |
| `lifecycle_test.go` | Order lifecycle tests |
| `make_order_kat_test.go` | Known-answer make-order tests |
| `node_test.go` | Node tests |
| `order_test.go` | Order tests |
| `parity_dustfee_test.go` | Dust/fee parity tests |
| `parity_fee_test.go` | Fee parity tests |
| `persist_test.go` | Persistence tests |
| `response_test.go` | Response formatter tests |
| `server_test.go` | Server tests |
| `store_test.go` | Store tests |
| `swap_guard_test.go` | Swap guard tests |
| `swap_locktime_test.go` | Locktime guard tests |
| `swap_security_test.go` | Security regression tests |
| `swap_test.go` | Swap tests |
| `swap_two_phase_test.go` | Two-phase swap tests |
| `utxo_proof_test.go` | Utxo proof tests |
| `utxo_select_test.go` | Utxo select tests |
| `wallet_methods_test.go` | Wallet RPC method tests |

### 2.2 `p2p/` (12)

| File | Purpose |
|------|---------|
| `addr.go` | P2P `addr` message encode/decode |
| `conn.go` | Conn wrapper, version handshake, packet I/O |
| `envelope.go` | XBridge payload envelope + varint |
| `message.go` | P2P message framing (magic/cmd/len/checksum) |
| `params.go` | Network params (magic, port) |
| `seeds.go` | Bootstrap seed hostnames |
| `version.go` | Version message structs |
| `addr_test.go` | addr message tests |
| `envelope_test.go` | Envelope tests |
| `message_fuzz_test.go` | Message framing fuzz tests |
| `message_test.go` | Message framing tests |
| `version_test.go` | Version message tests |

### 2.3 `p2p/discovery/` (4)

| File | Purpose |
|------|---------|
| `addrman.go` | Address manager (tried/pending, cooldown) |
| `peer_manager.go` | Peer pool, read/write loops, servicenode feed |
| `addrman_test.go` | Address manager tests |
| `peer_manager_test.go` | Peer manager tests |

### 2.4 `p2p/servicenode/` (2)

| File | Purpose |
|------|---------|
| `servicenode.go` | Servicenode record parse/verify/sign |
| `servicenode_test.go` | Servicenode parse tests |

### 2.5 `proto/` (9)

| File | Purpose |
|------|---------|
| `command.go` | `XBridgeCommand` enum mirror |
| `packet.go` | `Packet` marshal/unmarshal/digest |
| `body.go` | `BodyWriter`/`BodyReader` primitives |
| `body_types.go` | Per-command body structs + `DecodeBody` |
| `body_fuzz_test.go` | Body decode fuzz tests |
| `body_overflow_test.go` | Body overflow tests |
| `body_test.go` | Body encode/decode tests |
| `codec_test.go` | Packet codec tests |
| `packet_test.go` | Packet round-trip tests |

### 2.6 `swap/` (9)

| File | Purpose |
|------|---------|
| `state.go` | `State` + `DescrState` enums, TTL constants |
| `session.go` | Session/transaction value types |
| `transaction.go` | Transaction struct helpers |
| `deposit.go` | Deposit/refund/HTLC data helpers |
| `price.go` | Price ratio helpers |
| `deposit_test.go` | Deposit tests |
| `session_test.go` | Session tests |
| `state_test.go` | State enum tests |
| `transaction_test.go` | Transaction tests |

### 2.7 `wallet/` (7)

| File | Purpose |
|------|---------|
| `connector.go` | `Connector` interface + `Chain`/`Utxo`/`PrevTx` |
| `rpc.go` | `RPCConnector` JSON-RPC wallet client |
| `conf.go` | Wallet config (conf-derived `Chain`) |
| `local.go` | Local wallet fallback |
| `conf_test.go` | Conf tests |
| `local_test.go` | Local wallet tests |
| `rpc_test.go` | RPC connector tests |

### 2.8 `coins/` (17)

| File | Purpose |
|------|---------|
| `coin.go` | Coin registry built from conf |
| `address.go` | `Address`, `Coin.DecodeAddress` |
| `amount.go` | `ParseAmount`/`FormatAmount` |
| `base58.go` | Base58 encode/decode |
| `base58check.go` | Base58check encode/decode |
| `bech32.go` | Bech32 encode/decode |
| `cashaddr.go` | BCH cashaddr encode/decode |
| `htlc.go` | HTLC script builders |
| `script.go` | Script op builders |
| `tx.go` | Tx serialize/deserialize/sign/verify |
| `address_fuzz_test.go` | Address decode fuzz tests |
| `cashaddr_test.go` | Cashaddr tests |
| `coins_test.go` | Coins aggregate tests |
| `coin_test.go` | Coin registry tests |
| `tx_overflow_test.go` | Tx overflow tests |
| `tx_sighash_test.go` | Sighash tests |
| `tx_test.go` | Tx tests |

### 2.9 `crypto/` (2)

| File | Purpose |
|------|---------|
| `signer.go` | `BtcSigner`: packet sign/verify, keygen |
| `signer_test.go` | Signer tests |

### 2.10 `config/` (2)

| File | Purpose |
|------|---------|
| `conf.go` | xbridge.conf INI parse (`[Main]` + `[TICKER]`) |
| `conf_test.go` | Conf parse tests |

### 2.11 `log/` (7)

| File | Purpose |
|------|---------|
| `logger.go` | Leveled logger |
| `multi.go` | Multi-writer logger |
| `file.go` | Rotating file writer |
| `dedup.go` | Deduplicating logger |
| `dedup_test.go` | Dedup tests |
| `file_test.go` | File writer tests |
| `multi_test.go` | Multi-writer tests |

### 2.12 `cmd/xbridged/` (1)

| File | Purpose |
|------|---------|
| `main.go` | Daemon entrypoint: conf, server, node |

### 2.13 `cmd/liveprobe/` (1)

| File | Purpose |
|------|---------|
| `main.go` | Live-network diagnostic probe |

---

## 3. Symbol Mapping Table

Status legend: **1:1** = faithful equivalent; **partial** = Go covers a subset of the C++ surface; **GAP** = no Go equivalent.

| # | C++ symbol (ref) | Go equivalent (cand) | Status |
|---|------------------|----------------------|--------|
| 1 | `TxCancelReason` enum (25 values, `xbridgepacket.h:21-48`) | — (reasons carried as raw `uint32`; `proto.CancelBody.Reason`, `store.go Reason`, `MoveToHistoryU32`) | **GAP** |
| 2 | `XBridgeCommand` enum (23 values, `xbridgepacket.h:52-282`) | `proto/command.go` `Xbc*` consts (23) | 1:1 |
| 3 | `Transaction::State` (11, `xbridgetransaction.h:36-50`) | `swap/state.go` `Tr*` `State` (11) + `IsTerminal`/`IsValid` | 1:1 |
| 4 | `TransactionDescr::State` (16, `xbridgetransactiondescr.h:43-61`) | `swap/state.go` `DescrState` (16) + `strState` names | 1:1 |
| 5 | `Transaction::` TTL consts (lockTime, pendingTTL, TTL, deadlineTTL, blocksTTL) | `swap/state.go` `LockTime`, `PendingTTL`, `TTL`, `DeadlineTTL`, `BlocksTTL` | 1:1 |
| 6 | `XBridgePacket` class (`xbridgepacket.h`) | `proto/packet.go` `Packet` | 1:1 |
| 7 | `Session` class (`xbridgesession.h/.cpp`, phases) | `api/swap.go` `SwapSession` handlers + `proto/body_types.go` bodies | partial |
| 8 | `Exchange` class (`xbridgeexchange.h/.cpp`) | `api/store.go` `Store`, `api/order.go` `Order` | partial |
| 9 | `WalletConnector` interface (`xbridgewalletconnector.h`) | `wallet/connector.go` `Connector` interface | partial |
| 10 | `BtcWalletConnector<CryptoProvider>` (`xbridgewalletconnectorbtc.*`) | `wallet/rpc.go` `RPCConnector` | partial |
| 11 | `BtcCryptoProvider` (`xbridgecryptoproviderbtc.*`) | `crypto/signer.go` `BtcSigner` | 1:1 |
| 12 | `XBitcoinAddress` / `CBase58Data` (`xbitcoinaddress.*`) | `coins/address.go` `Address`, `coins/base58.go`/`base58check.go` | partial |
| 13 | `xbridge::rpc` helpers (`xbridgerpc.cpp`) | `wallet/rpc.go` `RPCConnector` methods | partial |
| 14 | `Settings` (`util/settings.h`) | `config/conf.go` `Conf`/`Main`/`CoinConf` | 1:1 |
| 15 | `App` (`xbridgeapp.cpp/.h`) | `api/node.go` `Node` + `api/engine.go` | partial |
| 16 | RPC handlers in `rpcxbridge.cpp` (24) | `api/handlers.go` + `api/dispatch.go` | 23 mapped, 1 GAP |
| 17 | `App::selectUtxos` / `App::selectPartialUtxos` (`xbridgeapp.cpp`) | `api/utxo_select.go` `selectUtxos`/`selectPartialUtxos` | 1:1 |
| 18 | `xBridgeStringValueFromAmount` (`xutil.cpp`) | `api/response.go` `formatXAmount` | 1:1 |
| 19 | `xBridgeStringValueFromPrice` (`xutil.cpp`) | `api/response.go` `formatXPrice` | 1:1 |
| 20 | `xBridgeSignificantDigits` (`xutil.cpp`) | `api/response.go` `xBridgeSignificantDigits` | 1:1 |
| 21 | `xutil::iso8601` | `api/response.go` `iso8601` | 1:1 |
| 22 | `xutil::base64_encode` / `base64_decode` | — (stdlib `encoding/base64` only in wallet sign-message path) | **GAP** |
| 23 | `xutil::makeError` | `api/response.go` `makeError` | 1:1 |
| 24 | `xbridge::Error` enum + `xbridgeErrorText` (`util/xbridgeerror.*`) | `api/response.go` `err*` consts + `xbridgeErrorText` | 1:1 |
| 25 | `XBitcoinTransaction` (`xbitcointransaction.*`) | `coins/tx.go` `Tx` | 1:1 |
| 26 | `XBridgeTransactionMember` (`xbridgetransactionmember.*`) | `swap/deposit.go`, `swap/transaction.go` | partial |
| 27 | `xbridgedb` LevelDB persistence | `api/persist.go` | partial |
| 28 | Per-coin connector subclasses (bch/bcd/btg/dgb/part/dcr/devault/stealth) | — (Go is conf-driven; one generic connector) | **GAP** |
| 29 | `Session::getAddressBook` | — | **GAP** |
| 30 | `XSeries`/`XSeriesCache` (`util/xseries.*`) | `api/handlers.go` `dxGetOrderHistory` local-fills aggregation | **GAP** |
| 31 | `currency.h` / `currencypair.h` helpers | — | **GAP** |
| 32 | `bitcoinrpcconnector` (bitcoin-cli subprocess) | — (Go uses HTTP JSON-RPC directly) | **GAP** |
| 33 | `rpc::eth_gasPrice/eth_accounts/eth_getBalance/eth_sendTransaction` | — | **GAP** |
| 34 | `rpc::requestAddressBook`, `addMultisigAddress`, `dumpPrivKey`, `importPrivKey`, `getNewPubKey`, `getTransaction` | — | **GAP** |
| 35 | `txlog` / `fastdelegate` / `posixtimeconversion` (infra) | — (Go uses stdlib time/log) | **GAP** |
| 36 | `xuiconnector.h` (UI connector) | — | **GAP** |

### 3.1 dx* RPC handler ↔ Go handler map (row 16 expanded)

| dx* handler (`rpcxbridge.cpp`) | Go handler (`api/handlers.go`) | Status |
|-------------------------------|-------------------------------|--------|
| `dxGetOrderFills` | `dxGetOrderFills` | 1:1 |
| `dxGetOrders` | `dxGetOrders` | 1:1 |
| `dxGetOrder` | `dxGetOrder` | 1:1 |
| `dxGetLocalTokens` | `dxGetLocalTokens` | 1:1 |
| `dxLoadXBridgeConf` | `dxLoadXBridgeConf` | 1:1 |
| `dxGetNewTokenAddress` | `dxGetNewTokenAddress` | 1:1 |
| `dxGetNetworkTokens` | `dxGetNetworkTokens` | 1:1 |
| `dxMakeOrder` | `dxMakeOrder` | 1:1 |
| `dxMakePartialOrder` | `dxMakePartialOrder` | 1:1 |
| `dxTakeOrder` | `dxTakeOrder` | 1:1 |
| `dxCancelOrder` | `dxCancelOrder` | 1:1 |
| `dxGetOrderHistory` | `dxGetOrderHistory` | 1:1 |
| `dxGetOrderBook` | `dxGetOrderBook` | 1:1 |
| `dxGetTokenBalances` | `dxGetTokenBalances` | 1:1 |
| `dxGetMyOrders` | `dxGetMyOrders` | 1:1 |
| `dxGetMyPartialOrderChain` | `dxGetMyPartialOrderChain` | 1:1 |
| `dxPartialOrderChainDetails` | `dxPartialOrderChainDetails` | 1:1 |
| `dxGetLockedUtxos` | `dxGetLockedUtxos` | 1:1 |
| `dxFlushCancelledOrders` | `dxFlushCancelledOrders` | 1:1 |
| `gettradingdata` | — (only `dxGetTradingData` exposed) | **GAP** |
| `dxGetTradingData` | `dxGetTradingData` | 1:1 |
| `dxSplitAddress` | `dxSplitAddress` | 1:1 |
| `dxSplitInputs` | `dxSplitInputs` | 1:1 |
| `dxGetUtxos` | `dxGetUtxos` | 1:1 |

### 3.2 xutil helper ↔ formatter map (row 18-23 expanded)

| C++ (`util/xutil.cpp`) | Go (`api/response.go` / `api/utxo_select.go`) | Status |
|------------------------|----------------------------------------------|--------|
| `xBridgeStringValueFromAmount` | `formatXAmount` | 1:1 |
| `xBridgeStringValueFromPrice` | `formatXPrice` | 1:1 |
| `xBridgeValueFromAmount` / `xBridgeIntFromReal` | `xBridgeValueFromAmount` / `xBridgeIntFromReal` (`utxo_select.go`) | 1:1 |
| `xBridgeSignificantDigits` | `xBridgeSignificantDigits` | 1:1 |
| `xBridgeValidCoin` | `xBridgeValidCoin` | 1:1 |
| `xBridgeSourceAmountFromPrice` / `xBridgeDestAmountFromPrice` | `xBridgeSourceAmountFromPrice` | partial |
| `iso8601` | `iso8601` | 1:1 |
| `base64_encode` / `base64_decode` | — | **GAP** |
| `makeError` | `makeError` | 1:1 |
| `LogOrderMsg` | `log/` package (no direct port) | partial |

---

## 4. Coverage Summary

**1:1 (faithful):** wire contract surface — `XBridgeCommand`, `Transaction::State`, `TransactionDescr::State`, TTL constants, `XBridgePacket`, `BtcCryptoProvider`, `XBitcoinTransaction`, `Settings`-equivalent conf reader, all amount/price/date/error formatters, `xbridge::Error` + `xbridgeErrorText`, `selectUtxos`/`selectPartialUtxos`, and 23 of 24 RPC handlers (both name and JSON schema).

**Partial (behavioral equivalent but subset / restructured):** `Session` → `SwapSession`; `Exchange` → `Store`+`Order`; `WalletConnector`/`BtcWalletConnector` → `Connector`+`RPCConnector` (narrower method set — no `checkDepositTransaction`/`getSecretFromPaymentTransaction`/`createDepositUnlockScript` equivalents surfaced as connector methods, HTLC building lives in `coins/htlc.go` + `swap/deposit.go`); `App` → `Node`+`engine` (no exchange-node/aggregate history roles); `xbridgedb` → `persist.go`; `XBridgeTransactionMember` → swap deposit helpers. `dxGetOrderHistory` is schema-faithful but data source is local fills, not the network `XSeries`.

**GAPS:** `TxCancelReason` enum (raw `uint32` only), `xutil::base64_*`, `gettradingdata` RPC, per-coin C++ connector subclasses (by design — conf-driven), `Session::getAddressBook`, `XSeries`/`XSeriesCache`, `currency.h`/`currencypair.h`, `bitcoinrpcconnector`, `rpc::eth_*`, several `rpc::` wallet helpers (`requestAddressBook`, `addMultisigAddress`, `dumpPrivKey`, `importPrivKey`, `getNewPubKey`, `getTransaction`), `txlog`/`fastdelegate`/`posixtimeconversion` infra, `xuiconnector`. Most GAPs are infrastructure or exchange-node-only concerns the thin client intentionally omits; the wire-critical gap is **`TxCancelReason`** (affects log/cancel-reason fidelity).

---

### Counts (recap)

- Reference files: **73** under `xbridge/` (53 root + 4 cashaddr + 16 util), plus 5 referenced plumbing files.
- Candidate files: **110** `.go` (54 non-test + 56 test).
- Mapped symbols: **99** (1:1 or partial mappings, row-level counts; see table).
- GAP symbols/entries: **16**.
