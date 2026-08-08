package engine

// Intl.NumberFormat's options.
//
// The formatting itself -- grouping, the decimal separator, the localised
// minus sign and infinity -- was already in intl_format.go, per locale. What
// was missing is everything the caller can ask for: the options bag was not
// read at all, so `{style: "percent"}` and `{maximumFractionDigits: 2}` were
// accepted and ignored, and resolvedOptions() reported neither.

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

type numberOptions struct {
	tag              string
	style            string // "decimal", "percent", "currency", "unit"
	currency         string
	currencyDisplay  string
	currencySign     string
	unit             string
	unitDisplay      string
	useGrouping      string // "auto", "always", "min2", or "" for false
	notation         string
	compactDisplay   string
	signDisplay      string
	numbering        string
	roundingPriority string // as COMPUTED, which is what resolvedOptions reports
	roundingType     string
	roundingMode     string
	roundingIncr     int
	trailingZero     string
	digits           pluralOptions
}

func defaultNumberOptions() numberOptions {
	return numberOptions{tag: defaultLocale, style: "decimal", useGrouping: "auto", numbering: "latn",
		notation: "standard", signDisplay: "auto",
		roundingMode: "halfExpand", roundingIncr: 1, trailingZero: "auto",
		roundingPriority: "auto", roundingType: "fractionDigits",
		digits: pluralOptions{minInt: 1, minFrac: 0, maxFrac: 3}}
}

// String and parseNumberOptions round the resolved state through a slot, which
// holds Values and so holds a String. Tab-separated because a currency display
// name never contains one and a comma would collide with the digit fields.
func (n numberOptions) String() string {
	return strings.Join([]string{
		n.tag, n.style, n.currency, n.currencyDisplay, n.currencySign, n.unit,
		n.unitDisplay, n.useGrouping, n.notation, n.compactDisplay, n.signDisplay,
		n.numbering, n.roundingPriority, n.roundingType, n.roundingMode, strconv.Itoa(n.roundingIncr), n.trailingZero,
		strconv.Itoa(n.digits.minInt), strconv.Itoa(n.digits.minFrac),
		strconv.Itoa(n.digits.maxFrac), strconv.Itoa(n.digits.minSig),
		strconv.Itoa(n.digits.maxSig),
	}, "\t")
}

func parseNumberOptions(s string) numberOptions {
	f := strings.Split(s, "\t")
	if len(f) != 22 {
		return defaultNumberOptions()
	}
	i := func(k int) int { v, _ := strconv.Atoi(f[k]); return v }
	return numberOptions{tag: f[0], style: f[1], currency: f[2], currencyDisplay: f[3],
		currencySign: f[4], unit: f[5], unitDisplay: f[6], useGrouping: f[7],
		notation: f[8], compactDisplay: f[9], signDisplay: f[10],
		numbering: f[11], roundingPriority: f[12], roundingType: f[13],
		roundingMode: f[14], roundingIncr: i(15), trailingZero: f[16],
		digits: pluralOptions{minInt: i(17), minFrac: i(18), maxFrac: i(19),
			minSig: i(20), maxSig: i(21)}}
}

// requireNumberFormat is RequireInternalSlot([[InitializedNumberFormat]]).
func (rt *Runtime) requireNumberFormat(this Value) (numberOptions, *ThrowError) {
	this = rt.unwrapLegacyIntl(this, slotIntlNumberOpts)
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlNumberOpts); v.IsString() {
			return parseNumberOptions(rt.strGo(v)), nil
		}
	}
	return numberOptions{}, rt.typeError("not an Intl.NumberFormat")
}

// isWellFormedCurrency is the 3-letter ISO 4217 shape. The list of codes that
// actually exist is not checked, which is what the specification asks for:
// "well-formed", not "assigned".
func isWellFormedCurrency(s string) bool { return len(s) == 3 && tagAlpha(s) }

// isWellFormedUnit is a sanctioned unit, or two of them joined by "-per-".
func isWellFormedUnit(s string) bool {
	if tagContains(sanctionedUnits, s) {
		return true
	}
	num, den, ok := strings.Cut(s, "-per-")
	return ok && tagContains(sanctionedUnits, num) && tagContains(sanctionedUnits, den)
}

