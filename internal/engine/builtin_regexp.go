package engine

// RegExp constructor + RegExp.prototype (ant modules/regex.c + builtin_regexp),
// backed by internal/regexpjs. Also the String↔RegExp methods match/replace/
// search/split.

import (
	"fmt"
	"strings"
	"unicode"

	"goant/internal/regexpjs"
)

// regExpEscape implements RegExp.escape's EncodeForRegExpEscape (ES2025): the
// first character, if an ASCII letter or digit, is hex-escaped so the result can
// never begin an identifier; syntax characters get a backslash; whitespace and a
// fixed punctuator set become \xHH / \uHHHH; everything else passes through.
func regExpEscape(s string) string {
	const syntax = "^$\\.*+?()[]{}|/"
	const punct = ",-=<>#&!%:;@~'`\"" + " "
	var b strings.Builder
	first := true
	for _, c := range s {
		if first && ((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			fmt.Fprintf(&b, "\\x%02x", c)
			first = false
			continue
		}
		first = false
		switch {
		case strings.ContainsRune(syntax, c):
			b.WriteByte('\\')
			b.WriteRune(c)
		case c == '\t':
			b.WriteString("\\t")
		case c == '\n':
			b.WriteString("\\n")
		case c == '\v':
			b.WriteString("\\v")
		case c == '\f':
			b.WriteString("\\f")
		case c == '\r':
			b.WriteString("\\r")
		case strings.ContainsRune(punct, c) || c == 0xA0 || c == 0xFEFF ||
			c == 0x2028 || c == 0x2029 || unicode.IsSpace(c):
			if c <= 0xFF {
				fmt.Fprintf(&b, "\\x%02x", c)
			} else {
				fmt.Fprintf(&b, "\\u%04x", c)
			}
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func (rt *Runtime) initRegExpBuiltin() {
	proto := rt.newObject(rt.objectProto)
	rt.regexpProto = proto
	po := rt.objPtr(proto)

	rt.defMethod(po, "exec", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.regexpExec(this, arg(args, 0))
	})
	rt.defMethod(po, "test", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("RegExp.prototype.test called on non-object")
		}
		s, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		res, e := rt.regExpExecAbstract(this, s)
		if e != nil {
			return mkundef(), e
		}
		return mkbool(!res.IsNull()), nil
	})
	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Generic: `/${this.source}/${this.flags}` (works on any object).
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("RegExp.prototype.toString called on non-object")
		}
		sv, e := rt.getField(this, "source")
		if e != nil {
			return mkundef(), e
		}
		src, _ := rt.toStringValue(sv)
		fv, e := rt.getField(this, "flags")
		if e != nil {
			return mkundef(), e
		}
		flags, _ := rt.toStringValue(fv)
		return rt.newString("/" + string(rt.strBytes(src)) + "/" + string(rt.strBytes(flags))), nil
	})

	var ctor Value
	ctor = rt.newNativeFunc("RegExp", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		p := arg(args, 0)
		flagsArg := arg(args, 1)
		patternIsRegExp, e := rt.isRegExp(p)
		if e != nil {
			return mkundef(), e
		}
		// RegExp(re) with no new.target, a regexp pattern, no flags override, and
		// pattern.constructor === RegExp returns pattern unchanged. The constructor
		// [[Get]] is observable (e.g. through a Proxy).
		if !rt.constructing() && patternIsRegExp && flagsArg.IsUndefined() {
			pc, e := rt.getField(p, "constructor")
			if e != nil {
				return mkundef(), e
			}
			if pc == ctor {
				return p, nil
			}
		}
		pattern := ""
		flags := ""
		if o := rt.objPtr(p); o != nil && o.regex != nil {
			// A native RegExp: use its compiled source/flags directly.
			pattern = o.regex.Source
			flags = o.regex.Flags
		} else if patternIsRegExp {
			// A RegExp-like object (or Proxy): read source (and flags, unless
			// overridden) via [[Get]] rather than coercing the object to a string.
			srcV, e := rt.getField(p, "source")
			if e != nil {
				return mkundef(), e
			}
			if !srcV.IsUndefined() { // RegExpInitialize: undefined -> ""
				sv, e := rt.toStringValue(srcV)
				if e != nil {
					return mkundef(), e
				}
				pattern = string(rt.strBytes(sv))
			}
			if flagsArg.IsUndefined() {
				fV, e := rt.getField(p, "flags")
				if e != nil {
					return mkundef(), e
				}
				if !fV.IsUndefined() {
					fv, e := rt.toStringValue(fV)
					if e != nil {
						return mkundef(), e
					}
					flags = string(rt.strBytes(fv))
				}
			}
		} else if !p.IsUndefined() {
			s, e := rt.toStringValue(p)
			if e != nil {
				return mkundef(), e
			}
			pattern = string(rt.strBytes(s))
		}
		if !flagsArg.IsUndefined() {
			s, e := rt.toStringValue(flagsArg)
			if e != nil {
				return mkundef(), e
			}
			flags = string(rt.strBytes(s))
		}
		rv, e := rt.newRegExp(pattern, flags)
		if e != nil {
			return mkundef(), e
		}
		if o := rt.objPtr(rv); o != nil { // honor new.target (subclassing)
			o.proto = rt.newTargetProto(rt.regexpProto)
		}
		return rv, nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	// RegExp.escape(S): escape a string for literal use in a pattern (ES2025).
	rt.defMethod(cobj, "escape", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if arg(args, 0).Type() != TStr {
			return mkundef(), rt.typeError("RegExp.escape argument must be a string")
		}
		return rt.newString(regExpEscape(string(rt.strBytes(arg(args, 0))))), nil
	})
	// Annex B legacy static RegExp properties: accessor properties on the RegExp
	// constructor, non-enumerable + configurable, whose getter/setter throw a
	// TypeError unless the receiver is %RegExp% itself. `input`/`$_` also has a
	// setter; the rest (lastMatch, lastParen, leftContext, rightContext, $1…$9)
	// are get-only.
	legacyGet := func(get func() string) Value {
		return rt.newNativeFunc("get", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if this != rt.regexpCtor {
				return mkundef(), rt.typeError("RegExp legacy static property getter requires the RegExp constructor as receiver")
			}
			return rt.newString(get()), nil
		})
	}
	defLegacyGet := func(names []string, get func() string) {
		g := legacyGet(get)
		for _, n := range names {
			cobj.defineAccessor(n, g, mkundef(), true, false, attrConfigurable)
		}
	}
	inputGet := legacyGet(func() string { return rt.regexpInput })
	inputSet := rt.newNativeFunc("set", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this != rt.regexpCtor {
			return mkundef(), rt.typeError("RegExp legacy static property setter requires the RegExp constructor as receiver")
		}
		s, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		rt.regexpInput = string(rt.strBytes(s))
		return mkundef(), nil
	})
	for _, n := range []string{"input", "$_"} {
		cobj.defineAccessor(n, inputGet, inputSet, true, true, attrConfigurable)
	}
	defLegacyGet([]string{"lastMatch", "$&"}, func() string { return rt.regexpLastMatch })
	defLegacyGet([]string{"lastParen", "$+"}, func() string { return rt.regexpLastParen })
	defLegacyGet([]string{"leftContext", "$`"}, func() string { return rt.regexpLeftContext })
	defLegacyGet([]string{"rightContext", "$'"}, func() string { return rt.regexpRightContext })
	for i := 1; i <= 9; i++ {
		idx := i - 1
		cobj.defineAccessor("$"+itoaSmall(i), legacyGet(func() string { return rt.regexpParen[idx] }), mkundef(), true, false, attrConfigurable)
	}
	rt.defSpeciesGetter(ctor)
	rt.regexpCtor = ctor
	rt.defGlobal("RegExp", ctor)

	rt.initRegExpAccessors()
	rt.initStringRegexpMethods()

	// RegExp.prototype[Symbol.match/replace/search/split] delegate to the String
	// operations with `this` as the pattern (so str.match(regex) works via them).
	defSym := func(sym Value, length int, run func(this Value, args []Value) (Value, *ThrowError)) {
		if sym == 0 {
			return
		}
		// A well-known-symbol method's name is "[Symbol.<desc>]" (its description is
		// e.g. "Symbol.replace").
		name := ""
		if d := rt.symbolDesc(sym); d.IsString() {
			name = "[" + string(rt.strBytes(d)) + "]"
		}
		fn := rt.newNativeFunc(name, length, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return run(this, args)
		})
		po.defineOwnSymbol(sym.handle(), fn, attrWritable|attrConfigurable)
	}
	defSym(rt.symMatch, 1, func(this Value, args []Value) (Value, *ThrowError) {
		// Generic RegExp.prototype[@@match] (22.2.6.8): reads global/unicode and
		// runs RegExpExec, all via [[Get]] so it composes with Proxy traps.
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("RegExp.prototype[Symbol.match] called on non-object")
		}
		s, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		// ES2024: read "flags" once (ToString) and derive g / u|v from the string,
		// rather than reading "global"/"unicode" separately.
		flagsStr, e := rt.regExpFlagsString(this)
		if e != nil {
			return mkundef(), e
		}
		if !flagsContain(flagsStr, 'g') {
			return rt.regExpExecAbstract(this, s)
		}
		fullUnicode := flagsContain(flagsStr, 'u') || flagsContain(flagsStr, 'v')
		if e := rt.setLastIndexOrThrow(this, 0); e != nil {
			return mkundef(), e
		}
		res := rt.newArray()
		ro := rt.objPtr(res)
		n := 0
		for {
			result, e := rt.regExpExecAbstract(this, s)
			if e != nil {
				return mkundef(), e
			}
			if result.IsNull() {
				if n == 0 {
					return mknull(), nil
				}
				return res, nil
			}
			m0, e := rt.getField(result, "0")
			if e != nil {
				return mkundef(), e
			}
			matchStr, e := rt.toStringValue(m0)
			if e != nil {
				return mkundef(), e
			}
			rt.arraySet(ro, ro.arrLen, matchStr)
			n++
			if len(rt.strBytes(matchStr)) == 0 {
				li, e := rt.getField(this, "lastIndex")
				if e != nil {
					return mkundef(), e
				}
				liN, e := rt.toIntegerOrInfinity(li)
				if e != nil {
					return mkundef(), e
				}
				if e := rt.setLastIndexOrThrow(this, rt.advanceStringIndex(s, liN, fullUnicode)); e != nil {
					return mkundef(), e
				}
			}
		}
	})
	defSym(rt.symReplace, 2, func(this Value, args []Value) (Value, *ThrowError) {
		// Always run the generic exec-driven algorithm: it reads the exec result's
		// length/0/index/N/groups observably (through a subclass's overridden exec
		// and each property's coercion), which a fast native-substring path skips.
		return rt.regexpSymbolReplace(this, arg(args, 0), arg(args, 1))
	})
	defSym(rt.symSearch, 1, func(this Value, args []Value) (Value, *ThrowError) {
		// Generic RegExp.prototype[@@search] (22.2.6.11): saves/restores lastIndex
		// around a single RegExpExec, all via [[Get]]/[[Set]].
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("RegExp.prototype[Symbol.search] called on non-object")
		}
		s, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		prevLI, e := rt.getField(this, "lastIndex")
		if e != nil {
			return mkundef(), e
		}
		if !rt.sameValue(prevLI, mknum(0)) { // SameValue(previousLastIndex, +0)
			if e := rt.setFieldThrow(this, "lastIndex", mknum(0)); e != nil {
				return mkundef(), e
			}
		}
		result, e := rt.regExpExecAbstract(this, s)
		if e != nil {
			return mkundef(), e
		}
		curLI, e := rt.getField(this, "lastIndex")
		if e != nil {
			return mkundef(), e
		}
		if !rt.sameValue(curLI, prevLI) {
			if e := rt.setFieldThrow(this, "lastIndex", prevLI); e != nil {
				return mkundef(), e
			}
		}
		if result.IsNull() {
			return mknum(-1), nil
		}
		return rt.getField(result, "index")
	})
	defSym(rt.symSplit, 2, func(this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() || !this.IsObjectType() {
			return mkundef(), rt.typeError("Method RegExp.prototype[Symbol.split] called on incompatible receiver")
		}
		// Fast path: an ordinary RegExp whose @@species is the default RegExp ctor.
		if re := rt.objPtr(this); re != nil && re.regex != nil {
			if C, e := rt.speciesConstructor(this, rt.regexpCtor); e == nil && C == rt.regexpCtor {
				return rt.stringSplitRegexp(arg(args, 0), re.regex, arg(args, 1))
			}
		}
		// Generic path: SpeciesConstructor(this, %RegExp%) supplies the splitter.
		C, e := rt.speciesConstructor(this, rt.regexpCtor)
		if e != nil {
			return mkundef(), e
		}
		flagsV, e := rt.getField(this, "flags")
		if e != nil {
			return mkundef(), e
		}
		flagsS, e := rt.toStringValue(flagsV)
		if e != nil {
			return mkundef(), e
		}
		newFlags := string(rt.strBytes(flagsS))
		unicode := strings.Contains(newFlags, "u")
		if !strings.Contains(newFlags, "y") {
			newFlags += "y"
		}
		splitter, e := rt.construct(C, []Value{this, rt.newString(newFlags)})
		if e != nil {
			return mkundef(), e
		}
		if so := rt.objPtr(splitter); so != nil && so.regex != nil {
			return rt.stringSplitRegexp(arg(args, 0), so.regex, arg(args, 1))
		}
		// A non-RegExp splitter: run the fully generic exec-driven algorithm.
		return rt.regexpSymbolSplitGeneric(splitter, arg(args, 0), arg(args, 1), unicode)
	})
	defSym(rt.symMatchAll, 1, func(this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Method RegExp.prototype[Symbol.matchAll] called on incompatible receiver")
		}
		sv, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		// matcher = Construct(SpeciesConstructor(R, %RegExp%), (R, flags)); iterate
		// from its lastIndex.
		C, e := rt.speciesConstructor(this, rt.regexpCtor)
		if e != nil {
			return mkundef(), e
		}
		flagsV, e := rt.getField(this, "flags")
		if e != nil {
			return mkundef(), e
		}
		flagsS, e := rt.toStringValue(flagsV)
		if e != nil {
			return mkundef(), e
		}
		matcher, e := rt.construct(C, []Value{this, flagsS})
		if e != nil {
			return mkundef(), e
		}
		if mo := rt.objPtr(matcher); mo == nil || mo.regex == nil {
			return mkundef(), rt.typeError("RegExp[Symbol.matchAll] species constructor did not return a RegExp")
		}
		li, e := rt.getField(this, "lastIndex")
		if e != nil {
			return mkundef(), e
		}
		liN, e := rt.toLengthClamped(li)
		if e != nil {
			return mkundef(), e
		}
		if e := rt.setField(matcher, "lastIndex", mknum(liN)); e != nil {
			return mkundef(), e
		}
		flagsStr := string(rt.strBytes(flagsS))
		global := strings.ContainsRune(flagsStr, 'g')
		unicode := strings.ContainsRune(flagsStr, 'u') || strings.ContainsRune(flagsStr, 'v')
		return rt.createRegExpStringIterator(matcher, sv, global, unicode), nil
	})
}

