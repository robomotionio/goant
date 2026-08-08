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
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
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
	return []byte(c.String(string(b)))
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
