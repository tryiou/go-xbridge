# xbridge-go

A portable, standalone **Go** reimplementation of the Blocknet **XBridge**
atomic-swap engine. It is a **thin client**: it speaks the existing XBridge
wire protocol to the live Blocknet service-node P2P network, so a user can trade
by connecting their own (SPV) wallets — **without running `blocknetd`**.

Service-node fees are paid by the user's connected **Blocknet SPV wallet**
(via Blocknet core RPC); BLOCK is treated like any other coin through the
wallet-connector abstraction.

> This is a from-scratch reimplementation, not a wrapper around `blocknetd`.
> The wire contract lives in [`docs/protocol.md`](docs/protocol.md).

## Status

| Package | State | What it does |
|---------|-------|--------------|
| `proto` | wired + tested | Packet header/body codec, command set, signing digest, **and per-`XBridgeCommand` body layouts** (all commands 2–50 ported 1:1 from the C++ writers, incl. `xbcTransaction`/`xbcPendingTransaction`/`xbcTransactionAccepting`). `DecodeBody` dispatches to typed structs. The `xbcTransaction` layout is **validated against a live captured packet** (decodes `DOGE→BLOCK` with 1 UTXO). Stdlib only; unit-tested. |
| `p2p`   | partial | Bitcoin P2P framing + `version`/`verack` handshake **live-verified** against a real Blocknet 4.4.1 node; `xbridge` transport envelope (varint + 20-byte dest addr + 8-byte ts) wrap/unwrap the `XBridgePacket` (`p2p/envelope.go`); `ReadPacket`/`WritePacket` decode/encode it. `cmd/liveprobe` exercises a live node. |
| `crypto`| wired + tested | `BtcSigner` over `btcd/btcec/v2`: 64-byte compact ECDSA over `Packet.Digest()`; round-trip + tamper tests pass. |
| `coins` | partial | Stdlib-only: coin registry + amount parsing + address codec (base58/bech32) **+ UTXO tx model, serialization, HTLC script, and SIGHASH_ALL signing/verification** (`script.go`, `tx.go`, `htlc.go`); unit-tested (`docs/coins.md`). Non-UTXO chains (DCR/PART) still todo. |
| `wallet`| partial | `Connector` contract + two impls: `RPCConnector` (JSON-RPC to a Blocknet-core-compatible wallet/node — `getnewaddress`, `listunspent`, `signrawtransactionwithwallet`, `sendrawtransaction`, `estimatesmartfee`) and `LocalConnector` (signs locally via a `LocalSigner`, optional `Broadcaster`). Unit-tested via httptest + a real HTLC sign/verify round-trip (`docs/wallet.md`). Address/UTXO/fee queries still flow from the connected wallet, not synthesized. |
| `swap`  | partial | `Transaction` state machine (port of `xbridgetransaction*`, incl. `xBridgePartialOrderDriftCheck` for partial joins): join + two-confirmation progression + expiry; unit-tested. Session/deposit layer still todo. |
| `api`   | partial | `dx*` JSON-RPC surface — all 23 `dx*` commands registered and ported 1:1 from `rpcxbridge.cpp` (field names, positional params, JSON value types). Read/order-entry commands are wire-correct; on-chain swap execution (HTLC deposits/refunds) and a few thin-client-only commands (`dxGetOrderHistory`, `dxGetTradingData`) are documented gaps. See [docs/api.md](docs/api.md). |

## Build

```sh
cd xbridge-go
go build ./...
go test ./...
```

## Layout

```
xbridge-go/
  proto/     packet + body codec, command set, signing digest
  p2p/       Bitcoin P2P framing, connection, version/verack handshake
  crypto/    secp256k1 Signer interface (btcec recipe)
  docs/      protocol.md, swap.md, coins.md — canonical specs
  coins/     coin registry, amount parsing, address codec (base58/bech32)
  wallet/    Connector contract + RPC/local wallet connectors (sign/broadcast)
  swap/      Transaction state machine (port of xbridgetransaction*)
  api/       dx* API surface (port of rpcxbridge.cpp)
```

## Next steps

1. ~~Wire `crypto.Signer` with `btcd/btcec/v2` and add a signing test.~~ ✅ done.
2. ~~Implement the Bitcoin `version`/`verack` handshake (`p2p/conn.go`,
   `p2p/version.go`).~~ ✅ done and **live-verified** against a real Blocknet
   4.4.1 service node (`coreproxy.airdns.org:42111`, magic `a1 a0 a2 a3`):
   handshake accepted, `xbridge` command confirmed as `"xbridge"`, and live
   packets decode with `version=55` / real `XBridgeCommand`s. The `xbridge`
   payload carries a transport envelope (varint + 20-byte dest addr + 8-byte
   timestamp) now implemented in `p2p/envelope.go`.
3. ~~Build `swap/` state machine core (join + two-confirmation progression +
   expiry), unit-tested.~~ ✅ done (`swap/`, `docs/swap.md`). `Session`/deposit
   layer (`trSigned`/`trCommited`, per-coin tx) still todo — needs `coins/` +
   `wallet/`.
4. ~~Build `coins/` foundation (coin registry, amount parsing, address
   codec).~~ ✅ done. ~~Add per-coin **tx construction** (UTXO tx model,
   serialization, HTLC deposit script, SIGHASH_ALL sign/verify).~~ ✅ done
   (`coins/`, `docs/coins.md`). Remaining: non-UTXO adapters (DCR/PART) and the
   `wallet/` RPC connector to drive the `swap` Session/deposit layer
   (`trSigned`/`trCommited`).
5. ~~Build `wallet/` connector to the connected SPV wallet (signs + broadcasts +
   pays fee).~~ ✅ done (`wallet/`, `docs/wallet.md`): `RPCConnector` (JSON-RPC
   to a Blocknet-core-compatible wallet) + `LocalConnector` (local keys +
   optional broadcast). Remaining: compose with `swap/` into a `Session`/deposit
   driver (`trSigned`/`trCommited`), non-UTXO adapters (DCR/PART), segwit BIP143
   sighash.