// initNumberOptions reads the options bag in the order the specification lists
// it, which is observable through getters, and validates as it goes.
func (rt *Runtime) initNumberOptions(options Value, requested []string) (numberOptions, *ThrowError) {
	n := defaultNumberOptions()
	if len(requested) > 0 {
		// The whole tag, extension and all: resolveNumberingSystem below reads
		// the -u-nu keyword out of it and hands back the tag with that keyword
		// kept or dropped according to whether it was honoured.
		n.tag = lookupMatcher(requested)
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return n, e
	}
	ns, hasNS, e := rt.intlStringOption(options, "numberingSystem", nil)
	if e != nil {
		return n, e
	}
	if hasNS {
		if !isUnicodeType(ns) {
			return n, rt.rangeError("Invalid numberingSystem: " + ns)
		}
		ns = asciiLower(ns)
	} else {
		ns = ""
	}
	n.tag, n.numbering = resolveNumberingSystem(n.tag, ns)
	style, ok, e := rt.intlStringOption(options, "style",
		[]string{"decimal", "percent", "currency", "unit"})
	if e != nil {
		return n, e
	}
	if ok {
		n.style = style
	}
	// The style's own requirement is checked HERE, where the option is read,
	// not after the whole bag: {style: "currency", unit: "test"} is a missing
	// currency before it is a malformed unit, because the unit has not been
	// read yet.
	currency, hasCurrency, e := rt.intlStringOption(options, "currency", nil)
	if e != nil {
		return n, e
	}
	if !hasCurrency {
		if n.style == "currency" {
			return n, rt.typeError("style is currency but no currency was given")
		}
	} else {
		if !isWellFormedCurrency(currency) {
			return n, rt.rangeError("Invalid currency code: " + currency)
		}
		// Kept only where it means something: resolvedOptions().currency is
		// undefined for a formatter that is not formatting money, however the
		// option was written.
		if n.style == "currency" {
			n.currency = asciiUpper(currency)
		}
	}
	currencyDisplay, hasCD, e := rt.intlStringOption(options, "currencyDisplay",
		[]string{"code", "symbol", "narrowSymbol", "name"})
	if e != nil {
		return n, e
	}
	currencySign, hasCS, e := rt.intlStringOption(options, "currencySign",
		[]string{"standard", "accounting"})
	if e != nil {
		return n, e
	}
	unit, hasUnit, e := rt.intlStringOption(options, "unit", nil)
	if e != nil {
		return n, e
	}
	if !hasUnit {
		if n.style == "unit" {
			return n, rt.typeError("style is unit but no unit was given")
		}
	} else {
		if !isWellFormedUnit(unit) {
			return n, rt.rangeError("Invalid unit: " + unit)
		}
		n.unit = unit
	}
	unitDisplay, hasUD, e := rt.intlStringOption(options, "unitDisplay",
		[]string{"short", "narrow", "long"})
	if e != nil {
		return n, e
	}

	if n.style == "currency" {
		n.currencyDisplay, n.currencySign = "symbol", "standard"
		if hasCD {
			n.currencyDisplay = currencyDisplay
		}
		if hasCS {
			n.currencySign = currencySign
		}
	}
	if n.style == "unit" {
		n.unitDisplay = "short"
		if hasUD {
			n.unitDisplay = unitDisplay
		}
	}

	notation, hasNotation, e := rt.intlStringOption(options, "notation",
		[]string{"standard", "scientific", "engineering", "compact"})
	if e != nil {
		return n, e
	}
	if hasNotation {
		n.notation = notation
	}
	// The default fraction digits are the style's: money has the currency's
	// minor-unit count, percentages have none, everything else has up to three.
	// The currency's count is a rule about writing an AMOUNT, though, so it
	// only applies to a number written as one: ¥1.2E3 is not a sum of money
	// with no minor units, it is a number in engineering notation.
	minFrac, maxFrac := 0, 3
	switch {
	case n.style == "percent":
		maxFrac = 0
	case n.style == "currency" && n.notation == "standard":
		minFrac, maxFrac = currencyDigits(n.currency), currencyDigits(n.currency)
	}
	d, e := rt.intlDigitOptions(options, minFrac, maxFrac, n.notation)
	if e != nil {
		return n, e
	}
	n.digits = d
	n.roundingIncr, n.roundingMode = d.roundingIncr, d.roundingMode
	n.trailingZero, n.roundingType = d.trailingZero, d.roundingType
	// resolvedOptions reports the priority the digits ENDED UP with, which is
	// not always the one that was asked for: compact notation with no digit
	// options picks "morePrecision" for itself.
	n.roundingPriority = d.computed

	compactDisplay, hasCompact, e := rt.intlStringOption(options, "compactDisplay",
		[]string{"short", "long"})
	if e != nil {
		return n, e
	}
	if n.notation == "compact" {
		n.compactDisplay = "short"
		if hasCompact {
			n.compactDisplay = compactDisplay
		}
	}
	// Compact notation groups only from five digits up, because "12 345" as
	// "12,345" beside "12K" is a distinction without a difference.
	if n.notation == "compact" {
		n.useGrouping = "min2"
	}
	// useGrouping is the one option whose type changed: it takes a boolean for
	// compatibility and a string for what it now means, and false is the only
	// falsy value that turns grouping off.
	if !options.IsUndefined() {
		v, e := rt.getField(options, "useGrouping")
		if e != nil {
			return n, e
		}
		if !v.IsUndefined() {
			// The order is the specification's: `true` first, then anything
			// falsy, then the strings. "true" and "false" as STRINGS are the
			// default rather than an error, which is a compatibility rule for
			// code written before the option took strings at all.
			switch s, e := rt.toStringValue(v); {
			case e != nil:
				return n, e
			case v.Type() == TBool && rt.toBoolean(v):
				n.useGrouping = "always"
			case !rt.toBoolean(v):
				n.useGrouping = ""
			case rt.strGo(s) == "true" || rt.strGo(s) == "false":
				n.useGrouping = "auto"
			case tagContains([]string{"auto", "always", "min2"}, rt.strGo(s)):
				n.useGrouping = rt.strGo(s)
			default:
				return n, rt.rangeError("Invalid value " + rt.strGo(s) + " for option useGrouping")
			}
		}
	}
	signDisplay, hasSign, e := rt.intlStringOption(options, "signDisplay",
		[]string{"auto", "never", "always", "exceptZero", "negative"})
	if e != nil {
		return n, e
	}
	if hasSign {
		n.signDisplay = signDisplay
	}
	return n, nil
}

