package engine

// Locale-sensitive case mapping, over golang.org/x/text/cases.
//
// The default mapping and the Turkish one are different functions, not
// different renderings of the same one: "i" upper-cases to "I" almost
// everywhere and to "İ" in Turkish and Azerbaijani, and "I" lower-cases to "i"
// almost everywhere and to "ı" in those two. Lithuanian keeps the dot on a
// lower-case i under an accent. Everything else falls through to the fast
// default path in builtin_string.go.

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

// localeCaseLanguages are the languages whose case mapping differs from the
// default. Anything else is the default mapping, which the ASCII fast path
// already does well.
var localeCaseLanguages = []string{"tr", "az", "lt", "el"}

func localeCaseBytes(b []byte, tags []string, upper bool) []byte {
	lang := ""
	if len(tags) > 0 {
		if t, ok := parseLangTag(tags[0]); ok {
			lang = t.lang
		}
	}
	if !tagContains(localeCaseLanguages, lang) {
		if upper {
			return jsToUpperCase(b)
		}
		return jsToLowerCase(b)
	}
	tag, err := language.Parse(lang)
	if err != nil {
		if upper {
			return jsToUpperCase(b)
		}
		return jsToLowerCase(b)
	}
	// x/text works on well-formed UTF-8. A string carrying an unpaired
	// surrogate is not, and mapping it would replace the surrogate rather than
	// preserve it, so those keep the default path -- which does preserve them.
	if !validWTF8ForCasing(b) {
		if upper {
			return jsToUpperCase(b)
		}
		return jsToLowerCase(b)
	}
	c := cases.Lower(tag)
	if upper {
		c = cases.Upper(tag)
	}
	if !upper && lang == "lt" {
		return []byte(lithuanianLower(string(b), c))
	}
	return []byte(c.String(string(b)))
}

// lithuanianLower is the one rule x/text puts in the wrong place. Lowercasing
// I, J or I-with-ogonek in Lithuanian adds a COMBINING DOT ABOVE when an
// accent above follows, and the dot goes immediately after the letter, ahead of
// any marks below it: "I\u0325\u0300" is "i\u0307\u0325\u0300". Everything
// else is x/text's, run over the stretches between such letters.
func lithuanianLower(s string, c cases.Caser) string {
	var b strings.Builder
	start := 0
	for i, r := range s {
		lower := ""
		switch r {
		case 'I':
			lower = "i"
		case 'J':
			lower = "j"
		case 0x012E:
			lower = "\u012F"
		default:
			continue
		}
		if !moreAbove(s[i+utf8.RuneLen(r):]) {
			continue
		}
		b.WriteString(c.String(s[start:i]))
		b.WriteString(lower)
		b.WriteRune(0x0307)
		start = i + utf8.RuneLen(r)
	}
	b.WriteString(c.String(s[start:]))
	return b.String()
}

// moreAbove is the SpecialCasing condition of that name: an accent above
// follows, with nothing between it and the letter that is a base character or
// another accent above.
func moreAbove(s string) bool {
	for _, r := range s {
		switch norm.NFC.PropertiesString(string(r)).CCC() {
		case 230:
			return true
		case 0:
			return false
		}
	}
	return false
}

// validWTF8ForCasing reports whether the bytes decode without an unpaired
// surrogate, which is the one thing x/text cannot round-trip.
func validWTF8ForCasing(b []byte) bool {
	for i := 0; i < len(b); {
		if b[i] < 0x80 {
			i++
			continue
		}
		// The WTF-8 encoding of a surrogate starts ED A0..BF.
		if b[i] == 0xED && i+1 < len(b) && b[i+1] >= 0xA0 {
			return false
		}
		i++
	}
	return true
}
