package regexpjs

// The stdlib fast path.
//
// regexp2 is a backtracking matcher over []rune: four bytes per code unit, three
// struct-field writes per instruction dispatch, and a capture walk per match. Go's
// own regexp is none of those things, and on Octane's RegExp corpus it runs the
// same patterns about three times faster. It is not a drop-in replacement — no
// backreferences, no lookaround, and its own idea of what `\s` and `.` mean — so
// what happens here is a translation on a short leash: a pattern is rewritten
// into RE2 syntax only when every construct in it means the same thing on the
// other side, and the result is used only on subjects where the offsets line up.
// Anything else still goes to regexp2, which stays the reference implementation.
//
// Two restrictions carry most of the safety, and both are checked, not assumed:
//
//   - The subject must be pure ASCII. ECMAScript indexes strings in UTF-16 code
//     units and Go's regexp reports byte offsets; on an ASCII subject those are
//     the same number, so there is no position map to get wrong.
//   - The pattern must be pure ASCII too. That removes the case-folding
//     disagreements in one line — RE2's `(?i)` folds U+212A KELVIN SIGN to `k`
//     and U+017F to `s`, ECMAScript's Canonicalize does neither — along with
//     every question about surrogates in the pattern text.
//
// GOANT_REGEXP_VERIFY=1 runs both matchers on every fast-path call and panics on
// the first disagreement; test262 under it is what says the translation is
// honest rather than merely plausible.

import (
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
)

// VerifyFast makes every fast-path match run the regexp2 matcher as well and
// panic if the two disagree. It is a test hook, not a knob: the cost is running
// both engines.
var VerifyFast bool

// jsSpace is `\s` as ECMAScript defines it — WhiteSpace ∪ LineTerminator, which
// is the ASCII controls and space, NBSP, ZWNBSP, the Unicode Zs category and the
// two non-ASCII line terminators. RE2's own `\s` is `[\t\n\f\r ]`, missing the
// vertical tab and everything above ASCII, so neither `\s` nor `\S` can go
// through as itself.
const jsSpace = `\x{9}-\x{d}\x{20}\x{a0}\x{1680}\x{2000}-\x{200a}` +
	`\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// jsNonSpace is the same class complemented, spelled out because RE2 has no
// negation inside a character class.
const jsNonSpace = `\x{0}-\x{8}\x{e}-\x{1f}\x{21}-\x{9f}\x{a1}-\x{167f}` +
	`\x{1681}-\x{1fff}\x{200b}-\x{2027}\x{202a}-\x{202e}\x{2030}-\x{205e}` +
	`\x{2060}-\x{2fff}\x{3001}-\x{fefe}\x{ff00}-\x{10ffff}`

// jsDot is ECMAScript's `.` without the `s` flag: anything but a line
// terminator. RE2's `.` stops at "\n" alone.
const jsDot = `[^\x{a}\x{d}\x{2028}\x{2029}]`

// neverMatch is a character class with no members — the way to write an
// assertion that always fails, which is what `^` becomes in the suffix program.
const neverMatch = `[^\x{0}-\x{10ffff}]`

// re2conv translates one ECMAScript pattern into RE2 syntax, or gives up. It is
// a plain recursive descent over the pattern's own grammar rather than a rewrite
// of the regexp2 form: the point is to know exactly what each construct means,
// and a source-to-source patch of an already-patched string does not.
type re2conv struct {
	src    string
	i      int
	out    strings.Builder
	groups int
	dotAll bool
	icase  bool
	// word records a `\b` or `\B`, which reads the character before the match and
	// so cannot be answered from a suffix of the subject; caret records a `^`,
	// which can, because outside multiline mode it simply never holds anywhere
	// but position 0. suffix is set on the second pass, which is the one that
	// takes it away.
	word   bool
	caret  bool
	suffix bool
	bad    bool
}

// compileFast returns the RE2 twin of an ECMAScript pattern: head for a search
// that starts at the beginning of the subject and tail for one that starts
// partway in. Go's regexp always searches from the beginning of what it is
// given, so the second kind has to be a search over a suffix — which asks the
// same question only if nothing in the pattern reads what was cut away. `^` is
// the exception that can be translated rather than refused: at any nonzero
// offset it always fails, so the suffix program spells it as an assertion that
// always fails. `\b` cannot be, and leaves tail nil.
func compileFast(pattern string, ignoreCase, dotAll bool) (head, tail *regexp.Regexp, groups int) {
	if !isASCII(pattern) {
		return nil, nil, 0
	}
	build := func(suffix bool) (*regexp.Regexp, *re2conv) {
		c := &re2conv{src: pattern, dotAll: dotAll, icase: ignoreCase, suffix: suffix}
		c.disjunction()
		if c.bad || c.i != len(c.src) {
			return nil, c
		}
		out := c.out.String()
		if ignoreCase {
			out = "(?i:" + out + ")"
		}
		re, err := regexp.Compile(out)
		if err != nil {
			return nil, c
		}
		return re, c
	}
	head, c := build(false)
	if head == nil {
		return nil, nil, 0
	}
	switch {
	case c.word: // no suffix search is possible
	case !c.caret:
		tail = head // nothing to rewrite
	default:
		tail, _ = build(true)
	}
	return head, tail, c.groups
}

// The required literal.
//
// Both matchers answer "does this 2KB string contain a match?" by trying the
// pattern at every one of its 2000 positions. When the pattern contains a
// literal run — Octane's most expensive single pattern needs the eight
// characters `"\/Qngr(` — a match cannot exist unless that run does, and looking
// for a fixed string is a memchr rather than a regex. On the benchmark's
// biggest subjects that one test decides the question for 15,000 calls that
// were otherwise scanning the whole string to conclude nothing.
//
// The literal is read off the RE2 translation's parse tree, so it only exists
// for patterns that have a translation — but it is used on both matchers, since
// what it proves is a property of the pattern, not of the engine.