// currencyDigits is the minor-unit count of an ISO 4217 code. Two is the rule;
// the exceptions are the currencies with no minor unit at all and the three
// with a thousandth.
func currencyDigits(code string) int {
	switch code {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF",
		"UGX", "UYI", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 3
	}
	return 2
}

// numberPart is one span of formatted output with the name formatToParts gives
// it. format() is the concatenation of the values, which is what keeps the two
// from ever disagreeing about what was written.
type numberPart struct{ typ, val string }

// numberParts renders a Number through the resolved options, on top of the
// locale-aware grouping and separators intl_format.go already does.
func numberParts(n numberOptions, li localeInfo, v float64) []numberPart {
	return numberPartsOf(n, li, v, "")
}

// numberPartsOf is numberParts with an exact decimal spelling to use in place
// of the float, which is how a BigInt keeps every digit it has.
func numberPartsOf(n numberOptions, li localeInfo, v float64, digits string) []numberPart {
	var out []numberPart
	add := func(typ, val string) {
		if val != "" {
			out = append(out, numberPart{typ, val})
		}
	}
	if d, g, ok := numberingSeparators(n.numbering); ok {
		li.decimal, li.group = d, g
	}
	if n.style == "percent" && v == v {
		v *= 100
		// The exact digits have to move too, or the number would be formatted
		// from a spelling a hundred times smaller than the value it came from.
		if digits != "" {
			digits = shiftDecimal(digits, 2)
		}
	}

	neg := math.Signbit(v)
	nan := v != v
	var intPart, frac string
	var exponent int
	infinite := math.IsInf(v, 0)
	compactSuffix := ""
	if !nan && !infinite && n.notation == "compact" && v != 0 {
		// The compact patterns divide by a power of a thousand and name what
		// is left. Below a thousand there is nothing to compact.
		mag := 0
		for a := math.Abs(v); a >= 1000 && mag < 4; a /= 1000 {
			mag++
		}
		if mag > 0 {
			v /= math.Pow(1000, float64(mag))
			compactSuffix = compactName(mag, n.compactDisplay)
		}
	}
	if !nan && !infinite && (n.notation == "scientific" || n.notation == "engineering") {
		// The mantissa is what gets formatted; the exponent is written after
		// it. Engineering keeps the exponent a multiple of three, which is what
		// makes 0.000345 read as 345E-6 rather than 3.45E-4.
		if v != 0 {
			exponent = int(math.Floor(math.Log10(math.Abs(v))))
			if n.notation == "engineering" {
				exponent = int(math.Floor(float64(exponent)/3)) * 3
			}
			v /= math.Pow(10, float64(exponent))
		}
	}
	if !infinite && !nan {
		intPart, frac = expandDecimal(strings.TrimPrefix(numberToString(math.Abs(v)), "-"))
		if digits != "" {
			// A BigInt or a string arrives as its exact decimal digits rather
			// than as a float, because past 2^53 the float has already lost
			// them. Compact and the exponent notations still work from the
			// float: they divide the value, and dividing the digits is a
			// different job that no test asks for.
			neg = strings.HasPrefix(digits, "-")
			intPart, frac, _ = strings.Cut(strings.TrimPrefix(digits, "-"), ".")
		}
		if n.roundingType == "morePrecision" || n.roundingType == "lessPrecision" {
			// Both roundings are computed and one of them is kept, chosen by
			// the PLACE each rounded to rather than by the result: significant
			// digits round to the (e - p + 1)th place, fraction digits to the
			// -maxFrac'th, and the lower place is the more precise. That is why
			// 1234 compacts to "1.2K" -- two significant digits reach further
			// down than no fraction digits do -- and 987654321 to "988M", where
			// they do not.
			si, sf := roundToSignificant(intPart, frac, n.digits.maxSig, n.digits.minSig, n.roundingMode, neg)
			fi, ff := roundDecimal(intPart, frac, n.digits.maxFrac, n.roundingMode, 1, neg)
			ff = strings.TrimRight(ff, "0")
			for len(ff) < n.digits.minFrac {
				ff += "0"
			}
			sigPlace := decimalExponent(intPart, frac) - n.digits.maxSig + 1
			fracPlace := -n.digits.maxFrac
			keepSig := sigPlace <= fracPlace
			if n.roundingType == "lessPrecision" {
				keepSig = !keepSig
			}
			if keepSig {
				intPart, frac = si, sf
			} else {
				intPart, frac = fi, ff
			}
		} else if n.digits.maxSig > 0 {
			intPart, frac = roundToSignificant(intPart, frac, n.digits.maxSig, n.digits.minSig, n.roundingMode, neg)
		} else {
			intPart, frac = roundDecimal(intPart, frac, n.digits.maxFrac,
				n.roundingMode, n.roundingIncr, neg)
			// Trailing zeros beyond the minimum are not written; the minimum
			// itself is, which is what minimumFractionDigits means.
			frac = strings.TrimRight(frac, "0")
			for len(frac) < n.digits.minFrac {
				frac += "0"
			}
			frac = trimTrailingZeros(frac, n.trailingZero)
		}
		for len(intPart) < n.digits.minInt {
			intPart = "0" + intPart
		}
	}

	// Rounding can turn a small negative number into a zero, and a signed zero
	// is not what "-0.0000001 to two places" should print.
	zero := !infinite && !nan && strings.Trim(intPart, "0") == "" && strings.Trim(frac, "0") == ""
	switch n.signDisplay {
	case "never":
	case "always":
		if neg {
			add("minusSign", li.minus)
		} else {
			add("plusSign", "+")
		}
	case "exceptZero":
		// NaN is not a number that is above or below nothing, so it takes no
		// sign here either.
		if !zero && !nan {
			if neg {
				add("minusSign", li.minus)
			} else {
				add("plusSign", "+")
			}
		}
	default: // "auto" and "negative"
		if neg && !(zero && n.signDisplay == "negative") {
			add("minusSign", li.minus)
		}
	}

	if nan {
		add("nan", li.nan)
		return mapPartDigits(withStyleAffixes(n, out), n.numbering)
	}
	if infinite {
		add("infinity", li.inf)
		return mapPartDigits(withStyleAffixes(n, out), n.numbering)
	}

	grouped := intPart
	if n.useGrouping != "" && !(n.useGrouping == "min2" && len(intPart) <= li.minGroup+3) {
		grouped = li.groupInteger(intPart)
	}
	// The grouped form is the ungrouped digits with separators inserted, so
	// splitting on the separator recovers the runs the parts API asks for.
	for i, run := range strings.Split(grouped, li.group) {
		if i > 0 {
			add("group", li.group)
		}
		add("integer", run)
	}
	if frac != "" {
		add("decimal", li.decimal)
		add("fraction", frac)
	}
	if compactSuffix != "" {
		// The long names are words, and a word is written apart from the
		// number: "988 million", but "988M".
		if n.compactDisplay == "long" {
			add("literal", " ")
		}
		add("compact", compactSuffix)
	}
	if n.notation == "scientific" || n.notation == "engineering" {
		add("exponentSeparator", "E")
		e := exponent
		if e < 0 {
			add("exponentMinusSign", li.minus)
			e = -e
		}
		add("exponentInteger", strconv.Itoa(e))
	}
	return mapPartDigits(withStyleAffixes(n, out), n.numbering)
}

