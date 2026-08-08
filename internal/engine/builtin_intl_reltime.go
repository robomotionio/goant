package engine

// Intl.RelativeTimeFormat.
//
// As with ListFormat, the wording is English's: CLDR carries a phrase per
// locale, per unit, per style and per plural category, which is a table this
// engine does not yet generate. The machinery -- unit validation, the
// numeric: "auto" special cases, the parts -- is the part that is the same in
// every locale, and it is what is here.

import (
	"math"
	"strings"
)

type relTimeOptions struct {
	tag       string
	numeric   string // "always" or "auto"
	style     string // "long", "short", "narrow"
	numbering string
}

func (r relTimeOptions) String() string {
	return strings.Join([]string{r.tag, r.numeric, r.style, r.numbering}, "\t")
}

func parseRelTimeOptions(s string) relTimeOptions {
	f := strings.Split(s, "\t")
	if len(f) != 4 {
		return relTimeOptions{tag: defaultLocale, numeric: "always", style: "long", numbering: "latn"}
	}
	return relTimeOptions{tag: f[0], numeric: f[1], style: f[2], numbering: f[3]}
}

// relTimeUnits is the singular form of every unit the method accepts. The
// plural spellings are accepted too and mean the same thing.
var relTimeUnits = []string{
	"year", "quarter", "month", "week", "day", "hour", "minute", "second",
}

// singularUnit maps "days" to "day" and rejects anything that is not a unit.
func singularUnit(s string) (string, bool) {
	if tagContains(relTimeUnits, s) {
		return s, true
	}
	if t, ok := strings.CutSuffix(s, "s"); ok && tagContains(relTimeUnits, t) {
		return t, true
	}
	return "", false
}

// cldrWidth is the name CLDR gives a style: "long" is unsuffixed there as here,
// and the other two match.
func cldrWidth(style string) string {
	switch style {
	case "short", "narrow":
		return style
	}
	return "long"
}

// relTimeFor is the locale's relative-time patterns for one unit and style,
// falling back to the wider styles and then to English.
func relTimeFor(tag, style, unit string) (cldrRelative, bool) {
	widths := []string{cldrWidth(style)}
	switch widths[0] {
	case "narrow":
		widths = append(widths, "short", "long")
	case "short":
		widths = append(widths, "long")
	}
	for _, t := range []string{tag, "en-US"} {
		for _, w := range widths {
			if r, ok := cldrRelatives()[t+"\t"+w+"\t"+unit]; ok {
				return r, true
			}
		}
	}
	return cldrRelative{}, false
}

// relTimeAuto is the wording numeric: "auto" uses in place of a number, where
// the locale has one: "yesterday", "last year", "now". An empty string means
// there is none and the numeric form is used instead.
func relTimeAuto(tag, style, unit string, v float64) string {
	if v != math.Trunc(v) || v < -2 || v > 2 {
		return ""
	}
	r, ok := relTimeFor(tag, style, unit)
	if !ok {
		return ""
	}
	return r.named[int(v)+2]
}

// relTimeParts renders the phrase as spans. The numeric span carries the unit
// it counts, which is what formatToParts reports alongside its value.
func (r relTimeOptions) relTimeParts(v float64, unit string, li localeInfo) []relPart {
	if r.numeric == "auto" {
		if phrase := relTimeAuto(r.tag, r.style, unit, v); phrase != "" {
			return []relPart{{numberPart{"literal", phrase}, ""}}
		}
	}
	// The count is formatted through the number rules, so grouping and the
	// locale's digits apply to "in 1,000 days" as they would anywhere else.
	n := defaultNumberOptions()
	n.tag, n.numbering = r.tag, r.numbering
	num := numberParts(n, li, math.Abs(v))

	// The pattern says what goes on either side of the count, and which
	// pattern applies is the count's plural category: Polish has four.
	pattern := ""
	if cr, ok := relTimeFor(r.tag, r.style, unit); ok {
		side := cr.future
		if math.Signbit(v) {
			side = cr.past
		}
		pattern = pluralPick(side, r.tag, math.Abs(v))
	}
	if pattern == "" {
		word := unit
		if math.Abs(v) != 1 {
			word += "s"
		}
		pattern = "in {0} " + word
		if math.Signbit(v) {
			pattern = "{0} " + word + " ago"
		}
	}
	i := strings.Index(pattern, "{0}")
	if i < 0 {
		return []relPart{{numberPart{"literal", pattern}, ""}}
	}
	var out []relPart
	if head := unquoteCLDR(pattern[:i]); head != "" {
		out = append(out, relPart{numberPart{"literal", head}, ""})
	}
	for _, p := range num {
		out = append(out, relPart{p, unit})
	}
	if tail := unquoteCLDR(pattern[i+3:]); tail != "" {
		out = append(out, relPart{numberPart{"literal", tail}, ""})
	}
	return out
}

