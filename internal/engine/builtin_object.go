package engine

import "strconv"

// Object constructor + Object.prototype (ant ant.c object sections /
// builtin_object). The prototype root's methods (toString/valueOf/
// hasOwnProperty/isPrototypeOf/propertyIsEnumerable) plus core Object statics.

func (rt *Runtime) initObjectBuiltin() {
	proto := rt.objPtr(rt.objectProto)

	rt.defMethod(proto, "hasOwnProperty", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// ToPropertyKey(V) is performed BEFORE ToObject(this) — and it may yield a
		// Symbol (via the key's Symbol.toPrimitive), which must not be ToString'd.
		pk, e := rt.toPropertyKey(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(obj)
		if o == nil {
			return mkfalse(), nil
		}
		if !pk.IsSymbol() {
			if e := rt.namespaceTDZ(obj, string(rt.strBytes(pk))); e != nil {
				return mkundef(), e
			}
		}
		// HasOwnProperty -> [[GetOwnProperty]] routes through the proxy trap.
		if o.proxy != nil {
			d, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, rt.toPropertyKeyValue(pk))
			if e != nil {
				return mkundef(), e
			}
			return mkbool(!d.IsUndefined()), nil
		}
		if pk.IsSymbol() {
			return mkbool(o.shape.lookupSymbol(pk.handle()) >= 0), nil
		}
		name, e := rt.propKeyString(pk)
		if e != nil {
			return mkundef(), e
		}
		// An index in element backing store (array/typed-array/string-wrapper) is an
		// own property; canonicalIndex parses the key STRING (arrayIndex only takes a
		// numeric Value, so a string index like "0" was wrongly missed). An index not
		// in fast storage falls through to hasOwn (an attribute-defined named index).
		if idx, ok := canonicalIndex(name); ok && rt.hasOwnIndex(obj, o, idx) {
			return mktrue(), nil
		}
		if obj.Type() == TArr && name == "length" {
			return mktrue(), nil
		}
		return mkbool(o.hasOwn(name)), nil
	})

	rt.defMethod(proto, "isPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target := arg(args, 0)
		if !target.IsObjectType() {
			return mkfalse(), nil
		}
		// ToObject(this) happens after the V-is-object check, so a null/undefined
		// receiver throws only when V is an object (matching 20.1.3.4).
		o, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		// Walk V's prototype chain via [[GetPrototypeOf]] (a proxy in the chain
		// routes through its trap, which can throw) looking for O.
		cur := target
		for depth := 0; depth < maxProtoChainDepth; depth++ {
			p, e := rt.getPrototypeOfValue(cur)
			if e != nil {
				return mkundef(), e
			}
			if !p.IsObjectType() {
				return mkfalse(), nil
			}
			if p == o {
				return mktrue(), nil
			}
			cur = p
		}
		return mkfalse(), nil
	})

	rt.defMethod(proto, "propertyIsEnumerable", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// ToPropertyKey(V) (before ToObject(this)) may yield a Symbol.
		pk, e := rt.toPropertyKey(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(obj)
		if o == nil {
			return mkfalse(), nil
		}
		if !pk.IsSymbol() {
			if e := rt.namespaceTDZ(obj, string(rt.strBytes(pk))); e != nil {
				return mkundef(), e
			}
		}
		// A Proxy routes [[GetOwnProperty]] through its trap (or its target); read
		// the resulting descriptor's enumerable flag.
		if o.proxy != nil {
			descV, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, pk)
			if e != nil {
				return mkundef(), e
			}
			if descV.IsUndefined() {
				return mkfalse(), nil
			}
			en, e := rt.getField(descV, "enumerable")
			if e != nil {
				return mkundef(), e
			}
			return mkbool(rt.toBoolean(en)), nil
		}
		if pk.IsSymbol() {
			d := o.ownDescriptorSym(pk.handle())
			return mkbool(d.exists && d.enumerable), nil
		}
		name, e := rt.propKeyString(pk)
		if e != nil {
			return mkundef(), e
		}
		if d := o.ownDescriptor(name); d.exists {
			return mkbool(d.enumerable), nil
		}
		// An indexed element in element backing store (array/typed-array/string
		// wrapper) is an own enumerable data property not tracked in the shape.
		if idx, ok := canonicalIndex(name); ok && rt.hasOwnIndex(obj, o, idx) {
			return mktrue(), nil
		}
		return mkfalse(), nil
	})

	rt.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		tag, e := rt.objectToStringTag(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.internString(tag), nil
	})
	rt.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// 20.1.3.5: return ? Invoke(O, "toString") — a nullish receiver throws in
		// GetV, and the actual (possibly overridden) toString is dispatched.
		m, e := rt.getField(this, "toString")
		if e != nil {
			return mkundef(), e
		}
		return rt.callValue(m, this, nil)
	})
	rt.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.toObjectValue(this) // ToObject(this): a primitive returns its wrapper
	})

	// Object constructor.
	var ctor Value
	ctor = rt.newNativeFunc("Object", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// 20.1.1.1 step 1: when NewTarget is a subclass (neither undefined nor the
		// Object constructor itself), ignore the argument and create an ordinary
		// object from NewTarget.prototype.
		if rt.constructing() && rt.pendingNewTarget != ctor {
			proto, e := rt.newTargetProtoE(rt.objectProto)
			if e != nil {
				return mkundef(), e
			}
			return rt.newObject(proto), nil
		}
		v := arg(args, 0)
		if v.IsNullish() {
			return rt.newPlainObject(), nil
		}
		if v.IsObjectType() || v.Type() == TTypedArray {
			return v, nil
		}
		// Box the primitive into its wrapper (String/Number/Boolean/Symbol).
		return rt.toObjectValue(v)
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.objectProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)

	rt.defMethod(cobj, "keys", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.enumerableOwnProps(arg(args, 0), 0)
	})
	rt.defMethod(cobj, "getPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(obj)
		if o == nil {
			return mknull(), nil
		}
		if o.proxy != nil {
			return rt.proxyGetPrototypeOf(o.proxy)
		}
		if o.proto.IsObjectType() {
			return o.proto, nil
		}
		return mknull(), nil
	})
	rt.defMethod(cobj, "create", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		p := arg(args, 0)
		if !p.IsObjectLike() && !p.IsNull() { // IsObjectLike: a TypedArray is a valid prototype
			return mkundef(), rt.typeError("Object prototype may only be an Object or null")
		}
		obj := rt.newObject(p)
		// A present Properties argument runs ObjectDefineProperties, which begins
		// with ToObject(Properties) — so null throws and a string boxes.
		if props := arg(args, 1); !props.IsUndefined() {
			if e := rt.objectDefineProperties(obj, props); e != nil {
				return mkundef(), e
			}
		}
		return obj, nil
	})
	rt.defMethod(cobj, "defineProperty", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj := arg(args, 0)
		if !obj.IsObjectLike() {
			return mkundef(), rt.typeError("Object.defineProperty called on non-object")
		}
		if e := rt.objectDefinePropertyKey(obj, arg(args, 1), arg(args, 2)); e != nil {
			return mkundef(), e
		}
		return obj, nil
	})
	rt.defMethod(cobj, "defineProperties", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj := arg(args, 0)
		if !obj.IsObjectLike() {
			return mkundef(), rt.typeError("Object.defineProperties called on non-object")
		}
		if e := rt.objectDefineProperties(obj, arg(args, 1)); e != nil {
			return mkundef(), e
		}
		return obj, nil
	})
	rt.defMethod(cobj, "getOwnPropertyDescriptor", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// [[GetOwnProperty]] begins with ToObject: null/undefined throw a
		// TypeError, and a primitive (e.g. a string) is wrapped so its exotic
		// own properties (indices, "length") are observable.
		obj, e := rt.toObjectValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(obj)
		if o == nil {
			return mkundef(), nil
		}
		if o.proxy != nil {
			return rt.proxyGetOwnPropertyDescriptor(o.proxy, arg(args, 1))
		}
		if key := arg(args, 1); key.IsSymbol() {
			d := o.ownDescriptorSym(key.handle())
			if !d.exists {
				return mkundef(), nil
			}
			return rt.descriptorToObject(d), nil
		}
		name, e := rt.propKeyString(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		if nd, ok, ne := rt.namespaceDescriptor(obj, name); ok {
			return nd, ne
		}
		d := o.ownDescriptor(name)
		if !d.exists {
			if arg(args, 0).Type() == TTypedArray {
				// Integer-indexed exotic [[GetOwnProperty]]: a live element is a
				// writable/enumerable/configurable data property; a canonical index
				// that is out of range (or detached) has no descriptor.
				if idx, ok := canonicalIndex(name); ok {
					if v, live := rt.taGet(o, int(idx)); live {
						return rt.makeDataDescriptor(v, true, true, true), nil
					}
					return mkundef(), nil
				}
			}
			if arg(args, 0).Type() == TArr {
				// The array "length" data property (value = length, writable,
				// non-enumerable, non-configurable) is virtual — synthesize it.
				if name == "length" {
					do := rt.newPlainObject()
					dp := rt.objPtr(do)
					dp.defineOwn("value", mknum(float64(o.arrLen)), attrDefault)
					dp.defineOwn("writable", mkbool(!o.flags.arrLenNonWritable), attrDefault)
					dp.defineOwn("enumerable", mkfalse(), attrDefault)
					dp.defineOwn("configurable", mkfalse(), attrDefault)
					return do, nil
				}
				// canonicalIndex parses the key STRING; arrayIndex only accepts a
				// numeric Value, so a string index key ("0") wrongly returned no
				// descriptor. hasOwnIndex confirms the element is present (not a hole).
				if idx, ok := canonicalIndex(name); ok && rt.hasOwnIndex(arg(args, 0), o, idx) {
					v, _ := rt.getElement(arg(args, 0), mknum(float64(idx)))
					// A frozen array's elements are non-writable; a sealed (or
					// frozen) array's are non-configurable.
					return rt.makeDataDescriptor(v, !o.flags.frozen, true, !o.flags.frozen && !o.flags.sealed), nil
				}
			}
			if o.boxed.Type() == TStr {
				// String exotic [[GetOwnProperty]]: an in-range index is a
				// non-writable, enumerable, non-configurable data property.
				if idx, ok := canonicalIndex(name); ok {
					b := rt.strBytes(o.boxed)
					if int(idx) < utf16Len(b) {
						return rt.makeDataDescriptor(rt.charAt(b, int(idx)), false, true, false), nil
					}
				}
			}
			return mkundef(), nil
		}
		return rt.descriptorToObject(d), nil
	})
	rt.defMethod(cobj, "getOwnPropertyNames", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(arg(args, 0)) // ToObject (null/undefined -> TypeError)
		if e != nil {
			return mkundef(), e
		}
		return rt.ownPropertyNames(obj, false)
	})
	rt.defMethod(cobj, "hasOwn", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(obj)
		if o == nil {
			return mkfalse(), nil
		}
		pk, e := rt.toPropertyKey(arg(args, 1)) // may yield a Symbol
		if e != nil {
			return mkundef(), e
		}
		if pk.IsSymbol() {
			return mkbool(o.shape.lookupSymbol(pk.handle()) >= 0), nil
		}
		name, e := rt.propKeyString(pk)
		if e != nil {
			return mkundef(), e
		}
		if obj.Type() == TArr {
			if idx, ok := canonicalIndex(name); ok {
				return mkbool(idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty()), nil
			}
			if name == "length" {
				return mktrue(), nil
			}
		}
		return mkbool(o.ownDescriptor(name).exists), nil
	})
	rt.defMethod(cobj, "groupBy", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// RequireObjectCoercible(items) then IsCallable(callback) precede the
		// GetIterator step, so an invalid callback throws even for empty input.
		if arg(args, 0).IsNullish() {
			return mkundef(), rt.typeError("Object.groupBy called on null or undefined")
		}
		cb := arg(args, 1)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Object.groupBy: callback is not a function")
		}
		items, e := rt.iterableValues(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		res := rt.newObject(mknull())
		ro := rt.objPtr(res)
		for i, it := range items {
			kv, e := rt.callValue(cb, mkundef(), []Value{it, mknum(float64(i))})
			if e != nil {
				return mkundef(), e
			}
			key, e := rt.propKeyString(kv)
			if e != nil {
				return mkundef(), e
			}
			grp, ok := ro.getOwn(key)
			if !ok {
				grp = rt.newArray()
				ro.defineOwn(key, grp, attrDefault)
			}
			go2 := rt.objPtr(grp)
			rt.arraySet(go2, go2.arrLen, it)
		}
		return res, nil
	})
	rt.defMethod(cobj, "getOwnPropertySymbols", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(arg(args, 0)) // ToObject: null/undefined throws; a primitive is boxed
		if e != nil {
			return mkundef(), e
		}
		res := rt.newArray()
		ra := rt.objPtr(res)
		if o := rt.objPtr(obj); o != nil {
			if o.proxy != nil {
				// Route through the [[OwnPropertyKeys]] trap so its invariants run
				// (an abrupt completion propagates); keep only the Symbol keys.
				keys, e := rt.proxyOwnKeys(o.proxy)
				if e != nil {
					return mkundef(), e
				}
				for _, kv := range keys {
					if kv.IsSymbol() {
						rt.arraySet(ra, ra.arrLen, kv)
					}
				}
				return res, nil
			}
			for _, off := range o.ownSymbolKeys() {
				rt.arraySet(ra, ra.arrLen, mkval(TSymbol, uint64(off)))
			}
		}
		return res, nil
	})
	rt.defMethod(cobj, "getOwnPropertyDescriptors", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(arg(args, 0)) // ToObject: a primitive is wrapped
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(obj)
		res := rt.newPlainObject()
		reso := rt.objPtr(res)
		if o != nil && o.proxy != nil {
			// Proxy: [[OwnPropertyKeys]] once, then [[GetOwnProperty]] per key,
			// storing the FromPropertyDescriptor result (skip an undefined desc).
			keys, e := rt.proxyOwnKeys(o.proxy)
			if e != nil {
				return mkundef(), e
			}
			for _, k := range keys {
				d, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, k)
				if e != nil {
					return mkundef(), e
				}
				if d.IsUndefined() {
					continue
				}
				if k.IsSymbol() {
					reso.defineOwnSymbol(k.handle(), d, attrDefault)
				} else {
					name, _ := rt.propKeyString(k)
					reso.defineOwn(name, d, attrDefault)
				}
			}
			return res, nil
		}
		switch obj.Type() {
		case TArr:
			for i := uint32(0); i < o.arrLen; i++ {
				if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
					reso.defineOwn(strconv.Itoa(int(i)), rt.makeDataDescriptor(o.arr[i], !o.flags.frozen, true, !o.flags.frozen && !o.flags.sealed), attrDefault)
				}
			}
		case TTypedArray:
			for i, l := 0, rt.taLength(o); i < l; i++ {
				if val, ok := rt.taGet(o, i); ok {
					reso.defineOwn(strconv.Itoa(i), rt.makeDataDescriptor(val, true, true, true), attrDefault)
				}
			}
		}
		if o.boxed.Type() == TStr {
			b := rt.strBytes(o.boxed)
			for i, l := 0, utf16Len(b); i < l; i++ {
				reso.defineOwn(strconv.Itoa(i), rt.makeDataDescriptor(rt.charAt(b, i), false, true, false), attrDefault)
			}
		}
		for _, k := range o.ownKeys() {
			if nd, ok, ne := rt.namespaceDescriptor(obj, k); ok {
				if ne != nil {
					return mkundef(), ne
				}
				reso.defineOwn(k, nd, attrDefault)
				continue
			}
			d := o.ownDescriptor(k)
			if d.exists {
				reso.defineOwn(k, rt.descriptorToObject(d), attrDefault)
			}
		}
		for _, off := range o.ownSymbolKeys() {
			d := o.ownDescriptorSym(off)
			if d.exists {
				reso.defineOwnSymbol(off, rt.descriptorToObject(d), attrDefault)
			}
		}
		return res, nil
	})
	rt.defMethod(cobj, "preventExtensions", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if o := rt.objPtr(arg(args, 0)); o != nil {
			if o.proxy != nil {
				ok, e := rt.proxyPreventExtensions(o.proxy)
				if e != nil {
					return mkundef(), e
				}
				if !ok { // O.[[PreventExtensions]]() returned false
					return mkundef(), rt.typeError("Object.preventExtensions: proxy preventExtensions trap returned false")
				}
			} else {
				o.flags.extensible = false
			}
		}
		return arg(args, 0), nil
	})
	rt.defMethod(cobj, "isExtensible", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o != nil && o.proxy != nil {
			ext, e := rt.proxyIsExtensible(o.proxy)
			return mkbool(ext), e
		}
		return mkbool(o != nil && o.flags.extensible), nil
	})
	rt.defMethod(cobj, "seal", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := rt.sealObject(arg(args, 0), false); e != nil {
			return mkundef(), e
		}
		return arg(args, 0), nil
	})
	rt.defMethod(cobj, "freeze", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := rt.sealObject(arg(args, 0), true); e != nil {
			return mkundef(), e
		}
		return arg(args, 0), nil
	})
	rt.defMethod(cobj, "isSealed", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.isSealedOrFrozenE(arg(args, 0), false)
		return mkbool(s), e
	})
	rt.defMethod(cobj, "isFrozen", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.isSealedOrFrozenE(arg(args, 0), true)
		return mkbool(s), e
	})

	rt.defMethod(cobj, "assign", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		to, e := rt.toObjectValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		for _, src := range args[1:] {
			if src.IsNullish() {
				continue
			}
			from, e := rt.toObjectValue(src)
			if e != nil {
				return mkundef(), e
			}
			// CopyDataProperties: enumerable own string+symbol keys, in
			// [[OwnPropertyKeys]] order, each Get then Set(to, key, val, true).
			keys, e := rt.ownKeyValues(from)
			if e != nil {
				return mkundef(), e
			}
			for _, key := range keys {
				enum, exists, e := rt.ownKeyEnumerable(from, key)
				if e != nil {
					return mkundef(), e
				}
				if !exists || !enum {
					continue
				}
				val, e := rt.getElement(from, key)
				if e != nil {
					return mkundef(), e
				}
				if e := rt.setThrow(to, key, val); e != nil {
					return mkundef(), e
				}
			}
		}
		return to, nil
	})
	rt.defMethod(cobj, "is", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkbool(rt.sameValue(arg(args, 0), arg(args, 1))), nil
	})
	rt.defMethod(cobj, "setPrototypeOf", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj := arg(args, 0)
		if o := rt.objPtr(obj); o != nil {
			p := arg(args, 1)
			if o.proxy != nil {
				ok, e := rt.proxySetPrototypeOf(o.proxy, p)
				if e != nil {
					return mkundef(), e
				}
				if !ok {
					return mkundef(), rt.typeError("Object.setPrototypeOf: proxy [[SetPrototypeOf]] returned false")
				}
			} else if p.IsObjectLike() || p.IsNull() { // a TypedArray is a valid prototype
				if !rt.ordinarySetProto(o, p) {
					return mkundef(), rt.typeError("Object.setPrototypeOf: cannot set prototype (non-extensible, immutable, or cyclic)")
				}
			} else {
				return mkundef(), rt.typeError("Object.setPrototypeOf: prototype must be an object or null")
			}
		} else if obj.IsNullish() {
			return mkundef(), rt.typeError("Object.setPrototypeOf called on null or undefined")
		}
		return obj, nil
	})
	rt.defMethod(cobj, "values", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.enumerableOwnProps(arg(args, 0), 1)
	})
	rt.defMethod(cobj, "entries", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.enumerableOwnProps(arg(args, 0), 2)
	})
	rt.defMethod(cobj, "fromEntries", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iterable := arg(args, 0)
		if iterable.IsNullish() { // RequireObjectCoercible
			return mkundef(), rt.typeError("Object.fromEntries called on null or undefined")
		}
		res := rt.newPlainObject()
		o := rt.objPtr(res)
		// AddEntriesFromIterable: step one entry at a time, closing the iterator on
		// any abrupt completion (a non-object entry, a throwing element get, or a
		// throwing key coercion) rather than draining it eagerly.
		iter, e := rt.getSyncIterator(iterable)
		if e != nil {
			return mkundef(), e
		}
		nextMethod, e := rt.getField(iter, "next")
		if e != nil {
			return mkundef(), e
		}
		// closeAbrupt closes the iterator (best effort) and returns the pending error.
		closeAbrupt := func(e *ThrowError) (Value, *ThrowError) {
			rt.iteratorClose(iter)
			return mkundef(), e
		}
		for {
			entry, done, e := rt.iterStepValue(iter, nextMethod)
			if e != nil {
				return mkundef(), e // IteratorStep abrupt: the record is already closed
			}
			if done {
				return res, nil
			}
			if !entry.IsObjectType() {
				return closeAbrupt(rt.typeError("iterator value is not an entry object"))
			}
			k, e := rt.getElement(entry, mknum(0))
			if e != nil {
				return closeAbrupt(e)
			}
			v, e := rt.getElement(entry, mknum(1))
			if e != nil {
				return closeAbrupt(e)
			}
			pk, e := rt.toPropertyKey(k) // CreateDataPropertyOnObject: ToPropertyKey(k)
			if e != nil {
				return closeAbrupt(e)
			}
			if pk.IsSymbol() {
				o.defineOwnSymbol(pk.handle(), v, attrDefault)
			} else {
				name, e := rt.propKeyString(pk)
				if e != nil {
					return closeAbrupt(e)
				}
				o.defineOwn(name, v, attrDefault)
			}
		}
	})

	rt.defGlobal("Object", ctor)
}

