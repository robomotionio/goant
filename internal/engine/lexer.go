package engine

// Port of ant src/silver/lexer.c — the byte tokenizer. Behavioral parity is
// the goal: token boundaries, ASI newline tracking, numeric-literal forms
// (hex/octal/binary/legacy-octal/bigint + numeric separators), string/template
// scanning, HTML-comment and hashbang skipping, and Unicode identifiers.
//
// Regex literals are NOT tokenized here (ant doesn't either): the parser
// re-lexes a `/` as a regex when the grammar allows one (see scanRegex).

import (
	"strconv"
	"unicode"
	"unicode/utf8"
)

// character-class flags (lexer.h CHAR_*).
const (
	charDigit  = 0x01
	charXDigit = 0x02
	charAlpha  = 0x04
	charIdent  = 0x08
	charIdent1 = 0x10
	charWS     = 0x20
	charOctal  = 0x40
)

var charType = func() [256]uint8 {
	var t [256]uint8
	t['\t'], t['\n'], t['\r'], t[' '] = charWS, charWS, charWS, charWS
	for c := '0'; c <= '7'; c++ {
		t[c] = charDigit | charXDigit | charIdent | charOctal
	}
	t['8'] = charDigit | charXDigit | charIdent
	t['9'] = charDigit | charXDigit | charIdent
	for c := 'A'; c <= 'F'; c++ {
		t[c] = charXDigit | charAlpha | charIdent | charIdent1
		t[c+('a'-'A')] = charXDigit | charAlpha | charIdent | charIdent1
	}
	for c := 'G'; c <= 'Z'; c++ {
		t[c] = charAlpha | charIdent | charIdent1
		t[c+('a'-'A')] = charAlpha | charIdent | charIdent1
	}
	t['_'] = charIdent | charIdent1
	t['$'] = charIdent | charIdent1
	return t
}()

func isDigitByte(c byte) bool  { return charType[c]&charDigit != 0 }
func isXDigitByte(c byte) bool { return charType[c]&charXDigit != 0 }
func isIdentByte(c byte) bool  { return charType[c]&charIdent != 0 }
func isIdent1Byte(c byte) bool { return charType[c]&charIdent1 != 0 }
func isOctalByte(c byte) bool  { return charType[c]&charOctal != 0 }

var singleCharTok = func() [128]Token {
	var t [128]Token
	t['('] = TokLParen
	t[')'] = TokRParen
	t['{'] = TokLBrace
	t['}'] = TokRBrace
	t['['] = TokLBracket
	t[']'] = TokRBracket
	t[';'] = TokSemicolon
	t[','] = TokComma
	t[':'] = TokColon
	t['~'] = TokTilda
	t['#'] = TokHash
	return t
}()

type lexState struct {
	pos  int
	toff int
	tlen int
	// prevEnd is the source offset just past the previous consumed token (set
	// when next() advances). It gives a concise arrow body's true end, since by
	// the time its span is recorded the current token is already the following
	// one (e.g. the `)` after `x=>x+1`).
	prevEnd    int
	tval       Value
	tok        Token
	consumed   bool
	hadNewline bool
	// escKeyword marks a TokErr produced because an identifier with a unicode
	// escape spells a reserved word (`if`). Such a token is invalid as a
	// keyword / identifier reference, but IS a valid IdentifierName, so
	// property-name and property-key parsing accept it.
	escKeyword bool
}

type lexer struct {
	code   string
	strict bool
	module bool // Module goal: HTML-comment openers/closers are not comments
	// noHTMLClose suppresses the `-->` SingleLineHTMLCloseComment. It is set when
	// the source is not a Script or Module but a fragment parsed on its own goal
	// symbol — the dynamic Function constructor's parameter text — where the
	// HTMLCloseComment production of InputElementHashbangOrRegExp does not apply.
	noHTMLClose bool
	// commentErr is set when whitespace skipping runs off the end of an
	// unterminated block comment (`/* …` with no closing `*/`). Per the spec an
	// unterminated MultiLineComment is an early SyntaxError, so nextRaw turns this
	// into a TokErr rather than reaching a clean EOF.
	commentErr bool
	st         lexState
}

func newLexer(code string, strict bool) *lexer {
	return &lexer{
		code:   code,
		strict: strict,
		st:     lexState{tval: mkundef(), tok: TokErr, consumed: true},
	}
}

func (l *lexer) save() lexState      { return l.st }
func (l *lexer) restore(st lexState) { l.st = st }
func (l *lexer) tokenText() string   { return l.code[l.st.toff : l.st.toff+l.st.tlen] }

// lexCheckpoint captures the lexer position for pushSource/popSource
// (ant sv_lexer_push_source / pop_source), used to lex template-expression
// sub-sources.
type lexCheckpoint struct {
	code   string
	strict bool
	st     lexState
}

func (l *lexer) pushSource(code string) lexCheckpoint {
	cp := lexCheckpoint{l.code, l.strict, l.st}
	l.code = code
	l.st = lexState{tval: mkundef(), tok: TokErr, consumed: true}
	return cp
}

func (l *lexer) popSource(cp lexCheckpoint) {
	l.code = cp.code
	l.strict = cp.strict
	l.st = cp.st
}

// ---- unicode identifier helpers (ant is_unicode_id_start/continue) ----

