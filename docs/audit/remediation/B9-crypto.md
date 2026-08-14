# B9 — Crypto-connector parity (CRYPTO-F77, F82, F83, F88, F89, F91, F92)

Branch: `fix/crypto-connectors` (off `main` @ B7 merge `dedbb1e`).
Status: MERGED into `main` @ `2d076a3` (fast-forward).
C++ reference: Blocknet Core @ `e9ddbc2bd` (v4.4.1 era).
Go subject: `coins/tx.go`, `coins/coin.go`, `coins/coins_test.go`,
`crypto/signer.go`, `api/swap.go`, `swap/deposit.go`, `wallet/rpc.go` (7 OPEN
findings, plus `CRYPTO-F98`/`CRYPTO-F99` documented residuals; per
`register.md` Owner B9).

The forkid-sighash + per-coin-connector pass. The headline item is the S1
CRYPTO-F77: a locally-signed BCH refund/claim committed the legacy `0x01`
sighash, which BCH nodes reject — fund-loss risk on a live manifest coin. The
branch generalizes the BIP143 digest, selects the digest by the coin's
`CreateTxMethod`-derived descriptor, and fixes four coin-agnostic divergences.

## Findings resolved on this branch

| ID | Title | C++ source of truth | Go call sites → fix |
|---|---|---|---|
| F77 | BCH forkid sighash missing in local signing (S1) | `xbridgewalletconnectorbch.cpp:391-406` — `SigHashType(SIGHASH_ALL).withForkId()` = `0x41`; digest via the BIP143 branch of `SignatureHash` (`:191-256`, no nTime); `push_back(0x41)`. Live mainnet replay protection (`:203-209,497-499`, median ≥ 1605441600) rewrites the fork value to `0xffdead` → digest hashType `0xffdead41`, DER byte still `0x41`. BTG fork value 79 (`btg.cpp:69`) → digest `0x4F41` | `coins/tx.go` — new `HashForSigningBIP143(idx, scriptCode, amount, hashType)`, `HashForSigningForkID`, `SignTxInputForkID`, `VerifyTxInputForkID`, `SignTxInputForCoin`; `coins/coin.go` `SignatureKind`/`ForkValue` (BCH `0xffdead`, DEVAULT `0`, BTG `79`); `api/swap.go:1507` `buildRefundTx` commits the recorded deposit P2SH value, `:1582` `redeemCounterparty` the validated deposit amount; `swap/deposit.go` `SignInput` dispatches |
| F82 | RNG top-bit bias in private-key generation (S4) | `xbridgecryptoproviderbtc.cpp:204-210` `makeNewKey` — full-range `GetStrongRandBytes` + `secp256k1_ec_seckey_verify` retry | `crypto/signer.go` `NewPrivateKey` — full 256-bit range, redraw into [1, N-1] (bounded loop) |
| F83 | Block-hash byte order unverified (S3) | `base_blob<256>::SetHex` (`uint256.cpp:27-53`) — internal bytes = display bytes REVERSED (last hex pair in `data[0]`) | `wallet/rpc.go` `revHashHex` pinned against the Bitcoin genesis hash; parity oracle transcribes `SetHex` |
| F88 | Segwit/BIP143 signing dead code (S3) | `bch.cpp`/`btg.cpp` forkid digests are the BIP143 preimage | BIP143 is now LIVE — the base of `HashForSigningForkID`. The "bech32 re-encoded legacy" sub-claim is unsubstantiated (only bech32 usage is the per-coin segwit address codec, `coins/address.go`) |
| F89 | Coin-family misclassification (S3) | `xbridgeapp.cpp:1047-1086` connector dispatch; `btg.cpp:69` (fork 79 + bech32 `btg`); `devault.cpp:171,274-280` (BCH family, cashaddr `devault`, replay protection disabled) | `coins/coin.go` descriptor tables; BTG/DEVAULT address codecs pinned (`TestBTGAddressRoundTrip`, `TestDevaultAddressCashaddr`) |
| F91 | `signrawtransaction` payload: `"ALL"` in privkeys slot (S3) | `xbridgewalletconnectorbtc.cpp:1055-1089` — `[rawtx, prevtxs|null, keys|null]`, privkeys always null | `wallet/rpc.go` `SignRawTransaction` args (same for the `signrawtransactionwithwallet` fallback) |
| F92 | `secretFromPayTx` scanned only input 0 (S3) | `xbridgewalletconnectorbtc.cpp:2241-2276` `getSecretFromPaymentTransaction` iterates every vin's scriptSig | `api/swap.go` `secretFromPayTx` scans all inputs |