// newRegExp compiles a pattern/flags pair into a RegExp object.
func (rt *Runtime) newRegExp(pattern, flags string) (Value, *ThrowError) {
	re, err := regexpjs.Compile(pattern, flags)
	if err != nil {
		return mkundef(), &ThrowError{Value: rt.makeError(rt.errors.syntaxProto, "SyntaxError", err.Error()), rt: rt}
	}
	v := rt.newObject(rt.regexpProto)
	o := rt.objPtr(v)
	o.regex = re
	o.setSlot(slotBrand, mknum(brandRegExp))
	// source/flags/global/… are accessor getters on RegExp.prototype (installed
	// in initRegExpAccessors); only lastIndex is an own data property.
	o.defineOwn("lastIndex", mknum(0), attrWritable)
	return v, nil
}

// initRegExpAccessors installs source/flags and the per-flag boolean getters on
// RegExp.prototype (ES 22.2.6). Each reads the receiver's [[OriginalFlags]];
// on the prototype itself they return undefined, and flags is fully generic
// (reads the individual flag getters via [[Get]]).
func (rt *Runtime) initRegExpAccessors() {
	po := rt.objPtr(rt.regexpProto)
	flagGetter := func(name string, ch byte, read func(*regexpjs.Regexp) bool) {
		g := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil {
				return mkundef(), rt.typeError("RegExp.prototype." + name + " getter called on non-object")
			}
			if o.regex == nil {
				if this == rt.regexpProto {
					return mkundef(), nil
				}
				return mkundef(), rt.typeError("RegExp.prototype." + name + " getter called on incompatible receiver")
			}
			if read != nil {
				return mkbool(read(o.regex)), nil
			}
			return mkbool(strings.IndexByte(o.regex.Flags, ch) >= 0), nil
		})
		po.defineAccessor(name, g, mkundef(), true, false, attrConfigurable)
	}
	flagGetter("global", 'g', func(re *regexpjs.Regexp) bool { return re.Global })
	flagGetter("ignoreCase", 'i', func(re *regexpjs.Regexp) bool { return re.IgnoreCase })
	flagGetter("multiline", 'm', func(re *regexpjs.Regexp) bool { return re.Multiline })
	flagGetter("dotAll", 's', func(re *regexpjs.Regexp) bool { return re.DotAll })
	flagGetter("unicode", 'u', func(re *regexpjs.Regexp) bool { return re.Unicode })
	flagGetter("sticky", 'y', func(re *regexpjs.Regexp) bool { return re.Sticky })
	flagGetter("hasIndices", 'd', nil)
	flagGetter("unicodeSets", 'v', nil)

	srcGetter := rt.newNativeFunc("get source", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("RegExp.prototype.source getter called on non-object")
		}
		if o.regex == nil {
			if this == rt.regexpProto {
				return rt.internString("(?:)"), nil
			}
			return mkundef(), rt.typeError("RegExp.prototype.source getter called on incompatible receiver")
		}
		return rt.internString(nonEmptySource(o.regex.Source)), nil
	})
	po.defineAccessor("source", srcGetter, mkundef(), true, false, attrConfigurable)

	// flags is generic: it reads each flag getter through [[Get]] and concatenates
	// the letters, so it works on any object (and observes Proxy traps).
	order := []struct {
		name string
		ch   byte
	}{{"hasIndices", 'd'}, {"global", 'g'}, {"ignoreCase", 'i'}, {"multiline", 'm'}, {"dotAll", 's'}, {"unicode", 'u'}, {"unicodeSets", 'v'}, {"sticky", 'y'}}
	flagsGetter := rt.newNativeFunc("get flags", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("RegExp.prototype.flags getter called on non-object")
		}
		var b []byte
		for _, f := range order {
			v, e := rt.getField(this, f.name)
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(v) {
				b = append(b, f.ch)
			}
		}
		return rt.internString(string(b)), nil
	})
	po.defineAccessor("flags", flagsGetter, mkundef(), true, false, attrConfigurable)
}