func isUnicodeIDStart(r rune) bool {
	if r == 0x2E2F {
		return false
	}
	if unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Lm, unicode.Lo, unicode.Nl) {
		return true
	}
	// idStartExt carries the ID_Start additions from Unicode 15.1/16.0, which Go's
	// bundled (15.0) category tables above do not yet include.
	return isOtherIDStart(r) || unicode.Is(idStartExt, r)
}

func isUnicodeIDContinue(r rune) bool {
	if isUnicodeIDStart(r) {
		return true
	}
	if r == 0x200C || r == 0x200D {
		return true
	}
	if unicode.In(r, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc) {
		return true
	}
	// idContinueExt carries the ID_Continue additions from Unicode 15.1/16.0.
	return isOtherIDContinue(r) || unicode.Is(idContinueExt, r)
}

// isOtherIDStart / isOtherIDContinue implement Unicode's Other_ID_Start and
// Other_ID_Continue property sets (PropList.txt). ECMAScript's ID_Start/
// ID_Continue include these grandfathered characters (e.g. ℘ U+2118, the
// Ethiopic digits) even though their general category (Sm/So/Sk/Po/No) is not a
// letter/mark/number-letter — so a plain-category check misses them.
func isOtherIDStart(r rune) bool {
	switch r {
	case 0x1885, 0x1886, 0x2118, 0x212E, 0x309B, 0x309C:
		return true
	}
	return false
}

func isOtherIDContinue(r rune) bool {
	switch {
	case r == 0x00B7, r == 0x0387, r == 0x19DA:
		return true
	case r >= 0x1369 && r <= 0x1371:
		return true
	}
	return false
}

// isUnicodeSpace mirrors ant is_unicode_space: returns the byte width of a
// Unicode whitespace char at code[i:], and whether it's a line terminator.
func (l *lexer) isUnicodeSpace(i int) (width int, lineTerm bool) {
	r, n := utf8.DecodeRuneInString(l.code[i:])
	if r == utf8.RuneError && n <= 1 {
		return 0, false
	}
	if r == 0xFEFF {
		return n, false
	}
	if unicode.Is(unicode.Zs, r) {
		return n, false
	}
	if unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
		return n, true
	}
	return 0, false
}

// ---- whitespace / comment skipping (ant sv_skiptonext) ----

func (l *lexer) skipToNext(n int) (int, bool) {
	code := l.code
	end := len(code)
	sawNL := false
	p := n

	// hashbang at very start: runs to the next LineTerminator (LF, CR, or the
	// UTF-8 LS/PS sequences), which the main whitespace loop below then consumes
	// (setting sawNL) — so a multi-byte LS/PS terminator is handled uniformly.
	if p == 0 && end >= 2 && code[0] == '#' && code[1] == '!' {
		for p += 2; p < end && !isSingleByteLineTerm(code[p]) && !isLSorPS(code, p, end); p++ {
		}
	}

	for p < end {
		c := code[p]
		if c >= 0x80 {
			w, lt := l.isUnicodeSpace(p)
			if w > 0 {
				if lt {
					sawNL = true
				}
				p += w
				continue
			}
			break
		}
		switch c {
		case ' ', '\t', '\v', '\f':
			p++
		case '\n', '\r':
			// Both LF and CR are LineTerminators (relevant to ASI).
			sawNL = true
			p++
		case '/':
			if p+1 >= end {
				goto done
			}
			if code[p+1] == '/' {
				// A single-line comment runs to the next LineTerminator: LF, CR, or
				// the UTF-8 LS/PS sequences (left unconsumed for the newline handling).
				p += 2
				for p < end && !isSingleByteLineTerm(code[p]) &&
					!isLSorPS(code, p, end) {
					p++
				}
			} else if code[p+1] == '*' {
				p += 2
				closed := false
				for p+1 < end {
					if code[p] == '*' && code[p+1] == '/' {
						p += 2
						closed = true
						break
					}
					// A MultiLineComment containing any LineTerminator (LF, CR, or the
					// LS/PS sequences) counts as a LineTerminator for ASI.
					if code[p] == '\n' || code[p] == '\r' || (code[p] >= 0x80 && isLSorPS(code, p, end)) {
						sawNL = true
					}
					p++
				}
				if !closed {
					// Unterminated block comment: no `*/` before end of input.
					l.commentErr = true
					p = end
					goto done
				}
			} else {
				goto done
			}
		default:
			goto done
		}
	}

done:
	// HTML comments: <!-- ... and (at line start) --> ... (Annex B; a Module
	// has no HTML-comment grammar, so there they tokenize as operators → an error).
	for !l.module {
		if p+3 < end && code[p] == '<' && code[p+1] == '!' && code[p+2] == '-' && code[p+3] == '-' {
			p = l.skipHTMLLineComment(p+4, end, &sawNL)
		} else if p+2 < end && code[p] == '-' && code[p+1] == '-' && code[p+2] == '>' &&
			(sawNL || htmlCloseAtLineStart(code, p, !l.noHTMLClose)) {
			// A `-->` HTML close comment must follow a LineTerminatorSequence and then
			// only whitespace / delimited comments. sawNL covers a preceding multiline
			// comment that itself contained a newline (`/* … \n … */-->`), which
			// htmlCloseAtLineStart (scanning raw text) misses.
			p = l.skipHTMLLineComment(p+3, end, &sawNL)
		} else {
			break
		}
		// after skipping an HTML comment, continue skipping whitespace/comments
		p2, nl2 := l.skipToNext(p)
		if nl2 {
			sawNL = true
		}
		if p2 == p {
			break
		}
		p = p2
	}
	return p, sawNL
}

