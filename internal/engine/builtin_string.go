package engine

// String constructor + String.prototype (ant builtin_string). Index-based
// methods operate on UTF-16 code units via the strings.go cursor helpers.

import (
	"math"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// isRegExpValue implements ES IsRegExp: an object whose Symbol.match is truthy
// is "regexp-like", otherwise the real [[RegExpMatcher]] (o.regex) decides.
func (rt *Runtime) isRegExpValue(v Value) bool {
	o := rt.objPtr(v)
	if o == nil {
		return false
	}
	if rt.symMatch != 0 {
		if m := rt.getFieldSymbol(v, rt.symMatch.handle()); !m.IsUndefined() {
			return rt.toBoolean(m)
		}
	}
	return o.regex != nil
}

// thisStringBytes coerces a method receiver to its WTF-8 string bytes.
func (rt *Runtime) thisStringBytes(this Value) ([]byte, *ThrowError) {
	if this.IsString() {
		return rt.strBytes(this), nil
	}
	s, e := rt.toStringValue(this)
	if e != nil {
		return nil, e
	}
	return rt.strBytes(s), nil
}

func (rt *Runtime) initStringBuiltin() {
	proto := rt.objPtr(rt.stringProto)

	rt.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsString() {
			return this, nil
		}
		return mkundef(), rt.typeError("String.prototype.toString requires a string")
	})
	rt.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsString() {
			return this, nil
		}
		return mkundef(), rt.typeError("String.prototype.valueOf requires a string")
	})

	rt.defMethod(proto, "charAt", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		idx := rt.intArg(args, 0)
		if idx < 0 || idx >= utf16Len(b) {
			return rt.internString(""), nil
		}
		return rt.charAt(b, idx), nil
	})
	rt.defMethod(proto, "charCodeAt", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		idx := rt.intArg(args, 0)
		if idx < 0 || idx >= utf16Len(b) {
			return mknum(math.NaN()), nil
		}
		return mknum(float64(utf16CodeUnitAt(b, idx))), nil
	})
	rt.defMethod(proto, "codePointAt", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		idx := rt.intArg(args, 0)
		if idx < 0 || idx >= utf16Len(b) {
			return mkundef(), nil
		}
		return mknum(float64(utf16CodepointAt(b, idx))), nil
	})
	rt.defMethod(proto, "indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(utf16IndexOf(b, sub, 0))), nil
	})
	rt.defMethod(proto, "lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(utf16LastIndexOf(b, sub))), nil
	})
	rt.defMethod(proto, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		if rt.isRegExpValue(arg(args, 0)) {
			return mkundef(), rt.typeError("First argument to String.prototype.includes must not be a regular expression")
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return mkbool(utf16IndexOf(b, sub, 0) >= 0), nil
	})
	rt.defMethod(proto, "startsWith", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		if rt.isRegExpValue(arg(args, 0)) {
			return mkundef(), rt.typeError("First argument to String.prototype.startsWith must not be a regular expression")
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return mkbool(strings.HasPrefix(string(b), string(sub))), nil
	})
	rt.defMethod(proto, "endsWith", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		if rt.isRegExpValue(arg(args, 0)) {
			return mkundef(), rt.typeError("First argument to String.prototype.endsWith must not be a regular expression")
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return mkbool(strings.HasSuffix(string(b), string(sub))), nil
	})

	rt.defMethod(proto, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		n := utf16Len(b)
		start := relIndex(rt, arg(args, 0), n, 0)
		end := relIndex(rt, arg(args, 1), n, n)
		if start >= end {
			return rt.internString(""), nil
		}
		bs, be := utf16RangeToByteRange(b, start, end)
		return rt.newStringBytes(append([]byte{}, b[bs:be]...)), nil
	})
	rt.defMethod(proto, "substring", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		n := utf16Len(b)
		start := clampIndex(rt.intArg(args, 0), n)
		end := n
		if !arg(args, 1).IsUndefined() {
			end = clampIndex(rt.intArg(args, 1), n)
		}
		if start > end {
			start, end = end, start
		}
		bs, be := utf16RangeToByteRange(b, start, end)
		return rt.newStringBytes(append([]byte{}, b[bs:be]...)), nil
	})
	rt.defMethod(proto, "toUpperCase", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.ToUpper(string(b))), nil
	})
	rt.defMethod(proto, "toLowerCase", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.ToLower(string(b))), nil
	})
	rt.defMethod(proto, "trim", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.TrimFunc(string(b), jsStrWhitespace)), nil
	})
	rt.defMethod(proto, "concat", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		out := append([]byte{}, b...)
		for _, a := range args {
			s, e := rt.toStringValue(a)
			if e != nil {
				return mkundef(), e
			}
			out = append(out, rt.strBytes(s)...)
		}
		return rt.newStringBytes(out), nil
	})
	rt.defMethod(proto, "repeat", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		n := rt.intArg(args, 0)
		if n < 0 {
			return mkundef(), rt.rangeError("Invalid count value")
		}
		return rt.newStringBytes([]byte(strings.Repeat(string(b), n))), nil
	})
	rt.defMethod(proto, "split", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		res := rt.newArray()
		ro := rt.objPtr(res)
		if arg(args, 0).IsUndefined() {
			rt.arraySet(ro, 0, rt.newStringBytes(append([]byte{}, b...)))
			return res, nil
		}
		sep, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		var parts []string
		if len(sep) == 0 {
			// split into UTF-16 units
			for i := 0; i < utf16Len(b); i++ {
				el := rt.charAt(b, i)
				rt.arraySet(ro, ro.arrLen, el)
			}
			return res, nil
		}
		parts = strings.Split(string(b), string(sep))
		for _, p := range parts {
			rt.arraySet(ro, ro.arrLen, rt.newString(p))
		}
		return res, nil
	})

	rt.defMethod(proto, "substr", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		n := utf16Len(b)
		start := rt.intArg(args, 0)
		if start < 0 {
			start = max(n+start, 0)
		}
		length := n - start
		if !arg(args, 1).IsUndefined() {
			length = rt.intArg(args, 1)
		}
		if length < 0 || start >= n {
			return rt.internString(""), nil
		}
		end := min(start+length, n)
		bs, be := utf16RangeToByteRange(b, start, end)
		return rt.newStringBytes(append([]byte{}, b[bs:be]...)), nil
	})
	rt.defMethod(proto, "localeCompare", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		other, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(strings.Compare(string(b), string(other)))), nil
	})
	rt.defMethod(proto, "toLocaleLowerCase", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.ToLower(string(b))), nil
	})
	rt.defMethod(proto, "toLocaleUpperCase", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.ToUpper(string(b))), nil
	})

	pad := func(atStart bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			b, e := rt.thisStringBytes(this)
			if e != nil {
				return mkundef(), e
			}
			targetLen := rt.intArg(args, 0)
			cur := utf16Len(b)
			if cur >= targetLen {
				return rt.newStringBytes(append([]byte{}, b...)), nil
			}
			filler := " "
			if !arg(args, 1).IsUndefined() {
				fb, e := rt.stringArg(args, 1)
				if e != nil {
					return mkundef(), e
				}
				filler = string(fb)
			}
			if filler == "" {
				return rt.newStringBytes(append([]byte{}, b...)), nil
			}
			need := targetLen - cur
			var padB []byte
			for utf16Len(padB) < need {
				padB = append(padB, filler...)
			}
			// Truncate the pad to exactly `need` UTF-16 units.
			ps, pe := utf16RangeToByteRange(padB, 0, need)
			padB = padB[ps:pe]
			if atStart {
				return rt.newStringBytes(append(append([]byte{}, padB...), b...)), nil
			}
			return rt.newStringBytes(append(append([]byte{}, b...), padB...)), nil
		}
	}
	rt.defMethod(proto, "padStart", 1, pad(true))
	rt.defMethod(proto, "padEnd", 1, pad(false))
	rt.defMethod(proto, "at", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		n := utf16Len(b)
		idx := rt.intArg(args, 0)
		if idx < 0 {
			idx += n
		}
		if idx < 0 || idx >= n {
			return mkundef(), nil
		}
		return rt.charAt(b, idx), nil
	})
	rt.defMethod(proto, "trimStart", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.TrimLeftFunc(string(b), jsStrWhitespace)), nil
	})
	rt.defMethod(proto, "trimEnd", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.TrimRightFunc(string(b), jsStrWhitespace)), nil
	})

	// String constructor.
	ctor := rt.newNativeFunc("String", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if len(args) == 0 {
			return rt.internString(""), nil
		}
		return rt.toStringValue(args[0])
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.stringProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	// String.prototype[Symbol.iterator] iterates by code point (astral-aware).
	if rt.symIterator != 0 {
		strIter := rt.newNativeFunc("[Symbol.iterator]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			s, e := rt.toStringValue(this)
			if e != nil {
				return mkundef(), e
			}
			vals, _ := rt.iterableValues(s)
			i := 0
			return rt.newIteratorObject(func() (Value, bool) {
				if i >= len(vals) {
					return mkundef(), true
				}
				v := vals[i]
				i++
				return v, false
			}), nil
		})
		proto.defineOwnSymbol(rt.symIterator.handle(), strIter, attrWritable|attrConfigurable)
	}
	rt.defMethod(proto, "normalize", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		form := "NFC"
		if f := arg(args, 0); !f.IsUndefined() {
			fv, e := rt.toStringValue(f)
			if e != nil {
				return mkundef(), e
			}
			form = string(rt.strBytes(fv))
		}
		var nf norm.Form
		switch form {
		case "NFC":
			nf = norm.NFC
		case "NFD":
			nf = norm.NFD
		case "NFKC":
			nf = norm.NFKC
		case "NFKD":
			nf = norm.NFKD
		default:
			return mkundef(), rt.rangeError("The normalization form should be one of NFC, NFD, NFKC, NFKD")
		}
		return rt.newStringBytes(nf.Bytes(b)), nil
	})

	// String.raw`...` — assemble a template's raw strings with substitutions.
	rt.defMethod(cobj, "raw", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		tmpl := arg(args, 0)
		rawV, e := rt.getField(tmpl, "raw")
		if e != nil {
			return mkundef(), e
		}
		if rawV.IsNullish() {
			return mkundef(), rt.typeError("String.raw requires a template object")
		}
		n, e := rt.lengthOf(rawV)
		if e != nil {
			return mkundef(), e
		}
		var b []byte
		for i := 0; i < n; i++ {
			seg, _ := rt.getElement(rawV, mknum(float64(i)))
			ss, _ := rt.toStringValue(seg)
			b = append(b, rt.strBytes(ss)...)
			if i+1 < n && i+1 < len(args) {
				sub, _ := rt.toStringValue(args[i+1])
				b = append(b, rt.strBytes(sub)...)
			}
		}
		return rt.newStringBytes(b), nil
	})
	rt.defMethod(cobj, "fromCharCode", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		units := make([]uint16, len(args))
		for i, a := range args {
			n, e := rt.toNumber(a)
			if e != nil {
				return mkundef(), e
			}
			units[i] = uint16(toUint32(n))
		}
		return rt.newStringBytes(utf16ToWTF8(units)), nil
	})
	rt.defMethod(cobj, "fromCodePoint", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		var out []byte
		for _, a := range args {
			n, e := rt.toNumber(a)
			if e != nil {
				return mkundef(), e
			}
			out = wtf8Encode(out, uint32(n))
		}
		return rt.newStringBytes(out), nil
	})
	rt.defGlobal("String", ctor)
}