// isRegExp implements IsRegExp (22.2.7.2): an object is a regexp if its @@match
// property is truthy, or (when @@match is undefined) it carries a compiled
// regex internal.
func (rt *Runtime) isRegExp(v Value) (bool, *ThrowError) {
	if !v.IsObjectType() {
		return false, nil
	}
	if rt.symMatch != 0 {
		m, e := rt.getElement(v, rt.symMatch)
		if e != nil {
			return false, e
		}
		if !m.IsUndefined() {
			return rt.toBoolean(m), nil
		}
	}
	o := rt.objPtr(v)
	return o != nil && o.regex != nil, nil
}

// regexpStrIterState is the internal state of a RegExp String iterator
// (%RegExpStringIterator% instance): the matcher R, the subject string S, the
// global/unicode flags, and whether it is exhausted. Held in
// rt.regexpStrIterStates keyed by the iterator object so a shared
// %RegExpStringIteratorPrototype%.next reads it and the brand check works.
type regexpStrIterState struct {
	r       Value
	s       Value
	global  bool
	unicode bool
	done    bool
}

// createRegExpStringIterator implements CreateRegExpStringIterator (22.2.9.1):
// it builds a RegExp String iterator over matcher R and string S. Matches are
// produced lazily (per next() call) via the abstract RegExpExec, so a matcher
// with a user-supplied exec (or a Proxy) is honored.
func (rt *Runtime) createRegExpStringIterator(r, s Value, global, unicode bool) Value {
	v := rt.newObject(rt.regexpStrIterProto)
	if rt.regexpStrIterStates == nil {
		rt.regexpStrIterStates = map[*object]*regexpStrIterState{}
	}
	rt.regexpStrIterStates[rt.objPtr(v)] = &regexpStrIterState{r: r, s: s, global: global, unicode: unicode}
	return v
}

// regexpStrIterNext is the shared %RegExpStringIteratorPrototype%.next: it reads
// the receiver's iteration state (a TypeError if absent — the missing-brand
// check) and yields the next match, driving RegExpExec so custom exec methods
// and lastIndex updates are observable (22.2.9.2.1).
func (rt *Runtime) regexpStrIterNext(this Value) (Value, *ThrowError) {
	if !this.IsObjectType() {
		return mkundef(), rt.typeError("RegExp String Iterator.prototype.next called on a non-object")
	}
	st := rt.regexpStrIterStates[rt.objPtr(this)]
	if st == nil {
		return mkundef(), rt.typeError("RegExp String Iterator.prototype.next called on an incompatible receiver")
	}
	if st.done {
		return rt.genResult(mkundef(), true), nil
	}
	match, e := rt.abstractRegExpExec(st.r, st.s)
	if e != nil {
		return mkundef(), e
	}
	if match.IsNull() {
		st.done = true
		return rt.genResult(mkundef(), true), nil
	}
	if !st.global {
		st.done = true
		return rt.genResult(match, false), nil
	}
	// Global: an empty match must advance the matcher's lastIndex, or the next
	// step would match at the same position forever.
	m0, e := rt.getElement(match, mknum(0))
	if e != nil {
		return mkundef(), e
	}
	matchStr, e := rt.toStringValue(m0)
	if e != nil {
		return mkundef(), e
	}
	if utf16Len(rt.strBytes(matchStr)) == 0 {
		li, e := rt.getField(st.r, "lastIndex")
		if e != nil {
			return mkundef(), e
		}
		thisIndex, e := rt.toLengthClamped(li)
		if e != nil {
			return mkundef(), e
		}
		nextIndex := rt.advanceStringIndex(st.s, thisIndex, st.unicode)
		if e := rt.setField(st.r, "lastIndex", mknum(nextIndex)); e != nil {
			return mkundef(), e
		}
	}
	return rt.genResult(match, false), nil
}

// advanceStringIndex implements AdvanceStringIndex (22.2.7.3): +1, or +2 when
// unicode and the code unit at index is a lead surrogate paired with a trail.
func (rt *Runtime) advanceStringIndex(s Value, index float64, unicode bool) float64 {
	if !unicode {
		return index + 1
	}
	b := rt.strBytes(s)
	i := int(index)
	if i+1 >= utf16Len(b) {
		return index + 1
	}
	hi := utf16CodeUnitAt(b, i)
	lo := utf16CodeUnitAt(b, i+1)
	if hi >= 0xD800 && hi <= 0xDBFF && lo >= 0xDC00 && lo <= 0xDFFF {
		return index + 2
	}
	return index + 1
}

// canonicalFlags reorders regexp flag characters into the ES canonical order
// (d, g, i, m, s, u, v, y), dropping duplicates.
func canonicalFlags(flags string) string {
	var b []byte
	for _, c := range []byte("dgimsuvy") {
		for i := 0; i < len(flags); i++ {
			if flags[i] == c {
				b = append(b, c)
				break
			}
		}
	}
	return string(b)
}

const brandRegExp = 1001

