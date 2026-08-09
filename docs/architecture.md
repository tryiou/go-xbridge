# go-xbridge — Architecture & contributor guide

This is the developer-facing reference for the `go-xbridge` codebase: how the
packages are organized, what each one does, how the pieces connect, and how to
build/test the port. End users should read [`../README.md`](../README.md); the
wire contract is in [`protocol.md`](protocol.md); the `dx*` JSON-RPC surface is
in [`api.md`](api.md); C++↔Go fidelity and open divergences are in
[`audit.md`](audit.md).

> Status here can lag the code; the code is authoritative.

## Overview

`go-xbridge` is a from-scratch **Go reimplementation** of the Blocknet XBridge
atomic-swap engine. It is a **thin client**: it connects to the live Blocknet
service-node P2P network, speaks the `xbridge` wire message, and trades through
the user's own SPV wallets via JSON-RPC. It does not validate blocks, serve a
blockchain, or run a service node.

The port is organized bottom-up: `proto`/`p2p` (wire) → `coins` (transaction
construction) → `wallet` (RPC/local signing) → `swap` (state machine) → `api`
(dx* RPC + the three-party swap client driver) → `cmd/xbridged` (daemon).

## Layout

| Package | What it does |
|---------|--------------|
| `proto/` | XBridge packet header/body codec, `XBridgeCommand` set, signing digest, and per-command body layouts. `DecodeBody` dispatches to typed structs. |
| `p2p/` | Bitcoin-style P2P framing, `version`/`verack` handshake, the XBridge transport envelope (`encodeXBridgePayload`/`DecodeXBridgePayload`). |
| `p2p/discovery/` | Automatic network discovery: DNS seeds + fixed IPs, `getaddr`/`addr` gossip, an outbound peer pool. |
| `p2p/servicenode/` | Learns the servicenode set from `SNREGISTER`/`SNPING`/`SNLISTPING` messages; feeds `dxGetNetworkTokens`. |
| `crypto/` | secp256k1 compact ECDSA signer (`BtcSigner`) over `btcd/btcec/v2`. |
| `coins/` | Per-coin model: coin registry, amount parsing, address codec (base58/bech32/CashAddr), UTXO tx construction, HTLC deposit scripts, legacy + BIP143 signing. All coin definitions come from `xbridge.conf`. |
| `config/` | Read-only INI loader mirroring `xbridgeapp.cpp::createConf()`. |
| `wallet/` | `Connector` contract + two implementations: `RPCConnector` (drives a wallet/node over JSON-RPC) and `LocalConnector` (signs locally). |
| `swap/` | `Transaction` state machine (port of `xbridgetransaction.*`) + the HTLC deposit layer (`DepositSpec`, `Session`). |
| `api/` | The `dx*` JSON-RPC surface (drop-in for dapps) + the three-party Maker ⇄ ServiceNode ⇄ Taker swap client driver, on a single-owner engine (see "Engine & concurrency"). |
| `log/` | Dependency-free logging: size-based rotating file writer + multi-handler fanout. |
| `cmd/xbridged` | The daemon. Flags: `-conf` (default `<home>/.blocknet/xbridge.conf`, fatal if missing), `-network`, `-node`, `-addnode`, `-rpcbind`, `-datadir`, etc. |
| `cmd/liveprobe` | Ad-hoc live node check: dials + handshakes a real service node. |

## Package reference

### `proto/` — wire codec

Packet header (129 bytes), body field encoding, command set, and the signing
digest — the byte-level contract. All multi-byte integers are little-endian.
The `xbcTransaction` layout is validated against a live captured packet
(decodes `DOGE→BLOCK` with 1 UTXO). Stdlib-only.

- `packet.go` — `Packet`, `Marshal`, `Unmarshal`, `Digest` (SHA256 over header
  + body with the signature region zeroed).
- `body.go` / `body_types.go` — typed field encodings and per-`XBridgeCommand`
  body layouts. **`body_types.go` top comment is the authoritative layout
  record** — the C++ header-comment enums are stale. The `xbcServicesPing` and
  `xbcXChatMessage` bodies are typed but have no C++ live writer; servicenode
  messages are parsed by `p2p/servicenode` directly.
- `command.go` — `XBridgeCommand` values.

The wire format itself is specified in [`protocol.md`](protocol.md).

### `p2p/` — P2P transport

