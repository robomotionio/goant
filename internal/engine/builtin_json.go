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
		} else if replacer.Type() == TArr {
			ro := rt.objPtr(replacer)
			st.allow = map[string]bool{}
			for i := uint32(0); i < ro.arrLen; i++ {
				k, _ := rt.getElement(replacer, mknum(float64(i)))
				if k.IsString() || k.Type() == TNum {
					s, _ := rt.toStringValue(k)
					st.allow[string(rt.strBytes(s))] = true
				}
			}
		}
		// space
		switch sp := arg(args, 2); {
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
		v, perr := p.parse()
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
			return rt.jsonRevive(holder, "", reviver)
		}
		return v, nil
	})

	rt.objPtr(rt.global).defineOwn("JSON", json, attrWritable|attrConfigurable)
}

// ---- stringify ----

type jsonStringifier struct {
	rt         *Runtime
	replacerFn Value
	allow      map[string]bool
	gap        string
	indent     string
}

func (st *jsonStringifier) str(key string, holder Value, indent string) (string, bool, *ThrowError) {
	rt := st.rt
	v, e := rt.getField(holder, key)
	if e != nil {
		return "", false, e
	}
	// toJSON
	if v.IsObjectType() {
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

	switch v.Type() {
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
			return st.stringifyObject(v, indent)
		}
		return "", false, nil
	}
}

func (st *jsonStringifier) stringifyArray(v Value, indent string) (string, bool, *ThrowError) {
	rt := st.rt
	o := rt.objPtr(v)
	newIndent := indent + st.gap
	n := int(o.arrLen)
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
	o := rt.objPtr(v)
	newIndent := indent + st.gap
	var parts []string
	for _, k := range o.ownKeysEnumerable() {
		if st.allow != nil && !st.allow[k] {
			continue
		}
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
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
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
			if r < 0x20 {
				b.WriteString("\\u")
				const hexd = "0123456789abcdef"
				b.WriteByte(hexd[(r>>12)&0xF])
				b.WriteByte(hexd[(r>>8)&0xF])
				b.WriteByte(hexd[(r>>4)&0xF])
				b.WriteByte(hexd[r&0xF])
			} else {
				b.WriteRune(r)
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

func (p *jsonParser) parse() (Value, error) {
	p.skipWS()
	if p.pos >= len(p.src) {
		return mkundef(), &jsonError{"Unexpected end of JSON input"}
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
			return mkundef(), err
		}
		return p.rt.newString(s), nil
	case c == 't':
		return p.parseLit("true", mktrue())
	case c == 'f':
		return p.parseLit("false", mkfalse())
	case c == 'n':
		return p.parseLit("null", mknull())
	case c == '-' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	}
	return mkundef(), &jsonError{"Unexpected token in JSON"}
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
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.pos++
		} else {
			break
		}
	}
	f, err := strconv.ParseFloat(p.src[start:p.pos], 64)
	if err != nil {
		return mkundef(), &jsonError{"Invalid number in JSON"}
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
		b.WriteByte(c)
		p.pos++
	}
	return "", &jsonError{"Unterminated JSON string"}
}

func (p *jsonParser) parseArray() (Value, error) {
	p.pos++ // [
	arr := p.rt.newArray()
	ao := p.rt.objPtr(arr)
	p.skipWS()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		return arr, nil
	}
	for {
		v, err := p.parse()
		if err != nil {
			return mkundef(), err
		}
		p.rt.arraySet(ao, ao.arrLen, v)
		p.skipWS()
		if p.pos >= len(p.src) {
			return mkundef(), &jsonError{"Unterminated JSON array"}
		}
		if p.src[p.pos] == ',' {
			p.pos++
			p.skipWS()
			continue
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return arr, nil
		}
		return mkundef(), &jsonError{"Expected ',' or ']' in JSON array"}
	}
}

func (p *jsonParser) parseObject() (Value, error) {
	p.pos++ // {
	obj := p.rt.newPlainObject()
	o := p.rt.objPtr(obj)
	p.skipWS()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return obj, nil
	}
	for {
		p.skipWS()
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return mkundef(), &jsonError{"Expected string key in JSON object"}
		}
		key, err := p.parseString()
		if err != nil {
			return mkundef(), err
		}
		p.skipWS()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return mkundef(), &jsonError{"Expected ':' in JSON object"}
		}
		p.pos++
		v, err := p.parse()
		if err != nil {
			return mkundef(), err
		}
		o.defineOwn(key, v, attrDefault)
		p.skipWS()
		if p.pos >= len(p.src) {
			return mkundef(), &jsonError{"Unterminated JSON object"}
		}
		if p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return obj, nil
		}
		return mkundef(), &jsonError{"Expected ',' or '}' in JSON object"}
	}
}

// jsonRevive walks a parsed value applying the reviver (JSON.parse reviver).
func (rt *Runtime) jsonRevive(holder Value, key string, reviver Value) (Value, *ThrowError) {
	val, _ := rt.getField(holder, key)
	if val.Type() == TArr {
		o := rt.objPtr(val)
		for i := uint32(0); i < o.arrLen; i++ {
			nv, e := rt.jsonRevive(val, numberToString(float64(i)), reviver)
			if e != nil {
				return mkundef(), e
			}
			if nv.IsUndefined() {
				rt.setElement(val, mknum(float64(i)), mkundef())
			} else {
				rt.setElement(val, mknum(float64(i)), nv)
			}
		}
	} else if val.IsObjectType() {
		o := rt.objPtr(val)
		for _, k := range o.ownKeysEnumerable() {
			nv, e := rt.jsonRevive(val, k, reviver)
			if e != nil {
				return mkundef(), e
			}
			if nv.IsUndefined() {
				o.deleteOwn(k)
			} else {
				o.defineOwn(k, nv, attrDefault)
			}
		}
	}
	return rt.callValue(reviver, holder, []Value{rt.newString(key), val})
}
