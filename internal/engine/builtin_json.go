package engine

// JSON.parse / JSON.stringify (ant modules/json.c), with reviver, replacer, and
// space support.

import (
	"math"
	"strconv"
	"strings"
)

func (rt *Runtime) initJSONBuiltin() {
	json := rt.newObject(rt.objectProto)
	jo := rt.objPtr(json)

	rt.defMethod(jo, "stringify", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		st := &jsonStringifier{rt: rt}
		// replacer
		replacer := arg(args, 1)
		if rt.isCallable(replacer) {
			st.replacerFn = replacer
		} else if rt.isArrayValue(replacer) {
			// PropertyList: for each element, keep a String, ToString(a Number), or
			// ToString(a String/Number wrapper object); de-duplicated, order-preserved.
			st.hasPropertyList = true
			seen := map[string]bool{}
			n, e := rt.lengthOf(replacer)
			if e != nil {
				return mkundef(), e
			}
			for i := 0; i < n; i++ {
				k, e := rt.getElement(replacer, mknum(float64(i)))
				if e != nil {
					return mkundef(), e
				}
				var item Value
				have := false
				switch {
				case k.IsString():
					item, have = k, true
				case k.Type() == TNum:
					s, e := rt.toStringValue(k)
					if e != nil {
						return mkundef(), e
					}
					item, have = s, true
				case k.IsObjectType():
					if o := rt.objPtr(k); o != nil && (o.getSlot(slotPrimitive).Type() == TNum || o.boxed.Type() == TStr) {
						s, e := rt.toStringValue(k)
						if e != nil {
							return mkundef(), e
						}
						item, have = s, true
					}
				}
				if have {
					ks := string(rt.strBytes(item))
					if !seen[ks] {
						seen[ks] = true
						st.propertyList = append(st.propertyList, ks)
					}
				}
			}
		}
		// space: a Number/String wrapper object is unwrapped to its primitive
		// (ToNumber/ToString) before the Number/String gap rules apply.
		sp := arg(args, 2)
		if sp.IsObjectType() {
			if o := rt.objPtr(sp); o != nil {
				if o.getSlot(slotPrimitive).Type() == TNum {
					n, e := rt.toNumber(sp)
					if e != nil {
						return mkundef(), e
					}
					sp = mknum(n)
				} else if o.boxed.Type() == TStr {
					s, e := rt.toStringValue(sp)
					if e != nil {
						return mkundef(), e
					}
					sp = s
				}
			}
		}
		switch {
		case sp.Type() == TNum:
			n := int(sp.Number())
			if n > 10 {
				n = 10
			}
			if n > 0 {
				st.gap = strings.Repeat(" ", n)
			}
		case sp.IsString():
			s := string(rt.strBytes(sp))
			if len(s) > 10 {
				s = s[:10]
			}
			st.gap = s
		}
		holder := rt.newPlainObject()
		rt.objPtr(holder).defineOwn("", arg(args, 0), attrDefault)
		out, ok, e := st.str("", holder, "")
		if e != nil {
			return mkundef(), e
		}
		if !ok {
			return mkundef(), nil
		}
		return rt.newString(out), nil
	})

	rt.defMethod(jo, "parse", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		p := &jsonParser{rt: rt, src: string(rt.strBytes(s))}
		v, src, perr := p.parse()
		if perr != nil {
			ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(perr.Error())})
			return mkundef(), &ThrowError{Value: ev, rt: rt}
		}
		p.skipWS()
		if p.pos != len(p.src) {
			ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString("Unexpected non-whitespace character after JSON")})
			return mkundef(), &ThrowError{Value: ev, rt: rt}
		}
		if reviver := arg(args, 1); rt.isCallable(reviver) {
			holder := rt.newPlainObject()
			rt.objPtr(holder).defineOwn("", v, attrDefault)
			return rt.jsonRevive(holder, "", reviver, src)
		}
		return v, nil
	})

	// JSON.rawJSON(text): a frozen, null-prototype object whose [[RawJSON]] text is
	// emitted verbatim by stringify. text must be a non-empty JSON primitive with
	// no leading/trailing whitespace.
	rt.defMethod(jo, "rawJSON", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		js := string(rt.strBytes(s))
		jsonSyntaxErr := func(msg string) *ThrowError {
			ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(msg)})
			return &ThrowError{Value: ev, rt: rt}
		}
		isWS := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
		if js == "" || isWS(js[0]) || isWS(js[len(js)-1]) {
			return mkundef(), jsonSyntaxErr("JSON.rawJSON text must be a non-empty string with no leading or trailing whitespace")
		}
		p := &jsonParser{rt: rt, src: js}
		v, _, perr := p.parse()
		if perr != nil {
			return mkundef(), jsonSyntaxErr(perr.Error())
		}
		p.skipWS()
		if p.pos != len(p.src) {
			return mkundef(), jsonSyntaxErr("Unexpected non-whitespace character after JSON")
		}
		switch v.Type() {
		case TNull, TBool, TNum, TStr: // a JSON primitive
		default:
			return mkundef(), jsonSyntaxErr("JSON.rawJSON text must be a primitive value")
		}
		raw := rt.newObject(mknull())
		o := rt.objPtr(raw)
		o.defineOwn("rawJSON", rt.newString(js), attrEnumerable)
		o.setSlot(slotRawJSON, rt.newString(js))
		if e := rt.sealObject(raw, true); e != nil {
			return mkundef(), e
		}
		return raw, nil
	})
	rt.defMethod(jo, "isRawJSON", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if o := rt.objPtr(arg(args, 0)); o != nil && o.getSlot(slotRawJSON).Type() == TStr {
			return mktrue(), nil
		}
		return mkfalse(), nil
	})

	rt.setStringTag(json, "JSON")
	rt.objPtr(rt.global).defineOwn("JSON", json, attrWritable|attrConfigurable)
}

