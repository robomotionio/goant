package engine

import (
	"math"
	"strconv"
)

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
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		return rt.proxyGet(o.proxy, rt.internString(name), obj)
	}
	switch obj.Type() {
	case TArr:
		if name == "length" {
			o := rt.objPtr(obj)
			return mknum(float64(o.arrLen)), nil
		}
		// Canonical index keys reach array elements (e.g. arr["0"]). A hit in fast
		// storage wins; otherwise fall through to the ordinary [[Get]] below, which
		// finds an index defined with non-default attributes (stored as a named
		// property) or an inherited one, then the prototype chain.
		if idx, ok := canonicalIndex(name); ok {
			o := rt.objPtr(obj)
			if idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
				return o.arr[idx], nil
			}
		}
	case TTypedArray:
		// A canonical numeric index reads the element directly (undefined when
		// out of range or the buffer is detached) and never consults the
		// prototype chain. Non-index names ("length", "byteLength", …) fall
		// through to the ordinary [[Get]] below.
		if idx, ok := canonicalIndex(name); ok {
			v, _ := rt.taGet(rt.objPtr(obj), int(idx))
			return v, nil
		}
	case TStr:
		if name == "length" {
			return mknum(float64(utf16Len(rt.strBytes(obj)))), nil
		}
	}
	if obj.IsObjectType() || obj.Type() == TTypedArray {
		// Ordinary [[Get]] walking the prototype chain with the original receiver.
		// A Proxy encountered in the chain dispatches its [[Get]] trap (receiver
		// stays obj), which resolveProp would otherwise walk straight past.
		idx, isIdx := canonicalIndex(name)
		cur := obj
		for depth := 0; depth < maxProtoChainDepth; depth++ {
			o := rt.objPtr(cur)
			if o == nil {
				return mkundef(), nil
			}
			if o.proxy != nil {
				return rt.proxyGet(o.proxy, rt.internString(name), obj)
			}
			// An index in a prototype's element backing store (e.g. Array.prototype[0])
			// is inherited too — the shape lookup below only sees named properties.
			if isIdx {
				if v, ok := rt.ownIndexElement(o, cur, idx); ok {
					return v, nil
				}
			}
			if slot := o.shape.lookupInterned(name); slot >= 0 {
				if o.isAccessorSlot(uint32(slot)) {
					p := o.shape.propAt(uint32(slot))
					if !p.hasGetter {
						return mkundef(), nil
					}
					return rt.callValue(p.getter, obj, nil)
				}
				return o.slotGet(uint32(slot)), nil
			}
			cur = o.proto
		}
		return mkundef(), nil
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
	case TBigInt:
		return rt.bigintProto
	default:
		return mkundef()
	}
}

// setField writes obj.name = v (ignoring rejection; see setFieldR for strict).
func (rt *Runtime) setField(obj Value, name string, v Value) *ThrowError {
	_, e := rt.setFieldR(obj, name, v)
	return e
}

// createDataProperty implements CreateDataPropertyOrThrow(O, P, V): defines a
// fresh enumerable/writable/configurable own data property, throwing a TypeError
// if [[DefineOwnProperty]] is rejected (non-extensible target, or a
// non-configurable existing property). On a Proxy this uses the defineProperty
// trap (a plain [[Set]] would wrongly fire the set trap).
func (rt *Runtime) createDataProperty(obj, key, v Value) *ThrowError {
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		desc := rt.newPlainObject()
		do := rt.objPtr(desc)
		do.defineOwn("value", v, attrDefault)
		do.defineOwn("writable", mktrue(), attrDefault)
		do.defineOwn("enumerable", mktrue(), attrDefault)
		do.defineOwn("configurable", mktrue(), attrDefault)
		return rt.proxyDefineProperty(o.proxy, rt.toPropertyKeyValue(key), desc)
	}
	// Fast path: a fresh index on an ordinary extensible array with no shadowing
	// named/accessor slot is a plain create with default element attributes.
	if idx, ok := rt.arrayIndexOf(key); ok && obj.Type() == TArr {
		o := rt.objPtr(obj)
		if o.flags.extensible && o.shape.lookupInterned(strconv.Itoa(int(idx))) < 0 {
			rt.arraySet(o, idx, v)
			return nil
		}
	}
	// General case: ordinary [[DefineOwnProperty]] with a default data descriptor,
	// which throws when the definition is rejected.
	desc := rt.newPlainObject()
	do := rt.objPtr(desc)
	do.defineOwn("value", v, attrDefault)
	do.defineOwn("writable", mktrue(), attrDefault)
	do.defineOwn("enumerable", mktrue(), attrDefault)
	do.defineOwn("configurable", mktrue(), attrDefault)
	return rt.objectDefinePropertyKey(obj, rt.toPropertyKeyValue(key), desc)
}

