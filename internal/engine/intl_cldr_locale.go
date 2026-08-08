package engine

// Reading the per-locale CLDR tables tools/gencldrlocale writes.
//
// The tables are string constants, which is read-only data in the binary and
// costs a host that never formats anything nothing at all. Each is turned into
// a map on first use, and the maps are keyed by the same canonical tag
// lookupLocale returns, so a formatter that has resolved its locale already has
// the key.

import (
	"strconv"
	"strings"
	"sync"
)

// eachLine calls f with the tab-separated fields of every line of a table.
func eachLine(data string, f func(fields []string)) {
	for len(data) > 0 {
		line := data
		if i := strings.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = ""
		}
		if line == "" {
			continue
		}
		f(strings.Split(line, "\t"))
	}
}

// pluralPatterns reads a "one=…;other=…" cell.
func pluralPatterns(cell string) map[string]string {
	if cell == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(cell, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			out[k] = v
		}
	}
	return out
}

// ---- numbers ----

// cldrNumberPattern is a locale's currency, accounting and percent patterns as
// CLDR writes them ("#,##0.00 ¤"), plus the fewest integer digits a number
// needs before its groups are separated.
type cldrNumberPattern struct {
	currency, accounting, percent string
	minGroup                      int
	rangePat, approxPat           string
}

var cldrNumbers = sync.OnceValue(func() map[string]cldrNumberPattern {
	out := map[string]cldrNumberPattern{}
	eachLine(cldrNumberData, func(f []string) {
		if len(f) < 5 {
			return
		}
		n, _ := strconv.Atoi(f[4])
		if n < 1 {
			n = 1
		}
		p := cldrNumberPattern{currency: f[1], accounting: f[2], percent: f[3], minGroup: n}
		if len(f) > 6 {
			p.rangePat, p.approxPat = f[5], f[6]
		}
		out[f[0]] = p
	})
	return out
})

// currencyAffix is where a currency symbol goes and what separates it from the
// number, read off the pattern: "¤#,##0.00" puts it in front with nothing
// between, "#,##0.00 ¤" after with a no-break space.
type currencyAffix struct {
	after     bool
	separator string
	parens    bool // an accounting negative is written in parentheses
}

func readCurrencyAffix(standard, accounting string) currencyAffix {
	var a currencyAffix
	pat, neg, _ := strings.Cut(accounting, ";")
	if pat == "" {
		pat = standard
	}
	a.parens = strings.Contains(neg, "(")
	i := strings.IndexRune(pat, '¤')
	if i < 0 {
		return a
	}
	rest := pat[i+len("¤"):]
	a.after = strings.ContainsAny(pat[:i], "#0")
	if a.after {
		// The separator sits between the number and the symbol, which is
		// everything after the last digit.
		if j := strings.LastIndexAny(pat[:i], "#0"); j >= 0 {
			a.separator = pat[j+1 : i]
		}
	} else {
		if j := strings.IndexAny(rest, "#0"); j >= 0 {
			a.separator = rest[:j]
		}
	}
	return a
}

// percentSeparator is what a locale puts between a number and its percent
// sign, which is a no-break space in German and nothing in English.
func percentSeparator(pattern string) string {
	i := strings.IndexByte(pattern, '%')
	if i < 0 {
		return ""
	}
	if j := strings.LastIndexAny(pattern[:i], "#0"); j >= 0 {
		return pattern[j+1 : i]
	}
	return ""
}

// ---- currency symbols ----

var cldrCurrencies = sync.OnceValue(func() map[string]map[string][2]string {
	out := map[string]map[string][2]string{}
	eachLine(cldrCurrencyData, func(f []string) {
		if len(f) < 2 {
			return
		}
		m := map[string][2]string{}
		for _, cell := range strings.Split(f[1], ",") {
			code, rest, ok := strings.Cut(cell, "=")
			if !ok {
				continue
			}
			sym, narrow, hasNarrow := strings.Cut(rest, "/")
			if !hasNarrow {
				narrow = sym
			}
			m[code] = [2]string{sym, narrow}
		}
		out[f[0]] = m
	})
	return out
})

