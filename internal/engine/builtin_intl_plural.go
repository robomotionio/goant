package engine

// Intl.PluralRules, over golang.org/x/text/feature/plural.
//
// The rules themselves are CLDR's and the library already carries them, in
// their compiled form, for every locale. What is written here is the part
// ECMA-402 adds: turning a Number into the six plural operands the rules are
// written against, which means formatting it first -- "1.0" is `one` in some
// locales and "1" is not, and the difference is only visible in how many
// fraction digits were asked for.

import (
	"math"
	"strconv"
	"strings"

	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

// pluralOptions is what a PluralRules instance resolved to. It is stored on the
// instance as a string because slots hold Values, and the shape is small enough
// that parsing it back costs less than a side table would.
type pluralOptions struct {
	tag      string // the locale as REQUESTED, not as localeTable resolved it
	ordinal  bool
	notation string
	compact  string // compactDisplay, only meaningful when notation is compact
	minInt   int
	minFrac  int
	maxFrac  int
	minSig   int // 0 when significant digits were not asked for
	maxSig   int

	roundingMode string
	roundingIncr int
	trailingZero string
	priority     string // the option as written
	// roundingType is which digit counts actually decide the rounding:
	// "fractionDigits", "significantDigits", or -- when both are in play and
	// the value picks between them -- "morePrecision"/"lessPrecision".
	roundingType string
	// computed is [[ComputedRoundingPriority]], which is what resolvedOptions
	// reports: it is the option unless the digit counts overrode it, which is
	// what compact notation choosing its own does.
	computed string
}

func (p pluralOptions) String() string {
	kind := "cardinal"
	if p.ordinal {
		kind = "ordinal"
	}
	return kind + "," + strconv.Itoa(p.minInt) + "," + strconv.Itoa(p.minFrac) +
		"," + strconv.Itoa(p.maxFrac) + "," + strconv.Itoa(p.minSig) + "," +
		strconv.Itoa(p.maxSig) + "," + p.notation + "," + p.compact + "," +
		p.roundingMode + "," + strconv.Itoa(p.roundingIncr) + "," +
		p.trailingZero + "," + p.priority + "," + p.roundingType + "," +
		p.computed + "," + p.tag
}

func parsePluralOptions(s string) pluralOptions {
	f := strings.Split(s, ",")
	if len(f) != 15 {
		return defaultPluralOptions()
	}
	n := func(i int) int { v, _ := strconv.Atoi(f[i]); return v }
	return pluralOptions{ordinal: f[0] == "ordinal", minInt: n(1), minFrac: n(2),
		maxFrac: n(3), minSig: n(4), maxSig: n(5), notation: f[6], compact: f[7],
		roundingMode: f[8], roundingIncr: n(9), trailingZero: f[10], priority: f[11],
		roundingType: f[12], computed: f[13], tag: f[14]}
}

func defaultPluralOptions() pluralOptions {
	return pluralOptions{minInt: 1, maxFrac: 3, notation: "standard", tag: defaultLocale,
		roundingMode: "halfExpand", roundingIncr: 1, trailingZero: "auto", priority: "auto",
		roundingType: "fractionDigits", computed: "auto"}
}

// intlDigitOptions is SetNumberFormatDigitOptions, shared by PluralRules and
// NumberFormat. The two fraction defaults differ per caller -- money carries
// the currency's minor units -- which is why they are arguments.
//
// The five digit counts are READ before roundingPriority and CONVERTED after
// it. That is not a detail: which of them are consulted at all depends on the
// priority and on whether the notation is compact, and a getter on the options
// bag can see both the order of the reads and the fact that a count nobody
// needs is never converted.
func (rt *Runtime) intlDigitOptions(options Value, defMinFrac, defMaxFrac int, notation string) (pluralOptions, *ThrowError) {
	p := defaultPluralOptions()
	p.minFrac, p.maxFrac = defMinFrac, defMaxFrac
	raw := func(name string) (Value, *ThrowError) {
		if options.IsUndefined() {
			return mkundef(), nil
		}
		return rt.getField(options, name)
	}
	// DefaultNumberOption. The range is checked before the value is floored, so
	// 3.000001 is out of range for a maximum of three rather than rounding down
	// into it: the option was written as a number of digits and that is not one.
	def := func(v Value, name string, lo, hi, fallback int) (int, *ThrowError) {
		if v.IsUndefined() {
			return fallback, nil
		}
		num, e := rt.toNumber(v)
		if e != nil {
			return 0, e
		}
		if math.IsNaN(num) || num < float64(lo) || num > float64(hi) {
			return 0, rt.rangeError("Value " + numberToString(num) + " out of range for " + name)
		}
		return int(math.Floor(num)), nil
	}
	mnid, e := raw("minimumIntegerDigits")
	if e != nil {
		return p, e
	}
	if p.minInt, e = def(mnid, "minimumIntegerDigits", 1, 21, 1); e != nil {
		return p, e
	}
	mnfdV, e := raw("minimumFractionDigits")
	if e != nil {
		return p, e
	}
	mxfdV, e := raw("maximumFractionDigits")
	if e != nil {
		return p, e
	}
	mnsdV, e := raw("minimumSignificantDigits")
	if e != nil {
		return p, e
	}
	mxsdV, e := raw("maximumSignificantDigits")
	if e != nil {
		return p, e
	}
	incr, hasIncr, e := rt.intlNumberOption(options, "roundingIncrement", 1, 5000, 1)
	if e != nil {
		return p, e
	}
	if hasIncr && !intContains(validRoundingIncrements, incr) {
		return p, rt.rangeError("Invalid roundingIncrement")
	}
	p.roundingIncr = incr
	mode, ok, e := rt.intlStringOption(options, "roundingMode", roundingModes)
	if e != nil {
		return p, e
	}
	if ok {
		p.roundingMode = mode
	}
	priority, ok, e := rt.intlStringOption(options, "roundingPriority",
		[]string{"auto", "morePrecision", "lessPrecision"})
	if e != nil {
		return p, e
	}
	if ok {
		p.priority = priority
	}
	trailing, ok, e := rt.intlStringOption(options, "trailingZeroDisplay",
		[]string{"auto", "stripIfInteger"})
	if e != nil {
		return p, e
	}
	if ok {
		p.trailingZero = trailing
	}

	hasSd := !mnsdV.IsUndefined() || !mxsdV.IsUndefined()
	hasFd := !mnfdV.IsUndefined() || !mxfdV.IsUndefined()
	needSd, needFd := true, true
	if p.priority == "auto" {
		// Without a priority the significant digits win outright where they
		// were asked for, and compact notation with no digit options at all
		// asks for neither: it chooses for itself, below.
		needSd = hasSd
		if hasSd || (!hasFd && notation == "compact") {
			needFd = false
		}
	}
	if needSd {
		if hasSd {
			mnsd, e := def(mnsdV, "minimumSignificantDigits", 1, 21, 1)
			if e != nil {
				return p, e
			}
			mxsd, e := def(mxsdV, "maximumSignificantDigits", mnsd, 21, 21)
			if e != nil {
				return p, e
			}
			p.minSig, p.maxSig = mnsd, mxsd
		} else {
			p.minSig, p.maxSig = 1, 21
		}
	}
	if needFd {
		if hasFd {
			// Whichever end was left out takes its default against the end that
			// was given, so asking for at most one fraction digit of a currency
			// narrows its two-digit minimum rather than contradicting it.
			mnfd, mxfd := -1, -1
			if !mnfdV.IsUndefined() {
				if mnfd, e = def(mnfdV, "minimumFractionDigits", 0, 100, 0); e != nil {
					return p, e
				}
			}
			if !mxfdV.IsUndefined() {
				if mxfd, e = def(mxfdV, "maximumFractionDigits", 0, 100, 0); e != nil {
					return p, e
				}
			}
			switch {
			case mnfd < 0:
				mnfd = min(defMinFrac, mxfd)
			case mxfd < 0:
				mxfd = max(defMaxFrac, mnfd)
			case mnfd > mxfd:
				return p, rt.rangeError("minimumFractionDigits is greater than maximumFractionDigits")
			}
			p.minFrac, p.maxFrac = mnfd, mxfd
		} else {
			p.minFrac, p.maxFrac = defMinFrac, defMaxFrac
		}
	}
	switch {
	case !needSd && !needFd:
		// Compact notation deciding for itself: at most two significant digits
		// and no fraction digits, whichever of the two says more. That is what
		// makes 1234 "1.2K" and 987654321 "988M".
		p.minFrac, p.maxFrac = 0, 0
		p.minSig, p.maxSig = 1, 2
		p.roundingType, p.computed = "morePrecision", "morePrecision"
	case p.priority == "morePrecision", p.priority == "lessPrecision":
		p.roundingType, p.computed = p.priority, p.priority
	case hasSd:
		p.roundingType, p.computed = "significantDigits", "auto"
	default:
		p.roundingType, p.computed = "fractionDigits", "auto"
	}
	// An increment names the last place, so there has to BE one: not with
	// significant digits, not with a priority that picks between two roundings,
	// and not with an open-ended maximum.
	if p.roundingIncr != 1 {
		if p.roundingType != "fractionDigits" {
			return p, rt.typeError("roundingIncrement cannot be combined with significant digits or a rounding priority")
		}
		if p.minFrac != p.maxFrac {
			return p, rt.rangeError("roundingIncrement requires minimumFractionDigits to equal maximumFractionDigits")
		}
	}
	return p, nil
}

// pluralOperands is the (i, v, w, f, t) of the plural rules, computed from the
// number as it would be WRITTEN with these digit options rather than from the
// number itself. That is the whole subtlety: 1 and 1.0 are the same value and
// need not be the same plural category.
func pluralOperands(n float64, p pluralOptions) (i, v, w, f, tOp int) {
	n = math.Abs(n)
	if math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, 0, 0, 0, 0
	}
	intPart, frac := expandDecimal(numberToString(n))
	if p.maxSig > 0 {
		intPart, frac = roundToSignificant(intPart, frac, p.maxSig, p.minSig, "halfExpand", false)
	} else {
		intPart, frac = roundFraction(intPart, frac, p.maxFrac)
		for len(frac) < p.minFrac {
			frac += "0"
		}
	}
	// The integer operand saturates rather than wrapping: the rules only ever
	// compare it against small numbers and a modulus, and a tag with twenty
	// digits in it has the same answer as one with nine.
	if len(intPart) > 9 {
		i = 999999999
	} else {
		i, _ = strconv.Atoi(intPart)
	}
	v = len(frac)
	trimmed := strings.TrimRight(frac, "0")
	w = len(trimmed)
	if frac != "" {
		f, _ = strconv.Atoi(frac)
	}
	if trimmed != "" {
		tOp, _ = strconv.Atoi(trimmed)
	}
	return
}

