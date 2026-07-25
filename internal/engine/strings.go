package engine

// Port of ant src/utf8.c (UTF-16-over-WTF-8 machinery) + flat-string storage.
//
// JS strings are sequences of UTF-16 code units, but ant stores them as WTF-8
// (UTF-8 extended to encode lone surrogates), which keeps every string builtin
// byte-oriented. The bridge is the code-unit ↔ byte-offset mapping below: each
// WTF-8 sequence contributes 1 UTF-16 unit, except a 4-byte (astral) sequence
// which contributes a surrogate *pair* (2 units).
//
// ant's thread-local scan cursor cache is a performance optimization; this port
// starts with correct linear scans and can add caching in a later perf pass.

import "unicode/utf8"

// ---- flat string storage ----

// string payload low-2-bit tag packing: Value data = (handle << 2) | tag.
func mkFlatStr(h Handle) Value { return mkval(TStr, uint64(h)<<2|strHeapTagFlat) }
func strHandle(v Value) Handle { return Handle(v.Data() >> 2) }
func strTagOf(v Value) uint64  { return v.Data() & strHeapTagMask }

const (
	strAsciiUnknown = 0
	strAsciiYes     = 1
	strAsciiNo      = 2
)

// newStringBytes creates a flat string from raw WTF-8 bytes.
func (rt *Runtime) newStringBytes(b []byte) Value {
	h, fs := rt.strings.alloc()
	fs.bytes = b
	fs.isASCII = strAsciiUnknown
	return mkFlatStr(h)
}

// newString creates a flat string from a Go (UTF-8) string.
func (rt *Runtime) newString(s string) Value {
	return rt.newStringBytes([]byte(s))
}

// flatOf returns the flat-string payload for a T_STR flat Value (nil otherwise).
func (rt *Runtime) flatOf(v Value) *flatString {
	if v.Type() != TStr || strTagOf(v) != strHeapTagFlat {
		return nil
	}
	return rt.strings.get(strHandle(v))
}

// strBytes returns the WTF-8 bytes of a flat string.
func (rt *Runtime) strBytes(v Value) []byte {
	if fs := rt.flatOf(v); fs != nil {
		return fs.bytes
	}
	return nil
}

// strIsASCII reports whether the flat string is pure ASCII (tri-state cached).
func (rt *Runtime) strIsASCII(v Value) bool {
	fs := rt.flatOf(v)
	if fs == nil {
		return false
	}
	if fs.isASCII == strAsciiUnknown {
		fs.isASCII = strAsciiYes
		for _, b := range fs.bytes {
			if b >= 0x80 {
				fs.isASCII = strAsciiNo
				break
			}
		}
	}
	return fs.isASCII == strAsciiYes
}

// internString returns a canonical flat string for s (ant intern_string).
func (rt *Runtime) internString(s string) Value {
	if h, ok := rt.interned[s]; ok {
		return mkFlatStr(h)
	}
	hv := rt.newString(s)
	rt.interned[s] = strHandle(hv)
	return hv
}

// ---- WTF-8 / UTF-16 mapping (ant utf8.c) ----

// wtf8Decode reports the WTF-8 byte length, UTF-16 unit count, and code point
// of the sequence at b[i:] (ant utf16_scan_decode). Truncated sequences decode
// as a single byte, matching ant.
func wtf8Decode(b []byte, i int) (slen, units int, cp uint32) {
	c := b[i]
	if c < 0x80 {
		return 1, 1, uint32(c)
	}
	switch {
	case c&0xE0 == 0xC0:
		if i+1 < len(b) {
			return 2, 1, uint32(c&0x1F)<<6 | uint32(b[i+1]&0x3F)
		}
	case c&0xF0 == 0xE0:
		if i+2 < len(b) {
			return 3, 1, uint32(c&0x0F)<<12 | uint32(b[i+1]&0x3F)<<6 | uint32(b[i+2]&0x3F)
		}
	case c&0xF8 == 0xF0:
		if i+3 < len(b) {
			return 4, 2, uint32(c&0x07)<<18 | uint32(b[i+1]&0x3F)<<12 | uint32(b[i+2]&0x3F)<<6 | uint32(b[i+3]&0x3F)
		}
	}
	return 1, 1, uint32(c)
}