// enumerableOwnKeys returns own enumerable string keys (array indices first),
// used by Object.assign/values/entries and JSON.
func (rt *Runtime) enumerableOwnKeys(v Value) []string {
	keys, _ := rt.enumerableOwnKeysE(v)
	return keys
}

// enumerableOwnKeysE is enumerableOwnKeys with error propagation: a proxy's
// ownKeys / getOwnPropertyDescriptor traps can throw (e.g. the duplicate-key
// invariant), and callers that return a completion must surface it.
func (rt *Runtime) enumerableOwnKeysE(v Value) ([]string, *ThrowError) {
	o := rt.objPtr(v)
	if o == nil {
		return nil, nil
	}
	// A namespace reports enumerability through [[GetOwnProperty]], which reads
	// each binding: one still in its temporal dead zone makes the whole call throw.
	if e := rt.namespaceTDZAll(v); e != nil {
		return nil, e
	}
	if o.proxy != nil {
		// EnumerableOwnPropertyNames: [[OwnPropertyKeys]] then filter the string
		// keys by the enumerability reported by [[GetOwnProperty]] (both traps).
		ks, e := rt.proxyOwnKeys(o.proxy)
		if e != nil {
			return nil, e
		}
		var out []string
		for _, kv := range ks {
			if !kv.IsString() {
				continue
			}
			desc, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, kv)
			if e != nil {
				return nil, e
			}
			if desc.IsUndefined() {
				continue
			}
			if en, _ := rt.getField(desc, "enumerable"); rt.toBoolean(en) {
				out = append(out, string(rt.strBytes(kv)))
			}
		}
		return out, nil
	}
	var keys []string
	if v.Type() == TArr {
		for i := uint32(0); i < o.arrLen; i++ {
			if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
				keys = append(keys, numberToString(float64(i)))
			}
		}
	}
	if v.Type() == TTypedArray {
		for i, l := 0, rt.taLength(o); i < l; i++ {
			keys = append(keys, strconv.Itoa(i))
		}
	}
	if o.boxed.Type() == TStr {
		// A String wrapper's characters are enumerable own index properties.
		for i, l := 0, utf16Len(rt.strBytes(o.boxed)); i < l; i++ {
			keys = append(keys, strconv.Itoa(i))
		}
	}
	keys = append(keys, o.ownKeysEnumerable()...)
	return keys, nil
}

