package engine

// Locale-sensitive formatting for Intl and the toLocale* methods, over the
// tables in intl_data.go.
//
// The default locale is pinned rather than read from the host. V8 resolves it
// through ICU from $LANG, which meant one flow rendered 02.01.2026 on a Turkish
// desktop and 1/2/2026 on the same robot running as a service with no $LANG
// set. Since scripts demonstrably parse these strings, output that changes with
// the machine is worse than output that is merely opinionated. en-US is what
// that resolution fell back to whenever $LANG was unset or C, so pinning it
// keeps the common case identical and makes the rest reproducible.

import (
	"math"
	"strconv"
	"strings"
	"time"
)

const defaultLocale = "en-US"

type dateTimeKind int

const (
	dtDate dateTimeKind = iota
	dtTime
	dtDateTime
)

// lookupLocale resolves a BCP 47 tag against localeTable: an exact match first,
// then the language's default region, then the pinned default. Matching is
// case-insensitive and ignores extension subtags, so "EN-us" and
// "en-US-u-ca-gregory" both land on en-US.
func lookupLocale(tag string) (localeInfo, string) {
	base := tag
	// Drop everything from the first singleton subtag ("-u-", "-x-", ...).
	for i, part := range strings.Split(tag, "-") {
		if i > 0 && len(part) == 1 {
			base = strings.Join(strings.Split(tag, "-")[:i], "-")
			break
		}
	}
	parts := strings.Split(base, "-")
	lang := strings.ToLower(parts[0])
	canon := lang
	// language[-script][-region]: the region subtag is two letters or three
	// digits, which is what tells it apart from a four-letter script subtag.
	for _, p := range parts[1:] {
		if len(p) == 2 || (len(p) == 3 && allDigits(p)) {
			canon = lang + "-" + strings.ToUpper(p)
			break
		}
	}
	if li, ok := localeTable[canon]; ok {
		li.tag = canon
		return li, canon
	}
	if t, ok := languageDefaults[lang]; ok {
		li := localeTable[t]
		li.tag = t
		return li, t
	}
	li := localeTable[defaultLocale]
	li.tag = defaultLocale
	return li, defaultLocale
}

// resolveLocaleArg validates a `locales` argument the way CanonicalizeLocaleList
// does and resolves it to the entry that will format. Only the first tag is
// considered, which is what ICU's best-fit matcher does for the tags we ship.
func (rt *Runtime) resolveLocaleArg(v Value) (localeInfo, *ThrowError) {
	li, _, e := rt.resolveLocaleArgTag(v)
	return li, e
}

// resolveLocaleArgTag also reports the tag it resolved to, which
// Intl.*.prototype.resolvedOptions() has to echo back.
//
// The whole list is canonicalised even though only the first entry can be
// resolved, because the validation is observable: a RangeError for the third
// tag in an array has to be thrown whether or not the first one already
// decided the answer.
func (rt *Runtime) resolveLocaleArgTag(v Value) (localeInfo, string, *ThrowError) {
	tags, e := rt.canonicalizeLocaleList(v)
	if e != nil {
		return localeInfo{}, "", e
	}
	if len(tags) == 0 {
		return localeTable[defaultLocale], defaultLocale, nil
	}
	li, resolved := lookupLocale(tags[0])
	return li, resolved, nil
}

// ---- date/time ----

// formatDateTime renders t through the locale's pattern. t must already be in
// the zone being displayed; this only reads its wall-clock fields.
func (li localeInfo) formatDateTime(kind dateTimeKind, t time.Time) string {
	switch kind {
	case dtDate:
		return li.expand(li.date, t)
	case dtTime:
		return li.expand(li.time, t)
	}
	d, tm := li.expand(li.date, t), li.expand(li.time, t)
	out := strings.Replace(li.glue, "{d}", d, 1)
	return strings.Replace(out, "{t}", tm, 1)
}

// expand substitutes the pattern's {…} tokens. Anything outside braces is a
// literal, so a locale whose pattern contains a stray brace is not possible by
// construction: the generator only ever emits balanced tokens.
func (li localeInfo) expand(pattern string, t time.Time) string {
	var b strings.Builder
	b.Grow(len(pattern) + 8)
	for i := 0; i < len(pattern); {
		if pattern[i] != '{' {
			b.WriteByte(pattern[i])
			i++
			continue
		}
		j := strings.IndexByte(pattern[i:], '}')
		if j < 0 {
			b.WriteByte(pattern[i])
			i++
			continue
		}
		b.WriteString(li.token(pattern[i+1:i+j], t))
		i += j + 1
	}
	return b.String()
}

func (li localeInfo) token(tok string, t time.Time) string {
	switch tok {
	case "Y":
		return strconv.Itoa(t.Year())
	case "M":
		return strconv.Itoa(int(t.Month()))
	case "MM":
		return twoDigits(int(t.Month()))
	case "D":
		return strconv.Itoa(t.Day())
	case "DD":
		return twoDigits(t.Day())
	case "H":
		return strconv.Itoa(li.displayHour(t.Hour()))
	case "HH":
		return twoDigits(li.displayHour(t.Hour()))
	case "m":
		return strconv.Itoa(t.Minute())
	case "mm":
		return twoDigits(t.Minute())
	case "s":
		return strconv.Itoa(t.Second())
	case "ss":
		return twoDigits(t.Second())
	case "P":
		if t.Hour() < 12 {
			return li.am
		}
		return li.pm
	}
	return ""
}