// nonEmptySource implements EscapeRegExpPattern (22.2.3.2.5): an empty pattern
// renders as "(?:)", and every unescaped "/" and line terminator is escaped so
// the result is a valid RegularExpressionLiteral body (/source/flags).
func nonEmptySource(p string) string {
	if p == "" {
		return "(?:)"
	}
	var b strings.Builder
	rs := []rune(p)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case '\\': // keep an escape sequence intact (do not re-escape its operand)
			b.WriteByte('\\')
			if i+1 < len(rs) {
				i++
				b.WriteRune(rs[i])
			}
		case '/':
			b.WriteString("\\/")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case ' ':
			b.WriteString("\\u2028")
		case ' ':
			b.WriteString("\\u2029")
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// regexpExec runs RegExp.prototype.exec, returning a match-result array or null.
// regExpFlagsString performs ToString(? Get(R, "flags")) — the single flags read
// the RegExp Symbol methods use (its getter observes the individual flag props).
func (rt *Runtime) regExpFlagsString(r Value) ([]byte, *ThrowError) {
	fv, e := rt.getField(r, "flags")
	if e != nil {
		return nil, e
	}
	fs, e := rt.toStringValue(fv)
	if e != nil {
		return nil, e
	}
	return rt.strBytes(fs), nil
}

// toLengthClamped implements ToLength: ToIntegerOrInfinity clamped to
// [0, 2^53 - 1].
func (rt *Runtime) toLengthClamped(v Value) (float64, *ThrowError) {
	n, e := rt.toIntegerOrInfinity(v)
	if e != nil {
		return 0, e
	}
	if n <= 0 {
		return 0, nil
	}
	if n > 9007199254740991 {
		return 9007199254740991, nil
	}
	return n, nil
}

// flagsContain reports whether the flags string contains flag character c.
func flagsContain(s []byte, c byte) bool {
	for _, b := range s {
		if b == c {
			return true
		}
	}
	return false
}

// regExpExecAbstract is the spec RegExpExec(R, S) abstract operation: it reads
// R.exec via [[Get]] (routing through a Proxy trap / a user-supplied exec) and
// calls it when callable, otherwise falls back to the builtin exec. S must be
// already coerced to a string by the caller.
func (rt *Runtime) regExpExecAbstract(r, s Value) (Value, *ThrowError) {
	exec, e := rt.getField(r, "exec")
	if e != nil {
		return mkundef(), e
	}
	if rt.isCallable(exec) {
		res, e := rt.callValue(exec, r, []Value{s})
		if e != nil {
			return mkundef(), e
		}
		if !res.IsNull() && !res.IsObjectType() {
			return mkundef(), rt.typeError("exec method returned neither an object nor null")
		}
		return res, nil
	}
	return rt.regexpExec(r, s)
}

// setLastIndexOrThrow performs Set(R, "lastIndex", n, true): a TypeError when
// lastIndex is not writable (a sticky/global exec must Set it with throw=true).
func (rt *Runtime) setLastIndexOrThrow(this Value, n float64) *ThrowError {
	ok, e := rt.setFieldR(this, "lastIndex", mknum(n))
	if e != nil {
		return e
	}
	if !ok {
		return rt.typeError("Cannot assign to read only property 'lastIndex'")
	}
	return nil
}

// updateLegacyRegExpState records the Annex B legacy RegExp static state after a
// successful built-in match: RegExp.input/$_, lastMatch/$&, lastParen/$+,
// leftContext/$`, rightContext/$', and $1…$9. input is the subject as runes and
// m the match (rune offsets).
func (rt *Runtime) updateLegacyRegExpState(input []rune, m *regexpjs.Match) {
	rt.regexpInput = string(input)
	rt.regexpLastMatch = m.Groups[0].Value
	for i := 0; i < 9; i++ {
		if i+1 < len(m.Groups) && m.Groups[i+1].Index >= 0 {
			rt.regexpParen[i] = m.Groups[i+1].Value
		} else {
			rt.regexpParen[i] = ""
		}
	}
	rt.regexpLastParen = ""
	for i := len(m.Groups) - 1; i >= 1; i-- {
		if m.Groups[i].Index >= 0 {
			rt.regexpLastParen = m.Groups[i].Value
			break
		}
	}
	start, end := m.Index, m.Index+m.Groups[0].Length
	if start >= 0 && start <= len(input) {
		rt.regexpLeftContext = string(input[:start])
	}
	if end >= 0 && end <= len(input) {
		rt.regexpRightContext = string(input[end:])
	}
}

func (rt *Runtime) regexpExec(this, strVal Value) (Value, *ThrowError) {
	o := rt.objPtr(this)
	if o == nil || o.regex == nil {
		return mkundef(), rt.typeError("Method RegExp.prototype.exec called on incompatible receiver")
	}
	s, e := rt.toStringValue(strVal)
	if e != nil {
		return mkundef(), e
	}
	input := wtf8ToRunes(rt.strBytes(s))
	re := o.regex

	// lastIndex = ToLength(Get(R, "lastIndex")) — always read (observable via a
	// getter), used as the search start only for a global/sticky regexp.
	liv, e := rt.getField(this, "lastIndex")
	if e != nil {
		return mkundef(), e
	}
	lif, e := rt.toIntegerOrInfinity(liv)
	if e != nil {
		return mkundef(), e
	}
	lastIndex := 0
	switch {
	case lif <= 0:
		lastIndex = 0
	case lif > float64(len(input)):
		lastIndex = len(input) + 1 // out of range -> no match
	default:
		lastIndex = int(lif)
	}
	start := 0
	if re.Global || re.Sticky {
		start = lastIndex
	}
	if start > len(input) {
		if re.Global || re.Sticky {
			if e := rt.setLastIndexOrThrow(this, 0); e != nil {
				return mkundef(), e
			}
		}
		return mknull(), nil
	}
	m, err := re.Exec(input, start)
	if err != nil {
		return mkundef(), rt.typeError("regexp exec error: " + err.Error())
	}
	if m == nil {
		if re.Global || re.Sticky {
			if e := rt.setLastIndexOrThrow(this, 0); e != nil {
				return mkundef(), e
			}
		}
		return mknull(), nil
	}
	if re.Global || re.Sticky {
		if e := rt.setLastIndexOrThrow(this, float64(m.Index+m.Groups[0].Length)); e != nil {
			return mkundef(), e
		}
	}
	if len(m.Groups) > 0 {
		rt.updateLegacyRegExpState(input, m) // RegExp.lastMatch/input/$1…$9 (Annex B)
	}

	res := rt.newArray()
	ro := rt.objPtr(res)
	groups := mkundef()
	for i, g := range m.Groups {
		missing := g.Index < 0 && i > 0
		val := rt.newString(g.Value)
		if missing {
			val = mkundef()
		}
		rt.arraySet(ro, uint32(i), val)
		if g.Name != "" && !allDigits(g.Name) {
			if groups.IsUndefined() {
				groups = rt.newObject(mknull())
			}
			rt.objPtr(groups).defineOwn(g.Name, val, attrDefault)
		}
	}
	ro.defineOwn("index", mknum(float64(m.Index)), attrDefault)
	ro.defineOwn("input", s, attrDefault)
	ro.defineOwn("groups", groups, attrDefault)
	// The `d` (hasIndices) flag adds an `indices` array of [start, end] pairs
	// (undefined for an unmatched group) plus a matching `indices.groups`.
	if strings.Contains(re.Flags, "d") {
		indices := rt.newArray()
		io := rt.objPtr(indices)
		idxGroups := mkundef()
		for i, g := range m.Groups {
			pair := mkundef()
			if g.Index >= 0 {
				p := rt.newArray()
				po := rt.objPtr(p)
				rt.arraySet(po, 0, mknum(float64(g.Index)))
				rt.arraySet(po, 1, mknum(float64(g.Index+g.Length)))
				pair = p
			}
			rt.arraySet(io, uint32(i), pair)
			if g.Name != "" && !allDigits(g.Name) {
				if idxGroups.IsUndefined() {
					idxGroups = rt.newObject(mknull())
				}
				rt.objPtr(idxGroups).defineOwn(g.Name, pair, attrDefault)
			}
		}
		io.defineOwn("groups", idxGroups, attrDefault)
		ro.defineOwn("indices", indices, attrDefault)
	}
	return res, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// initStringRegexpMethods installs match/replace/search/split on String.prototype.
func (rt *Runtime) initStringRegexpMethods() {
	sp := rt.objPtr(rt.stringProto)

	rt.defMethod(sp, "search", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() { // RequireObjectCoercible before delegating to @@search
			return mkundef(), rt.typeError("String.prototype.search called on null or undefined")
		}
		if r, ok, e := rt.delegateSymbolMethod(rt.symSearch, arg(args, 0), this); ok {
			return r, e
		}
		// S = ToString(this); rx = RegExpCreate(regexp, undefined) (regexpArg, which
		// does NOT perform IsRegExp); Invoke(rx, @@search, «S») — so a replaced
		// RegExp.prototype[@@search] is observed.
		sv, e := rt.toStringValue(this)
		if e != nil {
			return mkundef(), e
		}
		rx, e := rt.regexpArg(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		searcher, e := rt.getElement(rx, rt.symSearch)
		if e != nil {
			return mkundef(), e
		}
		return rt.callValue(searcher, rx, []Value{sv})
	})

	rt.defMethod(sp, "match", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() { // RequireObjectCoercible before delegating to @@match
			return mkundef(), rt.typeError("String.prototype.match called on null or undefined")
		}
		if r, ok, e := rt.delegateSymbolMethod(rt.symMatch, arg(args, 0), this); ok {
			return r, e
		}
		// S = ToString(this); rx = RegExpCreate(regexp, undefined) (regexpArg, which
		// does NOT perform IsRegExp); Invoke(rx, @@match, «S») — so a replaced
		// RegExp.prototype[@@match] is observed and the global/non-global logic
		// lives in one place (the @@match method).
		s, e := rt.toStringValue(this)
		if e != nil {
			return mkundef(), e
		}
		rx, e := rt.regexpArg(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		matcher, e := rt.getElement(rx, rt.symMatch)
		if e != nil {
			return mkundef(), e
		}
		return rt.callValue(matcher, rx, []Value{s})
	})

	rt.defMethod(sp, "replace", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() { // RequireObjectCoercible before delegating to @@replace
			return mkundef(), rt.typeError("String.prototype.replace called on null or undefined")
		}
		// GetMethod(searchValue, @@replace) for an Object searchValue; delegate with
		// the raw receiver O (ToString happens inside @@replace).
		if pat := arg(args, 0); pat.IsObjectType() {
			m, e := rt.getElement(pat, rt.symReplace)
			if e != nil {
				return mkundef(), e
			}
			if rt.isCallable(m) {
				return rt.callValue(m, pat, []Value{this, arg(args, 1)})
			}
		}
		return rt.stringReplace(this, arg(args, 0), arg(args, 1))
	})

	rt.defMethod(sp, "matchAll", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() { // RequireObjectCoercible
			return mkundef(), rt.typeError("String.prototype.matchAll called on null or undefined")
		}
		regexp := arg(args, 0)
		if regexp.IsObjectType() {
			// A RegExp argument must be global; then GetMethod(regexp, @@matchAll)
			// and delegate if callable (abrupts propagate).
			isRe, e := rt.isRegExp(regexp)
			if e != nil {
				return mkundef(), e
			}
			if isRe {
				flags, e := rt.getField(regexp, "flags")
				if e != nil {
					return mkundef(), e
				}
				if flags.IsNullish() {
					return mkundef(), rt.typeError("String.prototype.matchAll called with a non-global RegExp argument")
				}
				fs, e := rt.toStringValue(flags)
				if e != nil {
					return mkundef(), e
				}
				if !strings.ContainsRune(string(rt.strBytes(fs)), 'g') {
					return mkundef(), rt.typeError("String.prototype.matchAll called with a non-global RegExp argument")
				}
			}
			matcher, e := rt.getElement(regexp, rt.symMatchAll)
			if e != nil {
				return mkundef(), e
			}
			if rt.isCallable(matcher) {
				sv, e := rt.toStringValue(this)
				if e != nil {
					return mkundef(), e
				}
				return rt.callValue(matcher, regexp, []Value{sv})
			}
		}
		// Default: S = ToString(O), rx = RegExpCreate(regexp, "g"), then
		// Invoke(rx, @@matchAll, « S ») — a normal rx runs RegExp.prototype
		// [@@matchAll], but a removed/overridden trap is observed (not bypassed by
		// iterating directly).
		s, e := rt.toStringValue(this)
		if e != nil {
			return mkundef(), e
		}
		reObj, e := rt.construct(rt.regexpCtor, []Value{regexp, rt.newString("g")})
		if e != nil {
			return mkundef(), e
		}
		matcher, e := rt.getElement(reObj, rt.symMatchAll)
		if e != nil {
			return mkundef(), e
		}
		if !rt.isCallable(matcher) {
			return mkundef(), rt.typeError("rx[Symbol.matchAll] is not a function")
		}
		return rt.callValue(matcher, reObj, []Value{s})
	})

	rt.defMethod(sp, "replaceAll", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() { // RequireObjectCoercible
			return mkundef(), rt.typeError("String.prototype.replaceAll called on null or undefined")
		}
		search := arg(args, 0)
		if search.IsObjectType() {
			// Only an Object searchValue is inspected: a RegExp must be global, then
			// GetMethod(search, @@replace) and, if present, delegate (the observable
			// protocol; abrupts propagate). A primitive skips straight to ToString.
			isRe, e := rt.isRegExp(search)
			if e != nil {
				return mkundef(), e
			}
			if isRe {
				flags, e := rt.getField(search, "flags")
				if e != nil {
					return mkundef(), e
				}
				if flags.IsNullish() {
					return mkundef(), rt.typeError("String.prototype.replaceAll called with a non-global RegExp argument")
				}
				fs, e := rt.toStringValue(flags)
				if e != nil {
					return mkundef(), e
				}
				if !strings.ContainsRune(string(rt.strBytes(fs)), 'g') {
					return mkundef(), rt.typeError("replaceAll must be called with a global RegExp")
				}
			}
			replacer, e := rt.getElement(search, rt.symReplace)
			if e != nil {
				return mkundef(), e
			}
			// GetMethod(searchValue, @@replace): present but not callable is a
			// TypeError; if present, delegate (@@replace receives the original
			// coercible `this` (O), not its ToString).
			if !replacer.IsNullish() {
				if !rt.isCallable(replacer) {
					return mkundef(), rt.typeError("String.prototype.replaceAll: searchValue[@@replace] is not a function")
				}
				return rt.callValue(replacer, search, []Value{this, arg(args, 1)})
			}
		}
		s, e := rt.toStringValue(this)
		if e != nil {
			return mkundef(), e
		}
		ss := string(rt.strBytes(s))
		se, e := rt.toStringValue(search)
		if e != nil {
			return mkundef(), e
		}
		needle := string(rt.strBytes(se))
		repl := arg(args, 1)
		callable := rt.isCallable(repl)
		var replStr string
		if !callable {
			rs, e := rt.toStringValue(repl)
			if e != nil {
				return mkundef(), e
			}
			replStr = string(rt.strBytes(rs))
		}
		inputRunes := []rune(ss)
		// Enumerate every (non-overlapping) match position of needle in ss; an empty
		// needle matches at every code-unit boundary (including the end).
		var b strings.Builder
		pos := 0
		advance := len(needle)
		if advance == 0 {
			advance = 1
		}
		for pos <= len(ss) {
			var at int
			if needle == "" {
				at = pos
			} else {
				idx := strings.Index(ss[pos:], needle)
				if idx < 0 {
					break
				}
				at = pos + idx
			}
			b.WriteString(ss[pos:at])
			utf16pos := byteOffsetToUtf16([]byte(ss), at)
			var rep string
			if callable {
				rv, e := rt.callValue(repl, mkundef(), []Value{rt.newString(needle), mknum(float64(utf16pos)), s})
				if e != nil {
					return mkundef(), e
				}
				rvs, e := rt.toStringValue(rv)
				if e != nil {
					return mkundef(), e
				}
				rep = string(rt.strBytes(rvs))
			} else {
				rep, _ = rt.expandReplacement(replStr, needle, utf16pos, inputRunes, []regexpjs.Group{{Index: utf16pos, Value: needle}}, false, nil)
			}
			b.WriteString(rep)
			// Emit the skipped code unit for an empty-needle step, then advance.
			if needle == "" {
				if at < len(ss) {
					j := at + 1 // advance one UTF-8 rune (skip continuation bytes)
					for j < len(ss) && ss[j]&0xC0 == 0x80 {
						j++
					}
					b.WriteString(ss[at:j])
					pos = j
				} else {
					pos = at + 1
				}
			} else {
				pos = at + advance
			}
		}
		if pos <= len(ss) {
			b.WriteString(ss[pos:])
		}
		return rt.newString(b.String()), nil
	})

	rt.defMethod(sp, "split", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() { // RequireObjectCoercible
			return mkundef(), rt.typeError("String.prototype.split called on null or undefined")
		}
		// GetMethod(separator, @@split) for an Object separator; delegate with the
		// raw receiver O (NOT ToString(this) — ToString happens inside @@split).
		sep := arg(args, 0)
		if sep.IsObjectType() {
			m, e := rt.getElement(sep, rt.symSplit)
			if e != nil {
				return mkundef(), e
			}
			if rt.isCallable(m) {
				return rt.callValue(m, sep, []Value{this, arg(args, 1)})
			}
		}
		if o := rt.objPtr(sep); o != nil && o.regex != nil {
			return rt.stringSplitRegexp(this, o.regex, arg(args, 1))
		}
		return rt.stringSplitString(this, args)
	})
}

