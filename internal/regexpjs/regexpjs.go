// Package regexpjs is the JS→regex translation layer (PLAN.md Phase 4.3/8): it
// validates ECMAScript regular expressions and retargets them onto
// dlclark/regexp2 (which supports an ECMAScript mode directly). Flags g/y and
// lastIndex handling live at the JS level; i/m/s/u map to regexp2 options.
//
// Position semantics: subjects are passed in — and all offsets reported back —
// as UTF-16 code units, the domain ECMAScript indexes strings in. A `u`/`v`
// pattern matches whole code points, so Exec recodes such a subject and maps the
// resulting offsets back (see Exec).
package regexpjs

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

// Regexp is a compiled JS regular expression.
type Regexp struct {
	re     *regexp2.Regexp
	Source string
	Flags  string

	Global      bool
	IgnoreCase  bool
	Multiline   bool
	DotAll      bool
	Unicode     bool
	UnicodeSets bool
	Sticky      bool

	// groupNames maps the safe internal capture-group name regexp2 sees back to
	// the original ECMAScript name (nil when the pattern has no named groups).
	groupNames map[string]string
	// groupKinds lists the capture groups in ECMAScript definition order (true =
	// named). It is used to reorder regexp2's results, which number all unnamed
	// groups before all named ones, back into left-to-right order. nil when the
	// pattern has no named groups (no reordering needed).
	groupKinds []bool
	// quantParent maps each ECMAScript capture group to a quantified enclosing
	// capture group (or 0). See quantifiedParents.
	quantParent []int
}

// Group is one capture group in a match (Index is a rune offset; -1 = unmatched).
type Group struct {
	Index  int
	Length int
	Value  string
	Name   string
}

// Match is a successful regex match with its capture groups (group 0 is whole).
type Match struct {
	Index  int
	Groups []Group
}

// Compile validates flags and compiles pattern under ECMAScript semantics.
// translateAnnexBEscapes rewrites the JS identity escapes that regexp2 would
// otherwise misread. `\A \Z \z \G` (regexp2 anchors) become the literal letter,
// and `\c` not followed by an ASCII control letter becomes a literal backslash +
// c (Annex B ExtendedAtom). All other escapes — including `\\` — pass through
// verbatim; the two-rune advance on `\\` keeps a literal backslash from being
// rescanned.
func translateAnnexBEscapes(src string) string {
	rs := []rune(src)
	var b strings.Builder
	inClass := false
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\\' || i+1 >= len(rs) {
			switch rs[i] {
			case '[':
				inClass = true
			case ']':
				inClass = false
			}
			b.WriteRune(rs[i])
			continue
		}
		switch n := rs[i+1]; n {
		case 'A', 'Z', 'z', 'G':
			b.WriteRune(n)
		case 'p', 'P':
			// In a non-Unicode pattern a bare \p / \P (not a \p{…} property escape) is
			// an identity escape — the literal letter — not the incomplete property
			// escape regexp2 would reject.
			if i+2 < len(rs) && rs[i+2] == '{' {
				b.WriteRune('\\')
				b.WriteRune(n)
			} else {
				b.WriteRune(n)
			}
		case 'c':
			switch {
			case i+2 < len(rs) && isASCIILetter(rs[i+2]):
				b.WriteString(`\c`)
			case inClass && i+2 < len(rs) && (rs[i+2] == '_' || (rs[i+2] >= '0' && rs[i+2] <= '9')):
				// Annex B ClassControlLetter admits a DecimalDigit or '_' as well, and
				// only INSIDE a character class: `[\c0]` is U+0010 (0x30 mod 32) while a
				// bare `\c0` stays the three literal characters.
				fmt.Fprintf(&b, `\x%02X`, rs[i+2]%32)
				i++ // the control letter is consumed here too
			default:
				b.WriteString(`\\c`)
			}
		default:
			b.WriteRune('\\')
			b.WriteRune(n)
		}
		i++ // consumed the escaped rune too
	}
	return b.String()
}

