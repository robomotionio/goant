package regexpjs

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// isValidGroupName reports whether name is a RegExpIdentifierName: an
// IdentifierStart followed by IdentifierParts. ID_Start is approximated by
// letters + Nl, ID_Continue by letters + Nl + Nd + Mn + Mc + Pc; `$`, `_`, and
// ZWJ/ZWNJ are always allowed. Emoji, spaces and lone surrogates are rejected.
func isValidGroupName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		start := c == '$' || c == '_' || unicode.IsLetter(c) || unicode.Is(unicode.Nl, c)
		if i == 0 {
			if !start {
				return false
			}
			continue
		}
		cont := start || c == 0x200C || c == 0x200D || unicode.IsDigit(c) ||
			unicode.Is(unicode.Mn, c) || unicode.Is(unicode.Mc, c) || unicode.Is(unicode.Pc, c)
		if !cont {
			return false
		}
	}
	return true
}

// validateDuplicateGroupNames enforces the ECMAScript early error that forbids
// two named capture groups sharing a name unless they sit in separate
// alternatives of a Disjunction (where at most one can ever match). The
// duplicate-named-groups proposal (ES2025) permits `(?<a>x)|(?<a>y)` but still
// rejects `(?<a>x)(?<a>y)`; regexp2 gives every group a unique internal name and
// so never reports either, so the conflict is detected here.
//
// Each group is tagged with its alternation path: the vector of branch indices
// of every enclosing group level (and the top level), where a `|` advances the
// branch index of the current level. Two same-named groups are in separate
// alternatives iff their paths diverge — differ in branch index — at some shared
// level; if neither ever diverges from the other, both can match and it is an
// error.
func validateDuplicateGroupNames(src string) error {
	rs := []rune(src)
	type def struct {
		name string
		path []int
	}
	var defs []def
	stack := []int{0} // branch index per open level; index 0 is the top level
	inClass := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' {
			i++ // skip the escaped rune
			continue
		}
		if inClass {
			if c == ']' {
				inClass = false
			}
			continue
		}
		switch c {
		case '[':
			inClass = true
		case '|':
			stack[len(stack)-1]++
		case '(':
			if i+2 < len(rs) && rs[i+1] == '?' && rs[i+2] == '<' &&
				!(i+3 < len(rs) && (rs[i+3] == '=' || rs[i+3] == '!')) {
				if name, _, err := decodeGroupName(rs, i+3); err == nil {
					path := make([]int, len(stack))
					copy(path, stack)
					defs = append(defs, def{name, path})
				}
			}
			stack = append(stack, 0) // this group opens a fresh alternation scope
		case ')':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	for a := 0; a < len(defs); a++ {
		for b := a + 1; b < len(defs); b++ {
			if defs[a].name == defs[b].name && !pathsDiverge(defs[a].path, defs[b].path) {
				return fmt.Errorf("duplicate capture group name '%s'", defs[a].name)
			}
		}
	}
	return nil
}

// pathsDiverge reports whether two alternation paths choose different branches at
// some shared level (meaning the two groups are in separate alternatives).
func pathsDiverge(p1, p2 []int) bool {
	n := min(len(p1), len(p2))
	for i := range n {
		if p1[i] != p2[i] {
			return true
		}
	}
	return false
}