// displayHour maps a 0..23 clock hour onto the locale's hour cycle: h23 keeps
// 0..23, h24 shows midnight as 24, h12 shows 12-hour with noon and midnight as
// 12, and h11 shows them as 0.
func (li localeInfo) displayHour(h int) int {
	switch li.hourCycle {
	case "h24":
		if h == 0 {
			return 24
		}
		return h
	case "h12":
		if h%12 == 0 {
			return 12
		}
		return h % 12
	case "h11":
		return h % 12
	default: // h23
		return h
	}
}

// ---- numbers ----

// maxLocaleFractionDigits is Intl.NumberFormat's default
// maximumFractionDigits. It is the reason (1/3).toLocaleString() is "0.333"
// rather than the full expansion Number.prototype.toString gives.
const maxLocaleFractionDigits = 3

// formatNumber renders n the way Intl.NumberFormat's default options do:
// grouped, at most three fraction digits, with the locale's separators.
func (li localeInfo) formatNumber(n float64) string {
	switch {
	case n != n:
		return li.nan
	case n > maxFloat:
		return li.inf
	case n < -maxFloat:
		return li.minus + li.inf
	}

	// Start from the ECMAScript Number::toString digits so that a value like
	// 1234567890123456789 renders as its shortest round-trip form
	// (…456800), which is what ICU shows, rather than the exact binary value.
	s := numberToString(n)
	// Number::toString renders negative zero as "0"; ICU keeps the sign.
	neg := math.Signbit(n)
	s = strings.TrimPrefix(s, "-")
	intPart, frac := expandDecimal(s)
	intPart, frac = roundFraction(intPart, frac, maxLocaleFractionDigits)

	var b strings.Builder
	if neg {
		b.WriteString(li.minus)
	}
	b.WriteString(li.groupInteger(intPart))
	if frac != "" {
		b.WriteString(li.decimal)
		b.WriteString(frac)
	}
	return b.String()
}

const maxFloat = 1.7976931348623157e308

// expandDecimal turns Number::toString output into plain positional notation,
// returning the integer and fraction digit strings. 1e21 stringifies as "1e+21"
// but formats as 1,000,000,000,000,000,000,000, so the exponent has to be
// spread back into digits.
func expandDecimal(s string) (string, string) {
	mant, exp := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant = s[:i]
		exp, _ = parseSignedInt(s[i+1:])
	}
	intPart, frac := mant, ""
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		intPart, frac = mant[:i], mant[i+1:]
	}
	digits := intPart + frac
	point := len(intPart) + exp // digits before the decimal point
	switch {
	case point <= 0:
		digits = strings.Repeat("0", 1-point) + digits
		point = 1
	case point > len(digits):
		digits += strings.Repeat("0", point-len(digits))
	}
	intPart = strings.TrimLeft(digits[:point], "0")
	if intPart == "" {
		intPart = "0"
	}
	return intPart, strings.TrimRight(digits[point:], "0")
}

func parseSignedInt(s string) (int, bool) {
	neg := false
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	if !allDigits(s) {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		return -n, true
	}
	return n, true
}

// roundFraction truncates frac to at most max digits, rounding half away from
// zero, which is the rounding mode Intl.NumberFormat defaults to. A carry can
// propagate into the integer part (9.9995 -> 10).
func roundFraction(intPart, frac string, max int) (string, string) {
	if len(frac) <= max {
		return intPart, frac
	}
	roundUp := frac[max] >= '5'
	frac = frac[:max]
	if !roundUp {
		return intPart, strings.TrimRight(frac, "0")
	}
	intLen := len(intPart)
	digits := []byte(intPart + frac)
	i := len(digits) - 1
	for ; i >= 0; i-- {
		if digits[i] != '9' {
			digits[i]++
			break
		}
		digits[i] = '0'
	}
	if i < 0 {
		// Every digit carried, as in 999.9999: the integer part grows by one.
		digits = append([]byte{'1'}, digits...)
		intLen++
	}
	return string(digits[:intLen]), strings.TrimRight(string(digits[intLen:]), "0")
}

// groupInteger inserts the locale's group separator. Most locales group every
// three digits; the Indian pattern groups the last three and then every two.
// minGroup is CLDR's minimumGroupingDigits: where it is 2, a four-digit number
// stays ungrouped (es-ES writes 1234 but 12.345).
func (li localeInfo) groupInteger(digits string) string {
	if li.group == "" || len(digits) < li.minGroup+3 {
		return digits
	}
	var parts []string
	rest := digits
	if len(rest) > 3 {
		parts = append(parts, rest[len(rest)-3:])
		rest = rest[:len(rest)-3]
	}
	step := 3
	if li.indian {
		step = 2
	}
	for len(rest) > step {
		parts = append(parts, rest[len(rest)-step:])
		rest = rest[:len(rest)-step]
	}
	parts = append(parts, rest)
	// parts were collected least-significant first
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, li.group)
}
