package engine

// structuredClone (HTML StructuredSerialize + StructuredDeserialize, done in one
// pass since there is no wire between the two ends here).
//
// A host runtime needs it for the same reason a browser does: passing a value
// somewhere it must not be shared — a message, a snapshot, a state store that
// has to stop changing under its owner. JSON round-tripping is the usual
// substitute and it is wrong in three ways that matter: it loses Map, Set, Date,
// RegExp and typed arrays, it throws on a cycle instead of preserving it, and it
// silently drops undefined.
//
// What cannot be cloned is refused rather than approximated. A function or a
// symbol has no serialized form, and returning some stand-in for it would put a
// value in the copy that the original never had.

import "strconv"

// structuredCloneError is the DataCloneError equivalent. There is no
// DOMException here, so it surfaces as a TypeError carrying the same message a
// browser gives, which is what a script checking for the failure will match on.
func (rt *Runtime) structuredCloneError(what string) *ThrowError {
	return rt.typeError(what + " could not be cloned")
}

// structuredClone deep-copies v, preserving identity and cycles: an object
// reached twice in the input is one object in the output.
func (rt *Runtime) structuredClone(v Value, memo map[Value]Value) (Value, *ThrowError) {
	switch v.Type() {
	case TUndef, TNull, TBool, TNum, TStr, TBigInt:
		// Primitives are already values; a string cell is immutable, so sharing
		// it is not sharing state.
		return v, nil
	case TSymbol:
		return mkundef(), rt.structuredCloneError("A symbol")
	case TFunc, TCFunc:
		return mkundef(), rt.structuredCloneError("A function")
	}
	if c, ok := memo[v]; ok {
		return c, nil
	}
	o := rt.objPtr(v)
	if o == nil {
		return v, nil
	}
	if o.flags.isCallable {
		return mkundef(), rt.structuredCloneError("A function")
	}
	if o.proxy != nil {
		// A Proxy has no state of its own to copy, and cloning what its traps
		// answer would produce an object with the target's contents and none of
		// its behaviour.
		return mkundef(), rt.structuredCloneError("A Proxy")
	}
	if o.promise() != nil {
		return mkundef(), rt.structuredCloneError("A Promise")
	}

	switch {
	case o.brandID() == brandDate:
		d, e := rt.NewDate(o.boxed.Number())
		if e != nil {
			return mkundef(), rt.typeError(e.Error())
		}
		memo[v] = d
		return d, nil

	case o.regex() != nil:
		src, e := rt.getField(v, "source")
		if e != nil {
			return mkundef(), e
		}
		flags, e := rt.getField(v, "flags")
		if e != nil {
			return mkundef(), e
		}
		ctor, e := rt.getField(rt.global, "RegExp")
		if e != nil {
			return mkundef(), e
		}
		c, e := rt.construct(ctor, []Value{src, flags})
		if e != nil {
			return mkundef(), e
		}
		// lastIndex is state, and a clone that resets it would make the copy
		// behave differently from the original on its very next exec.
		li, e := rt.getField(v, "lastIndex")
		if e == nil {
			_ = rt.setField(c, "lastIndex", li)
		}
		memo[v] = c
		return c, nil

	case o.abObj:
		return rt.cloneArrayBuffer(v, o, memo)

	case o.ta != nil:
		return rt.cloneTypedArray(v, o, memo)

	case o.dv() != nil:
		return rt.cloneDataView(v, o, memo)

	case o.coll() != nil:
		return rt.cloneCollection(v, o, memo)

	case o.brandID() == brandError:
		return rt.cloneError(v, memo)
	}

	if isArr, e := rt.isArrayE(v); e != nil {
		return mkundef(), e
	} else if isArr {
		return rt.cloneArray(v, memo)
	}
	return rt.clonePlainObject(v, o, memo)
}

// cloneArrayBuffer copies the bytes. Detached buffers are refused, matching the
// browser: there is nothing left to copy, and a zero-length stand-in would hide
// the mistake.
func (rt *Runtime) cloneArrayBuffer(v Value, o *object, memo map[Value]Value) (Value, *ThrowError) {
	if o.abuf == nil && o.extend().abMax > 0 {
		return mkundef(), rt.structuredCloneError("A detached ArrayBuffer")
	}
	if o.extend().abShared {
		// A SharedArrayBuffer is shared BY DESIGN: the algorithm passes the same
		// buffer through rather than copying it.
		memo[v] = v
		return v, nil
	}
	name := "ArrayBuffer"
	ctor, e := rt.getField(rt.global, name)
	if e != nil {
		return mkundef(), e
	}
	c, e := rt.construct(ctor, []Value{mknum(float64(len(o.abuf)))})
	if e != nil {
		return mkundef(), e
	}
	if co := rt.objPtr(c); co != nil {
		copy(co.abuf, o.abuf)
	}
	memo[v] = c
	return c, nil
}