// translateGroupNames rewrites every named capture group `(?<name>…)` and named
// backreference `\k<name>` to a safe internal name (`__gN`) that regexp2 accepts,
// returning the rewritten source plus a map from the internal name back to the
// original ECMAScript name (with any \u escapes decoded). ES permits `$`, `_`,
// ZWJ/ZWNJ and arbitrary IdentifierPart code points — including \u escapes — in
// group names, none of which regexp2's `\w+` name grammar allows; and it permits
// duplicate names across disjoint alternatives, which unique internal names make
// compilable.
func translateGroupNames(src string) (string, map[string]string, []bool, error) {
	rs := []rune(src)
	names := map[string]string{}   // decoded original name -> internal name
	reverse := map[string]string{} // internal name -> decoded original name

	// Pass 1: collect every named-group definition, so that backreferences —
	// which may appear before their group (forward references are legal) — resolve.
	inClass := false
	counter := 0
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' {
			i++ // skip the escaped rune
			continue
		}
		if c == '[' {
			inClass = true
			continue
		}
		if c == ']' {
			inClass = false
			continue
		}
		if !inClass && c == '(' && i+2 < len(rs) && rs[i+1] == '?' && rs[i+2] == '<' &&
			!(i+3 < len(rs) && (rs[i+3] == '=' || rs[i+3] == '!')) {
			name, end, err := decodeGroupName(rs, i+3)
			if err != nil {
				return "", nil, nil, err
			}
			if !isValidGroupName(name) {
				return "", nil, nil, fmt.Errorf("invalid capture group name")
			}
			in := "__g" + strconv.Itoa(counter)
			counter++
			names[name] = in
			reverse[in] = name
			i = end
		}
	}
	if counter == 0 {
		return src, nil, nil, nil // no named groups; leave the source untouched
	}

	// Pass 2: rewrite definitions and defined backreferences to the internal
	// names; a \k<name> with no matching definition is left untouched (a
	// non-Unicode Annex-B literal, or a Unicode-mode error regexp2 will report).
	var out strings.Builder
	var kinds []bool // per capture group, in definition order: true=named
	inClass = false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' {
			if !inClass && i+1 < len(rs) && rs[i+1] == 'k' && i+2 < len(rs) && rs[i+2] == '<' {
				name, end, err := decodeGroupName(rs, i+3)
				if err != nil {
					return "", nil, nil, err
				}
				if in, ok := names[name]; ok {
					out.WriteString("\\k<")
					out.WriteString(in)
					out.WriteByte('>')
					i = end
					continue
				}
				// Undefined reference: emit \k< and let the name flow through.
				out.WriteString("\\k<")
				i += 2
				continue
			}
			out.WriteRune(c)
			if i+1 < len(rs) {
				out.WriteRune(rs[i+1])
				i++
			}
			continue
		}
		if c == '[' {
			inClass = true
			out.WriteRune(c)
			continue
		}
		if c == ']' {
			inClass = false
			out.WriteRune(c)
			continue
		}
		if !inClass && c == '(' {
			if i+2 < len(rs) && rs[i+1] == '?' && rs[i+2] == '<' &&
				!(i+3 < len(rs) && (rs[i+3] == '=' || rs[i+3] == '!')) {
				name, end, _ := decodeGroupName(rs, i+3)
				kinds = append(kinds, true)
				out.WriteString("(?<")
				out.WriteString(names[name])
				out.WriteByte('>')
				i = end
				continue
			}
			if !(i+1 < len(rs) && rs[i+1] == '?') {
				kinds = append(kinds, false)
			}
			out.WriteRune(c)
			continue
		}
		out.WriteRune(c)
	}
	return out.String(), reverse, kinds, nil
}

// isRegexSyntaxChar reports whether c is a SyntaxCharacter: one whose sole
// IdentityEscape form (`\c`) is legal in Unicode mode.
func isRegexSyntaxChar(c rune) bool {
	switch c {
	case '^', '$', '\\', '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '|':
		return true
	}
	return false
}

// isClassEscapeLetter reports whether c introduces a CharacterClassEscape
// (`\d \D \s \S \w \W`), which may not stand as an endpoint of a ClassRange.
func isClassEscapeLetter(c rune) bool {
	switch c {
	case 'd', 'D', 's', 'S', 'w', 'W':
		return true
	}
	return false
}

// isQuantifierBrace reports whether rs[i] == '{' begins a valid quantifier
// `{n}` / `{n,}` / `{n,m}`. In Unicode mode a `{` that does not is an error.
func isQuantifierBrace(rs []rune, i int) bool {
	j := i + 1
	digits := 0
	for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
		j++
		digits++
	}
	if digits == 0 {
		return false
	}
	if j < len(rs) && rs[j] == ',' {
		j++
		for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
			j++
		}
	}
	return j < len(rs) && rs[j] == '}'
}

