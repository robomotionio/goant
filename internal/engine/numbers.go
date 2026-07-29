package engine

// Port of the JS number⇄string conversions (ant src/numbers.cc). These are a
// frequent conformance pitfall, so the ECMAScript Number::toString algorithm
// (spec 6.1.6.1.20) and ToNumber-on-strings (StrNumericLiteral) are implemented
// exactly rather than deferred to Go's default formatting.

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// numberToString implements ECMAScript Number::toString(x) for radix 10.
func numberToString(d float64) string {
	switch {
	case math.IsNaN(d):
		return "NaN"
	case d == 0:
		return "0" // covers both +0 and -0
	case d < 0:
		return "-" + numberToString(-d)
	case math.IsInf(d, 1):
		return "Infinity"
	}

	// Shortest round-tripping digits via strconv's 'e' form: "d.dddde±XX".
	// digits = the significant digits, n = decimal-point position such that
	// value = digits × 10^(n-k), k = len(digits) (spec variables s, k, n).
	e := strconv.FormatFloat(d, 'e', -1, 64)
	mantissa, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mantissa, ".", "", 1)
	k := len(digits)
	n := exp + 1

	switch {
	case k <= n && n <= 21:
		// integer with trailing zeros
		return digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return "0." + strings.Repeat("0", -n) + digits
	default:
		// exponential notation
		var b strings.Builder
		b.WriteByte(digits[0])
		if k > 1 {
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		b.WriteByte('e')
		ep := n - 1
		if ep >= 0 {
			b.WriteByte('+')
		} else {
			b.WriteByte('-')
			ep = -ep
		}
		b.WriteString(strconv.Itoa(ep))
		return b.String()
	}
}

// numberToStringRadix implements Number.prototype.toString(radix) for
// radix 2..36 (radix 10 delegates to numberToString).
func numberToStringRadix(d float64, radix int) string {
	if radix == 10 {
		return numberToString(d)
	}
	switch {
	case math.IsNaN(d):
		return "NaN"
	case math.IsInf(d, 1):
		return "Infinity"
	case math.IsInf(d, -1):
		return "-Infinity"
	case d == 0:
		return "0"
	}
	neg := d < 0
	d = math.Abs(d)

	const digitChars = "0123456789abcdefghijklmnopqrstuvwxyz"
	intPart := math.Floor(d)
	frac := d - intPart

	// Integer part.
	var ip []byte
	if intPart == 0 {
		ip = []byte{'0'}
	}
	for intPart > 0 {
		rem := int(math.Mod(intPart, float64(radix)))
		ip = append([]byte{digitChars[rem]}, ip...)
		intPart = math.Floor(intPart / float64(radix))
	}

	out := string(ip)
	// Fractional part — emit up to ~1100 digits, matching typical engine caps.
	if frac > 0 {
		var fb strings.Builder
		fb.WriteByte('.')
		for i := 0; i < 1100 && frac > 0; i++ {
			frac *= float64(radix)
			dig := int(math.Floor(frac))
			fb.WriteByte(digitChars[dig])
			frac -= math.Floor(frac)
		}
		out += fb.String()
	}
	if neg {
		return "-" + out
	}
	return out
}

// jsStrWhitespace reports whether r is ECMAScript StrWhiteSpace / LineTerminator
// (used to trim ToNumber string input).
func jsStrWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0xFEFF,
		0x2028, 0x2029, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004,
		0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200A, 0x202F, 0x205F, 0x3000:
		return true
	}
	return false
}

// stringToNumber implements ECMAScript ToNumber applied to a String value
// (StringNumericLiteral). Returns NaN on any invalid input.
func stringToNumber(s string) float64 {
	s = strings.TrimFunc(s, jsStrWhitespace)
	if s == "" {
		return 0
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	// Radix prefixes (no sign allowed).
	if len(s) > 2 && s[0] == '0' {
		switch s[1] {
		case 'x', 'X':
			return parseRadixString(s[2:], 16)
		case 'o', 'O':
			return parseRadixString(s[2:], 8)
		case 'b', 'B':
			return parseRadixString(s[2:], 2)
		}
	}
	// Decimal literal. Reject forms Go accepts but JS does not.
	if !validJSDecimal(s) {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// Overflow to ±Inf is the correct JS result, not NaN.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return v
		}
		return math.NaN()
	}
	return v
}