// ---- stringify ----

type jsonStringifier struct {
	rt         *Runtime
	replacerFn Value
	// propertyList is the ordered, de-duplicated PropertyList from an array
	// replacer (nil when no array replacer). When non-nil, an object serializes
	// exactly these keys in this order (a present-but-empty list yields "{}").
	propertyList    []string
	hasPropertyList bool
	gap             string
	indent          string
	stack           []Value // objects/arrays currently being serialized (cycle detection)
}

// enterCycle pushes v onto the serialization stack, returning a TypeError if v
// is already present (a cyclical structure). The returned func pops it.
func (st *jsonStringifier) enterCycle(v Value) (func(), *ThrowError) {
	for _, s := range st.stack {
		if s == v {
			return nil, st.rt.typeError("Converting circular structure to JSON")
		}
	}
	st.stack = append(st.stack, v)
	return func() { st.stack = st.stack[:len(st.stack)-1] }, nil
}

func (st *jsonStringifier) str(key string, holder Value, indent string) (string, bool, *ThrowError) {
	rt := st.rt
	v, e := rt.getField(holder, key)
	if e != nil {
		return "", false, e
	}
	// toJSON: looked up for Objects and (per SerializeJSONProperty) BigInt values.
	if v.IsObjectType() || v.Type() == TBigInt {
		if tj, _ := rt.getField(v, "toJSON"); rt.isCallable(tj) {
			nv, terr := rt.callValue(tj, v, []Value{rt.newString(key)})
			if terr != nil {
				return "", false, terr
			}
			v = nv
		}
	}
	if st.replacerFn != 0 {
		nv, terr := rt.callValue(st.replacerFn, holder, []Value{rt.newString(key), v})
		if terr != nil {
			return "", false, terr
		}
		v = nv
	}

	// Unwrap a Number/String/Boolean/BigInt wrapper object to its primitive
	// ([[NumberData]]/[[StringData]]/[[BooleanData]]/[[BigIntData]]) before the
	// type switch (SerializeJSONProperty step 4).
	if v.IsObjectType() {
		if o := rt.objPtr(v); o != nil {
			switch prim := o.getSlot(slotPrimitive); {
			case prim.Type() == TNum:
				n, e := rt.toNumber(v)
				if e != nil {
					return "", false, e
				}
				v = mknum(n)
			case prim.Type() == TBigInt:
				v = prim
			case o.boxed.Type() == TStr:
				s, e := rt.toStringValue(v)
				if e != nil {
					return "", false, e
				}
				v = s
			case o.boxed.Type() == TBool:
				v = o.boxed
			}
		}
	}

	switch v.Type() {
	case TBigInt:
		return "", false, rt.typeError("Do not know how to serialize a BigInt")
	case TNull:
		return "null", true, nil
	case TBool:
		if v.Bool() {
			return "true", true, nil
		}
		return "false", true, nil
	case TStr:
		return jsonQuote(string(rt.strBytes(v))), true, nil
	case TNum:
		if math.IsNaN(v.Number()) || math.IsInf(v.Number(), 0) {
			return "null", true, nil
		}
		return numberToString(v.Number()), true, nil
	case TArr:
		return st.stringifyArray(v, indent)
	case TFunc, TCFunc, TUndef:
		return "", false, nil
	default:
		if v.IsObjectType() {
			// A JSON.rawJSON object emits its [[RawJSON]] text verbatim.
			if o := rt.objPtr(v); o != nil {
				if raw := o.getSlot(slotRawJSON); raw.Type() == TStr {
					return string(rt.strBytes(raw)), true, nil
				}
			}
			// A Proxy wrapping an array serializes as an array (via its traps).
			if o := rt.objPtr(v); o != nil && o.proxy != nil && rt.isArrayValue(v) {
				return st.stringifyArray(v, indent)
			}
			return st.stringifyObject(v, indent)
		}
		return "", false, nil
	}
}

