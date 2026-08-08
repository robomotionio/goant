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
	tag     string
	numeric string // "always" or "auto"
	style   string // "long", "short", "narrow"
}

func (r relTimeOptions) String() string { return r.tag + "\t" + r.numeric + "\t" + r.style }

func parseRelTimeOptions(s string) relTimeOptions {
	f := strings.Split(s, "\t")
	if len(f) != 3 {
		return relTimeOptions{tag: defaultLocale, numeric: "always", style: "long"}
	}
	return relTimeOptions{tag: f[0], numeric: f[1], style: f[2]}
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

// relTimeAuto is the wording numeric: "auto" uses in place of a number, where
// English has one. An empty string means there is none and the numeric form is
// used instead.
func relTimeAuto(unit string, v float64) string {
	if v != math.Trunc(v) {
		return ""
	}
	switch unit {
	case "day":
		switch v {
		case -1:
			return "yesterday"
		case 0:
			return "today"
		case 1:
			return "tomorrow"
		}
	case "year", "quarter", "month", "week":
		switch v {
		case -1:
			return "last " + unit
		case 0:
			return "this " + unit
		case 1:
			return "next " + unit
		}
	case "hour", "minute":
		if v == 0 {
			return "this " + unit
		}
	case "second":
		if v == 0 {
			return "now"
		}
	}
	return ""
}

// relTimeParts renders the phrase as spans. The numeric span carries the unit
// it counts, which is what formatToParts reports alongside its value.
func (r relTimeOptions) relTimeParts(v float64, unit string, li localeInfo) []relPart {
	if r.numeric == "auto" {
		if phrase := relTimeAuto(unit, v); phrase != "" {
			return []relPart{{numberPart{"literal", phrase}, ""}}
		}
	}
	// The count is formatted through the number rules, so grouping and the
	// locale's digits apply to "in 1,000 days" as they would anywhere else.
	n := defaultNumberOptions()
	n.tag = r.tag
	num := numberParts(n, li, math.Abs(v))

	word := unit
	if math.Abs(v) != 1 {
		word += "s"
	}
	var out []relPart
	add := func(typ, val, u string) {
		if val != "" {
			out = append(out, relPart{numberPart{typ, val}, u})
		}
	}
	past := math.Signbit(v)
	if !past {
		add("literal", "in ", "")
	}
	for _, p := range num {
		out = append(out, relPart{p, unit})
	}
	if past {
		add("literal", " "+word+" ago", "")
	} else {
		add("literal", " "+word, "")
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
	r := relTimeOptions{tag: defaultLocale, numeric: "always", style: "long"}
	if len(requested) > 0 {
		if t, ok := parseLangTag(requested[0]); ok {
			r.tag = t.languageID()
		} else {
			r.tag = requested[0]
		}
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return r, e
	}
	if _, _, e := rt.intlStringOption(options, "numberingSystem", nil); e != nil {
		return r, e
	}
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
