package engine

// Intl.DisplayNames.
//
// The names themselves are CLDR's and this engine does not carry them, so
// every lookup misses and `fallback` decides what comes back: the code, or
// undefined. That is the specification's own behaviour for a code the
// implementation has no name for, which is why the constructor is worth having
// even without the data -- `of("fr")` answering "fr" is a defined answer, and a
// missing constructor is not.

import "strings"

type displayOptions struct {
	tag       string
	kind      string // "language", "region", "script", "currency", "calendar", "dateTimeField"
	style     string // "narrow", "short", "long"
	fallback  string // "code", "none"
	langStyle string // languageDisplay: "dialect", "standard"
}

func (d displayOptions) String() string {
	return strings.Join([]string{d.tag, d.kind, d.style, d.fallback, d.langStyle}, "\t")
}

func parseDisplayOptions(s string) displayOptions {
	f := strings.Split(s, "\t")
	if len(f) != 5 {
		return displayOptions{tag: defaultLocale, kind: "language", style: "long",
			fallback: "code", langStyle: "dialect"}
	}
	return displayOptions{tag: f[0], kind: f[1], style: f[2], fallback: f[3], langStyle: f[4]}
}

// dateTimeFields is the closed list the "dateTimeField" type accepts.
var dateTimeFields = []string{
	"era", "year", "quarter", "month", "weekOfYear", "weekday", "day",
	"dayPeriod", "hour", "minute", "second", "timeZoneName",
}

// canonicalDisplayCode validates the code against the type's grammar and
// returns it in canonical case, which is what `of` answers with when there is
// no name for it. A code that does not match the grammar is a RangeError --
// the type decides what "malformed" means.
func canonicalDisplayCode(kind, code string) (string, bool) {
	switch kind {
	case "language":
		t, ok := parseLangTag(code)
		if !ok {
			return "", false
		}
		t.canonicalize()
		return t.String(), true
	case "region":
		if !isRegionSubtag(code) {
			return "", false
		}
		return asciiUpper(code), true
	case "script":
		if !isScriptSubtag(code) {
			return "", false
		}
		return tagTitle(code), true
	case "currency":
		if !isWellFormedCurrency(code) {
			return "", false
		}
		return asciiUpper(code), true
	case "calendar":
		if !isUnicodeType(code) {
			return "", false
		}
		return asciiLower(code), true
	case "dateTimeField":
		if !tagContains(dateTimeFields, code) {
			return "", false
		}
		return code, true
	}
	return "", false
}

func (rt *Runtime) requireDisplayNames(this Value) (displayOptions, *ThrowError) {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlDisplayOpts); v.IsString() {
			return parseDisplayOptions(rt.strGo(v)), nil
		}
	}
	return displayOptions{}, rt.typeError("not an Intl.DisplayNames")
}

func (rt *Runtime) initDisplayOptions(options Value, requested []string) (displayOptions, *ThrowError) {
	d := displayOptions{tag: defaultLocale, style: "long", fallback: "code", langStyle: "dialect"}
	if len(requested) > 0 {
		if t, ok := parseLangTag(requested[0]); ok {
			d.tag = t.languageID()
		} else {
			d.tag = requested[0]
		}
	}
	if options.IsNull() {
		return d, rt.typeError("Options must be an object")
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return d, e
	}
	style, ok, e := rt.intlStringOption(options, "style", []string{"narrow", "short", "long"})
	if e != nil {
		return d, e
	}
	if ok {
		d.style = style
	}
	// type has no default: an Intl.DisplayNames that does not say what it names
	// cannot name anything, so its absence is a TypeError rather than a choice.
	kind, ok, e := rt.intlStringOption(options, "type",
		[]string{"language", "region", "script", "currency", "calendar", "dateTimeField"})
	if e != nil {
		return d, e
	}
	if !ok {
		return d, rt.typeError("Intl.DisplayNames requires a type option")
	}
	d.kind = kind
	fallback, ok, e := rt.intlStringOption(options, "fallback", []string{"code", "none"})
	if e != nil {
		return d, e
	}
	if ok {
		d.fallback = fallback
	}
	langStyle, ok, e := rt.intlStringOption(options, "languageDisplay", []string{"dialect", "standard"})
	if e != nil {
		return d, e
	}
	if ok && d.kind == "language" {
		d.langStyle = langStyle
	}
	return d, nil
}