// ownKeyValues returns an object's own property keys (string then symbol) as
// property-key Values in [[OwnPropertyKeys]] order, routing a Proxy through its
// ownKeys trap. Used by Object.assign (CopyDataProperties).
func (rt *Runtime) ownKeyValues(v Value) ([]Value, *ThrowError) {
	o := rt.objPtr(v)
	if o == nil {
		return nil, nil
	}
	if o.proxy != nil {
		return rt.proxyOwnKeys(o.proxy)
	}
	var keys []Value
	switch v.Type() {
	case TArr:
		for i := uint32(0); i < o.arrLen; i++ {
			if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
				keys = append(keys, rt.newString(numberToString(float64(i))))
			}
		}
	case TTypedArray:
		for i, l := 0, rt.taLength(o); i < l; i++ {
			keys = append(keys, rt.newString(strconv.Itoa(i)))
		}
	}
	if o.boxed.Type() == TStr {
		for i, l := 0, utf16Len(rt.strBytes(o.boxed)); i < l; i++ {
			keys = append(keys, rt.newString(strconv.Itoa(i)))
		}
	}
	for _, k := range o.ownKeys() {
		keys = append(keys, rt.newString(k))
	}
	for _, off := range o.ownSymbolKeys() {
		keys = append(keys, mkval(TSymbol, uint64(off)))
	}
	return keys, nil
}