// relPart is a span with the unit it belongs to, or "" for the wording around
// it. formatToParts reports the unit only on the parts that count something.
type relPart struct {
	numberPart
	unit string
}

func (rt *Runtime) requireRelTimeFormat(this Value) (relTimeOptions, *ThrowError) {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlRelTimeOpts); v.IsString() {
			return parseRelTimeOptions(rt.strGo(v)), nil
		}
	}
	return relTimeOptions{}, rt.typeError("not an Intl.RelativeTimeFormat")
}

func (rt *Runtime) initRelTimeOptions(options Value, requested []string) (relTimeOptions, *ThrowError) {
	r := relTimeOptions{tag: defaultLocale, numeric: "always", style: "long", numbering: "latn"}
	if len(requested) > 0 {
		r.tag = lookupMatcher(requested)
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return r, e
	}
	ns, hasNS, e := rt.intlStringOption(options, "numberingSystem", nil)
	if e != nil {
		return r, e
	}
	if hasNS {
		if !isUnicodeType(ns) {
			return r, rt.rangeError("Invalid numberingSystem: " + ns)
		}
		ns = asciiLower(ns)
	} else {
		ns = ""
	}
	r.tag, r.numbering = resolveNumberingSystem(r.tag, ns)
	style, ok, e := rt.intlStringOption(options, "style", []string{"long", "short", "narrow"})
	if e != nil {
		return r, e
	}
	if ok {
		r.style = style
	}
	numeric, ok, e := rt.intlStringOption(options, "numeric", []string{"always", "auto"})
	if e != nil {
		return r, e
	}
	if ok {
		r.numeric = numeric
	}
	return r, nil
}

// relTimeArgs is the shared argument checking of format and formatToParts: a
// finite Number, and a unit this locale knows.
func (rt *Runtime) relTimeArgs(args []Value) (float64, string, *ThrowError) {
	v, e := rt.toNumber(arg(args, 0))
	if e != nil {
		return 0, "", e
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, "", rt.rangeError("Value must be finite")
	}
	s, e := rt.toStringValue(arg(args, 1))
	if e != nil {
		return 0, "", e
	}
	unit, ok := singularUnit(rt.strGo(s))
	if !ok {
		return 0, "", rt.rangeError("Invalid unit: " + rt.strGo(s))
	}
	return v, unit, nil
}

// unitPartsArray is partsArray for spans that carry a unit: the property is
// present only where there is one, which is how formatToParts distinguishes
// the number from the words around it.
func (rt *Runtime) unitPartsArray(parts []relPart) Value {
	arr := rt.newArray()
	ao := rt.objPtr(arr)
	for i, p := range parts {
		o := rt.newPlainObject()
		oo := rt.objPtr(o)
		oo.defineOwn("type", rt.newString(p.typ), attrDefault)
		oo.defineOwn("value", rt.newString(p.val), attrDefault)
		if p.unit != "" {
			oo.defineOwn("unit", rt.newString(p.unit), attrDefault)
		}
		rt.arraySet(ao, uint32(i), o)
	}
	return arr
}