## Documented residuals (new register rows)

| ID | Title | Why non-portable |
|---|---|---|
| F98 | PART (Particl) connector | `XParticlTransaction` serialization (0xA0 version marker), confidential outputs (`vpout` vector-of-pointers), amount-committing digest (`xbridgewalletconnectorpart.cpp:113-195`). A thin client broadcasting a BTC-format PART tx puts malformed bytes on-chain. One manifest conf (`particl--v0.19.2.5.conf`) |
| F99 | BCD (Bitcoin Diamond) connector | `BCDTransaction` writes an extra `preBlockHash` (uint256) field when `nVersion == CURRENT_VERSION_FORK` (`xbridgewalletconnectorbcd.cpp:106-108`) — `coins.Tx` has no conditional-serialize flag. One manifest conf (`bitcoindiamond--v1.3.0.conf`) |

DCR: not in the live manifest (no `[DCR]` in `blockchain-configuration-files`),
so its distinct session/tx model was dropped from scope rather than
documented. A follow-up would need `dcrwallet`-specific RPC + a Decred tx
codec.

## Forkid digest reference

For forkid coins the HTLC refund/claim digest is the BIP143 preimage with the
sighash type committed as the trailing 4-byte field:

```
hashType = (forkValue << 8) | SIGHASH_FORKID | SIGHASH_ALL
         = (forkValue << 8) | 0x41
preimage = version ‖ hashPrevouts ‖ hashSequence ‖ outpoint ‖
           scriptCode ‖ amount ‖ nSequence ‖ hashOutputs ‖ locktime ‖ hashType
digest   = dSHA256(preimage)      # no nTime, matching the C++ forkid SignatureHash
```

| Coin | forkValue | digest hashType | DER byte |
|---|---|---|---|
| BCH (live mainnet) | `0xffdead` | `0xffdead41` | `0x41` |
| DEVAULT | `0` | `0x41` | `0x41` |
| BTG | `79` | `0x4F41` | `0x41` |

The committed `amount` is the exact spent output value: the deposit P2SH
(`outAmount + fee2`) for the refund, the validated counterparty deposit amount
(`oBinTxP2SHAmount`) for the claim — recorded structurally (`ourDepositP2SH`),
never recomputed.

## Golden vectors

- `coins/tx_test.go` — `TestHashForSigningForkID` (0x41/0x4F41, differs from
  plain SIGHASH_ALL), `TestSignTxInputForkIDRoundTrip` (byte 0x41, amount
  tamper), `TestSignTxInputForCoinDispatch` (per-coin, cross-fork rejection),
  `TestHashForSigningBIP143MatchesSegwit` (parameterization == old segwit).
- `api/swap_test.go` — `TestBCHRefundForkidSigned`: drives a BCH maker through
  CreateA and verifies the refund against fork value `0xffdead` and the
  broadcast deposit's P2SH value.
- `wallet/rpc_test.go` — `TestSignRawTransactionPayloadMatchesCpp`,
  `TestRevHashHexCapturedBlockHash`.
- `crypto/signer_test.go` — `TestNewPrivateKeyFullRange`.
- `tools/parity` oracle — `forkidSignatureHash` (BCH/BCH-replay/BTG digests)
  and `blockHashByteOrder` (genesis internal bytes) under
  `parity/oracle/main.cpp`; `TestForkidSignatureHashMatchesCpp` and
  `TestBlockHashByteOrderMatchesCpp` in `parity/go/parity_value_test.go`.

## Verify

- `gofmt -l .` clean; `go build ./...`, `go vet ./...`, `go test ./...`,
  `go test -count=1 -race ./api/... ./coins/... ./swap/... ./wallet/...`
  `./crypto/...`.
- `make parity` (A–F) and `make canary` green from `tools/`.