// enumerableOwnProps implements EnumerableOwnProperties for Object.keys/values/
// entries (kind 0/1/2): ToObject, then for each own string key (snapshot) that
// [[GetOwnProperty]] reports enumerable, collect the key / Get value / [key,val]
// pair — so a getter that toggles a later key's enumerability is observed.
func (rt *Runtime) enumerableOwnProps(v Value, kind int) (Value, *ThrowError) {
	obj, e := rt.toObjectValue(v)
	if e != nil {
		return mkundef(), e
	}
	keys, e := rt.ownKeyValues(obj)
	if e != nil {
		return mkundef(), e
	}
	res := rt.newArray()
	ro := rt.objPtr(res)
	for _, key := range keys {
		if key.IsSymbol() {
			continue // string keys only
		}
		// EnumerableOwnProperties asks [[GetOwnProperty]] for each key, which on a
		// module namespace reads the binding — one in its temporal dead zone throws.
		if e := rt.namespaceTDZ(obj, string(rt.strBytes(key))); e != nil {
			return mkundef(), e
		}
		enum, exists, e := rt.ownKeyEnumerable(obj, key)
		if e != nil {
			return mkundef(), e
		}
		if !exists || !enum {
			continue
		}
		if kind == 0 { // keys
			rt.arraySet(ro, ro.arrLen, key)
			continue
		}
		val, e := rt.getElement(obj, key)
		if e != nil {
			return mkundef(), e
		}
		if kind == 1 { // values
			rt.arraySet(ro, ro.arrLen, val)
			continue
		}
		pair := rt.newArray() // entries
		po := rt.objPtr(pair)
		rt.arraySet(po, 0, key)
		rt.arraySet(po, 1, val)
		rt.arraySet(ro, ro.arrLen, pair)
	}
	return res, nil
}

// ownKeyEnumerable reports whether key is an own enumerable property of v (and
// whether it exists at all), via [[GetOwnProperty]] (a Proxy's trap).
func (rt *Runtime) ownKeyEnumerable(v, key Value) (bool, bool, *ThrowError) {
	o := rt.objPtr(v)
	if o == nil {
		return false, false, nil
	}
	if o.proxy != nil {
		desc, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, key)
		if e != nil {
			return false, false, e
		}
		if desc.IsUndefined() {
			return false, false, nil
		}
		en, _ := rt.getField(desc, "enumerable")
		return rt.toBoolean(en), true, nil
	}
	if key.IsSymbol() {
		d := o.ownDescriptorSym(key.handle())
		return d.enumerable, d.exists, nil
	}
	name := string(rt.strBytes(key))
	if idx, ok := canonicalIndex(name); ok && rt.hasOwnIndex(v, o, idx) {
		return true, true, nil // array/typed-array/string element: enumerable data
	}
	d := o.ownDescriptor(name)
	return d.enumerable, d.exists, nil
}