// setFieldR writes obj.name = v, returning whether the write took effect (false
// = rejected by a non-writable data property, a setter-less accessor, or a
// non-extensible object). Callers in strict mode turn a false into a TypeError.
func (rt *Runtime) setFieldR(obj Value, name string, v Value) (bool, *ThrowError) {
	if obj.IsNullish() {
		return false, rt.typeError("cannot set properties of " + rt.nullishName(obj))
	}
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		return true, rt.proxySet(o.proxy, rt.internString(name), v, obj)
	}
	if obj.Type() == TArr && name == "length" {
		// A non-writable length rejects any [[Set]] (OpPutField throws in strict
		// mode / stays silent otherwise; mutators check the rejection explicitly).
		if o := rt.objPtr(obj); o != nil && o.flags.arrLenNonWritable {
			return false, nil
		}
		return rt.setArrayLength(obj, v)
	}
	if obj.IsObjectType() || obj.Type() == TTypedArray {
		// Ordinary [[Set]]: walk the chain for an accessor (call its setter with
		// this=obj) or a Proxy (dispatch its [[Set]] trap with receiver=obj). A
		// data property or the chain end falls through to setProp, which
		// creates/updates the own property on the receiver.
		cur := obj
		for depth := 0; depth < maxProtoChainDepth; depth++ {
			o := rt.objPtr(cur)
			if o == nil {
				break
			}
			if o.proxy != nil {
				return true, rt.proxySet(o.proxy, rt.internString(name), v, obj)
			}
			if slot := o.shape.lookupInterned(name); slot >= 0 {
				if o.isAccessorSlot(uint32(slot)) {
					p := o.shape.propAt(uint32(slot))
					if p.hasSetter {
						_, e := rt.callValue(p.setter, obj, []Value{v})
						return true, e
					}
					return false, nil // setter-less accessor: rejected
				}
				break // data property: fall through to setProp
			}
			cur = o.proto
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

// canonicalNumericIndex implements CanonicalNumericIndexString(s): the numeric
// value of s when s is the exact canonical string form of a Number ("-0",
// "1.1", "-1", "NaN", "Infinity" all qualify; "1e2", " 1", "" do not). Typed
// arrays treat every such key as an element key (never an ordinary property),
// even when it does not address a live element.
func canonicalNumericIndex(s string) (float64, bool) {
	if s == "-0" {
		return math.Copysign(0, -1), true
	}
	n := stringToNumber(s)
	if numberToString(n) == s {
		return n, true
	}
	return 0, false
}

// integerIndex reports whether f is a valid integer index value (a non-negative
// integer that is not -0), returning it as an int.
func integerIndex(f float64) (int, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < 0 {
		return 0, false
	}
	if f == 0 && math.Signbit(f) {
		return 0, false
	}
	return int(f), true
}

// getElement reads obj[key] with array/string fast paths.
func (rt *Runtime) getElement(obj Value, key Value) (Value, *ThrowError) {
	if obj.IsNullish() {
		return mkundef(), rt.typeError("cannot read properties of " + rt.nullishName(obj))
	}
	if pk, ke := rt.toPropertyKey(key); ke != nil {
		return mkundef(), ke
	} else {
		key = pk
	}
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		return rt.proxyGet(o.proxy, rt.toPropertyKeyValue(key), obj)
	}
	if key.IsSymbol() {
		return rt.getFieldSymbol(obj, key.handle())
	}
	if idx, ok := rt.arrayIndexOf(key); ok {
		switch obj.Type() {
		case TTypedArray:
			if v, ok := rt.taGet(rt.objPtr(obj), int(idx)); ok {
				return v, nil
			}
			return mkundef(), nil
		case TArr:
			o := rt.objPtr(obj)
			if idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
				return o.arr[idx], nil
			}
			// Not in fast element storage: an index defined with non-default
			// attributes or as an accessor lives as a named property — fall through
			// to the named-property + prototype-chain lookup below.
		case TStr:
			b := rt.strBytes(obj)
			if int(idx) < utf16Len(b) {
				return rt.charAt(b, int(idx)), nil
			}
			return mkundef(), nil
		default:
			// String exotic object (new String / a String subclass instance): an
			// index in range reads the wrapped character.
			if o := rt.objPtr(obj); o != nil && o.boxed.Type() == TStr {
				b := rt.strBytes(o.boxed)
				if int(idx) < utf16Len(b) {
					return rt.charAt(b, int(idx)), nil
				}
			}
		}
	}
	name, e := rt.propKeyString(key)
	if e != nil {
		return mkundef(), e
	}
	return rt.getField(obj, name)
}

