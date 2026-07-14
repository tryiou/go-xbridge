package coins

import (
	"errors"
	"math/big"
	"strings"
)

// ParseAmount converts a human decimal amount string (e.g. "1.5") for coin c
// into base units (e.g. satoshis). It requires the fractional part to fit within
// c.Decimals places; extra precision is rejected rather than silently rounded.
func ParseAmount(c Coin, s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("coins: empty amount")
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}

	intPart, fracPart := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart = s[:dot]
		fracPart = s[dot+1:]
	}
	if fracPart != "" && strings.IndexByte(fracPart, '.') >= 0 {
		return 0, errors.New("coins: multiple decimal points")
	}
	if len(fracPart) > c.Decimals {
		return 0, errors.New("coins: too many decimal places")
	}

	// Scale: base = intPart * 10^decimals + fracPart padded to decimals.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(c.Decimals)), nil)
	intVal, ok := new(big.Int).SetString(intPart, 10)
	if !ok && intPart != "" {
		return 0, errors.New("coins: invalid integer part")
	}
	if intPart == "" {
		intVal = big.NewInt(0)
	}
	total := new(big.Int).Mul(intVal, scale)
	if fracPart != "" {
		fracVal, ok := new(big.Int).SetString(fracPart, 10)
		if !ok {
			return 0, errors.New("coins: invalid fractional part")
		}
		// Pad fracPart on the right to c.Decimals places.
		fracScale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(c.Decimals-len(fracPart))), nil)
		total.Add(total, new(big.Int).Mul(fracVal, fracScale))
	}
	if neg {
		total.Neg(total)
	}
	if !total.IsUint64() {
		return 0, errors.New("coins: amount overflows uint64")
	}
	return total.Uint64(), nil
}

// FormatAmount renders base units (e.g. satoshis) as a decimal string for coin
// c, trimming trailing zeros and a dangling decimal point.
func FormatAmount(c Coin, v uint64) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(c.Decimals)), nil)
	q := new(big.Int)
	r := new(big.Int)
	q.QuoRem(new(big.Int).SetUint64(v), scale, r)
	intStr := q.String()
	if r.Sign() == 0 {
		return intStr
	}
	frac := r.String()
	// Left-pad the fractional part with zeros to c.Decimals, then trim trailing.
	for len(frac) < c.Decimals {
		frac = "0" + frac
	}
	frac = strings.TrimRight(frac, "0")
	return intStr + "." + frac
}
