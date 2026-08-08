package engine

// Decimal rounding for Intl.NumberFormat: the nine rounding modes, the
// rounding increment, and trailingZeroDisplay.
//
// It is done on the digit string rather than on the float, because that is
// what the specification is written against and because the float cannot
// answer the question: 1.005 is not 1.005 in binary, and "round 1.005 to two
// places, half away from zero" has one right answer whatever the nearest
// double happens to be. numberToString has already produced the shortest
// round-trip digits, which is the decimal the caller wrote.

import (
	"math/big"
	"strings"
)

// roundingModes is the set the option accepts, in the specification's order.
var roundingModes = []string{
	"ceil", "floor", "expand", "trunc",
	"halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven",
}

// validRoundingIncrements is the closed set the option accepts. They are the
// increments that divide a power of ten evenly enough to be useful for prices.
var validRoundingIncrements = []int{
	1, 2, 5, 10, 20, 25, 50, 100, 200, 250, 500,
	1000, 2000, 2500, 5000,
}

// roundDecimal rounds the digit string (intPart, frac) to `places` fraction
// digits, in multiples of `inc` at that scale, under the given mode. neg says
// which way "ceil" and "floor" point; the digits themselves are unsigned.
//
// It returns the rounded integer and fraction digits, the fraction padded to
// exactly `places` so the caller can decide what to trim.
func roundDecimal(intPart, frac string, places int, mode string, inc int, neg bool) (string, string) {
	if inc < 1 {
		inc = 1
	}
	// Scale to an integer: everything up to `places` fraction digits becomes
	// the quotient, everything after it the remainder that decides the round.
	for len(frac) < places {
		frac += "0"
	}
	kept := intPart + frac[:places]
	rest := frac[places:]

	n, ok := new(big.Int).SetString(strings.TrimLeft(kept, "0")+"", 10)
	if !ok || strings.TrimLeft(kept, "0") == "" {
		n = big.NewInt(0)
	}
	incN := big.NewInt(int64(inc))

	// The remainder to compare against half is the tail digits plus whatever
	// `n` has beyond a multiple of the increment. Both are expressed over a
	// common denominator: rest as a fraction of 10^len(rest), scaled by inc.
	q, r := new(big.Int).QuoRem(n, incN, new(big.Int))
	// remainder = r * 10^len(rest) + rest, over denominator inc * 10^len(rest)
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(rest))), nil)
	remainder := new(big.Int).Mul(r, pow)
	if rest != "" {
		if tail, ok := new(big.Int).SetString(rest, 10); ok {
			remainder.Add(remainder, tail)
		}
	}
	denom := new(big.Int).Mul(incN, pow)

	if remainder.Sign() != 0 {
		twice := new(big.Int).Lsh(remainder, 1) // 2 * remainder, to compare with denom
		cmp := twice.Cmp(denom)
		up := false
		switch mode {
		case "ceil":
			up = !neg
		case "floor":
			up = neg
		case "expand":
			up = true
		case "trunc":
			up = false
		case "halfCeil":
			up = cmp > 0 || (cmp == 0 && !neg)
		case "halfFloor":
			up = cmp > 0 || (cmp == 0 && neg)
		case "halfTrunc":
			up = cmp > 0
		case "halfEven":
			up = cmp > 0 || (cmp == 0 && q.Bit(0) == 1)
		default: // halfExpand
			up = cmp >= 0
		}
		if up {
			q.Add(q, big.NewInt(1))
		}
	}
	n = q.Mul(q, incN)

	digits := n.String()
	for len(digits) <= places {
		digits = "0" + digits
	}
	return digits[:len(digits)-places], digits[len(digits)-places:]
}

// trimTrailingZeros is trailingZeroDisplay: "auto" keeps the padding the
// minimum-fraction-digits option asked for, "stripIfInteger" drops it when
// there is nothing but zeros after the point.
func trimTrailingZeros(frac string, display string) string {
	if display == "stripIfInteger" && strings.Trim(frac, "0") == "" {
		return ""
	}
	return frac
}