// validateUnicodePattern enforces the Unicode-mode (`u`/`v`) Pattern early errors
// that Annex B permits under the plain grammar but Unicode mode forbids, and that
// regexp2 does not report: an unrecognized IdentityEscape (`\M`, `\a`, `\c` not
// followed by a control letter), a decimal escape or named backreference to a
// group that does not exist, a lone `{` that is not a quantifier, and a
// ClassRange with a CharacterClassEscape endpoint (`[\d-a]`). It runs only in
// Unicode mode, where these are all SyntaxErrors.
func validateUnicodePattern(pattern string, unicodeSets bool) error {
	rs := []rune(pattern)

	// Pass 1: count capturing groups and collect named-group names, so a
	// backreference can be checked against what actually exists.
	groupCount := 0
	named := map[string]bool{}
	inClass := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' {
			i++
			continue
		}
		if inClass {
			if c == ']' {
				inClass = false
			}
			continue
		}
		switch {
		case c == '[':
			inClass = true
		case c == '(' && !(i+1 < len(rs) && rs[i+1] == '?'):
			groupCount++
		case c == '(' && i+2 < len(rs) && rs[i+1] == '?' && rs[i+2] == '<' &&
			!(i+3 < len(rs) && (rs[i+3] == '=' || rs[i+3] == '!')):
			groupCount++
			if name, end, err := decodeGroupName(rs, i+3); err == nil {
				named[name] = true
				i = end
			}
		}
	}

	// Pass 2: validate escapes, the lone-`{` quantifier rule, and class ranges.
	inClass = false
	prevClassEsc := false // previous class atom was a CharacterClassEscape
	haveLeft := false     // a left ClassAtom exists (a range may follow)
	for i := 0; i < len(rs); {
		c := rs[i]
		if c == '\\' {
			adv, classEsc, err := validateUEscape(rs, i, inClass, groupCount, named)
			if err != nil {
				return err
			}
			if inClass {
				prevClassEsc = classEsc
				haveLeft = true
			}
			i += adv
			continue
		}
		if !inClass {
			switch {
			case c == '[':
				inClass = true
				prevClassEsc = false
				haveLeft = false
			case c == '{' && !isQuantifierBrace(rs, i):
				return fmt.Errorf("lone `{` is not a valid quantifier in unicode mode")
			}
			i++
			continue
		}
		// Inside a character class.
		switch {
		case c == ']':
			inClass = false
		case c == '-' && !unicodeSets && haveLeft && i+1 < len(rs) && rs[i+1] != ']':
			// A ClassRange `left - right`: neither endpoint may be a class escape.
			// (Only in plain `u` mode; under `v`, `--` is the set-difference operator
			// and class contents are checked by validateVModeClasses.)
			if prevClassEsc {
				return fmt.Errorf("invalid class range with a character-class-escape endpoint")
			}
			if rs[i+1] == '\\' && i+2 < len(rs) && isClassEscapeLetter(rs[i+2]) {
				return fmt.Errorf("invalid class range with a character-class-escape endpoint")
			}
			haveLeft = false
			prevClassEsc = false
		default:
			haveLeft = true
			prevClassEsc = false
		}
		i++
	}
	return nil
}

// validateUEscape validates the escape at rs[i] ('\\') in Unicode mode, returning
// the number of runes it spans and whether it is a CharacterClassEscape.
func validateUEscape(rs []rune, i int, inClass bool, groupCount int, named map[string]bool) (adv int, classEsc bool, err error) {
	if i+1 >= len(rs) {
		return 0, false, fmt.Errorf("trailing backslash in regular expression")
	}
	n := rs[i+1]
	switch {
	case isClassEscapeLetter(n):
		return 2, true, nil
	case n == 'b' || n == 'B' || n == 'f' || n == 'n' || n == 'r' || n == 't' || n == 'v':
		return 2, false, nil
	case isRegexSyntaxChar(n) || n == '/' || n == '-':
		return 2, false, nil
	case n == '0':
		return 2, false, nil
	case n == 'p' || n == 'P':
		// A Unicode property escape MUST be brace-delimited in Unicode mode; `\pL`
		// (regexp2's braceless .NET form) is a SyntaxError. It is also a
		// CharacterClassEscape, so it may not be a ClassRange endpoint.
		if i+2 >= len(rs) || rs[i+2] != '{' {
			return 0, false, fmt.Errorf("`\\%c` must be `\\%c{…}` in unicode mode", n, n)
		}
		j := i + 3
		for j < len(rs) && rs[j] != '}' {
			j++
		}
		if j >= len(rs) {
			return 0, false, fmt.Errorf("unterminated `\\%c{…}` escape in unicode mode", n)
		}
		return j - i + 1, true, nil
	case n == 'x' || n == 'u' || n == 'q':
		// Consume the brace-delimited escapes `\u{…}` / `\q{…}` wholesale so their
		// `{` is not misread as a lone quantifier brace. `\uHHHH` / `\xHH` have no
		// braces; their hex digits are left to regexp2.
		if i+2 < len(rs) && rs[i+2] == '{' {
			j := i + 3
			for j < len(rs) && rs[j] != '}' {
				j++
			}
			if j >= len(rs) {
				return 0, false, fmt.Errorf("unterminated `\\%c{…}` escape in unicode mode", n)
			}
			return j - i + 1, false, nil
		}
		return 2, false, nil
	case n == 'c':
		if i+2 < len(rs) && isASCIILetter(rs[i+2]) {
			return 3, false, nil
		}
		return 0, false, fmt.Errorf("invalid `\\c` control escape in unicode mode")
	case n == 'k':
		if inClass {
			return 2, false, nil // no named backreference inside a class; stay lenient
		}
		if i+2 >= len(rs) || rs[i+2] != '<' {
			return 0, false, fmt.Errorf("`\\k` must name a group in unicode mode")
		}
		name, end, e := decodeGroupName(rs, i+3)
		if e != nil {
			return 0, false, fmt.Errorf("incomplete `\\k` group name in unicode mode")
		}
		if name == "" || !named[name] {
			return 0, false, fmt.Errorf("`\\k` references an undefined group name")
		}
		return end - i + 1, false, nil
	case n >= '1' && n <= '9':
		if inClass {
			return 2, false, nil // a decimal escape in a class is left to regexp2
		}
		j, val := i+1, 0
		for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
			val = val*10 + int(rs[j]-'0')
			j++
		}
		if val > groupCount {
			return 0, false, fmt.Errorf("backreference \\%d to a nonexistent group in unicode mode", val)
		}
		return j - i, false, nil
	default:
		return 0, false, fmt.Errorf("invalid identity escape `\\%c` in unicode mode", n)
	}
}