- `message.go` — Bitcoin P2P `Message`, `Checksum`, `Marshal`, `UnmarshalMessage`.
- `envelope.go` — the XBridge transport envelope: varint length + 20-byte
  destination uint160 + 8-byte timestamp wrapping the `XBridgePacket`.
  Exports `ReadVarInt`/`MarshalVarStr`/`UnmarshalVarStr` for raw-payload parsing.
- `version.go` — the `version`/`verack` handshake (live-verified against a real
  Blocknet 4.4.1 node). Thin client advertises no services, no chain height,
  `relay=false`.
- `conn.go` — connection + `handshake()`, `ReadPacket`/`WritePacket`,
  `OnNonXBridge` routes raw non-`xbridge` messages (e.g. servicenode
  `snr`/`snp`/`snlp`) to callers.
- `addr.go`, `params.go`, `seeds.go` — address gossip, network magics/ports,
  DNS + fixed-IP seed list.

`p2p/discovery/` — automatic discovery "like a core wallet": DNS seeds, fixed
IPs, `getaddr`/`addr` gossip, an outbound peer pool, relayed behind the
`api.XConn` interface so no manual `-node` is required.

`p2p/servicenode/` — learns the servicenode set from
`SNREGISTER`/`SNPING`/`SNLISTPING` P2P messages, mirroring how a core XBridge
wallet learns the network token set. `WalletServices()` returns the union of
SPV-tier wallet tokens (`^[^:]+$`, excluding `xr`/`xrs`) from servicenodes
pinged within the 5-minute running window. Feeds `dxGetNetworkTokens`.

### `crypto/` — signing

`BtcSigner` over `btcd/btcec/v2`: 64-byte compact ECDSA over `Packet.Digest()`.
`VerifyAgainst` checks a signature against an explicit 33-byte hex pubkey (C++
`packet->verify(pubkey)`), used to verify cancel/reject packets against the
order's `mPubKey`/`oPubKey`/`sPubKey`. External dependencies are
`btcd/btcec/v2`, `dcrd/dcrec/secp256k1/v4` and `golang.org/x/crypto` (ripemd160);
see `go.mod`.

### `coins/` — per-coin model

The foundation the `swap` deposit/refund layer (and `wallet/`) builds on. It
covers the three things every chain interaction needs:

1. **Coin metadata** (`coin.go`) — ticker, name, decimal precision, base58check
   version bytes / bech32 HRP. The registry is **conf-driven**: `InitFromConf`
   builds `Coins` from the `[TICKER]` sections of `xbridge.conf`; there is no
   baked-in set. The segwit/HRP selection in `coin.go` is a per-`CreateTxMethod`
   lookup table reproducing C++'s connector classes.
2. **Amount parsing** (`amount.go`) — decimal string ⇄ base units, honoring each
   coin's `Decimals`.
3. **Address codec** (`base58*.go`, `bech32.go`, `cashaddr.go`, `address.go`) —
   legacy (P2PKH/P2SH) and native segwit (P2WPKH/P2WSH/P2TR) addresses, plus BCH
   CashAddr. `Address.ID()` exposes the 20-byte `swap.Addr` the swap layer uses.
   The BCH family is selected by `CreateTxMethod` (`"BCH"` → `FamilyUTXOBCH`);
   BCH's legacy base58 version byte collides with BTC's, so BCH addresses decode
   **only** via `cashaddrDecode`.

**Transaction construction** (`script.go`, `tx.go`, `htlc.go`), ported from
`xbridgewalletconnectorbtc.cpp`:

- `script.go` — opcodes + minimal `pushData`/`pushNum`, `BuildP2PKHScript` /
  `BuildP2SHScript`.
- `tx.go` — `Tx`/`TxIn`/`TxOut`/`OutPoint` + classic/segwit `Serialize`, legacy
  `HashForSigning` (SIGHASH_ALL, matches C++ `SignatureHash`), and
  `SignTxInput`/`VerifyTxInput`. Segwit (BIP143): `HashForSigningSegwit` with the
  witness-v0 commitment, `P2WPKHScriptCode`, `SignTxInputSegwit`/
  `VerifyTxInputSegwit` — validated against the canonical BIP143 known-answer
  vectors. (`TxIn.Amount` carries the spent value for the commitment; it is not
  serialized.) On time-field coins, `HashForSigning` commits the 4-byte LE
  `TxTime` after `nVersion` (C++ `CTransactionSignatureSerializer`); golden
  digests from a C++ oracle are asserted in `TestHashForSigningWithTimeField`.