func (st *jsonStringifier) stringifyArray(v Value, indent string) (string, bool, *ThrowError) {
	rt := st.rt
	leave, e := st.enterCycle(v)
	if e != nil {
		return "", false, e
	}
	defer leave()
	newIndent := indent + st.gap
	n, e := rt.lengthOf(v)
	if e != nil {
		return "", false, e
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		s, ok, e := st.str(numberToString(float64(i)), v, newIndent)
		if e != nil {
			return "", false, e
		}
		if !ok {
			s = "null"
		}
		parts[i] = s
	}
	if len(parts) == 0 {
		return "[]", true, nil
	}
	if st.gap == "" {
		return "[" + strings.Join(parts, ",") + "]", true, nil
	}
	return "[\n" + newIndent + strings.Join(parts, ",\n"+newIndent) + "\n" + indent + "]", true, nil
}

func (st *jsonStringifier) stringifyObject(v Value, indent string) (string, bool, *ThrowError) {
	rt := st.rt
	leave, e := st.enterCycle(v)
	if e != nil {
		return "", false, e
	}
	defer leave()
	o := rt.objPtr(v)
	newIndent := indent + st.gap
	var parts []string
	// With an array replacer, serialize exactly the PropertyList keys in order
	// (str skips any that are absent on the object). Otherwise use
	// EnumerableOwnPropertyNames — for a proxy, via its ownKeys +
	// getOwnPropertyDescriptor traps.
	var keys []string
	if st.hasPropertyList {
		keys = st.propertyList
	} else if o.proxy != nil {
		// A proxy routes EnumerableOwnPropertyNames through its ownKeys +
		// getOwnPropertyDescriptor traps; a revoked proxy throws (propagate it
		// rather than serializing as an empty object).
		ek, e := rt.enumerableOwnKeysE(v)
		if e != nil {
			return "", false, e
		}
		keys = ek
	} else {
		keys = o.ownKeysEnumerable()
	}
	for _, k := range keys {
		s, ok, e := st.str(k, v, newIndent)
		if e != nil {
			return "", false, e
		}
		if !ok {
			continue
		}
		sep := ":"
		if st.gap != "" {
			sep = ": "
		}
		parts = append(parts, jsonQuote(k)+sep+s)
	}
	if len(parts) == 0 {
		return "{}", true, nil
	}
	if st.gap == "" {
		return "{" + strings.Join(parts, ",") + "}", true, nil
	}
	return "{\n" + newIndent + strings.Join(parts, ",\n"+newIndent) + "\n" + indent + "}", true, nil
}

