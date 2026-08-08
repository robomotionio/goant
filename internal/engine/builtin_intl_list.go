package engine

// Intl.ListFormat.
//
// The patterns are English's. CLDR carries a set per locale and per style, and
// they differ in small ways (the Oxford comma, the word for "or", whether a
// two-item list uses a different pattern from the tail of a longer one). This
// ships the machinery with one locale's data rather than pretending to more:
// a locale we have no patterns for formats as English rather than failing, and
// the generator that fixes that is the same one A4 needs.

import "strings"

type listOptions struct {
	tag   string
	kind  string // "conjunction", "disjunction", "unit"
	style string // "long", "short", "narrow"
}

func (l listOptions) String() string { return l.tag + "\t" + l.kind + "\t" + l.style }

func parseListOptions(s string) listOptions {
	f := strings.Split(s, "\t")
	if len(f) != 3 {
		return listOptions{tag: defaultLocale, kind: "conjunction", style: "long"}
	}
	return listOptions{tag: f[0], kind: f[1], style: f[2]}
}

// patterns returns the four CLDR list patterns: the one for a two-item list,
// and the start, middle and end pieces of a longer one.
func (l listOptions) patterns() (two, start, middle, end string) {
	switch l.kind {
	case "disjunction":
		return "{0} or {1}", "{0}, {1}", "{0}, {1}", "{0}, or {1}"
	case "unit":
		if l.style == "narrow" {
			return "{0} {1}", "{0} {1}", "{0} {1}", "{0} {1}"
		}
		return "{0}, {1}", "{0}, {1}", "{0}, {1}", "{0}, {1}"
	}
	if l.style == "narrow" {
		return "{0}, {1}", "{0}, {1}", "{0}, {1}", "{0}, {1}"
	}
	if l.style == "short" {
		return "{0} & {1}", "{0}, {1}", "{0}, {1}", "{0}, & {1}"
	}
	return "{0} and {1}", "{0}, {1}", "{0}, {1}", "{0}, and {1}"
}

// listParts builds the element/literal spans CreatePartsFromList produces. The
// patterns nest left to right: the accumulated head is {0} and the next
// element is {1}, so the literals come out in the order they are written.
func (l listOptions) listParts(items []string) []numberPart {
	two, start, middle, end := l.patterns()
	switch len(items) {
	case 0:
		return nil
	case 1:
		return []numberPart{{"element", items[0]}}
	case 2:
		return applyListPattern([]numberPart{{"element", items[0]}}, items[1], two)
	}
	parts := applyListPattern([]numberPart{{"element", items[0]}}, items[1], start)
	for i := 2; i < len(items)-1; i++ {
		parts = applyListPattern(parts, items[i], middle)
	}
	return applyListPattern(parts, items[len(items)-1], end)
}

// applyListPattern substitutes head for {0} and next for {1}, keeping whatever
// is between them as a literal.
func applyListPattern(head []numberPart, next, pattern string) []numberPart {
	before, rest, _ := strings.Cut(pattern, "{0}")
	between, after, _ := strings.Cut(rest, "{1}")
	var out []numberPart
	if before != "" {
		out = append(out, numberPart{"literal", before})
	}
	out = append(out, head...)
	if between != "" {
		out = append(out, numberPart{"literal", between})
	}
	out = append(out, numberPart{"element", next})
	if after != "" {
		out = append(out, numberPart{"literal", after})
	}
	return out
}

// listStrings reads the iterable argument. Every element must already be a
// String -- ListFormat does not coerce, because a list of numbers is far more
// likely to be a mistake than an intention.
func (rt *Runtime) listStrings(v Value) ([]string, *ThrowError) {
	if v.IsUndefined() {
		return nil, nil
	}
	var out []string
	e := rt.iterateWithClose(v, func(el Value) (bool, *ThrowError) {
		if !el.IsString() {
			return true, rt.typeError("Iterable yielded a non-string value")
		}
		out = append(out, rt.strGo(el))
		return false, nil
	})
	return out, e
}

func (rt *Runtime) requireListFormat(this Value) (listOptions, *ThrowError) {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlListOpts); v.IsString() {
			return parseListOptions(rt.strGo(v)), nil
		}
	}
	return listOptions{}, rt.typeError("not an Intl.ListFormat")
}

func (rt *Runtime) initListOptions(options Value, requested []string) (listOptions, *ThrowError) {
	l := listOptions{tag: defaultLocale, kind: "conjunction", style: "long"}
	if len(requested) > 0 {
		if t, ok := parseLangTag(requested[0]); ok {
			l.tag = t.languageID()
		} else {
			l.tag = requested[0]
		}
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return l, e
	}
	kind, ok, e := rt.intlStringOption(options, "type",
		[]string{"conjunction", "disjunction", "unit"})
	if e != nil {
		return l, e
	}
	if ok {
		l.kind = kind
	}
	style, ok, e := rt.intlStringOption(options, "style", []string{"long", "short", "narrow"})
	if e != nil {
		return l, e
	}
	if ok {
		l.style = style
	}
	return l, nil
}
