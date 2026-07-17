package engine

// Port of ant src/escape.c — string/template escape decoding. cookString turns
// a raw quoted string token into its cooked value; decodeEscape handles one
// backslash escape and returns the extra bytes consumed beyond the leading
// two-char `\X`.

func charAt(in string, i, end int) byte {
	if i < end {
		return in[i]
	}
	return 0
}

func appendRuneUTF8(out []byte, cp uint32) []byte {
	// WTF-8 so lone surrogates from \uXXXX escapes round-trip (not RuneError).
	return wtf8Encode(out, cp)
}

func decodeHexEscape(in string, pos int, out []byte) ([]byte, int) {
	cp := (hexVal(in[pos+2]) << 4) | hexVal(in[pos+3])
	return appendRuneUTF8(out, cp), 2
}

func decodeOctalEscape(in string, pos, end int, out []byte) ([]byte, int) {
	c := in[pos+1]
	extra := 0
	val := int(c - '0')
	if c2 := charAt(in, pos+2, end); c2 >= '0' && c2 <= '7' {
		val = val*8 + int(c2-'0')
		extra++
		if c3 := charAt(in, pos+3, end); c3 >= '0' && c3 <= '7' && val*8+int(c3-'0') <= 255 {
			val = val*8 + int(c3-'0')
			extra++
		}
	}
	return appendRuneUTF8(out, uint32(val)), extra
}

func decodeUnicodeBraced(in string, pos, end int, out []byte) ([]byte, int) {
	var cp uint32
	i := pos + 3
	for i < end && isXDigitByte(in[i]) {
		cp = (cp << 4) | hexVal(in[i])
		i++
	}
	if i < end && in[i] == '}' {
		return appendRuneUTF8(out, cp), i - pos - 1
	}
	return append(out, 'u'), 0
}

func decodeUnicodeFixed(in string, pos, end int, out []byte) ([]byte, int) {
	cp := (hexVal(in[pos+2]) << 12) | (hexVal(in[pos+3]) << 8) |
		(hexVal(in[pos+4]) << 4) | hexVal(in[pos+5])

	if cp >= 0xD800 && cp <= 0xDBFF && pos+11 < end &&
		in[pos+6] == '\\' && in[pos+7] == 'u' &&
		isXDigitByte(in[pos+8]) && isXDigitByte(in[pos+9]) &&
		isXDigitByte(in[pos+10]) && isXDigitByte(in[pos+11]) {
		lo := (hexVal(in[pos+8]) << 12) | (hexVal(in[pos+9]) << 8) |
			(hexVal(in[pos+10]) << 4) | hexVal(in[pos+11])
		if lo >= 0xDC00 && lo <= 0xDFFF {
			cp = 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00)
			return appendRuneUTF8(out, cp), 10
		}
	}
	return appendRuneUTF8(out, cp), 4
}

// decodeEscape decodes the escape at in[pos] ('\'), appending to out. Returns
// the updated slice and the extra byte count consumed beyond `\X`.
func decodeEscape(in string, pos, end int, out []byte, quote byte) ([]byte, int) {
	c := in[pos+1]
	// LineContinuation: a backslash immediately followed by a LineTerminator
	// sequence (LF, CR, CR LF, and the UTF-8 encodings of LS U+2028 / PS U+2029)
	// evaluates to the empty String.
	switch {
	case c == '\n':
		return out, 0
	case c == '\r':
		if charAt(in, pos+2, end) == '\n' {
			return out, 1
		}
		return out, 0
	case c == 0xE2 && charAt(in, pos+2, end) == 0x80 &&
		(charAt(in, pos+3, end) == 0xA8 || charAt(in, pos+3, end) == 0xA9):
		return out, 2
	}
	switch c {
	case 'n':
		return append(out, '\n'), 0
	case 't':
		return append(out, '\t'), 0
	case 'r':
		return append(out, '\r'), 0
	case 'v':
		return append(out, '\v'), 0
	case 'f':
		return append(out, '\f'), 0
	case 'b':
		return append(out, '\b'), 0
	case '\\':
		return append(out, '\\'), 0
	case '0':
		if c2 := charAt(in, pos+2, end); !(c2 >= '0' && c2 <= '7') {
			return append(out, 0), 0
		}
		return decodeOctalEscape(in, pos, end, out)
	case '1', '2', '3', '4', '5', '6', '7':
		return decodeOctalEscape(in, pos, end, out)
	case 'x':
		if pos+3 < end && isXDigitByte(in[pos+2]) && isXDigitByte(in[pos+3]) {
			return decodeHexEscape(in, pos, out)
		}
		return append(out, c), 0
	case 'u':
		if pos+2 < end && in[pos+2] == '{' {
			return decodeUnicodeBraced(in, pos, end, out)
		}
		if pos+5 < end && isXDigitByte(in[pos+2]) && isXDigitByte(in[pos+3]) &&
			isXDigitByte(in[pos+4]) && isXDigitByte(in[pos+5]) {
			return decodeUnicodeFixed(in, pos, end, out)
		}
		return append(out, c), 0
	default:
		if c == quote {
			return append(out, quote), 0
		}
		return append(out, c), 0
	}
}