func parseRadixString(s string, radix int) float64 {
	if s == "" {
		return math.NaN()
	}
	// Accumulating in a float64 (val = val*radix + d) rounds at every digit once
	// the value passes 2^53, and those roundings compound: 0x1000000000000081 is
	// 2^60+129, which must round up to 2^60+256 — the nearest double — but came
	// out as 2^60, off by a full ULP. The exact integer has to be built first and
	// rounded once, at the end.
	//
	// A uint64 is exact while the digits cannot overflow it, and Go's
	// uint64->float64 conversion rounds to nearest even, which is the rule the
	// spec wants. Only a longer literal needs the slow path.
	var bits int
	switch radix {
	case 2:
		bits = 1
	case 8:
		bits = 3
	case 16:
		bits = 4
	}
	if bits != 0 && len(s)*bits <= 64 {
		var u uint64
		for i := 0; i < len(s); i++ {
			d := digitVal(s[i])
			if d < 0 || d >= radix || !isRadixDigit(s[i], radix) {
				return math.NaN()
			}
			u = u<<uint(bits) | uint64(d)
		}
		return float64(u)
	}
	acc := new(big.Int)
	base := big.NewInt(int64(radix))
	for i := 0; i < len(s); i++ {
		d := digitVal(s[i])
		if d < 0 || d >= radix || !isRadixDigit(s[i], radix) {
			return math.NaN()
		}
		acc.Mul(acc, base)
		acc.Add(acc, big.NewInt(int64(d)))
	}
	// Float64 rounds to nearest even and reports ±Inf on overflow, which is the
	// correct result for a literal too large to represent.
	f, _ := new(big.Float).SetInt(acc).Float64()
	return f
}

func isRadixDigit(c byte, radix int) bool {
	d := digitVal(c)
	if d < 0 || d >= radix {
		return false
	}
	// digitVal doesn't reject non-alphanumeric; validate the char class.
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// toExponentialStr implements Number.prototype.toExponential. When hasDigits is
// false the shortest round-tripping fraction is used.
func toExponentialStr(x float64, fracDigits int, hasDigits bool) string {
	if math.IsNaN(x) {
		return "NaN"
	}
	if math.IsInf(x, 1) {
		return "Infinity"
	}
	if math.IsInf(x, -1) {
		return "-Infinity"
	}
	if x == 0 {
		x = 0 // -0 formats without a sign in toExponential (sign is x < 0)
	}
	if !hasDigits {
		return fixExponent(strconv.FormatFloat(x, 'e', -1, 64))
	}
	neg := math.Signbit(x)
	ax := math.Abs(x)
	if ax == 0 {
		m := "0"
		if fracDigits > 0 {
			m = "0." + strings.Repeat("0", fracDigits)
		}
		out := m + "e+0"
		if neg {
			return "-" + out
		}
		return out
	}
	// Exact decimal exponent from the shortest round-trip representation (avoids
	// math.Log10 boundary error), then exact round-half-away via big.Rat so ties
	// resolve to the larger magnitude (ECMAScript 21.1.3.2).
	_, expStr, _ := strings.Cut(strconv.FormatFloat(ax, 'e', -1, 64), "e")
	e, _ := strconv.Atoi(expStr)
	r := new(big.Rat).SetFloat64(ax)
	scale := fracDigits - e
	pow := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absInt(scale))), nil))
	if scale >= 0 {
		r.Mul(r, pow)
	} else {
		r.Quo(r, pow)
	}
	n := ratRoundHalfAway(r)
	digits := n.String()
	if len(digits) > fracDigits+1 { // rounded up an extra digit (e.g. 9.99 -> 10)
		e += len(digits) - (fracDigits + 1)
		digits = digits[:fracDigits+1]
	}
	m := digits[:1]
	if fracDigits > 0 {
		m += "." + digits[1:]
	}
	out := m + "e" + expSign(e) + strconv.Itoa(absInt(e))
	if neg {
		return "-" + out
	}
	return out
}

// ratRoundHalfAway rounds a non-negative rational to the nearest integer, ties
// away from zero.
func ratRoundHalfAway(r *big.Rat) *big.Int {
	num := new(big.Int).Set(r.Num())
	den := r.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	twice := new(big.Int).Lsh(rem, 1) // 2*rem
	if twice.Cmp(den) >= 0 {          // fractional part >= 1/2 -> round up
		q.Add(q, big.NewInt(1))
	}
	return q
}

