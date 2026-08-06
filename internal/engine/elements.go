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
				return o.arrAt(idx), nil
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
			return mknum(float64(rt.strLen16(obj))), nil
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
				if i := o.argMap.index(name); i >= 0 {
					return o.argMap.get(i), nil
				}
				return o.slotGet(uint32(slot)), nil
			}
			// An Array in the prototype chain contributes its exotic "length" (which
			// is not a shape slot) — e.g. reading .length on Object.create([1,2,3]).
			if name == "length" && cur.Type() == TArr {
				return mknum(float64(o.arrLen)), nil
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
	// A write reaching an object that predates this invocation is a write the
	// next run would inherit. See invocation_dirty.go.
	rt.noteSharedMutation(obj)
	_, e := rt.setFieldR(obj, name, v)
	return e
}

// setFieldThrow performs Set(obj, name, v, true): like setField but a rejected
// write (a non-writable data property or a setterless accessor) is a TypeError
// rather than a silent no-op.
func (rt *Runtime) setFieldThrow(obj Value, name string, v Value) *ThrowError {
	ok, e := rt.setFieldR(obj, name, v)
	if e != nil {
		return e
	}
	if !ok {
		return rt.typeError("Cannot assign to read-only property '" + name + "'")
	}
	return nil
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
	rt.noteSharedMutation(obj)
	if obj.IsNullish() {
		return false, rt.typeError("cannot set properties of " + rt.nullishName(obj))
	}
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		return rt.proxySet(o.proxy, rt.internString(name), v, obj)
	}
	// A String exotic object's in-range character indices are non-writable,
	// non-configurable data properties: [[Set]] on one fails (a strict-mode
	// assignment then throws).
	if o := rt.objPtr(obj); o != nil && o.boxed.Type() == TStr {
		if fidx, isNum := canonicalNumericIndex(name); isNum {
			if idx, integral := integerIndex(fidx); integral && idx >= 0 && idx < rt.strLen16(o.boxed) {
				return false, nil
			}
		}
	}
	if obj.Type() == TArr && name == "length" {
		// OrdinarySet reads the own descriptor BEFORE the value is coerced, so a
		// length that is ALREADY non-writable refuses the assignment without ever
		// running the coercion. (One that becomes non-writable DURING the coercion
		// is caught inside setArrayLength instead.)
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
				return rt.proxySet(o.proxy, rt.internString(name), v, obj)
			}
			// A typed array reached on the chain (the receiver inherits from one)
			// answers with its OWN integer-indexed [[Set]] and the walk stops: a
			// canonical index it cannot address discards the write (10.4.5.5 step
			// 1.b), so an accessor further along — on %TypedArray%.prototype, say —
			// is never reached and nothing is created on the receiver. cur == obj is
			// the plain typed-array case, already handled by setElementR.
			if cur != obj && cur.Type() == TTypedArray && o.ta != nil {
				if fidx, isNum := canonicalNumericIndex(name); isNum {
					if idx, integral := integerIndex(fidx); !integral || idx < 0 || idx >= rt.taLength(o) {
						return true, nil
					}
					// Addressable: the typed array's own [[GetOwnProperty]] answers with a
					// writable data descriptor, so the write creates an own property on
					// the RECEIVER and the walk stops here. Falling through to setProp
					// would resume the chain and find an accessor further along — on
					// %TypedArray%.prototype, say — which the spec never reaches.
					return rt.setOnReceiver(obj, rt.internString(name), v)
				}
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
	// PutValue on a property reference with a primitive base performs
	// ToObject(base).[[Set]](name, W, base): the walk is the ordinary one — it
	// dispatches a Proxy's trap and runs an inherited setter with the PRIMITIVE
	// as `this` — and the final write fails because the Receiver is not an Object
	// (a strict `sym.p = x` then throws).
	return rt.ordinarySet(rt.primitiveProto(obj), rt.internString(name), v, obj)
}

