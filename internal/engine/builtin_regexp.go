package engine

// RegExp constructor + RegExp.prototype (ant modules/regex.c + builtin_regexp),
// backed by internal/regexpjs. Also the String↔RegExp methods match/replace/
// search/split.

import (
	"strings"

	"goant/internal/regexpjs"
)

func (rt *Runtime) initRegExpBuiltin() {
	proto := rt.newObject(rt.objectProto)
	rt.regexpProto = proto
	po := rt.objPtr(proto)

	rt.defMethod(po, "exec", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.regexpExec(this, arg(args, 0))
	})
	rt.defMethod(po, "test", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res, e := rt.regexpExec(this, arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return mkbool(!res.IsNull()), nil
	})
	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.regex == nil {
			return rt.internString("/(?:)/"), nil
		}
		return rt.newString("/" + o.regex.Source + "/" + o.regex.Flags), nil
	})

	ctor := rt.newNativeFunc("RegExp", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		pattern := ""
		flags := ""
		p := arg(args, 0)
		if o := rt.objPtr(p); o != nil && o.regex != nil {
			pattern = o.regex.Source
			flags = o.regex.Flags
		} else if !p.IsUndefined() {
			s, e := rt.toStringValue(p)
			if e != nil {
				return mkundef(), e
			}
			pattern = string(rt.strBytes(s))
		}
		if !arg(args, 1).IsUndefined() {
			s, e := rt.toStringValue(args[1])
			if e != nil {
				return mkundef(), e
			}
			flags = string(rt.strBytes(s))
		}
		return rt.newRegExp(pattern, flags)
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defSpeciesGetter(ctor)
	rt.defGlobal("RegExp", ctor)

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
		str := arg(args, 0)
		re := rt.objPtr(this)
		if re == nil || re.regex == nil {
			return mkundef(), rt.typeError("Method RegExp.prototype[Symbol.match] called on incompatible receiver")
		}
		s, e := rt.toStringValue(str)
		if e != nil {
			return mkundef(), e
		}
		if !re.regex.Global {
			return rt.regexpExec(this, s)
		}
		input := []rune(string(rt.strBytes(s)))
		res := rt.newArray()
		ro := rt.objPtr(res)
		pos, any := 0, false
		for {
			m, err := re.regex.Exec(input, pos)
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
	defSym(rt.symReplace, func(this Value, args []Value) (Value, *ThrowError) {
		return rt.stringReplace(arg(args, 0), this, arg(args, 1))
	})
	defSym(rt.symSearch, func(this Value, args []Value) (Value, *ThrowError) {
		str := arg(args, 0)
		re := rt.objPtr(this)
		if re == nil || re.regex == nil {
			return mkundef(), rt.typeError("Method RegExp.prototype[Symbol.search] called on incompatible receiver")
		}
		b, e := rt.thisStringBytes(str)
		if e != nil {
			return mkundef(), e
		}
		m, err := re.regex.Exec([]rune(string(b)), 0)
		if err != nil || m == nil {
			return mknum(-1), nil
		}
		return mknum(float64(m.Index)), nil
	})
	defSym(rt.symSplit, func(this Value, args []Value) (Value, *ThrowError) {
		str := arg(args, 0)
		re := rt.objPtr(this)
		if re == nil || re.regex == nil {
			return mkundef(), rt.typeError("Method RegExp.prototype[Symbol.split] called on incompatible receiver")
		}
		return rt.stringSplitRegexp(str, re.regex, arg(args, 1))
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
	o.defineOwn("source", rt.internString(nonEmptySource(pattern)), 0)
	o.defineOwn("flags", rt.internString(canonicalFlags(flags)), 0)
	o.defineOwn("global", mkbool(re.Global), 0)
	o.defineOwn("ignoreCase", mkbool(re.IgnoreCase), 0)
	o.defineOwn("multiline", mkbool(re.Multiline), 0)
	o.defineOwn("lastIndex", mknum(0), attrWritable)
	return v, nil
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
		if m := rt.getFieldSymbol(arg(args, 0), symOrZero(rt.symReplace)); rt.isCallable(m) {
			s, e := rt.toStringValue(this)
			if e != nil {
				return mkundef(), e
			}
			return rt.callValue(m, arg(args, 0), []Value{s, arg(args, 1)})
		}
		return rt.stringReplace(this, arg(args, 0), arg(args, 1))
	})

	rt.defMethod(sp, "matchAll", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.toStringValue(this)
		if e != nil {
			return mkundef(), e
		}
		pat := arg(args, 0)
		if o := rt.objPtr(pat); o != nil && o.regex != nil && !o.regex.Global {
			return mkundef(), rt.typeError("String.prototype.matchAll called with a non-global RegExp argument")
		}
		reObj, e := rt.regexpArg(pat)
		if e != nil {
			return mkundef(), e
		}
		re := rt.objPtr(reObj).regex
		input := []rune(string(rt.strBytes(s)))
		// Precompute all match-result arrays, then hand back an iterator.
		var results []Value
		pos := 0
		for {
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
			adv := m.Index + m.Groups[0].Length
			if adv <= pos {
				adv = pos + 1
			}
			pos = adv
		}
		return rt.sliceIterator(results), nil
	})

	rt.defMethod(sp, "replaceAll", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		search := arg(args, 0)
		if o := rt.objPtr(search); o != nil && o.regex != nil {
			if !o.regex.Global {
				return mkundef(), rt.typeError("replaceAll must be called with a global RegExp")
			}
			return rt.stringReplace(this, search, arg(args, 1))
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
		if rt.isCallable(repl) {
			var b strings.Builder
			pos := 0
			for {
				idx := strings.Index(ss[pos:], needle)
				if idx < 0 || needle == "" {
					break
				}
				at := pos + idx
				b.WriteString(ss[pos:at])
				rv, e := rt.callValue(repl, mkundef(), []Value{rt.newString(needle), mknum(float64(at)), s})
				if e != nil {
					return mkundef(), e
				}
				rs, _ := rt.toStringValue(rv)
				b.Write(rt.strBytes(rs))
				pos = at + len(needle)
			}
			b.WriteString(ss[pos:])
			return rt.newString(b.String()), nil
		}
		rs, e := rt.toStringValue(repl)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(strings.ReplaceAll(ss, needle, string(rt.strBytes(rs)))), nil
	})

	rt.defMethod(sp, "split", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if m := rt.getFieldSymbol(arg(args, 0), symOrZero(rt.symSplit)); rt.isCallable(m) {
			s, e := rt.toStringValue(this)
			if e != nil {
				return mkundef(), e
			}
			return rt.callValue(m, arg(args, 0), []Value{s, arg(args, 1)})
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
	m := rt.getFieldSymbol(pattern, sym.handle())
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
		return expandReplacement(string(rt.strBytes(rs)), match, groups), nil
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

// expandReplacement handles $&, $1..$9, $`, $', $$ in a string replacement.
func expandReplacement(tmpl, match string, groups []regexpjs.Group) string {
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
		case c >= '1' && c <= '9':
			n := int(c - '0')
			// Two-digit group reference when valid.
			if i+2 < len(tmpl) && tmpl[i+2] >= '0' && tmpl[i+2] <= '9' {
				two := n*10 + int(tmpl[i+2]-'0')
				if two < len(groups) {
					n = two
					i++
				}
			}
			if n < len(groups) && groups[n].Index >= 0 {
				out.WriteString(groups[n].Value)
			}
			i++
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
	if arg(args, 0).IsUndefined() {
		rt.arraySet(ro, 0, rt.newStringBytes(append([]byte{}, b...)))
		return res, nil
	}
	sep, e := rt.stringArg(args, 0)
	if e != nil {
		return mkundef(), e
	}
	limit := -1
	if !arg(args, 1).IsUndefined() {
		limit = int(toUint32(float64(rt.intArg(args, 1))))
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
