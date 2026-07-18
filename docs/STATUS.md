# xbridge-go — Implementation Status & Roadmap

> **Contributor-facing.** This document tracks per-package implementation state
> and the historical task log. End users should read [`docs/usage.md`](usage.md)
> instead. Status here can lag the code; the code is authoritative.

## Package status

| Package | State | What it does |
|---------|-------|--------------|
| `proto` | wired + tested | Packet header/body codec, command set, signing digest, **and per-`XBridgeCommand` body layouts** (all commands 2–50 ported 1:1 from the C++ writers, incl. `xbcTransaction`/`xbcPendingTransaction`/`xbcTransactionAccepting`). `DecodeBody` dispatches to typed structs. The `xbcTransaction` layout is **validated against a live captured packet** (decodes `DOGE→BLOCK` with 1 UTXO). Stdlib only; unit-tested. |
| `p2p`   | partial | Bitcoin P2P framing + `version`/`verack` handshake **live-verified** against a real Blocknet 4.4.1 node; `xbridge` transport envelope (varint + 20-byte dest addr + 8-byte ts) wrap/unwrap the `XBridgePacket` (`p2p/envelope.go`); `ReadPacket`/`WritePacket` decode/encode it. **Automatic network discovery** (`p2p/discovery`): seeds from DNS + fixed IPs (`p2p/seeds.go`), `getaddr`/`addr` gossip, a pool of outbound peers relayed behind the `api.XConn` interface — so `xbridged` connects "like a core wallet" with no manual `-node`. `cmd/liveprobe` exercises a live node. |
| `crypto`| wired + tested | `BtcSigner` over `btcd/btcec/v2`: 64-byte compact ECDSA over `Packet.Digest()`; round-trip + tamper tests pass. |
| `coins` | partial | Stdlib-only: coin registry + amount parsing + address codec (base58/bech32) **+ UTXO tx model, serialization, HTLC script, and both legacy SIGHASH_ALL and segwit BIP143 signing/verification** (`script.go`, `tx.go`, `htlc.go`); BIP143 validated against the canonical known-answer vectors; unit-tested (`docs/coins.md`). Plus a **Bitcoin Cash CashAddr** codec + per-chain `Family` discriminator (`FamilyUTXOBTC`/`FamilyUTXOBCH`) so BCH addresses decode via CashAddr. Non-UTXO chains (DCR/PART) still todo. |
| `wallet`| partial | `Connector` contract + two impls: `RPCConnector` (JSON-RPC to a Blocknet-core-compatible wallet/node — `getnewaddress`, `listunspent`, `signrawtransactionwithwallet`, `sendrawtransaction`, `estimatesmartfee`) and `LocalConnector` (signs locally via a `LocalSigner`, optional `Broadcaster`). Unit-tested via httptest + a real HTLC sign/verify round-trip (`docs/wallet.md`). Address/UTXO/fee queries still flow from the connected wallet, not synthesized. |
| `swap`  | partial | `Transaction` state machine (port of `xbridgetransaction*`, incl. `xBridgePartialOrderDriftCheck` for partial joins): join + two-confirmation progression + expiry; unit-tested. Deposit layer (`swap/deposit.go`/`swap/session.go`): HTLC `DepositSpec` (build/sign/refund) + `Session` gating trJoined→trHold→trInitialized→trCreated→trFinished on both deposits confirming — constructed + tested; P2P lockTime-exchange/claim/refund *spending* of HTLC outputs still todo. `trSigned`/`trCommited` confirmed vestigial in C++ (never assigned) — gated via `IncreaseStateCounter`, not set. |
| `api`   | partial | `dx*` JSON-RPC surface — all 23 `dx*` commands registered and ported 1:1 from `rpcxbridge.cpp` (field names, positional params, JSON value types). Read/order-entry commands are wire-correct; the three-party client driver (`api/swap.go`) runs the Maker ⇄ ServiceNode ⇄ Taker handshake against in-memory connectors (`TestSwapHandshake`). Remaining: live-hub verification over `p2p` and the thin-client architectural limits noted in [`docs/api.md`](api.md). |

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

## Open items

- Non-UTXO adapters: Decred (`DCR`), Particl (`PART`).
- Live-service-node verification of the swap-handshake claim/refund spends
  (in-memory only today; see `docs/api.md` Tier-3 section).
- `dx*` thin-client architectural limits (see [`docs/api.md`](api.md)):
  `dxGetOrderHistory` / `dxGetTradingData` reflect session-local fills only;
  `dxGetNetworkTokens` completeness is bounded by P2P servicenode-ping coverage.