// jsonQuote produces a JSON string literal.
func jsonQuote(s string) string {
	const hexd = "0123456789abcdef"
	var b strings.Builder
	b.WriteByte('"')
	src := []byte(s)
	i := 0
	for i < len(src) {
		c := src[i]
		// Decode one WTF-8 code point (may be a lone surrogate, unlike UTF-8).
		var cp rune
		size := 1
		switch {
		case c < 0x80:
			cp = rune(c)
		case c < 0xE0 && i+1 < len(src):
			cp = rune(c&0x1F)<<6 | rune(src[i+1]&0x3F)
			size = 2
		case c < 0xF0 && i+2 < len(src):
			cp = rune(c&0x0F)<<12 | rune(src[i+1]&0x3F)<<6 | rune(src[i+2]&0x3F)
			size = 3
		case i+3 < len(src):
			cp = rune(c&0x07)<<18 | rune(src[i+1]&0x3F)<<12 | rune(src[i+2]&0x3F)<<6 | rune(src[i+3]&0x3F)
			size = 4
		default:
			cp = rune(c)
		}
		i += size
		switch cp {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		default:
			// Control chars and lone surrogates (ES2019 well-formed JSON) escape.
			if cp < 0x20 || (cp >= 0xD800 && cp <= 0xDFFF) {
				b.WriteString("\\u")
				b.WriteByte(hexd[(cp>>12)&0xF])
				b.WriteByte(hexd[(cp>>8)&0xF])
				b.WriteByte(hexd[(cp>>4)&0xF])
				b.WriteByte(hexd[cp&0xF])
			} else {
				b.WriteRune(cp)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ---- parse ----

type jsonParser struct {
	rt  *Runtime
	src string
	pos int
}

type jsonError struct{ msg string }

func (e *jsonError) Error() string { return e.msg }

func (p *jsonParser) skipWS() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

// jsonSrc is the parse record for one JSON value: its source substring plus,
// for composites, the child records. It backs the reviver's context.source (the
// JSON source-text-access proposal): a primitive value exposes its source text;
// an object/array (composite) exposes none.
type jsonSrc struct {
	text      string              // source substring for this value
	composite bool                // an object or array (no context.source)
	props     map[string]*jsonSrc // object entries by key
	elems     []*jsonSrc          // array element records, by index
}

func (p *jsonParser) parse() (Value, *jsonSrc, error) {
	p.skipWS()
	if p.pos >= len(p.src) {
		return mkundef(), nil, &jsonError{"Unexpected end of JSON input"}
	}
	start := p.pos
	// prim wraps a primitive parse result with its source span.
	prim := func(v Value, err error) (Value, *jsonSrc, error) {
		if err != nil {
			return mkundef(), nil, err
		}
		return v, &jsonSrc{text: p.src[start:p.pos]}, nil
	}
	c := p.src[p.pos]
	switch {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"':
		s, err := p.parseString()
		if err != nil {
			return mkundef(), nil, err
		}
		return prim(p.rt.newString(s), nil)
	case c == 't':
		return prim(p.parseLit("true", mktrue()))
	case c == 'f':
		return prim(p.parseLit("false", mkfalse()))
	case c == 'n':
		return prim(p.parseLit("null", mknull()))
	case c == '-' || (c >= '0' && c <= '9'):
		return prim(p.parseNumber())
	}
	return mkundef(), nil, &jsonError{"Unexpected token in JSON"}
}

func (p *jsonParser) parseLit(lit string, v Value) (Value, error) {
	if strings.HasPrefix(p.src[p.pos:], lit) {
		p.pos += len(lit)
		return v, nil
	}
	return mkundef(), &jsonError{"Unexpected token in JSON"}
}

func (p *jsonParser) parseNumber() (Value, error) {
	start := p.pos
	bad := func() (Value, error) { return mkundef(), &jsonError{"Invalid number in JSON"} }
	digit := func() bool { return p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' }
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		p.pos++
	}
	// Integer part: a single 0, or a nonzero digit followed by more digits (JSON
	// forbids leading zeros).
	if p.pos < len(p.src) && p.src[p.pos] == '0' {
		p.pos++
	} else if digit() {
		for digit() {
			p.pos++
		}
	} else {
		return bad()
	}
	// Fraction: '.' then at least one digit.
	if p.pos < len(p.src) && p.src[p.pos] == '.' {
		p.pos++
		if !digit() {
			return bad()
		}
		for digit() {
			p.pos++
		}
	}
	// Exponent: [eE][+-]? then at least one digit.
	if p.pos < len(p.src) && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}
		if !digit() {
			return bad()
		}
		for digit() {
			p.pos++
		}
	}
	f, err := strconv.ParseFloat(p.src[start:p.pos], 64)
	if err != nil {
		// A magnitude that overflows to ±Infinity (ErrRange) is still valid JSON;
		// ParseFloat returns the correct ±Inf. Any other error is a syntax error.
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			return mkundef(), &jsonError{"Invalid number in JSON"}
		}
	}
	return mknum(f), nil
}

func (p *jsonParser) parseString() (string, error) {
	p.pos++ // opening quote
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '"' {
			p.pos++
			return b.String(), nil
		}
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.src) {
				break
			}
			e := p.src[p.pos]
			switch e {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if p.pos+4 >= len(p.src) {
					return "", &jsonError{"Invalid unicode escape"}
				}
				var cp uint32
				for k := 1; k <= 4; k++ {
					cp = cp<<4 | hexVal(p.src[p.pos+k])
				}
				b.WriteString(string([]byte(string(rune(cp)))))
				p.pos += 4
			default:
				return "", &jsonError{"Invalid escape in JSON string"}
			}
			p.pos++
			continue
		}
		if c < 0x20 {
			// A JSON string may not contain an unescaped control character.
			return "", &jsonError{"Bad control character in JSON string"}
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", &jsonError{"Unterminated JSON string"}
}

