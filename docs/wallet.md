# Wallet — connector to the connected SPV wallet

`wallet/` is the bridge between the `coins` transaction-construction layer and
the user's own wallet. The library never holds BLOCK or pays fees itself; the
**connected SPV wallet** (or any Blocknet-core-compatible node) does — it holds
the keys, signs, broadcasts, and (for BLOCK) pays the service-node fee via core
RPC. `wallet/` defines the contract and two implementations.

> Transaction *construction* (deposit/refund/payment scripts, HTLC redeem
> logic) lives in `coins/`. `wallet/` only signs and moves bytes.

## Connector contract

```go
type Connector interface {
    Ticker() string
    GetNewAddress() (string, error)
    ListUnspent(minConf int) ([]Utxo, error)
    SignRawTransaction(txHex string, prevTxs []PrevTx) (signedHex string, complete bool, err error)
    SendRawTransaction(txHex string) (txid string, err error)
    EstimateFee(confTarget int) (uint64, error) // sat/vB
}
```

`Utxo` is a spendable output (txid, vout, address, amount, scriptPubKey,
confirmations). `PrevTx` is the per-input previous output the wallet needs to
sign: `txid`, `vout`, `scriptPubKey`, `amount` (in base units).

## RPCConnector

`RPCConnector` drives a single coin over a Bitcoin-Core-style JSON-RPC 1.0
client (`RPCClient`: HTTP + basic auth, `{"jsonrpc":"1.0","id","method",
"params"}` / `{"result","error","id"}`).

| method                       | RPC call                          | notes |
|------------------------------|-----------------------------------|-------|
| `GetNewAddress`              | `getnewaddress`                   | native segwit (`bech32`) when the coin supports it, else wallet default |
| `ListUnspent`                | `listunspent minconf 9999999 nil true` | float amount → base units via `coins.ParseAmount` |
| `SignRawTransaction`         | `signrawtransactionwithwallet`    | `prevTxs` carry each input's `scriptPubKey`+`amount`; returns signed hex + `complete` |
| `SendRawTransaction`         | `sendrawtransaction`              | returns network txid |
| `EstimateFee`                | `estimatesmartfee confTarget ECONOMICAL` | `feerate` (BTC/kvB) → sat/vB |

Amounts cross the JSON boundary as floats (`listunspent` emits them;
`signrawtransactionwithwallet` needs them per-input). To avoid IEEE-754 drift,
`wallet` converts via string formatting: base units →
`coins.FormatAmount` → `ParseFloat`, and the reverse →
`strconv.FormatFloat` → `coins.ParseAmount`.

## LocalConnector

`LocalConnector` implements `Connector` for the case where the library itself
holds the keys (no external wallet to delegate to). It signs each input with a
caller-supplied `LocalSigner`:

```go
type LocalSigner interface {
    SignInput(tx *coins.Tx, idx int, prev PrevTx) ([]byte, error) // returns full scriptSig
}
```

`SignRawTransaction` parses the hex (`coins.Deserialize`), asks the signer for
each input's finalized scriptSig (e.g. via `coins.BuildRefundScriptSig` /
`BuildPaymentScriptSig`), and re-serializes. An optional `Broadcaster`
(`func(txHex string)(txid string,err error)`) backs `SendRawTransaction`; a nil
broadcaster makes the connector sign-only (offline/test signing).
`GetNewAddress` / `ListUnspent` / `EstimateFee` have no local source and return
an error — the deposit flow supplies inputs and `prevTxs` explicitly instead.

## Tests

- `rpc_test.go` — `httptest` JSON-RPC mock (checks basic auth, answers each
  method); asserts `GetNewAddress`, `ListUnspent` (1.5 BTC → 150000000 sat),
  `SignRawTransaction` (`complete=true`), `SendRawTransaction`, `EstimateFee`
  (0.0001 BTC/kB → 10 sat/vB), plus an unauthorized case.
- `local_test.go` — builds a real P2SH HTLC deposit (`coins.BuildDepositUnlockScript`
  + P2SH), signs the refund branch through `LocalConnector`, then verifies the
  embedded signature with `coins.VerifyTxInput` and confirms tampering fails.

## Not yet here

- **Non-UTXO adapters** (Decred, Particl) — need their own `Connector` shapes.
- **Live-network execution** — the deposit/refund/payment driver is built and
  unit-tested against mocks (see `swap.md` §6–7); end-to-end execution against a
  real C++ service node still needs a live wallet and is the remaining gap.
