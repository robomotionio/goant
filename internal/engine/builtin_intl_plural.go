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
	priority     string
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
		p.trailingZero + "," + p.priority + "," + p.tag
}

func parsePluralOptions(s string) pluralOptions {
	f := strings.Split(s, ",")
	if len(f) != 13 {
		return defaultPluralOptions()
	}
	n := func(i int) int { v, _ := strconv.Atoi(f[i]); return v }
	return pluralOptions{ordinal: f[0] == "ordinal", minInt: n(1), minFrac: n(2),
		maxFrac: n(3), minSig: n(4), maxSig: n(5), notation: f[6], compact: f[7],
		roundingMode: f[8], roundingIncr: n(9), trailingZero: f[10], priority: f[11],
		tag: f[12]}
}

func defaultPluralOptions() pluralOptions {
	return pluralOptions{minInt: 1, maxFrac: 3, notation: "standard", tag: defaultLocale,
		roundingMode: "halfExpand", roundingIncr: 1, trailingZero: "auto", priority: "auto"}
}

// intlDigitOptions is SetNumberFormatDigitOptions, shared by PluralRules and --
// when it grows options -- NumberFormat. The defaults differ per caller, which
// is why they are arguments rather than constants.
func (rt *Runtime) intlDigitOptions(options Value, defMinFrac, defMaxFrac int) (pluralOptions, *ThrowError) {
	p := defaultPluralOptions()
	p.minFrac, p.maxFrac = defMinFrac, defMaxFrac
	get := func(name string, lo, hi, fallback int) (int, bool, *ThrowError) {
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
	var e *ThrowError
	if p.minInt, _, e = get("minimumIntegerDigits", 1, 21, 1); e != nil {
		return p, e
	}
	// The read order is the specification's -- integer, then fraction, then
	// significant -- and it is observable through getters on the options bag.
	minFrac, hasMinFrac, e := get("minimumFractionDigits", 0, 100, defMinFrac)
	if e != nil {
		return p, e
	}
	maxFrac, hasMaxFrac, e := get("maximumFractionDigits", 0, 100, defMaxFrac)
	if e != nil {
		return p, e
	}
	minSig, hasMinSig, e := get("minimumSignificantDigits", 1, 21, 1)
	if e != nil {
		return p, e
	}
	maxSig, hasMaxSig, e := get("maximumSignificantDigits", 1, 21, 21)
	if e != nil {
		return p, e
	}
	if hasMinSig || hasMaxSig {
		p.minSig, p.maxSig = minSig, maxSig
		if p.minSig > p.maxSig {
			return p, rt.rangeError("minimumSignificantDigits is greater than maximumSignificantDigits")
		}
		return p, nil
	}
	p.minFrac, p.maxFrac = minFrac, maxFrac
	if hasMinFrac && !hasMaxFrac && p.maxFrac < p.minFrac {
		p.maxFrac = p.minFrac
	}
	// The other way round too: a currency's default minimum is two digits, and
	// asking for at most one is a narrowing rather than a contradiction.
	if hasMaxFrac && !hasMinFrac && p.minFrac > p.maxFrac {
		p.minFrac = p.maxFrac
	}
	if p.minFrac > p.maxFrac {
		return p, rt.rangeError("minimumFractionDigits is greater than maximumFractionDigits")
	}
	return p, nil
}

// intlRoundingOptions is the tail of SetNumberFormatDigitOptions: the four
// options read after the digits, in that order, which a getter can observe.
func (rt *Runtime) intlRoundingOptions(p *pluralOptions, options Value) *ThrowError {
	incr, hasIncr, e := rt.intlNumberOption(options, "roundingIncrement", 1, 5000, 1)
	if e != nil {
		return e
	}
	if hasIncr && !intContains(validRoundingIncrements, incr) {
		return rt.rangeError("Invalid roundingIncrement")
	}
	p.roundingIncr = incr
	mode, ok, e := rt.intlStringOption(options, "roundingMode", roundingModes)
	if e != nil {
		return e
	}
	if ok {
		p.roundingMode = mode
	}
	priority, ok, e := rt.intlStringOption(options, "roundingPriority",
		[]string{"auto", "morePrecision", "lessPrecision"})
	if e != nil {
		return e
	}
	if ok {
		p.priority = priority
	}
	trailing, ok, e := rt.intlStringOption(options, "trailingZeroDisplay",
		[]string{"auto", "stripIfInteger"})
	if e != nil {
		return e
	}
	if ok {
		p.trailingZero = trailing
	}
	return nil
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
	// Zero has one significant digit and it is already written: padding it out
	// to the minimum would make "0" into "0.0", which says no more than "0".
	if sig == 0 {
		return intPart, strings.TrimRight(frac, "0")
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
