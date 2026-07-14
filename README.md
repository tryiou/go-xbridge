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
| `proto` | foundational | Packet header/body codec, command set, signing digest. Stdlib only; unit-tested. |
| `p2p`   | partial | Bitcoin P2P message framing + connection read/write; `version`/`verack` handshake implemented (`p2p/version.go`), **live-verify against a real node still TODO** (see `docs/protocol.md` §1.3). |
| `crypto`| wired + tested | `BtcSigner` over `btcd/btcec/v2`: 64-byte compact ECDSA over `Packet.Digest()`; round-trip + tamper tests pass. |
| `coins` | partial | Stdlib-only: coin registry + amount parsing + address codec (base58/bech32) **+ UTXO tx model, serialization, HTLC script, and SIGHASH_ALL signing/verification** (`script.go`, `tx.go`, `htlc.go`); unit-tested (`docs/coins.md`). Non-UTXO chains (DCR/PART) still todo. |
| `wallet`| partial | `Connector` contract + two impls: `RPCConnector` (JSON-RPC to a Blocknet-core-compatible wallet/node — `getnewaddress`, `listunspent`, `signrawtransactionwithwallet`, `sendrawtransaction`, `estimatesmartfee`) and `LocalConnector` (signs locally via a `LocalSigner`, optional `Broadcaster`). Unit-tested via httptest + a real HTLC sign/verify round-trip (`docs/wallet.md`). Address/UTXO/fee queries still flow from the connected wallet, not synthesized. |
| `swap`  | partial | `Transaction` state machine (port of `xbridgetransaction*`): join + two-confirmation progression + expiry; unit-tested. Session/deposit layer still todo. |
| `api`   | todo | `dx*` operations as Go calls (port of `rpcxbridge.cpp`). |

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
  api/       (todo) dx* API surface
```

## Next steps

1. ~~Wire `crypto.Signer` with `btcd/btcec/v2` and add a signing test.~~ ✅ done.
2. ~~Implement the Bitcoin `version`/`verack` handshake (`p2p/conn.go`,
   `p2p/version.go`).~~ ✅ done (unit-tested; **live-verify against a real node
   still TODO** — see `docs/protocol.md` §1.3).
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
