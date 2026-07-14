package regexpjs

import (
	"fmt"
	"strconv"
	"strings"
)

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