// symOrZero returns a symbol's handle, or a never-matching sentinel when the
// well-known symbol hasn't been initialized.
func symOrZero(v Value) uint32 {
	if v == 0 {
		return 0xFFFFFFFF
	}
	return v.handle()
}

// delegateSymbolMethod checks whether pattern carries the well-known symbol
// method (Symbol.match/replace/search/split); if so it invokes pattern[sym](str)
// and returns (result, true). Real RegExp objects don't define these, so they
// fall through to the built-in path.
func (rt *Runtime) delegateSymbolMethod(sym, pattern, str Value) (Value, bool, *ThrowError) {
	// Only an Object pattern is inspected (a primitive skips the @@method lookup);
	// the well-known symbol is read via [[Get]] so a Proxy trap observes it, and
	// the method receives the raw receiver O (ToString happens inside @@method).
	if sym == 0 || !pattern.IsObjectType() {
		return mkundef(), false, nil
	}
	m, e := rt.getElement(pattern, sym)
	if e != nil {
		return mkundef(), true, e
	}
	if !rt.isCallable(m) {
		return mkundef(), false, nil
	}
	r, e := rt.callValue(m, pattern, []Value{str})
	return r, true, e
}

// coerceRegExp turns a value into a compiled regex (compiling a string pattern).
func (rt *Runtime) coerceRegExp(v Value) (*regexpjs.Regexp, *ThrowError) {
	if o := rt.objPtr(v); o != nil && o.regex != nil {
		return o.regex, nil
	}
	pat := ""
	if !v.IsUndefined() {
		s, e := rt.toStringValue(v)
		if e != nil {
			return nil, e
		}
		pat = string(rt.strBytes(s))
	}
	re, err := regexpjs.Compile(pat, "")
	if err != nil {
		return nil, rt.typeError(err.Error())
	}
	return re, nil
}

