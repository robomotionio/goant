package engine

// Object constructor + Object.prototype (ant ant.c object sections /
// builtin_object). The prototype root's methods (toString/valueOf/
// hasOwnProperty/isPrototypeOf/propertyIsEnumerable) plus core Object statics.

func (rt *Runtime) initObjectBuiltin() {
	proto := rt.objPtr(rt.objectProto)

	rt.defMethod(proto, "hasOwnProperty", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return mkfalse(), nil
		}
		name, e := rt.propKeyString(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		if this.Type() == TArr {
			if idx, ok := arrayIndex(arg(args, 0)); ok {
				return mkbool(idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty()), nil
			}
			if name == "length" {
				return mktrue(), nil
			}
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
		o := rt.objPtr(this)
		if o == nil {
			return mkfalse(), nil
		}
		name, e := rt.propKeyString(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		slot := o.shape.lookupInterned(name)
		return mkbool(slot >= 0 && o.shape.attrsAt(uint32(slot))&attrEnumerable != 0), nil
	})

	rt.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.internString(rt.objectToStringTag(this)), nil
	})
	rt.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.toStringValue(this)
	})
	rt.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return this, nil
	})

	// Object constructor.
	ctor := rt.newNativeFunc("Object", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		if v.IsNullish() {
			return rt.newPlainObject(), nil
		}
		if v.IsObjectType() {
			return v, nil
		}
		return rt.newPlainObject(), nil // primitive wrapper boxing (Phase 4 refine)
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.objectProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)

	rt.defMethod(cobj, "keys", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.objectKeys(arg(args, 0)), nil
	})
	rt.defMethod(cobj, "getPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mknull(), nil
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
		if !obj.IsObjectType() {
			return mkundef(), rt.typeError("Object.defineProperty called on non-object")
		}
		name, e := rt.propKeyString(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		if e := rt.objectDefineProperty(obj, name, arg(args, 2)); e != nil {
			return mkundef(), e
		}
		return obj, nil
	})
	rt.defMethod(cobj, "defineProperties", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj := arg(args, 0)
		if !obj.IsObjectType() {
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
		name, e := rt.propKeyString(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		d := o.ownDescriptor(name)
		if !d.exists {
			if arg(args, 0).Type() == TArr {
				if idx, ok := arrayIndex(arg(args, 1)); ok && idx < o.arrLen {
					v, _ := rt.getElement(arg(args, 0), arg(args, 1))
					return rt.makeDataDescriptor(v, true, true, true), nil
				}
			}
			return mkundef(), nil
		}
		return rt.descriptorToObject(d), nil
	})
	rt.defMethod(cobj, "getOwnPropertyNames", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.ownPropertyNames(arg(args, 0), false), nil
	})
	rt.defMethod(cobj, "preventExtensions", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if o := rt.objPtr(arg(args, 0)); o != nil {
			o.flags.extensible = false
		}
		return arg(args, 0), nil
	})
	rt.defMethod(cobj, "isExtensible", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		return mkbool(o != nil && o.flags.extensible), nil
	})
	rt.defMethod(cobj, "seal", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		rt.sealObject(arg(args, 0), false)
		return arg(args, 0), nil
	})
	rt.defMethod(cobj, "freeze", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		rt.sealObject(arg(args, 0), true)
		return arg(args, 0), nil
	})
	rt.defMethod(cobj, "isSealed", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkbool(rt.isSealedOrFrozen(arg(args, 0), false)), nil
	})
	rt.defMethod(cobj, "isFrozen", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkbool(rt.isSealedOrFrozen(arg(args, 0), true)), nil
	})

	rt.defMethod(cobj, "assign", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target := arg(args, 0)
		if target.IsNullish() {
			return mkundef(), rt.typeError("Cannot convert undefined or null to object")
		}
		for _, src := range args[1:] {
			if src.IsNullish() {
				continue
			}
			so := rt.objPtr(src)
			if so == nil {
				continue
			}
			for _, k := range rt.enumerableOwnKeys(src) {
				v, e := rt.getField(src, k)
				if e != nil {
					return mkundef(), e
				}
				if e := rt.setField(target, k, v); e != nil {
					return mkundef(), e
				}
			}
		}
		return target, nil
	})
	rt.defMethod(cobj, "is", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkbool(rt.sameValue(arg(args, 0), arg(args, 1))), nil
	})
	rt.defMethod(cobj, "setPrototypeOf", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj := arg(args, 0)
		if o := rt.objPtr(obj); o != nil {
			p := arg(args, 1)
			if p.IsObjectType() || p.IsNull() {
				o.proto = p
			}
		}
		return obj, nil
	})
	rt.defMethod(cobj, "values", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res := rt.newArray()
		ro := rt.objPtr(res)
		for _, k := range rt.enumerableOwnKeys(arg(args, 0)) {
			v, _ := rt.getField(arg(args, 0), k)
			rt.arraySet(ro, ro.arrLen, v)
		}
		return res, nil
	})
	rt.defMethod(cobj, "entries", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res := rt.newArray()
		ro := rt.objPtr(res)
		for _, k := range rt.enumerableOwnKeys(arg(args, 0)) {
			v, _ := rt.getField(arg(args, 0), k)
			pair := rt.newArray()
			po := rt.objPtr(pair)
			rt.arraySet(po, 0, rt.internString(k))
			rt.arraySet(po, 1, v)
			rt.arraySet(ro, ro.arrLen, pair)
		}
		return res, nil
	})
	rt.defMethod(cobj, "fromEntries", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res := rt.newPlainObject()
		o := rt.objPtr(res)
		vals, e := rt.iterableValues(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		for _, entry := range vals {
			k, _ := rt.getElement(entry, mknum(0))
			v, _ := rt.getElement(entry, mknum(1))
			name, e := rt.propKeyString(k)
			if e != nil {
				return mkundef(), e
			}
			o.defineOwn(name, v, attrDefault)
		}
		return res, nil
	})

	rt.defGlobal("Object", ctor)
}

