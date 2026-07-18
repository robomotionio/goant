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
// The `v`-flag "properties of strings" (Basic_Emoji, RGI_Emoji, and the RGI
// sequence sets) are resolved from the generated u17StringProperties table,
// which holds a regexp2 alternation of every literal code-point sequence each
// property matches (see unicode17_gen.go / tools/genunicode).

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
					if pat, ok := u17StringProperties[name]; ok {
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
			// Under u+i, the ASCII \w orbit folds in ſ (U+017F, from s) and the
			// Kelvin sign (U+212A, from k), which regexp2's IgnoreCase misses for the
			// shorthand. \W must correspondingly exclude them.
			switch rs[i+1] {
			case 'w':
				if inClass {
					out.WriteString(`\wſK`)
				} else {
					out.WriteString(`[0-9A-Za-z_ſK]`)
				}
				i++
				continue
			case 'W':
				if !inClass {
					out.WriteString(`[^0-9A-Za-z_ſK]`)
					i++
					continue
				}
			}
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
// isVModeReservedDoublePunct reports whether c is a punctuator whose doubled form
// (`&&`, `!!`, `##`, `$$`, `%%`, `**`, `++`, `,,`, `..`, `::`, `;;`, `<<`, `==`,
// `>>`, `??`, `@@`, `^^`, ``` `` ```, `~~`) is a ClassSetReservedDoublePunctuator
// — forbidden as a literal inside a `v`-mode character class.
func isVModeReservedDoublePunct(c rune) bool {
	switch c {
	case '&', '!', '#', '$', '%', '*', '+', ',', '.', ':', ';', '<', '=', '>', '?', '@', '^', '~', '`':
		return true
	}
	return false
}

// validateVModeClasses enforces the `v`-flag (unicodeSets) ClassSetExpression
// early errors that regexp2 does not: an unescaped ClassSetSyntaxCharacter
// (`( ) { } / |`, and a `-` outside a range) may not appear as a literal, and a
// ClassSetReservedDoublePunctuator (`&&`, `!!`, `##`, …) is forbidden where a
// class-set operand is expected (`&&`/`--` remain valid as set operators between
// operands). These patterns are all valid under the plain `u` flag, so the check
// only runs for `v`.
func validateVModeClasses(pattern string) error {
	rs := []rune(pattern)
	i := 0
	for i < len(rs) {
		if rs[i] == '\\' {
			i += 2 // skip an escape at the top level (outside any class)
			continue
		}
		if rs[i] == '[' {
			next, err := validateVModeClassBody(rs, i)
			if err != nil {
				return err
			}
			i = next
			continue
		}
		i++
	}
	return nil
}

// validateVModeClassBody validates a single `[…]` class (rs[start] == '[') and
// returns the index just past its closing ']'.
func validateVModeClassBody(rs []rune, start int) (int, error) {
	i := start + 1
	if i < len(rs) && rs[i] == '^' {
		i++
	}
	prevOperand := false // a ClassSetOperand precedes the current position
	for i < len(rs) {
		c := rs[i]
		switch {
		case c == ']':
			return i + 1, nil
		case c == '\\':
			// An escape is a ClassSetOperand. Consume the brace-delimited escapes
			// `\p{…}` / `\P{…}` / `\q{…}` / `\u{…}` wholesale so their braces are not
			// read as syntax characters.
			if i+2 < len(rs) && (rs[i+1] == 'p' || rs[i+1] == 'P' || rs[i+1] == 'q' || rs[i+1] == 'u') && rs[i+2] == '{' {
				j := i + 3
				for j < len(rs) && rs[j] != '}' {
					j++
				}
				if j >= len(rs) {
					return 0, fmt.Errorf("unterminated %c-escape in character class", rs[i+1])
				}
				i = j + 1
			} else {
				i += 2
			}
			prevOperand = true
		case c == '[':
			n, err := validateVModeClassBody(rs, i)
			if err != nil {
				return 0, err
			}
			i = n
			prevOperand = true
		case c == '&' && i+1 < len(rs) && rs[i+1] == '&':
			if !prevOperand {
				return 0, fmt.Errorf("`&&` with no left operand in v-mode class")
			}
			i += 2
			prevOperand = false
		case c == '-' && i+1 < len(rs) && rs[i+1] == '-':
			if !prevOperand {
				return 0, fmt.Errorf("`--` with no left operand in v-mode class")
			}
			i += 2
			prevOperand = false
		case i+1 < len(rs) && rs[i+1] == c && isVModeReservedDoublePunct(c):
			return 0, fmt.Errorf("reserved double punctuator `%c%c` in v-mode class", c, c)
		case c == '(' || c == ')' || c == '{' || c == '}' || c == '/' || c == '|':
			return 0, fmt.Errorf("unescaped `%c` must be escaped in a v-mode class", c)
		case c == '-':
			// A `-` is legal only as a range operator between two ClassSetCharacters;
			// a leading `-` (no left operand) is an error, e.g. `[-]`.
			if !prevOperand {
				return 0, fmt.Errorf("unescaped `-` must be escaped in a v-mode class")
			}
			i++
			prevOperand = false
		default:
			i++
			prevOperand = true
		}
	}
	return 0, fmt.Errorf("unterminated character class")
}

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
// a bare general category ("Lu"/"Letter"), or a binary property ("White_Space")
// — to a Unicode RangeTable. The property data comes from the generated
// Unicode 17.0.0 tables (unicode17_gen.go), which Test262's property-escapes
// tests target; the Go toolchain's bundled unicode package lags that version.
func resolveUnicodeProperty(name string) (*unicode.RangeTable, bool) {
	// ECMAScript uses exact (not "loose") property matching: no surrounding or
	// interior whitespace is permitted in a `\p{…}` name or value.
	if strings.ContainsAny(name, " \t\n\r") {
		return nil, false
	}
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		prop := name[:eq]
		val := name[eq+1:]
		switch prop {
		case "Script", "sc":
			return lookupScript17(val, u17Scripts)
		case "Script_Extensions", "scx":
			return lookupScript17(val, u17ScriptExtensions)
		case "General_Category", "gc":
			return lookupCategory17(val)
		}
		return nil, false
	}
	switch name {
	case "ASCII":
		return &unicode.RangeTable{R16: []unicode.Range16{{0x00, 0x7F, 1}}}, true
	case "Any":
		return &unicode.RangeTable{R16: []unicode.Range16{{0x0, 0xFFFF, 1}}, R32: []unicode.Range32{{0x10000, 0x10FFFF, 1}}}, true
	case "Assigned":
		return u17Assigned, true
	}
	// A lone \p{name} is valid only for a General_Category value or an
	// ECMAScript-permitted binary property. A bare script name is not a valid
	// lone property; esBinaryProperties gates out Go names ES rejects.
	if rt, ok := lookupCategory17(name); ok {
		return rt, true
	}
	if canon, ok := esBinaryProperties[name]; ok {
		rt, ok := u17Binary[canon]
		return rt, ok
	}
	return nil, false
}