// regexpArg returns a RegExp object (wrapping a string pattern if needed).
func (rt *Runtime) regexpArg(v Value) (Value, *ThrowError) {
	if o := rt.objPtr(v); o != nil && o.regex != nil {
		return v, nil
	}
	pat := ""
	if !v.IsUndefined() {
		s, e := rt.toStringValue(v)
		if e != nil {
			return mkundef(), e
		}
		pat = string(rt.strBytes(s))
	}
	return rt.newRegExp(pat, "")
}

// stringReplace implements String.prototype.replace for string and regex
// patterns with string or function replacements.
// abstractRegExpExec implements RegExpExec(R, S): call R.exec if it is callable
// (routing through [[Get]] so a Proxy trap observes it), else the builtin exec.
func (rt *Runtime) abstractRegExpExec(rx, s Value) (Value, *ThrowError) {
	exec, e := rt.getField(rx, "exec")
	if e != nil {
		return mkundef(), e
	}
	if rt.isCallable(exec) {
		r, e := rt.callValue(exec, rx, []Value{s})
		if e != nil {
			return mkundef(), e
		}
		if !r.IsNull() && !r.IsObjectType() {
			return mkundef(), rt.typeError("RegExp exec method returned something other than an Object or null")
		}
		return r, nil
	}
	return rt.regexpExec(rx, s)
}