// arrayIndexOf resolves a property key to an array index, accepting both
// numbers and canonical integer-index strings ("0", "123").
func (rt *Runtime) arrayIndexOf(key Value) (uint32, bool) {
	if idx, ok := arrayIndex(key); ok {
		return idx, true
	}
	if key.IsString() {
		return canonicalIndex(rt.strGo(key))
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

// typedArrayCanonicalHas implements the integer-indexed exotic [[HasProperty]]
// for a canonical numeric index key: such a key is an own check
// (IsValidIntegerIndex) that never consults the prototype. isCanon reports
// whether name is a CanonicalNumericIndexString at all; when false the caller
// falls back to OrdinaryHasProperty. obj must be a typed array.
func (rt *Runtime) typedArrayCanonicalHas(obj Value, name string) (has, isCanon bool) {
	if obj.Type() != TTypedArray {
		return false, false
	}
	fidx, canon := canonicalNumericIndex(name)
	if !canon {
		return false, false
	}
	if idx, integral := integerIndex(fidx); integral {
		if o := rt.objPtr(obj); o != nil {
			if _, live := rt.taGet(o, idx); live {
				return true, true
			}
		}
	}
	return false, true
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
				return o.arrAt(idx), nil
			}
			// Not in fast element storage: an index defined with non-default
			// attributes or as an accessor lives as a named property — fall through
			// to the named-property + prototype-chain lookup below.
		case TStr:
			if int(idx) < rt.strLen16(obj) {
				return rt.strCharAt(obj, int(idx)), nil
			}
			return mkundef(), nil
		default:
			// String exotic object (new String / a String subclass instance): an
			// index in range reads the wrapped character.
			if o := rt.objPtr(obj); o != nil && o.boxed.Type() == TStr {
				if int(idx) < rt.strLen16(o.boxed) {
					return rt.strCharAt(o.boxed, int(idx)), nil
				}
			}
		}
	}
	if obj.Type() == TTypedArray {
		// Integer-indexed exotic [[Get]]: a canonical numeric index string that does
		// not address a live element (the array-index fast path above returned any
		// in-bounds element) yields undefined WITHOUT consulting the prototype chain.
		// A number key is always canonical; a string key must round-trip through
		// CanonicalNumericIndexString ("-0"/"1.5"/"-1"/out-of-range qualify, "1e1"
		// or "length" do not — those remain ordinary named lookups).
		isCanon := key.IsNumber()
		if !isCanon && key.IsString() {
			_, isCanon = canonicalNumericIndex(rt.strGo(key))
		}
		if isCanon {
			return mkundef(), nil
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
		if o.proxy != nil {
			// A symbol read on a proxy routes through its [[Get]] trap (with the
			// original receiver), forwarding to the target when no trap is present.
			return rt.proxyGet(o.proxy, mkval(TSymbol, uint64(sym)), obj)
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
	// OrdinaryGet with a Receiver: only an accessor needs it, so this is exactly
	// getWithReceiver — including its handling of the exotic own properties (an
	// array's "length", a typed array's elements, a String object's indices) that
	// a shape-slot walk cannot see.
	return rt.getWithReceiver(base, pk, receiver)
}

// putSuperProp implements a super-property write (`super.x = v`): PutValue on a
// Super Reference performs base.[[Set]](key, v, this) — a setter on the super
// chain runs with `this`, and a writable data property is created/updated on
// the receiver (this), not on the base. It reports whether the write took
// effect (false for a refused write, so strict code can throw).
func (rt *Runtime) putSuperProp(base, key, v, receiver Value) (bool, *ThrowError) {
	if base.IsNullish() {
		return false, rt.typeError("cannot set properties of " + rt.nullishName(base))
	}
	pk, ke := rt.toPropertyKey(key)
	if ke != nil {
		return false, ke
	}
	return rt.ordinarySet(base, pk, v, receiver)
}

func (rt *Runtime) hasFieldSymbol(obj Value, sym uint32) bool {
	cur := obj
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		// A Proxy in the chain routes [[HasProperty]] through its trap (or forwards
		// to its target), including for a symbol key.
		if o.proxy != nil {
			has, _ := rt.proxyHas(o.proxy, mkval(TSymbol, uint64(sym)))
			return has
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
	_, e := rt.setElementR(obj, key, v)
	return e
}

// inheritedIndexedSet handles the OrdinarySet case where an index is not an own
// property of the receiver but the prototype chain (walked from `start`) holds
// an interceptor for it: an accessor (invoke its setter, or reject when
// setter-less) or a non-writable data property (reject). handled is false when
// no interceptor is found — the caller then creates an own element on the
// receiver (inherited writable data / absent both create own). Only called when
// rt.indexedProtoIntercept is set, so ordinary array growth pays nothing.
func (rt *Runtime) inheritedIndexedSet(receiver, start Value, idx uint32, v Value) (handled, ok bool, err *ThrowError) {
	name := strconv.Itoa(int(idx))
	for cur := start; ; {
		o := rt.objPtr(cur)
		if o == nil {
			return false, false, nil
		}
		if o.proxy != nil {
			b, e := rt.proxySet(o.proxy, rt.internString(name), v, receiver)
			return true, b, e
		}
		// A typed array on the chain answers with its own integer-indexed [[Set]]
		// and the walk stops. This branch is reached when the RECEIVER is an array
		// (the array-index fast path) — the plain-object receiver goes through
		// setFieldR's walk instead, which carries the same rule.
		if cur.Type() == TTypedArray && o.ta != nil {
			if idx >= uint32(rt.taLength(o)) {
				return true, true, nil // unaddressable: discarded, nothing created
			}
			return false, false, nil // addressable: ordinary create-on-receiver
		}
		if slot := o.shape.lookupInterned(name); slot >= 0 {
			if o.isAccessorSlot(uint32(slot)) {
				p := o.shape.propAt(uint32(slot))
				if p.hasSetter {
					_, e := rt.callValue(p.setter, receiver, []Value{v})
					return true, e == nil, e
				}
				return true, false, nil // setter-less accessor: rejected
			}
			if o.shape.attrsAt(uint32(slot))&attrWritable == 0 {
				return true, false, nil // inherited non-writable data: rejected
			}
			return false, false, nil // inherited writable data: create own on receiver
		}
		cur = o.proto
	}
}

// setElementR is [[Set]] for a computed/element key, additionally reporting
// whether the assignment took effect. It returns false (with no error) for a
// silently-refused write — a non-writable data property, a setter-less
// accessor, an add to a non-extensible object, or a frozen array element — so a
// strict-mode assignment can throw a TypeError on that result.
func (rt *Runtime) setElementR(obj Value, key, v Value) (bool, *ThrowError) {
	if obj.IsNullish() {
		return false, rt.typeError("cannot set properties of " + rt.nullishName(obj))
	}
	pk, ke := rt.toPropertyKey(key)
	if ke != nil {
		return false, ke
	}
	key = pk
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		return rt.proxySet(o.proxy, rt.toPropertyKeyValue(key), v, obj)
	}
	if key.IsSymbol() {
		// Ordinary [[Set]] for a symbol key: walk the chain for an accessor (call
		// its setter) or a Proxy; honor a non-writable data property (reject); an
		// own writable data property is updated in place (preserving its
		// attributes); otherwise a fresh own data property is created on obj.
		sym := key.handle()
		for cur := obj; ; {
			o := rt.objPtr(cur)
			if o == nil {
				break
			}
			if o.proxy != nil {
				return rt.proxySet(o.proxy, key, v, obj)
			}
			if slot := o.shape.lookupSymbol(sym); slot >= 0 {
				if o.isAccessorSlot(uint32(slot)) {
					p := o.shape.propAt(uint32(slot))
					if p.hasSetter {
						_, e := rt.callValue(p.setter, obj, []Value{v})
						return e == nil, e
					}
					return false, nil // setter-less accessor: rejected
				}
				if o.shape.attrsAt(uint32(slot))&attrWritable == 0 {
					return false, nil // non-writable data property: rejected
				}
				if cur == obj {
					o.slotSet(uint32(slot), v) // own writable: update value in place
					return true, nil
				}
				break // inherited writable data: create an own property on obj
			}
			cur = o.proto
		}
		if o := rt.objPtr(obj); o != nil {
			if !o.flags.extensible {
				return false, nil // cannot add to a non-extensible object
			}
			o.defineOwnSymbol(sym, v, attrDefault)
		}
		return true, nil
	}
	if idx, ok := rt.arrayIndexOf(key); ok && obj.Type() == TArr {
		o := rt.objPtr(obj)
		// An indexed write into an array older than this invocation is state the
		// next run inherits, exactly as a named one is. Noted here rather than in
		// arraySet, which is inlined into the interpreter's dispatch loop and
		// whose comment explains at length why nothing may be added to it.
		rt.noteSharedMutationOf(o)
		// Fast paths: a live in-range element, or an index at/past the current
		// length (no named index property lives there — a defineProperty on an
		// index extends the length past it). Otherwise a hole inside the logical
		// array may be shadowed by a named index property defined with attributes
		// (non-writable data rejects the write; an accessor invokes its setter) —
		// honor it; absent one, keep the array fast.
		if int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
			if o.flags.frozen {
				return false, nil // a frozen array's elements are non-writable ([[Set]] fails)
			}
			rt.arraySet(o, idx, v)
			return true, nil
		}
		// Past the live-element fast path the index would be a new own property;
		// a non-extensible array rejects it (unless a named index property already
		// lives there, updated via setFieldR below).
		if !o.flags.extensible && o.shape.lookupInterned(strconv.Itoa(int(idx))) < 0 {
			return false, nil
		}
		// When idx is not an own property, an inherited indexed accessor or
		// non-writable data property on the prototype chain intercepts the write
		// (OrdinarySet). Gated on the runtime flag so ordinary growth stays a direct
		// fast write; own named-index properties (checked below) take precedence.
		if rt.indexedProtoIntercept && o.shape.lookupInterned(strconv.Itoa(int(idx))) < 0 {
			if handled, ok, e := rt.inheritedIndexedSet(obj, o.proto, idx, v); e != nil || handled {
				return ok, e
			}
		}
		// A far index whose gap past the dense store would balloon the backing
		// slice spills to a named property (sparse array): length still tracks the
		// index, but we never allocate the intervening holes (e.g. a[2**32-2]=x).
		if idx > uint32(len(o.arr)) && idx-uint32(len(o.arr)) > maxDenseGap {
			name := strconv.Itoa(int(idx))
			if o.shape.lookupInterned(name) >= 0 {
				return rt.setFieldR(obj, name, v)
			}
			o.defineOwn(name, v, attrDefault)
			if idx+1 > o.arrLen {
				o.arrLen = idx + 1
			}
			return true, nil
		}
		if idx >= o.arrLen {
			rt.arraySet(o, idx, v)
			return true, nil
		}
		name := strconv.Itoa(int(idx))
		if o.shape.lookupInterned(name) >= 0 {
			return rt.setFieldR(obj, name, v)
		}
		rt.arraySet(o, idx, v)
		return true, nil
	}
	if obj.Type() == TTypedArray {
		fidx, isNum := key.Number(), key.IsNumber()
		if !isNum && key.IsString() {
			fidx, isNum = canonicalNumericIndex(rt.strGo(key))
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
					return false, e
				}
				if integral {
					rt.taSetBig(o, idx, bi)
				}
			} else {
				n, e := rt.toNumber(v)
				if e != nil {
					return false, e
				}
				if integral {
					rt.taSet(o, idx, n)
				}
			}
			// Integer-indexed exotic [[Set]] always reports success (an
			// out-of-range write is a silent no-op, never a strict-mode throw).
			return true, nil
		}
	}
	name, e := rt.propKeyString(key)
	if e != nil {
		return false, e
	}
	return rt.setFieldR(obj, name, v)
}

