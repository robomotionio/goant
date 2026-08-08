package engine

// CanonicalizeLocaleList and the locale negotiation every Intl constructor
// shares.

import (
	"sort"
	"strconv"
	"sync"

	"golang.org/x/text/language"
)

// canonicalizeLocaleList is ECMA-402 9.2.1. It returns the canonicalised tags
// in the order they were given, without duplicates: a String is one tag, an
// Intl.Locale is its own tag, and anything else is read as an array-like.
//
// The list is what supportedLocalesOf filters and what every constructor picks
// its locale from, so the structural check and the RangeError both live here
// rather than at each call site.
func (rt *Runtime) canonicalizeLocaleList(v Value) ([]string, *ThrowError) {
	if v.IsUndefined() {
		return nil, nil
	}
	var seen []string
	add := func(el Value) *ThrowError {
		tag := ""
		if t, ok := rt.localeBrand(el); ok {
			tag = t
		} else {
			if !el.IsString() && !el.IsObjectType() {
				return rt.typeError("Locale list elements must be strings or objects")
			}
			s, e := rt.toStringValue(el)
			if e != nil {
				return e
			}
			tag = rt.strGo(s)
		}
		canon, ok := canonicalizeLangTag(tag)
		if !ok {
			return rt.rangeError("Incorrect locale information provided: " + tag)
		}
		if !tagContains(seen, canon) {
			seen = append(seen, canon)
		}
		return nil
	}

	if v.IsString() {
		return seen, add(v)
	}
	if tag, ok := rt.localeBrand(v); ok {
		return seen, add(rt.newString(tag))
	}
	// Everything else is read as an array-like, including a primitive: a
	// Number is wrapped, and a property found on Number.prototype counts. That
	// is a strange thing for a script to do and the specification says to do
	// it, which is exactly the kind of thing a conformance suite checks.
	o, e := rt.toObjectValue(v)
	if e != nil {
		return nil, e
	}
	n, e := rt.lengthOf(o)
	if e != nil {
		return nil, e
	}
	// One index at a time, all the way through: an element's own ToString may
	// install the next one, so the reads cannot be batched ahead of the
	// conversions.
	for i := 0; i < n; i++ {
		has, e := rt.hasPropE(o, strconv.Itoa(i))
		if e != nil {
			return nil, e
		}
		if !has {
			continue
		}
		el, e := rt.getElement(o, mknum(float64(i)))
		if e != nil {
			return nil, e
		}
		if e := add(el); e != nil {
			return nil, e
		}
	}
	return seen, nil
}

// availableLocales is the set of tags the formatting tables actually carry,
// sorted. Negotiation is only honest about a locale if resolvedOptions will
// then report it, so this is the same table lookupLocale resolves against.
var availableLocales = sync.OnceValue(func() []string {
	out := make([]string, 0, len(localeTable)+len(languageDefaults))
	for tag := range localeTable {
		out = append(out, tag)
	}
	// A bare language counts as available when lookupLocale resolves it: "en"
	// is not a key of the table and "en-US" is, and answering "no data for
	// en" while formatting en perfectly well would be the dishonest half of
	// the pair.
	for lang := range languageDefaults {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
})

// bestAvailableLocale is ECMA-402 9.2.2: the longest prefix of the tag, cut at
// a hyphen, that the implementation has data for.
func bestAvailableLocale(available []string, tag string) (string, bool) {
	candidate := tag
	for {
		for _, a := range available {
			if a == candidate {
				return a, true
			}
		}
		i := lastHyphen(candidate)
		if i < 0 {
			return "", false
		}
		// A single-letter subtag before the hyphen is an extension singleton,
		// which is dropped along with the subtag it introduces.
		if i >= 2 && candidate[i-2] == '-' {
			i -= 2
		}
		candidate = candidate[:i]
	}
}

func lastHyphen(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '-' {
			return i
		}
	}
	return -1
}

// localeIsAvailable says whether this engine has anything for a tag. Two
// different kinds of data qualify: the formatting tables, which cover a fixed
// list, and the collation and plural rules, which cover every language CLDR
// knows -- so a tag whose language x/text recognises counts even when no
// pattern table names it. "xyz" is well formed and no language at all, and
// does not.
func localeIsAvailable(tag string) bool {
	t, ok := parseLangTag(tag)
	if !ok {
		return false
	}
	if _, ok := bestAvailableLocale(availableLocales(), t.languageID()); ok {
		return true
	}
	_, err := language.Parse(t.lang)
	return err == nil
}

// lookupMatcher is ECMA-402 9.2.3: the first requested locale there is data
// for, keywords and all, or the default when there is none. Echoing back a
// locale we have nothing for would make resolvedOptions disagree with
// supportedLocalesOf, which is the pair a script uses to find out.
func lookupMatcher(requested []string) string {
	for _, tag := range requested {
		if localeIsAvailable(tag) {
			return tag
		}
	}
	return defaultLocale
}