// ratPow10 returns 10^n as an exact rational (n may be negative).
func ratPow10(n int) *big.Rat {
	neg := n < 0
	if neg {
		n = -n
	}
	p := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil))
	if neg {
		p.Inv(p)
	}
	return p
}

// roundSignificant returns the sig-significant-digit decimal for x > 0 as a
// digit string of exactly sig characters, plus the exponent e such that the
// value is 0.<digits> x 10^(e+1) — that is, the first digit has place value
// 10^e.
//
// toExponential and toPrecision both say: "let n be an integer for which
// n / 10^(e-p+1) - x is as close to zero as possible; if there are two such n,
// pick the larger n". Go's FormatFloat rounds half to even instead, so it
// disagrees on every exact tie — (1.25).toPrecision(2) is "1.3", not "1.2".
// x is a double, so the tie is a fact about its exact binary value and has to be
// decided in exact arithmetic.
func roundSignificant(x float64, sig int) (string, int) {
	r := new(big.Rat).SetFloat64(x)
	// e must be the exact floor(log10(x)), not the exponent of the shortest
	// round-trip form. They differ just below a power of ten: the double nearest
	// 1e-21 is a shade under it, so the shortest form says -21 while the true
	// exponent is -22. Rounding under the wrong e yields 1.000000000000000e-21
	// where the spec — which searches over e as well as n — wants
	// 9.999999999999999e-22, the genuinely closer answer.
	_, es, _ := strings.Cut(strconv.FormatFloat(x, 'e', -1, 64), "e")
	e, _ := strconv.Atoi(es)
	for r.Cmp(ratPow10(e)) < 0 {
		e--
	}
	for r.Cmp(ratPow10(e+1)) >= 0 {
		e++
	}
	s := ratRoundHalfAway(new(big.Rat).Mul(r, ratPow10(sig-1-e))).String()
	if len(s) > sig {
		// Rounding carried into a new digit (9.99 at 2 digits -> 10), which is
		// exactly 10^sig: drop the trailing zero and raise the exponent.
		e++
		s = s[:sig]
	}
	return s, e
}

// toPrecisionStr implements Number.prototype.toPrecision.
func toPrecisionStr(x float64, p int) string {
	if math.IsNaN(x) {
		return "NaN"
	}
	if math.IsInf(x, 0) {
		if x < 0 {
			return "-Infinity"
		}
		return "Infinity"
	}
	if x == 0 {
		if p == 1 {
			return "0"
		}
		return "0." + strings.Repeat("0", p-1)
	}
	neg := x < 0
	if neg {
		x = -x
	}
	digits, exp := roundSignificant(x, p)

	var out string
	switch {
	case exp < -6 || exp >= p:
		m := digits[:1]
		if p > 1 {
			m += "." + digits[1:]
		}
		out = m + "e" + expSign(exp) + strconv.Itoa(absInt(exp))
	case exp == p-1:
		out = digits
	case exp >= 0:
		out = digits[:exp+1] + "." + digits[exp+1:]
	default:
		out = "0." + strings.Repeat("0", -exp-1) + digits
	}
	if neg {
		return "-" + out
	}
	return out
}

// fixExponent rewrites Go's "e+05"/"e-05" exponent form to JS's "e+5"/"e-5".
func fixExponent(s string) string {
	i := strings.IndexByte(s, 'e')
	if i < 0 {
		return s
	}
	mant, exp := s[:i], s[i+1:]
	sign := "+"
	if len(exp) > 0 && (exp[0] == '+' || exp[0] == '-') {
		if exp[0] == '-' {
			sign = "-"
		}
		exp = exp[1:]
	}
	exp = strings.TrimLeft(exp, "0")
	if exp == "" {
		exp = "0"
	}
	return mant + "e" + sign + exp
}

