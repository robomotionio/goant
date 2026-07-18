package regexpjs

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// This file evaluates the `v`-flag ClassSetExpression grammar (ES2024 22.2.1):
// a class body is a union, intersection, or subtraction of operands, where an
// operand may itself be a nested class, a \q{…} string disjunction, a property
// escape, or a single character. The result is a set of code points plus a set
// of multi-code-point strings, which is rendered back as a regexp2 alternation.
//
// Sets are kept as sorted disjoint intervals rather than one entry per code
// point: complementing a property (\P{…}) spans the whole 0..10FFFF range, and
// materialising that as individual members is far too slow.

// cpRange is an inclusive code-point interval.
type cpRange struct{ lo, hi rune }

// cpSet is a set of code points as sorted, disjoint, non-adjacent intervals.
type cpSet []cpRange

const maxCP = 0x10FFFF

// norm sorts the intervals and coalesces the overlapping and adjacent ones.
func (s cpSet) norm() cpSet {
	if len(s) < 2 {
		return s
	}
	sort.Slice(s, func(i, j int) bool { return s[i].lo < s[j].lo })
	out := s[:1]
	for _, r := range s[1:] {
		last := &out[len(out)-1]
		if r.lo <= last.hi+1 {
			if r.hi > last.hi {
				last.hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

func (a cpSet) or(b cpSet) cpSet {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	return append(append(cpSet{}, a...), b...).norm()
}

func (a cpSet) and(b cpSet) cpSet {
	var out cpSet
	for i, j := 0, 0; i < len(a) && j < len(b); {
		lo, hi := max(a[i].lo, b[j].lo), min(a[i].hi, b[j].hi)
		if lo <= hi {
			out = append(out, cpRange{lo, hi})
		}
		if a[i].hi < b[j].hi {
			i++
		} else {
			j++
		}
	}
	return out
}

// not returns the complement of s within 0..10FFFF.
func (s cpSet) not() cpSet {
	var out cpSet
	next := rune(0)
	for _, r := range s {
		if r.lo > next {
			out = append(out, cpRange{next, r.lo - 1})
		}
		if r.hi+1 > next {
			next = r.hi + 1
		}
	}
	if next <= maxCP {
		out = append(out, cpRange{next, maxCP})
	}
	return out
}

func (a cpSet) minus(b cpSet) cpSet { return a.and(b.not()) }

func cpOne(c rune) cpSet { return cpSet{{c, c}} }

// cpFromTable converts a Unicode range table to interval form.
func cpFromTable(rt *unicode.RangeTable) cpSet {
	var out cpSet
	for _, r := range rt.R16 {
		if r.Stride == 1 {
			out = append(out, cpRange{rune(r.Lo), rune(r.Hi)})
			continue
		}
		for c := rune(r.Lo); c <= rune(r.Hi); c += rune(r.Stride) {
			out = append(out, cpRange{c, c})
		}
	}
	for _, r := range rt.R32 {
		if r.Stride == 1 {
			out = append(out, cpRange{rune(r.Lo), rune(r.Hi)})
			continue
		}
		for c := rune(r.Lo); c <= rune(r.Hi); c += rune(r.Stride) {
			out = append(out, cpRange{c, c})
		}
	}
	return out.norm()
}

// classSet is the value of a ClassSetExpression: code points, plus the strings
// of two or more code points contributed by \q{…} and by properties of strings.
// A one-code-point string is just that code point, so it lands in cps.
type classSet struct {
	cps  cpSet
	strs []string
}

func (s classSet) union(o classSet) classSet {
	return classSet{s.cps.or(o.cps), mergeStrings(s.strs, o.strs, nil)}
}

func (s classSet) intersect(o classSet) classSet {
	keep := map[string]bool{}
	for _, x := range o.strs {
		keep[x] = true
	}
	return classSet{s.cps.and(o.cps), mergeStrings(filterStrings(s.strs, keep, true), nil, nil)}
}

func (s classSet) subtract(o classSet) classSet {
	drop := map[string]bool{}
	for _, x := range o.strs {
		drop[x] = true
	}
	return classSet{s.cps.minus(o.cps), filterStrings(s.strs, drop, false)}
}

func filterStrings(strs []string, set map[string]bool, keep bool) []string {
	var out []string
	for _, x := range strs {
		if set[x] == keep {
			out = append(out, x)
		}
	}
	return out
}

func mergeStrings(a, b, _ []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, x := range append(append([]string{}, a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// pattern renders the set as a regexp2 sub-pattern. Strings are tried before the
// code-point class and longest first, since ES matches the longest alternative.
func (s classSet) pattern() string {
	if len(s.strs) == 0 {
		if len(s.cps) == 0 {
			return "(?!)" // the empty set never matches
		}
		return s.cps.class()
	}
	strs := append([]string{}, s.strs...)
	sort.SliceStable(strs, func(i, j int) bool {
		return len([]rune(strs[i])) > len([]rune(strs[j]))
	})
	var b strings.Builder
	b.WriteString("(?:")
	for _, x := range strs {
		for _, c := range x {
			b.WriteString(esc(uint32(c)))
		}
		b.WriteByte('|')
	}
	if len(s.cps) == 0 {
		// Trim the trailing '|' so an empty class does not add an empty branch.
		return strings.TrimSuffix(b.String(), "|") + ")"
	}
	b.WriteString(s.cps.class())
	b.WriteByte(')')
	return b.String()
}

// class renders the code points as a regexp2 character class.
func (s cpSet) class() string {
	var b strings.Builder
	b.WriteByte('[')
	for _, r := range s {
		if r.lo == r.hi {
			b.WriteString(esc(uint32(r.lo)))
			continue
		}
		b.WriteString(esc(uint32(r.lo)) + "-" + esc(uint32(r.hi)))
	}
	b.WriteByte(']')
	return b.String()
}

// csParser is a recursive-descent parser for one v-mode class body.
type csParser struct {
	rs []rune
	i  int
}

func (p *csParser) eof() bool { return p.i >= len(p.rs) }

func (p *csParser) has(s string) bool {
	return p.i+len(s) <= len(p.rs) && string(p.rs[p.i:p.i+len(s)]) == s
}

// classContents parses a class body up to (not consuming) its closing ']'.
func (p *csParser) classContents() (classSet, error) {
	if p.eof() || p.rs[p.i] == ']' {
		return classSet{}, nil
	}
	acc, err := p.operandOrRange()
	if err != nil {
		return classSet{}, err
	}
	switch {
	case p.has("&&"):
		for p.has("&&") {
			p.i += 2
			rhs, err := p.operand()
			if err != nil {
				return classSet{}, err
			}
			acc = acc.intersect(rhs)
		}
	case p.has("--"):
		for p.has("--") {
			p.i += 2
			rhs, err := p.operand()
			if err != nil {
				return classSet{}, err
			}
			acc = acc.subtract(rhs)
		}
	default:
		for !p.eof() && p.rs[p.i] != ']' {
			rhs, err := p.operandOrRange()
			if err != nil {
				return classSet{}, err
			}
			acc = acc.union(rhs)
		}
	}
	if !p.eof() && p.rs[p.i] != ']' {
		return classSet{}, fmt.Errorf("invalid character class")
	}
	return acc, nil
}

// operandOrRange parses an operand and, when it is a lone character followed by
// `-`, the ClassSetRange it starts.
func (p *csParser) operandOrRange() (classSet, error) {
	start := p.i
	s, err := p.operand()
	if err != nil {
		return classSet{}, err
	}
	lo, single := p.singleChar(start, p.i, s)
	if !single || !p.has("-") || p.has("--") {
		return s, nil
	}
	if p.i+1 < len(p.rs) && p.rs[p.i+1] == ']' {
		return s, nil
	}
	p.i++ // consume '-'
	hstart := p.i
	hi, err := p.operand()
	if err != nil {
		return classSet{}, err
	}
	hc, single := p.singleChar(hstart, p.i, hi)
	if !single {
		return classSet{}, fmt.Errorf("invalid class range")
	}
	if hc < lo {
		return classSet{}, fmt.Errorf("range out of order in character class")
	}
	return classSet{cps: cpSet{{lo, hc}}}, nil
}

// singleChar reports whether the operand spanning rs[from:to] was a single
// ClassSetCharacter (the only thing that may bound a range) and returns it.
func (p *csParser) singleChar(from, to int, s classSet) (rune, bool) {
	if len(s.strs) != 0 || len(s.cps) != 1 || s.cps[0].lo != s.cps[0].hi {
		return 0, false
	}
	if p.rs[from] == '[' || p.rs[from] == '\\' && from+1 < to && isClassEscapeLetter(p.rs[from+1]) {
		return 0, false // a nested class or \d-style escape is not a range bound
	}
	return s.cps[0].lo, true
}

// operand parses one ClassSetOperand.
func (p *csParser) operand() (classSet, error) {
	if p.eof() {
		return classSet{}, fmt.Errorf("unterminated character class")
	}
	switch c := p.rs[p.i]; {
	case c == '[':
		return p.nestedClass()
	case c == '\\':
		return p.escape()
	case c == ']':
		return classSet{}, fmt.Errorf("unexpected ']' in character class")
	default:
		p.i++
		return classSet{cps: cpOne(c)}, nil
	}
}

func (p *csParser) nestedClass() (classSet, error) {
	p.i++ // '['
	negate := false
	if !p.eof() && p.rs[p.i] == '^' {
		negate = true
		p.i++
	}
	s, err := p.classContents()
	if err != nil {
		return classSet{}, err
	}
	if p.eof() || p.rs[p.i] != ']' {
		return classSet{}, fmt.Errorf("unterminated character class")
	}
	p.i++ // ']'
	if !negate {
		return s, nil
	}
	if len(s.strs) != 0 {
		return classSet{}, fmt.Errorf("negated character class may not contain strings")
	}
	return classSet{cps: s.cps.not()}, nil
}

// escape parses a `\`-introduced operand: a character class escape, a property
// escape, a \q{…} string disjunction, or an escaped character.
func (p *csParser) escape() (classSet, error) {
	if p.i+1 >= len(p.rs) {
		return classSet{}, fmt.Errorf("trailing backslash in character class")
	}
	switch p.rs[p.i+1] {
	case 'd', 'D', 's', 'S', 'w', 'W':
		c := p.rs[p.i+1]
		p.i += 2
		return classSet{cps: builtinClassSet(c)}, nil
	case 'p', 'P':
		return p.propertyEscape()
	case 'q':
		return p.stringDisjunction()
	}
	c, adv, err := classSetChar(p.rs, p.i)
	if err != nil {
		return classSet{}, err
	}
	p.i += adv
	return classSet{cps: cpOne(c)}, nil
}

func (p *csParser) propertyEscape() (classSet, error) {
	negate := p.rs[p.i+1] == 'P'
	if p.i+2 >= len(p.rs) || p.rs[p.i+2] != '{' {
		return classSet{}, fmt.Errorf("invalid property escape")
	}
	end := p.i + 3
	for end < len(p.rs) && p.rs[end] != '}' {
		end++
	}
	if end >= len(p.rs) {
		return classSet{}, fmt.Errorf("incomplete property escape")
	}
	name := string(p.rs[p.i+3 : end])
	p.i = end + 1
	if strs, ok := stringPropertyMembers(name); ok {
		if negate {
			return classSet{}, fmt.Errorf("\\P may not name a property of strings")
		}
		return setFromMembers(strs), nil
	}
	rt, ok := resolveUnicodeProperty(name)
	if !ok {
		return classSet{}, fmt.Errorf("unknown unicode category or property `%s`", name)
	}
	set := cpFromTable(rt)
	if negate {
		set = set.not()
	}
	return classSet{cps: set}, nil
}

// stringDisjunction parses `\q{a|bc|…}`.
func (p *csParser) stringDisjunction() (classSet, error) {
	if p.i+2 >= len(p.rs) || p.rs[p.i+2] != '{' {
		return classSet{}, fmt.Errorf("invalid \\q escape")
	}
	i := p.i + 3
	var members []string
	var cur []rune
	for {
		if i >= len(p.rs) {
			return classSet{}, fmt.Errorf("unterminated \\q{…}")
		}
		if p.rs[i] == '}' {
			members = append(members, string(cur))
			i++
			break
		}
		if p.rs[i] == '|' {
			members = append(members, string(cur))
			cur = cur[:0]
			i++
			continue
		}
		c, adv, err := classSetChar(p.rs, i)
		if err != nil {
			return classSet{}, err
		}
		cur = append(cur, c)
		i += adv
	}
	p.i = i
	return setFromMembers(members), nil
}

// setFromMembers splits matched strings into the code-point and string parts.
func setFromMembers(members []string) classSet {
	var s classSet
	for _, m := range members {
		rs := []rune(m)
		if len(rs) == 1 {
			s.cps = s.cps.or(cpOne(rs[0]))
			continue
		}
		s.strs = append(s.strs, m)
	}
	s.strs = mergeStrings(s.strs, nil, nil)
	return s
}

// classSetChar decodes one character at rs[i], resolving the escapes legal in a
// v-mode class, and returns how many runes it spans.
func classSetChar(rs []rune, i int) (rune, int, error) {
	if rs[i] != '\\' {
		return rs[i], 1, nil
	}
	if i+1 >= len(rs) {
		return 0, 0, fmt.Errorf("trailing backslash in character class")
	}
	switch c := rs[i+1]; c {
	case 'n':
		return '\n', 2, nil
	case 'r':
		return '\r', 2, nil
	case 't':
		return '\t', 2, nil
	case 'f':
		return '\f', 2, nil
	case 'v':
		return '\v', 2, nil
	case 'b':
		return '\b', 2, nil
	case '0':
		return 0, 2, nil
	case 'x':
		if i+3 < len(rs) {
			return rune(hexVal(rs[i+2 : i+4])), 4, nil
		}
		return 0, 0, fmt.Errorf("invalid \\x escape")
	case 'c':
		if i+2 < len(rs) && isASCIILetter(rs[i+2]) {
			return rs[i+2] % 32, 3, nil
		}
		return 0, 0, fmt.Errorf("invalid \\c escape")
	case 'u':
		if i+2 < len(rs) && rs[i+2] == '{' {
			end := i + 3
			for end < len(rs) && rs[end] != '}' {
				end++
			}
			if end >= len(rs) {
				return 0, 0, fmt.Errorf("invalid \\u{…} escape")
			}
			return rune(hexVal(rs[i+3 : end])), end - i + 1, nil
		}
		if i+5 < len(rs) {
			hi := rune(hexVal(rs[i+2 : i+6]))
			// A surrogate pair spelled as two \uXXXX escapes is one code point.
			if hi >= 0xD800 && hi <= 0xDBFF && i+11 < len(rs) && rs[i+6] == '\\' && rs[i+7] == 'u' {
				lo := rune(hexVal(rs[i+8 : i+12]))
				if lo >= 0xDC00 && lo <= 0xDFFF {
					return 0x10000 + (hi-0xD800)<<10 + lo - 0xDC00, 12, nil
				}
			}
			return hi, 6, nil
		}
		return 0, 0, fmt.Errorf("invalid \\u escape")
	default:
		return c, 2, nil
	}
}

// builtinClassSet returns the code points of \d, \s, \w and their complements.
func builtinClassSet(c rune) cpSet {
	digit := cpSet{{'0', '9'}}
	word := cpSet{{'0', '9'}, {'A', 'Z'}, {'_', '_'}, {'a', 'z'}}
	space := cpSet{
		{0x09, 0x0D}, {0x20, 0x20}, {0xA0, 0xA0}, {0x1680, 0x1680}, {0x2000, 0x200A},
		{0x2028, 0x2029}, {0x202F, 0x202F}, {0x205F, 0x205F}, {0x3000, 0x3000}, {0xFEFF, 0xFEFF},
	}
	switch c {
	case 'd':
		return digit
	case 'D':
		return digit.not()
	case 'w':
		return word
	case 'W':
		return word.not()
	case 's':
		return space
	}
	return space.not()
}

// stringPropertyMembers decodes a property of strings from the generated table,
// whose entries are regexp2 alternations of escaped code-point sequences.
func stringPropertyMembers(name string) ([]string, bool) {
	pat, ok := u17StringProperties[name]
	if !ok {
		return nil, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(pat, "(?:"), ")")
	var out []string
	for _, alt := range strings.Split(body, "|") {
		rs := []rune(alt)
		var cur []rune
		for i := 0; i < len(rs); {
			c, adv, err := classSetChar(rs, i)
			if err != nil {
				return nil, false
			}
			cur = append(cur, c)
			i += adv
		}
		out = append(out, string(cur))
	}
	return out, true
}
