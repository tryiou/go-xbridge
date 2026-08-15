package swap

import "math"

// Port of src/xbridge/util/xutil.cpp price-integrity helpers used by
// Transaction::tryJoin for partial orders. XBridge amounts are base units
// of COIN = 1_000_000 (6 decimals of coin value); the helpers derive
// the counterparty's expected amounts from the quoted price and compare
// them with a small satoshi-level drift tolerance
// (xBridgePartialOrderDriftCheck).
//
// The C++ math (CAmount is int64; here amounts are non-negative
// uint64, which is functionally equivalent for trade amounts) is:
//
//	xBridgeSourceAmountFromPrice(counterpartyDest, source, dest):
//	  c = 1000000
//	  cda = counterpartyDest * c
//	  sa  = source * c
//	  da  = dest * c
//	  new = cda * (double(sa)/double(da)) + 1
//	  new /= c            // integer division
//	  return new < 1 ? 1 : new
//
//	xBridgeDestAmountFromPrice(counterpartySource, source, dest):
//	  c = 1000000
//	  csa = counterpartySource * c
//	  sa  = source * c
//	  da  = dest * c
//	  new = csa * (double(da)/double(sa)) + 1
//	  new /= c
//	  return new < 1 ? 1 : new
//
// The Go ports scale each operand to float64 BEFORE multiplying by c: a
// wire-controlled amount up to maxXSize (1e14) cannot wrap the uint64
// multiply (C++ uses a CAmount int64 here, but scaling in double preserves
// the result for every non-overflowing amount and stays defined beyond it),
// and mirror C++'s normalize ordering exactly — the +1'd double is truncated
// to an integer BEFORE the integer /c (xutil.cpp:331-332), so a derived
// amount within ~1 ulp below an integer multiple of c lands in the same
// bucket as C++.

const coin = uint64(1_000_000) // XBridge COIN base units

func priceSource(counterpartyDest, source, dest uint64) uint64 {
	// A zero denominator is an invalid input (C++ would divide by zero, UB);
	// return 0 as the defined sentinel, distinct from the <1→1 floor below
	// which normalizes a legitimate near-zero ratio.
	if dest == 0 {
		return 0
	}
	c := float64(coin)
	cda := float64(counterpartyDest) * c
	sa := float64(source) * c
	da := float64(dest) * c
	v := cda*(sa/da) + 1.0
	var ns uint64
	if v < float64(math.MaxInt64) {
		ns = uint64(v) / coin
	} else if q := v / c; q < float64(^uint64(0)) {
		ns = uint64(q)
	} else {
		ns = ^uint64(0)
	}
	if ns < 1 {
		return 1
	}
	return ns
}

func priceDest(counterpartySource, source, dest uint64) uint64 {
	// A zero denominator is an invalid input (C++ would divide by zero, UB);
	// return 0 as the defined sentinel, distinct from the <1→1 floor below
	// which normalizes a legitimate near-zero ratio.
	if source == 0 {
		return 0
	}
	c := float64(coin)
	csa := float64(counterpartySource) * c
	sa := float64(source) * c
	da := float64(dest) * c
	v := csa*(da/sa) + 1.0
	var nd uint64
	if v < float64(math.MaxInt64) {
		nd = uint64(v) / coin
	} else if q := v / c; q < float64(^uint64(0)) {
		nd = uint64(q)
	} else {
		nd = ^uint64(0)
	}
	if nd < 1 {
		return 1
	}
	return nd
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// PartialOrderDriftCheck mirrors xBridgePartialOrderDriftCheck
// (src/xbridge/util/xutil.cpp:338). makerSource/makerDest are the maker's
// from/to amounts; otherSource/otherDest are the taker's. Exact orders
// always pass; partial orders pass when the derived prices agree exactly,
// or — when the amounts are not evenly divisible — when the taker
// amounts fall within a 1-satoshi drift band of the maker's quoted
// price.
func PartialOrderDriftCheck(makerSource, makerDest, otherSource, otherDest uint64) bool {
	// A zero taker amount is an invalid hold (C++ divides by zero in the
	// divisibility check below, xutil.cpp:356, which is UB and typically a
	// SIGFPE crash on a hostile wire hold). Reject it as a price mismatch
	// instead of panicking — a hardening divergence from C++'s undefined
	// behavior.
	if otherSource == 0 || otherDest == 0 {
		return false
	}
	// Exact order always succeeds.
	if makerSource == otherDest && makerDest == otherSource {
		return true
	}
	checkSourceAmount := priceSource(makerDest, otherDest, otherSource)
	checkDestAmount := priceDest(makerSource, otherDest, otherSource)
	checkSourceAmountOther := priceSource(otherDest, makerDest, makerSource)
	checkDestAmountOther := priceDest(otherSource, makerDest, makerSource)

	if makerSource%otherDest == 0 && makerDest%otherSource == 0 {
		if checkSourceAmountOther != otherSource ||
			checkDestAmountOther != otherDest ||
			checkSourceAmount != makerSource ||
			checkDestAmount != makerDest {
			return false
		}
		return true
	}
	if checkSourceAmountOther != otherSource ||
		checkDestAmountOther != otherDest ||
		checkSourceAmount != makerSource ||
		checkDestAmount != makerDest {
		driftTakerSourceA := priceSource(otherDest+1, makerDest, makerSource)
		driftTakerSourceB := priceSource(otherDest-1, makerDest, makerSource)
		upperS := max64(driftTakerSourceA, driftTakerSourceB)
		lowerS := min64(driftTakerSourceA, driftTakerSourceB)
		if otherSource > upperS || otherSource < lowerS {
			return false
		}
		driftTakerDestA := priceDest(otherSource+1, makerDest, makerSource)
		driftTakerDestB := priceDest(otherSource-1, makerDest, makerSource)
		upperD := max64(driftTakerDestA, driftTakerDestB)
		lowerD := min64(driftTakerDestA, driftTakerDestB)
		if otherDest > upperD || otherDest < lowerD {
			return false
		}
	}
	return true
}