func expSign(e int) string {
	if e < 0 {
		return "-"
	}
	return "+"
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// strconvFixed implements Number.prototype.toFixed formatting. toFixed's sign is
// applied only when x < 0, which is false for -0, so an exact -0 formats without
// a sign (unlike a small negative that rounds to -0, e.g. (-0.4).toFixed(0)).
// The rounding rule is the spec's, not Go's. Step 5 strips the sign before
// anything else, and step 7.a then picks "an integer n for which n / 10^f - x is
// as close to zero as possible. If there are two such n, pick the larger n" — so
// an exact tie rounds up in magnitude. strconv.FormatFloat rounds half to even,
// which disagrees on precisely the ties: (0.5).toFixed(0) is "1", not "0", and
// (2.5).toFixed(0) is "3", not "2".
//
// The comparison is against x, the exact binary value of the double, so it has
// to be done exactly — big.Rat holds a float64 without loss. Deciding the tie in
// floating point is what makes engines disagree on this method.
func strconvFixed(n float64, digits int) string {
	if n == 0 {
		n = 0 // normalize -0 to +0
	}
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	if n >= 1e21 {
		return sign + numberToString(n)
	}

	// n * 10^digits, exactly.
	r := new(big.Rat).SetFloat64(n)
	if r == nil { // NaN or ±Inf never reach here, but SetFloat64 reports them this way
		return strconv.FormatFloat(n, 'f', digits, 64)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	r.Mul(r, new(big.Rat).SetInt(scale))

	// floor(r + 1/2): for a non-negative r this is round-half-up, which is "pick
	// the larger n" once the sign has been taken off.
	r.Add(r, big.NewRat(1, 2))
	i := new(big.Int).Quo(r.Num(), r.Denom())
	if r.Sign() < 0 && new(big.Int).Mul(i, r.Denom()).Cmp(r.Num()) != 0 {
		i.Sub(i, big.NewInt(1))
	}

	s := i.String()
	if digits == 0 {
		return sign + s
	}
	// Insert the point, left-padding when the value is smaller than one unit.
	if len(s) <= digits {
		s = strings.Repeat("0", digits-len(s)+1) + s
	}
	return sign + s[:len(s)-digits] + "." + s[len(s)-digits:]
}

// jsParseFloat implements parseFloat: parse the longest leading decimal prefix.
func jsParseFloat(s string) float64 {
	s = strings.TrimLeftFunc(s, jsStrWhitespace)
	if strings.HasPrefix(s, "Infinity") || strings.HasPrefix(s, "+Infinity") {
		return math.Inf(1)
	}
	if strings.HasPrefix(s, "-Infinity") {
		return math.Inf(-1)
	}
	// Find the longest valid float prefix.
	end := 0
	seenDot, seenE, seenDigit := false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			seenDigit = true
			end = i + 1
		case c == '+' || c == '-':
			if i != 0 && !(s[i-1] == 'e' || s[i-1] == 'E') {
				goto done
			}
		case c == '.':
			if seenDot || seenE {
				goto done
			}
			seenDot = true
		case c == 'e' || c == 'E':
			if seenE || !seenDigit {
				goto done
			}
			seenE = true
		default:
			goto done
		}
	}
done:
	if end == 0 {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return v
		}
		return math.NaN()
	}
	return v
}

// jsParseInt implements parseInt with optional radix.
func jsParseInt(s string, radix int) float64 {
	s = strings.TrimLeftFunc(s, jsStrWhitespace)
	if s == "" {
		return math.NaN()
	}
	sign := 1.0
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		sign = -1
		s = s[1:]
	}
	if radix == 0 {
		if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
			radix = 16
			s = s[2:]
		} else {
			radix = 10
		}
	} else if radix == 16 && len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	if radix < 2 || radix > 36 {
		return math.NaN()
	}
	var val float64
	consumed := 0
	for i := 0; i < len(s); i++ {
		d := digitVal(s[i])
		if d < 0 || d >= radix || !isAlnum(s[i]) {
			break
		}
		val = val*float64(radix) + float64(d)
		consumed++
	}
	if consumed == 0 {
		return math.NaN()
	}
	// Decimal digits: parse the whole run at once so the result is the
	// correctly-rounded double of the exact integer (accumulating in float64
	// digit-by-digit drifts by an ULP for large values, e.g. 21-digit inputs).
	if radix == 10 {
		if v, err := strconv.ParseFloat(s[:consumed], 64); err == nil || math.IsInf(v, 0) {
			return sign * v
		}
	}
	return sign * val
}

func isAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// validJSDecimal rejects Go-accepted forms that JS StrDecimalLiteral forbids
// (underscores, hex via ParseFloat, leading/trailing junk). JS allows an
// optional sign, digits with optional '.', and an optional exponent.
func validJSDecimal(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	sawDigit := false
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		sawDigit = true
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			sawDigit = true
		}
	}
	if !sawDigit {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		expDigit := false
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			expDigit = true
		}
		if !expDigit {
			return false
		}
	}
	return i == len(s)
}