// lookupSupportedLocales is ECMA-402 9.2.6, keeping the requested tag rather
// than the prefix it matched on -- that is what supportedLocalesOf returns.
func lookupSupportedLocales(requested []string) []string {
	var out []string
	avail := availableLocales()
	for _, tag := range requested {
		if _, ok := bestAvailableLocale(avail, tagNoExtensions(tag)); ok {
			out = append(out, tag)
		}
	}
	return out
}

// tagNoExtensions drops the -u-/-t-/-x- tail, which negotiation ignores.
func tagNoExtensions(tag string) string {
	t, ok := parseLangTag(tag)
	if !ok {
		return tag
	}
	return t.languageID()
}

// newArrayOfStrings builds a plain Array from a Go slice of tags or values.
func (rt *Runtime) newArrayOfStrings(ss []string) Value {
	arr := rt.newArray()
	ao := rt.objPtr(arr)
	for i, s := range ss {
		rt.arraySet(ao, uint32(i), rt.newString(s))
	}
	return arr
}

// supportedLocalesOf is ECMA-402 9.2.9, shared by every service constructor.
// The localeMatcher option is read and validated even though both matchers
// answer the same here, because reading it is observable and a bad value is a
// RangeError.
func (rt *Runtime) supportedLocalesOf(locales, options Value) (Value, *ThrowError) {
	requested, e := rt.canonicalizeLocaleList(locales)
	if e != nil {
		return mkundef(), e
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return mkundef(), e
	}
	return rt.newArrayOfStrings(lookupSupportedLocales(requested)), nil
}

// sanctionedUnits is Table 2 of ECMA-402: the simple units a NumberFormat may
// be asked for. It is a closed list in the specification rather than data, so
// it is written out here rather than generated.
var sanctionedUnits = []string{
	"acre", "bit", "byte", "celsius", "centimeter", "day", "degree", "fahrenheit",
	"fluid-ounce", "foot", "gallon", "gigabit", "gigabyte", "gram", "hectare",
	"hour", "inch", "kilobit", "kilobyte", "kilogram", "kilometer", "liter",
	"megabit", "megabyte", "meter", "microsecond", "mile", "mile-scandinavian",
	"milliliter", "millimeter", "millisecond", "minute", "month", "nanosecond",
	"ounce", "percent", "petabyte", "pound", "second", "stone", "terabit",
	"terabyte", "week", "yard", "year",
}

// supportedValuesOf is ECMA-402 8.3.2. Each list says what this engine can
// actually do, which for most keys is a good deal less than ICU: reporting a
// calendar or a numbering system that nothing here implements would be a
// louder kind of wrong than reporting one.
func (rt *Runtime) supportedValuesOf(keyv Value) (Value, *ThrowError) {
	s, e := rt.toStringValue(keyv)
	if e != nil {
		return mkundef(), e
	}
	switch rt.strGo(s) {
	case "calendar":
		return rt.newArrayOfStrings(supportedCalendars), nil
	case "collation":
		return rt.newArrayOfStrings(collationNames()), nil
	case "currency":
		return rt.newArrayOfStrings(nil), nil
	case "numberingSystem":
		return rt.newArrayOfStrings(numberingSystemNames()), nil
	case "timeZone":
		return rt.newArrayOfStrings(availableTimeZones()), nil
	case "unit":
		return rt.newArrayOfStrings(sanctionedUnits), nil
	}
	return mkundef(), rt.rangeError("Invalid key: " + rt.strGo(s))
}

// zeroOffsetLinks are the spellings of UTC that are not UTC.
var zeroOffsetLinks = []string{"Etc/GMT", "Etc/GMT0", "Etc/UTC", "GMT", "GMT0"}

// availableTimeZones is the identifier set of intl_timezone.go, sorted.
var availableTimeZones = sync.OnceValue(func() []string {
	out := make([]string, 0, len(zoneDisplayNames))
	for id := range zoneDisplayNames {
		// The list is of CANONICAL identifiers, so every link is dropped and
		// the zone it links to is kept: Egypt and US/Aleutian are spellings of
		// Africa/Cairo and America/Adak, and the zero-offset ones are all
		// spellings of UTC. A formatter asked for a link still keeps it: the
		// identifier reported is the one that was requested, here as
		// everywhere.
		if tagContains(zeroOffsetLinks, id) {
			continue
		}
		// UTC is the canonical name of the zone at zero, whatever order the
		// alias table happens to list its spellings in.
		if id != "UTC" {
			if primary, ok := primaryTimeZone(id); ok && primary != asciiLower(id) {
				continue
			}
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
})