// enumerableOwnKeys returns own enumerable string keys (array indices first),
// used by Object.assign/values/entries and JSON.
func (rt *Runtime) enumerableOwnKeys(v Value) []string {
	o := rt.objPtr(v)
	if o == nil {
		return nil
	}
	var keys []string
	if v.Type() == TArr {
		for i := uint32(0); i < o.arrLen; i++ {
			if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
				keys = append(keys, numberToString(float64(i)))
			}
		}
	}
	keys = append(keys, o.ownKeysEnumerable()...)
	return keys
}

// objectDefineProperty applies an ES5 property descriptor to obj[name].
func (rt *Runtime) objectDefineProperty(obj Value, name string, descVal Value) *ThrowError {
	if !descVal.IsObjectType() {
		return rt.typeError("Property description must be an object")
	}
	o := rt.objPtr(obj)
	existing := o.ownDescriptor(name)
	get := func(k string) (Value, bool) {
		if rt.hasProp(descVal, k) {
			v, _ := rt.getField(descVal, k)
			return v, true
		}
		return mkundef(), false
	}
	getV, hasGet := get("get")
	setV, hasSet := get("set")
	valV, hasVal := get("value")
	wV, hasW := get("writable")
	eV, hasE := get("enumerable")
	cV, hasC := get("configurable")

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
		o.defineAccessor(name, g, s, hg, hs, attrs)
		return nil
	}
	val := existing.value
	if hasVal {
		val = valV
	}
	o.defineOwn(name, val, attrs)
	return nil
}

// objectDefineProperties applies a map of descriptors (Object.defineProperties).
func (rt *Runtime) objectDefineProperties(obj, props Value) *ThrowError {
	po := rt.objPtr(props)
	if po == nil {
		return rt.typeError("Property descriptors must be an object")
	}
	for _, k := range po.ownKeysEnumerable() {
		desc, _ := rt.getField(props, k)
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
	o := rt.objPtr(v)
	if o == nil {
		return arr
	}
	ao := rt.objPtr(arr)
	if v.Type() == TArr {
		for i := uint32(0); i < o.arrLen; i++ {
			if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
				rt.arraySet(ao, ao.arrLen, rt.newString(numberToString(float64(i))))
			}
		}
		rt.arraySet(ao, ao.arrLen, rt.internString("length"))
	}
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		if p.key.sym {
			continue
		}
		if enumerableOnly && p.attrs&attrEnumerable == 0 {
			continue
		}
		rt.arraySet(ao, ao.arrLen, rt.internString(p.key.str))
	}
	return arr
}

// sealObject implements Object.seal / Object.freeze.
func (rt *Runtime) sealObject(v Value, freeze bool) {
	o := rt.objPtr(v)
	if o == nil {
		return
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
	}
	o.flags.sealed = true
}

// isSealedOrFrozen checks Object.isSealed / isFrozen.
func (rt *Runtime) isSealedOrFrozen(v Value, frozen bool) bool {
	o := rt.objPtr(v)
	if o == nil {
		return true // primitives are trivially sealed/frozen
	}
	if o.flags.extensible {
		return false
	}
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		if p.attrs&attrConfigurable != 0 {
			return false
		}
		if frozen && !(p.hasGetter || p.hasSetter) && p.attrs&attrWritable != 0 {
			return false
		}
	}
	return true
}

// objectToStringTag returns Object.prototype.toString's "[object Tag]" result.
func (rt *Runtime) objectToStringTag(v Value) string {
	switch v.Type() {
	case TUndef:
		return "[object Undefined]"
	case TNull:
		return "[object Null]"
	case TArr:
		return "[object Array]"
	case TFunc, TCFunc:
		return "[object Function]"
	case TStr:
		return "[object String]"
	case TNum:
		return "[object Number]"
	case TBool:
		return "[object Boolean]"
	default:
		return "[object Object]"
	}
}

// objectKeys returns an array of own enumerable string keys (integer indices
// first in ascending order, then insertion order).
func (rt *Runtime) objectKeys(v Value) Value {
	arr := rt.newArray()
	o := rt.objPtr(v)
	if o == nil {
		return arr
	}
	ao := rt.objPtr(arr)
	if v.Type() == TArr {
		for i := uint32(0); i < o.arrLen; i++ {
			if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
				rt.arraySet(ao, ao.arrLen, rt.newString(numberToString(float64(i))))
			}
		}
	}
	for _, k := range o.ownKeysEnumerable() {
		rt.arraySet(ao, ao.arrLen, rt.internString(k))
	}
	return arr
}
