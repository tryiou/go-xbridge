# Coins — per-coin metadata, amounts, addresses

`coins/` is the foundation the `swap` deposit/refund layer (and `wallet/`)
builds on. It is **stdlib-only** (like `proto/`), so it stays portable and
dependency-light. It covers the three things every chain interaction needs:

1. **Coin metadata** (`coin.go`) — ticker, name, decimal precision, and the
   base58check version bytes / bech32 HRP that identify addresses on that chain.
2. **Amount parsing** (`amount.go`) — decimal string ⇄ base units (satoshis,
   etc.), honoring each coin's `Decimals`.
3. **Address codec** (`base58*.go`, `bech32.go`, `address.go`) — decode/encode
   legacy (P2PKH/P2SH) and native segwit (P2WPKH/P2WSH/P2TR) addresses, and
   expose the 20-byte `swap.Addr` identifier the swap layer uses.

## Coin registry

Initial mainnet set (`Coins` in `coin.go`):

| Ticker | P2PKH | P2SH | bech32 HRP | segwit | decimals |
|--------|-------|------|------------|--------|----------|
| BTC    | 0x00  | 0x05 | `bc`       | yes    | 8 |
| LTC    | 0x30  | 0x32 | `ltc`      | yes    | 8 |
| DOGE   | 0x1e  | 0x16 | —          | no     | 8 |
| DGB    | 0x1e  | 0x3f | `dgb`      | yes    | 8 | **[VERIFY]**
| BLOCK  | 0x1a  | 0x1c | —          | no     | 8 |

BLOCK values are from `src/chainparams.cpp` (mainnet: `PUBKEY_ADDRESS = 26`,
`SCRIPT_ADDRESS = 28`; no `BECH32_PREFIX`, so no native segwit). BTC/LTC/DOGE
are standard. **DGB is marked `[VERIFY]`** — confirm against Digibyte's
chainparams before relying on it. Extend `Coins` for the rest of XBridge's
supported set (BCH, BCD, BTG, PART, DCR, DeVault, Stealth) as needed.

## Address model

`Address` carries the decoded form:

- `Kind` — P2PKH / P2SH / P2WPKH / P2WSH / P2TR.
- `Hash` — the identifier: 20 bytes for P2PKH/P2SH/P2WPKH, 32 bytes for
  P2WSH/P2TR.
- `ID() ([20]byte, bool)` — the `swap.Addr` (uint160) for kinds that fit in 20
  bytes (legacy + P2WPKH). Larger programs return `false`.

`Coin.DecodeAddress(s)` tries base58check (matching the coin's `P2PKH`/`P2SH`
version bytes) first, then native segwit (when `SegWit`). `Address.String()`
re-encodes. Tests use the Satoshi genesis P2PKH vector and the BIP173 bech32
vector (`BC1QW508D6…V8F3T4`) as external oracles.

## Not yet here

- **Transaction construction** (`xbitcointransaction*` in C++): building/serializing
  deposit + refund UTXO transactions per coin. That belongs in `coins/` once the
  wallet connector can sign.
- **Non-UTXO chains** (Decred, Particl) need their own adapters — out of scope
  for this foundation.
- Per-coin fee/utxo/dust rules.