// setThrow performs Set(O, key, val, true): a TypeError when the write is
// rejected (non-writable / non-extensible / a setter-less accessor).
func (rt *Runtime) setThrow(to, key, val Value) *ThrowError {
	if key.IsSymbol() {
		o := rt.objPtr(to)
		if o == nil {
			return nil
		}
		if o.proxy != nil {
			_, e := rt.proxySet(o.proxy, key, val, to)
			return e
		}
		sym := key.handle()
		if d := o.ownDescriptorSym(sym); d.exists {
			if d.isAccessor {
				if d.setter.IsUndefined() {
					return rt.typeError("Cannot set property (setter-less accessor)")
				}
				_, e := rt.callValue(d.setter, to, []Value{val})
				return e
			}
			if !d.writable {
				return rt.typeError("Cannot assign to read only property (symbol)")
			}
			attrs := uint8(0)
			if d.writable {
				attrs |= attrWritable
			}
			if d.enumerable {
				attrs |= attrEnumerable
			}
			if d.configable {
				attrs |= attrConfigurable
			}
			o.defineOwnSymbol(sym, val, attrs)
			return nil
		}
		if !o.flags.extensible {
			return rt.typeError("Cannot add property, object is not extensible")
		}
		o.defineOwnSymbol(sym, val, attrDefault)
		return nil
	}
	name := string(rt.strBytes(key))
	if idx, ok := canonicalIndex(name); ok && (to.Type() == TArr || to.Type() == TTypedArray) {
		return rt.setElement(to, mknum(float64(idx)), val)
	}
	ok, e := rt.setFieldR(to, name, val)
	if e != nil {
		return e
	}
	if !ok {
		return rt.typeError("Cannot assign to read only property '" + name + "'")
	}
	return nil
}

// objectDefineProperty applies an ES5 property descriptor to obj[name].
func (rt *Runtime) objectDefineProperty(obj Value, name string, descVal Value) *ThrowError {
	return rt.objectDefinePropertyKey(obj, rt.internString(name), descVal)
}

