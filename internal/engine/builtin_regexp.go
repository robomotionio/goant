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
	// Legacy static properties RegExp.$1 … RegExp.$9 (Annex B). They exist as
	// configurable data properties (updated on match is not required for the
	// existence-only conformance check).
	for i := 1; i <= 9; i++ {
		cobj.defineOwn("$"+itoaSmall(i), rt.internString(""), attrConfigurable)
	}
	// RegExp.escape(S): escape a string for literal use in a pattern (ES2025).
	rt.defMethod(cobj, "escape", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if arg(args, 0).Type() != TStr {
			return mkundef(), rt.typeError("RegExp.escape argument must be a string")
		}
		return rt.newString(regExpEscape(string(rt.strBytes(arg(args, 0))))), nil
	})
	// RegExp.lastMatch (Annex B legacy static): the last matched substring.
	cobj.defineAccessor("lastMatch", rt.newNativeFunc("get lastMatch", 0,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return rt.newString(rt.regexpLastMatch), nil
		}), mkundef(), true, false, attrConfigurable)
	rt.defSpeciesGetter(ctor)
	rt.regexpCtor = ctor
	rt.defGlobal("RegExp", ctor)

	rt.initRegExpAccessors()
	rt.initStringRegexpMethods()

	// RegExp.prototype[Symbol.match/replace/search/split] delegate to the String
	// operations with `this` as the pattern (so str.match(regex) works via them).
	defSym := func(sym Value, run func(this Value, args []Value) (Value, *ThrowError)) {
		if sym == 0 {
			return
		}
		fn := rt.newNativeFunc("", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return run(this, args)
		})
		po.defineOwnSymbol(sym.handle(), fn, attrWritable|attrConfigurable)
	}
	defSym(rt.symMatch, func(this Value, args []Value) (Value, *ThrowError) {
		// Generic RegExp.prototype[@@match] (22.2.6.8): reads global/unicode and
		// runs RegExpExec, all via [[Get]] so it composes with Proxy traps.
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("RegExp.prototype[Symbol.match] called on non-object")
		}
		s, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		gv, e := rt.getField(this, "global")
		if e != nil {
			return mkundef(), e
		}
		if !rt.toBoolean(gv) {
			return rt.regExpExecAbstract(this, s)
		}
		uv, e := rt.getField(this, "unicode")
		if e != nil {
			return mkundef(), e
		}
		fullUnicode := rt.toBoolean(uv)
		if e := rt.setField(this, "lastIndex", mknum(0)); e != nil {
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
				li, _ := rt.getField(this, "lastIndex")
				liN, _ := rt.toNumber(li)
				rt.setField(this, "lastIndex", mknum(rt.advanceStringIndex(s, liN, fullUnicode)))
			}
		}
	})
	defSym(rt.symReplace, func(this Value, args []Value) (Value, *ThrowError) {
		// A native RegExp uses the fast substring path; any other object (a
		// RegExp-like or a Proxy) runs the generic exec-driven algorithm.
		if o := rt.objPtr(this); o != nil && o.regex != nil {
			return rt.stringReplace(arg(args, 0), this, arg(args, 1))
		}
		return rt.regexpSymbolReplace(this, arg(args, 0), arg(args, 1))
	})
	defSym(rt.symSearch, func(this Value, args []Value) (Value, *ThrowError) {
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
		if n, _ := rt.toNumber(prevLI); n != 0 {
			if e := rt.setField(this, "lastIndex", mknum(0)); e != nil {
				return mkundef(), e
			}
		}
		result, e := rt.regExpExecAbstract(this, s)
		if e != nil {
			return mkundef(), e
		}
		curLI, _ := rt.getField(this, "lastIndex")
		if !rt.sameValue(curLI, prevLI) {
			if e := rt.setField(this, "lastIndex", prevLI); e != nil {
				return mkundef(), e
			}
		}
		if result.IsNull() {
			return mknum(-1), nil
		}
		return rt.getField(result, "index")
	})
	defSym(rt.symSplit, func(this Value, args []Value) (Value, *ThrowError) {
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
	defSym(rt.symMatchAll, func(this Value, args []Value) (Value, *ThrowError) {
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
		liN, e := rt.toNumber(li)
		if e != nil {
			return mkundef(), e
		}
		start := 0
		if liN > 0 {
			start = int(liN)
		}
		return rt.regexpMatchAllIterator(matcher, sv, start), nil
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

// regexpMatchAllIterator eagerly collects each match of reObj against s starting
// at startPos into a match-result array (with index/input), then returns an
// iterator over them (a non-global RegExp yields at most one).
func (rt *Runtime) regexpMatchAllIterator(reObj, s Value, startPos int) Value {
	re := rt.objPtr(reObj).regex
	input := []rune(string(rt.strBytes(s)))
	var results []Value
	pos := startPos
	if pos < 0 {
		pos = 0
	}
	for pos <= len(input) {
		m, err := re.Exec(input, pos)
		if err != nil || m == nil {
			break
		}
		res := rt.newArray()
		ro := rt.objPtr(res)
		for i, g := range m.Groups {
			if g.Index < 0 && i > 0 {
				rt.arraySet(ro, uint32(i), mkundef())
			} else {
				rt.arraySet(ro, uint32(i), rt.newString(g.Value))
			}
		}
		ro.defineOwn("index", mknum(float64(m.Index)), attrDefault)
		ro.defineOwn("input", s, attrDefault)
		results = append(results, res)
		if !re.Global {
			break
		}
		adv := m.Index + m.Groups[0].Length
		if adv <= pos {
			adv = pos + 1
		}
		pos = adv
	}
	return rt.sliceIterator(results)
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

func nonEmptySource(p string) string {
	if p == "" {
		return "(?:)"
	}
	return p
}

// regexpExec runs RegExp.prototype.exec, returning a match-result array or null.
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

func (rt *Runtime) regexpExec(this, strVal Value) (Value, *ThrowError) {
	o := rt.objPtr(this)
	if o == nil || o.regex == nil {
		return mkundef(), rt.typeError("Method RegExp.prototype.exec called on incompatible receiver")
	}
	s, e := rt.toStringValue(strVal)
	if e != nil {
		return mkundef(), e
	}
	input := []rune(string(rt.strBytes(s)))
	re := o.regex

	start := 0
	if re.Global || re.Sticky {
		liv, _ := rt.getField(this, "lastIndex")
		n, _ := rt.toNumberPrimitive(liv)
		start = int(n)
	}
	m, err := re.Exec(input, start)
	if err != nil {
		return mkundef(), rt.typeError("regexp exec error: " + err.Error())
	}
	if m == nil {
		if re.Global || re.Sticky {
			rt.setField(this, "lastIndex", mknum(0))
		}
		return mknull(), nil
	}
	if re.Global || re.Sticky {
		rt.setField(this, "lastIndex", mknum(float64(m.Index+m.Groups[0].Length)))
	}
	if len(m.Groups) > 0 {
		rt.regexpLastMatch = m.Groups[0].Value // RegExp.lastMatch (Annex B)
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
		b, e := rt.thisStringBytes(this)
		if e != nil {
			return mkundef(), e
		}
		re, e := rt.coerceRegExp(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		m, err := re.Exec([]rune(string(b)), 0)
		if err != nil || m == nil {
			return mknum(-1), nil
		}
		return mknum(float64(m.Index)), nil
	})

	rt.defMethod(sp, "match", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() { // RequireObjectCoercible before delegating to @@match
			return mkundef(), rt.typeError("String.prototype.match called on null or undefined")
		}
		if r, ok, e := rt.delegateSymbolMethod(rt.symMatch, arg(args, 0), this); ok {
			return r, e
		}
		s, e := rt.toStringValue(this)
		if e != nil {
			return mkundef(), e
		}
		reObj, e := rt.regexpArg(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		re := rt.objPtr(reObj).regex
		if !re.Global {
			return rt.regexpExec(reObj, s)
		}
		// Global match: collect all whole-match strings.
		input := []rune(string(rt.strBytes(s)))
		res := rt.newArray()
		ro := rt.objPtr(res)
		pos := 0
		any := false
		for {
			m, err := re.Exec(input, pos)
			if err != nil || m == nil {
				break
			}
			any = true
			rt.arraySet(ro, ro.arrLen, rt.newString(m.Groups[0].Value))
			adv := m.Index + m.Groups[0].Length
			if adv <= pos {
				adv = pos + 1
			}
			pos = adv
		}
		if !any {
			return mknull(), nil
		}
		return res, nil
	})

	rt.defMethod(sp, "replace", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() { // RequireObjectCoercible before delegating to @@replace
			return mkundef(), rt.typeError("String.prototype.replace called on null or undefined")
		}
		// GetMethod(searchValue, @@replace) via [[Get]] so a Proxy trap sees it.
		if pat := arg(args, 0); !pat.IsNullish() {
			m, e := rt.getElement(pat, rt.symReplace)
			if e != nil {
				return mkundef(), e
			}
			if rt.isCallable(m) {
				s, e := rt.toStringValue(this)
				if e != nil {
					return mkundef(), e
				}
				return rt.callValue(m, pat, []Value{s, arg(args, 1)})
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
		// Default: RegExpCreate(regexp, "g") then iterate.
		s, e := rt.toStringValue(this)
		if e != nil {
			return mkundef(), e
		}
		reObj, e := rt.construct(rt.regexpCtor, []Value{regexp, rt.newString("g")})
		if e != nil {
			return mkundef(), e
		}
		return rt.regexpMatchAllIterator(reObj, s, 0), nil
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
			if rt.isCallable(replacer) {
				sv, e := rt.toStringValue(this)
				if e != nil {
					return mkundef(), e
				}
				return rt.callValue(replacer, search, []Value{sv, arg(args, 1)})
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
				rep = expandReplacement(replStr, needle, utf16pos, inputRunes, []regexpjs.Group{{Index: utf16pos, Value: needle}}, nil)
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
		// GetMethod(separator, @@split) via [[Get]] so a Proxy trap sees it.
		if pat := arg(args, 0); !pat.IsNullish() {
			m, e := rt.getElement(pat, rt.symSplit)
			if e != nil {
				return mkundef(), e
			}
			if rt.isCallable(m) {
				s, e := rt.toStringValue(this)
				if e != nil {
					return mkundef(), e
				}
				return rt.callValue(m, pat, []Value{s, arg(args, 1)})
			}
		}
		sep := arg(args, 0)
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
	if sym == 0 || pattern.IsNullish() {
		return mkundef(), false, nil
	}
	// GetMethod routes through [[Get]] so a Proxy's trap observes the well-known
	// symbol lookup (String.prototype.match/replace/search/split spec step 2a).
	m, e := rt.getElement(pattern, sym)
	if e != nil {
		return mkundef(), true, e
	}
	if !rt.isCallable(m) {
		return mkundef(), false, nil
	}
	s, e := rt.toStringValue(str)
	if e != nil {
		return mkundef(), true, e
	}
	r, e := rt.callValue(m, pattern, []Value{s})
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
	Srunes := []rune(string(rt.strBytes(sV)))
	functional := rt.isCallable(repl)
	replStr := ""
	if !functional {
		rs, e := rt.toStringValue(repl)
		if e != nil {
			return mkundef(), e
		}
		replStr = string(rt.strBytes(rs))
	}
	gv, e := rt.getField(rx, "global")
	if e != nil {
		return mkundef(), e
	}
	global := rt.toBoolean(gv)
	fullUnicode := false
	if global {
		uv, e := rt.getField(rx, "unicode")
		if e != nil {
			return mkundef(), e
		}
		fullUnicode = rt.toBoolean(uv)
		if e := rt.setField(rx, "lastIndex", mknum(0)); e != nil {
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
			li, _ := rt.getField(rx, "lastIndex")
			liN, _ := rt.toNumber(li)
			if e := rt.setField(rx, "lastIndex", mknum(rt.advanceStringIndex(sV, liN, fullUnicode))); e != nil {
				return mkundef(), e
			}
		}
	}
	var out strings.Builder
	nextPos := 0
	for _, result := range results {
		lenV, _ := rt.getField(result, "length")
		ln, _ := rt.toNumber(lenV)
		nCaptures := max(int(ln)-1, 0)
		m0, e := rt.getElement(result, mknum(0))
		if e != nil {
			return mkundef(), e
		}
		matchedV, e := rt.toStringValue(m0)
		if e != nil {
			return mkundef(), e
		}
		matched := string(rt.strBytes(matchedV))
		idxV, _ := rt.getField(result, "index")
		pos, _ := rt.toNumber(idxV)
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
			callArgs := make([]Value, 0, nCaptures+3)
			callArgs = append(callArgs, matchedV)
			callArgs = append(callArgs, caps...)
			callArgs = append(callArgs, mknum(float64(position)), sV)
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
			// Named captures for $<name> come from the match result's `groups`
			// object (undefined when the pattern has no named groups).
			var named map[string]string
			if gv, _ := rt.getField(result, "groups"); gv.IsObjectType() {
				named = map[string]string{}
				for _, k := range rt.objPtr(gv).ownKeysEnumerable() {
					v, _ := rt.getField(gv, k)
					if v.IsUndefined() {
						named[k] = ""
					} else if sv, e := rt.toStringValue(v); e == nil {
						named[k] = string(rt.strBytes(sv))
					}
				}
			}
			replacement = expandReplacement(replStr, matched, position, Srunes, groups, named)
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
	S := []rune(string(rt.strBytes(sV)))
	res := rt.newArray()
	ro := rt.objPtr(res)
	lim := int64(1)<<32 - 1
	if !limitV.IsUndefined() {
		lim = int64(toUint32(float64(rt.intArg([]Value{limitV}, 0))))
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
		if e := rt.setField(splitter, "lastIndex", mknum(float64(q))); e != nil {
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
		liV, _ := rt.getField(splitter, "lastIndex")
		liN, _ := rt.toNumber(liV)
		end := min(int(liN), len(S))
		if end == p {
			q = int(rt.advanceStringIndex(sV, float64(q), unicode))
			continue
		}
		pushSeg(rt.newString(string(S[p:q])))
		if int64(ro.arrLen) == lim {
			return res, nil
		}
		lenV, _ := rt.getField(z, "length")
		ln, _ := rt.toNumber(lenV)
		for i := 1; i <= max(int(ln)-1, 0); i++ {
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
		return expandReplacement(string(rt.strBytes(rs)), match, index, []rune(string(rt.strBytes(s))), groups, named), nil
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
// match's rune offset into input; named is nil when the pattern has no named
// groups (so "$<" stays literal).
func expandReplacement(tmpl, match string, position int, input []rune, groups []regexpjs.Group, named map[string]string) string {
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
		case c == '<' && len(named) > 0:
			// Only a pattern that actually has named groups makes "$<" special;
			// otherwise it is literal. An absent/unmatched name yields "".
			if gt := strings.IndexByte(tmpl[i+2:], '>'); gt >= 0 {
				out.WriteString(named[tmpl[i+2:i+2+gt]])
				i += 2 + gt
			} else {
				out.WriteByte('$')
			}
		case c >= '1' && c <= '9':
			n := int(c - '0')
			consumed := 1
			// Prefer a two-digit reference when it names an existing capture.
			if i+2 < len(tmpl) && tmpl[i+2] >= '0' && tmpl[i+2] <= '9' {
				if two := n*10 + int(tmpl[i+2]-'0'); two >= 1 && two < len(groups) {
					n = two
					consumed = 2
				}
			}
			if n >= 1 && n < len(groups) {
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
	return out.String()
}

// stringSplitRegexp implements String.prototype.split with a regex separator.
func (rt *Runtime) stringSplitRegexp(this Value, re *regexpjs.Regexp, limitV Value) (Value, *ThrowError) {
	s, e := rt.toStringValue(this)
	if e != nil {
		return mkundef(), e
	}
	input := []rune(string(rt.strBytes(s)))
	res := rt.newArray()
	ro := rt.objPtr(res)
	limit := -1
	if !limitV.IsUndefined() {
		limit = rt.intArg([]Value{limitV}, 0)
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
		if limit >= 0 && int(ro.arrLen) >= limit {
			return res, nil
		}
		for gi := 1; gi < len(m.Groups); gi++ {
			if m.Groups[gi].Index < 0 {
				rt.arraySet(ro, ro.arrLen, mkundef())
			} else {
				rt.arraySet(ro, ro.arrLen, rt.newString(m.Groups[gi].Value))
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
	b, e := rt.thisStringBytes(this)
	if e != nil {
		return mkundef(), e
	}
	res := rt.newArray()
	ro := rt.objPtr(res)
	// lim === 0 yields an empty array — this precedes the undefined-separator
	// shortcut (spec §22.1.3.23 steps 6–8).
	limit := -1
	if !arg(args, 1).IsUndefined() {
		limit = int(toUint32(float64(rt.intArg(args, 1))))
	}
	if limit == 0 {
		return res, nil
	}
	if arg(args, 0).IsUndefined() {
		rt.arraySet(ro, 0, rt.newStringBytes(append([]byte{}, b...)))
		return res, nil
	}
	sep, e := rt.stringArg(args, 0)
	if e != nil {
		return mkundef(), e
	}
	if len(sep) == 0 {
		for i := 0; i < utf16Len(b); i++ {
			if limit >= 0 && int(ro.arrLen) >= limit {
				break
			}
			rt.arraySet(ro, ro.arrLen, rt.charAt(b, i))
		}
		return res, nil
	}
	for p := range strings.SplitSeq(string(b), string(sep)) {
		if limit >= 0 && int(ro.arrLen) >= limit {
			break
		}
		rt.arraySet(ro, ro.arrLen, rt.newString(p))
	}
	return res, nil
}