// getFieldSymbol reads a symbol-keyed property through the prototype chain. For
// a primitive receiver the walk begins at its wrapper prototype (so e.g.
// ""[Symbol.iterator] resolves through String.prototype).
func (rt *Runtime) getFieldSymbol(obj Value, sym uint32) (Value, *ThrowError) {
	cur := obj
	if !obj.IsObjectType() && obj.Type() != TTypedArray && !obj.IsNullish() {
		cur = rt.primitiveProto(obj)
	}
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		if slot := o.shape.lookupSymbol(sym); slot >= 0 {
			if o.isAccessorSlot(uint32(slot)) {
				p := o.shape.propAt(uint32(slot))
				if p.hasGetter {
					return rt.callValue(p.getter, obj, nil)
				}
				return mkundef(), nil
			}
			return o.slotGet(uint32(slot)), nil
		}
		cur = o.proto
	}
	return mkundef(), nil
}

// getSuperProp implements a super-property read (`super.x` / `super[k]`):
// GetV(base, key) with the original `this` as the accessor receiver. base is the
// home object's [[Prototype]]; the lookup walks base's own chain, and any getter
// runs with `receiver` (this), not base.
func (rt *Runtime) getSuperProp(base, key, receiver Value) (Value, *ThrowError) {
	if base.IsNullish() {
		return mkundef(), rt.typeError("cannot read properties of " + rt.nullishName(base))
	}
	pk, ke := rt.toPropertyKey(key)
	if ke != nil {
		return mkundef(), ke
	}
	if pk.IsSymbol() {
		sym := pk.handle()
		for cur, depth := base, 0; depth < maxProtoChainDepth; depth++ {
			o := rt.objPtr(cur)
			if o == nil {
				break
			}
			if o.proxy != nil {
				return rt.proxyGet(o.proxy, rt.toPropertyKeyValue(pk), receiver)
			}
			if slot := o.shape.lookupSymbol(sym); slot >= 0 {
				if o.isAccessorSlot(uint32(slot)) {
					if p := o.shape.propAt(uint32(slot)); p.hasGetter {
						return rt.callValue(p.getter, receiver, nil)
					}
					return mkundef(), nil
				}
				return o.slotGet(uint32(slot)), nil
			}
			cur = o.proto
		}
		return mkundef(), nil
	}
	name, e := rt.propKeyString(pk)
	if e != nil {
		return mkundef(), e
	}
	for cur, depth := base, 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		if o.proxy != nil {
			return rt.proxyGet(o.proxy, rt.internString(name), receiver)
		}
		if cur.Type() == TArr {
			if idx, ok := canonicalIndex(name); ok && idx < o.arrLen &&
				int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
				return o.arr[idx], nil
			}
		}
		if slot := o.shape.lookupInterned(name); slot >= 0 {
			if o.isAccessorSlot(uint32(slot)) {
				if p := o.shape.propAt(uint32(slot)); p.hasGetter {
					return rt.callValue(p.getter, receiver, nil)
				}
				return mkundef(), nil
			}
			return o.slotGet(uint32(slot)), nil
		}
		cur = o.proto
	}
	return mkundef(), nil
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