// validateModifierGroups checks ECMAScript inline modifier groups — `(?flags:…)`
// and `(?flags-flags:…)` from the Pattern Modifiers proposal — for the early
// errors the grammar imposes: every flag is one of i, m, s; no flag repeats
// within the add-set, within the remove-set, or across the two; a `:` must
// terminate the flag section; and the add and remove sets are not both empty.
// regexp2 (.NET inline-option syntax) is far more permissive here — it accepts
// `(?d:…)`, `(?ii:…)`, and the group-scoped `(?i)` form ECMAScript forbids — so
// these must be rejected before the pattern reaches it. Only a `(?` followed by
// an ASCII letter or `-` is a modifier group; `(?:`, `(?=`, `(?!`, `(?<…`,
// `(?>…`, `(?#…` are left for regexp2.
func validateModifierGroups(src string) error {
	rs := []rune(src)
	inClass := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' {
			i++ // skip the escaped rune
			continue
		}
		if c == '[' {
			inClass = true
			continue
		}
		if c == ']' {
			inClass = false
			continue
		}
		if inClass || c != '(' || i+2 >= len(rs) || rs[i+1] != '?' {
			continue
		}
		d := rs[i+2]
		isLetter := (d >= 'a' && d <= 'z') || (d >= 'A' && d <= 'Z')
		if !isLetter && d != '-' {
			continue // not a modifier group; let regexp2 parse it
		}
		add := map[rune]bool{}
		rem := map[rune]bool{}
		inRemove := false
		sawDash := false
		colon := false
		j := i + 2
		for ; j < len(rs); j++ {
			f := rs[j]
			if f == ':' {
				colon = true
				break
			}
			if f == '-' {
				if sawDash {
					return fmt.Errorf("invalid regular expression modifiers")
				}
				sawDash = true
				inRemove = true
				continue
			}
			if f != 'i' && f != 'm' && f != 's' {
				return fmt.Errorf("invalid regular expression modifier flag '%c'", f)
			}
			if add[f] || rem[f] {
				return fmt.Errorf("duplicate regular expression modifier flag '%c'", f)
			}
			if inRemove {
				rem[f] = true
			} else {
				add[f] = true
			}
		}
		if !colon {
			return fmt.Errorf("regular expression modifier group missing ':'")
		}
		if len(add) == 0 && len(rem) == 0 {
			return fmt.Errorf("regular expression modifier group has no flags")
		}
		i = j // resume scanning after the ':'
	}
	return nil
}