// isSingleByteLineTerm reports whether c is an ASCII LineTerminator (LF or CR).
func isSingleByteLineTerm(c byte) bool { return c == '\n' || c == '\r' }

// isLSorPS reports whether code[p:] begins with the UTF-8 encoding of LINE
// SEPARATOR (U+2028) or PARAGRAPH SEPARATOR (U+2029), both LineTerminators.
func isLSorPS(code string, p, end int) bool {
	return p+2 < end && code[p] == 0xE2 && code[p+1] == 0x80 &&
		(code[p+2] == 0xA8 || code[p+2] == 0xA9)
}

func (l *lexer) skipHTMLLineComment(p, end int, sawNL *bool) int {
	for p < end && l.code[p] != '\n' {
		p++
	}
	if p < end {
		*sawNL = true
		p++
	}
	return p
}

// htmlCloseAtLineStart reports whether the `-->` at p may open a
// SingleLineHTMLCloseComment. lenient adds the two acceptances that only a
// Script/Module goal has: the very start of the input, and (matching common
// engines) a `-->` directly after an opening bracket.
func htmlCloseAtLineStart(code string, p int, lenient bool) bool {
	for p > 0 {
		c := code[p-1]
		if c == '\n' || c == '\r' {
			return true
		}
		if c == ' ' || c == '\t' || c == '\f' || c == '\v' {
			p--
			continue
		}
		// HTMLCloseComment allows a SingleLineDelimitedCommentSequence before the
		// `-->`, so step back over each `/* … */` that contains no LineTerminator:
		// `/* a */ /* b */--> …` on a line of its own is all comment.
		if c == '/' && p >= 2 && code[p-2] == '*' {
			open := -1
			for i := p - 3; i >= 1; i-- {
				if code[i] == '*' && code[i-1] == '/' {
					open = i - 1
					break
				}
			}
			if open < 0 {
				return false
			}
			for i := open; i < p-2; i++ {
				if code[i] == '\n' || code[i] == '\r' || (code[i] >= 0x80 && isLSorPS(code, i, p)) {
					return false
				}
			}
			p = open
			continue
		}
		// A statement separator (`;`, `,`) does NOT get the bracket leniency:
		// `;-->` is a token sequence and a SyntaxError.
		switch c {
		case '{', '(', '[':
			return lenient
		}
		return false
	}
	return lenient
}

// ---- identifiers ----

// parseIdent scans an identifier at buf, returning its keyword token (or
// TokIdentifier) and the byte length consumed.
func (l *lexer) parseIdent(off int) (Token, int) {
	code := l.code
	buf := code[off:]
	if len(buf) == 0 {
		return TokErr, 0
	}
	c := buf[0]
	hasEscapes := false
	i := 0

	if c < 0x80 && c != '\\' && isIdent1Byte(c) {
		i = 1
		for i < len(buf) {
			d := buf[i]
			if d >= 0x80 || d == '\\' {
				goto slow
			}
			if !isIdentByte(d) {
				break
			}
			i++
		}
		return parseKeyword(buf[:i]), i
	}
	if c == '\\' {
		cp, el := parseUnicodeEscape(buf, 0)
		if el <= 0 || !isUnicodeIdentBegin(cp) {
			return TokErr, 0
		}
		i = el
		hasEscapes = true
		goto slowLoop
	}
	if c >= 0x80 {
		r, n := utf8.DecodeRuneInString(buf)
		if r == utf8.RuneError || !isUnicodeIDStart(r) {
			return TokErr, 0
		}
		i = n
		goto slowLoop
	}
	return TokErr, 0

slow:
	// fallthrough from ASCII fast path hitting an escape/high byte
slowLoop:
	for i < len(buf) {
		d := buf[i]
		if d == '\\' {
			cp, el := parseUnicodeEscape(buf, i)
			if el <= 0 || !isUnicodeIdentContinue(cp) {
				break
			}
			i += el
			hasEscapes = true
		} else if d < 0x80 {
			if !isIdentByte(d) {
				break
			}
			i++
		} else {
			r, n := utf8.DecodeRuneInString(buf[i:])
			if r == utf8.RuneError || !isUnicodeIDContinue(r) {
				break
			}
			i += n
		}
	}
	if hasEscapes {
		decoded := decodeIdentEscapes(buf[:i])
		if parseKeyword(decoded) != TokIdentifier {
			// An escaped identifier that spells a reserved word is not usable as
			// a keyword or identifier reference (TokErr), but is a valid
			// IdentifierName — the parser accepts it as a property name/key.
			l.st.escKeyword = true
			return TokErr, i
		}
		return TokIdentifier, i
	}
	return parseKeyword(buf[:i]), i
}

func isUnicodeIdentBegin(cp uint32) bool {
	if cp < 128 {
		return charType[cp]&charIdent1 != 0
	}
	return isUnicodeIDStart(rune(cp))
}

func isUnicodeIdentContinue(cp uint32) bool {
	if cp < 128 {
		return charType[cp]&(charIdent|charIdent1) != 0
	}
	return isUnicodeIDContinue(rune(cp))
}

