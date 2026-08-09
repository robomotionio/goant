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

// newStringBytes creates a flat string from raw WTF-8 bytes, charging the
// collector for them. Every caller but one hands over a buffer it has just
// allocated; the exception is the concat fast path, which appends into a buffer
// already charged for and so builds its cell with newStringBytesRaw instead.
//
// The limit test is inline and the charge is the call behind it, rather than
// letting chargeBytes do both: this is on the path of every string the engine
// creates, so a run with no limit set pays one compare and never a call. The
// body is duplicated in newStringBytesRaw for the same reason — delegating cost
// measurable time on string-building code that had set no limit at all.
//
// Note it must go through chargeBytes and not just add to allocBytes. Charging
// is what lowers gc.next when the byte budget runs out, and that is the only
// thing that brings a string-heavy script to a collection; incrementing the
// counter alone leaves it growing forever with nothing ever testing the limit.
//
// The strNext test is how a host that set NO limit is watched. Both collection
// triggers count object cells, so before it a script allocating nothing but
// strings grew with nothing looking at it at all. One load — liveN is what alloc
// just incremented — and a branch that is not taken; the policy is in
// Runtime.stringsFull.
func (rt *Runtime) newStringBytes(b []byte) Value {
	if rt.heapLimit != 0 {
		rt.chargeBytes(uint64(cap(b)))
	}
	h, fs := rt.strings.alloc()
	if rt.strings.liveN >= rt.gc.strNext {
		rt.stringsFull()
	}
	fs.bytes = b
	fs.gostr = "" // the cell may be recycled; drop the previous string's cache
	fs.isASCII = strAsciiUnknown
	return mkFlatStr(h)
}