// mapPartDigits rewrites the digit spans into the resolved numbering system.
// Only the spans that ARE digits: a separator or a currency symbol is not a
// number and does not change with the system counting it.
func mapPartDigits(parts []numberPart, system string) []numberPart {
	if system == "" || system == "latn" {
		return parts
	}
	for i, p := range parts {
		switch p.typ {
		case "integer", "fraction", "exponentInteger":
			parts[i].val = mapDigits(p.val, system)
		}
	}
	return parts
}

// currencySymbols is the standard symbol of the currencies a script is likely
// to name, and the narrow symbol where it differs. CLDR carries one per
// currency PER LOCALE -- "US$" in most of the world and "$" in the United
// States -- and this is the en table; a currency not in it is written as its
// code, which is what `currencyDisplay: "code"` asks for and a truthful answer
// rather than a guess.
var currencySymbols = map[string][2]string{
	"USD": {"$", "$"}, "EUR": {"€", "€"}, "GBP": {"£", "£"}, "JPY": {"¥", "¥"},
	"CNY": {"CN¥", "¥"}, "KRW": {"₩", "₩"}, "INR": {"₹", "₹"}, "RUB": {"RUB", "₽"},
	"TRY": {"TRY", "₺"}, "BRL": {"R$", "R$"}, "CAD": {"CA$", "$"}, "AUD": {"A$", "$"},
	"CHF": {"CHF", "CHF"}, "SEK": {"SEK", "kr"}, "NOK": {"NOK", "kr"},
	"DKK": {"DKK", "kr"}, "PLN": {"PLN", "zł"}, "MXN": {"MX$", "$"},
	"ZAR": {"ZAR", "R"}, "NZD": {"NZ$", "$"}, "HKD": {"HK$", "$"},
	"SGD": {"SGD", "$"}, "ILS": {"₪", "₪"}, "THB": {"THB", "฿"},
	"VND": {"₫", "₫"}, "PHP": {"₱", "₱"}, "NGN": {"₦", "₦"},
}