// validateQuantifiedAssertions rejects a quantifier applied to a lookaround
// assertion where ECMAScript forbids it: a lookbehind `(?<=…)` / `(?<!…)` is
// never quantifiable, and in Unicode (`u`/`v`) mode a lookahead `(?=…)` / `(?!…)`
// is not quantifiable either (in a non-Unicode pattern a quantified lookahead is
// permitted by Annex B). regexp2 accepts all of these, so they are caught here.
func validateQuantifiedAssertions(src string, unicode bool) error {
	rs := []rune(src)
	// assertKind per open paren: 0 = ordinary/other, 1 = lookbehind, 2 = lookahead.
	var stack []int
	inClass := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' {
			i++ // skip the escaped rune
			continue
		}
		if inClass {
			if c == ']' {
				inClass = false
			}
			continue
		}
		switch c {
		case '[':
			inClass = true
		case '(':
			kind := 0
			if i+2 < len(rs) && rs[i+1] == '?' {
				if rs[i+2] == '=' || rs[i+2] == '!' {
					kind = 2 // lookahead
				} else if rs[i+2] == '<' && i+3 < len(rs) && (rs[i+3] == '=' || rs[i+3] == '!') {
					kind = 1 // lookbehind
				}
			}
			stack = append(stack, kind)
		case ')':
			if len(stack) == 0 {
				continue // unbalanced; let regexp2 report it
			}
			kind := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if kind != 0 && quantifierFollows(rs, i+1) {
				if kind == 1 {
					return fmt.Errorf("lookbehind assertion is not quantifiable")
				}
				if unicode {
					return fmt.Errorf("lookahead assertion is not quantifiable in unicode mode")
				}
			}
		}
	}
	return nil
}

// quantifierFollows reports whether the token at rs[j] begins a quantifier: one
// of `*`, `+`, `?`, or a `{` that starts a `{n…}` bound.
func quantifierFollows(rs []rune, j int) bool {
	if j >= len(rs) {
		return false
	}
	switch rs[j] {
	case '*', '+', '?':
		return true
	case '{':
		return j+1 < len(rs) && rs[j+1] >= '0' && rs[j+1] <= '9'
	}
	return false
}

// decodeGroupName reads a group name starting at rs[start] up to the closing '>',
// decoding \u{HHHH} / \uHHHH escapes to their code points, and returns the decoded
// name and the index of the '>'.
func decodeGroupName(rs []rune, start int) (string, int, error) {
	var b strings.Builder
	i := start
	for i < len(rs) && rs[i] != '>' {
		if rs[i] == '\\' && i+1 < len(rs) && rs[i+1] == 'u' {
			cp, next, ok := decodeUnicodeEscape(rs, i+2)
			if !ok {
				return "", 0, fmt.Errorf("invalid \\u escape in group name")
			}
			// Combine a UTF-16 surrogate pair 𐀀 into its code point.
			if cp >= 0xD800 && cp <= 0xDBFF && next+1 < len(rs) && rs[next] == '\\' && rs[next+1] == 'u' {
				if lo, next2, ok2 := decodeUnicodeEscape(rs, next+2); ok2 && lo >= 0xDC00 && lo <= 0xDFFF {
					cp = 0x10000 + (cp-0xD800)*0x400 + (lo - 0xDC00)
					next = next2
				}
			}
			b.WriteRune(cp)
			i = next
			continue
		}
		b.WriteRune(rs[i])
		i++
	}
	if i >= len(rs) {
		return "", 0, fmt.Errorf("unterminated group name")
	}
	return b.String(), i, nil
}

// decodeUnicodeEscape reads a \uHHHH or \u{H…} body starting at rs[start] (just
// past the 'u'), returning the code point and the index just past the escape.
func decodeUnicodeEscape(rs []rune, start int) (rune, int, bool) {
	if start < len(rs) && rs[start] == '{' {
		j := start + 1
		var hex strings.Builder
		for j < len(rs) && rs[j] != '}' {
			hex.WriteRune(rs[j])
			j++
		}
		if j >= len(rs) || hex.Len() == 0 {
			return 0, 0, false
		}
		v, err := strconv.ParseInt(hex.String(), 16, 32)
		if err != nil || v > 0x10FFFF {
			return 0, 0, false
		}
		return rune(v), j + 1, true
	}
	if start+4 > len(rs) {
		return 0, 0, false
	}
	v, err := strconv.ParseInt(string(rs[start:start+4]), 16, 32)
	if err != nil {
		return 0, 0, false
	}
	return rune(v), start + 4, true
}
