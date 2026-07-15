package engine

// escape/unescape (annex-b B.2.1/B.2.2) and the URI codecs encodeURI/decodeURI/
// encodeURIComponent/decodeURIComponent (ES3 §15.1.3, ant modules/uri.c).

import "strings"

const hexDigits = "0123456789ABCDEF"

func (rt *Runtime) initURIBuiltins() {
	g := rt.objPtr(rt.global)

	rt.defMethod(g, "escape", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(jsEscape(b)), nil
	})
	rt.defMethod(g, "unescape", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return rt.newStringBytes(jsUnescape(b)), nil
	})

	// URI component/full encoders. unreserved sets per spec.
	const uriUnreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	const uriReserved = ";/?:@&=+$,#"
	encode := func(keep string) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			b, e := rt.stringArg(args, 0)
			if e != nil {
				return mkundef(), e
			}
			out, ok := uriEncode(b, keep)
			if !ok {
				ev, _ := rt.construct(rt.errors.uriErr, []Value{rt.newString("URI malformed")})
				return mkundef(), &ThrowError{Value: ev, rt: rt}
			}
			return rt.newString(out), nil
		}
	}
	decode := func(reserved string) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			b, e := rt.stringArg(args, 0)
			if e != nil {
				return mkundef(), e
			}
			out, ok := uriDecode(b, reserved)
			if !ok {
				ev, _ := rt.construct(rt.errors.uriErr, []Value{rt.newString("URI malformed")})
				return mkundef(), &ThrowError{Value: ev, rt: rt}
			}
			return rt.newStringBytes(out), nil
		}
	}
	rt.defMethod(g, "encodeURIComponent", 1, encode(uriUnreserved))
	rt.defMethod(g, "encodeURI", 1, encode(uriUnreserved+uriReserved))
	// decodeURIComponent decodes every escape; decodeURI keeps the reserved set
	// (and '#') percent-encoded (its reservedURISet).
	rt.defMethod(g, "decodeURIComponent", 1, decode(""))
	rt.defMethod(g, "decodeURI", 1, decode(uriReserved))
}

// jsEscape implements the annex-b escape function over UTF-16 code units.
func jsEscape(b []byte) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789@*_+-./"
	var sb strings.Builder
	n := utf16Len(b)
	for i := 0; i < n; i++ {
		cu := utf16CodeUnitAt(b, i)
		if cu < 128 && strings.IndexByte(unreserved, byte(cu)) >= 0 {
			sb.WriteByte(byte(cu))
		} else if cu < 256 {
			sb.WriteByte('%')
			sb.WriteByte(hexDigits[cu>>4])
			sb.WriteByte(hexDigits[cu&0xF])
		} else {
			sb.WriteString("%u")
			sb.WriteByte(hexDigits[(cu>>12)&0xF])
			sb.WriteByte(hexDigits[(cu>>8)&0xF])
			sb.WriteByte(hexDigits[(cu>>4)&0xF])
			sb.WriteByte(hexDigits[cu&0xF])
		}
	}
	return sb.String()
}

// jsUnescape reverses jsEscape.
func jsUnescape(b []byte) []byte {
	var units []uint16
	i, n := 0, utf16Len(b)
	for i < n {
		cu := utf16CodeUnitAt(b, i)
		if cu == '%' {
			if i+5 < n && utf16CodeUnitAt(b, i+1) == 'u' &&
				isHex4(b, i+2) {
				units = append(units, uint16(hex4(b, i+2)))
				i += 6
				continue
			}
			if i+2 < n && isHexUnit(utf16CodeUnitAt(b, i+1)) && isHexUnit(utf16CodeUnitAt(b, i+2)) {
				units = append(units, uint16(hexVal(byte(utf16CodeUnitAt(b, i+1)))<<4|hexVal(byte(utf16CodeUnitAt(b, i+2)))))
				i += 3
				continue
			}
		}
		units = append(units, uint16(cu))
		i++
	}
	return utf16ToWTF8(units)
}