// cookString decodes a raw quoted string token (including its surrounding
// quotes) into its cooked value (ant sv_lexer_str_literal).
func cookString(tok string) string {
	tlen := len(tok)
	if tlen == 0 {
		return ""
	}
	quote := tok[0]
	out := make([]byte, 0, tlen)
	n2 := 0
	for {
		old := n2
		n2++
		if !(old+2 < tlen) {
			break
		}
		if tok[n2] == '\\' {
			var extra int
			out, extra = decodeEscape(tok, n2, tlen, out, quote)
			n2 += extra + 1
		} else {
			out = append(out, tok[n2])
		}
	}
	return string(out)
}

// cookTemplateSegment decodes a template cooked segment over in[start:end]
// (ant decode_template_segment). Returns the cooked string and whether it is a
// valid cooked value (invalid → undefined cooked, e.g. bad \u or octal).
// normalizeCRLF converts each LineTerminatorSequence (a CRLF pair or a lone CR)
// to a single LF, as a template's raw value (TRV) requires. (The cooked value TV
// is normalized inline in cookTemplateSegment.) A `\r` escape sequence — the two
// bytes '\\','r' — is unaffected; only an actual CR byte (0x0D) is a terminator.
func normalizeCRLF(s string) string {
	cr := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' {
			cr = i
			break
		}
	}
	if cr < 0 {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' {
			out = append(out, '\n')
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

func cookTemplateSegment(in string, start, end int) (string, bool) {
	if end <= start {
		return "", true
	}
	out := make([]byte, 0, end-start)
	for i := start; i < end; i++ {
		if in[i] == '\r' {
			out = append(out, '\n')
			if i+1 < end && in[i+1] == '\n' {
				i++
			}
			continue
		}
		if in[i] != '\\' || i+1 >= end {
			out = append(out, in[i])
			continue
		}
		c := in[i+1]
		if c >= '1' && c <= '9' {
			return "", false
		}
		if c == '0' && i+2 < end && in[i+2] >= '0' && in[i+2] <= '9' {
			return "", false
		}
		if c == 'x' && !(i+3 < end && isXDigitByte(in[i+2]) && isXDigitByte(in[i+3])) {
			return "", false
		}
		if c == 'u' {
			if i+2 < end && in[i+2] == '{' {
				var cp uint32
				j := i + 3
				for j < end && isXDigitByte(in[j]) {
					cp = (cp << 4) | hexVal(in[j])
					j++
				}
				if !(j < end && in[j] == '}' && j > i+3 && cp <= 0x10FFFF) {
					return "", false
				}
			} else if !(i+5 < end && isXDigitByte(in[i+2]) && isXDigitByte(in[i+3]) &&
				isXDigitByte(in[i+4]) && isXDigitByte(in[i+5])) {
				return "", false
			}
		}
		var extra int
		out, extra = decodeEscape(in, i, end, out, '`')
		i += 1 + extra
	}
	return string(out), true
}
