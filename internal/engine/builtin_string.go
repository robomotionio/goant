package engine

// String constructor + String.prototype (ant builtin_string). Index-based
// methods operate on UTF-16 code units via the strings.go cursor helpers.

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/robomotionio/goant/internal/regexpjs"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

// jsToUpperCase / jsToLowerCase apply the Unicode default (language-independent)
// case conversion ECMAScript toUpperCase/toLowerCase require — including the
// 1→many and contextual mappings Go's strings.ToUpper/ToLower miss (ß→SS,
// İ→i̇, ﬀ→FF, final sigma). A string carrying lone surrogates isn't valid
// UTF-8, so it falls back to simple casing (which leaves those bytes intact).
//
// An all-ASCII string is answered without any of that. For ASCII the Unicode
// default mapping IS the ASCII mapping: no ASCII character has a 1→many
// uppercase mapping (ß is U+00DF, ﬀ is U+FB00) and none takes part in a
// contextual rule (final sigma needs Σ), so the two paths agree exactly and
// the fast one is around ten times cheaper. cases.Upper builds a fresh Caser
// on every call, which measured 5.1% of the CPU of a benchmark whose script
// uppercases one field per record — before the transform it then runs.
//
// Both return a slice the caller owns, always freshly allocated, because
// newStringBytes takes ownership of what it is given. Returning the input
// unchanged when there is nothing to convert would hand a string cell the
// receiver's own buffer.
func jsToUpperCase(b []byte) []byte {
	if isASCIIBytes(b) {
		out := make([]byte, len(b))
		for i, c := range b {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			out[i] = c
		}
		return out
	}
	if utf8.Valid(b) {
		return []byte(cases.Upper(language.Und).String(string(b)))
	}
	return wtf8Case(b, unicode.ToUpper)
}

// wtf8Case applies the simple case mapping f to every code point in b, copying
// through any byte that is not part of a decodable one.
//
// This is the lone-surrogate path. goant stores strings as WTF-8, so an
// unpaired surrogate is three bytes that are not valid UTF-8, and the code
// point has no case mapping — toUpperCase must hand it back untouched.
// strings.ToUpper cannot: it decodes, and an undecodable byte decodes to
// U+FFFD, so "a\uD800b".toUpperCase() came back with the surrogate replaced by
// a replacement character. Silent corruption of the one input the fallback
// existed to protect, and the comment above it said it did the opposite.
func wtf8Case(b []byte, f func(rune) rune) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			// Undecodable: pass the byte through rather than mapping it. A real
			// U+FFFD in the input decodes with size 3 and so does not land here.
			out = append(out, b[i])
			i++
			continue
		}
		out = utf8.AppendRune(out, f(r))
		i += size
	}
	return out
}

func jsToLowerCase(b []byte) []byte {
	if isASCIIBytes(b) {
		out := make([]byte, len(b))
		for i, c := range b {
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			out[i] = c
		}
		return out
	}
	return []byte(jsToLowerCaseSlow(b))
}

func jsToLowerCaseSlow(b []byte) string {
	if !utf8.Valid(b) {
		return string(wtf8Case(b, unicode.ToLower))
	}
	s := string(b)
	lower := cases.Lower(language.Und)
	if !strings.ContainsRune(s, 'Σ') {
		return lower.String(s)
	}
	// Σ is decided here rather than by the general mapping: Final_Sigma is a
	// CONTEXT condition, and the one place the default (language-independent)
	// lowercase mapping has one. Splitting the string at each Σ is safe precisely
	// because it is the only such rule — every other Und mapping is per-character.
	rs := []rune(s)
	var out strings.Builder
	start := 0
	for i, r := range rs {
		if r != 'Σ' {
			continue
		}
		out.WriteString(lower.String(string(rs[start:i])))
		if finalSigma(rs, i) {
			out.WriteRune('ς')
		} else {
			out.WriteRune('σ')
		}
		start = i + 1
	}
	out.WriteString(lower.String(string(rs[start:])))
	return out.String()
}

