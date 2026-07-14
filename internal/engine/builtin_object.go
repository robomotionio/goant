package engine

import "strconv"

// Object constructor + Object.prototype (ant ant.c object sections /
// builtin_object). The prototype root's methods (toString/valueOf/
// hasOwnProperty/isPrototypeOf/propertyIsEnumerable) plus core Object statics.

func (rt *Runtime) initObjectBuiltin() {
	proto := rt.objPtr(rt.objectProto)

	rt.defMethod(proto, "hasOwnProperty", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e0 := rt.toObjectValue(this)
		if e0 != nil {
			return mkundef(), e0
		}
		o := rt.objPtr(obj)
		if o == nil {
			return mkfalse(), nil
		}
		// HasOwnProperty -> [[GetOwnProperty]] routes through the proxy trap.
		if o.proxy != nil {
			d, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, rt.toPropertyKeyValue(arg(args, 0)))
			if e != nil {
				return mkundef(), e
			}
			return mkbool(!d.IsUndefined()), nil
		}
		if key := arg(args, 0); key.IsSymbol() {
			return mkbool(o.shape.lookupSymbol(key.handle()) >= 0), nil
		}
		name, e := rt.propKeyString(arg(args, 0))
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
		if this.Type() == TArr && name == "length" {
			return mktrue(), nil
		}
		return mkbool(o.hasOwn(name)), nil
	})

	rt.defMethod(proto, "isPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target := arg(args, 0)
		if !target.IsObjectType() {
			return mkfalse(), nil
		}
		cur := rt.objPtr(target)
		for depth := 0; depth < maxProtoChainDepth && cur != nil; depth++ {
			if !cur.proto.IsObjectType() {
				break
			}
			if cur.proto == this {
				return mktrue(), nil
			}
			cur = rt.objPtr(cur.proto)
		}
		return mkfalse(), nil
	})

	rt.defMethod(proto, "propertyIsEnumerable", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		key := arg(args, 0)
		isSym := key.IsSymbol()
		var name string
		if !isSym {
			var e *ThrowError
			if name, e = rt.propKeyString(key); e != nil {
				return mkundef(), e
			}
		}
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(obj)
		if o == nil {
			return mkfalse(), nil
		}
		if isSym {
			d := o.ownDescriptorSym(key.handle())
			return mkbool(d.exists && d.enumerable), nil
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
		return rt.internString(rt.objectToStringTag(this)), nil
	})
	rt.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.toStringValue(this)
	})
	rt.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.toObjectValue(this) // ToObject(this): a primitive returns its wrapper
	})

	// Object constructor.
	ctor := rt.newNativeFunc("Object", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
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
		if !p.IsObjectType() && !p.IsNull() {
			return mkundef(), rt.typeError("Object prototype may only be an Object or null")
		}
		obj := rt.newObject(p)
		if props := arg(args, 1); props.IsObjectType() {
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
		o := rt.objPtr(arg(args, 0))
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
					return rt.makeDataDescriptor(v, true, true, true), nil
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
		return rt.ownPropertyNames(obj, false), nil
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
		key := arg(args, 1)
		if key.IsSymbol() {
			return mkbool(o.shape.lookupSymbol(key.handle()) >= 0), nil
		}
		name, e := rt.propKeyString(key)
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
		items, e := rt.iterableValues(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 1)
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
		res := rt.newArray()
		ra := rt.objPtr(res)
		if o := rt.objPtr(arg(args, 0)); o != nil {
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
		switch obj.Type() {
		case TArr:
			for i := uint32(0); i < o.arrLen; i++ {
				if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
					reso.defineOwn(strconv.Itoa(int(i)), rt.makeDataDescriptor(o.arr[i], true, true, true), attrDefault)
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
				if e := rt.proxyPreventExtensions(o.proxy); e != nil {
					return mkundef(), e
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
				if e := rt.proxySetPrototypeOf(o.proxy, p); e != nil {
					return mkundef(), e
				}
			} else if p.IsObjectType() || p.IsNull() {
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
		res := rt.newPlainObject()
		o := rt.objPtr(res)
		vals, e := rt.iterableValues(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		for _, entry := range vals {
			if !entry.IsObjectType() {
				return mkundef(), rt.typeError("iterator value is not an entry object")
			}
			k, e := rt.getElement(entry, mknum(0))
			if e != nil {
				return mkundef(), e
			}
			v, e := rt.getElement(entry, mknum(1))
			if e != nil {
				return mkundef(), e
			}
			// CreateDataPropertyOnObject with ToPropertyKey(k) (symbol keys allowed).
			pk, e := rt.toPropertyKey(k)
			if e != nil {
				return mkundef(), e
			}
			if pk.IsSymbol() {
				o.defineOwnSymbol(pk.handle(), v, attrDefault)
			} else {
				o.defineOwn(string(rt.strBytes(pk)), v, attrDefault)
			}
		}
		return res, nil
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
			return rt.proxySet(o.proxy, key, val, to)
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
	if !descVal.IsObjectType() {
		return rt.typeError("Property description must be an object")
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
	}
	// A new property cannot be defined on a non-extensible object; a
	// non-configurable existing property cannot be redefined.
	if !existing.exists && !o.flags.extensible {
		return rt.rejectDefine("Cannot define property, object is not extensible")
	}
	get := func(k string) (Value, bool) {
		if rt.hasProp(descVal, k) {
			v, _ := rt.getField(descVal, k)
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

	// A descriptor cannot mix accessor fields with data fields (ToPropertyDescriptor),
	// and get/set, when present, must be callable or undefined.
	if (hasGet || hasSet) && (hasVal || hasW) {
		return rt.typeError("Invalid property descriptor. Cannot both specify accessors and a value or writable attribute")
	}
	if hasGet && !getV.IsUndefined() && !rt.isCallable(getV) {
		return rt.typeError("Getter must be a function")
	}
	if hasSet && !setV.IsUndefined() && !rt.isCallable(setV) {
		return rt.typeError("Setter must be a function")
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
					// A non-configurable element blocked the requested shrink.
					return rt.rejectDefine("Cannot redefine property: length")
				}
			}
			if hasW && !writable {
				o.flags.arrLenNonWritable = true
			}
			return nil
		}
		if idx, ok := canonicalIndex(name); ok {
			if writable && enumerable && configurable {
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
func (rt *Runtime) ownPropertyNames(v Value, enumerableOnly bool) Value {
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
		return arr
	}
	o := rt.objPtr(v)
	if o == nil {
		return arr
	}
	ao := rt.objPtr(arr)
	if o.proxy != nil {
		keys, _ := rt.proxyOwnKeys(o.proxy)
		for _, kv := range keys {
			if kv.IsString() {
				rt.arraySet(ao, ao.arrLen, kv)
			}
		}
		return arr
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
	return arr
}

// sealObject implements Object.seal / Object.freeze.
func (rt *Runtime) sealObject(v Value, freeze bool) *ThrowError {
	o := rt.objPtr(v)
	if o == nil {
		return nil
	}
	if o.proxy != nil {
		// SetIntegrityLevel via the proxy's preventExtensions + ownKeys +
		// getOwnPropertyDescriptor + defineProperty traps.
		if e := rt.proxyPreventExtensions(o.proxy); e != nil {
			return e
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
	o.flags.extensible = false
	o.ensureUniqueShape()
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		p.attrs &^= attrConfigurable
		if freeze && !(p.hasGetter || p.hasSetter) {
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
func (rt *Runtime) objectToStringTag(v Value) string {
	switch v.Type() {
	case TUndef:
		return "[object Undefined]"
	case TNull:
		return "[object Null]"
	}
	builtin := "Object"
	switch v.Type() {
	case TArr:
		builtin = "Array"
		// The arguments object is array-backed but tags as "Arguments".
		if o := rt.objPtr(v); o != nil && o.hasOwn("callee") {
			builtin = "Arguments"
		}
	case TFunc, TCFunc:
		builtin = "Function"
	case TStr:
		builtin = "String"
	case TNum:
		builtin = "Number"
	case TBool:
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
			case o.hasOwn("callee") && o.hasOwn("length"):
				builtin = "Arguments"
			}
		}
	}
	// A string-valued Symbol.toStringTag (own or inherited, via the wrapper
	// prototype for primitives) overrides the built-in tag.
	if rt.symToStringTag != 0 {
		lookup := v
		if !v.IsObjectType() {
			lookup = rt.primitiveProto(v)
		}
		if lookup.IsObjectType() {
			if tag, _ := rt.getFieldSymbol(lookup, rt.symToStringTag.handle()); tag.IsString() {
				builtin = string(rt.strBytes(tag))
			}
		}
	}
	return "[object " + builtin + "]"
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
