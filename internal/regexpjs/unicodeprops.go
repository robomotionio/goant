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
// extraFoldPairs are the CaseFolding.txt "S" (simple) mappings that Go's
// unicode.SimpleFold orbit does not carry, because they are not case-conversion
// pairs. ES Canonicalize uses the simple OR common folding, so these characters
// do match each other under `iu`.
var extraFoldPairs = map[rune]rune{
	0x1FD3: 0x0390, 0x0390: 0x1FD3, // ΐ
	0x1FE3: 0x03B0, 0x03B0: 0x1FE3, // ΰ
	0xFB05: 0xFB06, 0xFB06: 0xFB05, // ﬅ ﬆ
}

// foldOrbit is the set of characters that Canonicalize maps together with c.
func foldOrbit(c rune) []rune {
	orbit := []rune{c}
	seen := map[rune]bool{c: true}
	for i := 0; i < len(orbit); i++ {
		x := orbit[i]
		for f := unicode.SimpleFold(x); f != x; f = unicode.SimpleFold(f) {
			if !seen[f] {
				seen[f] = true
				orbit = append(orbit, f)
			}
		}
		if e, ok := extraFoldPairs[x]; ok && !seen[e] {
			seen[e] = true
			orbit = append(orbit, e)
		}
	}
	return orbit
}

// hexEscapeRune decodes a `\uXXXX`, `\u{...}` or `\xXX` escape starting at the
// backslash rs[i], returning the code point and how many runes it spans.
func hexEscapeRune(rs []rune, i int) (rune, int) {
	if i+1 >= len(rs) || rs[i] != '\\' {
		return 0, 0
	}
	hex := func(c rune) (int, bool) {
		switch {
		case c >= '0' && c <= '9':
			return int(c - '0'), true
		case c >= 'a' && c <= 'f':
			return int(c-'a') + 10, true
		case c >= 'A' && c <= 'F':
			return int(c-'A') + 10, true
		}
		return 0, false
	}
	switch rs[i+1] {
	case 'x':
		if i+3 >= len(rs) {
			return 0, 0
		}
		h, ok1 := hex(rs[i+2])
		l, ok2 := hex(rs[i+3])
		if !ok1 || !ok2 {
			return 0, 0
		}
		return rune(h*16 + l), 4
	case 'u':
		if i+2 < len(rs) && rs[i+2] == '{' {
			v, k := 0, i+3
			for ; k < len(rs) && rs[k] != '}'; k++ {
				d, ok := hex(rs[k])
				if !ok || v > 0x10FFFF {
					return 0, 0
				}
				v = v*16 + d
			}
			if k >= len(rs) || k == i+3 {
				return 0, 0
			}
			return rune(v), k - i + 1
		}
		if i+5 >= len(rs) {
			return 0, 0
		}
		v := 0
		for k := i + 2; k < i+6; k++ {
			d, ok := hex(rs[k])
			if !ok {
				return 0, 0
			}
			v = v*16 + d
		}
		return rune(v), 6
	}
	return 0, 0
}

func expandCaseFold(pattern string) string {
	rs := []rune(pattern)
	var out strings.Builder
	inClass := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && i+1 < len(rs) {
			// A hex escape names a character just as a literal does, but expanding
			// every one of them would also rewrite the range ENDPOINTS of an
			// expanded \p{…} class. Only the folds regexp2's own IgnoreCase cannot
			// see are worth the rewrite, and only outside a range.
			if r, n := hexEscapeRune(rs, i); n > 0 && extraFoldPairs[r] != 0 &&
				!(i > 0 && rs[i-1] == '-') && !(i+n < len(rs) && rs[i+n] == '-' && i+n+1 < len(rs) && rs[i+n+1] != ']') {
				writeFoldOrbit(&out, foldOrbit(r), inClass)
				i += n - 1
				continue
			}
			// \w / \W are handled by translateWordClass, which tracks ignoreCase down
			// the inline-modifier nesting — a `(?-i:…)` region must get the plain
			// ASCII word set back, which a whole-pattern rewrite here cannot express.
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
		orbit := foldOrbit(c)
		if len(orbit) <= 2 && extraFoldPairs[c] == 0 {
			out.WriteRune(c)
			continue
		}
		writeFoldOrbit(&out, orbit, inClass)
	}
	return out.String()
}

// writeFoldOrbit emits the orbit as a character class (or, inside one, as bare
// members).
func writeFoldOrbit(out *strings.Builder, orbit []rune, inClass bool) {
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

// isVModeReservedDoublePunct reports whether c is a punctuator whose doubled form
// (`&&`, `!!`, `##`, `$$`, `%%`, `**`, `++`, `,,`, `..`, `::`, `;;`, `<<`, `==`,
// `>>`, `??`, `@@`, `^^`, ``` “ ```, `~~`) is a ClassSetReservedDoublePunctuator
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

// translateVFlagSets rewrites every `v`-flag character class into a plain
// regexp2 sub-pattern by evaluating it as a ClassSetExpression (see classset.go)
// — union, intersection (&&) and difference (--) over operands that may be
// nested classes, \q{…} string disjunctions, property escapes, or characters.
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
			p := &csParser{rs: rs, i: i}
			set, err := p.nestedClass()
			if err != nil {
				return "", err
			}
			out.WriteString(set.pattern())
			i = p.i
			continue
		}
		out.WriteRune(rs[i])
		i++
	}
	return out.String(), nil
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
