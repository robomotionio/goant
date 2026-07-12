package engine

// Port of the JS number⇄string conversions (ant src/numbers.cc). These are a
// frequent conformance pitfall, so the ECMAScript Number::toString algorithm
// (spec 6.1.6.1.20) and ToNumber-on-strings (StrNumericLiteral) are implemented
// exactly rather than deferred to Go's default formatting.

import (
	"math"
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
	var val float64
	for i := 0; i < len(s); i++ {
		d := digitVal(s[i])
		if d < 0 || d >= radix || !isRadixDigit(s[i], radix) {
			return math.NaN()
		}
		val = val*float64(radix) + float64(d)
	}
	return val
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
	prec := fracDigits
	if !hasDigits {
		prec = -1
	}
	s := strconv.FormatFloat(x, 'e', prec, 64)
	return fixExponent(s)
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
	// p significant digits via 'e' with p-1 fraction digits.
	es := strconv.FormatFloat(x, 'e', p-1, 64)
	mantissa, expStr, _ := strings.Cut(es, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mantissa, ".", "", 1)

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

// strconvFixed implements Number.prototype.toFixed formatting.
func strconvFixed(n float64, digits int) string {
	if math.Abs(n) >= 1e21 {
		return numberToString(n)
	}
	return strconv.FormatFloat(n, 'f', digits, 64)
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