// parseUnicodeEscape decodes a \uXXXX or \u{...} escape at buf[pos], returning
// the code point and byte length consumed (0 on failure).
func parseUnicodeEscape(buf string, pos int) (uint32, int) {
	if pos+3 >= len(buf) || buf[pos] != '\\' || buf[pos+1] != 'u' {
		return 0, 0
	}
	if buf[pos+2] == '{' {
		i := pos + 3
		var cp uint32
		nd := 0
		for i < len(buf) && isXDigitByte(buf[i]) {
			cp = (cp << 4) | hexVal(buf[i])
			i++
			nd++
			if cp > 0x10FFFF {
				return 0, 0
			}
		}
		if nd == 0 || i >= len(buf) || buf[i] != '}' {
			return 0, 0
		}
		return cp, i - pos + 1
	}
	if pos+5 >= len(buf) {
		return 0, 0
	}
	var cp uint32
	for i := 0; i < 4; i++ {
		d := buf[pos+2+i]
		if !isXDigitByte(d) {
			return 0, 0
		}
		cp = (cp << 4) | hexVal(d)
	}
	return cp, 6
}

func hexVal(c byte) uint32 {
	switch {
	case c <= '9':
		return uint32(c - '0')
	default:
		return uint32((c|0x20)-'a') + 10
	}
}

func decodeIdentEscapes(src string) string {
	out := make([]byte, 0, len(src))
	i := 0
	for i < len(src) {
		if cp, el := parseUnicodeEscape(src, i); el > 0 {
			out = utf8.AppendRune(out, rune(cp))
			i += el
		} else {
			out = append(out, src[i])
			i++
		}
	}
	return string(out)
}

// ---- numbers ----

func hasNumericSep(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			return true
		}
	}
	return false
}

// hasBadSeparator reports whether a numeric literal misuses `_` separators: each
// `_` must sit directly between two digits of the number's radix (so it may not
// lead, trail, double up, or abut a `.`, exponent `e`, sign, or radix prefix —
// isDigit is false for all of those). s is the digit portion for the radix.
func hasBadSeparator(s string, isDigit func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			continue
		}
		if i == 0 || i+1 >= len(s) || !isDigit(s[i-1]) || !isDigit(s[i+1]) {
			return true
		}
	}
	return false
}

func stripSep(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

func scanDecimalLiteral(buf string) int {
	i := 0
	for i < len(buf) && (isDigitByte(buf[i]) || buf[i] == '_') {
		i++
	}
	if i < len(buf) && buf[i] == '.' {
		i++
		for i < len(buf) && (isDigitByte(buf[i]) || buf[i] == '_') {
			i++
		}
	}
	if i < len(buf) && (buf[i]|0x20) == 'e' {
		i++
		if i < len(buf) && (buf[i] == '+' || buf[i] == '-') {
			i++
		}
		for i < len(buf) && (isDigitByte(buf[i]) || buf[i] == '_') {
			i++
		}
	}
	return i
}

// decimalBigIntEligible reports whether a scanned decimal literal (possibly with
// `_` separators) may legally carry a BigInt `n` suffix. Per the grammar, only
// `0` or a NonZeroDigit-led integer qualifies: a fractional part, an exponent, or
// a superfluous leading zero (08, 09) all disqualify it.
func decimalBigIntEligible(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '.' || c == 'e' || c == 'E' {
			return false
		}
	}
	return len(s) == 1 || s[0] != '0'
}

// leadingZeroOctalRun reports whether the digit run after a leading `0` is a
// LegacyOctalIntegerLiteral (every digit 0-7). A run containing 8 or 9 (e.g.
// 0790) is a NonOctalDecimalIntegerLiteral and must be evaluated in base 10.
func leadingZeroOctalRun(buf string) bool {
	for i := 1; i < len(buf) && isDigitByte(buf[i]); i++ {
		if buf[i] == '8' || buf[i] == '9' {
			return false
		}
	}
	return true
}

// leadingZeroSepBad reports whether a leading-zero integer literal misuses `_`
// separators. A LegacyOctal / NonOctalDecimal integer (0 followed by more integer
// digits) admits no separators at all, so any `_` in the integer part (before a
// `.` or exponent) is a SyntaxError — e.g. 0_0, 08_0, 0_0123456789.
func leadingZeroSepBad(digits string) bool {
	if len(digits) < 2 || digits[0] != '0' {
		return false
	}
	for i := 0; i < len(digits); i++ {
		switch digits[i] {
		case '.', 'e', 'E':
			return false // reached fractional/exponent with no integer-part separator
		case '_':
			return true
		}
	}
	return false
}

// parseDecimalLiteral scans and evaluates a decimal literal, returning its
// length, the float64 value, and ok.
func parseDecimalLiteral(buf string) (length int, value float64, ok bool) {
	n := scanDecimalLiteral(buf)
	digits := buf[:n]
	if hasNumericSep(digits) {
		digits = stripSep(digits)
	}
	v, err := parseJSFloat(digits)
	if err != nil {
		// Overflow to ±Inf is a valid numeric literal (StringNumericValue rounds an
		// out-of-range magnitude to Infinity); only a genuine parse failure is !ok.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return n, v, true
		}
		return n, v, false
	}
	return n, v, true
}

// parseJSFloat parses a cleaned decimal literal (no separators) to float64 with
// correct round-to-nearest, matching JS StringNumericValue.
func parseJSFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