// finalSigma reports whether the Σ at rs[i] takes the word-final form: it is
// preceded by a Cased character and not followed by one, in both directions
// skipping Case_Ignorable characters. U+0345 is BOTH cased and case-ignorable,
// and being case-ignorable is what decides it — so `\u0345\u03A3` lowercases to
// a non-final sigma while `\u0391\u0345\u03A3` gives the final one.
func finalSigma(rs []rune, i int) bool {
	cased := regexpjs.UnicodeBinaryProperty("Cased")
	ignorable := regexpjs.UnicodeBinaryProperty("Case_Ignorable")
	if cased == nil || ignorable == nil {
		return false
	}
	before := false
	for j := i - 1; j >= 0; j-- {
		if unicode.Is(ignorable, rs[j]) {
			continue
		}
		before = unicode.Is(cased, rs[j])
		break
	}
	if !before {
		return false
	}
	for j := i + 1; j < len(rs); j++ {
		if unicode.Is(ignorable, rs[j]) {
			continue
		}
		return !unicode.Is(cased, rs[j])
	}
	return true
}

// isRegExpValue implements ES IsRegExp: an object whose Symbol.match is truthy
// is "regexp-like", otherwise the real [[RegExpMatcher]] (o.regex) decides.
func (rt *Runtime) isRegExpValue(v Value) bool {
	o := rt.objPtr(v)
	if o == nil {
		return false
	}
	if rt.symMatch != 0 {
		if m, _ := rt.getFieldSymbol(v, rt.symMatch.handle()); !m.IsUndefined() {
			return rt.toBoolean(m)
		}
	}
	return o.regex != nil
}

// thisString coerces a method receiver to a string, returning both the string
// and its WTF-8 bytes.
//
// Both, because a String.prototype method needs both and one cannot be got back
// from the other. The bytes are what every search and slice works over; the
// value is what carries the cached UTF-16 length and code-unit index, so a
// method that returns only bytes forces its caller to rescan the string to find
// out how long it is — which is what made walking a string quadratic.
func (rt *Runtime) thisString(this Value) (Value, []byte, *ThrowError) {
	if this.IsString() {
		return this, rt.strBytes(this), nil
	}
	// RequireObjectCoercible: a String.prototype method rejects a null/undefined
	// receiver before ToString (which would otherwise stringify it to "null").
	if this.IsNullish() {
		return mkundef(), nil, rt.typeError("String.prototype method called on null or undefined")
	}
	s, e := rt.toStringValue(this)
	if e != nil {
		return mkundef(), nil, e
	}
	return s, rt.strBytes(s), nil
}

// thisStringBytes is thisString for a caller that needs only the bytes.
func (rt *Runtime) thisStringBytes(this Value) ([]byte, *ThrowError) {
	_, b, e := rt.thisString(this)
	return b, e
}

// maxStringLength is the greatest length a String value may have. There is no
// spec limit — 2^53-1 is the notional maximum — so this matches V8's, which is
// what the error message and the boundary are observable against: "a".repeat of
// that many is fine and one more is a RangeError.
const maxStringLength = 1<<29 - 24

