package engine

// Numbering systems.
//
// A numbering system with a "simple digit mapping" is exactly that: its ten
// digits are ten consecutive code points, so rendering a number in it is a
// translation of the ASCII digits and nothing more. That is every system CLDR
// carries except the four algorithmic ones (Roman numerals and the like),
// which is why this is a table of one code point each rather than a formatter.

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// numberingZero maps a numbering system to the code point of its zero.
var numberingZero = sync.OnceValue(func() map[string]rune {
	m := map[string]rune{}
	for name, v := range parseAliasTable(cldrNumberingSystemsData) {
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		m[name] = rune(n)
	}
	return m
})

// numberingSystemNames is every system with a simple digit mapping, sorted --
// what Intl.supportedValuesOf("numberingSystem") answers.
var numberingSystemNames = sync.OnceValue(func() []string {
	m := numberingZero()
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
})

// isNumberingSystem reports whether a name is one this engine can render in.
func isNumberingSystem(name string) bool {
	_, ok := numberingZero()[name]
	return ok
}

// mapDigits rewrites ASCII digits into the numbering system's own. Everything
// else in the string -- separators, signs, letters -- is left alone, because a
// numbering system is about digits and not about anything else.
func mapDigits(s, system string) string {
	zero, ok := numberingZero()[system]
	if !ok || zero == '0' {
		return s
	}
	if !strings.ContainsFunc(s, func(r rune) bool { return r >= '0' && r <= '9' }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + utf8.UTFMax)
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(zero + (r - '0'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// resolveNumberingSystem is ResolveLocale for the "nu" key: the option wins
// where it names a system we have, the tag's -u-nu is the fallback, and the
// keyword survives into the resolved locale only when the value came from it.
// The tag is returned with the keyword kept or dropped accordingly.
func resolveNumberingSystem(tag, option string) (resolvedTag, system string) {
	t, ok := parseLangTag(tag)
	if !ok {
		return tag, "latn"
	}
	kept := &langTag{lang: t.lang, script: t.script, region: t.region, variants: t.variants}
	system = "latn"
	// The tag's value is used unless the option names a system we have and a
	// different one -- an option we cannot honour is not an override. And the
	// keyword survives into the resolved locale exactly when its value is the
	// one being used, which is ResolveLocale's rule.
	usable := option != "" && isNumberingSystem(option)
	if v, has := t.uKeyword("nu"); has && isNumberingSystem(v) {
		if !usable || option == v {
			system = v
			kept.setUKeyword("nu", v)
		}
	}
	if usable {
		system = option
	}
	return kept.String(), system
}

// cldrCollations is the set of collation types a -u-co keyword may name. A
// well-formed type that is not one of these is not a collation, so it is
// dropped rather than carried into the resolved locale.
var cldrCollations = sync.OnceValue(func() map[string]string { return parseAliasTable(cldrCollationsData) })

func isCollationType(s string) bool {
	_, ok := cldrCollations()[s]
	return ok
}

// collationNames is every collation type, sorted -- what
// Intl.supportedValuesOf("collation") answers.
var collationNames = sync.OnceValue(func() []string {
	m := cldrCollations()
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
})