func (p *jsonParser) parseArray() (Value, *jsonSrc, error) {
	p.pos++ // [
	arr := p.rt.newArray()
	ao := p.rt.objPtr(arr)
	src := &jsonSrc{composite: true}
	p.skipWS()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		return arr, src, nil
	}
	for {
		v, csrc, err := p.parse()
		if err != nil {
			return mkundef(), nil, err
		}
		p.rt.arraySet(ao, ao.arrLen, v)
		src.elems = append(src.elems, csrc)
		p.skipWS()
		if p.pos >= len(p.src) {
			return mkundef(), nil, &jsonError{"Unterminated JSON array"}
		}
		if p.src[p.pos] == ',' {
			p.pos++
			p.skipWS()
			continue
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return arr, src, nil
		}
		return mkundef(), nil, &jsonError{"Expected ',' or ']' in JSON array"}
	}
}

func (p *jsonParser) parseObject() (Value, *jsonSrc, error) {
	p.pos++ // {
	obj := p.rt.newPlainObject()
	o := p.rt.objPtr(obj)
	src := &jsonSrc{composite: true, props: map[string]*jsonSrc{}}
	p.skipWS()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return obj, src, nil
	}
	for {
		p.skipWS()
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return mkundef(), nil, &jsonError{"Expected string key in JSON object"}
		}
		key, err := p.parseString()
		if err != nil {
			return mkundef(), nil, err
		}
		p.skipWS()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return mkundef(), nil, &jsonError{"Expected ':' in JSON object"}
		}
		p.pos++
		v, csrc, err := p.parse()
		if err != nil {
			return mkundef(), nil, err
		}
		o.defineOwn(key, v, attrDefault)
		src.props[key] = csrc // last duplicate key wins, matching the value
		p.skipWS()
		if p.pos >= len(p.src) {
			return mkundef(), nil, &jsonError{"Unterminated JSON object"}
		}
		if p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return obj, src, nil
		}
		return mkundef(), nil, &jsonError{"Expected ',' or '}' in JSON object"}
	}
}

