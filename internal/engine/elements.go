package engine

// Property and element access used by the interpreter's GET_FIELD/PUT_FIELD/
// GET_ELEM/PUT_ELEM opcodes (ant ops/property.h + ant.c). Layers accessor
// invocation, array fast-path, string indexing, and array `.length` on top of
// the Phase 2 ordinary-object protocol.

// getField reads obj.name with accessor and exotic (array/string length)
// handling.
func (rt *Runtime) getField(obj Value, name string) (Value, *ThrowError) {
	if obj.IsNullish() {
		return mkundef(), rt.typeError("cannot read properties of " + rt.nullishName(obj) + " (reading '" + name + "')")
	}
	switch obj.Type() {
	case TArr:
		if name == "length" {
			o := rt.objPtr(obj)
			return mknum(float64(o.arrLen)), nil
		}
		// Canonical index keys reach array elements (e.g. arr["0"]).
		if idx, ok := canonicalIndex(name); ok {
			o := rt.objPtr(obj)
			if idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
				return o.arr[idx], nil
			}
			return mkundef(), nil
		}
	case TStr:
		if name == "length" {
			return mknum(float64(utf16Len(rt.strBytes(obj)))), nil
		}
	}
	if obj.IsObjectType() || obj.Type() == TTypedArray {
		holder, slot, found := rt.resolveProp(obj, name)
		if !found {
			return mkundef(), nil
		}
		if holder.isAccessorSlot(slot) {
			p := holder.shape.propAt(slot)
			if !p.hasGetter {
				return mkundef(), nil
			}
			return rt.callValue(p.getter, obj, nil)
		}
		return holder.slotGet(slot), nil
	}
	// Primitive property access resolves against the wrapper prototype, with the
	// primitive itself passed as the accessor receiver.
	if proto := rt.primitiveProto(obj); proto.IsObjectType() {
		holder, slot, found := rt.resolveProp(proto, name)
		if !found {
			return mkundef(), nil
		}
		if holder.isAccessorSlot(slot) {
			p := holder.shape.propAt(slot)
			if !p.hasGetter {
				return mkundef(), nil
			}
			return rt.callValue(p.getter, obj, nil)
		}
		return holder.slotGet(slot), nil
	}
	return mkundef(), nil
}

// primitiveProto returns the wrapper prototype for a primitive value.
func (rt *Runtime) primitiveProto(v Value) Value {
	switch v.Type() {
	case TStr:
		return rt.stringProto
	case TNum:
		return rt.numberProto
	case TBool:
		return rt.booleanProto
	case TSymbol:
		return rt.symbolProto
	default:
		return mkundef()
	}
}

// setField writes obj.name = v (ignoring rejection; see setFieldR for strict).
func (rt *Runtime) setField(obj Value, name string, v Value) *ThrowError {
	_, e := rt.setFieldR(obj, name, v)
	return e
}

// setFieldR writes obj.name = v, returning whether the write took effect (false
// = rejected by a non-writable data property, a setter-less accessor, or a
// non-extensible object). Callers in strict mode turn a false into a TypeError.
func (rt *Runtime) setFieldR(obj Value, name string, v Value) (bool, *ThrowError) {
	if obj.IsNullish() {
		return false, rt.typeError("cannot set properties of " + rt.nullishName(obj))
	}
	if obj.Type() == TArr && name == "length" {
		e := rt.setArrayLength(obj, v)
		return e == nil, e
	}
	if obj.IsObjectType() || obj.Type() == TTypedArray {
		if holder, slot, found := rt.resolveProp(obj, name); found && holder.isAccessorSlot(slot) {
			p := holder.shape.propAt(slot)
			if p.hasSetter {
				_, e := rt.callValue(p.setter, obj, []Value{v})
				return true, e
			}
			return false, nil // setter-less accessor: rejected
		}
		return rt.setProp(obj, name, v), nil
	}
	return true, nil // primitive receiver: ignored
}

// arrayIndexOf resolves a property key to an array index, accepting both
// numbers and canonical integer-index strings ("0", "123").
func (rt *Runtime) arrayIndexOf(key Value) (uint32, bool) {
	if idx, ok := arrayIndex(key); ok {
		return idx, true
	}
	if key.IsString() {
		return canonicalIndex(string(rt.strBytes(key)))
	}
	return 0, false
}

// canonicalIndex parses a canonical array-index string (no leading zeros,
// < 2^32-1).
func canonicalIndex(s string) (uint32, bool) {
	if s == "" || len(s) > 10 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + uint64(s[i]-'0')
	}
	if n >= 0xFFFFFFFF {
		return 0, false
	}
	return uint32(n), true
}

