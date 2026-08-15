package coins

import (
	"errors"

	xlog "go-xbridge/log"
)

// AddressKind enumerates the script/address types XBridge deposits use.
type AddressKind int

const (
	// P2PKH is a legacy pay-to-pubkey-hash (base58check, version = coin.P2PKH).
	P2PKH AddressKind = iota
	// P2SH is a legacy pay-to-script-hash (base58check, version = coin.P2SH).
	P2SH
	// P2WPKH is a native segwit v0 pay-to-witness-pubkey-hash (20-byte program).
	P2WPKH
	// P2WSH is native segwit v0 pay-to-witness-script-hash (32-byte program).
	P2WSH
	// P2TR is a taproot (segwit v1) address (32-byte program).
	P2TR
)

// Address is a decoded blockchain address for a specific coin.
type Address struct {
	Coin Coin
	Kind AddressKind
	// Prefix is the base58check version byte for legacy addresses.
	Prefix byte
	// Hash is the address identifier: 20 bytes for P2PKH/P2SH/P2WPKH, 32 bytes
	// for P2WSH/P2TR.
	Hash []byte
	// WitnessVersion is the segwit version (0 for P2WPKH/P2WSH, 1 for P2TR).
	WitnessVersion int
}

// ID returns the 20-byte uint160 identifier used by the swap layer for the
// envelope destination and session addresses (a 20-byte uint160). It is valid
// for P2PKH, P2SH, and P2WPKH; other kinds return false (their identifier
// exceeds 20 bytes).
func (a Address) ID() ([20]byte, bool) {
	if len(a.Hash) != 20 {
		return [20]byte{}, false
	}
	var out [20]byte
	copy(out[:], a.Hash)
	return out, true
}

// String re-encodes the address to its canonical string form.
func (a Address) String() string {
	switch a.Kind {
	case P2PKH, P2SH:
		if a.Coin.Family() == FamilyUTXOBCH {
			typ := 0
			if a.Kind == P2SH {
				typ = 1
			}
			s, err := cashaddrEncode(a.Coin.CashAddrPrefix, typ, a.Hash)
			if err != nil {
				return ""
			}
			return s
		}
		return base58CheckEncode(a.Prefix, a.Hash)
	case P2WPKH, P2WSH, P2TR:
		s, err := bech32Encode(a.Coin.Bech32HRP, a.WitnessVersion, a.Hash)
		if err != nil {
			return ""
		}
		return s
	default:
		return ""
	}
}

// DecodeAddress decodes an address string for coin c, detecting legacy
// base58check (P2PKH/P2SH), native segwit (bech32/bech32m), and — for the
// Bitcoin Cash family — CashAddr.
func (c Coin) DecodeAddress(s string) (Address, error) {
	a, err := c.decodeAddress(s)
	if err != nil {
		xlog.Debug("coins: address decode failed", "coin", c.Ticker, "addr", s, "err", err)
	}
	return a, err
}

func (c Coin) decodeAddress(s string) (Address, error) {
	if s == "" {
		return Address{}, errors.New("coins: empty address")
	}
	// Bitcoin Cash uses CashAddr exclusively; its legacy base58check version
	// byte collides with BTC's, so we decode CashAddr first and reject anything
	// else for that family.
	if c.Family() == FamilyUTXOBCH {
		typ, h, err := cashaddrDecode(s, c.CashAddrPrefix)
		if err != nil {
			return Address{}, err
		}
		kind := P2PKH
		if typ == 1 {
			kind = P2SH
		}
		return Address{Coin: c, Kind: kind, Hash: h}, nil
	}
	// Try legacy base58check first.
	if prefix, payload, err := base58CheckDecode(s); err == nil {
		switch prefix {
		case c.P2PKH:
			return Address{Coin: c, Kind: P2PKH, Prefix: prefix, Hash: payload}, nil
		case c.P2SH:
			return Address{Coin: c, Kind: P2SH, Prefix: prefix, Hash: payload}, nil
		default:
			return Address{}, errors.New("coins: address version byte does not match coin")
		}
	}
	// Try native segwit.
	if c.SegWit && c.Bech32HRP != "" {
		hrp, ver, prog, err := bech32Decode(s)
		if err == nil && hrp == c.Bech32HRP {
			switch {
			case ver == 0 && len(prog) == 20:
				return Address{Coin: c, Kind: P2WPKH, Hash: prog, WitnessVersion: 0}, nil
			case ver == 0 && len(prog) == 32:
				return Address{Coin: c, Kind: P2WSH, Hash: prog, WitnessVersion: 0}, nil
			case ver == 1 && len(prog) == 32:
				return Address{Coin: c, Kind: P2TR, Hash: prog, WitnessVersion: 1}, nil
			default:
				return Address{}, errors.New("coins: unsupported segwit program")
			}
		}
	}
	return Address{}, errors.New("coins: address is not valid for " + c.Ticker)
}