// objectDefinePropertyKey applies a descriptor to obj[key] for a string or
// symbol key.
func (rt *Runtime) objectDefinePropertyKey(obj Value, key Value, descVal Value) *ThrowError {
	// ToPropertyKey(P) precedes ToPropertyDescriptor(Attributes): a throwing key
	// coercion must be observed before the "descriptor must be an object" check
	// (Reflect.defineProperty / Object.defineProperty step ordering). The result
	// is a string/symbol, so the later key handling does not re-coerce it.
	pk, e := rt.toPropertyKey(key)
	if e != nil {
		return e
	}
	key = pk
	if !descVal.IsObjectType() {
		return rt.typeError("Property description must be an object")
	}
	// Only a STRING key on a namespace takes the namespace rule; a symbol falls
	// through to the ordinary path, where a new key on this non-extensible object
	// is correctly rejected. Testing isModuleNamespace alone would treat the
	// helper's symbol early-return as an acceptance.
	if !key.IsSymbol() && rt.isModuleNamespace(obj) {
		return rt.namespaceDefineProperty(obj, key, descVal)
	}
	o := rt.objPtr(obj)
	if o == nil {
		return rt.typeError("Object.defineProperty called on non-object")
	}
	if o.proxy != nil {
		return rt.proxyDefineProperty(o.proxy, key, descVal)
	}
	// Integer-indexed exotic [[DefineOwnProperty]]: any canonical numeric key is
	// an element key, defined through the buffer and never as an ordinary named
	// slot. A canonical numeric key that is not a live integer index is rejected.
	if obj.Type() == TTypedArray && !key.IsSymbol() {
		kname, e := rt.propKeyString(key)
		if e != nil {
			return e
		}
		if fidx, isNum := canonicalNumericIndex(kname); isNum {
			if idx, integral := integerIndex(fidx); integral {
				return rt.taDefineIndex(o, idx, descVal)
			}
			return rt.rejectDefine("Cannot define property: invalid typed array index")
		}
	}
	sym := key.IsSymbol()
	name := ""
	var symOff uint32
	var existing ownDesc
	if sym {
		symOff = key.handle()
		existing = o.ownDescriptorSym(symOff)
	} else {
		var e *ThrowError
		name, e = rt.propKeyString(key)
		if e != nil {
			return e
		}
		existing = o.ownDescriptor(name)
		// A live element in fast array storage is an own data property with default
		// attributes even though it has no shape slot; synthesize its descriptor so a
		// partial redefinition (e.g. {configurable:false}) preserves the existing
		// value/attributes instead of treating the index as new — which would default
		// the value to undefined and drop the element.
		if !existing.exists && obj.Type() == TArr {
			if idx, ok := canonicalIndex(name); ok && int(idx) < len(o.arr) && o.arr[idx] != tEmpty {
				existing = ownDesc{exists: true, value: o.arr[idx], writable: true, enumerable: true, configable: true}
			}
		}
	}
	// A new property cannot be defined on a non-extensible object; a
	// non-configurable existing property cannot be redefined.
	if !existing.exists && !o.flags.extensible {
		return rt.rejectDefine("Cannot define property, object is not extensible")
	}
	// A throwing accessor (or Proxy has/get trap) on the descriptor propagates;
	// getErr captures the first abrupt and short-circuits the remaining reads.
	var getErr *ThrowError
	get := func(k string) (Value, bool) {
		if getErr != nil {
			return mkundef(), false
		}
		h, e := rt.hasPropE(descVal, k)
		if e != nil {
			getErr = e
			return mkundef(), false
		}
		if h {
			v, e := rt.getField(descVal, k)
			if e != nil {
				getErr = e
				return mkundef(), false
			}
			return v, true
		}
		return mkundef(), false
	}
	// Field reads follow the spec ToPropertyDescriptor order (enumerable,
	// configurable, value, writable, get, set) — observable via a Proxy get trap.
	eV, hasE := get("enumerable")
	cV, hasC := get("configurable")
	valV, hasVal := get("value")
	wV, hasW := get("writable")
	getV, hasGet := get("get")
	setV, hasSet := get("set")
	if getErr != nil {
		return getErr
	}

	// A descriptor cannot mix accessor fields with data fields (ToPropertyDescriptor),
	// and get/set, when present, must be callable or undefined.
	if (hasGet || hasSet) && (hasVal || hasW) {
		return rt.typeError("Invalid property descriptor. Cannot both specify accessors and a value or writable attribute")
	}
	// Arguments exotic [[DefineOwnProperty]] (10.4.4.2). A mapped index that is
	// being made non-writable WITHOUT a value of its own first takes the value it
	// currently aliases — otherwise the stale value the ordinary property holds
	// would be resurrected as the frozen one. The map itself is updated after the
	// definition is applied, since a rejected definition changes nothing.
	argIdx := -1
	if !sym {
		argIdx = o.argMap.index(name)
	}
	if argIdx >= 0 && (hasVal || hasW) && !hasVal && hasW && !rt.toBoolean(wV) {
		valV, hasVal = o.argMap.get(argIdx), true
	}
	applyArgMap := func() {
		if argIdx < 0 {
			return
		}
		switch {
		case hasGet || hasSet:
			o.argMap.unmap(name) // an accessor is no longer an alias
		default:
			if hasVal {
				o.argMap.set(argIdx, valV)
			}
			if hasW && !rt.toBoolean(wV) {
				o.argMap.unmap(name)
			}
		}
	}
	if hasGet && !getV.IsUndefined() && !rt.isCallable(getV) {
		return rt.typeError("Getter must be a function")
	}
	if hasSet && !setV.IsUndefined() && !rt.isCallable(setV) {
		return rt.typeError("Setter must be a function")
	}
	// The Array "length" data property can never become an accessor.
	if !sym && obj.Type() == TArr && name == "length" && (hasGet || hasSet) {
		return rt.rejectDefine("Cannot redefine property: length")
	}

	// ValidateAndApplyPropertyDescriptor (10.1.6.3): a non-configurable existing
	// property tightly constrains what a redefinition may change. Only the exact
	// forbidden transitions reject — an identical (no-op) redefine is allowed.
	if existing.exists && !existing.configable {
		descIsAccessor := hasGet || hasSet
		descIsData := hasVal || hasW
		switch {
		case hasC && rt.toBoolean(cV):
			return rt.rejectDefine("Cannot redefine property")
		case hasE && rt.toBoolean(eV) != existing.enumerable:
			return rt.rejectDefine("Cannot redefine property")
		case descIsAccessor && !existing.isAccessor, descIsData && existing.isAccessor:
			return rt.rejectDefine("Cannot redefine property") // no data<->accessor conversion
		case existing.isAccessor:
			if hasGet && !rt.sameValue(getV, existing.getter) {
				return rt.rejectDefine("Cannot redefine property")
			}
			if hasSet && !rt.sameValue(setV, existing.setter) {
				return rt.rejectDefine("Cannot redefine property")
			}
		case !existing.writable:
			// Non-writable data: writable may not go true, value may not change.
			if hasW && rt.toBoolean(wV) {
				return rt.rejectDefine("Cannot redefine property")
			}
			if hasVal && !rt.sameValue(valV, existing.value) {
				return rt.rejectDefine("Cannot redefine property")
			}
		}
	}

	// Start from existing attrs (or all-false for a new property).
	writable, enumerable, configurable := existing.writable, existing.enumerable, existing.configable
	if hasW {
		writable = rt.toBoolean(wV)
	}
	if hasE {
		enumerable = rt.toBoolean(eV)
	}
	if hasC {
		configurable = rt.toBoolean(cV)
	}
	attrs := uint8(0)
	if writable {
		attrs |= attrWritable
	}
	if enumerable {
		attrs |= attrEnumerable
	}
	if configurable {
		attrs |= attrConfigurable
	}
	// Record that an integer-indexed accessor or non-writable indexed data
	// property now exists (it may live on a prototype), so an array-index [[Set]]
	// to an absent index knows it must walk the chain for an inherited interceptor.
	if !sym {
		resultIsAccessor := hasGet || hasSet || (existing.exists && existing.isAccessor && !hasVal && !hasW)
		if resultIsAccessor || !writable {
			if _, isIdx := canonicalIndex(name); isIdx {
				rt.indexedProtoIntercept = true
			}
		}
	}

	if hasGet || hasSet {
		g, s := existing.getter, existing.setter
		hg, hs := existing.isAccessor && !existing.getter.IsUndefined(), existing.isAccessor && !existing.setter.IsUndefined()
		if hasGet {
			g, hg = getV, !getV.IsUndefined()
		}
		if hasSet {
			s, hs = setV, !setV.IsUndefined()
		}
		if sym {
			o.defineAccessorSymbol(symOff, g, s, hg, hs, attrs)
		} else {
			o.defineAccessor(name, g, s, hg, hs, attrs)
			// An accessor on an array index moves out of fast storage and extends
			// the array length like any index [[DefineOwnProperty]].
			if obj.Type() == TArr {
				if idx, ok := canonicalIndex(name); ok {
					if int(idx) < len(o.arr) {
						o.arr[idx] = tEmpty
					}
					if idx+1 > o.arrLen {
						o.arrLen = idx + 1
					}
				}
			}
		}
		applyArgMap()
		return nil
	}
	// A generic descriptor (no value/writable and no get/set) over an existing
	// accessor keeps it an accessor — only the attributes change (do NOT fall
	// through to the data path, which would convert it to a data property).
	if !hasVal && !hasW && existing.exists && existing.isAccessor {
		hg, hs := !existing.getter.IsUndefined(), !existing.setter.IsUndefined()
		if sym {
			o.defineAccessorSymbol(symOff, existing.getter, existing.setter, hg, hs, attrs)
		} else {
			o.defineAccessor(name, existing.getter, existing.setter, hg, hs, attrs)
		}
		return nil
	}
	// A newly created data property (or one converted from an accessor) whose
	// descriptor omits `value` defaults to undefined — NOT the zero Value, which
	// NaN-decodes to the number 0. Only an existing data property keeps its value.
	val := existing.value
	if !existing.exists || existing.isAccessor {
		val = mkundef()
	}
	if hasVal {
		val = valV
	}
	// Array exotic [[DefineOwnProperty]]: a plain (all-default) data descriptor on
	// a canonical index writes fast element storage; a value on "length" retargets
	// the length. An index with non-default attributes falls through to a named
	// property (element reads fall back to it) but still extends the length.
	if obj.Type() == TArr && !sym {
		if name == "length" && !hasGet && !hasSet {
			// "length" is always non-configurable and non-enumerable: a descriptor
			// that would flip either attribute to true is rejected (a no-op false is
			// fine). existing is all-false here, so the resolved booleans are true
			// only when the descriptor explicitly requested the forbidden change.
			if configurable {
				return rt.rejectDefine("Cannot redefine property: length")
			}
			if enumerable {
				return rt.rejectDefine("Cannot redefine property: length")
			}
			// Array exotic length [[DefineOwnProperty]]: a non-writable length cannot
			// be changed or made writable again; a value sets the length; writable:false
			// locks it.
			if o.flags.arrLenNonWritable {
				if hasVal {
					if nl, e := rt.toNumber(val); e != nil {
						return e
					} else if uint32(nl) != o.arrLen || float64(uint32(nl)) != nl {
						return rt.rejectDefine("Cannot redefine property: length")
					}
				}
				if hasW && writable {
					return rt.rejectDefine("Cannot redefine property: length")
				}
				return nil
			}
			if hasVal {
				ok, e := rt.setArrayLength(obj, val)
				if e != nil {
					return e
				}
				if !ok {
					// A non-configurable element blocked the requested shrink: length
					// stops one past that index. Per ArraySetLength step 17.d, a
					// requested writable:false is still applied to the length before the
					// define is reported as failed (which throws in a throwing context).
					if hasW && !writable {
						o.flags.arrLenNonWritable = true
					}
					return rt.rejectDefine("Cannot redefine property: length")
				}
			}
			if hasW && !writable {
				o.flags.arrLenNonWritable = true
			}
			return nil
		}
		if idx, ok := canonicalIndex(name); ok {
			// Defining an index at or beyond the current length would extend
			// length; a non-writable length forbids that (ArraySetLength step,
			// 15.4.5.1 step 4.b).
			if idx >= o.arrLen && o.flags.arrLenNonWritable {
				return rt.rejectDefine("Cannot define property: array length is not writable")
			}
			if writable && enumerable && configurable {
				// A prior non-default definition of this index lives as a named shape
				// slot that shadows fast element storage; drop it so the all-default
				// fast element is the property observed (otherwise a redefine of a
				// configurable index would keep the stale named descriptor).
				if o.shape.lookupInterned(name) >= 0 {
					o.deleteOwn(name)
				}
				rt.arraySet(o, idx, val)
				return nil
			}
			// Move the index out of fast storage into a named property (with its
			// attributes); clear any shadowing fast element so reads see the named
			// one, and extend the length past it.
			if int(idx) < len(o.arr) {
				o.arr[idx] = tEmpty
			}
			o.defineOwn(name, val, attrs)
			if idx+1 > o.arrLen { // array [[DefineOwnProperty]] extends length
				o.arrLen = idx + 1
			}
			return nil
		}
	}
	if sym {
		o.defineOwnSymbol(symOff, val, attrs)
	} else {
		o.defineOwn(name, val, attrs)
	}
	applyArgMap()
	return nil
}