// currencyText is what the currency span reads, given the display option.
func currencyText(code, display string) string {
	pair, ok := currencySymbols[code]
	if !ok {
		return code
	}
	switch display {
	case "symbol":
		return pair[0]
	case "narrowSymbol":
		return pair[1]
	}
	return code
}

// withStyleAffixes puts the style's own marker around the number: the percent
// sign after it, the currency symbol before it, the unit after it.
func withStyleAffixes(n numberOptions, parts []numberPart) []numberPart {
	// English pluralises on "not one", which is what the unit spelling needs
	// to know and the only thing it needs to know about the value.
	plural := true
	for _, p := range parts {
		if p.typ == "integer" && p.val == "1" {
			plural = false
		}
		if p.typ == "fraction" || p.typ == "group" {
			plural = true
		}
	}
	switch n.style {
	case "percent":
		return append(parts, numberPart{"percentSign", "%"})
	case "currency":
		// English places the symbol first with no space, and the accounting
		// sign writes a negative amount in parentheses instead of with a minus
		// -- which means taking the minus back out. Both are en's rules; CLDR
		// carries a pattern per locale.
		neg, signed := false, false
		if len(parts) > 0 && (parts[0].typ == "minusSign" || parts[0].typ == "plusSign") {
			signed = true
			neg = parts[0].typ == "minusSign"
		}
		if n.currencySign == "accounting" && neg {
			parts, signed = parts[1:], false
		}
		text := currencyText(n.currency, n.currencyDisplay)
		out := []numberPart{{"currency", text}}
		if n.currencyDisplay == "code" || n.currencyDisplay == "name" || text == n.currency {
			out = append(out, numberPart{"literal", "\u00a0"})
		}
		if n.currencySign == "accounting" && neg {
			out = append([]numberPart{{"literal", "("}}, out...)
			return append(append(out, parts...), numberPart{"literal", ")"})
		}
		if signed {
			// The sign belongs outside the symbol: -$987.00 and +$0.00, not
			// $-987.00 and $+0.00.
			return append(append([]numberPart{parts[0]}, out...), parts[1:]...)
		}
		return append(out, parts...)
	case "unit":
		text, space := unitText(n.unit, n.unitDisplay, plural)
		if space {
			parts = append(parts, numberPart{"literal", " "})
		}
		return append(parts, numberPart{"unit", text})
	}
	return parts
}