// ---- array helpers ----

// arraySet stores v at index idx, growing the backing store and length.
// maxDenseGap bounds how far past the materialized dense store an index write may
// extend the fast backing slice. A larger jump is stored as a named property so a
// sparse write near the 2^32 index ceiling can't balloon the slice (and OOM).
const maxDenseGap = 1 << 20

func (rt *Runtime) arraySet(o *object, idx uint32, v Value) {
	// Byte for byte what it was before the memory limit existed, and it must
	// stay that way. arraySet is inlined into runFrameBody — the interpreter's
	// single enormous dispatch loop — so anything added here grows that loop and
	// costs every script in the engine, not just the ones storing array
	// elements. A guarded call and even a lone extra compare measured ~9% across
	// unrelated workloads, which is how the cost of array accounting was found:
	// not in array code, but in the profile of an object-allocation benchmark.
	//
	// Array element storage is therefore charged nowhere. It is still COUNTED,
	// by liveePayload at the next sweep, so the limit is enforced correctly on
	// what an array retains; what is given up is only the ability to trigger a
	// collection on array growth alone. That needs a script whose memory is
	// almost entirely one array's elements and which allocates too few cells to
	// reach the count threshold — rare, and it costs a late stop rather than a
	// missed one.
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
	newLen, e := rt.arrayLengthValue(v)
	if e != nil {
		return false, e
	}
	// The coercion above is observable and precedes the writability check, so a
	// valueOf that makes `length` non-writable still runs before the assignment
	// is refused.
	if o := rt.objPtr(obj); o != nil && o.flags.arrLenNonWritable {
		return newLen == o.arrLen, nil
	}
	return rt.setArrayLengthTo(obj, newLen)
}

