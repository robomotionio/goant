package engine

import "strconv"

// Reflect (ant modules/reflect.c). A namespace object of the reflective object
// operations, reusing the same internal protocol that backs the operators and
// Object.* statics. Reflect methods return booleans for the [[Set]]-family ops
// (never throw on rejection) and forward faithfully otherwise.

// ownDescOf is [[GetOwnProperty]](P) for a non-proxy object, covering array/
// typed-array/string-wrapper elements and the array "length" that ownDescriptor
// (shape-only) misses.
func (rt *Runtime) ownDescOf(o *object, pk Value) ownDesc {
	if pk.IsSymbol() {
		return o.ownDescriptorSym(pk.handle())
	}
	name := string(rt.strBytes(pk))
	if idx, ok := canonicalIndex(name); ok {
		switch {
		case o.ta != nil:
			if v, live := rt.taGet(o, int(idx)); live {
				return ownDesc{exists: true, writable: true, enumerable: true, configable: true, value: v}
			}
			return ownDesc{}
		case o.typeTag == TArr:
			if idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
				return ownDesc{exists: true, writable: true, enumerable: true, configable: true, value: o.arr[idx]}
			}
		case o.boxed.Type() == TStr:
			b := rt.strBytes(o.boxed)
			if int(idx) < utf16Len(b) {
				return ownDesc{exists: true, writable: false, enumerable: true, configable: false, value: rt.charAt(b, int(idx))}
			}
		}
	}
	if name == "length" && o.typeTag == TArr {
		return ownDesc{exists: true, writable: !o.flags.arrLenNonWritable, value: mknum(float64(o.arrLen))}
	}
	return o.ownDescriptor(name)
}

// reflectSet implements OrdinarySet(target, key, val, receiver) returning whether
// the write took effect (Reflect.set's boolean), honoring the receiver.
func (rt *Runtime) reflectSet(target, key, val, receiver Value) (bool, *ThrowError) {
	pk, e := rt.toPropertyKey(key)
	if e != nil {
		return false, e
	}
	cur := target
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		if o.proxy != nil {
			return rt.proxySet(o.proxy, pk, val, receiver)
		}
		d := rt.ownDescOf(o, pk)
		if d.exists {
			if d.isAccessor {
				if d.setter.IsUndefined() {
					return false, nil
				}
				if _, e := rt.callValue(d.setter, receiver, []Value{val}); e != nil {
					return false, e
				}
				return true, nil
			}
			if !d.writable || !receiver.IsObjectType() {
				return false, nil
			}
			return rt.setDataOnReceiver(receiver, pk, val)
		}
		cur = o.proto
	}
	if !receiver.IsObjectType() {
		return false, nil
	}
	return rt.setDataOnReceiver(receiver, pk, val)
}

// setDataOnReceiver writes val to receiver[pk] as a data property: updates an
// existing writable data property (rejecting an accessor / non-writable one) or
// creates a fresh one (rejecting a non-extensible receiver). Returns false when
// rejected rather than throwing.
func (rt *Runtime) setDataOnReceiver(receiver, pk, val Value) (bool, *ThrowError) {
	ro := rt.objPtr(receiver)
	if ro == nil {
		return false, nil
	}
	if ro.proxy == nil {
		ex := rt.ownDescOf(ro, pk)
		if ex.exists {
			if ex.isAccessor || !ex.writable {
				return false, nil
			}
		} else if !ro.flags.extensible {
			return false, nil
		}
		desc := rt.newPlainObject()
		do := rt.objPtr(desc)
		do.defineOwn("value", val, attrDefault)
		if !ex.exists { // a new property is a full data descriptor
			do.defineOwn("writable", mktrue(), attrDefault)
			do.defineOwn("enumerable", mktrue(), attrDefault)
			do.defineOwn("configurable", mktrue(), attrDefault)
		}
		if e := rt.objectDefinePropertyKey(receiver, pk, desc); e != nil {
			if e.rejected {
				return false, nil
			}
			return false, e
		}
		return true, nil
	}
	// Proxy receiver: define a data property via its trap.
	desc := rt.newPlainObject()
	do := rt.objPtr(desc)
	do.defineOwn("value", val, attrDefault)
	do.defineOwn("writable", mktrue(), attrDefault)
	do.defineOwn("enumerable", mktrue(), attrDefault)
	do.defineOwn("configurable", mktrue(), attrDefault)
	e := rt.proxyDefineProperty(ro.proxy, pk, desc)
	return e == nil, e
}