// requiredLiteral returns a run of characters that every match of re must
// contain, or nil when there is no useful one. Only constructs that must be
// present at least once contribute: an alternation or an optional group could
// match without its literal, and a folded literal is not a fixed string.
func requiredLiteral(re *syntax.Regexp) []rune {
	switch re.Op {
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			return nil
		}
		return re.Rune
	case syntax.OpCapture:
		return requiredLiteral(re.Sub[0])
	case syntax.OpPlus:
		return requiredLiteral(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return requiredLiteral(re.Sub[0])
		}
	case syntax.OpConcat:
		var best []rune
		for _, sub := range re.Sub {
			if lit := requiredLiteral(sub); len(lit) > len(best) {
				best = lit
			}
		}
		return best
	}
	return nil
}

// minRequiredLiteral is how long a required literal has to be to be worth
// looking for. One character is too weak a filter to pay for a whole extra pass.
const minRequiredLiteral = 2

// findRequiredLiteral picks the literal for a compiled RE2 program. Astral
// characters are excluded: the rune-domain scan below works in UTF-16 code
// units, where those are two units rather than one.
func findRequiredLiteral(re *regexp.Regexp) ([]rune, string) {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return nil, ""
	}
	lit := requiredLiteral(parsed.Simplify())
	if len(lit) < minRequiredLiteral {
		return nil, ""
	}
	for _, c := range lit {
		if c > 0xFFFF {
			return nil, ""
		}
	}
	return lit, string(lit)
}

// missingLiteral reports that input holds no match at or after start because the
// pattern's required literal is not there.
//
// Under verification the prescan is skipped here and kept on the fast path, so
// that regexp2 answers without it and a literal that excluded a real match shows
// up as a disagreement rather than as two engines agreeing on the same mistake.
func (r *Regexp) missingLiteral(input []rune, start int) bool {
	r.ensureFast()
	return r.reqLit != nil && !VerifyFast && indexRunes(input[start:], r.reqLit) < 0
}

func indexRunes(hay, needle []rune) int {
	c := needle[0]
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i] != c {
			continue
		}
		j := 1
		for ; j < len(needle) && hay[i+j] == needle[j]; j++ {
		}
		if j == len(needle) {
			return i
		}
	}
	return -1
}

// HasFast reports whether the pattern has an RE2 translation to run at all,
// working it out on the first ask.
func (r *Regexp) HasFast() bool {
	r.ensureFast()
	return r.fast != nil
}