- `htlc.go` — `KeyID(pubKey)` = HASH160(pubKey); `BuildDepositUnlockScript`
  builds the XBridge HTLC redeem script (C++ `createDepositUnlockScript`):
  IF branch = `<lockTime> CLTV OP_DROP DUP HASH160 <KeyID(my)> EQUALVERIFY
  CHECKSIG`; ELSE branch = `DUP HASH160 <KeyID(other)> EQUALVERIFY
  CHECKSIGVERIFY SIZE 33 EQUALVERIFY HASH160 <secretHash> EQUAL`. Also
  `BuildRefundScriptSig` (`<sig> <myPubKey> OP_1 <inner>`) and
  `BuildPaymentScriptSig` (`<xPubKey> <sig> <myPubKey> OP_0 <inner>`).

The deposit output is a P2SH of `BuildDepositUnlockScript(...)`; the refund and
payment transactions spend it via the two `scriptSig` builders above, signing
with `SignTxInput`.

### `config/` — conf loader

Read-only INI loader mirroring `xbridgeapp.cpp::createConf()`. `CoinConf`
carries every `[TICKER]` key; the library never creates or mutates the file.

### `wallet/` — wallet connector

The bridge between `coins` transaction construction and the user's own wallet.
The library never holds BLOCK or pays fees itself — the **connected SPV wallet**
does: it holds the keys, signs, broadcasts, and (for BLOCK) pays the service-node
fee via core RPC. `wallet/` only signs and moves bytes.

```go
type Connector interface {
    Ticker() string
    GetNewAddress() (string, error)
    ListUnspent(minConf int) ([]Utxo, error)
    SignRawTransaction(txHex string, prevTxs []PrevTx) (signedHex string, complete bool, err error)
    SendRawTransaction(txHex string) (txid string, err error)
    GetRelayFee() (float64, error)
    GetBlockCount() (int64, error)
    GetBlockHash(height int64) ([32]byte, error)
    GetRawTransaction(txid string) (string, error)
    SignMessage(address, message string) ([]byte, error)
    VerifyMessage(address string, sig []byte, message string) (bool, error)
}
```