// getElement reads obj[key] with array/string fast paths.
func (rt *Runtime) getElement(obj Value, key Value) (Value, *ThrowError) {
	if obj.IsNullish() {
		return mkundef(), rt.typeError("cannot read properties of " + rt.nullishName(obj))
	}
	if key.IsSymbol() {
		return rt.getFieldSymbol(obj, key.handle()), nil
	}
	if idx, ok := rt.arrayIndexOf(key); ok {
		switch obj.Type() {
		case TArr:
			o := rt.objPtr(obj)
			if idx < o.arrLen && int(idx) < len(o.arr) {
				el := o.arr[idx]
				if el.IsEmpty() {
					return mkundef(), nil
				}
				return el, nil
			}
			return mkundef(), nil
		case TStr:
			b := rt.strBytes(obj)
			if int(idx) < utf16Len(b) {
				return rt.charAt(b, int(idx)), nil
			}
			return mkundef(), nil
		}
	}
	name, e := rt.propKeyString(key)
	if e != nil {
		return mkundef(), e
	}
	return rt.getField(obj, name)
}

// getFieldSymbol reads a symbol-keyed property through the prototype chain.
func (rt *Runtime) getFieldSymbol(obj Value, sym uint32) Value {
	cur := obj
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		if slot := o.shape.lookupSymbol(sym); slot >= 0 {
			if o.isAccessorSlot(uint32(slot)) {
				p := o.shape.propAt(uint32(slot))
				if p.hasGetter {
					v, _ := rt.callValue(p.getter, obj, nil)
					return v
				}
				return mkundef()
			}
			return o.slotGet(uint32(slot))
		}
		cur = o.proto
	}
	return mkundef()
}

func (rt *Runtime) hasFieldSymbol(obj Value, sym uint32) bool {
	cur := obj
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		if o.shape.lookupSymbol(sym) >= 0 {
			return true
		}
		cur = o.proto
	}
	return false
}

// setElement writes obj[key] = v with the array fast path.
func (rt *Runtime) setElement(obj Value, key, v Value) *ThrowError {
	if obj.IsNullish() {
		return rt.typeError("cannot set properties of " + rt.nullishName(obj))
	}
	if key.IsSymbol() {
		if o := rt.objPtr(obj); o != nil {
			o.defineOwnSymbol(key.handle(), v, attrDefault)
		}
		return nil
	}
	if idx, ok := rt.arrayIndexOf(key); ok && obj.Type() == TArr {
		rt.arraySet(rt.objPtr(obj), idx, v)
		return nil
	}
	name, e := rt.propKeyString(key)
	if e != nil {
		return e
	}
	return rt.setField(obj, name, v)
}

// ---- array helpers ----

// arraySet stores v at index idx, growing the backing store and length.
func (rt *Runtime) arraySet(o *object, idx uint32, v Value) {
	for uint32(len(o.arr)) <= idx {
		o.arr = append(o.arr, tEmpty)
	}
	o.arr[idx] = v
	if idx+1 > o.arrLen {
		o.arrLen = idx + 1
	}
}

func (rt *Runtime) setArrayLength(obj Value, v Value) *ThrowError {
	n, e := rt.toNumber(v)
	if e != nil {
		return e
	}
	newLen := uint32(n)
	if float64(newLen) != n {
		return rt.rangeError("Invalid array length")
	}
	o := rt.objPtr(obj)
	if newLen < uint32(len(o.arr)) {
		o.arr = o.arr[:newLen]
	}
	o.arrLen = newLen
	return nil
}

// charAt returns the one-UTF-16-unit string at index i.
func (rt *Runtime) charAt(b []byte, i int) Value {
	cu := utf16CodeUnitAt(b, i)
	return rt.newStringBytes(utf16ToWTF8([]uint16{uint16(cu)}))
}

// ---- key coercion ----

// arrayIndex returns key as a valid array index if it is a non-negative
// integer number below 2^32-1.
func arrayIndex(key Value) (uint32, bool) {
	if key.Type() != TNum {
		return 0, false
	}
	d := key.Number()
	if d < 0 || d != float64(uint32(d)) || uint32(d) == 0xFFFFFFFF {
		return 0, false
	}
	return uint32(d), true
}

// propKeyString coerces a property key Value to its string form (ToPropertyKey
// for string/number keys; symbol keys land with the Symbol type in Phase 5).
func (rt *Runtime) propKeyString(key Value) (string, *ThrowError) {
	if key.IsString() {
		return string(rt.strBytes(key)), nil
	}
	s, ok := rt.toStringPrimitive(key)
	if !ok {
		return "", rt.typeError("cannot convert property key to string")
	}
	return string(rt.strBytes(s)), nil
}

func (rt *Runtime) nullishName(v Value) string {
	if v.IsNull() {
		return "null"
	}
	return "undefined"
}