func parseRadix(buf string, radix float64, start int, isDigit func(byte) bool) (int, float64) {
	var val float64
	i := start
	for i < len(buf) && (isDigit(buf[i]) || buf[i] == '_') {
		if buf[i] != '_' {
			val = val*radix + float64(digitVal(buf[i]))
		}
		i++
	}
	return i, val
}

func digitVal(c byte) int {
	switch {
	case c >= 'a':
		return int(c-'a') + 10
	case c >= 'A':
		return int(c-'A') + 10
	default:
		return int(c - '0')
	}
}

func numberHasInvalidTail(buf string, toklen int) bool {
	if toklen >= len(buf) {
		return false
	}
	c := buf[toklen]
	if c < 0x80 {
		return charType[c]&(charIdent|charIdent1) != 0
	}
	// A multi-byte follower only invalidates the numeric literal if it is a
	// Unicode IdentifierPart (`1é`); whitespace and line terminators like LS/PS
	// (which begin with 0xE2) legitimately separate the number from the next token.
	r, _ := utf8.DecodeRuneInString(buf[toklen:])
	return isUnicodeIdentContinue(uint32(r))
}

func isIdentContinueByte(c byte) bool {
	if c >= 0x80 {
		return true
	}
	return charType[c]&(charIdent|charIdent1) != 0
}

// parseNumber scans a numeric literal at buf, filling token state.
func (l *lexer) parseNumber(off int) {
	buf := l.code[off:]
	var value float64
	numlen := 0

	sepBad := false
	// bigIntOK tracks whether the scanned form may legally carry a BigInt `n`
	// suffix: `0`, a non-leading-zero decimal integer, or a 0b/0o/0x literal —
	// never a legacy octal (07n), non-octal decimal (08n), or fractional/exponent.
	bigIntOK := false
	if buf[0] == '0' && len(buf) > 1 {
		c1 := buf[1] | 0x20
		isBin := func(c byte) bool { return c == '0' || c == '1' }
		switch {
		case c1 == 'b':
			numlen, value = parseRadix(buf, 2, 2, isBin)
			if numlen == 2 { // "0b" with no binary digits
				l.st.tok, l.st.tlen = TokErr, 2
				return
			}
			sepBad = hasBadSeparator(buf[2:numlen], isBin)
			bigIntOK = true
		case c1 == 'o':
			numlen, value = parseRadix(buf, 8, 2, isOctalByte)
			if numlen == 2 { // "0o" with no octal digits
				l.st.tok, l.st.tlen = TokErr, 2
				return
			}
			sepBad = hasBadSeparator(buf[2:numlen], isOctalByte)
			bigIntOK = true
		case c1 == 'x':
			numlen, value = parseRadix(buf, 16, 2, isXDigitByte)
			if numlen == 2 { // "0x" with no hex digits
				l.st.tok, l.st.tlen = TokErr, 2
				return
			}
			sepBad = hasBadSeparator(buf[2:numlen], isXDigitByte)
			bigIntOK = true
		case isOctalByte(buf[1]) && leadingZeroOctalRun(buf):
			if l.strict {
				l.st.tok, l.st.tlen = TokErr, 1
				return
			}
			numlen, value = parseRadix(buf, 8, 1, isOctalByte)
			// A legacy octal literal may not contain separators at all.
			for k := 0; k < numlen; k++ {
				if buf[k] == '_' {
					sepBad = true
					break
				}
			}
		case isDigitByte(buf[1]) && l.strict:
			l.st.tok, l.st.tlen = TokErr, 1
			return
		default:
			var ok bool
			numlen, value, ok = parseDecimalLiteral(buf)
			if !ok {
				l.st.tok, l.st.tlen = TokErr, numlen
				return
			}
			sepBad = hasBadSeparator(buf[:numlen], isDigitByte) || leadingZeroSepBad(buf[:numlen])
			bigIntOK = decimalBigIntEligible(buf[:numlen])
		}
	} else {
		var ok bool
		numlen, value, ok = parseDecimalLiteral(buf)
		if !ok {
			l.st.tok, l.st.tlen = TokErr, numlen
			return
		}
		sepBad = hasBadSeparator(buf[:numlen], isDigitByte)
		bigIntOK = decimalBigIntEligible(buf[:numlen])
	}
	if sepBad {
		l.st.tok, l.st.tlen = TokErr, numlen
		return
	}

	l.st.tval = tov(value)
	toklen := numlen
	if toklen < len(buf) && buf[toklen] == 'n' {
		if !bigIntOK {
			l.st.tok, l.st.tlen = TokErr, toklen+1
			return
		}
		l.st.tok = TokBigInt
		toklen++
	} else {
		l.st.tok = TokNumber
	}
	if numberHasInvalidTail(buf, toklen) {
		l.st.tok, l.st.tlen = TokErr, toklen
		return
	}
	l.st.tlen = toklen
}

// ---- strings & templates ----