// roundToSignificant rounds to at most maxSig significant digits and pads the
// fraction out to minSig of them, which is what the significant-digit options
// mean once the number is written down.
func roundToSignificant(intPart, frac string, maxSig, minSig int, mode string, neg bool) (string, string) {
	digits := strings.TrimLeft(intPart, "0")
	lead := len(digits)
	if lead == 0 {
		// 0.00123: the significant digits start after the leading zeros.
		z := len(frac) - len(strings.TrimLeft(frac, "0"))
		if z < len(frac) {
			intPart, frac = roundDecimal(intPart, frac, z+maxSig, mode, 1, neg)
			frac = strings.TrimRight(frac, "0")
		}
	} else if lead > maxSig {
		// More integer digits than significant digits asked for, so the
		// rounding happens above the decimal point: 123456 to three
		// significant digits is 123000, not 123456. roundFraction cannot
		// express that, so the digits are rounded in place and the tail
		// replaced with zeros.
		// Rounding above the point still obeys the mode: the digits after the
		// cut are the remainder, and roundDecimal is the one that knows what
		// each mode does with one.
		cut := digits[:maxSig] + "." + digits[maxSig:]
		ci, _ := expandDecimal(cut)
		ri, _ := roundDecimal(ci, digits[maxSig:], 0, mode, 1, neg)
		if len(ri) > 0 {
			return ri + strings.Repeat("0", lead-maxSig), ""
		}
		kept := []byte(digits[:maxSig])
		if digits[maxSig] >= '5' {
			i := len(kept) - 1
			for ; i >= 0; i-- {
				if kept[i] < '9' {
					kept[i]++
					break
				}
				kept[i] = '0'
			}
			if i < 0 {
				// 999 rounded up is 1000, which is one digit longer: the zeros
				// that follow are counted from maxSig, not from what is now in
				// hand, so 999999 becomes 1000000 rather than 100000.
				kept = append([]byte{'1'}, kept...)
			}
		}
		return string(kept) + strings.Repeat("0", lead-maxSig), ""
	} else {
		intPart, frac = roundDecimal(intPart, frac, maxSig-lead, mode, 1, neg)
		frac = strings.TrimRight(frac, "0")
	}
	sig := lead + len(strings.TrimRight(frac, "0"))
	if lead == 0 {
		sig = len(strings.TrimRight(frac, "0")) - (len(frac) - len(strings.TrimLeft(frac, "0")))
		if sig < 0 {
			sig = 0
		}
	}
	// Zero counts as one significant digit -- the one that is written -- so it
	// pads out like any other number: three significant digits of 0 is "0.00".
	if sig == 0 {
		return intPart, strings.Repeat("0", max(minSig-1, 0))
	}
	for sig < minSig {
		frac += "0"
		sig++
	}
	return intPart, frac
}

