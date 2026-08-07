# go-xbridge — Implementation Status & Roadmap

> **Contributor-facing.** This document tracks per-package implementation state
> and the historical task log. End users should read [`docs/usage.md`](usage.md)
> instead. Status here can lag the code; the code is authoritative.
>
> The **consolidated audit register** (C++↔Go fidelity, `dx*` equivalence matrix,
> security findings) lives in [`docs/AUDIT.md`](AUDIT.md).

## Package status

| Package | State | What it does |
|---------|-------|--------------|
| `proto` | wired + tested | Packet header/body codec, command set, signing digest, **and per-`XBridgeCommand` body layouts** (all commands 2–50 ported 1:1 from the C++ writers, incl. `xbcTransaction`/`xbcPendingTransaction`/`xbcTransactionAccepting`). `DecodeBody` dispatches to typed structs. The `xbcTransaction` layout is **validated against a live captured packet** (decodes `DOGE→BLOCK` with 1 UTXO). Stdlib only; unit-tested. `XbcServicesPing` was dropped from `DecodeBody` — servicenode messages are now parsed by `p2p/servicenode` directly. **uint256 byte order VERIFIED** — Bitcoin internal LE preserved verbatim (C++ `xbridgeapp.cpp:2082` `blockHash.begin()`,32; matches Go `body_types.go`). |
| `p2p`   | partial | Bitcoin P2P framing + `version`/`verack` handshake **live-verified** against a real Blocknet 4.4.1 node; `xbridge` transport envelope (varint + 20-byte dest addr + 8-byte ts) wrap/unwrap the `XBridgePacket` (`p2p/envelope.go`); `ReadPacket`/`WritePacket` decode/encode it. **Automatic network discovery** (`p2p/discovery`): seeds from DNS + fixed IPs (`p2p/seeds.go`), `getaddr`/`addr` gossip, a pool of outbound peers relayed behind the `api.XConn` interface — so `xbridged` connects "like a core wallet" with no manual `-node`. `cmd/liveprobe` exercises a live node. **`Conn.OnNonXBridge`** routes raw non-`xbridge` P2P messages (e.g. servicenode `snr`/`snp`/`snlp`) to callers; `version.go`/`envelope.go` export `ReadVarInt`/`MarshalVarStr`/`UnmarshalVarStr` for raw-payload parsing. |
| `p2p/servicenode` | wired + tested | New package: learns the servicenode set from `SNREGISTER`/`SNPING`/`SNLISTPING` P2P messages (`servicenode.go`), mirroring how a core XBridge wallet learns the network token set (`xrouterpeermgr.cpp:536`). `WalletServices()` returns the union of SPV-tier wallet tokens (`^[^:]+$`, excluding `xr`/`xrs`) from servicenodes pinged within the 5-minute running window. Feeds `dxGetNetworkTokens`. Unit-tested. |
| `crypto`| wired + tested | `BtcSigner` over `btcd/btcec/v2`: 64-byte compact ECDSA over `Packet.Digest()`; round-trip + tamper tests pass. `VerifyAgainst` checks a signature against an explicit 33-byte hex pubkey (C++ `packet->verify(pubkey)`), used to verify cancel/reject packets against the order's `mPubKey`/`oPubKey`/`sPubKey`. |
| `coins` | partial | Stdlib-only: coin registry + amount parsing + address codec (base58/bech32) **+ UTXO tx model, serialization, HTLC script, and both legacy SIGHASH_ALL and segwit BIP143 signing/verification** (`script.go`, `tx.go`, `htlc.go`); BIP143 validated against the canonical known-answer vectors; unit-tested (`docs/coins.md`). Plus a **Bitcoin Cash CashAddr** codec + per-chain `Family` discriminator (`FamilyUTXOBTC`/`FamilyUTXOBCH`) so BCH addresses decode via CashAddr. Non-UTXO chains (DCR/PART) still todo. |
| `wallet`| partial | `Connector` contract + two impls: `RPCConnector` (JSON-RPC to a Blocknet-core-compatible wallet/node — `getnewaddress`, `listunspent`, `signrawtransactionwithwallet`, `sendrawtransaction`, `estimatesmartfee`) and `LocalConnector` (signs locally via a `LocalSigner`, optional `Broadcaster`). Unit-tested via httptest + a real HTLC sign/verify round-trip (`docs/wallet.md`). Address/UTXO/fee queries still flow from the connected wallet, not synthesized. |
| `log`   | wired + tested | New dependency-free package: a size-based rotating file writer (`file.go`) and a `slog` multi-handler fanning records to multiple destinations (`multi.go`). Used by `cmd/xbridged` for `-logfile` logging and by the panic-safe RPC envelope. Unit-tested. |
| `swap`  | partial | `Transaction` state machine (port of `xbridgetransaction*`, incl. `xBridgePartialOrderDriftCheck` for partial joins): join + two-confirmation progression + expiry; unit-tested. Deposit layer (`swap/deposit.go`/`swap/session.go`): HTLC `DepositSpec` (build/sign/refund) + `Session` gating trJoined→trHold→trInitialized→trCreated→trFinished on both deposits confirming — constructed + tested. Deposit construction is **C++-faithful**: inputs stamped `SEQUENCE_FINAL` (0xffffffff, matching `createRawTransaction(cltv=true)`) and the HTLC output locks `Amount + fee2` (`outAmount+fee2`, so `checkDepositTransaction`'s `>= amount + 0.95*fee2` passes). Claim/refund *spending* of HTLC outputs is **wired** (`api/swap.go` `buildRefundTx`/`redeemCounterparty`, `api/locktime.go` lockTime-drift check, `api/node.go` auto-refund broadcast on expiry). `trSigned`/`trCommited` confirmed vestigial in C++ (never assigned) — gated via `IncreaseStateCounter`, not set. `api/locktime.go` adds `acceptableLockTimeDrift`/`computeLockTimeFor` validating the counterparty deposit lockTime before our deposit/redeem. |
| `api`   | partial | `dx*` JSON-RPC surface — all 23 `dx*` commands registered and ported 1:1 from `rpcxbridge.cpp` (field names, positional params, JSON value types). The C++ `gettradingdata` command is intentionally NOT exposed — only `dxGetTradingData` is. Read/order-entry commands are wire-correct; the three-party client driver (`api/swap.go`) runs the Maker ⇄ ServiceNode ⇄ Taker handshake against in-memory connectors (`TestSwapHandshake`). Remote **cancel/reject** handling is now ported (`onRemoteCancel`/`onRemoteReject`, `xbridgesession.cpp:3288-3485`); inbound packets are signature-verified; `dxGetOrderBook` emits `[]` for empty sides and is PARITY at detail levels 1–3 (detail 4 nesting diverges — see [`AUDIT.md`](AUDIT.md) S2-A); dust/fee fidelity uses conf `DustAmount` (C++ xbridgeapp.cpp:345) then the C++-defined constant 5460 (xbridgewalletconnectorbtc.cpp:1526), with `FeePerByte`/`MinTxFee` fee floors (xbridgewalletconnectorbtc.cpp:1949-1972); `Order` carries the full `TransactionDescr` fidelity fields (persisted to disk). UTXO ownership-proof challenge string matches C++ `UtxoEntry::toString()` byte-for-byte — the whole-coin `listunspent` `"value"` double formatted like C++'s default `ostringstream` (see [`AUDIT.md`](AUDIT.md) C13), so cross-impl proofs verify. Remaining: live-hub verification over `p2p` and the thin-client architectural limits noted in [`docs/api.md`](api.md). |

## Roadmap / history

1. ✅ Wire `crypto.Signer` with `btcd/btcec/v2` and add a signing test.
2. ✅ Implement the Bitcoin `version`/`verack` handshake (`p2p/conn.go`,
   `p2p/version.go`), **live-verified** against a real Blocknet 4.4.1 service
   node (`coreproxy.airdns.org:42111`, magic `a1 a0 a2 a3`): handshake
   accepted, `xbridge` command confirmed as `"xbridge"`, and live packets decode
   with `version=55` / real `XBridgeCommand`s. The `xbridge` payload carries a
   transport envelope (varint + 20-byte dest addr + 8-byte timestamp) in
   `p2p/envelope.go`.
3. ✅ Build `swap/` state machine core (join + two-confirmation progression +
   expiry), unit-tested (`swap/`, `docs/swap.md`), including the `Session`/
   deposit layer that gates `trJoined→trHold→trInitialized→trCreated→trFinished`
   once both HTLC deposits confirm (`trSigned`/`trCommited` are vestigial in C++,
   as noted in `docs/swap.md` §2).
4. ✅ Build `coins/` foundation (coin registry, amount parsing, address codec)
   and per-coin **tx construction** (UTXO tx model, serialization, HTLC deposit
   script, SIGHASH_ALL + segwit BIP143 sign/verify), validated against the
   canonical known-answer vectors (`coins/`, `docs/coins.md`). Remaining:
   non-UTXO adapters (DCR/PART).
5. ✅ Build `wallet/` connector to the connected SPV wallet (signs + broadcasts +
   pays fee): `RPCConnector` (JSON-RPC to a Blocknet-core-compatible wallet) +
   `LocalConnector` (local keys + optional broadcast). The three-party client
   driver (`api/swap.go`) runs the full Maker ⇄ ServiceNode ⇄ Taker handshake
   against in-memory connectors; remaining: live-hub verification over `p2p` and
   non-UTXO adapters (DCR/PART).
6. ✅ **Security hardening (F1/F2/F10, 2026).** RPC binds to loopback by default
   (`-rpcbind`, mirroring blocknetd's `httpserver.cpp:308` loopback default) with
   optional HTTP Basic auth enforced when `-rpcuser` + `-rpcpassword` are both
   configured (constant-time compare, no cookie fallback); RPC request bodies are
   capped at 4 MiB (`http.MaxBytesReader`). Swap-handshake inbound packets
   (Hold/Init/CreateA/B/ConfirmA/B/Finished) are re-verified against the trusted
   hub key pinned at session creation — the servicenode chosen by `MakeOrder` for
   the maker, `Order.SNodePubkey` for the taker — plus its registry membership
   (C++ `packet->verify(xtx->sPubKey)` (`xbridgesession.cpp:1364`) + `getSn`
   (`:1384`)); an order with no self-consistent hub anchor is refused
   (`NO_SERVICE_NODE`) and cmd-3 broadcasts are ignored (client binds no handler),
   so a forged `Finished` can never disable the auto-refund watcher. See
   [`AUDIT.md`](AUDIT.md) F1/F2/F10/S2-E.

## Open items

- Non-UTXO adapters: Decred (`DCR`), Particl (`PART`).
- Live-service-node verification of the swap-handshake claim/refund spends
  (in-memory only today; see `docs/api.md` Tier-3 section).
- `dx*` thin-client architectural limits (see [`docs/api.md`](api.md)):
  `dxGetOrderHistory` / `dxGetTradingData` reflect session-local fills only;
  `dxGetNetworkTokens` completeness is bounded by P2P servicenode-ping coverage
  (now learned via the servicenode registry, but still P2P-bounded).