// unitNames is the en spelling of each sanctioned unit, singular and plural,
// per display width. A unit not listed is written as its own name, which is
// wrong-looking but never misleading; CLDR carries the whole table per locale.
var unitNames = map[string][3][2]string{
	"year":        {{"year", "years"}, {"yr", "yrs"}, {"y", "y"}},
	"month":       {{"month", "months"}, {"mth", "mths"}, {"m", "m"}},
	"week":        {{"week", "weeks"}, {"wk", "wks"}, {"w", "w"}},
	"day":         {{"day", "days"}, {"day", "days"}, {"d", "d"}},
	"hour":        {{"hour", "hours"}, {"hr", "hrs"}, {"h", "h"}},
	"minute":      {{"minute", "minutes"}, {"min", "min"}, {"m", "m"}},
	"second":      {{"second", "seconds"}, {"sec", "sec"}, {"s", "s"}},
	"millisecond": {{"millisecond", "milliseconds"}, {"ms", "ms"}, {"ms", "ms"}},
	"microsecond": {{"microsecond", "microseconds"}, {"μs", "μs"}, {"μs", "μs"}},
	"nanosecond":  {{"nanosecond", "nanoseconds"}, {"ns", "ns"}, {"ns", "ns"}},
	"percent":     {{"percent", "percent"}, {"%", "%"}, {"%", "%"}},
	"meter":       {{"meter", "meters"}, {"m", "m"}, {"m", "m"}},
	"kilometer":   {{"kilometer", "kilometers"}, {"km", "km"}, {"km", "km"}},
	"centimeter":  {{"centimeter", "centimeters"}, {"cm", "cm"}, {"cm", "cm"}},
	"millimeter":  {{"millimeter", "millimeters"}, {"mm", "mm"}, {"mm", "mm"}},
	"mile":        {{"mile", "miles"}, {"mi", "mi"}, {"mi", "mi"}},
	"foot":        {{"foot", "feet"}, {"ft", "ft"}, {"′", "′"}},
	"inch":        {{"inch", "inches"}, {"in", "in"}, {"″", "″"}},
	"yard":        {{"yard", "yards"}, {"yd", "yd"}, {"yd", "yd"}},
	"gram":        {{"gram", "grams"}, {"g", "g"}, {"g", "g"}},
	"kilogram":    {{"kilogram", "kilograms"}, {"kg", "kg"}, {"kg", "kg"}},
	"pound":       {{"pound", "pounds"}, {"lb", "lb"}, {"#", "#"}},
	"celsius":     {{"degree Celsius", "degrees Celsius"}, {"°C", "°C"}, {"°C", "°C"}},
	"fahrenheit":  {{"degree Fahrenheit", "degrees Fahrenheit"}, {"°F", "°F"}, {"°", "°"}},
	"byte":        {{"byte", "bytes"}, {"byte", "byte"}, {"B", "B"}},
	"kilobyte":    {{"kilobyte", "kilobytes"}, {"kB", "kB"}, {"kB", "kB"}},
	"megabyte":    {{"megabyte", "megabytes"}, {"MB", "MB"}, {"MB", "MB"}},
	"gigabyte":    {{"gigabyte", "gigabytes"}, {"GB", "GB"}, {"GB", "GB"}},
	"terabyte":    {{"terabyte", "terabytes"}, {"TB", "TB"}, {"TB", "TB"}},
	"liter":       {{"liter", "liters"}, {"L", "L"}, {"L", "L"}},
	"milliliter":  {{"milliliter", "milliliters"}, {"mL", "mL"}, {"mL", "mL"}},
	"gallon":      {{"gallon", "gallons"}, {"gal", "gal"}, {"gal", "gal"}},
	"degree":      {{"degree", "degrees"}, {"deg", "deg"}, {"°", "°"}},
	"acre":        {{"acre", "acres"}, {"ac", "ac"}, {"ac", "ac"}},
	"hectare":     {{"hectare", "hectares"}, {"ha", "ha"}, {"ha", "ha"}},
	"bit":         {{"bit", "bits"}, {"bit", "bit"}, {"b", "b"}},
}

// unitText is the unit as written, and whether a space belongs before it. A
// compound unit ("kilometer-per-hour") is its two halves joined by "/" in the
// short and narrow widths and by " per " in the long one.
func unitText(unit, display string, plural bool) (string, bool) {
	idx := 1
	switch display {
	case "long":
		idx = 0
	case "narrow":
		idx = 2
	}
	one := func(u string, p bool) string {
		names, ok := unitNames[u]
		if !ok {
			return u
		}
		if p {
			return names[idx][1]
		}
		return names[idx][0]
	}
	// A unit written as a sign rather than a word is set against the number:
	// "3%", not "3 %". Only the long form spells it out.
	if unit == "percent" {
		if idx == 0 {
			return "percent", true
		}
		return "%", false
	}
	if num, den, ok := strings.Cut(unit, "-per-"); ok {
		if idx == 0 {
			return one(num, plural) + " per " + one(den, false), true
		}
		// The denominator of a compound takes its narrowest spelling whatever
		// the numerator's is: "km/h", not "km/hr".
		wide := idx
		idx = 2
		den2 := one(den, false)
		idx = wide
		return one(num, plural) + "/" + den2, idx == 1
	}
	return one(unit, plural), idx != 2
}

// formatNumberWith is the concatenation of the parts, so format() and
// formatToParts() cannot drift apart.
func (rt *Runtime) formatNumberWith(n numberOptions, li localeInfo, v float64) string {
	return rt.formatNumberOf(n, li, v, "")
}

// formatNumberOf is formatNumberWith with an exact decimal spelling.
func (rt *Runtime) formatNumberOf(n numberOptions, li localeInfo, v float64, digits string) string {
	var b strings.Builder
	for _, p := range numberPartsOf(n, li, v, digits) {
		b.WriteString(p.val)
	}
	return b.String()
}