var pluralFormNames = map[plural.Form]string{
	plural.Other: "other", plural.Zero: "zero", plural.One: "one",
	plural.Two: "two", plural.Few: "few", plural.Many: "many",
}

func (p pluralOptions) rules() *plural.Rules {
	if p.ordinal {
		return plural.Ordinal
	}
	return plural.Cardinal
}

func (p pluralOptions) selectForm(tag language.Tag, n float64) string {
	i, v, w, f, t := pluralOperands(n, p)
	return pluralFormNames[p.rules().MatchPlural(tag, i, v, w, f, t)]
}

// pluralCategories is the set of forms this locale can produce. x/text does not
// export the rule set's form list, so it is discovered by asking: every integer
// up to two hundred plus a handful of fractional values covers every category
// CLDR defines, because the rules are written in terms of small numbers, the
// last one or two digits, and whether there is a fraction at all.
func (p pluralOptions) categories(tag language.Tag) []string {
	seen := map[string]bool{}
	for n := 0; n <= 200; n++ {
		seen[p.selectForm(tag, float64(n))] = true
	}
	for _, n := range []float64{0.1, 0.5, 1.1, 1.5, 2.5, 3.5, 10.1, 11.5, 100.5, 1000000} {
		seen[p.selectForm(tag, n)] = true
	}
	// CLDR's order, which is what the specification means by "the categories
	// in the order zero, one, two, few, many, other" -- not alphabetical.
	var out []string
	for _, form := range []string{"zero", "one", "two", "few", "many", "other"} {
		if seen[form] {
			out = append(out, form)
		}
	}
	return out
}

