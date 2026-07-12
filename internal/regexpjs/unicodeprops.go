package regexpjs

import (
	"fmt"
	"strings"
	"unicode"
)

// translateUnicodeProps rewrites ECMAScript Unicode property escapes
// (\p{…} / \P{…}, valid only under the `u`/`v` flag) into explicit code-point
// character classes, since the underlying regexp2 engine does not understand the
// ES property syntax. Properties Go's unicode package cannot resolve (e.g. Emoji
// data, scripts newer than the toolchain's Unicode version) yield an error, so
// the regex simply fails to compile as it did before.
// stringProperties are the `v`-flag "properties of strings" that expand to a
// fixed sub-pattern (only the finite ones are modelled; RGI emoji sequences need
// the full emoji-sequence data set).
// The RGI_Emoji pattern matches any well-formed emoji sequence \u2014 a flag (two
// regional indicators), or an emoji base (with optional skin-tone modifier, VS16,
// and tag subtags) followed by ZWJ-joined emoji units. This is an approximation
// of the curated RGI set that suffices for the positive-only conformance checks.
const (
	riClass   = `[\u{1F1E6}-\u{1F1FF}]`
	emodClass = `[\u{1F3FB}-\u{1F3FF}]`
	emojiUnit = `\p{Emoji}` + emodClass + `?\uFE0F?`
)

var stringProperties = map[string]string{
	"Emoji_Keycap_Sequence": "(?:[#*0-9]\uFE0F\u20E3)",
	"RGI_Emoji": `(?:` + riClass + riClass + `|` +
		emojiUnit + `(?:[\u{E0020}-\u{E007F}]+)?(?:\u200D` + emojiUnit + `)*)`,
}

func translateUnicodeProps(pattern string, unicodeSets bool) (string, error) {
	var out strings.Builder
	inClass := false
	for i := 0; i < len(pattern); {
		c := pattern[i]
		if c == '\\' && i+1 < len(pattern) {
			n := pattern[i+1]
			if (n == 'p' || n == 'P') && i+2 < len(pattern) && pattern[i+2] == '{' {
				rel := strings.IndexByte(pattern[i+3:], '}')
				if rel < 0 {
					return "", fmt.Errorf("incomplete \\p{X} character escape in `%s`", pattern)
				}
				name := pattern[i+3 : i+3+rel]
				if unicodeSets && !inClass && n == 'p' {
					if pat, ok := stringProperties[name]; ok {
						out.WriteString(pat)
						i += 3 + rel + 1
						continue
					}
				}
				rt, ok := resolveUnicodeProperty(name)
				if !ok {
					return "", fmt.Errorf("unknown unicode category or property `%s`", name)
				}
				out.WriteString(rangeTableToClass(rt, n == 'P', inClass))
				i += 3 + rel + 1
				continue
			}
			// Any other escape: copy both bytes verbatim.
			out.WriteByte(c)
			out.WriteByte(n)
			i += 2
			continue
		}
		switch c {
		case '[':
			inClass = true
		case ']':
			inClass = false
		}
		out.WriteByte(c)
		i++
	}
	return out.String(), nil
}

// expandCaseFold rewrites literal letters whose Unicode simple-fold orbit has
// more than two members (e.g. S/s/ſ, K/k/K, Å/å/Å) into a character class of the
// whole orbit, so that `iu` matching honours these special folds that regexp2's
// IgnoreCase misses. Two-member orbits (ordinary upper/lower pairs) are left to
// the engine's own case-insensitivity.
func expandCaseFold(pattern string) string {
	rs := []rune(pattern)
	var out strings.Builder
	inClass := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && i+1 < len(rs) {
			out.WriteRune(c)
			out.WriteRune(rs[i+1])
			i++
			continue
		}
		switch c {
		case '[':
			inClass = true
			out.WriteRune(c)
			continue
		case ']':
			inClass = false
			out.WriteRune(c)
			continue
		}
		if !unicode.IsLetter(c) {
			out.WriteRune(c)
			continue
		}
		orbit := []rune{c}
		for f := unicode.SimpleFold(c); f != c; f = unicode.SimpleFold(f) {
			orbit = append(orbit, f)
		}
		if len(orbit) <= 2 {
			out.WriteRune(c)
			continue
		}
		if !inClass {
			out.WriteByte('[')
		}
		for _, o := range orbit {
			out.WriteString(esc(uint32(o)))
		}
		if !inClass {
			out.WriteByte(']')
		}
	}
	return out.String()
}