// cloneTypedArray clones the view's buffer (through the memo, so two views over
// one buffer stay two views over one buffer) and rebuilds the view on it.
func (rt *Runtime) cloneTypedArray(v Value, o *object, memo map[Value]Value) (Value, *ThrowError) {
	if rt.taDetached(o) {
		return mkundef(), rt.structuredCloneError("A detached TypedArray")
	}
	buf, e := rt.structuredClone(o.ta.buf, memo)
	if e != nil {
		return mkundef(), e
	}
	name := typedArrayName(o.ta.kind)
	ctor, terr := rt.getField(rt.global, name)
	if terr != nil {
		return mkundef(), terr
	}
	args := []Value{buf, mknum(float64(o.ta.byteOffset))}
	if !o.ta.track {
		args = append(args, mknum(float64(o.ta.length)))
	}
	c, terr := rt.construct(ctor, args)
	if terr != nil {
		return mkundef(), terr
	}
	memo[v] = c
	return c, nil
}

// cloneDataView is cloneTypedArray for the untyped view.
func (rt *Runtime) cloneDataView(v Value, o *object, memo map[Value]Value) (Value, *ThrowError) {
	d := o.dv()
	buf, e := rt.structuredClone(d.buf, memo)
	if e != nil {
		return mkundef(), e
	}
	ctor, terr := rt.getField(rt.global, "DataView")
	if terr != nil {
		return mkundef(), terr
	}
	args := []Value{buf, mknum(float64(d.byteOffset))}
	if !d.track {
		args = append(args, mknum(float64(d.byteLength)))
	}
	c, terr := rt.construct(ctor, args)
	if terr != nil {
		return mkundef(), terr
	}
	memo[v] = c
	return c, nil
}

// cloneCollection copies a Map or a Set. The clone is registered in the memo
// BEFORE its entries are cloned, so a map holding itself terminates.
func (rt *Runtime) cloneCollection(v Value, o *object, memo map[Value]Value) (Value, *ThrowError) {
	c := o.coll()
	if c.weak {
		// A WeakMap's contents are not enumerable, by design: what it holds is
		// only visible to someone who already has the key.
		return mkundef(), rt.structuredCloneError("A WeakMap or WeakSet")
	}
	name := "Map"
	if c.isSet {
		name = "Set"
	}
	ctor, e := rt.getField(rt.global, name)
	if e != nil {
		return mkundef(), e
	}
	dst, e := rt.construct(ctor, nil)
	if e != nil {
		return mkundef(), e
	}
	memo[v] = dst
	add, e := rt.getField(dst, map[bool]string{true: "add", false: "set"}[c.isSet])
	if e != nil {
		return mkundef(), e
	}
	// Over a snapshot of the keys: cloning an entry runs no user code here, but
	// the collection's own slices are reallocated as the clone fills up.
	keys := append([]Value(nil), c.keys...)
	vals := append([]Value(nil), c.vals...)
	for i, k := range keys {
		ck, terr := rt.structuredClone(k, memo)
		if terr != nil {
			return mkundef(), terr
		}
		args := []Value{ck}
		if !c.isSet {
			cv, terr := rt.structuredClone(vals[i], memo)
			if terr != nil {
				return mkundef(), terr
			}
			args = append(args, cv)
		}
		if _, terr := rt.callValue(add, dst, args); terr != nil {
			return mkundef(), terr
		}
	}
	return dst, nil
}

// cloneError copies an Error through its own constructor so the copy keeps the
// subclass's prototype, then the four properties the algorithm carries.
func (rt *Runtime) cloneError(v Value, memo map[Value]Value) (Value, *ThrowError) {
	nameV, e := rt.getField(v, "name")
	if e != nil {
		return mkundef(), e
	}
	name := "Error"
	if nameV.IsString() {
		if ctor, ge := rt.getField(rt.global, rt.strGo(nameV)); ge == nil && rt.isConstructorValue(ctor) {
			name = rt.strGo(nameV)
		}
	}
	ctor, e := rt.getField(rt.global, name)
	if e != nil {
		return mkundef(), e
	}
	msg, e := rt.getField(v, "message")
	if e != nil {
		return mkundef(), e
	}
	c, e := rt.construct(ctor, []Value{msg})
	if e != nil {
		return mkundef(), e
	}
	memo[v] = c
	for _, k := range []string{"name", "stack"} {
		if pv, ge := rt.getField(v, k); ge == nil && !pv.IsUndefined() {
			_ = rt.setField(c, k, pv)
		}
	}
	if cause, ge := rt.getField(v, "cause"); ge == nil && !cause.IsUndefined() {
		cc, terr := rt.structuredClone(cause, memo)
		if terr != nil {
			return mkundef(), terr
		}
		_ = rt.setField(c, "cause", cc)
	}
	return c, nil
}