// toPropertyKey implements ToPropertyKey: an object key is coerced via
// ToPrimitive(string) so a boxed Symbol/String/Number becomes its primitive
// (e.g. Object(sym) used as a key resolves to the symbol, not "Symbol(...)").
func (rt *Runtime) toPropertyKey(key Value) (Value, *ThrowError) {
	if !key.IsObjectType() {
		return key, nil
	}
	return rt.toPrimitive(key, "string")
}

// setElement writes obj[key] = v with the array fast path.
func (rt *Runtime) setElement(obj Value, key, v Value) *ThrowError {
	if obj.IsNullish() {
		return rt.typeError("cannot set properties of " + rt.nullishName(obj))
	}
	pk, ke := rt.toPropertyKey(key)
	if ke != nil {
		return ke
	}
	key = pk
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		return rt.proxySet(o.proxy, rt.toPropertyKeyValue(key), v, obj)
	}
	if key.IsSymbol() {
		if o := rt.objPtr(obj); o != nil {
			o.defineOwnSymbol(key.handle(), v, attrDefault)
		}
		return nil
	}
	if idx, ok := rt.arrayIndexOf(key); ok && obj.Type() == TArr {
		o := rt.objPtr(obj)
		// Fast paths: a live in-range element, or an index at/past the current
		// length (no named index property lives there — a defineProperty on an
		// index extends the length past it). Otherwise a hole inside the logical
		// array may be shadowed by a named index property defined with attributes
		// (non-writable data rejects the write; an accessor invokes its setter) —
		// honor it; absent one, keep the array fast.
		if int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
			rt.arraySet(o, idx, v)
			return nil
		}
		// A far index whose gap past the dense store would balloon the backing
		// slice spills to a named property (sparse array): length still tracks the
		// index, but we never allocate the intervening holes (e.g. a[2**32-2]=x).
		if idx > uint32(len(o.arr)) && idx-uint32(len(o.arr)) > maxDenseGap {
			name := strconv.Itoa(int(idx))
			if o.shape.lookupInterned(name) >= 0 {
				_, e := rt.setFieldR(obj, name, v)
				return e
			}
			o.defineOwn(name, v, attrDefault)
			if idx+1 > o.arrLen {
				o.arrLen = idx + 1
			}
			return nil
		}
		if idx >= o.arrLen {
			rt.arraySet(o, idx, v)
			return nil
		}
		name := strconv.Itoa(int(idx))
		if o.shape.lookupInterned(name) >= 0 {
			_, e := rt.setFieldR(obj, name, v)
			return e
		}
		rt.arraySet(o, idx, v)
		return nil
	}
	if obj.Type() == TTypedArray {
		fidx, isNum := key.Number(), key.IsNumber()
		if !isNum && key.IsString() {
			fidx, isNum = canonicalNumericIndex(string(rt.strBytes(key)))
		}
		if isNum {
			// Integer-indexed exotic [[Set]]: coerce the value first (this can
			// throw), then write only when the key addresses a live in-bounds
			// integer index. A canonical numeric key that is not a valid index
			// (fractional, negative, -0, NaN, out of range, or detached) is a
			// silent no-op — it never becomes an ordinary named property.
			o := rt.objPtr(obj)
			idx, integral := integerIndex(fidx)
			if o.ta != nil && isBigIntKind(o.ta.kind) {
				bi, e := rt.toBigInt(v)
				if e != nil {
					return e
				}
				if integral {
					rt.taSetBig(o, idx, bi)
				}
			} else {
				n, e := rt.toNumber(v)
				if e != nil {
					return e
				}
				if integral {
					rt.taSet(o, idx, n)
				}
			}
			return nil
		}
	}
	name, e := rt.propKeyString(key)
	if e != nil {
		return e
	}
	return rt.setField(obj, name, v)
}

// ---- array helpers ----

// arraySet stores v at index idx, growing the backing store and length.
// maxDenseGap bounds how far past the materialized dense store an index write may
// extend the fast backing slice. A larger jump is stored as a named property so a
// sparse write near the 2^32 index ceiling can't balloon the slice (and OOM).
const maxDenseGap = 1 << 20