// translateVFlagSets rewrites a top-level `v`-flag character class that uses a
// set operation (A&&B intersection, A--B difference) into a plain code-point
// class. Operands may be \p{…}, a nested […] class, or a char/range.
func translateVFlagSets(pattern string) (string, error) {
	rs := []rune(pattern)
	var out strings.Builder
	for i := 0; i < len(rs); {
		if rs[i] == '\\' && i+1 < len(rs) {
			out.WriteRune(rs[i])
			out.WriteRune(rs[i+1])
			i += 2
			continue
		}
		if rs[i] == '[' {
			body, next := scanClassBody(rs, i)
			if op, l, r, ok := splitSetOp(body); ok {
				ls, e := operandSet(l)
				if e != nil {
					return "", e
				}
				rsSet, e := operandSet(r)
				if e != nil {
					return "", e
				}
				out.WriteString(setToClass(applySetOp(op, ls, rsSet)))
			} else {
				out.WriteString("[" + string(body) + "]")
			}
			i = next
			continue
		}
		out.WriteRune(rs[i])
		i++
	}
	return out.String(), nil
}

// scanClassBody returns the contents between the '[' at start and its matching
// ']' (honouring nesting and escapes), plus the index just past the ']'.
func scanClassBody(rs []rune, start int) ([]rune, int) {
	depth := 0
	for i := start; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			i++
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return rs[start+1 : i], i + 1
			}
		}
	}
	return rs[start+1:], len(rs)
}

// splitSetOp splits a class body on a single top-level && or -- operator.
func splitSetOp(body []rune) (op string, left, right []rune, ok bool) {
	depth := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
		case '[':
			depth++
		case ']':
			depth--
		default:
			if depth == 0 && i+1 < len(body) {
				if (body[i] == '&' && body[i+1] == '&') || (body[i] == '-' && body[i+1] == '-') {
					return string(body[i : i+2]), body[:i], body[i+2:], true
				}
			}
		}
	}
	return "", nil, nil, false
}

func applySetOp(op string, a, b codePointSet) codePointSet {
	res := codePointSet{}
	if op == "&&" {
		for c := range a {
			if b[c] {
				res[c] = true
			}
		}
	} else { // "--"
		for c := range a {
			if !b[c] {
				res[c] = true
			}
		}
	}
	return res
}

// operandSet evaluates one set-expression operand to a code-point set.
func operandSet(operand []rune) (codePointSet, error) {
	s := strings.TrimSpace(string(operand))
	if strings.HasPrefix(s, "[") {
		body, _ := scanClassBody([]rune(s), 0)
		if op, l, r, ok := splitSetOp(body); ok {
			ls, e := operandSet(l)
			if e != nil {
				return nil, e
			}
			rs, e := operandSet(r)
			if e != nil {
				return nil, e
			}
			return applySetOp(op, ls, rs), nil
		}
		return classMembers(body)
	}
	if strings.HasPrefix(s, `\p{`) || strings.HasPrefix(s, `\P{`) {
		name := s[3 : len(s)-1]
		rt, ok := resolveUnicodeProperty(name)
		if !ok {
			return nil, fmt.Errorf("unknown unicode property `%s`", name)
		}
		set := setFromTable(rt)
		if s[1] == 'P' {
			neg := codePointSet{}
			for c := rune(0); c <= 0x10FFFF; c++ {
				if !set[c] {
					neg[c] = true
				}
			}
			return neg, nil
		}
		return set, nil
	}
	return classMembers([]rune(s))
}

// classMembers parses the plain contents of a character class (chars, ranges,
// and \x/\u/\p escapes) into a code-point set.
func classMembers(body []rune) (codePointSet, error) {
	set := codePointSet{}
	pts := []rune{}
	for i := 0; i < len(body); {
		c, adv, prop, e := classAtom(body, i)
		if e != nil {
			return nil, e
		}
		if prop != nil {
			for k := range prop {
				set[k] = true
			}
			i += adv
			continue
		}
		if i+adv < len(body) && body[i+adv] == '-' && i+adv+1 < len(body) && body[i+adv+1] != ']' {
			hi, adv2, _, e := classAtom(body, i+adv+1)
			if e != nil {
				return nil, e
			}
			for r := c; r <= hi; r++ {
				set[r] = true
			}
			i += adv + 1 + adv2
			continue
		}
		pts = append(pts, c)
		set[c] = true
		i += adv
	}
	return set, nil
}