// wtf8ToRunes decodes a WTF-8 string into its code points, preserving lone
// surrogates as runes in [0xD800, 0xDFFF]. Unlike []rune(string(b)), which
// replaces the invalid UTF-8 of a lone surrogate with U+FFFD (and splits it into
// several runes), this yields exactly one rune per code point — which the RegExp
// engine needs so `u`-mode patterns match lone surrogates as their own code
// points (ES treats each as a distinct character).
func wtf8ToRunes(b []byte) []rune {
	if isASCIIBytes(b) {
		runes := make([]rune, len(b))
		for i, c := range b {
			runes[i] = rune(c)
		}
		return runes
	}
	runes := make([]rune, 0, len(b))
	for i := 0; i < len(b); {
		slen, _, cp := wtf8Decode(b, i)
		if slen <= 0 {
			slen = 1
		}
		runes = append(runes, rune(cp))
		i += slen
	}
	return runes
}

// wtf8ToUTF16Runes decodes a WTF-8 string into one rune per UTF-16 code unit,
// splitting astral code points into their surrogate pair. ECMAScript indexes
// strings in code units and a non-`u` regexp matches them one code unit at a
// time, so this — not wtf8ToRunes — is the domain the RegExp engine works in.
func wtf8ToUTF16Runes(b []byte) []rune {
	if isASCIIBytes(b) {
		runes := make([]rune, len(b))
		for i, c := range b {
			runes[i] = rune(c)
		}
		return runes
	}
	runes := make([]rune, 0, len(b))
	for i := 0; i < len(b); {
		slen, units, cp := wtf8Decode(b, i)
		if slen <= 0 {
			slen = 1
		}
		if units == 2 {
			c := cp - 0x10000
			runes = append(runes, rune(0xD800+(c>>10)), rune(0xDC00+c&0x3FF))
		} else {
			runes = append(runes, rune(cp))
		}
		i += slen
	}
	return runes
}

// utf16RunesToString re-encodes a UTF-16 code-unit rune slice (as produced by
// wtf8ToUTF16Runes) as WTF-8: surrogate pairs recombine into astral code points
// and unpaired surrogates survive as themselves. Plain string(rs) cannot be used
// because Go maps every surrogate rune to U+FFFD.
func utf16RunesToString(rs []rune) string {
	ascii := true
	for _, r := range rs {
		if r >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		b := make([]byte, len(rs))
		for i, r := range rs {
			b[i] = byte(r)
		}
		return string(b)
	}
	out := make([]byte, 0, len(rs)*3)
	for i := 0; i < len(rs); i++ {
		cp := uint32(rs[i])
		if cp >= 0xD800 && cp <= 0xDBFF && i+1 < len(rs) && rs[i+1] >= 0xDC00 && rs[i+1] <= 0xDFFF {
			cp = 0x10000 + (cp-0xD800)<<10 + uint32(rs[i+1]) - 0xDC00
			i++
		}
		out = wtf8Encode(out, cp)
	}
	return string(out)
}