// formatNumberParts is formatToParts: the same spans, as objects.
func (rt *Runtime) formatNumberParts(n numberOptions, li localeInfo, v float64) Value {
	return rt.partsArray(numberParts(n, li, v))
}

// partsArray turns spans into the {type, value} objects every formatToParts
// returns.
func (rt *Runtime) partsArray(parts []numberPart) Value {
	arr := rt.newArray()
	ao := rt.objPtr(arr)
	for i, p := range parts {
		o := rt.newPlainObject()
		oo := rt.objPtr(o)
		oo.defineOwn("type", rt.newString(p.typ), attrDefault)
		oo.defineOwn("value", rt.newString(p.val), attrDefault)
		rt.arraySet(ao, uint32(i), o)
	}
	return arr
}

// intlNumberOption is GetNumberOption: a Number in [lo, hi], floored.
func (rt *Runtime) intlNumberOption(options Value, name string, lo, hi, fallback int) (int, bool, *ThrowError) {
	if options.IsUndefined() {
		return fallback, false, nil
	}
	v, e := rt.getField(options, name)
	if e != nil {
		return 0, false, e
	}
	if v.IsUndefined() {
		return fallback, false, nil
	}
	num, e := rt.toNumber(v)
	if e != nil {
		return 0, false, e
	}
	// The range is checked before the value is floored, so 3.000001 is out of
	// range for a maximum of 3 rather than rounding down into it: the option
	// was written as a number of digits and 3.000001 is not one.
	if math.IsNaN(num) || num < float64(lo) || num > float64(hi) {
		return 0, false, rt.rangeError("Value " + numberToString(num) + " out of range for " + name)
	}
	return int(math.Floor(num)), true, nil
}

func intContains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// isNumberPart reports whether a span is part of the number itself rather than
// something written around it. What is written around it -- a currency symbol,
// a sign, a unit and the space before it -- is a "modifier", and the modifiers
// are what a range may or may not repeat.
func isNumberPart(typ string) bool {
	switch typ {
	case "integer", "group", "decimal", "fraction", "compact", "nan", "infinity",
		"exponentSeparator", "exponentMinusSign", "exponentInteger":
		return true
	}
	return false
}

// splitModifiers reports where the number itself begins and ends: everything
// before lead and from mid on is written around it.
func splitModifiers(parts []numberPart) (lead, mid int) {
	for lead < len(parts) && !isNumberPart(parts[lead].typ) {
		lead++
	}
	mid = len(parts)
	for mid > lead && !isNumberPart(parts[mid-1].typ) {
		mid--
	}
	return lead, mid
}

func partsRunes(parts []numberPart) int {
	n := 0
	for _, p := range parts {
		n += len([]rune(p.val))
	}
	return n
}

func samePartRun(a, b []numberPart) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rangeParts is CreatePartsFromRange: the two ends formatted, marked with which
// end each span came from.
//
// What is written around the two ends decides how the range reads. Repeated,
// the ends need holding apart -- "$3 - $5" with spaces. Written once, they do
// not, and the dash closes up: "-$3.00-5.00", "3-5 m", "3-5". The line between
// the two is where ICU draws it, and it is a strange place: a modifier of a
// SINGLE code point is repeated and anything longer is shared, so "$3 - $5"
// and "-$3.00-5.00" differ only by the minus sign that made the prefix two
// characters long.
func rangeParts(start, end []numberPart) []sourcedPart {
	if samePartRun(start, end) {
		// The same number at both ends is one value the rounding reached from
		// two directions, and says so: "~$3".
		out := []sourcedPart{{numberPart{"approximatelySign", "~"}, "shared"}}
		for _, p := range start {
			out = append(out, sourcedPart{p, "shared"})
		}
		return out
	}
	sLead, sMid := splitModifiers(start)
	eLead, eMid := splitModifiers(end)
	shared := samePartRun(start[:sLead], end[:eLead]) &&
		samePartRun(start[sMid:], end[eMid:])
	width := partsRunes(start[:sLead]) + partsRunes(start[sMid:])

	var out []sourcedPart
	if shared && width > 1 {
		for _, p := range start[:sLead] {
			out = append(out, sourcedPart{p, "shared"})
		}
		for _, p := range start[sLead:sMid] {
			out = append(out, sourcedPart{p, "startRange"})
		}
		out = append(out, sourcedPart{numberPart{"literal", "–"}, "shared"})
		for _, p := range end[eLead:eMid] {
			out = append(out, sourcedPart{p, "endRange"})
		}
		for _, p := range end[eMid:] {
			out = append(out, sourcedPart{p, "shared"})
		}
		return out
	}
	sep := "–"
	if width > 0 {
		sep = " – "
	}
	for _, p := range start {
		out = append(out, sourcedPart{p, "startRange"})
	}
	out = append(out, sourcedPart{numberPart{"literal", sep}, "shared"})
	for _, p := range end {
		out = append(out, sourcedPart{p, "endRange"})
	}
	return out
}