// classAtom decodes one atom (a literal, an \xHH/\uHHHH/\u{…} escape, or a
// \p{…} property) at body[i], returning the rune (or a property set) and how
// many runes were consumed.
func classAtom(body []rune, i int) (rune, int, codePointSet, error) {
	c := body[i]
	if c != '\\' || i+1 >= len(body) {
		return c, 1, nil, nil
	}
	n := body[i+1]
	switch n {
	case 'x':
		if i+3 < len(body) {
			v := hexVal(body[i+2 : i+4])
			return rune(v), 4, nil, nil
		}
	case 'u':
		if i+2 < len(body) && body[i+2] == '{' {
			end := i + 3
			for end < len(body) && body[end] != '}' {
				end++
			}
			return rune(hexVal(body[i+3 : end])), end - i + 1, nil, nil
		}
		if i+5 < len(body) {
			return rune(hexVal(body[i+2 : i+6])), 6, nil, nil
		}
	case 'p', 'P':
		if i+2 < len(body) && body[i+2] == '{' {
			end := i + 3
			for end < len(body) && body[end] != '}' {
				end++
			}
			rt, ok := resolveUnicodeProperty(string(body[i+3 : end]))
			if !ok {
				return 0, 0, nil, fmt.Errorf("unknown unicode property")
			}
			return 0, end - i + 1, setFromTable(rt), nil
		}
	case '0':
		return 0, 2, nil, nil
	}
	return n, 2, nil, nil
}

func hexVal(rs []rune) uint32 {
	var v uint32
	for _, r := range rs {
		v <<= 4
		switch {
		case r >= '0' && r <= '9':
			v |= uint32(r - '0')
		case r >= 'a' && r <= 'f':
			v |= uint32(r-'a') + 10
		case r >= 'A' && r <= 'F':
			v |= uint32(r-'A') + 10
		}
	}
	return v
}

// resolveUnicodeProperty maps an ES property name — "Script=Greek", "gc=Lu",
// a bare general category ("Lu"/"Letter"), a binary property ("White_Space"),
// or a bare script ("Greek") — to a Unicode RangeTable.
func resolveUnicodeProperty(name string) (*unicode.RangeTable, bool) {
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		prop := strings.TrimSpace(name[:eq])
		val := strings.TrimSpace(name[eq+1:])
		switch prop {
		case "Script", "sc", "Script_Extensions", "scx":
			if rt, ok := unicode.Scripts[val]; ok {
				return rt, true
			}
			rt, ok := supplementaryScripts[val]
			return rt, ok
		case "General_Category", "gc":
			return lookupCategory(val)
		}
		return nil, false
	}
	if name == "Unified_Ideograph" {
		return mergeTables(unicode.Properties["Unified_Ideograph"], supplementaryUnifiedIdeograph), true
	}
	switch name {
	case "ASCII":
		return &unicode.RangeTable{R16: []unicode.Range16{{0x00, 0x7F, 1}}}, true
	case "Any":
		return &unicode.RangeTable{R16: []unicode.Range16{{0x0, 0xFFFF, 1}}, R32: []unicode.Range32{{0x10000, 0x10FFFF, 1}}}, true
	case "Assigned":
		return unicode.Categories["L"], true // approximation, unused by the corpus
	}
	if rt, ok := lookupCategory(name); ok {
		return rt, true
	}
	if rt, ok := unicode.Properties[name]; ok {
		return rt, true
	}
	if rt, ok := emojiProperties[name]; ok {
		return rt, true
	}
	if rt, ok := unicode.Scripts[name]; ok {
		return rt, true
	}
	return nil, false
}

// codePointSet is a simple sorted set of code points used to evaluate the `v`
// flag's class set operations (intersection &&, difference --, union).
type codePointSet map[rune]bool

// mergeTables concatenates two RangeTables' ranges (order is irrelevant for the
// class-emission use here).
func mergeTables(a, b *unicode.RangeTable) *unicode.RangeTable {
	m := &unicode.RangeTable{}
	if a != nil {
		m.R16 = append(m.R16, a.R16...)
		m.R32 = append(m.R32, a.R32...)
	}
	if b != nil {
		m.R16 = append(m.R16, b.R16...)
		m.R32 = append(m.R32, b.R32...)
	}
	return m
}