// objectDefineProperties applies a map of descriptors (Object.defineProperties).
func (rt *Runtime) objectDefineProperties(obj, props Value) *ThrowError {
	// ObjectDefineProperties (20.1.2.3.1) begins with ToObject(Properties): a
	// primitive is boxed (its wrapper has no own enumerable descriptors, so it is
	// a no-op), and null/undefined is a TypeError.
	props, e := rt.toObjectValue(props)
	if e != nil {
		return e
	}
	// Enumerate via [[OwnPropertyKeys]] + [[GetOwnProperty]] (proxy traps), then
	// read each descriptor object via [[Get]].
	keys, e := rt.enumerableOwnKeysE(props)
	if e != nil {
		return e
	}
	for _, k := range keys {
		desc, e := rt.getField(props, k)
		if e != nil {
			return e
		}
		if e := rt.objectDefineProperty(obj, k, desc); e != nil {
			return e
		}
	}
	return nil
}

// descriptorToObject builds a descriptor object from an ownDesc.
func (rt *Runtime) descriptorToObject(d ownDesc) Value {
	obj := rt.newPlainObject()
	o := rt.objPtr(obj)
	if d.isAccessor {
		o.defineOwn("get", d.getter, attrDefault)
		o.defineOwn("set", d.setter, attrDefault)
	} else {
		o.defineOwn("value", d.value, attrDefault)
		o.defineOwn("writable", mkbool(d.writable), attrDefault)
	}
	o.defineOwn("enumerable", mkbool(d.enumerable), attrDefault)
	o.defineOwn("configurable", mkbool(d.configable), attrDefault)
	return obj
}

// completeDescriptor round-trips a user descriptor object through
// ToPropertyDescriptor + CompletePropertyDescriptor + FromPropertyDescriptor,
// yielding a fresh object with every field present (absent fields defaulted).
func (rt *Runtime) completeDescriptor(desc Value) Value {
	has := func(name string) bool { return rt.hasProp(desc, name) }
	get := func(name string) Value { v, _ := rt.getField(desc, name); return v }
	isAccessor := has("get") || has("set")
	var d ownDesc
	d.isAccessor = isAccessor
	if isAccessor {
		if has("get") {
			d.getter = get("get")
		} else {
			d.getter = mkundef()
		}
		if has("set") {
			d.setter = get("set")
		} else {
			d.setter = mkundef()
		}
	} else {
		if has("value") {
			d.value = get("value")
		} else {
			d.value = mkundef()
		}
		d.writable = has("writable") && rt.toBoolean(get("writable"))
	}
	d.enumerable = has("enumerable") && rt.toBoolean(get("enumerable"))
	d.configable = has("configurable") && rt.toBoolean(get("configurable"))
	return rt.descriptorToObject(d)
}

func (rt *Runtime) makeDataDescriptor(v Value, w, e, c bool) Value {
	obj := rt.newPlainObject()
	o := rt.objPtr(obj)
	o.defineOwn("value", v, attrDefault)
	o.defineOwn("writable", mkbool(w), attrDefault)
	o.defineOwn("enumerable", mkbool(e), attrDefault)
	o.defineOwn("configurable", mkbool(c), attrDefault)
	return obj
}

// ownPropertyNames returns own keys (including non-enumerable when all=false
// means include-all). Integer indices come first.
func (rt *Runtime) ownPropertyNames(v Value, enumerableOnly bool) (Value, *ThrowError) {
	arr := rt.newArray()
	if v.IsString() {
		ao := rt.objPtr(arr)
		n := utf16Len(rt.strBytes(v))
		for i := 0; i < n; i++ {
			rt.arraySet(ao, ao.arrLen, rt.newString(strconv.Itoa(i)))
		}
		if !enumerableOnly {
			rt.arraySet(ao, ao.arrLen, rt.internString("length"))
		}
		return arr, nil
	}
	o := rt.objPtr(v)
	if o == nil {
		return arr, nil
	}
	ao := rt.objPtr(arr)
	if o.proxy != nil {
		// The [[OwnPropertyKeys]] trap validates its own invariants (only property
		// keys, no duplicates, non-configurable/non-extensible coverage) even
		// though getOwnPropertyNames keeps only the String keys — so its abrupt
		// completion must propagate.
		keys, e := rt.proxyOwnKeys(o.proxy)
		if e != nil {
			return mkundef(), e
		}
		for _, kv := range keys {
			if kv.IsString() {
				rt.arraySet(ao, ao.arrLen, kv)
			}
		}
		return arr, nil
	}
	if v.Type() == TArr {
		for i := uint32(0); i < o.arrLen; i++ {
			if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
				rt.arraySet(ao, ao.arrLen, rt.newString(numberToString(float64(i))))
			}
		}
		rt.arraySet(ao, ao.arrLen, rt.internString("length"))
	}
	if v.Type() == TTypedArray {
		// Integer indices [0, length) are the typed array's own enumerable data
		// properties (length lives on the prototype).
		for i, l := 0, rt.taLength(o); i < l; i++ {
			rt.arraySet(ao, ao.arrLen, rt.newString(strconv.Itoa(i)))
		}
	}
	if o.boxed.Type() == TStr { // String wrapper: char indices are own (then "length")
		for i, l := 0, utf16Len(rt.strBytes(o.boxed)); i < l; i++ {
			rt.arraySet(ao, ao.arrLen, rt.newString(strconv.Itoa(i)))
		}
	}
	keys := o.ownKeys()
	if enumerableOnly {
		keys = o.ownKeysEnumerable()
	}
	for _, k := range keys {
		rt.arraySet(ao, ao.arrLen, rt.internString(k))
	}
	return arr, nil
}

// sealObject implements Object.seal / Object.freeze.
func (rt *Runtime) sealObject(v Value, freeze bool) *ThrowError {
	o := rt.objPtr(v)
	if o == nil {
		return nil
	}
	if o.proxy != nil {
		// SetIntegrityLevel via the proxy's preventExtensions + ownKeys +
		// getOwnPropertyDescriptor + defineProperty traps. A false result from
		// [[PreventExtensions]] makes SetIntegrityLevel fail (a TypeError here).
		ok, e := rt.proxyPreventExtensions(o.proxy)
		if e != nil {
			return e
		}
		if !ok {
			return rt.typeError("Object.freeze/seal: proxy preventExtensions trap returned false")
		}
		keys, e := rt.proxyOwnKeys(o.proxy)
		if e != nil {
			return e
		}
		for _, k := range keys {
			desc := rt.newPlainObject()
			do := rt.objPtr(desc)
			do.defineOwn("configurable", mkfalse(), attrDefault)
			if freeze {
				cur, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, k)
				if e != nil {
					return e
				}
				if cur.IsUndefined() {
					continue
				}
				g, _ := rt.getField(cur, "get")
				s, _ := rt.getField(cur, "set")
				if !rt.isCallable(g) && !rt.isCallable(s) {
					do.defineOwn("writable", mkfalse(), attrDefault)
				}
			}
			if e := rt.proxyDefineProperty(o.proxy, k, desc); e != nil {
				return e
			}
		}
		return nil
	}
	// A module namespace's [[DefineOwnProperty]] accepts only a no-op. Sealing
	// asks for {configurable: false}, which every export already is; FREEZING also
	// asks for {writable: false}, which no export is — so it is rejected, after the
	// [[GetOwnProperty]] read that precedes it.
	if rt.moduleNamespaces[o] {
		if freeze {
			for _, k := range o.ownKeys() {
				if e := rt.namespaceTDZ(v, k); e != nil {
					return e
				}
				return rt.typeError("Cannot redefine property: " + k)
			}
		}
		o.flags.sealed = true
		return nil
	}
	o.flags.extensible = false
	o.ensureUniqueShape()
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		p.attrs &^= attrConfigurable
		if freeze && !p.isAccessor {
			p.attrs &^= attrWritable
		}
	}
	icEpochBump()
	if freeze {
		o.flags.frozen = true
		if v.Type() == TArr {
			// A frozen array's length becomes non-writable (its elements are locked
			// by the attribute sweep above).
			o.flags.arrLenNonWritable = true
		}
	}
	o.flags.sealed = true
	return nil
}

