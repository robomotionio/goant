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
			return rt.newString(uriEncode(b, keep)), nil
		}
	}
	decode := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		out, ok := uriDecode(b)
		if !ok {
			ev, _ := rt.construct(rt.errors.uriErr, []Value{rt.newString("URI malformed")})
			return mkundef(), &ThrowError{Value: ev, rt: rt}
		}
		return rt.newStringBytes(out), nil
	}
	rt.defMethod(g, "encodeURIComponent", 1, encode(uriUnreserved))
	rt.defMethod(g, "encodeURI", 1, encode(uriUnreserved+uriReserved))
	rt.defMethod(g, "decodeURIComponent", 1, decode)
	rt.defMethod(g, "decodeURI", 1, decode)
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

// uriEncode percent-encodes UTF-8 bytes not in the keep set.
func uriEncode(b []byte, keep string) string {
	var sb strings.Builder
	for _, c := range b {
		if c < 128 && strings.IndexByte(keep, c) >= 0 {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('%')
			sb.WriteByte(hexDigits[c>>4])
			sb.WriteByte(hexDigits[c&0xF])
		}
	}
	return sb.String()
}

// uriDecode reverses percent-encoding; ok=false on malformed input.
func uriDecode(b []byte) ([]byte, bool) {
	var out []byte
	for i := 0; i < len(b); i++ {
		if b[i] == '%' {
			if i+2 >= len(b) || !isHexByte(b[i+1]) || !isHexByte(b[i+2]) {
				return nil, false
			}
			out = append(out, byte(hexVal(b[i+1])<<4|hexVal(b[i+2])))
			i += 2
		} else {
			out = append(out, b[i])
		}
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