// ---- helpers ----

func (rt *Runtime) intArg(args []Value, i int) int {
	if i >= len(args) || args[i].IsUndefined() {
		return 0
	}
	n, _ := rt.toNumberPrimitive(args[i])
	if n != n { // NaN → 0
		return 0
	}
	return int(n)
}

func (rt *Runtime) stringArg(args []Value, i int) ([]byte, *ThrowError) {
	s, e := rt.toStringValue(arg(args, i))
	if e != nil {
		return nil, e
	}
	return rt.strBytes(s), nil
}

func clampIndex(i, n int) int {
	if i < 0 {
		return 0
	}
	if i > n {
		return n
	}
	return i
}

// utf16IndexOf returns the UTF-16 index of sub in b at or after `from` (-1 if
// absent). Implemented via byte search then offset conversion.
func utf16IndexOf(b, sub []byte, from int) int {
	if len(sub) == 0 {
		return from
	}
	bStart, _, ok := utf16IndexToByteOffset(b, from)
	if !ok {
		return -1
	}
	byteIdx := indexBytes(b[bStart:], sub)
	if byteIdx < 0 {
		return -1
	}
	return byteOffsetToUtf16(b, bStart+byteIdx)
}

func utf16LastIndexOf(b, sub []byte) int {
	if len(sub) == 0 {
		return utf16Len(b)
	}
	byteIdx := lastIndexBytes(b, sub)
	if byteIdx < 0 {
		return -1
	}
	return byteOffsetToUtf16(b, byteIdx)
}

func indexBytes(b, sub []byte) int     { return strings.Index(string(b), string(sub)) }
func lastIndexBytes(b, sub []byte) int { return strings.LastIndex(string(b), string(sub)) }