// regexpSymbolReplace is the generic RegExp.prototype[@@replace] for a non-native
// receiver (RegExp-like objects and Proxies): it drives matches through the
// abstract RegExpExec so every property access (global, unicode, exec, …) is
// observable, then builds the replacement.
func (rt *Runtime) regexpSymbolReplace(rx, strVal, repl Value) (Value, *ThrowError) {
	if !rx.IsObjectType() {
		return mkundef(), rt.typeError("RegExp.prototype[Symbol.replace] called on incompatible receiver")
	}
	sV, e := rt.toStringValue(strVal)
	if e != nil {
		return mkundef(), e
	}
	Srunes := wtf8ToRunes(rt.strBytes(sV))
	functional := rt.isCallable(repl)
	replStr := ""
	if !functional {
		rs, e := rt.toStringValue(repl)
		if e != nil {
			return mkundef(), e
		}
		replStr = string(rt.strBytes(rs))
	}
	// flags = ToString(Get(rx, "flags")); global/fullUnicode derive from it (the
	// native flags getter itself reads the individual flag getters).
	flagsV, e := rt.getField(rx, "flags")
	if e != nil {
		return mkundef(), e
	}
	flagsS, e := rt.toStringValue(flagsV)
	if e != nil {
		return mkundef(), e
	}
	flags := string(rt.strBytes(flagsS))
	global := strings.ContainsRune(flags, 'g')
	fullUnicode := strings.ContainsRune(flags, 'u') || strings.ContainsRune(flags, 'v')
	if global {
		if e := rt.setLastIndexOrThrow(rx, 0); e != nil {
			return mkundef(), e
		}
	}
	var results []Value
	for {
		result, e := rt.abstractRegExpExec(rx, sV)
		if e != nil {
			return mkundef(), e
		}
		if result.IsNull() {
			break
		}
		results = append(results, result)
		if !global {
			break
		}
		m0, e := rt.getElement(result, mknum(0))
		if e != nil {
			return mkundef(), e
		}
		ms, e := rt.toStringValue(m0)
		if e != nil {
			return mkundef(), e
		}
		if len(rt.strBytes(ms)) == 0 {
			li, e := rt.getField(rx, "lastIndex")
			if e != nil {
				return mkundef(), e
			}
			liN, e := rt.toLengthClamped(li) // ToLength: clamp to [0, 2^53-1]
			if e != nil {
				return mkundef(), e
			}
			if e := rt.setFieldThrow(rx, "lastIndex", mknum(rt.advanceStringIndex(sV, liN, fullUnicode))); e != nil {
				return mkundef(), e
			}
		}
	}
	var out strings.Builder
	nextPos := 0
	for _, result := range results {
		lenV, e := rt.getField(result, "length")
		if e != nil {
			return mkundef(), e
		}
		lnF, e := rt.toIntegerOrInfinity(lenV) // ToLength
		if e != nil {
			return mkundef(), e
		}
		nCaptures := 0
		if lnF > 1 {
			if lnF > 1<<31 {
				nCaptures = 1<<31 - 1
			} else {
				nCaptures = int(lnF) - 1
			}
		}
		m0, e := rt.getElement(result, mknum(0))
		if e != nil {
			return mkundef(), e
		}
		matchedV, e := rt.toStringValue(m0)
		if e != nil {
			return mkundef(), e
		}
		matched := string(rt.strBytes(matchedV))
		idxV, e := rt.getField(result, "index")
		if e != nil {
			return mkundef(), e
		}
		pos, e := rt.toIntegerOrInfinity(idxV)
		if e != nil {
			return mkundef(), e
		}
		position := min(max(int(pos), 0), len(Srunes))
		caps := make([]Value, nCaptures)
		groups := make([]regexpjs.Group, nCaptures+1)
		groups[0] = regexpjs.Group{Index: position, Length: len([]rune(matched)), Value: matched}
		for i := 1; i <= nCaptures; i++ {
			cv, e := rt.getElement(result, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			if cv.IsUndefined() {
				caps[i-1] = mkundef()
				groups[i] = regexpjs.Group{Index: -1}
			} else {
				cs, e := rt.toStringValue(cv)
				if e != nil {
					return mkundef(), e
				}
				caps[i-1] = cs
				groups[i] = regexpjs.Group{Index: 0, Value: string(rt.strBytes(cs))}
			}
		}
		var replacement string
		if functional {
			callArgs := make([]Value, 0, nCaptures+4)
			callArgs = append(callArgs, matchedV)
			callArgs = append(callArgs, caps...)
			callArgs = append(callArgs, mknum(float64(position)), sV)
			// When the match has named captures, the groups object is passed as the
			// final argument to the replacer function.
			if gv, _ := rt.getField(result, "groups"); !gv.IsUndefined() {
				callArgs = append(callArgs, gv)
			}
			rv, e := rt.callValue(repl, mkundef(), callArgs)
			if e != nil {
				return mkundef(), e
			}
			rs, e := rt.toStringValue(rv)
			if e != nil {
				return mkundef(), e
			}
			replacement = string(rt.strBytes(rs))
		} else {
			// namedCaptures = ? Get(result, "groups"); if not undefined it is
			// ToObject'd, and each $<name> resolves via ? Get(groups, name) then
			// ? ToString — read lazily so the abrupt of only an accessed name shows.
			gv, e := rt.getField(result, "groups")
			if e != nil {
				return mkundef(), e
			}
			hasNamed := !gv.IsUndefined()
			lookup := func(name string) (string, *ThrowError) {
				v, e := rt.getField(gv, name)
				if e != nil {
					return "", e
				}
				if v.IsUndefined() {
					return "", nil
				}
				sv, e := rt.toStringValue(v)
				if e != nil {
					return "", e
				}
				return string(rt.strBytes(sv)), nil
			}
			replacement, e = rt.expandReplacement(replStr, matched, position, Srunes, groups, hasNamed, lookup)
			if e != nil {
				return mkundef(), e
			}
		}
		if position >= nextPos {
			out.WriteString(string(Srunes[nextPos:position]))
			out.WriteString(replacement)
			nextPos = position + len([]rune(matched))
		}
	}
	if nextPos < len(Srunes) {
		out.WriteString(string(Srunes[nextPos:]))
	}
	return rt.newString(out.String()), nil
}

// regexpSymbolSplitGeneric is the fully generic RegExp.prototype[@@split] path
// (22.2.6.14) for a splitter that is not a native RegExp: it drives matches via
// the abstract RegExpExec so every access is observable.
func (rt *Runtime) regexpSymbolSplitGeneric(splitter, strVal, limitV Value, unicode bool) (Value, *ThrowError) {
	sV, e := rt.toStringValue(strVal)
	if e != nil {
		return mkundef(), e
	}
	S := wtf8ToRunes(rt.strBytes(sV))
	res := rt.newArray()
	ro := rt.objPtr(res)
	lim := int64(1)<<32 - 1
	if !limitV.IsUndefined() {
		ln, e := rt.toNumber(limitV) // ToUint32, propagating a throwing valueOf
		if e != nil {
			return mkundef(), e
		}
		lim = int64(toUint32(ln))
	}
	if lim == 0 {
		return res, nil
	}
	pushSeg := func(v Value) { rt.arraySet(ro, ro.arrLen, v) }
	if len(S) == 0 {
		z, e := rt.abstractRegExpExec(splitter, sV)
		if e != nil {
			return mkundef(), e
		}
		if z.IsNull() {
			pushSeg(sV)
		}
		return res, nil
	}
	p := 0
	q := 0
	for q < len(S) {
		if e := rt.setFieldThrow(splitter, "lastIndex", mknum(float64(q))); e != nil {
			return mkundef(), e
		}
		z, e := rt.abstractRegExpExec(splitter, sV)
		if e != nil {
			return mkundef(), e
		}
		if z.IsNull() {
			q = int(rt.advanceStringIndex(sV, float64(q), unicode))
			continue
		}
		liV, e := rt.getField(splitter, "lastIndex")
		if e != nil {
			return mkundef(), e
		}
		liN, e := rt.toIntegerOrInfinity(liV) // ToLength
		if e != nil {
			return mkundef(), e
		}
		end := min(int(liN), len(S))
		if end == p {
			q = int(rt.advanceStringIndex(sV, float64(q), unicode))
			continue
		}
		pushSeg(rt.newString(string(S[p:q])))
		if int64(ro.arrLen) == lim {
			return res, nil
		}
		lenV, e := rt.getField(z, "length")
		if e != nil {
			return mkundef(), e
		}
		lnF, e := rt.toIntegerOrInfinity(lenV) // ToLength
		if e != nil {
			return mkundef(), e
		}
		nCaps := 0
		if lnF > 1 {
			if lnF > 1<<31 {
				nCaps = 1<<31 - 1
			} else {
				nCaps = int(lnF) - 1
			}
		}
		for i := 1; i <= nCaps; i++ {
			cv, e := rt.getElement(z, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			pushSeg(cv)
			if int64(ro.arrLen) == lim {
				return res, nil
			}
		}
		p = end
		q = p
	}
	pushSeg(rt.newString(string(S[p:])))
	return res, nil
}

func (rt *Runtime) stringReplace(this, pattern, repl Value) (Value, *ThrowError) {
	s, e := rt.toStringValue(this)
	if e != nil {
		return mkundef(), e
	}
	subject := string(rt.strBytes(s))

	replaceOne := func(match string, groups []regexpjs.Group, index int) (string, *ThrowError) {
		if rt.isCallable(repl) {
			callArgs := make([]Value, 0, len(groups)+2)
			for _, g := range groups {
				if g.Index < 0 {
					callArgs = append(callArgs, mkundef())
				} else {
					callArgs = append(callArgs, rt.newString(g.Value))
				}
			}
			callArgs = append(callArgs, mknum(float64(index)), s)
			rv, terr := rt.callValue(repl, mkundef(), callArgs)
			if terr != nil {
				return "", terr
			}
			rs, terr := rt.toStringValue(rv)
			if terr != nil {
				return "", terr
			}
			return string(rt.strBytes(rs)), nil
		}
		rs, terr := rt.toStringValue(repl)
		if terr != nil {
			return "", terr
		}
		var named map[string]string
		for _, g := range groups {
			if g.Name != "" && !allDigits(g.Name) {
				if named == nil {
					named = map[string]string{}
				}
				if g.Index >= 0 {
					named[g.Name] = g.Value
				} else {
					named[g.Name] = ""
				}
			}
		}
		hasNamed, lookup := namedFromMap(named)
		return rt.expandReplacement(string(rt.strBytes(rs)), match, index, wtf8ToRunes(rt.strBytes(s)), groups, hasNamed, lookup)
	}

	o := rt.objPtr(pattern)
	if o != nil && o.regex != nil {
		re := o.regex
		input := []rune(subject)
		var out strings.Builder
		pos := 0
		bytePos := 0
		_ = bytePos
		for {
			m, err := re.Exec(input, pos)
			if err != nil || m == nil {
				break
			}
			out.WriteString(string(input[pos:m.Index]))
			rep, terr := replaceOne(m.Groups[0].Value, m.Groups, m.Index)
			if terr != nil {
				return mkundef(), terr
			}
			out.WriteString(rep)
			adv := m.Index + m.Groups[0].Length
			pos = m.Index + m.Groups[0].Length
			if !re.Global {
				break
			}
			if m.Groups[0].Length == 0 {
				if adv < len(input) {
					out.WriteRune(input[adv])
				}
				pos = adv + 1
			}
			if pos > len(input) {
				break
			}
		}
		if pos <= len(input) {
			out.WriteString(string(input[pos:]))
		}
		return rt.newString(out.String()), nil
	}

	// String pattern: replace the first occurrence.
	ps, e := rt.toStringValue(pattern)
	if e != nil {
		return mkundef(), e
	}
	pat := string(rt.strBytes(ps))
	// A non-callable replaceValue is coerced to a string BEFORE the search (so its
	// ToString runs even when there is no match). Using the coerced string in
	// replaceOne is idempotent (toStringValue on a string calls no user code).
	if !rt.isCallable(repl) {
		rs, e := rt.toStringValue(repl)
		if e != nil {
			return mkundef(), e
		}
		repl = rs
	}
	idx := strings.Index(subject, pat)
	if idx < 0 {
		return s, nil
	}
	utf16idx := byteOffsetToUtf16([]byte(subject), idx)
	rep, terr := replaceOne(pat, []regexpjs.Group{{Index: utf16idx, Value: pat}}, utf16idx)
	if terr != nil {
		return mkundef(), terr
	}
	return rt.newString(subject[:idx] + rep + subject[idx+len(pat):]), nil
}

// expandReplacement handles the $ substitutions of GetSubstitution (22.1.3.19.1)
// in a string replacement: $$, $&, $` (portion before the match), $' (portion
// after), $1..$99 numbered captures, and $<name> named captures. position is the
// match's rune offset into input. hasNamed is false when the match has no named
// captures (so "$<" stays literal); otherwise lookupNamed resolves each name to
// its substitution (propagating a Get/ToString abrupt from a groups object).
func (rt *Runtime) expandReplacement(tmpl, match string, position int, input []rune, groups []regexpjs.Group, hasNamed bool, lookupNamed func(string) (string, *ThrowError)) (string, *ThrowError) {
	matchLen := len([]rune(match))
	var out strings.Builder
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '$' || i+1 >= len(tmpl) {
			out.WriteByte(tmpl[i])
			continue
		}
		c := tmpl[i+1]
		switch {
		case c == '$':
			out.WriteByte('$')
			i++
		case c == '&':
			out.WriteString(match)
			i++
		case c == '`':
			if position >= 0 && position <= len(input) {
				out.WriteString(string(input[:position]))
			}
			i++
		case c == '\'':
			if end := position + matchLen; end >= 0 && end <= len(input) {
				out.WriteString(string(input[end:]))
			}
			i++
		case c == '<' && hasNamed:
			// Only a match with named captures makes "$<" special; otherwise it is
			// literal. The name is looked up on the groups object (an absent or
			// unmatched name yields ""); a Get/ToString abrupt propagates.
			if gt := strings.IndexByte(tmpl[i+2:], '>'); gt >= 0 {
				s, e := lookupNamed(tmpl[i+2 : i+2+gt])
				if e != nil {
					return "", e
				}
				out.WriteString(s)
				i += 2 + gt
			} else {
				out.WriteByte('$')
			}
		case c >= '0' && c <= '9':
			// $nn (two decimal digits 01..99, a leading zero allowed) is preferred
			// when it names an existing capture; otherwise a single digit $n (1..9).
			n, consumed := -1, 0
			if i+2 < len(tmpl) && tmpl[i+2] >= '0' && tmpl[i+2] <= '9' {
				if two := int(c-'0')*10 + int(tmpl[i+2]-'0'); two >= 1 && two < len(groups) {
					n, consumed = two, 2
				}
			}
			if n < 0 && c >= '1' && int(c-'0') < len(groups) {
				n, consumed = int(c-'0'), 1
			}
			if n >= 1 {
				if groups[n].Index >= 0 {
					out.WriteString(groups[n].Value)
				}
				i += consumed
			} else {
				// Not a valid capture reference: the '$' is literal (the digits are
				// written as ordinary characters on the following iterations).
				out.WriteByte('$')
			}
		default:
			out.WriteByte('$')
		}
	}
	return out.String(), nil
}

// namedFromMap builds an expandReplacement lookup over a pre-computed name→value
// map (the internal, non-observable replace paths). An absent name yields "".
func namedFromMap(named map[string]string) (bool, func(string) (string, *ThrowError)) {
	if named == nil {
		return false, nil
	}
	return true, func(name string) (string, *ThrowError) { return named[name], nil }
}

// stringSplitRegexp implements String.prototype.split with a regex separator.
func (rt *Runtime) stringSplitRegexp(this Value, re *regexpjs.Regexp, limitV Value) (Value, *ThrowError) {
	s, e := rt.toStringValue(this)
	if e != nil {
		return mkundef(), e
	}
	input := wtf8ToRunes(rt.strBytes(s))
	res := rt.newArray()
	ro := rt.objPtr(res)
	// lim = (limit is undefined) ? 2^32-1 : ToUint32(limit); a throwing coercion
	// propagates.
	var limit int64 = 1<<32 - 1
	if !limitV.IsUndefined() {
		ln, e := rt.toNumber(limitV)
		if e != nil {
			return mkundef(), e
		}
		limit = int64(toUint32(ln))
	}
	if limit == 0 {
		return res, nil
	}
	// Empty input: return [] if the separator matches (even the empty match),
	// otherwise [""] (spec §22.2.6.14 step 12).
	if len(input) == 0 {
		if m, err := re.Exec(input, 0); err == nil && m != nil {
			return res, nil
		}
		rt.arraySet(ro, 0, s)
		return res, nil
	}
	last := 0
	pos := 0
	for pos <= len(input) {
		m, err := re.Exec(input, pos)
		if err != nil || m == nil || m.Index >= len(input) {
			break
		}
		end := m.Index + m.Groups[0].Length
		if end == last {
			pos = m.Index + 1
			continue
		}
		rt.arraySet(ro, ro.arrLen, rt.newString(string(input[last:m.Index])))
		if int64(ro.arrLen) >= limit {
			return res, nil
		}
		// Each capture is its own result element and counts toward the limit.
		for gi := 1; gi < len(m.Groups); gi++ {
			if m.Groups[gi].Index < 0 {
				rt.arraySet(ro, ro.arrLen, mkundef())
			} else {
				rt.arraySet(ro, ro.arrLen, rt.newString(m.Groups[gi].Value))
			}
			if int64(ro.arrLen) >= limit {
				return res, nil
			}
		}
		last = end
		pos = end
	}
	rt.arraySet(ro, ro.arrLen, rt.newString(string(input[last:])))
	return res, nil
}

// stringSplitString is the plain-string split path (moved from builtin_string).
func (rt *Runtime) stringSplitString(this Value, args []Value) (Value, *ThrowError) {
	// Spec §22.1.3.23 order: S = ToString(this); lim = ToUint32(limit); R =
	// ToString(separator); THEN the lim==0 and separator-undefined shortcuts (so a
	// throwing limit/separator coercion is observed even when the result is trivial).
	b, e := rt.thisStringBytes(this)
	if e != nil {
		return mkundef(), e
	}
	res := rt.newArray()
	ro := rt.objPtr(res)
	var limit int64 = 1<<32 - 1
	if !arg(args, 1).IsUndefined() {
		ln, e := rt.toNumber(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		limit = int64(toUint32(ln))
	}
	sepB, e := rt.stringArg(args, 0) // R = ToString(separator) (even if undefined)
	if e != nil {
		return mkundef(), e
	}
	if limit == 0 {
		return res, nil
	}
	if arg(args, 0).IsUndefined() {
		rt.arraySet(ro, 0, rt.newStringBytes(append([]byte{}, b...)))
		return res, nil
	}
	sep := string(sepB)
	if len(sep) == 0 {
		for i := 0; i < utf16Len(b); i++ {
			if int64(ro.arrLen) >= limit {
				break
			}
			rt.arraySet(ro, ro.arrLen, rt.charAt(b, i))
		}
		return res, nil
	}
	for p := range strings.SplitSeq(string(b), sep) {
		if int64(ro.arrLen) >= limit {
			break
		}
		rt.arraySet(ro, ro.arrLen, rt.newString(p))
	}
	return res, nil
}