// ExecASCII is Exec for a subject already known to be pure ASCII, run on the
// translated pattern. handled is false when the call is not one the fast path
// can take — no translation, or a search from a nonzero offset by a pattern that
// reads text to the left of it — and the caller must fall back to Exec.
func (r *Regexp) ExecASCII(s string, start int) (m *Match, handled bool) {
	if r.fast == nil || start < 0 || start > len(s) {
		return nil, false
	}
	prog, base, sub := r.fast, 0, s
	if start > 0 {
		if r.fastTail == nil {
			return nil, false
		}
		prog, base, sub = r.fastTail, start, s[start:]
	}
	var loc []int
	if r.reqLitStr == "" || strings.Contains(sub, r.reqLitStr) {
		loc = prog.FindStringSubmatchIndex(sub)
	}
	// A sticky match has to begin exactly where the search did.
	if loc != nil && !(r.Sticky && loc[0] != 0) {
		groups := make([]Group, len(loc)/2)
		for i := range groups {
			a, b := loc[2*i], loc[2*i+1]
			if a < 0 {
				groups[i] = Group{Index: -1}
				continue
			}
			// A substring of a Go string shares its bytes, so the capture values
			// cost nothing to carry.
			groups[i] = Group{Index: a + base, Length: b - a, Value: sub[a:b]}
		}
		m = &Match{Index: loc[0] + base, Groups: groups}
		r.dropStaleCaptures(m)
	}
	if VerifyFast {
		r.verifyFast(s, start, m)
	}
	return m, true
}

// dropStaleCaptures clears a capture that a later iteration of a quantified
// enclosing group should have reset. Neither matcher does that on its own — both
// keep the earlier capture — and ECMAScript reads it as undefined. See
// quantifiedParents.
func (r *Regexp) dropStaleCaptures(out *Match) {
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
}