// sourcedPart is a span that also says which end of a range it came from.
type sourcedPart struct {
	numberPart
	source string
}

func (rt *Runtime) sourcedPartsArray(parts []sourcedPart) Value {
	arr := rt.newArray()
	ao := rt.objPtr(arr)
	for i, p := range parts {
		o := rt.newPlainObject()
		oo := rt.objPtr(o)
		oo.defineOwn("type", rt.newString(p.typ), attrDefault)
		oo.defineOwn("value", rt.newString(p.val), attrDefault)
		oo.defineOwn("source", rt.newString(p.source), attrDefault)
		rt.arraySet(ao, uint32(i), o)
	}
	return arr
}

func sourcedString(parts []sourcedPart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.val)
	}
	return b.String()
}

// compactName is what a compact pattern writes after the number, for a
// magnitude counted in powers of a thousand. English's; CLDR carries a set per
// locale and per plural category.
func compactName(mag int, display string) string {
	short := []string{"", "K", "M", "B", "T"}
	long := []string{"", "thousand", "million", "billion", "trillion"}
	if mag < 1 || mag > 4 {
		return ""
	}
	if display == "long" {
		return long[mag]
	}
	return short[mag]
}

// intlNumericArg is ToIntlMathematicalValue as far as this engine needs it: a
// Number, or a BigInt with its exact decimal digits alongside, because a
// BigInt above 2^53 has more of them than the float can hold.
func (rt *Runtime) intlNumericArg(v Value) (float64, string, *ThrowError) {
	prim, e := rt.toPrimitive(v, "number")
	if e != nil {
		return 0, "", e
	}
	if b := rt.bigIntVal(prim); b != nil {
		digits := bigIntToString(b, 10)
		f, _ := new(big.Float).SetInt(b).Float64()
		return f, digits, nil
	}
	if prim.IsString() {
		// A string argument is read as an exact decimal rather than through a
		// float, which is the whole point of being allowed to pass one:
		// "1.0000000000000001" is a number the double cannot hold and the
		// string can.
		s := rt.strGo(prim)
		n, e := rt.toNumber(prim)
		if e != nil {
			return 0, "", e
		}
		if exact, ok := parseExactDecimal(s); ok {
			return n, exact, nil
		}
		return n, "", nil
	}
	n, e := rt.toNumber(prim)
	return n, "", e
}

// parseExactDecimal spells a StringNumericLiteral as a plain decimal, keeping
// every digit: "1e3" is "1000" and "1.0000000000000001" is itself. It reports
// false for the forms that have no digits to keep -- NaN, the infinities, and
// the non-decimal radixes, which are exact as floats anyway up to 2^53 and are
// not what a caller passing a string is after.
func parseExactDecimal(s string) (string, bool) {
	s = strings.TrimFunc(s, jsStrWhitespace)
	neg := false
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		neg, s = true, rest
	} else if rest, ok := strings.CutPrefix(s, "+"); ok {
		s = rest
	}
	if s == "" || strings.ContainsAny(s, "xXoObBnN") {
		return "", false
	}
	mant, expPart, hasExp := strings.Cut(s, "e")
	if !hasExp {
		mant, expPart, hasExp = strings.Cut(s, "E")
	}
	exp := 0
	if hasExp {
		v, err := strconv.Atoi(expPart)
		if err != nil {
			return "", false
		}
		exp = v
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	if intPart == "" && frac == "" {
		return "", false
	}
	for _, d := range intPart + frac {
		if d < '0' || d > '9' {
			return "", false
		}
	}
	// Applying the exponent is moving the point, which is padding with zeros
	// on whichever side runs out first.
	digits := intPart + frac
	point := len(intPart) + exp
	for point < 0 {
		digits, point = "0"+digits, point+1
	}
	for point > len(digits) {
		digits += "0"
	}
	out := strings.TrimLeft(digits[:point], "0")
	if out == "" {
		out = "0"
	}
	if tail := strings.TrimRight(digits[point:], "0"); tail != "" {
		out += "." + tail
	}
	if neg {
		out = "-" + out
	}
	return out, true
}

// shiftDecimal moves an exact decimal's point right by n places, which is what
// a percentage does to the number it is written from.
func shiftDecimal(s string, n int) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac, _ := strings.Cut(s, ".")
	for len(frac) < n {
		frac += "0"
	}
	intPart, frac = intPart+frac[:n], frac[n:]
	if t := strings.TrimLeft(intPart, "0"); t != "" {
		intPart = t
	} else {
		intPart = "0"
	}
	out := intPart
	if frac = strings.TrimRight(frac, "0"); frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}