// requirePluralRules is RequireInternalSlot([[InitializedPluralRules]]): the
// methods live on the prototype and so can be called with any receiver, and
// Intl.PluralRules.prototype is itself not a PluralRules.
func (rt *Runtime) requirePluralRules(this Value) (pluralOptions, *ThrowError) {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlPluralOpts); v.IsString() {
			return parsePluralOptions(rt.strGo(v)), nil
		}
	}
	return pluralOptions{}, rt.typeError("not an Intl.PluralRules")
}

func (rt *Runtime) pluralOptionsOf(this Value) pluralOptions {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlPluralOpts); v.IsString() {
			return parsePluralOptions(rt.strGo(v))
		}
	}
	return defaultPluralOptions()
}

// pluralTag turns the instance's resolved locale into the language.Tag x/text
// matches rules against. A tag it cannot parse falls back to the root locale,
// whose rules are "everything is other" -- which is the honest answer for a
// language whose rules are not carried.
func pluralTag(tag string) language.Tag {
	t, err := language.Parse(tag)
	if err != nil {
		return language.Und
	}
	return t
}

// decimalExponent is the power of ten of a number's leading digit: 1 for 12.3,
// -3 for 0.00456, and 0 for a zero, which is the exponent ToRawPrecision gives
// it. It is what turns a count of significant digits into the PLACE they round
// to, which is how the two roundings are compared.
func decimalExponent(intPart, frac string) int {
	if d := strings.TrimLeft(intPart, "0"); d != "" {
		return len(d) - 1
	}
	zeros := len(frac) - len(strings.TrimLeft(frac, "0"))
	if zeros == len(frac) {
		return 0 // the number is zero
	}
	return -zeros - 1
}