// verifyFast runs the same match through regexp2 and panics if the two engines
// disagree about anything ECMAScript can observe. Group names are not compared:
// regexp2 numbers its unnamed groups and the fast path leaves them unnamed,
// which every caller already tells apart.
func (r *Regexp) verifyFast(s string, start int, got *Match) {
	want, err := r.Exec([]rune(s), start)
	mismatch := func(why string) {
		panic("regexpjs: fast path disagrees on /" + r.Source + "/" + r.Flags +
			" against " + strconv.Quote(s) + " from " + strconv.Itoa(start) + ": " + why)
	}
	switch {
	case err != nil:
		mismatch("regexp2 failed: " + err.Error())
	case (got == nil) != (want == nil):
		mismatch("one matched and the other did not")
	case got == nil:
		return
	case got.Index != want.Index:
		mismatch("index " + strconv.Itoa(got.Index) + " vs " + strconv.Itoa(want.Index))
	case len(got.Groups) != len(want.Groups):
		mismatch("group count " + strconv.Itoa(len(got.Groups)) + " vs " + strconv.Itoa(len(want.Groups)))
	}
	for i := range got.Groups {
		if got.Groups[i].Index != want.Groups[i].Index ||
			got.Groups[i].Length != want.Groups[i].Length ||
			got.Groups[i].Value != want.Groups[i].Value {
			mismatch("group " + strconv.Itoa(i))
		}
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// at is the byte k ahead of the cursor, or 0 past the end. Callers that care
// about the difference test eof first; the rest are looking for a specific
// non-zero byte and a 0 answers them correctly.
func (c *re2conv) at(k int) byte {
	if c.i+k < len(c.src) {
		return c.src[c.i+k]
	}
	return 0
}

func (c *re2conv) eof() bool { return c.i >= len(c.src) }

// lit emits one literal character in a form no RE2 metacharacter rule can
// reinterpret, inside a character class or out of one. Alphanumerics go through
// as themselves — nothing in either grammar gives them a second meaning — and
// everything else is spelled as a braced hex escape, which is a complete token
// wherever it appears and needs no knowledge of what follows it.
func (c *re2conv) lit(r rune) {
	if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
		c.out.WriteByte(byte(r))
		return
	}
	c.out.WriteString(`\x{`)
	c.out.WriteString(strconv.FormatUint(uint64(r), 16))
	c.out.WriteByte('}')
}

// foldsIntoASCII reports whether case-folding anything in [lo,hi] can reach an
// ASCII character. Two code points do: U+017F LATIN SMALL LETTER LONG S folds
// with `s` and U+212A KELVIN SIGN with `k`. RE2's `(?i)` follows Unicode simple
// folding and would match those; ECMAScript's Canonicalize stops at the first
// step that would turn a non-ASCII character into an ASCII one, and does not.
// A pattern that can name either of them under `i` is therefore left to regexp2.
func foldsIntoASCII(lo, hi rune) bool {
	return lo <= 0x17F && hi >= 0x17F || lo <= 0x212A && hi >= 0x212A
}

// disjunction is `Alternative ('|' Alternative)*`. It reports whether the whole
// thing can match the empty string and whether it contains a capture group;
// quantifier below is the only caller that needs to know, and it needs both.
func (c *re2conv) disjunction() (nullable, capt bool) {
	nullable, capt = c.alternative()
	for !c.bad && c.at(0) == '|' {
		c.i++
		c.out.WriteByte('|')
		n, g := c.alternative()
		nullable = nullable || n
		capt = capt || g
	}
	return nullable, capt
}

func (c *re2conv) alternative() (nullable, capt bool) {
	nullable = true
	for !c.bad && !c.eof() && c.at(0) != '|' && c.at(0) != ')' {
		n, g := c.term()
		nullable = nullable && n
		capt = capt || g
	}
	return nullable, capt
}

func (c *re2conv) term() (nullable, capt bool) {
	switch c.at(0) {
	case '^':
		c.i++
		c.caret = true
		if c.suffix {
			c.out.WriteString(neverMatch)
		} else {
			c.out.WriteByte('^')
		}
		c.rejectQuantifier()
		return true, false
	case '$':
		// `$` looks right, not left, so it does not disqualify a suffix search.
		c.i++
		c.out.WriteByte('$')
		c.rejectQuantifier()
		return true, false
	case '\\':
		if b := c.at(1); b == 'b' || b == 'B' {
			c.i += 2
			c.out.WriteByte('\\')
			c.out.WriteByte(b)
			c.word = true
			c.rejectQuantifier()
			return true, false
		}
	}
	n, g := c.atom()
	if c.bad {
		return false, false
	}
	return c.quantifier(n, g)
}

// rejectQuantifier refuses a quantified assertion. `^*` is a SyntaxError in
// ECMAScript and Annex B only lets a lookahead be quantified, which this
// translation does not accept anyway — but regexp2 is lenient enough to compile
// some of these, so the fast path declines rather than guesses.
func (c *re2conv) rejectQuantifier() {
	if _, _, _, ok := quantAt(c.src, c.i); ok {
		c.bad = true
	}
}

// quantifier applies a trailing quantifier, if there is one, to an atom already
// emitted.
func (c *re2conv) quantifier(nullable, capt bool) (bool, bool) {
	text, min, max, ok := quantAt(c.src, c.i)
	if !ok {
		return nullable, capt
	}
	// Go's parser has a repeat-count ceiling ECMAScript does not.
	if min > re2MaxRepeat || max > re2MaxRepeat {
		c.bad = true
		return false, false
	}
	// An ECMAScript iteration that consumed nothing is thrown away and the
	// captures inside it are reset with it (22.2.2.3.1, RepeatMatcher step 4);
	// RE2 keeps whatever the last iteration wrote, so /(a*)*/ on "b" leaves group
	// 1 empty there and undefined here. The two can only disagree when the body
	// both matches empty and has something to capture, which is exactly what is
	// refused.
	if nullable && capt {
		c.bad = true
		return false, false
	}
	c.i += len(text)
	c.out.WriteString(text)
	if c.at(0) == '?' { // lazy
		c.i++
		c.out.WriteByte('?')
	}
	return nullable || min == 0, capt
}

// quantAt reads the quantifier at src[i], returning its text and its repeat
// bounds (max = -1 for unbounded). ok is false only when there is no quantifier
// there at all — in Annex B a `{` that does not spell one is a literal brace, so
// this is the test the atom parser uses to tell the two apart, and a braced
// count too large for Go to compile still has to come back as a quantifier
// rather than as three literal characters.
func quantAt(src string, i int) (text string, min, max int, ok bool) {
	if i >= len(src) {
		return "", 0, 0, false
	}
	switch src[i] {
	case '*':
		return src[i : i+1], 0, -1, true
	case '?':
		return src[i : i+1], 0, 1, true
	case '+':
		return src[i : i+1], 1, -1, true
	case '{':
	default:
		return "", 0, 0, false
	}
	j := i + 1
	lo, digits := 0, 0
	for ; j < len(src) && src[j] >= '0' && src[j] <= '9'; j++ {
		lo = clampCountTo30(lo*10 + int(src[j]-'0'))
		digits++
	}
	if digits == 0 {
		return "", 0, 0, false
	}
	hi := lo
	if j < len(src) && src[j] == ',' {
		j++
		hi, digits = 0, 0
		for ; j < len(src) && src[j] >= '0' && src[j] <= '9'; j++ {
			hi = clampCountTo30(hi*10 + int(src[j]-'0'))
			digits++
		}
		if digits == 0 {
			hi = -1
		}
	}
	if j >= len(src) || src[j] != '}' {
		return "", 0, 0, false
	}
	return src[i : j+1], lo, hi, true
}

// clampCountTo30 keeps a wild repeat count from overflowing on its way to being
// rejected; anything this large is over the limit either way.
func clampCountTo30(v int) int {
	if v > 1<<30 {
		return 1 << 30
	}
	return v
}

// re2MaxRepeat is the largest repeat count Go's regexp parser accepts. A pattern
// over it is not an error in ECMAScript, just one regexp2 has to run.
const re2MaxRepeat = 1000

func (c *re2conv) atom() (nullable, capt bool) {
	switch b := c.at(0); b {
	case '.':
		c.i++
		if c.dotAll {
			c.out.WriteString(`(?s:.)`)
		} else {
			c.out.WriteString(jsDot)
		}
		return false, false
	case '(':
		return c.group()
	case '[':
		c.class()
		return false, false
	case '\\':
		c.escape(false)
		return false, false
	case '*', '+', '?', ')', '|':
		c.bad = true // nothing to quantify / not an atom
		return false, false
	case '{':
		// A `{` that begins a quantifier here has no atom in front of it, which
		// is an error; one that does not is an Annex B literal brace.
		if _, _, _, ok := quantAt(c.src, c.i); ok {
			c.bad = true
			return false, false
		}
		c.i++
		c.lit('{')
		return false, false
	default:
		c.i++
		c.lit(rune(b))
		return false, false
	}
}

func (c *re2conv) group() (nullable, capt bool) {
	if c.at(1) == '?' {
		// Lookahead, lookbehind, named groups and inline modifier groups all
		// start this way and none of them are translated.
		if c.at(2) != ':' {
			c.bad = true
			return false, false
		}
		c.i += 3
		c.out.WriteString("(?:")
	} else {
		c.i++
		c.groups++
		capt = true
		c.out.WriteByte('(')
	}
	n, g := c.disjunction()
	if c.bad || c.at(0) != ')' {
		c.bad = true
		return false, false
	}
	c.i++
	c.out.WriteByte(')')
	return n, capt || g
}

// escape consumes one backslash escape and emits it. It returns the character
// the escape stands for, or ok=false when the escape is a character class (`\d`
// and friends) rather than a single character — a class cannot be the endpoint
// of a class range, which is the only reason the distinction is needed.
func (c *re2conv) escape(inClass bool) (ch rune, ok bool) {
	c.i++ // the backslash
	if c.eof() {
		c.bad = true
		return 0, false
	}
	b := c.src[c.i]
	c.i++
	switch b {
	case 'd', 'D', 'w', 'W':
		// RE2's `\d` and `\w` are the ASCII classes ECMAScript defines, and their
		// complements agree with ECMAScript's on every ASCII character.
		c.out.WriteByte('\\')
		c.out.WriteByte(b)
		return 0, false
	case 's':
		if inClass {
			c.out.WriteString(jsSpace)
		} else {
			c.out.WriteString("[" + jsSpace + "]")
		}
		return 0, false
	case 'S':
		// RE2's own `\s` is missing the vertical tab, so neither `\s` nor `\S` can
		// go through as itself.
		if inClass {
			c.out.WriteString(jsNonSpace)
		} else {
			c.out.WriteString("[^" + jsSpace + "]")
		}
		return 0, false
	case 'b':
		if !inClass {
			c.bad = true // handled as an assertion in term; here it is a stray
			return 0, false
		}
		c.lit(8) // ClassEscape :: b is a backspace
		return 8, true
	case 'f':
		c.lit(12)
		return 12, true
	case 'n':
		c.lit(10)
		return 10, true
	case 'r':
		c.lit(13)
		return 13, true
	case 't':
		c.lit(9)
		return 9, true
	case 'v':
		c.lit(11)
		return 11, true
	case '0':
		// `\0` is NUL, but `\0` followed by a digit is a legacy octal escape.
		if !c.eof() && c.src[c.i] >= '0' && c.src[c.i] <= '9' {
			c.bad = true
			return 0, false
		}
		c.lit(0)
		return 0, true
	case 'x':
		return c.hexEscape(2)
	case 'u':
		return c.hexEscape(4)
	case 'c':
		if c.eof() || !isASCIILetter(rune(c.src[c.i])) {
			c.bad = true // Annex B: a literal backslash and `c`
			return 0, false
		}
		v := rune(c.src[c.i] % 32)
		c.i++
		c.lit(v)
		return v, true
	default:
		// Backreferences (`\1`…`\9`, `\k<…>`), property escapes (`\p`) and any
		// escape whose meaning is not settled above are not translated. What is
		// left is an identity escape for a punctuation character.
		if b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' {
			c.bad = true
			return 0, false
		}
		c.lit(rune(b))
		return rune(b), true
	}
}