// uriEncode implements the spec Encode operation over UTF-16 code units: a code
// unit in the keep set is copied, any other is UTF-8 encoded and percent-escaped
// octet by octet. A surrogate must be part of a valid pair; a lone surrogate is
// malformed (ok=false → URIError).
func uriEncode(b []byte, keep string) (string, bool) {
	var sb strings.Builder
	n := utf16Len(b)
	for i := 0; i < n; i++ {
		cu := utf16CodeUnitAt(b, i)
		var cp uint32
		switch {
		case cu >= 0xD800 && cu <= 0xDBFF: // high surrogate: a low surrogate must follow
			if i+1 >= n {
				return "", false
			}
			lo := utf16CodeUnitAt(b, i+1)
			if lo < 0xDC00 || lo > 0xDFFF {
				return "", false
			}
			cp = 0x10000 + (uint32(cu-0xD800) << 10) + uint32(lo-0xDC00)
			i++
		case cu >= 0xDC00 && cu <= 0xDFFF: // lone low surrogate
			return "", false
		default:
			cp = uint32(cu)
		}
		if cp < 128 && strings.IndexByte(keep, byte(cp)) >= 0 {
			sb.WriteByte(byte(cp))
			continue
		}
		var octets [4]byte
		var m int
		switch {
		case cp < 0x80:
			octets[0], m = byte(cp), 1
		case cp < 0x800:
			octets[0], octets[1] = 0xC0|byte(cp>>6), 0x80|byte(cp&0x3F)
			m = 2
		case cp < 0x10000:
			octets[0], octets[1], octets[2] = 0xE0|byte(cp>>12), 0x80|byte((cp>>6)&0x3F), 0x80|byte(cp&0x3F)
			m = 3
		default:
			octets[0], octets[1], octets[2], octets[3] = 0xF0|byte(cp>>18), 0x80|byte((cp>>12)&0x3F), 0x80|byte((cp>>6)&0x3F), 0x80|byte(cp&0x3F)
			m = 4
		}
		for k := 0; k < m; k++ {
			sb.WriteByte('%')
			sb.WriteByte(hexDigits[octets[k]>>4])
			sb.WriteByte(hexDigits[octets[k]&0xF])
		}
	}
	return sb.String(), true
}

// uriDecode implements the spec Decode operation: it reverses percent-encoding,
// assembling a multi-octet UTF-8 sequence from consecutive %XX escapes and
// rejecting (ok=false → URIError) any malformed byte, missing/invalid
// continuation octet, overlong encoding, surrogate, or out-of-range code point.
// A decoded ASCII character in the reserved set is left percent-encoded.
func uriDecode(b []byte, reserved string) ([]byte, bool) {
	var out []byte
	n := len(b)
	for i := 0; i < n; i++ {
		if b[i] != '%' {
			out = append(out, b[i])
			continue
		}
		if i+2 >= n || !isHexByte(b[i+1]) || !isHexByte(b[i+2]) {
			return nil, false
		}
		c := byte(hexVal(b[i+1])<<4 | hexVal(b[i+2]))
		start := i
		i += 2
		if c < 0x80 {
			if strings.IndexByte(reserved, c) >= 0 {
				out = append(out, b[start], b[start+1], b[start+2]) // keep "%XX"
			} else {
				out = append(out, c)
			}
			continue
		}
		// Multi-octet lead byte: the count comes from the leading one-bits.
		var nOct int
		switch {
		case c&0xE0 == 0xC0:
			nOct = 2
		case c&0xF0 == 0xE0:
			nOct = 3
		case c&0xF8 == 0xF0:
			nOct = 4
		default:
			return nil, false // a lone continuation (0x80–0xBF) or an invalid lead
		}
		octets := make([]byte, 1, nOct)
		octets[0] = c
		for j := 1; j < nOct; j++ {
			if i+3 >= n || b[i+1] != '%' || !isHexByte(b[i+2]) || !isHexByte(b[i+3]) {
				return nil, false
			}
			c2 := byte(hexVal(b[i+2])<<4 | hexVal(b[i+3]))
			if c2&0xC0 != 0x80 {
				return nil, false // not a continuation octet
			}
			octets = append(octets, c2)
			i += 3
		}
		// Assemble the code point and reject overlong / surrogate / out-of-range.
		var v uint32
		switch nOct {
		case 2:
			v = uint32(octets[0]&0x1F)<<6 | uint32(octets[1]&0x3F)
			if v < 0x80 {
				return nil, false
			}
		case 3:
			v = uint32(octets[0]&0x0F)<<12 | uint32(octets[1]&0x3F)<<6 | uint32(octets[2]&0x3F)
			if v < 0x800 || (v >= 0xD800 && v <= 0xDFFF) {
				return nil, false
			}
		case 4:
			v = uint32(octets[0]&0x07)<<18 | uint32(octets[1]&0x3F)<<12 | uint32(octets[2]&0x3F)<<6 | uint32(octets[3]&0x3F)
			if v < 0x10000 || v > 0x10FFFF {
				return nil, false
			}
		}
		out = append(out, octets...) // a validated UTF-8 sequence
	}
	return out, true
}

func isHexByte(c byte) bool { return isXDigitByte(c) }
func isHexUnit(u uint32) bool {
	return u < 128 && isXDigitByte(byte(u))
}
func isHex4(b []byte, i int) bool {
	for k := 0; k < 4; k++ {
		if !isHexUnit(utf16CodeUnitAt(b, i+k)) {
			return false
		}
	}
	return true
}
func hex4(b []byte, i int) uint32 {
	var v uint32
	for k := 0; k < 4; k++ {
		v = v<<4 | hexVal(byte(utf16CodeUnitAt(b, i+k)))
	}
	return v
}