func isASCIIBytes(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

// utf16Len returns the UTF-16 code-unit length of a WTF-8 string (ant utf16_strlen).
func utf16Len(b []byte) int {
	if isASCIIBytes(b) {
		return len(b)
	}
	n := 0
	for i := 0; i < len(b); {
		slen, units, _ := wtf8Decode(b, i)
		n += units
		i += slen
	}
	return n
}

// utf16CodeUnitAt returns the UTF-16 code unit at index (0xFFFFFFFF if out of
// range) (ant utf16_code_unit_at).
func utf16CodeUnitAt(b []byte, idx int) uint32 {
	if isASCIIBytes(b) {
		if idx < 0 || idx >= len(b) {
			return 0xFFFFFFFF
		}
		return uint32(b[idx])
	}
	pos := 0
	for i := 0; i < len(b); {
		slen, units, cp := wtf8Decode(b, i)
		if pos == idx {
			if units == 2 {
				return 0xD800 + ((cp - 0x10000) >> 10)
			}
			return cp
		}
		if units == 2 && pos+1 == idx {
			return 0xDC00 + ((cp - 0x10000) & 0x3FF)
		}
		i += slen
		pos += units
	}
	return 0xFFFFFFFF
}

// utf16CodepointAt returns the full code point at a UTF-16 index (combining a
// surrogate pair when idx starts one) (ant utf16_codepoint_at).
func utf16CodepointAt(b []byte, idx int) uint32 {
	if isASCIIBytes(b) {
		if idx < 0 || idx >= len(b) {
			return 0xFFFFFFFF
		}
		return uint32(b[idx])
	}
	pos := 0
	for i := 0; i < len(b); {
		slen, units, cp := wtf8Decode(b, i)
		if pos == idx {
			return cp
		}
		if units == 2 && pos+1 == idx {
			return 0xDC00 + ((cp - 0x10000) & 0x3FF)
		}
		i += slen
		pos += units
	}
	return 0xFFFFFFFF
}

// utf16IndexToByteOffset maps a UTF-16 index to a WTF-8 byte offset, also
// returning the byte length of the char there (ant utf16_index_to_byte_offset).
// ok=false when idx is past the end.
func utf16IndexToByteOffset(b []byte, idx int) (off, charBytes int, ok bool) {
	if isASCIIBytes(b) {
		if idx > len(b) {
			return 0, 0, false
		}
		cb := 0
		if idx < len(b) {
			cb = 1
		}
		return idx, cb, true
	}
	pos := 0
	i := 0
	for i < len(b) && pos < idx {
		slen, units, _ := wtf8Decode(b, i)
		i += slen
		pos += units
	}
	if i >= len(b) {
		if pos == idx {
			return len(b), 0, true
		}
		return 0, 0, false
	}
	slen, _, _ := wtf8Decode(b, i)
	return i, slen, true
}

// byteOffsetToUtf16 maps a WTF-8 byte offset to a UTF-16 index
// (ant byte_offset_to_utf16).
func byteOffsetToUtf16(b []byte, byteOff int) int {
	if byteOff > len(b) {
		byteOff = len(b)
	}
	if isASCIIBytes(b[:byteOff]) {
		return byteOff
	}
	pos := 0
	for i := 0; i < byteOff; {
		slen, units, _ := wtf8Decode(b, i)
		if i+slen > byteOff {
			break
		}
		i += slen
		pos += units
	}
	return pos
}

// concatWTF8 joins two WTF-8 strings. When the first ends with a lone HIGH
// surrogate and the second begins with a lone LOW surrogate, the pair at the
// seam becomes the single code point it denotes. That is the concatenation rule
// WTF-8 requires, and it is what makes `'\uD83D' + '\uDCA9'` equal to '💩'
// rather than a distinct string of the same two code units.
func concatWTF8(a, b []byte) []byte {
	if len(a) >= 3 && len(b) >= 3 {
		ha, lo3 := a[len(a)-3:], b[:3]
		// A surrogate is ED A0-AF (high) / ED B0-BF (low) in WTF-8. 0xED can only
		// be a leader byte, so finding it three bytes from the end is unambiguous.
		if ha[0] == 0xED && ha[1] >= 0xA0 && ha[1] <= 0xAF &&
			lo3[0] == 0xED && lo3[1] >= 0xB0 && lo3[1] <= 0xBF {
			hi := 0xD000 | uint32(ha[1]&0x3F)<<6 | uint32(ha[2]&0x3F)
			lo := 0xD000 | uint32(lo3[1]&0x3F)<<6 | uint32(lo3[2]&0x3F)
			cp := 0x10000 + (hi-0xD800)<<10 + (lo - 0xDC00)
			out := make([]byte, 0, len(a)+len(b)-2)
			out = append(out, a[:len(a)-3]...)
			out = wtf8Encode(out, cp)
			return append(out, b[3:]...)
		}
	}
	out := make([]byte, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}

// substringUnits returns the bytes of the [start,end) UTF-16 code-unit range of
// a WTF-8 string. A boundary that falls INSIDE a surrogate pair splits it: the
// half that stays is emitted as a lone surrogate. That is the case a byte-range
// slice cannot express, since the pair's 4-byte form has no interior boundary,
// and it is why `"\u{1F306}".slice(1)` is a string of one code unit rather than
// the empty string.
func substringUnits(b []byte, start, end int) []byte {
	if start < 0 {
		start = 0
	}
	if end <= start {
		return nil
	}
	if isASCIIBytes(b) {
		bs, be := min(start, len(b)), min(end, len(b))
		return b[bs:be]
	}
	out := make([]byte, 0, end-start)
	pos, i := 0, 0
	for i < len(b) && pos < end {
		slen, units, cp := wtf8Decode(b, i)
		next := pos + units
		switch {
		case next <= start: // entirely before the range
		case units == 1 || (pos >= start && next <= end):
			out = append(out, b[i:i+slen]...)
		default:
			// An astral code point straddling a boundary: emit the surrogate half
			// that lies inside the range.
			hi := uint32(0xD800 + ((cp - 0x10000) >> 10))
			lo := uint32(0xDC00 + ((cp - 0x10000) & 0x3FF))
			if pos >= start && pos < end {
				out = wtf8Encode(out, hi)
			}
			if pos+1 >= start && pos+1 < end {
				out = wtf8Encode(out, lo)
			}
		}
		i += slen
		pos = next
	}
	return out
}

// utf16RangeToByteRange maps a UTF-16 [start,end) range to a byte range
// (ant utf16_range_to_byte_range). Indices are assumed clamped by the caller.
func utf16RangeToByteRange(b []byte, start, end int) (bStart, bEnd int) {
	if isASCIIBytes(b) {
		bStart = min(start, len(b))
		bEnd = min(end, len(b))
		return
	}
	bStart, bEnd = len(b), len(b)
	foundStart, foundEnd := false, false
	pos, i := 0, 0
	for i < len(b) {
		if pos == start {
			bStart = i
			foundStart = true
		}
		if pos == end {
			bEnd = i
			foundEnd = true
			break
		}
		slen, units, _ := wtf8Decode(b, i)
		i += slen
		pos += units
	}
	if !foundStart && start >= pos {
		bStart = len(b)
	}
	if !foundEnd && end >= pos {
		bEnd = len(b)
	}
	return
}

// wtf8Encode appends the WTF-8 encoding of a code point to out. Unlike Go's
// utf8.AppendRune, lone surrogates (U+D800..U+DFFF) are encoded as their raw
// 3-byte form rather than replaced with U+FFFD.
func wtf8Encode(out []byte, cp uint32) []byte {
	if cp >= 0xD800 && cp <= 0xDFFF {
		return append(out,
			byte(0xE0|cp>>12),
			byte(0x80|(cp>>6)&0x3F),
			byte(0x80|cp&0x3F))
	}
	return utf8.AppendRune(out, rune(cp))
}

// utf16ToWTF8 encodes a sequence of UTF-16 code units to WTF-8, combining
// surrogate pairs into astral code points (used by String.fromCharCode etc.).
func utf16ToWTF8(units []uint16) []byte {
	out := make([]byte, 0, len(units))
	for i := 0; i < len(units); i++ {
		u := uint32(units[i])
		if u >= 0xD800 && u <= 0xDBFF && i+1 < len(units) {
			lo := uint32(units[i+1])
			if lo >= 0xDC00 && lo <= 0xDFFF {
				cp := 0x10000 + ((u - 0xD800) << 10) + (lo - 0xDC00)
				out = utf8.AppendRune(out, rune(cp))
				i++
				continue
			}
		}
		out = wtf8Encode(out, u)
	}
	return out
}