func (l *lexer) scanString(off int, quote byte) {
	buf := l.code[off:]
	rem := len(buf)
	i := 1
	for i < rem {
		// find next quote or backslash
		q, b := -1, -1
		for j := i; j < rem; j++ {
			if buf[j] == quote {
				q = j
				break
			}
			if buf[j] == '\\' {
				b = j
				break
			}
			// A raw LF or CR may not appear in a string literal (only escaped or via
			// a line continuation). LS (U+2028) and PS (U+2029) ARE permitted since
			// ES2019's JSON-superset change, so they are deliberately not rejected.
			if buf[j] == '\n' || buf[j] == '\r' {
				l.st.tok, l.st.tlen = TokErr, j
				return
			}
		}
		if q == -1 && b == -1 {
			l.st.tok, l.st.tlen = TokErr, rem
			return
		}
		if b == -1 || (q != -1 && q < b) {
			l.st.tok, l.st.tlen = TokString, q+1
			return
		}
		escPos := b
		if escPos+1 >= rem {
			l.st.tok, l.st.tlen = TokErr, rem
			return
		}
		escChar := buf[escPos+1]
		// Legacy octal (\1–\7, or \0 followed by a digit) and non-octal decimal
		// (\8, \9) escapes are SyntaxErrors in strict mode (Annex B.1.2).
		if l.strict {
			if escChar >= '1' && escChar <= '9' {
				l.st.tok, l.st.tlen = TokErr, escPos+2
				return
			}
			if escChar == '0' && escPos+2 < rem && buf[escPos+2] >= '0' && buf[escPos+2] <= '9' {
				l.st.tok, l.st.tlen = TokErr, escPos+3
				return
			}
		}
		skip := 2
		if escChar == '\r' && escPos+2 < rem && buf[escPos+2] == '\n' {
			// LineTerminatorSequence :: <CR><LF> is ONE terminator, so a line
			// continuation before it consumes both bytes; leaving the LF behind
			// would then be read as a raw newline in the literal.
			skip = 3
		}
		if escChar == 'x' {
			// \xHH requires exactly two hexadecimal digits.
			if escPos+3 >= rem || !isXDigitByte(buf[escPos+2]) || !isXDigitByte(buf[escPos+3]) {
				l.st.tok, l.st.tlen = TokErr, escPos+2
				return
			}
			skip = 4
		} else if escChar == 'u' {
			if escPos+2 < rem && buf[escPos+2] == '{' {
				// \u{H+} needs at least one hex digit (no separators), a closing
				// '}', and a code point <= 0x10FFFF.
				j := escPos + 3
				var cp uint32
				for j < rem && isXDigitByte(buf[j]) {
					cp = cp<<4 | uint32(hexVal(buf[j]))
					if cp > 0x10FFFF {
						cp = 0x110000 // sentinel: out of range
					}
					j++
				}
				if j == escPos+3 || j >= rem || buf[j] != '}' || cp > 0x10FFFF {
					l.st.tok, l.st.tlen = TokErr, j
					return
				}
				skip = j - escPos + 1
			} else {
				// \uHHHH requires exactly four hexadecimal digits.
				if escPos+5 >= rem ||
					!isXDigitByte(buf[escPos+2]) || !isXDigitByte(buf[escPos+3]) ||
					!isXDigitByte(buf[escPos+4]) || !isXDigitByte(buf[escPos+5]) {
					l.st.tok, l.st.tlen = TokErr, escPos+2
					return
				}
				skip = 6
			}
		}
		if escPos+skip > rem {
			l.st.tok, l.st.tlen = TokErr, rem
			return
		}
		i = escPos + skip
	}
	l.st.tok, l.st.tlen = TokErr, rem
}

func (l *lexer) scanTemplate(off int) {
	buf := l.code[off:]
	rem := len(buf)
	end, closed := skipTemplateLiteral(buf, rem, 0)
	if !closed || end <= 1 || end > rem {
		l.st.tok, l.st.tlen = TokErr, rem
		return
	}
	l.st.tok, l.st.tlen = TokTemplate, end
}

func skipStringLiteral(buf string, rem, start int, quote byte) int {
	i := start + 1
	for i < rem {
		if buf[i] == '\\' {
			i += 2
			continue
		}
		if buf[i] == quote {
			return i + 1
		}
		i++
	}
	return rem
}

func skipLineComment(buf string, rem, start int) int {
	i := start + 2
	for i < rem && buf[i] != '\n' {
		i++
	}
	return i
}

func skipBlockComment(buf string, rem, start int) int {
	i := start + 2
	for i+1 < rem && !(buf[i] == '*' && buf[i+1] == '/') {
		i++
	}
	if i+1 < rem {
		return i + 2
	}
	return rem
}

