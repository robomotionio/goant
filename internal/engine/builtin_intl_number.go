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
	"strconv"
	"strings"
)

type numberOptions struct {
	tag             string
	style           string // "decimal", "percent", "currency", "unit"
	currency        string
	currencyDisplay string
	currencySign    string
	unit            string
	unitDisplay     string
	useGrouping     string // "auto", "always", "min2", or "" for false
	notation        string
	compactDisplay  string
	signDisplay     string
	roundingMode    string
	roundingIncr    int
	trailingZero    string
	digits          pluralOptions
}

func defaultNumberOptions() numberOptions {
	return numberOptions{tag: defaultLocale, style: "decimal", useGrouping: "auto",
		notation: "standard", signDisplay: "auto",
		roundingMode: "halfExpand", roundingIncr: 1, trailingZero: "auto",
		digits: pluralOptions{minInt: 1, minFrac: 0, maxFrac: 3}}
}

// String and parseNumberOptions round the resolved state through a slot, which
// holds Values and so holds a String. Tab-separated because a currency display
// name never contains one and a comma would collide with the digit fields.
func (n numberOptions) String() string {
	return strings.Join([]string{
		n.tag, n.style, n.currency, n.currencyDisplay, n.currencySign, n.unit,
		n.unitDisplay, n.useGrouping, n.notation, n.compactDisplay, n.signDisplay,
		n.roundingMode, strconv.Itoa(n.roundingIncr), n.trailingZero,
		strconv.Itoa(n.digits.minInt), strconv.Itoa(n.digits.minFrac),
		strconv.Itoa(n.digits.maxFrac), strconv.Itoa(n.digits.minSig),
		strconv.Itoa(n.digits.maxSig),
	}, "\t")
}

func parseNumberOptions(s string) numberOptions {
	f := strings.Split(s, "\t")
	if len(f) != 19 {
		return defaultNumberOptions()
	}
	i := func(k int) int { v, _ := strconv.Atoi(f[k]); return v }
	return numberOptions{tag: f[0], style: f[1], currency: f[2], currencyDisplay: f[3],
		currencySign: f[4], unit: f[5], unitDisplay: f[6], useGrouping: f[7],
		notation: f[8], compactDisplay: f[9], signDisplay: f[10],
		roundingMode: f[11], roundingIncr: i(12), trailingZero: f[13],
		digits: pluralOptions{minInt: i(14), minFrac: i(15), maxFrac: i(16),
			minSig: i(17), maxSig: i(18)}}
}