// runesToWTF8 encodes runes as WTF-8: an adjacent surrogate pair (which only the
// code-unit matching domain produces) combines into its astral code point, and a
// lone surrogate keeps its raw 3-byte form instead of becoming U+FFFD.
func runesToWTF8(rs []rune) string {
	surrogate := false
	for _, c := range rs {
		if c >= 0xD800 && c <= 0xDFFF {
			surrogate = true
			break
		}
	}
	if !surrogate {
		return string(rs)
	}
	var b strings.Builder
	b.Grow(len(rs))
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c >= 0xD800 && c <= 0xDBFF && i+1 < len(rs) && rs[i+1] >= 0xDC00 && rs[i+1] <= 0xDFFF {
			b.WriteRune(0x10000 + (c-0xD800)<<10 + rs[i+1] - 0xDC00)
			i++
			continue
		}
		if c >= 0xD800 && c <= 0xDFFF {
			b.WriteByte(byte(0xE0 | c>>12))
			b.WriteByte(byte(0x80 | (c>>6)&0x3F))
			b.WriteByte(byte(0x80 | c&0x3F))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// translateDot rewrites `.` into an explicit class. ECMAScript's dot excludes
// every LineTerminator — \r and the two Unicode separators as well as \n —
// whereas regexp2's excludes only \n, and with `s` it matches everything. It is
// spelled out rather than left to regexp2's Singleline option because an inline
// `(?s:…)` / `(?-s:…)` modifier group changes dotAll for its body only, so the
// state is tracked down the group nesting as the pattern is scanned.
func translateDot(src string, dotAll bool) string {
	if !strings.ContainsRune(src, '.') {
		return src
	}
	const notLineTerminator = `[^\n\r\u2028\u2029]`
	rs := []rune(src)
	var b strings.Builder
	b.Grow(len(src))
	stack := []bool{dotAll}
	cur := func() bool { return stack[len(stack)-1] }
	inClass := 0
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\' && i+1 < len(rs):
			b.WriteRune(c)
			b.WriteRune(rs[i+1])
			i++
			continue
		case c == '[':
			inClass++
		case c == ']':
			if inClass > 0 {
				inClass--
			}
		case inClass > 0:
			// A '.' inside a class is a literal; nothing here needs tracking.
		case c == '(':
			stack = append(stack, modifiedDotAll(rs, i, cur()))
		case c == ')':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case c == '.':
			if cur() {
				b.WriteString(`[\s\S]`)
			} else {
				b.WriteString(notLineTerminator)
			}
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// modifiedDotAll returns the dotAll state inside the group opening at rs[i],
// applying the add/remove flags of an inline `(?ims-ims:` modifier group.
func modifiedDotAll(rs []rune, i int, outer bool) bool {
	if i+1 >= len(rs) || rs[i+1] != '?' {
		return outer
	}
	add := true
	for j := i + 2; j < len(rs); j++ {
		switch rs[j] {
		case 'i', 'm':
		case 's':
			outer = add
		case '-':
			add = false
		case ':':
			return outer
		default:
			return outer // not a modifier group: (?: (?= (?! (?<name> …
		}
	}
	return outer
}

// asciiFoldOutliers are the two characters whose .NET case-insensitive matching
// crosses the ASCII boundary that ECMAScript's non-Unicode Canonicalize keeps:
// it uppercases a single code point and then REFUSES the result when a
// non-ASCII character would become an ASCII one. So `/\u212a/i` must not match
// "k", and `/\u017f/i` must not match "s".
var asciiFoldOutliers = map[rune]bool{0x212A: true, 0x017F: true}

// isolateAsciiFoldOutliers wraps those characters in a `(?-i:…)` region so
// regexp2's IgnoreCase cannot fold them. Only outside a character class, where
// a group is not expressible.
func isolateAsciiFoldOutliers(src string) string {
	rs := []rune(src)
	var out strings.Builder
	inClass := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && i+1 < len(rs) {
			if r, n := hexEscapeAt(src, len(string(rs[:i]))); n > 0 && asciiFoldOutliers[rune(r)] && !inClass {
				out.WriteString("(?-i:")
				out.WriteString(string(rs[i : i+n]))
				out.WriteString(")")
				i += n - 1
				continue
			}
			out.WriteRune(c)
			out.WriteRune(rs[i+1])
			i++
			continue
		}
		switch c {
		case '[':
			inClass = true
		case ']':
			inClass = false
		}
		if !inClass && asciiFoldOutliers[c] {
			out.WriteString("(?-i:")
			out.WriteRune(c)
			out.WriteString(")")
			continue
		}
		out.WriteRune(c)
	}
	return out.String()
}

// annexBIdentityEscapes rewrites the escapes regexp2 gives a .NET meaning but
// ECMAScript's Annex B IdentityEscape leaves as the plain character: `\a` is BEL
// in .NET and the letter "a" here, and `\A` / `\Z` / `\z` / `\G` are .NET
// anchors. (In Unicode mode these are early errors, rejected before this.)
func annexBIdentityEscapes(src string) string {
	if !strings.Contains(src, `\`) {
		return src
	}
	rs := []rune(src)
	var out strings.Builder
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\\' || i+1 >= len(rs) {
			out.WriteRune(rs[i])
			continue
		}
		switch rs[i+1] {
		case 'a', 'e', 'A', 'Z', 'z', 'G':
			out.WriteRune(rs[i+1])
		default:
			out.WriteRune(rs[i])
			out.WriteRune(rs[i+1])
		}
		i++
	}
	return out.String()
}

// maxQuantifier bounds a `{n}` / `{n,}` / `{n,m}` count. ES puts no limit on
// DecimalDigits, but regexp2 mis-parses anything past its own integer range, and
// a count this large can only be satisfied by a subject longer than any string
// that fits in memory — so clamping is unobservable and keeps the pattern
// compilable (`b{9007199254740991}` must compile and simply never match).
const maxQuantifier = 1 << 24

// clampQuantifiers rewrites any repetition count above maxQuantifier down to it.
func clampQuantifiers(src string) string {
	if !strings.Contains(src, "{") {
		return src
	}
	rs := []rune(src)
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
		case ']':
			inClass = false
		}
		if c != '{' || inClass {
			out.WriteRune(c)
			continue
		}
		// Try to read `{ digits (, digits?)? }`; anything else is a literal brace.
		j := i + 1
		lo, loN := readDigits(rs, j)
		if loN == 0 {
			out.WriteRune(c)
			continue
		}
		j += loN
		hi, hiN, hasComma := "", 0, false
		if j < len(rs) && rs[j] == ',' {
			hasComma = true
			j++
			hi, hiN = readDigits(rs, j)
			j += hiN
		}
		if j >= len(rs) || rs[j] != '}' {
			out.WriteRune(c)
			continue
		}
		out.WriteByte('{')
		out.WriteString(clampCount(lo))
		if hasComma {
			out.WriteByte(',')
			if hiN > 0 {
				out.WriteString(clampCount(hi))
			}
		}
		out.WriteByte('}')
		i = j
	}
	return out.String()
}

func readDigits(rs []rune, i int) (string, int) {
	j := i
	for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
		j++
	}
	return string(rs[i:j]), j - i
}

func clampCount(digits string) string {
	if len(digits) > 9 {
		return strconv.Itoa(maxQuantifier)
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n > maxQuantifier {
		return strconv.Itoa(maxQuantifier)
	}
	return digits
}

// hexEscapeAt decodes a `\uXXXX` escape at s[i] (which must be the backslash),
// returning its value and the number of bytes consumed, or (0, 0).
func hexEscapeAt(s string, i int) (int, int) {
	if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
		return 0, 0
	}
	v := 0
	for k := i + 2; k < i+6; k++ {
		var d int
		switch c := s[k]; {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, 0
		}
		v = v*16 + d
	}
	return v, 6
}

// joinSurrogateEscapes is the inverse of splitAstralLiterals, for Unicode mode:
// RegExpUnicodeEscapeSequence reads `\u LeadSurrogate \u TrailSurrogate` as the
// ONE code point they encode (UTF16SurrogatePair), and a `u`/`v` subject is
// matched as code points — so the pattern has to name the code point too.
// Applies inside a class as well: `[\ud834\udf06]` is the class of U+1D306.
func joinSurrogateEscapes(src string) string {
	if !strings.Contains(src, `\u`) {
		return src
	}
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		if src[i] != '\\' {
			r, size := utf8.DecodeRuneInString(src[i:])
			b.WriteRune(r)
			i += size
			continue
		}
		if lead, n := hexEscapeAt(src, i); n > 0 && lead >= 0xD800 && lead <= 0xDBFF {
			if trail, n2 := hexEscapeAt(src, i+n); n2 > 0 && trail >= 0xDC00 && trail <= 0xDFFF {
				b.WriteRune(rune(0x10000 + (lead-0xD800)<<10 + (trail - 0xDC00)))
				i += n + n2
				continue
			}
		}
		// Any other escape is copied whole, so a `\\` cannot be mistaken for the
		// start of one.
		b.WriteByte('\\')
		i++
		if i < len(src) {
			r, size := utf8.DecodeRuneInString(src[i:])
			b.WriteRune(r)
			i += size
		}
	}
	return b.String()
}

// splitAstralLiterals rewrites every literal astral code point in a pattern as
// the two \uXXXX escapes for its surrogate pair.
func splitAstralLiterals(src string) string {
	if !strings.ContainsFunc(src, func(c rune) bool { return c > 0xFFFF }) {
		return src
	}
	var b strings.Builder
	b.Grow(len(src))
	for _, c := range src {
		if c > 0xFFFF {
			v := c - 0x10000
			fmt.Fprintf(&b, `\u%04X\u%04X`, 0xD800+(v>>10), 0xDC00+(v&0x3FF))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

func isASCIILetter(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func Compile(pattern, flags string) (*Regexp, error) {
	r := &Regexp{Source: pattern, Flags: flags}
	seen := map[rune]bool{}
	opts := regexp2.RegexOptions(regexp2.ECMAScript)
	for _, f := range flags {
		if seen[f] {
			return nil, fmt.Errorf("duplicate flag '%c' in regular expression", f)
		}
		seen[f] = true
		switch f {
		case 'g':
			r.Global = true
		case 'i':
			r.IgnoreCase = true
			opts |= regexp2.IgnoreCase
		case 'm':
			r.Multiline = true
			opts |= regexp2.Multiline
		case 's':
			r.DotAll = true
			opts |= regexp2.Singleline
		case 'u':
			r.Unicode = true
			opts |= regexp2.Unicode
		case 'v':
			// unicodeSets mode: treat like `u` and additionally resolve class set
			// operations (&&, --) ahead of compilation.
			r.Unicode = true
			r.UnicodeSets = true
			opts |= regexp2.Unicode
		case 'y':
			r.Sticky = true
		case 'd':
			// hasIndices — affects match-result shape only, no compile change.
		default:
			return nil, fmt.Errorf("invalid regular expression flag '%c'", f)
		}
	}
	// `u` and `v` select different pattern grammars, so they are mutually
	// exclusive (RegExpInitialize step 2).
	if seen['u'] && seen['v'] {
		return nil, fmt.Errorf("invalid regular expression flags 'u' and 'v' are mutually exclusive")
	}
	// Inline modifier groups `(?ims-ims:…)` have grammar early errors regexp2's
	// permissive .NET option syntax would not catch; reject the invalid ones here.
	if err := validateModifierGroups(pattern); err != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	// A quantifier on a lookbehind (any mode) or on a lookahead (Unicode mode) is
	// an early error the .NET engine would otherwise accept.
	if err := validateQuantifiedAssertions(pattern, r.Unicode); err != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	// Two named capture groups may share a name only across separate alternatives;
	// a same-alternative duplicate is an early error.
	if err := validateDuplicateGroupNames(pattern); err != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	// Unicode mode (`u`/`v`) forbids Annex B leniencies — unrecognized identity
	// escapes, backreferences to nonexistent groups, a lone `{`, class ranges with
	// a class-escape endpoint — that regexp2 would otherwise accept.
	if r.Unicode {
		if err := validateUnicodePattern(pattern, r.UnicodeSets); err != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", err)
		}
	}
	// The `v` flag imposes stricter ClassSetExpression early errors than `u`.
	if r.UnicodeSets {
		if err := validateVModeClasses(pattern); err != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", err)
		}
	}
	// An empty pattern matches the empty string; ECMAScript spells it (?:).
	src := pattern
	// Annex B identity escapes: outside Unicode mode, `\A \Z \z \G` are literal
	// letters in JS (regexp2/.NET would read them as anchors), and `\c` not
	// followed by a control letter is a literal backslash + c.
	if !r.Unicode {
		src = translateAnnexBEscapes(src)
		src = annexBClassRanges(src)
	}
	// Rename named groups / backreferences to regexp2-safe internal names (ES
	// allows names regexp2's \w+ grammar rejects, and duplicate names).
	if gs, gm, gk, gerr := translateGroupNames(src); gerr != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", gerr)
	} else {
		src, r.groupNames, r.groupKinds = gs, gm, gk
	}
	// Under the u/v flag, translate ES Unicode property escapes (\p{…}) into
	// explicit code-point classes regexp2 can compile.
	if r.UnicodeSets {
		t, terr := translateVFlagSets(src)
		if terr != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", terr)
		}
		src = t
	}
	// Property escapes may expand to sub-patterns that themselves contain \p (a
	// string property built from \p{Emoji}), so translate repeatedly to a fixpoint.
	for pass := 0; r.Unicode && pass < 4 && (strings.Contains(src, `\p`) || strings.Contains(src, `\P`)); pass++ {
		t, terr := translateUnicodeProps(src, r.UnicodeSets)
		if terr != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", terr)
		}
		if t == src {
			break
		}
		src = t
	}
	// Under the u+i flags, expand special-fold letters (S↔ſ, K↔K, …) into their
	// full orbit so regexp2's IgnoreCase does not miss them.
	if r.Unicode && r.IgnoreCase {
		src = expandCaseFold(src)
	}
	// Outside Unicode mode a pattern is a sequence of UTF-16 code units, so an
	// astral literal stands for its surrogate pair — and the subject is matched
	// in code units too. Spell the pair out so both sides agree. (In a class this
	// is also what ES means: `[𐌀]` is the class of its two code units, and an
	// astral ClassRange endpoint becomes the out-of-order range ES rejects.)
	if !r.Unicode {
		src = splitAstralLiterals(src)
		src = annexBIdentityEscapes(src)
		if r.IgnoreCase {
			src = isolateAsciiFoldOutliers(src)
		}
	} else {
		src = joinSurrogateEscapes(src)
	}
	// Under u/v, \w is decided by ES's Canonicalize rather than regexp2's
	// IgnoreCase option, and an inline modifier group can turn it on or off for a
	// region of the pattern.
	if r.Unicode {
		src = translateWordClass(src, r.IgnoreCase)
	}
	r.quantParent = quantifiedParents(src)
	src = clampQuantifiers(src)
	src = translateDot(src, r.DotAll)
	if src == "" {
		src = "(?:)"
	}
	re, err := regexp2.Compile(src, opts)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	r.re = re
	return r, nil
}

// Exec runs the regex against input — the subject string as UTF-16 code units,
// one rune per unit — starting at code-unit index start, and returns the first
// match (or nil if none). Sticky matches must begin exactly at start. All
// returned offsets are code-unit indices, matching ECMAScript string indexing.
func (r *Regexp) Exec(input []rune, start int) (*Match, error) {
	if start < 0 || start > len(input) {
		return nil, nil
	}
	// A `u`/`v` pattern matches whole code points, so a subject holding surrogate
	// pairs is recoded to code points for the match and the resulting offsets are
	// mapped back to code units. Subjects without a pair (the common case, lone
	// surrogates included) are already in both domains at once.
	if r.Unicode && hasSurrogatePair(input) {
		cps, units := toCodePoints(input)
		m, err := r.exec(cps, cpIndexAt(units, start))
		if err != nil || m == nil {
			return m, err
		}
		remapToCodeUnits(m, units)
		return m, nil
	}
	return r.exec(input, start)
}

// hasSurrogatePair reports whether input contains a high surrogate immediately
// followed by a low one — the only case where code-unit and code-point indexing
// of the subject diverge.
func hasSurrogatePair(input []rune) bool {
	for i := 0; i+1 < len(input); i++ {
		if input[i] >= 0xD800 && input[i] <= 0xDBFF && input[i+1] >= 0xDC00 && input[i+1] <= 0xDFFF {
			return true
		}
	}
	return false
}

// toCodePoints converts UTF-16 code units to code points, also returning the
// code-unit offset of each code point (plus a final entry for the end).
func toCodePoints(input []rune) (cps []rune, units []int) {
	cps = make([]rune, 0, len(input))
	units = make([]int, 0, len(input)+1)
	for i := 0; i < len(input); i++ {
		units = append(units, i)
		c := input[i]
		if c >= 0xD800 && c <= 0xDBFF && i+1 < len(input) && input[i+1] >= 0xDC00 && input[i+1] <= 0xDFFF {
			cps = append(cps, 0x10000+(c-0xD800)<<10+input[i+1]-0xDC00)
			i++
			continue
		}
		cps = append(cps, c)
	}
	units = append(units, len(input))
	return cps, units
}

// cpIndexAt maps a code-unit offset to a code-point index, rounding an offset
// that lands inside a surrogate pair down to the start of that pair.
func cpIndexAt(units []int, unit int) int {
	lo, hi := 0, len(units)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if units[mid] <= unit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// remapToCodeUnits rewrites a match's code-point offsets as code-unit offsets.
func remapToCodeUnits(m *Match, units []int) {
	at := func(cp int) int {
		if cp < 0 {
			return cp
		}
		if cp >= len(units) {
			return units[len(units)-1]
		}
		return units[cp]
	}
	m.Index = at(m.Index)
	for i := range m.Groups {
		g := &m.Groups[i]
		if g.Index < 0 {
			continue
		}
		end := at(g.Index + g.Length)
		g.Index = at(g.Index)
		g.Length = end - g.Index
	}
}

// exec runs the compiled pattern over runes in its own matching domain.
func (r *Regexp) exec(input []rune, start int) (*Match, error) {
	if start < 0 || start > len(input) {
		return nil, nil
	}
	m, err := r.re.FindRunesMatchStartingAt(input, start)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	if r.Sticky && m.Index != start {
		return nil, nil
	}
	groups := m.Groups()
	convert := func(g regexp2.Group) Group {
		name := g.Name
		if orig, ok := r.groupNames[name]; ok {
			name = orig // map the internal name back to the ECMAScript name
		}
		gg := Group{Index: -1, Name: name}
		if len(g.Captures) > 0 {
			gg.Index = g.Index
			gg.Length = g.Length
			// Not g.String(): Go's []rune->string conversion replaces every
			// surrogate rune with U+FFFD, which would corrupt both the halves of
			// a pair (non-Unicode mode) and a lone surrogate.
			gg.Value = runesToWTF8(input[g.Index : g.Index+g.Length])
		}
		return gg
	}
	out := &Match{Index: m.Index, Groups: make([]Group, len(groups))}
	// A quantifier resets the captures inside its body at the start of each
	// iteration, so a group whose last capture lies outside its quantified
	// parent's last capture did not participate in the final one. regexp2 keeps
	// the earlier capture; drop it.
	defer func() {
		for g := 1; g < len(out.Groups) && g < len(r.quantParent); g++ {
			p := r.quantParent[g]
			if p == 0 || p >= len(out.Groups) {
				continue
			}
			child, parent := out.Groups[g], out.Groups[p]
			if child.Index < 0 || parent.Index < 0 {
				continue
			}
			if child.Index < parent.Index || child.Index+child.Length > parent.Index+parent.Length {
				out.Groups[g] = Group{Index: -1, Name: child.Name}
			}
		}
	}()
	// regexp2 numbers every unnamed group before every named group; when the
	// pattern mixes the two, reorder them into ECMAScript left-to-right order.
	if r.groupKinds != nil && len(r.groupKinds)+1 == len(groups) {
		unnamedCount := 0
		for _, named := range r.groupKinds {
			if !named {
				unnamedCount++
			}
		}
		out.Groups[0] = convert(groups[0])
		unnamedPtr, namedPtr := 1, unnamedCount+1
		for es, named := range r.groupKinds {
			src := unnamedPtr
			if named {
				src, namedPtr = namedPtr, namedPtr+1
			} else {
				unnamedPtr++
			}
			out.Groups[es+1] = convert(groups[src])
		}
		return out, nil
	}
	for i, g := range groups {
		out.Groups[i] = convert(g)
	}
	return out, nil
}

// Test reports whether the regex matches anywhere at/after start.
func (r *Regexp) Test(input []rune, start int) (bool, error) {
	m, err := r.Exec(input, start)
	return m != nil, err
}

// GroupCount returns the number of capture groups (excluding group 0).
func (r *Regexp) GroupCount() int {
	return r.re.GetGroupNumbers()[len(r.re.GetGroupNumbers())-1]
}

// annexBClassRanges rewrites a character-class range whose endpoint is a CLASS
// ESCAPE (\d \D \s \S \w \W) so the '-' is a literal character rather than a
// range operator. Annex B B.1.4 (NonemptyClassRangesNoDash) makes `[a-\w]` the
// union of `a`, `-` and `\w` in a non-Unicode pattern; regexp2 rejects the range
// outright, and Unicode mode rejects it too (validateUnicodePattern).
func annexBClassRanges(src string) string {
	if !strings.ContainsRune(src, '[') {
		return src
	}
	rs := []rune(src)
	var b strings.Builder
	b.Grow(len(src))
	isClassEsc := func(c rune) bool { return strings.ContainsRune("dDsSwW", c) }
	inClass := false
	prevClassEsc := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && i+1 < len(rs) {
			b.WriteRune(c)
			b.WriteRune(rs[i+1])
			prevClassEsc = inClass && isClassEsc(rs[i+1])
			i++
			continue
		}
		switch {
		case !inClass && c == '[':
			inClass, prevClassEsc = true, false
		case inClass && c == ']':
			inClass, prevClassEsc = false, false
		case inClass && c == '-':
			nextIsClassEsc := i+2 < len(rs) && rs[i+1] == '\\' && isClassEsc(rs[i+2])
			if prevClassEsc || nextIsClassEsc {
				b.WriteString(`\-`)
				prevClassEsc = false
				continue
			}
			prevClassEsc = false
		default:
			prevClassEsc = false
		}
		b.WriteRune(c)
	}
	return b.String()
}

// wordClassUnicodeI is \w under the `u` (or `v`) flag with ignoreCase active:
// simple case folding brings LATIN SMALL LETTER LONG S and KELVIN SIGN into the
// word set, since they fold to "s" and "k".
const wordClassUnicodeI = `0-9A-Za-z_\u017F\u212A`

// wordClassPlain is \w with ignoreCase inactive — exactly the ASCII word set.
const wordClassPlain = `0-9A-Za-z_`

// translateWordClass spells out `\w` / `\W` for a Unicode-mode pattern, tracking
// ignoreCase down the inline-modifier group nesting the way translateDot tracks
// dotAll. regexp2 applies its IgnoreCase option to \w wholesale, which is both
// too wide where a `(?-i:…)` group turns matching case-sensitive again and too
// narrow where ES's simple case folding pulls in ſ and K.
func translateWordClass(src string, ignoreCase bool) string {
	if !strings.Contains(src, `\w`) && !strings.Contains(src, `\W`) &&
		!strings.Contains(src, `\b`) && !strings.Contains(src, `\B`) {
		return src
	}
	rs := []rune(src)
	var b strings.Builder
	b.Grow(len(src) + 16)
	stack := []bool{ignoreCase}
	cur := func() bool { return stack[len(stack)-1] }
	inClass := 0
	set := func() string {
		if cur() {
			return wordClassUnicodeI
		}
		return wordClassPlain
	}
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && i+1 < len(rs) {
			switch rs[i+1] {
			case 'w':
				if inClass > 0 {
					b.WriteString(set())
				} else {
					b.WriteString("[" + set() + "]")
				}
				i++
				continue
			case 'W':
				// Inside a class a complement cannot be spelled inline; regexp2's own
				// \W stands, which only differs for ſ / K.
				if inClass > 0 {
					b.WriteString(`\W`)
				} else {
					b.WriteString("[^" + set() + "]")
				}
				i++
				continue
			case 'b', 'B':
				// A word boundary is defined by the same word set, so it changes with
				// the region too. Only a region that DISAGREES with the flag needs
				// spelling out — elsewhere regexp2's own \b is already right, and the
				// lookaround form is more expensive. (`\b` inside a class is a
				// backspace, not a boundary.)
				if inClass > 0 || cur() == ignoreCase {
					break
				}
				w := "[" + set() + "]"
				nw := "[^" + set() + "]"
				if rs[i+1] == 'b' {
					b.WriteString("(?:(?<=" + w + ")(?=" + nw + "|$)|(?<=" + nw + "|^)(?=" + w + "))")
				} else {
					b.WriteString("(?:(?<=" + w + ")(?=" + w + ")|(?<=" + nw + "|^)(?=" + nw + "|$))")
				}
				i++
				continue
			}
			b.WriteRune(c)
			b.WriteRune(rs[i+1])
			i++
			continue
		}
		switch {
		case c == '[':
			inClass++
		case c == ']':
			if inClass > 0 {
				inClass--
			}
		case inClass > 0:
		case c == '(':
			stack = append(stack, modifiedIgnoreCase(rs, i, cur()))
		case c == ')':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		}
		b.WriteRune(c)
	}
	return b.String()
}

// modifiedIgnoreCase returns the ignoreCase state inside the group opening at
// rs[i], applying the add/remove flags of an inline `(?ims-ims:` modifier group.
func modifiedIgnoreCase(rs []rune, i int, outer bool) bool {
	if i+1 >= len(rs) || rs[i+1] != '?' {
		return outer
	}
	add := true
	for j := i + 2; j < len(rs); j++ {
		switch rs[j] {
		case 'm', 's':
		case 'i':
			outer = add
		case '-':
			add = false
		case ':':
			return outer
		default:
			return outer // not a modifier group: (?: (?= (?! (?<name> …
		}
	}
	return outer
}