// lookupScript17 resolves a Script or Script_Extensions value, accepting an ISO
// 15924 alias (e.g. "Grek") or the full name (e.g. "Greek").
func lookupScript17(val string, m map[string]*unicode.RangeTable) (*unicode.RangeTable, bool) {
	if full, ok := u17ScriptAlias[val]; ok {
		val = full
	}
	rt, ok := m[val]
	return rt, ok
}

// lookupCategory17 resolves a general-category value by its abbreviated name
// (Lu), its group name (L), or its ES long alias (Uppercase_Letter).
func lookupCategory17(val string) (*unicode.RangeTable, bool) {
	if rt, ok := u17Categories[val]; ok {
		return rt, true
	}
	if short, ok := u17CategoryAlias[val]; ok {
		if rt, ok := u17Categories[short]; ok {
			return rt, true
		}
	}
	return nil, false
}

// codePointSet is a simple sorted set of code points used to evaluate the `v`
// flag's class set operations (intersection &&, difference --, union).
type codePointSet map[rune]bool

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

// rangeTableToClass renders a RangeTable as a regexp2 character class. When
// inClass is true the ranges are emitted bare (no surrounding brackets) so they
// can be spliced into an existing [...] class; a negated `\P{…}` spliced into a
// class emits the complement ranges directly, since `[^…]` cannot be nested.
func rangeTableToClass(rt *unicode.RangeTable, negate, inClass bool) string {
	var b strings.Builder
	if inClass {
		if negate {
			writeComplementRanges(&b, rt)
		} else {
			writeTableRanges(&b, rt)
		}
		return b.String()
	}
	if negate {
		b.WriteString("[^")
	} else {
		b.WriteByte('[')
	}
	writeTableRanges(&b, rt)
	b.WriteByte(']')
	return b.String()
}

func writeTableRanges(b *strings.Builder, rt *unicode.RangeTable) {
	for _, r := range rt.R16 {
		writeClassRange(b, uint32(r.Lo), uint32(r.Hi), uint32(r.Stride))
	}
	for _, r := range rt.R32 {
		writeClassRange(b, r.Lo, r.Hi, r.Stride)
	}
}

// writeComplementRanges emits the gaps of rt within [0, 0x10FFFF] as bare class
// ranges — the code points a negated `\P{…}` matches.
func writeComplementRanges(b *strings.Builder, rt *unicode.RangeTable) {
	var pairs [][2]uint32
	add := func(lo, hi, stride uint32) {
		if stride <= 1 {
			pairs = append(pairs, [2]uint32{lo, hi})
			return
		}
		for c := lo; c <= hi; c += stride {
			pairs = append(pairs, [2]uint32{c, c})
		}
	}
	for _, r := range rt.R16 {
		add(uint32(r.Lo), uint32(r.Hi), uint32(r.Stride))
	}
	for _, r := range rt.R32 {
		add(r.Lo, r.Hi, r.Stride)
	}
	// Insertion sort by low bound (range counts are modest).
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && pairs[j-1][0] > pairs[j][0]; j-- {
			pairs[j-1], pairs[j] = pairs[j], pairs[j-1]
		}
	}
	var next uint32
	for _, p := range pairs {
		if p[0] > next {
			writeClassRange(b, next, p[0]-1, 1)
		}
		if p[1]+1 > next {
			next = p[1] + 1
		}
	}
	if next <= 0x10FFFF {
		writeClassRange(b, next, 0x10FFFF, 1)
	}
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
