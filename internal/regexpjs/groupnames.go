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
func translateGroupNames(src string) (string, map[string]string, error) {
	rs := []rune(src)
	var out strings.Builder
	names := map[string]string{}   // decoded original name -> internal name
	reverse := map[string]string{} // internal name -> decoded original name
	inClass := false
	counter := 0

	// nextInternal assigns/reuses an internal name for a decoded original name.
	// Duplicate original names each get a distinct internal name (same original in
	// the reverse map) so regexp2 sees unique names.
	nextInternal := func(orig string, isDef bool) (string, bool) {
		if !isDef {
			if in, ok := names[orig]; ok {
				return in, true
			}
			return "", false // reference to an undefined group name
		}
		in := "__g" + strconv.Itoa(counter)
		counter++
		names[orig] = in
		reverse[in] = orig
		return in, true
	}

	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' {
			// A named backreference \k<name>.
			if !inClass && i+1 < len(rs) && rs[i+1] == 'k' && i+2 < len(rs) && rs[i+2] == '<' {
				name, end, err := decodeGroupName(rs, i+3)
				if err != nil {
					return "", nil, err
				}
				in, ok := nextInternal(name, false)
				if !ok {
					return "", nil, fmt.Errorf("invalid named backreference to <%s>", name)
				}
				out.WriteString("\\k<")
				out.WriteString(in)
				out.WriteByte('>')
				i = end
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
		// A named group definition (?<name>…), but not a lookbehind (?<= / (?<!.
		if !inClass && c == '(' && i+2 < len(rs) && rs[i+1] == '?' && rs[i+2] == '<' &&
			!(i+3 < len(rs) && (rs[i+3] == '=' || rs[i+3] == '!')) {
			name, end, err := decodeGroupName(rs, i+3)
			if err != nil {
				return "", nil, err
			}
			in, _ := nextInternal(name, true)
			out.WriteString("(?<")
			out.WriteString(in)
			out.WriteByte('>')
			i = end
			continue
		}
		out.WriteRune(c)
	}
	if counter == 0 {
		return src, nil, nil // no named groups; leave the source untouched
	}
	return out.String(), reverse, nil
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