func (rt *Runtime) initReflectBuiltin() {
	reflect := rt.newObject(rt.objectProto)
	ro := rt.objPtr(reflect)

	needObj := func(v Value, m string) *ThrowError {
		if !v.IsObjectLike() {
			return rt.typeError("Reflect." + m + " called on non-object")
		}
		return nil
	}

	rt.defMethod(ro, "get", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := needObj(arg(args, 0), "get"); e != nil {
			return mkundef(), e
		}
		target := arg(args, 0)
		// Reflect.get(target, key[, receiver]) performs target.[[Get]](key,
		// receiver); receiver defaults to target. When a distinct receiver is
		// supplied, any accessor found along target's chain must run with that
		// receiver as its `this` (getSuperProp walks target's own chain first).
		if len(args) > 2 && args[2] != target {
			return rt.getSuperProp(target, arg(args, 1), args[2])
		}
		return rt.getElement(target, arg(args, 1))
	})
	rt.defMethod(ro, "set", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := needObj(arg(args, 0), "set"); e != nil {
			return mkundef(), e
		}
		receiver := arg(args, 0)
		if len(args) > 3 {
			receiver = args[3]
		}
		ok, e := rt.reflectSet(arg(args, 0), arg(args, 1), arg(args, 2), receiver)
		if e != nil {
			return mkundef(), e
		}
		return mkbool(ok), nil
	})
	rt.defMethod(ro, "has", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := needObj(arg(args, 0), "has"); e != nil {
			return mkundef(), e
		}
		pk, e := rt.toPropertyKey(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		if o := rt.objPtr(arg(args, 0)); o != nil && o.proxy != nil {
			has, e := rt.proxyHas(o.proxy, pk)
			return mkbool(has), e
		}
		if pk.IsSymbol() {
			return mkbool(rt.hasFieldSymbol(arg(args, 0), pk.handle())), nil
		}
		return mkbool(rt.hasProp(arg(args, 0), string(rt.strBytes(pk)))), nil
	})
	rt.defMethod(ro, "deleteProperty", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target := arg(args, 0)
		if !target.IsObjectLike() {
			return mkundef(), rt.typeError("Reflect.deleteProperty called on non-object")
		}
		// Route through [[Delete]] so integer-indexed exotic objects (typed
		// arrays) and proxies observe their own delete semantics.
		ok, e := rt.deleteElement(target, arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return mkbool(ok), nil
	})
	rt.defMethod(ro, "getPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.getPrototypeOf called on non-object")
		}
		if o.proxy != nil {
			return rt.proxyGetPrototypeOf(o.proxy)
		}
		if o.proto.IsNull() || o.proto == 0 {
			return mknull(), nil
		}
		return o.proto, nil
	})
	rt.defMethod(ro, "setPrototypeOf", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.setPrototypeOf called on non-object")
		}
		p := arg(args, 1)
		if !p.IsObjectType() && !p.IsNull() {
			return mkundef(), rt.typeError("Reflect.setPrototypeOf proto must be an object or null")
		}
		if o.proxy != nil {
			ok, e := rt.proxySetPrototypeOf(o.proxy, p)
			if e != nil {
				return mkundef(), e
			}
			return mkbool(ok), nil
		}
		// Returns whether [[SetPrototypeOf]] succeeded (false on cycle / non-
		// extensible / immutable prototype), rather than throwing.
		return mkbool(rt.ordinarySetProto(o, p)), nil
	})
	rt.defMethod(ro, "defineProperty", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := needObj(arg(args, 0), "defineProperty"); e != nil {
			return mkundef(), e
		}
		e := rt.objectDefinePropertyKey(arg(args, 0), arg(args, 1), arg(args, 2))
		if e == nil {
			return mktrue(), nil
		}
		if e.rejected { // a rejected define is a boolean false, not a thrown error
			return mkfalse(), nil
		}
		return mkundef(), e
	})
	rt.defMethod(ro, "getOwnPropertyDescriptor", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.getOwnPropertyDescriptor called on non-object")
		}
		pk, e := rt.toPropertyKey(arg(args, 1)) // symbol keys allowed
		if e != nil {
			return mkundef(), e
		}
		if o.proxy != nil {
			return rt.proxyGetOwnPropertyDescriptor(o.proxy, pk)
		}
		d := rt.ownDescOf(o, pk) // covers array/typed/string-wrapper elements + length
		if !d.exists {
			return mkundef(), nil
		}
		return rt.descriptorToObject(d), nil
	})
	rt.defMethod(ro, "isExtensible", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.isExtensible called on non-object")
		}
		if o.proxy != nil {
			ext, e := rt.proxyIsExtensible(o.proxy)
			return mkbool(ext), e
		}
		return mkbool(o.flags.extensible), nil
	})
	rt.defMethod(ro, "preventExtensions", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.preventExtensions called on non-object")
		}
		if o.proxy != nil {
			ok, e := rt.proxyPreventExtensions(o.proxy)
			if e != nil {
				return mkundef(), e
			}
			return mkbool(ok), nil // Reflect returns the boolean status
		}
		o.flags.extensible = false
		return mktrue(), nil
	})
	rt.defMethod(ro, "ownKeys", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.ownKeys called on non-object")
		}
		if o.proxy != nil {
			keys, e := rt.proxyOwnKeys(o.proxy)
			if e != nil {
				return mkundef(), e
			}
			res := rt.newArray()
			ra := rt.objPtr(res)
			for _, k := range keys {
				rt.arraySet(ra, ra.arrLen, k)
			}
			return res, nil
		}
		res := rt.newArray()
		ra := rt.objPtr(res)
		if arg(args, 0).Type() == TArr {
			for i := uint32(0); i < o.arrLen; i++ {
				if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
					rt.arraySet(ra, ra.arrLen, rt.newString(strconv.Itoa(int(i))))
				}
			}
			rt.arraySet(ra, ra.arrLen, rt.newString("length"))
		}
		if arg(args, 0).Type() == TTypedArray {
			for i, l := 0, rt.taLength(o); i < l; i++ {
				rt.arraySet(ra, ra.arrLen, rt.newString(strconv.Itoa(i)))
			}
		}
		for _, k := range o.ownKeys() {
			rt.arraySet(ra, ra.arrLen, rt.newString(k))
		}
		for _, off := range o.ownSymbolKeys() {
			rt.arraySet(ra, ra.arrLen, mkval(TSymbol, uint64(off)))
		}
		return res, nil
	})
	rt.defMethod(ro, "apply", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		if !rt.isCallable(fn) {
			return mkundef(), rt.typeError("Reflect.apply target is not a function")
		}
		// Reflect.apply uses CreateListFromArrayLike (length + indexed Gets), which
		// throws on a non-object argumentsList — not the iterator protocol.
		callArgs, e := rt.createListFromArrayLike(arg(args, 2))
		if e != nil {
			return mkundef(), e
		}
		return rt.callValue(fn, arg(args, 1), callArgs)
	})
	rt.defMethod(ro, "construct", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		if !rt.isCallable(fn) {
			return mkundef(), rt.typeError("Reflect.construct target is not a constructor")
		}
		// Reflect.construct uses CreateListFromArrayLike, not the iterator protocol.
		callArgs, e := rt.createListFromArrayLike(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		newTarget := fn
		if nt := arg(args, 2); !nt.IsUndefined() {
			newTarget = nt
		}
		return rt.constructWithTarget(fn, callArgs, newTarget)
	})

	rt.defGlobal("Reflect", reflect)
}