// hexEscape reads the fixed-width hex escapes `\xHH` and `\uHHHH`. A short one
// is an Annex B identity escape for the letter, which is not translated. The
// character it names may be non-ASCII — nothing in an ASCII subject can match
// it, but it still has to be emitted, since dropping it would turn "never
// matches" into "matches the empty string".
func (c *re2conv) hexEscape(width int) (rune, bool) {
	if c.i+width > len(c.src) {
		c.bad = true
		return 0, false
	}
	v, err := strconv.ParseUint(c.src[c.i:c.i+width], 16, 32)
	if err != nil {
		c.bad = true
		return 0, false
	}
	c.i += width
	r := rune(v)
	if c.icase && foldsIntoASCII(r, r) {
		c.bad = true
		return 0, false
	}
	c.lit(r)
	return r, true
}

func (c *re2conv) class() {
	c.i++ // '['
	neg := c.at(0) == '^'
	if neg {
		c.i++
	}
	if c.at(0) == ']' {
		// Neither of these spells itself in RE2: an empty class matches nothing
		// and a negated empty class matches every code unit.
		c.i++
		if neg {
			c.out.WriteString(`(?s:.)`)
		} else {
			c.out.WriteString(neverMatch)
		}
		return
	}
	c.out.WriteByte('[')
	if neg {
		c.out.WriteByte('^')
	}
	for {
		if c.bad {
			return
		}
		if c.eof() {
			c.bad = true
			return
		}
		if c.at(0) == ']' {
			c.i++
			break
		}
		lo, isChar := c.classAtom()
		if c.bad {
			return
		}
		// A `-` before the closing bracket is a literal, and so is one whose left
		// side was a class escape — Annex B reads `[a-\w]` as three members, which
		// is a rewrite regexp2 gets and this does not.
		if c.at(0) != '-' || c.at(1) == ']' || c.i+1 >= len(c.src) {
			continue
		}
		if !isChar {
			c.bad = true
			return
		}
		c.i++
		c.out.WriteByte('-')
		hi, isChar2 := c.classAtom()
		if c.bad {
			return
		}
		if !isChar2 || hi < lo || c.icase && foldsIntoASCII(lo, hi) {
			c.bad = true
			return
		}
	}
	c.out.WriteByte(']')
}

// classAtom emits one member of a character class and reports the character it
// stands for (ok=false for a class escape, which cannot bound a range).
func (c *re2conv) classAtom() (rune, bool) {
	if c.eof() {
		c.bad = true
		return 0, false
	}
	if c.src[c.i] == '\\' {
		return c.escape(true)
	}
	b := c.src[c.i]
	c.i++
	c.lit(rune(b))
	return rune(b), true
}