func (rt *Runtime) arraySet(o *object, idx uint32, v Value) {
	for uint32(len(o.arr)) <= idx {
		o.arr = append(o.arr, tEmpty)
	}
	o.arr[idx] = v
	if idx+1 > o.arrLen {
		o.arrLen = idx + 1
	}
}

// setArrayLength implements ArraySetLength for a plain value. Returns ok=false
// (no error) when a non-configurable index in [newLen, oldLen) blocks the shrink
// (length is clamped just above it); an invalid length value is a RangeError.
func (rt *Runtime) setArrayLength(obj Value, v Value) (bool, *ThrowError) {
	n, e := rt.toNumber(v)
	if e != nil {
		return false, e
	}
	newLen := uint32(n)
	if float64(newLen) != n {
		return false, rt.rangeError("Invalid array length")
	}
	o := rt.objPtr(obj)
	if newLen >= o.arrLen {
		o.arrLen = newLen
		return true, nil
	}
	// Shrinking: the fast arr[] elements are always configurable, but an index
	// defined with non-default attributes lives in the shape and may be
	// non-configurable. Find the highest such blocking index in [newLen, oldLen).
	blocked := int64(-1)
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		if p.key.sym {
			continue
		}
		if idx, ok := canonicalIndex(p.key.str); ok && idx >= newLen && idx < o.arrLen {
			if p.attrs&attrConfigurable == 0 && int64(idx) > blocked {
				blocked = int64(idx)
			}
		}
	}
	effective := newLen
	ok := true
	if blocked >= 0 {
		effective = uint32(blocked) + 1
		ok = false
	}
	// Delete configurable index properties at or above the effective length.
	for i := 0; i < o.shape.count(); {
		p := &o.shape.props[i]
		if !p.key.sym {
			if idx, isIdx := canonicalIndex(p.key.str); isIdx && idx >= effective && idx < o.arrLen {
				if o.deleteOwn(p.key.str) {
					continue // shape shifted; re-check this slot
				}
			}
		}
		i++
	}
	if int(effective) < len(o.arr) {
		o.arr = o.arr[:effective]
	}
	o.arrLen = effective
	return ok, nil
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
	// ToPropertyKey: an object key is taken to a primitive (string hint) first,
	// honoring Symbol.toPrimitive / valueOf / toString.
	if key.IsObjectType() || key.Type() == TTypedArray {
		p, e := rt.toPrimitive(key, "string")
		if e != nil {
			return "", e
		}
		key = p
	}
	s, ok := rt.toStringPrimitive(key)
	if !ok {
		return "", rt.typeError("cannot convert property key to string")
	}
	return string(rt.strBytes(s)), nil
}

// copyDataProps copies src's own enumerable properties (array indices, string
// keys, and symbol keys) into target, invoking getters (object spread / rest).
func (rt *Runtime) copyDataProps(target, src Value) *ThrowError {
	if src.IsNullish() {
		return nil
	}
	so := rt.objPtr(src)
	if so == nil {
		// Primitive (e.g. string): spread its indexed characters.
		if src.IsString() {
			n := utf16Len(rt.strBytes(src))
			for i := 0; i < n; i++ {
				v, _ := rt.getElement(src, mknum(float64(i)))
				rt.setElement(target, mknum(float64(i)), v)
			}
		}
		return nil
	}
	if src.Type() == TArr {
		for i := uint32(0); i < so.arrLen; i++ {
			if int(i) < len(so.arr) && !so.arr[i].IsEmpty() {
				rt.setElement(target, mknum(float64(i)), so.arr[i])
			}
		}
	}
	for _, k := range so.ownKeysEnumerable() {
		v, e := rt.getField(src, k)
		if e != nil {
			return e
		}
		if e := rt.setField(target, k, v); e != nil {
			return e
		}
	}
	for _, off := range so.ownSymbolKeys() {
		if d := so.ownDescriptorSym(off); d.exists && d.enumerable {
			v, e := rt.getFieldSymbol(src, off)
			if e != nil {
				return e
			}
			if o := rt.objPtr(target); o != nil {
				o.defineOwnSymbol(off, v, attrDefault)
			}
		}
	}
	return nil
}

func (rt *Runtime) nullishName(v Value) string {
	if v.IsNull() {
		return "null"
	}
	return "undefined"
}
