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
	digits          pluralOptions
}

func defaultNumberOptions() numberOptions {
	return numberOptions{tag: defaultLocale, style: "decimal", useGrouping: "auto",
		notation: "standard", signDisplay: "auto",
		digits: pluralOptions{minInt: 1, minFrac: 0, maxFrac: 3}}
}

// String and parseNumberOptions round the resolved state through a slot, which
// holds Values and so holds a String. Tab-separated because a currency display
// name never contains one and a comma would collide with the digit fields.
func (n numberOptions) String() string {
	return strings.Join([]string{
		n.tag, n.style, n.currency, n.currencyDisplay, n.currencySign, n.unit,
		n.unitDisplay, n.useGrouping, n.notation, n.compactDisplay, n.signDisplay,
		strconv.Itoa(n.digits.minInt), strconv.Itoa(n.digits.minFrac),
		strconv.Itoa(n.digits.maxFrac), strconv.Itoa(n.digits.minSig),
		strconv.Itoa(n.digits.maxSig),
	}, "\t")
}

func parseNumberOptions(s string) numberOptions {
	f := strings.Split(s, "\t")
	if len(f) != 16 {
		return defaultNumberOptions()
	}
	i := func(k int) int { v, _ := strconv.Atoi(f[k]); return v }
	return numberOptions{tag: f[0], style: f[1], currency: f[2], currencyDisplay: f[3],
		currencySign: f[4], unit: f[5], unitDisplay: f[6], useGrouping: f[7],
		notation: f[8], compactDisplay: f[9], signDisplay: f[10],
		digits: pluralOptions{minInt: i(11), minFrac: i(12), maxFrac: i(13),
			minSig: i(14), maxSig: i(15)}}
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
	d, e := rt.intlDigitOptions(options, minFrac, maxFrac)
	if e != nil {
		return n, e
	}
	n.digits = d

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

// format renders a Number through the resolved options, on top of the
// locale-aware grouping and separators intl_format.go already does.
func (rt *Runtime) formatNumberWith(n numberOptions, li localeInfo, v float64) string {
	switch {
	case v != v:
		return li.nan
	case math.IsInf(v, 0):
		s := li.inf
		if v < 0 {
			s = li.minus + s
		}
		return s
	}
	if n.style == "percent" {
		v *= 100
	}

	neg := math.Signbit(v)
	digits := strings.TrimPrefix(numberToString(math.Abs(v)), "-")
	intPart, frac := expandDecimal(digits)
	if n.digits.maxSig > 0 {
		intPart, frac = roundToSignificant(intPart, frac, n.digits.maxSig, n.digits.minSig)
	} else {
		intPart, frac = roundFraction(intPart, frac, n.digits.maxFrac)
		for len(frac) < n.digits.minFrac {
			frac += "0"
		}
	}
	for len(intPart) < n.digits.minInt {
		intPart = "0" + intPart
	}

	var b strings.Builder
	// Rounding can turn a negative number into a zero, and a signed zero is
	// not what "-0.0000001 to two places" should print.
	zero := strings.Trim(intPart, "0") == "" && strings.Trim(frac, "0") == ""
	switch n.signDisplay {
	case "never":
	case "always":
		if neg {
			b.WriteString(li.minus)
		} else {
			b.WriteString("+")
		}
	case "exceptZero":
		if zero {
			break
		}
		if neg {
			b.WriteString(li.minus)
		} else {
			b.WriteString("+")
		}
	default: // "auto" and "negative"
		if neg && !(zero && n.signDisplay == "negative") {
			b.WriteString(li.minus)
		}
	}

	if n.useGrouping == "" {
		b.WriteString(intPart)
	} else if n.useGrouping == "min2" && len(intPart) <= li.minGroup+3 {
		b.WriteString(intPart)
	} else {
		b.WriteString(li.groupInteger(intPart))
	}
	if frac != "" {
		b.WriteString(li.decimal)
		b.WriteString(frac)
	}

	switch n.style {
	case "percent":
		b.WriteString("%")
	case "currency":
		// Without CLDR's currency symbols and placement rules, the code is
		// what gets written -- which is exactly `currencyDisplay: "code"`, and
		// a truthful answer rather than a guessed symbol in a guessed position.
		return n.currency + " " + b.String()
	case "unit":
		return b.String() + " " + n.unit
	}
	return b.String()
}