// arrayLengthValue is ArraySetLength steps 3-5: the value is coerced TWICE —
// once by ToUint32 and once by ToNumber — and a length that does not survive
// both identically is a RangeError. Both coercions are observable, so a valueOf
// that mutates the array runs exactly twice and in this order.
func (rt *Runtime) arrayLengthValue(v Value) (uint32, *ThrowError) {
	newLen, e := rt.toUint32(v)
	if e != nil {
		return 0, e
	}
	numberLen, e := rt.toNumber(v)
	if e != nil {
		return 0, e
	}
	if float64(newLen) != numberLen {
		return 0, rt.rangeError("Invalid array length")
	}
	return newLen, nil
}

// setArrayLengthTo applies an already-validated new length, shrinking the array
// as far as its non-configurable elements allow.
func (rt *Runtime) setArrayLengthTo(obj Value, newLen uint32) (bool, *ThrowError) {
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
		return rt.strGo(key), nil
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
	return rt.strGo(s), nil
}

// copyDataProps copies src's own enumerable properties (array indices, string
// keys, and symbol keys) into target, invoking getters (object spread / rest).
func (rt *Runtime) copyDataProps(target, src Value) *ThrowError {
	return rt.copyDataPropsExcluding(target, src, nil)
}

// copyDataPropsExcluding is CopyDataProperties with an excludedItems list: the
// keys a `{a, [k]: b, ...rest}` pattern already bound. They are skipped BEFORE
// [[GetOwnProperty]], so a Proxy source never sees a descriptor request for one.
func (rt *Runtime) copyDataPropsExcluding(target, src Value, excluded []Value) *ThrowError {
	// CopyDataProperties (7.3.25): ToObject(source), then for each own key in
	// [[OwnPropertyKeys]] order, if [[GetOwnProperty]] reports it enumerable,
	// CreateDataPropertyOrThrow(target, key, ? Get(source, key)). Every step goes
	// through the ordinary object internal methods so a Proxy source dispatches
	// its ownKeys / getOwnPropertyDescriptor / get traps (a primitive string
	// boxes to a wrapper whose enumerable own keys are its character indices).
	if src.IsNullish() {
		return nil
	}
	from, e := rt.toObjectValue(src)
	if e != nil {
		return e
	}
	keys, e := rt.ownKeyValues(from)
	if e != nil {
		return e
	}
	// excludedItems are property keys, but a computed one may still be the raw
	// number the pattern wrote (`{ [1]: a, ...rest }`), so normalise once.
	for i, ex := range excluded {
		if !ex.IsString() && !ex.IsSymbol() {
			excluded[i] = rt.toPropertyKeyValue(ex)
		}
	}
	for _, key := range keys {
		if len(excluded) > 0 {
			skip := false
			for _, ex := range excluded {
				if rt.sameValue(ex, key) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
		}
		enum, exists, e := rt.ownKeyEnumerable(from, key)
		if e != nil {
			return e
		}
		if !exists || !enum {
			continue
		}
		v, e := rt.getElement(from, key)
		if e != nil {
			return e
		}
		if e := rt.createDataProperty(target, key, v); e != nil {
			return e
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