// isSealedOrFrozen checks Object.isSealed / isFrozen.
func (rt *Runtime) isSealedOrFrozen(v Value, frozen bool) bool {
	sealed, _ := rt.isSealedOrFrozenE(v, frozen)
	return sealed
}

func (rt *Runtime) isSealedOrFrozenE(v Value, frozen bool) (bool, *ThrowError) {
	o := rt.objPtr(v)
	if o == nil {
		return true, nil // primitives are trivially sealed/frozen
	}
	if o.proxy != nil {
		// TestIntegrityLevel via isExtensible + ownKeys + getOwnPropertyDescriptor.
		ext, e := rt.proxyIsExtensible(o.proxy)
		if e != nil {
			return false, e
		}
		if ext {
			return false, nil
		}
		keys, e := rt.proxyOwnKeys(o.proxy)
		if e != nil {
			return false, e
		}
		for _, k := range keys {
			cur, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, k)
			if e != nil {
				return false, e
			}
			if cur.IsUndefined() {
				continue
			}
			if cfg, _ := rt.getField(cur, "configurable"); rt.toBoolean(cfg) {
				return false, nil
			}
			if frozen {
				g, _ := rt.getField(cur, "get")
				s, _ := rt.getField(cur, "set")
				if !rt.isCallable(g) && !rt.isCallable(s) {
					if w, _ := rt.getField(cur, "writable"); rt.toBoolean(w) {
						return false, nil
					}
				}
			}
		}
		return true, nil
	}
	if o.flags.extensible {
		return false, nil
	}
	// A module namespace stores its exports as accessors so reads see the live
	// binding, but REPORTS them as writable data properties — so it is never
	// frozen, however many of them there are.
	if frozen && rt.moduleNamespaces[o] && len(o.ownKeys()) > 0 {
		return false, nil
	}
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		if p.attrs&attrConfigurable != 0 {
			return false, nil
		}
		if frozen && !(p.hasGetter || p.hasSetter) && p.attrs&attrWritable != 0 {
			return false, nil
		}
	}
	return true, nil
}

// objectToStringTag returns Object.prototype.toString's "[object Tag]" result.
func (rt *Runtime) objectToStringTag(v Value) (string, *ThrowError) {
	switch v.Type() {
	case TUndef:
		return "[object Undefined]", nil
	case TNull:
		return "[object Null]", nil
	}
	// IsArray unwraps proxies and throws on a revoked proxy (7.2.2), so a proxy
	// over an array tags as "Array".
	isArr, e := rt.isArrayE(v)
	if e != nil {
		return "", e
	}
	builtin := "Object"
	switch {
	case isArr:
		builtin = "Array"
		// The arguments object is array-backed but tags as "Arguments".
		if o := rt.objPtr(v); o != nil && o.hasOwn("callee") {
			builtin = "Arguments"
		}
	case v.Type() == TFunc, v.Type() == TCFunc:
		builtin = "Function"
	case v.Type() == TStr:
		builtin = "String"
	case v.Type() == TNum:
		builtin = "Number"
	case v.Type() == TBool:
		builtin = "Boolean"
	default:
		if o := rt.objPtr(v); o != nil {
			switch {
			case o.flags.isCallable:
				builtin = "Function"
			case o.brandID() == brandDate:
				builtin = "Date"
			case o.regex != nil:
				builtin = "RegExp"
			case o.brandID() == brandError:
				builtin = "Error"
			// A boxed String/Boolean wrapper (new String / new Boolean) tags by its
			// [[Class]]. A fresh object's zero-value boxed decodes as the number 0, so
			// only these two (whose tags differ from Number) are safe to key off boxed
			// here; a Number wrapper is not distinguishable this way and is left as-is.
			case o.boxed.Type() == TStr:
				builtin = "String"
			case o.boxed.Type() == TBool:
				builtin = "Boolean"
			case o.getSlot(slotPrimitive).Type() == TNum: // Number wrapper ([[NumberData]])
				builtin = "Number"
			case o.hasOwn("callee") && o.hasOwn("length"):
				builtin = "Arguments"
			}
		}
	}
	// A string-valued Symbol.toStringTag (own or inherited, via the wrapper
	// prototype for primitives) overrides the built-in tag; its Get can throw
	// (a proxy get trap or a throwing accessor) and must propagate.
	if rt.symToStringTag != 0 {
		lookup := v
		if !v.IsObjectType() {
			lookup = rt.primitiveProto(v)
		}
		if lookup.IsObjectType() {
			tag, e := rt.getFieldSymbol(lookup, rt.symToStringTag.handle())
			if e != nil {
				return "", e
			}
			if tag.IsString() {
				builtin = string(rt.strBytes(tag))
			}
		}
	}
	return "[object " + builtin + "]", nil
}

// objectKeys returns an array of own enumerable string keys (integer indices
// first in ascending order, then insertion order).
func (rt *Runtime) objectKeys(v Value) Value {
	arr := rt.newArray()
	if v.IsString() {
		ao := rt.objPtr(arr)
		n := utf16Len(rt.strBytes(v))
		for i := 0; i < n; i++ {
			rt.arraySet(ao, ao.arrLen, rt.newString(strconv.Itoa(i)))
		}
		return arr
	}
	o := rt.objPtr(v)
	if o == nil {
		return arr
	}
	ao := rt.objPtr(arr)
	if o.proxy != nil {
		// Route through the ownKeys + getOwnPropertyDescriptor traps.
		for _, k := range rt.enumerableOwnKeys(v) {
			rt.arraySet(ao, ao.arrLen, rt.internString(k))
		}
		return arr
	}
	if v.Type() == TArr {
		for i := uint32(0); i < o.arrLen; i++ {
			if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
				rt.arraySet(ao, ao.arrLen, rt.newString(numberToString(float64(i))))
			}
		}
	}
	if v.Type() == TTypedArray {
		for i, l := 0, rt.taLength(o); i < l; i++ {
			rt.arraySet(ao, ao.arrLen, rt.newString(strconv.Itoa(i)))
		}
	}
	for _, k := range o.ownKeysEnumerable() {
		rt.arraySet(ao, ao.arrLen, rt.internString(k))
	}
	return arr
}