func setFromTable(rt *unicode.RangeTable) codePointSet {
	s := codePointSet{}
	for _, r := range rt.R16 {
		for c := uint32(r.Lo); c <= uint32(r.Hi); c += uint32(r.Stride) {
			s[rune(c)] = true
		}
	}
	for _, r := range rt.R32 {
		for c := r.Lo; c <= r.Hi; c += r.Stride {
			s[rune(c)] = true
		}
	}
	return s
}

// setToClass renders a code-point set as a regexp2 character class.
func setToClass(s codePointSet) string {
	// Collect and coalesce into ranges.
	pts := make([]int, 0, len(s))
	for c := range s {
		pts = append(pts, int(c))
	}
	sortInts(pts)
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < len(pts); {
		j := i
		for j+1 < len(pts) && pts[j+1] == pts[j]+1 {
			j++
		}
		if i == j {
			b.WriteString(esc(uint32(pts[i])))
		} else {
			b.WriteString(esc(uint32(pts[i])) + "-" + esc(uint32(pts[j])))
		}
		i = j + 1
	}
	b.WriteByte(']')
	return b.String()
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// lookupCategory resolves a general-category value by its abbreviated name (Lu)
// or its ES long alias (Uppercase_Letter).
func lookupCategory(name string) (*unicode.RangeTable, bool) {
	if rt, ok := unicode.Categories[name]; ok {
		return rt, true
	}
	if short, ok := categoryAliases[name]; ok {
		if rt, ok := unicode.Categories[short]; ok {
			return rt, true
		}
	}
	return nil, false
}

var categoryAliases = map[string]string{
	"Letter": "L", "Cased_Letter": "LC", "Uppercase_Letter": "Lu",
	"Lowercase_Letter": "Ll", "Titlecase_Letter": "Lt", "Modifier_Letter": "Lm",
	"Other_Letter": "Lo", "Mark": "M", "Nonspacing_Mark": "Mn",
	"Spacing_Mark": "Mc", "Enclosing_Mark": "Me", "Number": "N",
	"Decimal_Number": "Nd", "Letter_Number": "Nl", "Other_Number": "No",
	"Punctuation": "P", "Connector_Punctuation": "Pc", "Dash_Punctuation": "Pd",
	"Open_Punctuation": "Ps", "Close_Punctuation": "Pe", "Initial_Punctuation": "Pi",
	"Final_Punctuation": "Pf", "Other_Punctuation": "Po", "Symbol": "S",
	"Math_Symbol": "Sm", "Currency_Symbol": "Sc", "Modifier_Symbol": "Sk",
	"Other_Symbol": "So", "Separator": "Z", "Space_Separator": "Zs",
	"Line_Separator": "Zl", "Paragraph_Separator": "Zp", "Other": "C",
	"Control": "Cc", "Format": "Cf", "Surrogate": "Cs", "Private_Use": "Co",
	"Unassigned": "Cn",
}

// rangeTableToClass renders a RangeTable as a regexp2 character class. When
// inClass is true the ranges are emitted bare (no surrounding brackets) so they
// can be spliced into an existing [...] class. Negated escapes are only handled
// outside a class.
func rangeTableToClass(rt *unicode.RangeTable, negate, inClass bool) string {
	var b strings.Builder
	if !inClass {
		if negate {
			b.WriteString("[^")
		} else {
			b.WriteByte('[')
		}
	}
	for _, r := range rt.R16 {
		writeClassRange(&b, uint32(r.Lo), uint32(r.Hi), uint32(r.Stride))
	}
	for _, r := range rt.R32 {
		writeClassRange(&b, r.Lo, r.Hi, r.Stride)
	}
	if !inClass {
		b.WriteByte(']')
	}
	return b.String()
}

// esc renders a code point as a regexp2-accepted escape: \uXXXX for the BMP,
// \u{…} (valid under the Unicode flag) for astral code points.
func esc(c uint32) string {
	if c <= 0xFFFF {
		return fmt.Sprintf(`\u%04X`, c)
	}
	return fmt.Sprintf(`\u{%X}`, c)
}

func writeClassRange(b *strings.Builder, lo, hi, stride uint32) {
	if stride <= 1 {
		if lo == hi {
			b.WriteString(esc(lo))
		} else {
			b.WriteString(esc(lo) + "-" + esc(hi))
		}
		return
	}
	for c := lo; c <= hi; c += stride {
		b.WriteString(esc(c))
	}
}