// newStringBytesRaw is newStringBytes without the charge, for a caller that has
// accounted for the bytes itself.
func (rt *Runtime) newStringBytesRaw(b []byte) Value {
	h, fs := rt.strings.alloc()
	if rt.strings.liveN >= rt.gc.strNext {
		rt.stringsFull()
	}
	fs.bytes = b
	fs.gostr = ""
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

// strGo returns the bytes of a flat string as a Go string, caching the
// conversion on the string itself.
//
// Converting with string(rt.strBytes(v)) copies the bytes on every call. That is paid
// per property access — the interpreter turns a name constant into a Go string
// before every GET_FIELD/PUT_FIELD — and the names in question are interned, so
// the same handful of strings were being rebuilt millions of times. Caching
// makes it once per string ever.
//
// Safe because a flat string's bytes are written once, in newStringBytes, and
// never mutated afterwards; that function also clears the cache, since pool
// cells are recycled.
func (rt *Runtime) strGo(v Value) string {
	fs := rt.flatOf(v)
	if fs == nil {
		return ""
	}
	if fs.gostr == "" && len(fs.bytes) > 0 {
		fs.gostr = string(fs.bytes)
	}
	return fs.gostr
}

// strUTF16 returns the string as UTF-16 code units, cached on the flat string.
//
// Regular expressions match over code units, so every exec, replace, split and
// match converted its subject afresh. A subject is usually matched many times
// — Octane's RegExp benchmark runs a fixed set of patterns over a fixed set of
// strings — and the conversion is proportional to the subject each time.
//
// The result is shared, so it MUST NOT be mutated. That is safe by
// construction: JavaScript strings are immutable, and the matcher only reads.
func (rt *Runtime) strUTF16(v Value) []rune {
	fs := rt.flatOf(v)
	if fs == nil {
		return nil
	}
	return flatUnits(fs)
}

// strIsASCII reports whether the flat string is pure ASCII (tri-state cached).
func (rt *Runtime) strIsASCII(v Value) bool {
	fs := rt.flatOf(v)
	if fs == nil {
		return false
	}
	return flatIsASCII(fs)
}

// flatIsASCII is strIsASCII for a caller that already has the cell, which the
// indexing helpers below all do — resolving the handle twice for one answer is
// most of what those helpers cost.
func flatIsASCII(fs *flatString) bool {
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

// ---- indexing a string, in constant time ----
//
// The three below are the whole reason the caches above exist. A JS string is
// indexed in UTF-16 code units and stored here in WTF-8, so `s.length` and
// `s.charCodeAt(i)` are both, taken literally, a decode of the string from its
// first byte — which makes the ordinary way to walk a string quadratic. Octane's
// gbemu decodes a ROM held in a non-ASCII string exactly that way, and spent 94%
// of its time here.
//
// The answer is not a faster scan. An ASCII string needs no scan at all, because
// its code units are its bytes; a string that is not ASCII gets decoded once
// into the same code-unit array the RegExp engine already caches, and every
// index after that is a load. What remains proportional to the string is the
// first access to it, and nothing else.

// strLen16 is the UTF-16 length of a string value.
func (rt *Runtime) strLen16(v Value) int {
	fs := rt.flatOf(v)
	if fs == nil {
		return utf16Len(rt.strBytes(v))
	}
	if fs.len16 != 0 {
		return int(fs.len16)
	}
	n := len(fs.bytes)
	if !flatIsASCII(fs) {
		n = utf16Len(fs.bytes)
	}
	if n <= 1<<31-1 {
		fs.len16 = int32(n)
	}
	return n
}

// flatUnits returns the cached code-unit view of a string that is not ASCII,
// decoding it on first use. Shared with strUTF16 and, like it, MUST NOT be
// mutated.
func flatUnits(fs *flatString) []rune {
	if fs.utf16 == nil && len(fs.bytes) > 0 {
		fs.utf16 = wtf8ToUTF16Runes(fs.bytes)
	}
	return fs.utf16
}

// strUnitAt is utf16CodeUnitAt over a string value (0xFFFFFFFF out of range).
func (rt *Runtime) strUnitAt(v Value, idx int) uint32 {
	fs := rt.flatOf(v)
	if fs == nil {
		return utf16CodeUnitAt(rt.strBytes(v), idx)
	}
	if flatIsASCII(fs) {
		if idx < 0 || idx >= len(fs.bytes) {
			return 0xFFFFFFFF
		}
		return uint32(fs.bytes[idx])
	}
	u := flatUnits(fs)
	if idx < 0 || idx >= len(u) {
		return 0xFFFFFFFF
	}
	return uint32(u[idx])
}

// strCodepointAt is utf16CodepointAt over a string value: the code unit at idx,
// or the whole code point when idx begins a surrogate pair.
func (rt *Runtime) strCodepointAt(v Value, idx int) uint32 {
	fs := rt.flatOf(v)
	if fs == nil {
		return utf16CodepointAt(rt.strBytes(v), idx)
	}
	if flatIsASCII(fs) {
		if idx < 0 || idx >= len(fs.bytes) {
			return 0xFFFFFFFF
		}
		return uint32(fs.bytes[idx])
	}
	u := flatUnits(fs)
	if idx < 0 || idx >= len(u) {
		return 0xFFFFFFFF
	}
	hi := uint32(u[idx])
	if hi >= 0xD800 && hi <= 0xDBFF && idx+1 < len(u) {
		if lo := uint32(u[idx+1]); lo >= 0xDC00 && lo <= 0xDFFF {
			return 0x10000 + (hi-0xD800)<<10 + (lo - 0xDC00)
		}
	}
	return hi
}

// strCharAt is the one-code-unit string at idx, the `s[i]` and s.charAt(i)
// result. The single-unit strings are interned: a loop over a string produces
// the same handful of them over and over, and allocating a fresh cell per
// character made walking a string cost a garbage collection.
func (rt *Runtime) strCharAt(v Value, idx int) Value {
	cu := rt.strUnitAt(v, idx)
	if cu < 0x80 {
		return rt.internString(string([]byte{byte(cu)}))
	}
	return rt.newStringBytes(utf16ToWTF8([]uint16{uint16(cu)}))
}

// internString returns a canonical flat string for s (ant intern_string).
func (rt *Runtime) internString(s string) Value {
	if h, ok := rt.interned[s]; ok {
		return mkFlatStr(h)
	}
	hv := rt.newString(s)
	rt.interned[s] = strHandle(hv)
	if rt.invWatermark != 0 {
		// The intern table outlives the invocation, so an entry added during one
		// would point at a freed cell after a release. Recorded so it can be
		// rolled back; see Invocation.Release.
		rt.invInterned = append(rt.invInterned, s)
	}
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

// strSubstring returns the bytes of the [start,end) code-unit range of a string
// value.
//
// Whether the subject is ASCII is read from the flat string's cache rather than
// worked out per call, because working it out is a scan of the whole subject —
// which would make a substring cost the length of the string it came out of
// instead of its own. Octane's typescript spends its run slicing one
// half-megabyte source text, and 82% of the benchmark was that scan.
func (rt *Runtime) strSubstring(sv Value, start, end int) []byte {
	fs := rt.flatOf(sv)
	if fs == nil {
		return substringUnits(rt.strBytes(sv), start, end, false)
	}
	return substringUnits(fs.bytes, start, end, flatIsASCII(fs))
}

// substringUnits returns the bytes of the [start,end) UTF-16 code-unit range of
// a WTF-8 string. A boundary that falls INSIDE a surrogate pair splits it: the
// half that stays is emitted as a lone surrogate. That is the case a byte-range
// slice cannot express, since the pair's 4-byte form has no interior boundary,
// and it is why `"\u{1F306}".slice(1)` is a string of one code unit rather than
// the empty string.
func substringUnits(b []byte, start, end int, ascii bool) []byte {
	if start < 0 {
		start = 0
	}
	if end <= start {
		return nil
	}
	if ascii {
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

// concatGrowThreshold is the result size at which a concatenation starts
// over-allocating and claiming the spare capacity. Below it the result is built
// exactly, because a short concatenation is almost always a one-off.
const concatGrowThreshold = 64

// concatStrings implements the string case of `+`.
//
// The obvious implementation — allocate len(a)+len(b) and copy both — makes the
// commonest string idiom in JavaScript quadratic:
//
//	let s = ""; for (…) s += chunk;
//
// Every iteration copies everything accumulated so far, so building an n-byte
// string costs O(n^2). goant took 854ms for 80k single-character appends and
// died at 110k having allocated 2.7GB; node does the same loop in 5ms because
// V8 builds a cons string and flattens lazily.
//
// Ropes would mean a second string representation and a flatten check in every
// consumer of string bytes. This gets the same asymptotics without one: a
// concatenation over-allocates, and the string it produces owns the leftover
// capacity. The next concatenation whose LEFT side is that owner appends in
// place and hands ownership on.
//
// Immutability survives because appending only ever writes ABOVE the owner's
// length. Every existing string keeps its own shorter slice of the same array,
// and the bytes below its length never change. Ownership is what makes it safe
// to write there at all: it passes to the result and is cleared on the source,
// so two different strings can never both extend from the same offset. A stale
// reference simply takes the copying path.
func (rt *Runtime) concatStrings(sa, sb Value) Value {
	fa, fb := rt.flatOf(sa), rt.flatOf(sb)
	if fa == nil || fb == nil {
		return rt.newStringBytes(concatWTF8(rt.strBytes(sa), rt.strBytes(sb)))
	}
	a, b := fa.bytes, fb.bytes
	if len(a) == 0 {
		return sb
	}
	if len(b) == 0 {
		return sa
	}
	// A high surrogate meeting a low surrogate has to be re-encoded as one code
	// point, which rewrites the tail of `a` rather than appending after it. Rare,
	// and the generic path already handles it.
	if joinsSurrogatePair(a, b) {
		return rt.newStringBytes(concatWTF8(a, b))
	}

	if fa.extendable && cap(a)-len(a) >= len(b) {
		fa.extendable = false
		// Same backing array: within capacity, append cannot reallocate. Only
		// len(b) is new memory — charging cap would re-charge the whole
		// accumulator on every append and make an O(n) loop trigger O(n^2)
		// bytes' worth of collections.
		rt.chargeBytes(uint64(len(b)))
		v := rt.newStringBytesRaw(append(a, b...))
		if fs := rt.flatOf(v); fs != nil {
			fs.extendable = true
		}
		return v
	}

	n := len(a) + len(b)
	// Below the threshold, allocate exactly and do not claim ownership. Most
	// concatenations are one-offs — a message, a key, a path — and doubling for
	// those is pure waste: it cost 4% on the string microbenchmark for no gain,
	// because nothing ever appends to the result.
	//
	// An accumulator passes the threshold within its first few iterations and
	// from then on doubles, which is what makes the loop amortised linear. The
	// copying done below the threshold is bounded by the threshold itself.
	if n < concatGrowThreshold {
		return rt.newStringBytes(concatWTF8(a, b))
	}
	// The doubling below is the allocation most likely to be the one that does
	// not fit. Ask before taking it; on refusal the script is already stopping,
	// so returning the left operand only supplies something type-correct for
	// the unwind to discard.
	if !rt.reserveBytes(uint64(2 * n)) {
		return sa
	}
	buf := make([]byte, 0, 2*n)
	buf = append(append(buf, a...), b...)
	return rt.newExtendableString(buf)
}

// joinsSurrogatePair reports whether a's last code unit and b's first form a
// surrogate pair, which concatWTF8 must merge into a single code point.
func joinsSurrogatePair(a, b []byte) bool {
	if len(a) < 3 || len(b) < 3 {
		return false
	}
	ha, lo3 := a[len(a)-3:], b[:3]
	return ha[0] == 0xED && ha[1] >= 0xA0 && ha[1] <= 0xAF &&
		lo3[0] == 0xED && lo3[1] >= 0xB0 && lo3[1] <= 0xBF
}

// newExtendableString creates a flat string that owns the spare capacity of b.
func (rt *Runtime) newExtendableString(b []byte) Value {
	v := rt.newStringBytes(b)
	if fs := rt.flatOf(v); fs != nil {
		fs.extendable = true
	}
	return v
}