func (rt *Runtime) initStringBuiltin() {
	proto := rt.objPtr(rt.stringProto)
	// String.prototype is itself a String wrapper whose [[StringData]] is "" (so
	// String.prototype.valueOf() is "" and Object.prototype.toString tags it
	// "[object String]").
	proto.boxed = rt.newString("")
	// String.prototype is a String exotic object, so it owns "length" like any
	// other: a non-writable, non-enumerable, non-configurable 0.
	proto.defineOwn("length", mknum(0), 0)

	strThis := func(this Value) (Value, *ThrowError) {
		if this.IsString() {
			return this, nil
		}
		if o := rt.objPtr(this); o != nil && o.boxed.IsString() {
			return o.boxed, nil
		}
		return mkundef(), rt.typeError("String.prototype method requires that 'this' be a String")
	}
	rt.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return strThis(this)
	})
	rt.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return strThis(this)
	})

	rt.defMethod(proto, "charAt", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, _, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		idx, e := rt.posArgE(args, 0)
		if e != nil {
			return mkundef(), e
		}
		if idx < 0 || idx >= rt.strLen16(sv) {
			return rt.internString(""), nil
		}
		return rt.strCharAt(sv, idx), nil
	})
	rt.defMethod(proto, "charCodeAt", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, _, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		idx, e := rt.posArgE(args, 0)
		if e != nil {
			return mkundef(), e
		}
		if idx < 0 || idx >= rt.strLen16(sv) {
			return mknum(math.NaN()), nil
		}
		return mknum(float64(rt.strUnitAt(sv, idx))), nil
	})
	rt.defMethod(proto, "codePointAt", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, _, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		idx, e := rt.posArgE(args, 0)
		if e != nil {
			return mkundef(), e
		}
		if idx < 0 || idx >= rt.strLen16(sv) {
			return mkundef(), nil
		}
		return mknum(float64(rt.strCodepointAt(sv, idx))), nil
	})
	rt.defMethod(proto, "indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		start, e := rt.strClampPos(arg(args, 1), rt.strLen16(sv))
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(utf16IndexOf(b, sub, start))), nil
	})
	rt.defMethod(proto, "lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		// ToNumber(position) is observable and must run (and may throw); NaN means
		// search the whole string, otherwise clamp ToInteger(position) to [0, len].
		numPos, e := rt.toNumber(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		n := rt.strLen16(sv)
		start := n
		if !math.IsNaN(numPos) {
			switch {
			case numPos < 0:
				start = 0
			case numPos < float64(n):
				start = int(math.Trunc(numPos))
			default:
				start = n
			}
		}
		if len(sub) == 0 {
			return mknum(float64(start)), nil
		}
		// The result is the greatest match index k ≤ start.
		result := -1
		for from := 0; ; {
			idx := utf16IndexOf(b, sub, from)
			if idx < 0 || idx > start {
				break
			}
			result = idx
			from = idx + 1
		}
		return mknum(float64(result)), nil
	})
	rt.defMethod(proto, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		if isRe, e := rt.isRegExp(arg(args, 0)); e != nil {
			return mkundef(), e
		} else if isRe {
			return mkundef(), rt.typeError("First argument to String.prototype.includes must not be a regular expression")
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		start, e := rt.strClampPos(arg(args, 1), rt.strLen16(sv))
		if e != nil {
			return mkundef(), e
		}
		return mkbool(utf16IndexOf(b, sub, start) >= 0), nil
	})
	rt.defMethod(proto, "startsWith", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		if isRe, e := rt.isRegExp(arg(args, 0)); e != nil {
			return mkundef(), e
		} else if isRe {
			return mkundef(), rt.typeError("First argument to String.prototype.startsWith must not be a regular expression")
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		n := rt.strLen16(sv)
		start, e := rt.strClampPos(arg(args, 1), n)
		if e != nil {
			return mkundef(), e
		}
		bs, _ := utf16RangeToByteRange(b, start, n)
		return mkbool(strings.HasPrefix(string(b[bs:]), string(sub))), nil
	})
	rt.defMethod(proto, "endsWith", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		if isRe, e := rt.isRegExp(arg(args, 0)); e != nil {
			return mkundef(), e
		} else if isRe {
			return mkundef(), rt.typeError("First argument to String.prototype.endsWith must not be a regular expression")
		}
		sub, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		n := rt.strLen16(sv)
		end := n
		if !arg(args, 1).IsUndefined() {
			if end, e = rt.strClampPos(arg(args, 1), n); e != nil {
				return mkundef(), e
			}
		}
		_, be := utf16RangeToByteRange(b, 0, end)
		return mkbool(strings.HasSuffix(string(b[:be]), string(sub))), nil
	})

	rt.defMethod(proto, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, _, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		n := rt.strLen16(sv)
		start, e := rt.relativeIndexE(arg(args, 0), n)
		if e != nil {
			return mkundef(), e
		}
		end := n
		if !arg(args, 1).IsUndefined() {
			if end, e = rt.relativeIndexE(arg(args, 1), n); e != nil {
				return mkundef(), e
			}
		}
		if start >= end {
			return rt.internString(""), nil
		}
		return rt.newStringBytes(rt.strSubstring(sv, start, end)), nil
	})
	rt.defMethod(proto, "substring", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, _, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		n := rt.strLen16(sv)
		start, e := rt.strClampPos(arg(args, 0), n)
		if e != nil {
			return mkundef(), e
		}
		end := n
		if !arg(args, 1).IsUndefined() {
			if end, e = rt.strClampPos(arg(args, 1), n); e != nil {
				return mkundef(), e
			}
		}
		if start > end {
			start, end = end, start
		}
		return rt.newStringBytes(rt.strSubstring(sv, start, end)), nil
	})
	rt.defMethod(proto, "toUpperCase", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newStringBytes(jsToUpperCase(b)), nil
	})
	rt.defMethod(proto, "toLowerCase", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newStringBytes(jsToLowerCase(b)), nil
	})
	// isWellFormed / toWellFormed (ES2024): a string is well-formed iff it has no
	// UNPAIRED surrogate code units. goant stores strings as WTF-8, but a surrogate
	// pair formed by concatenation ("\uD83D"+"\uDCA9") is stored as two lone-surrogate
	// encodings, so utf8.Valid is not sufficient — the units must be paired.
	// toWellFormed replaces each unpaired surrogate with U+FFFD.
	rt.defMethod(proto, "isWellFormed", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		return mkbool(wtf8WellFormed(b)), nil
	})
	rt.defMethod(proto, "toWellFormed", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		if utf8.Valid(b) {
			return rt.newStringBytes(append([]byte{}, b...)), nil
		}
		n := rt.strLen16(sv)
		units := make([]uint16, 0, n)
		for i := 0; i < n; i++ {
			cu := rt.strUnitAt(sv, i)
			switch {
			case cu >= 0xD800 && cu <= 0xDBFF: // high surrogate
				if i+1 < n {
					if next := rt.strUnitAt(sv, i+1); next >= 0xDC00 && next <= 0xDFFF {
						units = append(units, uint16(cu), uint16(next))
						i++
						continue
					}
				}
				units = append(units, 0xFFFD)
			case cu >= 0xDC00 && cu <= 0xDFFF: // lone low surrogate
				units = append(units, 0xFFFD)
			default:
				units = append(units, uint16(cu))
			}
		}
		return rt.newStringBytes(utf16ToWTF8(units)), nil
	})
	rt.defMethod(proto, "trim", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.TrimFunc(string(b), jsStrWhitespace)), nil
	})
	rt.defMethod(proto, "concat", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
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
		sv, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		nf, e := rt.toIntegerOrInfinity(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		if nf < 0 || math.IsInf(nf, 1) {
			return mkundef(), rt.rangeError("Invalid count value")
		}
		// int(nf) is undefined in Go when nf does not fit, and on amd64 1e100
		// lands on the minimum int — which reached strings.Repeat as a negative
		// count and panicked the process. Decide the size in float64, where the
		// comparison is meaningful, and only convert once it is known to fit.
		//
		// Repeating the empty string is exempt: the result is empty however large
		// the count, and V8 allows it — but it must return before the conversion,
		// not merely skip the length check, or the same overflow reaches
		// strings.Repeat.
		cur := rt.strLen16(sv)
		if cur == 0 {
			return rt.newString(""), nil
		}
		if nf > float64(maxStringLength/cur) {
			return mkundef(), rt.rangeError("Invalid string length")
		}
		return rt.newStringBytes([]byte(strings.Repeat(string(b), int(nf)))), nil
	})
	rt.defMethod(proto, "split", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, b, e := rt.thisString(this)
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
			for i := 0; i < rt.strLen16(sv); i++ {
				el := rt.strCharAt(sv, i)
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
		sv, _, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		size := rt.strLen16(sv)
		// ToIntegerOrInfinity(start) and, if present, (length) — a throwing valueOf
		// on either argument propagates.
		intStart, e := rt.toIntegerOrInfinity(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		end := math.Inf(1) // length defaults to +Infinity (to end of string)
		if !arg(args, 1).IsUndefined() {
			if end, e = rt.toIntegerOrInfinity(arg(args, 1)); e != nil {
				return mkundef(), e
			}
		}
		var start int
		switch {
		case math.IsInf(intStart, -1):
			start = 0
		case intStart < 0:
			start = int(math.Max(float64(size)+intStart, 0))
		default:
			start = int(math.Min(intStart, float64(size)))
		}
		resultLen := math.Max(math.Min(end, float64(size-start)), 0)
		if resultLen <= 0 {
			return rt.internString(""), nil
		}
		return rt.newStringBytes(rt.strSubstring(sv, start, start+int(resultLen))), nil
	})
	rt.defMethod(proto, "localeCompare", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		other, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		// Specified as Intl.Collator's compare when ECMA-402 is present, which
		// it is here, so this delegates rather than keeping a second ordering
		// that would disagree with it. That is also what makes canonically
		// equivalent strings compare equal: the collation algorithm normalises,
		// where a code-unit order cannot.
		c, e := rt.collatorForCompare(arg(args, 1), arg(args, 2))
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(c.compare(string(b), string(other)))), nil
	})
	rt.defMethod(proto, "toLocaleLowerCase", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newStringBytes(jsToLowerCase(b)), nil
	})
	rt.defMethod(proto, "toLocaleUpperCase", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newStringBytes(jsToUpperCase(b)), nil
	})

	pad := func(atStart bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			sv, b, e := rt.thisString(this)
			if e != nil {
				return mkundef(), e
			}
			mlF, e := rt.toIntegerOrInfinity(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			// Clamping to 0x7FFFFFFF and building the result anyway meant
			// "a".padStart(1e100) sat there trying to assemble a two-gigabyte
			// string. A length no String value can have is a RangeError, not a
			// long wait.
			if mlF > float64(maxStringLength) {
				return mkundef(), rt.rangeError("Invalid string length")
			}
			targetLen := 0
			if mlF > 0 {
				targetLen = int(mlF)
			}
			cur := rt.strLen16(sv)
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
			// Truncate the pad to exactly `need` UTF-16 units (may bisect a surrogate).
			padB = utf16TruncateUnits(padB, need)
			if atStart {
				return rt.newStringBytes(append(append([]byte{}, padB...), b...)), nil
			}
			return rt.newStringBytes(append(append([]byte{}, b...), padB...)), nil
		}
	}
	rt.defMethod(proto, "padStart", 1, pad(true))
	rt.defMethod(proto, "padEnd", 1, pad(false))
	rt.defMethod(proto, "at", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, _, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		n := rt.strLen16(sv)
		rel, e := rt.toIntegerOrInfinity(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		k := int(rel)
		if rel < 0 {
			if kf := float64(n) + rel; kf < 0 {
				return mkundef(), nil
			} else {
				k = int(kf)
			}
		} else if rel >= float64(n) {
			return mkundef(), nil
		}
		if k < 0 || k >= n {
			return mkundef(), nil
		}
		return rt.strCharAt(sv, k), nil
	})
	rt.defMethod(proto, "trimStart", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.TrimLeftFunc(string(b), jsStrWhitespace)), nil
	})
	rt.defMethod(proto, "trimEnd", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.TrimRightFunc(string(b), jsStrWhitespace)), nil
	})

	// String constructor.
	ctor := rt.newNativeFunc("String", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Whether this is `new String(x)` is decided by new.target, NOT by `this`
		// being an object: a plain call through a property (`globalThis.String(5)`,
		// or a String from another realm) passes an object `this` too.
		constructing := rt.constructing() && rt.objPtr(this) != nil
		var sv Value
		switch {
		case len(args) == 0:
			sv = rt.internString("")
		case args[0].IsSymbol() && !constructing:
			// String(symbol) is the one legal Symbol->string coercion (its
			// description); new String(symbol) is a TypeError.
			d := rt.symbolDesc(args[0])
			ds := ""
			if d.IsString() {
				ds = rt.strGo(d)
			}
			sv = rt.newString("Symbol(" + ds + ")")
		default:
			s, e := rt.toStringValue(args[0])
			if e != nil {
				return mkundef(), e
			}
			sv = s
		}
		if constructing {
			// new String(x): a String exotic object wrapping the primitive, with a
			// non-writable, non-enumerable length own property.
			o := rt.objPtr(this)
			o.boxed = sv
			o.defineOwn("length", mknum(float64(rt.strLen16(sv))), 0)
			return this, nil
		}
		return sv, nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.stringProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	// String.prototype[Symbol.iterator] iterates by code point (astral-aware).
	if rt.symIterator != 0 {
		strIter := rt.newNativeFunc("[Symbol.iterator]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if this.IsNullish() { // RequireObjectCoercible
				return mkundef(), rt.typeError("String.prototype[Symbol.iterator] called on null or undefined")
			}
			s, e := rt.toStringValue(this)
			if e != nil {
				return mkundef(), e
			}
			vals, _ := rt.iterableValues(s)
			return rt.newStringIterator(vals), nil
		})
		proto.defineOwnSymbol(rt.symIterator.handle(), strIter, attrWritable|attrConfigurable)
	}
	rt.defMethod(proto, "normalize", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, b, e := rt.thisString(this)
		if e != nil {
			return mkundef(), e
		}
		form := "NFC"
		if f := arg(args, 0); !f.IsUndefined() {
			fv, e := rt.toStringValue(f)
			if e != nil {
				return mkundef(), e
			}
			form = rt.strGo(fv)
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
			// Get(raw, i) then ToString: a Symbol segment (or one whose toString
			// throws) is an abrupt completion that must propagate.
			seg, e := rt.getElement(rawV, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			ss, e := rt.toStringValue(seg)
			if e != nil {
				return mkundef(), e
			}
			b = append(b, rt.strBytes(ss)...)
			if i+1 < n && i+1 < len(args) {
				sub, e := rt.toStringValue(args[i+1])
				if e != nil {
					return mkundef(), e
				}
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
			// Each argument must be a code point: a non-negative integer ≤ 0x10FFFF.
			if math.IsNaN(n) || n != math.Trunc(n) || n < 0 || n > 0x10FFFF {
				return mkundef(), rt.rangeError("Invalid code point " + numberToString(n))
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

// posArgE coerces a char-accessor position via ToIntegerOrInfinity, propagating
// an abrupt coercion. A negative position returns -1 and an infinite/huge one
// saturates, so both fall outside any string's range (the caller returns the
// out-of-range result).
func (rt *Runtime) posArgE(args []Value, i int) (int, *ThrowError) {
	f, e := rt.toIntegerOrInfinity(arg(args, i))
	if e != nil {
		return 0, e
	}
	if f < 0 {
		return -1, nil
	}
	if f > 0x7FFFFFFF {
		return 0x7FFFFFFF, nil
	}
	return int(f), nil
}

// strClampPos coerces a String-method position argument via ToIntegerOrInfinity
// and clamps it to [0, n] (a UTF-16 index), propagating an abrupt coercion.
func (rt *Runtime) strClampPos(v Value, n int) (int, *ThrowError) {
	f, e := rt.toIntegerOrInfinity(v)
	if e != nil {
		return 0, e
	}
	if f <= 0 {
		return 0, nil
	}
	if f >= float64(n) {
		return n, nil
	}
	return int(f), nil
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
// wtf8WellFormed reports whether the WTF-8 bytes b contain no unpaired UTF-16
// surrogate: every high surrogate is immediately followed by a low surrogate and
// there is no lone low surrogate. A concatenation-formed pair (stored as two
// lone-surrogate encodings) counts as well-formed.
func wtf8WellFormed(b []byte) bool {
	i := 0
	for i < len(b) {
		slen, _, cp := wtf8Decode(b, i)
		switch {
		case cp >= 0xD800 && cp <= 0xDBFF: // high surrogate: require a following low
			ni := i + slen
			if ni >= len(b) {
				return false
			}
			nslen, _, ncp := wtf8Decode(b, ni)
			if ncp < 0xDC00 || ncp > 0xDFFF {
				return false
			}
			i = ni + nslen
		case cp >= 0xDC00 && cp <= 0xDFFF: // lone low surrogate
			return false
		default:
			i += slen
		}
	}
	return true
}

// utf16TruncateUnits returns the first n UTF-16 code units of b (WTF-8). If the
// boundary at n falls inside a surrogate pair (an astral code point), the pair
// is split and only its lone high surrogate is emitted — String padding truncates
// the filler to an exact UTF-16 length, which can bisect a surrogate pair.
func utf16TruncateUnits(b []byte, n int) []byte {
	out := make([]byte, 0, len(b))
	pos, i := 0, 0
	for i < len(b) && pos < n {
		slen, units, cp := wtf8Decode(b, i)
		if pos+units <= n {
			out = append(out, b[i:i+slen]...)
			pos += units
			i += slen
			continue
		}
		out = wtf8Encode(out, 0xD800+((cp-0x10000)>>10)) // lone high surrogate
		pos++
	}
	return out
}

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