func isExprWS(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isIdentASCIIStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentASCIIContinue(c byte) bool {
	return isIdentASCIIStart(c) || (c >= '0' && c <= '9')
}

func skipRegexLiteral(buf string, rem, start int) int {
	i := start + 1
	inClass := false
	for i < rem {
		c := buf[i]
		if c == '\\' && i+1 < rem {
			i += 2
			continue
		}
		if c == '[' {
			inClass = true
		} else if c == ']' && inClass {
			inClass = false
		} else if c == '/' && !inClass {
			i++
			break
		}
		i++
	}
	for i < rem && isIdentASCIIStart(buf[i]) {
		i++
	}
	return i
}

func regexAllowedAfterIdent(buf string, start, end int) bool {
	switch buf[start:end] {
	case "if", "in", "do", "of", "for", "else", "case", "throw", "return",
		"typeof", "delete", "void", "new", "instanceof":
		return true
	}
	return false
}

func skipTemplateLiteral(buf string, rem, start int) (int, bool) {
	i := start + 1
	exprDepth := 0
	canStartRegex := true

	for i < rem {
		c := buf[i]
		if c == '\\' {
			i += 2
			continue
		}
		if exprDepth == 0 {
			if c == '`' {
				return i + 1, true
			}
			if c == '$' && i+1 < rem && buf[i+1] == '{' {
				exprDepth = 1
				canStartRegex = true
				i += 2
				continue
			}
			i++
			continue
		}
		if c == '\'' || c == '"' {
			i = skipStringLiteral(buf, rem, i, c)
			continue
		}
		if c == '`' {
			next, nested := skipTemplateLiteral(buf, rem, i)
			if !nested || next <= i {
				return rem, false
			}
			i = next
			canStartRegex = false
			continue
		}
		if c == '/' && i+1 < rem {
			if buf[i+1] == '/' {
				i = skipLineComment(buf, rem, i)
				continue
			}
			if buf[i+1] == '*' {
				i = skipBlockComment(buf, rem, i)
				continue
			}
			if canStartRegex {
				i = skipRegexLiteral(buf, rem, i)
				canStartRegex = false
				continue
			}
			i++
			canStartRegex = true
			continue
		}
		if isExprWS(c) {
			i++
			continue
		}
		if isIdentASCIIStart(c) {
			idStart := i
			i++
			for i < rem && isIdentASCIIContinue(buf[i]) {
				i++
			}
			canStartRegex = regexAllowedAfterIdent(buf, idStart, i)
			continue
		}
		if (c >= '0' && c <= '9') || (c == '.' && i+1 < rem && buf[i+1] >= '0' && buf[i+1] <= '9') {
			i++
			for i < rem {
				d := buf[i]
				if (d >= '0' && d <= '9') || d == '_' || d == '.' {
					i++
				} else {
					break
				}
			}
			canStartRegex = false
			continue
		}
		if c == '{' {
			exprDepth++
			i++
			canStartRegex = true
			continue
		}
		if c == '}' {
			exprDepth--
			i++
			canStartRegex = false
			continue
		}
		switch c {
		case '(', '[', ',', ';', ':', '?', '!', '~', '+', '-', '*', '%',
			'&', '|', '^', '=', '<', '>':
			i++
			canStartRegex = true
			continue
		case ')', ']':
			i++
			canStartRegex = false
			continue
		}
		i++
		canStartRegex = false
	}
	return rem, false
}

// ---- operators ----

func (l *lexer) parseOperator(off int) bool {
	buf := l.code[off:]
	rem := len(buf)
	m2 := func(c2 byte) bool { return rem >= 2 && buf[1] == c2 }
	m3 := func(c2, c3 byte) bool { return rem >= 3 && buf[1] == c2 && buf[2] == c3 }
	m4 := func(c2, c3, c4 byte) bool { return rem >= 4 && buf[1] == c2 && buf[2] == c3 && buf[3] == c4 }
	set := func(t Token, n int) { l.st.tok, l.st.tlen = t, n }

	switch buf[0] {
	case '?':
		switch {
		case m3('?', '='):
			set(TokNullishAssign, 3)
		case m2('?'):
			set(TokNullish, 2)
		case m2('.') && !(rem >= 3 && buf[2] >= '0' && buf[2] <= '9'):
			// `?.` is the optional-chaining punctuator only when not followed by a
			// decimal digit; `x?.3:0` is the conditional `x ? .3 : 0`.
			set(TokOptionalChain, 2)
		default:
			set(TokQ, 1)
		}
	case '!':
		switch {
		case m3('=', '='):
			set(TokSne, 3)
		case m2('='):
			set(TokNe, 2)
		default:
			set(TokNot, 1)
		}
	case '=':
		switch {
		case m3('=', '='):
			set(TokSeq, 3)
		case m2('='):
			set(TokEq, 2)
		case m2('>'):
			set(TokArrow, 2)
		default:
			set(TokAssign, 1)
		}
	case '<':
		switch {
		case m3('<', '='):
			set(TokShlAssign, 3)
		case m2('<'):
			set(TokShl, 2)
		case m2('='):
			set(TokLe, 2)
		default:
			set(TokLt, 1)
		}
	case '>':
		switch {
		case m4('>', '>', '='):
			set(TokZShrAssign, 4)
		case m3('>', '>'):
			set(TokZShr, 3)
		case m3('>', '='):
			set(TokShrAssign, 3)
		case m2('>'):
			set(TokShr, 2)
		case m2('='):
			set(TokGe, 2)
		default:
			set(TokGt, 1)
		}
	case '&':
		switch {
		case m3('&', '='):
			set(TokLandAssign, 3)
		case m2('&'):
			set(TokLand, 2)
		case m2('='):
			set(TokAndAssign, 2)
		default:
			set(TokAnd, 1)
		}
	case '|':
		switch {
		case m3('|', '='):
			set(TokLorAssign, 3)
		case m2('|'):
			set(TokLor, 2)
		case m2('='):
			set(TokOrAssign, 2)
		default:
			set(TokOr, 1)
		}
	case '+':
		switch {
		case m2('+'):
			set(TokPostInc, 2)
		case m2('='):
			set(TokPlusAssign, 2)
		default:
			set(TokPlus, 1)
		}
	case '-':
		switch {
		case m2('-'):
			set(TokPostDec, 2)
		case m2('='):
			set(TokMinusAssign, 2)
		default:
			set(TokMinus, 1)
		}
	case '*':
		switch {
		case m3('*', '='):
			set(TokExpAssign, 3)
		case m2('*'):
			set(TokExp, 2)
		case m2('='):
			set(TokMulAssign, 2)
		default:
			set(TokMul, 1)
		}
	case '/':
		if m2('=') {
			set(TokDivAssign, 2)
		} else {
			set(TokDiv, 1)
		}
	case '%':
		if m2('=') {
			set(TokRemAssign, 2)
		} else {
			set(TokRem, 1)
		}
	case '^':
		if m2('=') {
			set(TokXorAssign, 2)
		} else {
			set(TokXor, 1)
		}
	case '.':
		switch {
		case m3('.', '.'):
			set(TokRest, 3)
		case rem > 1 && isDigitByte(buf[1]):
			n, v, ok := parseDecimalLiteral(buf)
			if !ok || numberHasInvalidTail(buf, n) || hasBadSeparator(buf[:n], isDigitByte) {
				set(TokErr, n)
			} else {
				l.st.tval = tov(v)
				set(TokNumber, n)
			}
		default:
			set(TokDot, 1)
		}
	default:
		return false
	}
	return true
}

// ---- main entry points (ant sv_next_raw / sv_lexer_next / lookahead) ----

func (l *lexer) nextRaw() Token {
	if !l.st.consumed {
		return l.st.tok
	}
	l.st.consumed = false
	l.st.tok = TokErr
	l.st.toff, l.st.hadNewline = l.skipToNext(l.st.pos)
	l.st.pos = l.st.toff
	l.st.tlen = 0
	l.st.escKeyword = false

	if l.commentErr {
		// An unterminated block comment ran off the end of input: an early error.
		l.st.tok = TokErr
		return TokErr
	}

	if l.st.toff >= len(l.code) {
		l.st.tok = TokEOF
		return TokEOF
	}

	off := l.st.toff
	c := l.code[off]

	if c < 128 {
		if t := singleCharTok[c]; t != 0 {
			l.st.tok, l.st.tlen, l.st.pos = t, 1, off+1
			return t
		}
	}
	if c < 128 && isIdent1Byte(c) {
		tok, n := l.parseIdent(off)
		l.st.tok, l.st.tlen = tok, n
		l.st.pos = off + n
		return tok
	}
	if c < 128 && isDigitByte(c) {
		l.parseNumber(off)
		if l.st.tlen == 0 {
			l.st.tlen = 1
		}
		l.st.pos = off + l.st.tlen
		return l.st.tok
	}
	if c == '"' || c == '\'' {
		l.scanString(off, c)
		if l.st.tlen == 0 {
			l.st.tlen = 1
		}
		l.st.pos = off + l.st.tlen
		return l.st.tok
	}
	if c == '`' {
		l.scanTemplate(off)
		if l.st.tlen == 0 {
			l.st.tlen = 1
		}
		l.st.pos = off + l.st.tlen
		return l.st.tok
	}
	if l.parseOperator(off) {
		if l.st.tlen == 0 {
			l.st.tlen = 1
		}
		l.st.pos = off + l.st.tlen
		return l.st.tok
	}
	// Unicode identifier / fallback.
	tok, n := l.parseIdent(off)
	l.st.tok, l.st.tlen = tok, n
	if l.st.tlen == 0 {
		l.st.tlen = 1
	}
	l.st.pos = off + l.st.tlen
	return l.st.tok
}

func (l *lexer) next() Token {
	if !l.st.consumed {
		return l.st.tok
	}
	l.st.prevEnd = l.st.toff + l.st.tlen // end of the token being left behind
	return l.nextRaw()
}

// consume marks the current token consumed so the next next() advances.
func (l *lexer) consume() { l.st.consumed = true }

func (l *lexer) lookahead() Token {
	saved := l.st
	l.st.consumed = true
	tok := l.next()
	l.st = saved
	return tok
}

// scanRegex re-lexes the current `/` position as a regular-expression literal,
// returning the body/flags source span. Used by the parser when a regex is
// grammatically allowed. off is the byte offset of the leading `/`.
func (l *lexer) scanRegex(off int) (end int, ok bool) {
	buf := l.code
	rem := len(buf)
	end = skipRegexLiteral2(buf, rem, off)
	if end <= off+1 {
		return off, false
	}
	return end, true
}

// skipRegexLiteral2 is skipRegexLiteral over the whole code string.
func skipRegexLiteral2(buf string, rem, start int) int {
	i := start + 1
	inClass := false
	for i < rem {
		c := buf[i]
		// A regex literal may not contain any LineTerminator (LF, CR, LS, PS),
		// even after a backslash — such input is an unterminated regex.
		if c == '\n' || c == '\r' || isLSorPS(buf, i, rem) {
			return start
		}
		if c == '\\' {
			if i+1 >= rem {
				return start
			}
			if n := buf[i+1]; n == '\n' || n == '\r' || isLSorPS(buf, i+1, rem) {
				return start
			}
			i += 2
			continue
		}
		if c == '[' {
			inClass = true
		} else if c == ']' && inClass {
			inClass = false
		} else if c == '/' && !inClass {
			i++
			// flags
			for i < rem && (isIdentASCIIContinue(buf[i]) || buf[i] >= 0x80) {
				i++
			}
			return i
		}
		i++
	}
	return start
}

// numberValue returns the boxed numeric token value (for TokNumber/TokBigInt).
func (l *lexer) numberValue() Value { return l.st.tval }