- **`RPCConnector`** — drives a single coin over Bitcoin-Core-style JSON-RPC 1.0:
  `getnewaddress`, `listunspent`, `signrawtransaction` (with
  `signrawtransactionwithwallet` fallback), `sendrawtransaction`, `getinfo`,
  `getblockcount`, `getblockhash`, `getrawtransaction`, `signmessage`,
  `verifymessage`. `conf.go` builds it from a
  `config.CoinConf` — endpoint, credentials, decimals, segwit, RPC version and
  content-type all come from conf; nothing is hardcoded. Fees come from the conf
  `FeePerByte` (and the wallet's `getinfo` relay fee); **no `estimatesmartfee` /
  `estimatefee` RPC is used**, matching C++ (`api/handlers.go:1429`). Amounts
  cross the JSON boundary as floats; `wallet` converts via string formatting to
  avoid IEEE-754 drift (`coins.FormatAmount` ⇄ `strconv.FormatFloat` ⇄
  `coins.ParseAmount`).
- **`LocalConnector`** — implements `Connector` for the case where the library
  itself holds the keys. Signs each input with a caller-supplied `LocalSigner`
  (`SignInput(tx *coins.Tx, idx int, prev PrevTx) ([]byte, error)`), e.g. via
  `coins.BuildRefundScriptSig` / `BuildPaymentScriptSig`. An optional
  `Broadcaster` backs `SendRawTransaction`; a nil broadcaster makes the connector
  sign-only (offline/test signing). `GetNewAddress`/`ListUnspent`/`GetRelayFee`/
  `GetBlockCount`/`GetBlockHash`/`GetRawTransaction`/`SignMessage`/`VerifyMessage`
  have no local source and return an error — the deposit flow supplies inputs
  and `prevTxs` explicitly.

### `swap/` — swap state machine + deposit layer

Canonical spec of the C++ `xbridgetransaction.{h,cpp}` port (the **highest-risk**
part of the reimplementation, mirrored field-for-field and covered by
`swap/transaction_test.go`).

**Roles.** A swap has exactly two members: **A (maker)** created the order
(`xbcTransaction`); **B (taker)** created the complementary order that joins A's
(`xbcTransactionAccepting` → `TryJoin`). Each member has a `Source` address
(sends FROM) and a `Dest` address (receives TO), both 20-byte uint160 values.
The maker order describes the trade from A's perspective: A gives
`SourceCurrency:SourceAmount` and receives `DestCurrency:DestAmount`.

**States.** `trInvalid trNew trJoined trHold trInitialized trCreated trSigned
trCommited trFinished trCancelled trDropped`. `trSigned`/`trCommited` exist in
the enum but are **not** part of the `increaseStateCounter` progression
(signing/commit happen in the `xbridgesession*` deposit/refund layer, not the
two-confirmation gate). The gate walks:

```
trNew --TryJoin--> trJoined --(both Source)--> trHold
   --(both Dest)--> trInitialized --(both Source)--> trCreated
   --(both Dest)--> trFinished
```

`trCancelled`, `trDropped`, `trFinished` are terminal (`State.IsTerminal`).

**Join (`TryJoin`).** A taker order `o` joins maker `t` iff: both are `trNew`;
`t.SourceCurrency == o.DestCurrency` and `t.DestCurrency == o.SourceCurrency`;
`t.PartialAllowed == o.PartialAllowed`; **non-partial:** exact amount equality;
**partial:** `t.SourceAmount >= o.DestAmount`, `t.DestAmount >= o.SourceAmount`,
`o.DestAmount >= t.MinFromAmount`, plus `xBridgePartialOrderDriftCheck`
(price-integrity / satoshi-level drift band, ported in `swap/price.go`). On
success `t.B = o.A` and state → `trJoined`.

**Progression (`IncreaseStateCounter`).** Each phase requires **both** members
to confirm before advancing, matching C++'s single `m_a_stateChanged`/
`m_b_stateChanged` pair (the two flags are reused across phases, reset after
each transition):

| Current state | `from` must match | → next |
|---------------|-------------------|--------|
| `trJoined`    | `A.Source` & `B.Source` | `trHold` |
| `trHold`      | `A.Dest` & `B.Dest`     | `trInitialized` |
| `trInitialized` | `A.Source` & `B.Source` | `trCreated` |
| `trCreated`   | `A.Dest` & `B.Dest`     | `trFinished` |

An unrecognized `from` is a no-op; a `state` argument not equal to the current
state returns `trInvalid` and leaves the state unchanged; phases outside the
four handled (incl. `trSigned`/`trCommited`) return `trInvalid`.

**Timing.** `LockTime` 600 s; `PendingTTL` 360 s (trNew idle expiry); `TTL`
3600 s (post-trNew idle expiry); `DeadlineTTL` 604800 s (trNew creation
deadline); `BlocksTTL` 10080 blocks. `IsExpired(now)`: `trNew` &&
(age-from-creation > `DeadlineTTL` || age-since-last > `PendingTTL`) → expired;
state > `trNew` && age-since-last > `TTL` → expired.
`IsExpiredByBlockNumber(currentBlock)`: `trNew` && `currentBlock - BlockNumber >
BlocksTTL` → expired (the block-height analog of `DeadlineTTL`; `BlockNumber` is
the chain height at creation, set from `Connector.GetBlockCount`); state >
`trNew` delegates to time-based `IsExpired`. Implemented in
`swap/transaction.go`, unit-tested in `swap/transaction_test.go`.

**Deposit layer** (`swap/deposit.go`, `swap/session.go`), ported at the
construction + gating level:

- `DepositSpec` describes one side's HTLC deposit: `RedeemScript()` via
  `coins.BuildDepositUnlockScript`; `P2SHScript()` wraps it; `BuildDepositTx`
  locks `Amount + fee2` into the P2SH (`fee2 = minTxFee2(1,1)`, satisfying the
  C++ `depositP2SHAmount >= amount + 0.95*fee2` check), spending funding UTXOs
  stamped `SEQUENCE_FINAL` (0xffffffff, matching C++ `createRawTransaction(
  ..., cltv=true)`); the CLTV-enabling non-final sequence belongs on the *refund*
  spend only. The depositor generates a 33-byte `Secret`; `SecretHash()` is
  HASH160(Secret); the counterparty adopts only the `Hash`.
- `Session` wraps a joined `Transaction` for the local `Role`.
  `CreateLocalDeposit` generates the secret + builds the local `DepositSpec`
  (maker locks SourceAmount/SourceCurrency, taker locks DestAmount/DestCurrency).
  `AdoptCounterparty` records the revealed `Hash` + lockTime.
  `ConfirmLocalDeposit`/`ConfirmOtherDeposit` advance the progression through
  every gate once both sides' deposits confirm.

**Client driver lives in `api/swap.go`, not `swap/`.** The XBridge **CLIENT**
side of the Maker ⇄ ServiceNode HUB ⇄ Taker protocol: the local `SwapSession`
responds to hub-originated packets and performs the on-chain work
(build/broadcast the HTLC deposit, redeem the counterparty's deposit revealing
the secret, pre-build the CLTV refund). Driven end-to-end by
`TestSwapHandshake` in `api/swap_test.go` with fake connectors. The CLTV refund
(`BuildRefundScriptSig`) and ELSE-branch payment (`BuildPaymentScriptSig`,
revealing the secret) are built locally; the taker recovers the secret from the
maker's payTx via `conn.GetRawTransaction(APayTxID)`.

### `api/` — `dx*` RPC + swap driver

`dx*` JSON-RPC surface — all 23 `dx*` commands registered and ported 1:1 from
`rpcxbridge.cpp` (field names, positional params, JSON value types). The C++
`gettradingdata` command is intentionally NOT exposed — only `dxGetTradingData`
is. Contracts, response shapes and error codes are in [`api.md`](api.md).

- `server.go` / `response.go` — JSON-RPC 1.0 envelope, panic-safe, HTTP Basic
  auth, 4 MiB request-body cap.
- `dispatch.go` — command dispatch table.
- `node.go` — `Node`: inbound packet handling, hub-key re-verification of
  handshake packets, engine-scheduled refund sweep, persistence reload.
- `handlers.go` / `order.go` / `store.go` — per-command handlers, order model,
  bounded copy-on-write fills/history store.
- `swap.go` — the three-party swap client driver, with the two-phase handshake
  (wallet I/O on workers) and the engine-scheduled refund sweep.
- `engine.go` — the single-owner engine goroutine + worker pool (below).
- `persist.go` — per-trade state + keypair persistence to
  `<datadir>/xbridged-swaps.json`, written on the engine goroutine.
- `locktime.go` — `acceptableLockTimeDrift`/`computeLockTimeFor` validating the
  counterparty deposit lockTime before our deposit/redeem.

### Engine & concurrency (`api/`)

The engine is a **single owner**: one goroutine (`engineLoop`) owns all mutable
state — `n.sessions`, the live book writes, refund state, and the persist path —
so the swap handshake and the refund sweep can never race each other.

- **`engineLoop`** selects over five inputs in priority order: handler commands
  (`n.cmds`, from `submit`), decoded packets (`n.packets`, from the reader),
  worker results (`n.results`), the 60 s refund/persist ticker, and `n.stop`.
  Every handler runs inside `safeRun`, so a single bad packet can never kill the
  engine.
- **`readerLoop`** is the read half of the former `feed()`: it blocks on the
  socket, decodes + signature-verifies bodies, and forwards raw packets to the
  engine. A slow peer can no longer stall state processing, and malformed/forged
  packets are dropped before they reach the engine.
- **Worker pool** (`engineWorkers=4`): a `workTask` carries a self-contained
  `run` closure (all wallet RPC/`SendRawTransaction` I/O, capturing values by
  value) and an `apply` closure that runs on the engine. Results flow back over
  a channel buffered to the worker count, so the engine can never deadlock on a
  result send. A wallet that hangs on one coin cannot stall the engine or other
  sessions.
- **Two-phase handshake**: `OnCreateA/B` and `OnConfirmA/B` split into stage 1
  (engine: validate, snapshot the session, enqueue the task, set `await`) and a
  resume (engine: apply the outcome, clear `await`, send the hub response,
  persist). `await` is the retransmit guard: any hub packet arriving while a
  task is in flight is dropped, so a deposit/claim is broadcast at most once.
- **`submit(run, await)`** queues a handler/HTTP command for the engine; when
  the engine is not started (single-threaded tests) it runs inline. Public
  wrappers (`dispatchSwap`, `onRemoteCancel/Reject`, `checkRefunds`) are the
  only `submit` callers; code already on the engine calls the internal functions
  directly.
- **Ticker duties**: `scanRefunds` auto-broadcasts due pre-signed refunds (posting
  worker tasks, never doing I/O on the engine), `pruneSessions` drops terminal
  sessions so the live set stays bounded, and every 4th tick persists.
- **`Close()`** closes `n.stop`, then waits (`wg.Wait`) for every goroutine —
  including an in-flight wallet task — to drain *before* closing the connection,
  so no task ever writes after teardown.

### `log/` — logging

Dependency-free: a size-based rotating file writer (`file.go`) and a `slog`
multi-handler fanning records to multiple destinations (`multi.go`). Used by
`cmd/xbridged` for `-logfile` logging and by the panic-safe RPC envelope.

### `cmd/` — daemon & probe

- `xbridged/` — the daemon. Reads `xbridge.conf` (fatal if missing), starts
  discovery (or `-node`), serves the `dx*` RPC listener, reloads persisted
  swaps before dialing. Full flag reference: `../README.md` "Running `xbridged`".
- `liveprobe/` — ad-hoc live node check (dials + handshakes a real service node).

## Data flow

**Read commands (order book):** dapp → HTTP JSON-RPC → `api` handler →
`Store` (populated by `p2p` decoding `xbcPendingTransaction` broadcasts) →
response.

**Make/take a trade:** dapp → `dxMakeOrder`/`dxTakeOrder` → `api` picks the hub
(`findNodeWithService`) and pins its key → outbound packet
envelope-addressed to the hub over `p2p` → `api/swap.go` `SwapSession` drives
the handshake: build/broadcast HTLC deposits via `swap`+`coins` and the
`wallet.Connector` (fund/sign/broadcast) → claim/refund as the hub advances the
state → hub-pinned inbound packets re-verified before any state mutation.

**Cancellation/refund:** `dxCancelOrder` or the engine-scheduled refund sweep on
expiry; refund spends are built locally (`coins.BuildRefundScriptSig`) and
broadcast through the wallet connector.

**Engine channels:** the reader pushes decoded packets onto `n.packets`; HTTP
handlers submit commands to `n.cmds`; the engine dispatches wallet I/O to
`n.tasks`, workers post outcomes to `n.results`, and the engine applies them.
One goroutine therefore serializes every mutation of the sessions/book/refund
state.

## Build, test, verify

```bash
go build ./...          # build all packages
go vet ./...            # static checks
go test ./...           # unit tests
```

Requires Go 1.25+ (toolchain 1.26 works). Add `-run TestName` to scope tests.
The suite is hermetic — no live-network dials. Notable coverage:

- `proto/` — packet/body codec round-trips + tamper.
- `p2p/` — framing, envelope, version marshal; live-verified against a real
  Blocknet 4.4.1 node via `cmd/liveprobe`.
- `coins/` — BIP143 known-answer vectors, CashAddr round-trips, time-field
  sighash golden digests (C++ oracle), HTLC sign/verify.
- `wallet/` — `httptest` JSON-RPC mock + a real P2SH HTLC sign/verify round-trip.
- `swap/` — state-machine transition table, drift check, session gating.
- `api/` — `TestSwapHandshake` drives the full three-party handshake end-to-end
  with fake connectors; `api/concurrency_test.go` proves the single-owner engine
  under `-race`: refund sweep interleaving with a deposit resume, no-fork
  concurrent takes, a parked wallet not stalling other sessions, and `Close()`
  draining in-flight tasks.
- `coins/` — `TestConcurrentInitFromConfGet` hot-reloads the registry against
  concurrent readers (`-race`).
- `log/` — write-after-close is a nil-safe no-op, also under concurrent
  writer-vs-close (`-race`).

Cross-repo parity gate: `make parity` fact-checks the port
against the C++ `dx*` contract — run it after touching the port or C++ XBridge.

## Conventions

- **Nothing is hardcoded.** Every coin connector (incl. BLOCK and BTC) is
  defined entirely by its `[TICKER]` section in `xbridge.conf`.
- **Fidelity over shortcuts.** Byte-for-byte 1:1 with the C++ wire contract;
  validate against live captured packets and the C++ writers, not header
  comments.
- Amounts are base units of `COIN = 1_000_000`; `dx*` amounts display as
  **6-decimal** fixed strings (matches C++ `setprecision(6)`).
- Run `gofmt` before committing; keep logic / style / refactor in separate
  commits.

## Status & open items

See [`audit.md`](audit.md) for the live C++↔Go divergence register, security
findings, and verification gaps. Long-standing open work:

- Non-UTXO coin adapters: Decred (`DCR`), Particl (`PART`); DEVAULT is
  misclassified as the BTC family (audit S2-J).
- Live-service-node verification of the swap-handshake claim/refund spends
  (in-memory connectors only today; `cmd/liveprobe` dials + handshakes but does
  not drive a swap).
- Thin-client architectural limits (`dxGetOrderHistory` / `dxGetTradingData`
  reflect session-local fills; `dxGetNetworkTokens` is P2P-bounded) — see
  [`api.md`](api.md) "Tier 3".