// cloneArray copies the elements and any own enumerable named properties an
// array has picked up.
func (rt *Runtime) cloneArray(v Value, memo map[Value]Value) (Value, *ThrowError) {
	n, e := rt.lengthOf(v)
	if e != nil {
		return mkundef(), e
	}
	dst := rt.newArray()
	memo[v] = dst
	for i := 0; i < n; i++ {
		idx := mknum(float64(i))
		has, terr := rt.hasPropE(v, strconv.Itoa(i))
		if terr != nil {
			return mkundef(), terr
		}
		if !has {
			continue // a hole stays a hole
		}
		el, terr := rt.getElement(v, idx)
		if terr != nil {
			return mkundef(), terr
		}
		cel, terr := rt.structuredClone(el, memo)
		if terr != nil {
			return mkundef(), terr
		}
		if terr := rt.setElement(dst, idx, cel); terr != nil {
			return mkundef(), terr
		}
	}
	return rt.cloneOwnKeys(v, dst, memo, true)
}

// clonePlainObject copies own enumerable string-keyed properties onto a fresh
// ordinary object. The prototype is deliberately NOT carried: a clone is data,
// and an object that kept its class's prototype would have methods whose closed-
// over state belongs to the original.
func (rt *Runtime) clonePlainObject(v Value, o *object, memo map[Value]Value) (Value, *ThrowError) {
	if b := o.boxed; b.Type() == TStr || b.Type() == TBool {
		return rt.cloneBoxed(v, b, memo)
	}
	if p := o.getSlot(slotPrimitive); p.Type() == TNum {
		return rt.cloneBoxed(v, p, memo)
	}
	dst := rt.newPlainObject()
	memo[v] = dst
	return rt.cloneOwnKeys(v, dst, memo, false)
}

// cloneBoxed rebuilds a String/Number/Boolean wrapper object.
func (rt *Runtime) cloneBoxed(v Value, prim Value, memo map[Value]Value) (Value, *ThrowError) {
	name := "Number"
	switch prim.Type() {
	case TStr:
		name = "String"
	case TBool:
		name = "Boolean"
	}
	ctor, e := rt.getField(rt.global, name)
	if e != nil {
		return mkundef(), e
	}
	c, e := rt.construct(ctor, []Value{prim})
	if e != nil {
		return mkundef(), e
	}
	memo[v] = c
	return c, nil
}

// cloneOwnKeys copies src's own enumerable string keys onto dst, skipping the
// index properties an array has already copied.
func (rt *Runtime) cloneOwnKeys(src, dst Value, memo map[Value]Value, skipIndices bool) (Value, *ThrowError) {
	keys, e := rt.enumerableOwnKeysE(src)
	if e != nil {
		return mkundef(), e
	}
	for _, k := range keys {
		if skipIndices {
			if _, err := strconv.ParseUint(k, 10, 32); err == nil {
				continue
			}
			if k == "length" {
				continue
			}
		}
		pv, terr := rt.getField(src, k)
		if terr != nil {
			return mkundef(), terr
		}
		cv, terr := rt.structuredClone(pv, memo)
		if terr != nil {
			return mkundef(), terr
		}
		if terr := rt.setField(dst, k, cv); terr != nil {
			return mkundef(), terr
		}
	}
	return dst, nil
}

// typedArrayName maps a view kind back to its global constructor's name.
func typedArrayName(k taKind) string {
	switch k {
	case taInt8:
		return "Int8Array"
	case taUint8:
		return "Uint8Array"
	case taUint8Clamped:
		return "Uint8ClampedArray"
	case taInt16:
		return "Int16Array"
	case taUint16:
		return "Uint16Array"
	case taInt32:
		return "Int32Array"
	case taUint32:
		return "Uint32Array"
	case taFloat32:
		return "Float32Array"
	case taFloat64:
		return "Float64Array"
	case taBigInt64:
		return "BigInt64Array"
	case taBigUint64:
		return "BigUint64Array"
	case taFloat16:
		return "Float16Array"
	}
	return "Uint8Array"
}