// jsonRevive implements InternalizeJSONProperty(holder, name): it recurses into
// a composite value's elements/properties, writing each revived result back via
// the observable operations — [[Delete]] when the reviver returns undefined,
// CreateDataProperty otherwise — then calls the reviver with (name, value,
// context). Get/length/ownKeys/delete/define all propagate abrupt completions
// (a Proxy trap that throws surfaces); an ordinary rejected define/delete (a
// non-configurable property) is a silent no-op, not a throw.
func (rt *Runtime) jsonRevive(holder Value, key string, reviver Value, src *jsonSrc) (Value, *ThrowError) {
	val, e := rt.getField(holder, key)
	if e != nil {
		return mkundef(), e
	}
	// writeBack applies step 2.b.iii/2.c.ii: delete when undefined, else
	// CreateDataProperty (a rejected ordinary define is a discarded boolean).
	writeBack := func(k Value, nv Value) *ThrowError {
		if nv.IsUndefined() {
			if _, e := rt.deleteElement(val, k); e != nil {
				return e
			}
			return nil
		}
		if e := rt.createDataProperty(val, k, nv); e != nil && !e.rejected {
			return e
		}
		return nil
	}
	if val.IsObjectType() || val.Type() == TArr {
		if rt.isArrayValue(val) {
			n, e := rt.lengthOf(val) // LengthOfArrayLike = ToLength(Get(val,"length"))
			if e != nil {
				return mkundef(), e
			}
			for i := 0; i < n; i++ {
				// The child's parse record is the one recorded at this index; an index
				// beyond the originally-parsed length (grown during revival) has none.
				var csrc *jsonSrc
				if src != nil && i < len(src.elems) {
					csrc = src.elems[i]
				}
				nv, e := rt.jsonRevive(val, numberToString(float64(i)), reviver, csrc)
				if e != nil {
					return mkundef(), e
				}
				if e := writeBack(mknum(float64(i)), nv); e != nil {
					return mkundef(), e
				}
			}
		} else {
			keys, e := rt.enumerableOwnKeysE(val) // EnumerableOwnPropertyNames (proxy-aware)
			if e != nil {
				return mkundef(), e
			}
			for _, k := range keys {
				var csrc *jsonSrc
				if src != nil && src.props != nil {
					csrc = src.props[k]
				}
				nv, e := rt.jsonRevive(val, k, reviver, csrc)
				if e != nil {
					return mkundef(), e
				}
				if e := writeBack(rt.newString(k), nv); e != nil {
					return mkundef(), e
				}
			}
		}
	}
	// The reviver's third argument is a context object (the JSON source-text-access
	// proposal, adopted in V8). Its "source" property is the value's source text —
	// present only for a primitive value that came straight from the parse (a
	// composite value, or one created/replaced during revival, has none).
	ctx := rt.newPlainObject()
	if src != nil && !src.composite && !(val.IsObjectType() || val.Type() == TArr) {
		rt.objPtr(ctx).defineOwn("source", rt.newString(src.text), attrDefault)
	}
	return rt.callValue(reviver, holder, []Value{rt.newString(key), val, ctx})
}
