package swap

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

const coin = uint64(1_000_000) // XBridge COIN base units

func priceSource(counterpartyDest, source, dest uint64) uint64 {
	cda := counterpartyDest * coin
	sa := source * coin
	da := dest * coin
	ns := uint64(float64(cda)*(float64(sa)/float64(da))) + 1
	ns /= coin
	if ns < 1 {
		return 1
	}
	return ns
}

func priceDest(counterpartySource, source, dest uint64) uint64 {
	csa := counterpartySource * coin
	sa := source * coin
	da := dest * coin
	nd := uint64(float64(csa)*(float64(da)/float64(sa))) + 1
	nd /= coin
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
