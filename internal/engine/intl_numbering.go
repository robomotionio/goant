package engine

// Numbering systems.
//
// A numbering system with a "simple digit mapping" is exactly that: it has ten
// digits and a number is spelled one digit at a time, so rendering a number in
// it is a translation of the ASCII digits and nothing more. That is every
// system CLDR carries except the four algorithmic ones (Roman numerals and the
// like), which is why this is a table of digits rather than a formatter.
//
// The digits are usually consecutive code points, but hanidec's are not --
// 〇 is U+3007 and 一 is U+4E00 -- so all ten are carried.

import (
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// numberingDigits maps a numbering system to its ten digits, zero first.
var numberingDigits = sync.OnceValue(func() map[string][]rune {
	m := map[string][]rune{}
	for name, v := range parseAliasTable(cldrNumberingSystemsData) {
		d := []rune(v)
		if len(d) != 10 {
			continue
		}
		m[name] = d
	}
	return m
})

// numberingSystemNames is every system with a simple digit mapping, sorted --
// what Intl.supportedValuesOf("numberingSystem") answers.
var numberingSystemNames = sync.OnceValue(func() []string {
	m := numberingDigits()
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
})

// isNumberingSystem reports whether a name is one this engine can render in.
func isNumberingSystem(name string) bool {
	_, ok := numberingDigits()[name]
	return ok
}

// mapDigits rewrites ASCII digits into the numbering system's own. Everything
// else in the string -- separators, signs, letters -- is left alone, because a
// numbering system is about digits and not about anything else.
func mapDigits(s, system string) string {
	digits, ok := numberingDigits()[system]
	if !ok || digits[0] == '0' {
		return s
	}
	if !strings.ContainsFunc(s, func(r rune) bool { return r >= '0' && r <= '9' }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + utf8.UTFMax)
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(digits[r-'0'])
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

// languageCollations is the tailorings CLDR gives a language beyond the
// standard one. A collation not listed for the locale is not one it can be
// asked for: pinyin orders Chinese and says nothing about German.
var languageCollations = map[string][]string{
	"ar": {"compat"},
	"de": {"phonebk"},
	"es": {"trad"},
	"ja": {"unihan"},
	"ko": {"searchjl", "unihan"},
	"zh": {"big5han", "gb2312han", "pinyin", "stroke", "unihan", "zhuyin"},
}

// universalCollations are the two orderings that are not a language's own and
// may be asked for anywhere.
var universalCollations = []string{"emoji", "eor"}

// collationAvailable reports whether a locale can be ordered this way.
func collationAvailable(tag, collation string) bool {
	if tagContains(universalCollations, collation) {
		return true
	}
	lang := tag
	if i := strings.IndexByte(lang, '-'); i > 0 {
		lang = lang[:i]
	}
	return tagContains(languageCollations[asciiLower(lang)], collation)
}

// collationNames is every collation type, sorted -- what
// Intl.supportedValuesOf("collation") answers.
var collationNames = sync.OnceValue(func() []string {
	// Only the orderings a Collator will actually accept, which is the ones
	// some locale has: listing a collation nothing can be asked for would be
	// the same dishonesty as listing a locale with no data.
	seen := map[string]bool{}
	out := make([]string, 0, 16)
	add := func(name string) {
		if _, ok := cldrCollations()[name]; ok && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, name := range universalCollations {
		add(name)
	}
	for _, names := range languageCollations {
		for _, name := range names {
			add(name)
		}
	}
	sort.Strings(out)
	return out
})

// numberingSeparators overrides a locale's separators for the numbering
// systems that bring their own. Arabic-Indic digits are written with the
// Arabic decimal separator and thousands separator whatever locale asked for
// them, which is why "en-US-u-nu-arab" reads ١٫٥ and not ١.٥.
func numberingSeparators(system string) (decimal, group string, ok bool) {
	switch system {
	case "arab", "arabext":
		return "٫", "٬", true
	}
	return "", "", false
}