// requireNumberFormat is RequireInternalSlot([[InitializedNumberFormat]]).
func (rt *Runtime) requireNumberFormat(this Value) (numberOptions, *ThrowError) {
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
		// The tag is kept as asked for, minus the extension: the number tables
		// are per-locale but the resolved locale must not claim keywords this
		// service did not honour.
		if t, ok := parseLangTag(requested[0]); ok {
			n.tag = t.languageID()
		} else {
			n.tag = requested[0]
		}
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return n, e
	}
	if _, _, e := rt.intlStringOption(options, "numberingSystem", nil); e != nil {
		return n, e
	}
	style, ok, e := rt.intlStringOption(options, "style",
		[]string{"decimal", "percent", "currency", "unit"})
	if e != nil {
		return n, e
	}
	if ok {
		n.style = style
	}
	currency, hasCurrency, e := rt.intlStringOption(options, "currency", nil)
	if e != nil {
		return n, e
	}
	if hasCurrency {
		if !isWellFormedCurrency(currency) {
			return n, rt.rangeError("Invalid currency code: " + currency)
		}
		n.currency = asciiUpper(currency)
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
	if hasUnit {
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
		if !hasCurrency {
			return n, rt.typeError("style is currency but no currency was given")
		}
		n.currencyDisplay, n.currencySign = "symbol", "standard"
		if hasCD {
			n.currencyDisplay = currencyDisplay
		}
		if hasCS {
			n.currencySign = currencySign
		}
	}
	if n.style == "unit" {
		if !hasUnit {
			return n, rt.typeError("style is unit but no unit was given")
		}
		n.unitDisplay = "short"
		if hasUD {
			n.unitDisplay = unitDisplay
		}
	}

	// The default fraction digits are the style's: money has the currency's
	// minor-unit count, percentages have none, everything else has up to three.
	minFrac, maxFrac := 0, 3
	switch n.style {
	case "percent":
		maxFrac = 0
	case "currency":
		minFrac, maxFrac = currencyDigits(n.currency), currencyDigits(n.currency)
	}
	notation, hasNotation, e := rt.intlStringOption(options, "notation",
		[]string{"standard", "scientific", "engineering", "compact"})
	if e != nil {
		return n, e
	}
	if hasNotation {
		n.notation = notation
	}
	if n.notation == "compact" {
		// Compact notation has no default fraction digits at all; the
		// significant-digit rule takes over.
		maxFrac = 0
	}
	roundingIncrement, hasIncr, e := rt.intlNumberOption(options, "roundingIncrement", 1, 5000, 1)
	if e != nil {
		return n, e
	}
	if hasIncr && !intContains(validRoundingIncrements, roundingIncrement) {
		return n, rt.rangeError("Invalid roundingIncrement")
	}
	n.roundingIncr = roundingIncrement
	roundingMode, hasMode, e := rt.intlStringOption(options, "roundingMode", roundingModes)
	if e != nil {
		return n, e
	}
	if hasMode {
		n.roundingMode = roundingMode
	}
	trailing, hasTrailing, e := rt.intlStringOption(options, "trailingZeroDisplay",
		[]string{"auto", "stripIfInteger"})
	if e != nil {
		return n, e
	}
	if hasTrailing {
		n.trailingZero = trailing
	}
	d, e := rt.intlDigitOptions(options, minFrac, maxFrac)
	if e != nil {
		return n, e
	}
	n.digits = d
	// A rounding increment only makes sense at a fixed number of fraction
	// digits: it says what the last place steps by, and with significant
	// digits or an open-ended maximum there is no last place.
	if n.roundingIncr != 1 {
		if n.digits.maxSig > 0 {
			return n, rt.typeError("roundingIncrement cannot be used with significant digits")
		}
		if n.digits.minFrac != n.digits.maxFrac {
			return n, rt.rangeError("roundingIncrement requires minimumFractionDigits to equal maximumFractionDigits")
		}
	}

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
	// useGrouping is the one option whose type changed: it takes a boolean for
	// compatibility and a string for what it now means, and false is the only
	// falsy value that turns grouping off.
	if !options.IsUndefined() {
		v, e := rt.getField(options, "useGrouping")
		if e != nil {
			return n, e
		}
		if !v.IsUndefined() {
			switch {
			case v.Type() == TBool:
				if rt.toBoolean(v) {
					n.useGrouping = "always"
				} else {
					n.useGrouping = ""
				}
			case v.IsString() && rt.strGo(v) == "":
				n.useGrouping = ""
			default:
				s, e := rt.toStringValue(v)
				if e != nil {
					return n, e
				}
				got := rt.strGo(s)
				if !tagContains([]string{"auto", "always", "min2"}, got) {
					return n, rt.rangeError("Invalid value " + got + " for option useGrouping")
				}
				n.useGrouping = got
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
	var out []numberPart
	add := func(typ, val string) {
		if val != "" {
			out = append(out, numberPart{typ, val})
		}
	}
	if v != v {
		add("nan", li.nan)
		return withStyleAffixes(n, out)
	}
	if n.style == "percent" {
		v *= 100
	}

	neg := math.Signbit(v)
	var intPart, frac string
	infinite := math.IsInf(v, 0)
	if !infinite {
		intPart, frac = expandDecimal(strings.TrimPrefix(numberToString(math.Abs(v)), "-"))
		if n.digits.maxSig > 0 {
			intPart, frac = roundToSignificant(intPart, frac, n.digits.maxSig, n.digits.minSig)
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
	zero := !infinite && strings.Trim(intPart, "0") == "" && strings.Trim(frac, "0") == ""
	switch n.signDisplay {
	case "never":
	case "always":
		if neg {
			add("minusSign", li.minus)
		} else {
			add("plusSign", "+")
		}
	case "exceptZero":
		if !zero {
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

	if infinite {
		add("infinity", li.inf)
		return withStyleAffixes(n, out)
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
	return withStyleAffixes(n, out)
}

// withStyleAffixes puts the style's own marker around the number: the percent
// sign after it, the currency code or the unit beside it.
func withStyleAffixes(n numberOptions, parts []numberPart) []numberPart {
	switch n.style {
	case "percent":
		return append(parts, numberPart{"percentSign", "%"})
	case "currency":
		// Without CLDR's currency symbols and placement rules the code is what
		// gets written -- which is exactly `currencyDisplay: "code"`, and a
		// truthful answer rather than a guessed symbol in a guessed position.
		return append([]numberPart{{"currency", n.currency}, {"literal", " "}}, parts...)
	case "unit":
		return append(parts, numberPart{"literal", " "}, numberPart{"unit", n.unit})
	}
	return parts
}

// formatNumberWith is the concatenation of the parts, so format() and
// formatToParts() cannot drift apart.
func (rt *Runtime) formatNumberWith(n numberOptions, li localeInfo, v float64) string {
	var b strings.Builder
	for _, p := range numberParts(n, li, v) {
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
	f := math.Floor(num)
	if math.IsNaN(f) || f < float64(lo) || f > float64(hi) {
		return 0, false, rt.rangeError("Value " + numberToString(num) + " out of range for " + name)
	}
	return int(f), true, nil
}

func intContains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