// ---- compact patterns ----

// compactMagnitudes is how many powers of ten CLDR carries a compact pattern
// for, counted from a thousand.
const compactMagnitudes = 12

type compactTable [compactMagnitudes]map[string]string

var cldrCompacts = sync.OnceValue(func() map[string]compactTable {
	out := map[string]compactTable{}
	eachLine(cldrCompactData, func(f []string) {
		if len(f) < 2+compactMagnitudes {
			return
		}
		var t compactTable
		for i := 0; i < compactMagnitudes; i++ {
			t[i] = pluralPatterns(f[2+i])
		}
		out[f[0]+"\t"+f[1]] = t
	})
	return out
})

// ---- units ----

var cldrUnits = sync.OnceValue(func() map[string]map[string]string {
	out := map[string]map[string]string{}
	eachLine(cldrUnitData, func(f []string) {
		if len(f) < 4 {
			return
		}
		out[f[0]+"\t"+f[1]+"\t"+f[2]] = pluralPatterns(f[3])
	})
	return out
})

// ---- list patterns ----

// cldrList is the four patterns a list is built from.
type cldrList struct{ start, middle, end, two string }

var cldrLists = sync.OnceValue(func() map[string]cldrList {
	out := map[string]cldrList{}
	eachLine(cldrListData, func(f []string) {
		if len(f) < 6 {
			return
		}
		out[f[0]+"\t"+f[1]] = cldrList{f[2], f[3], f[4], f[5]}
	})
	return out
})

// ---- relative time ----

// cldrRelative is one unit's relative-time patterns in one width: the past and
// future patterns per plural category, and the names for the offsets from two
// back to two forward, empty where the locale has no name for one.
type cldrRelative struct {
	past, future map[string]string
	named        [5]string
}

var cldrRelatives = sync.OnceValue(func() map[string]cldrRelative {
	out := map[string]cldrRelative{}
	eachLine(cldrRelativeData, func(f []string) {
		if len(f) < 6 {
			return
		}
		var r cldrRelative
		r.past, r.future = pluralPatterns(f[3]), pluralPatterns(f[4])
		for i, s := range strings.SplitN(f[5], "|", 5) {
			if i < len(r.named) {
				r.named[i] = s
			}
		}
		out[f[0]+"\t"+f[1]+"\t"+f[2]] = r
	})
	return out
})

// ---- picking a pattern ----

// pluralPick is the pattern for a count, falling back to "other", which every
// locale has. The category comes from the plural rules the engine already
// carries for Intl.PluralRules.
func pluralPick(m map[string]string, tag string, n float64) string {
	if m == nil {
		return ""
	}
	// The category is decided by the number as it will be WRITTEN, not by its
	// value: Polish counts 123456.78 seconds as "sekundy" and 123456 of them
	// as "sekund", and the only difference is the two digits after the comma.
	// So the operands come from the same digit settings the count is formatted
	// with, which is the default three fraction digits.
	p := defaultPluralOptions()
	p.tag = tag
	if cat, ok := m[p.selectForm(pluralTag(tag), n)]; ok {
		return cat
	}
	return m["other"]
}

// unquoteCLDR removes the quoting CLDR uses to keep a literal out of the
// pattern syntax: "Mio'.'" is "Mio.", and "”" is one apostrophe.
func unquoteCLDR(s string) string {
	if !strings.ContainsRune(s, '\'') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		// Everything up to the closing quote is literal.
		j := strings.IndexByte(s[i+1:], '\'')
		if j < 0 {
			b.WriteString(s[i+1:])
			break
		}
		b.WriteString(s[i+1 : i+1+j])
		i += j + 1
	}
	return b.String()
}
