package engine

import "strconv"

// Reflect (ant modules/reflect.c). A namespace object of the reflective object
// operations, reusing the same internal protocol that backs the operators and
// Object.* statics. Reflect methods return booleans for the [[Set]]-family ops
// (never throw on rejection) and forward faithfully otherwise.

func (rt *Runtime) initReflectBuiltin() {
	reflect := rt.newObject(rt.objectProto)
	ro := rt.objPtr(reflect)

	needObj := func(v Value, m string) *ThrowError {
		if !v.IsObjectType() {
			return rt.typeError("Reflect." + m + " called on non-object")
		}
		return nil
	}

	rt.defMethod(ro, "get", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := needObj(arg(args, 0), "get"); e != nil {
			return mkundef(), e
		}
		return rt.getElement(arg(args, 0), arg(args, 1))
	})
	rt.defMethod(ro, "set", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := needObj(arg(args, 0), "set"); e != nil {
			return mkundef(), e
		}
		if e := rt.setElement(arg(args, 0), arg(args, 1), arg(args, 2)); e != nil {
			return mkfalse(), nil
		}
		return mktrue(), nil
	})
	rt.defMethod(ro, "has", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := needObj(arg(args, 0), "has"); e != nil {
			return mkundef(), e
		}
		key := arg(args, 1)
		if key.IsSymbol() {
			return mkbool(rt.hasFieldSymbol(arg(args, 0), key.handle())), nil
		}
		name, e := rt.propKeyString(key)
		if e != nil {
			return mkundef(), e
		}
		return mkbool(rt.hasProp(arg(args, 0), name)), nil
	})
	rt.defMethod(ro, "deleteProperty", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.deleteProperty called on non-object")
		}
		name, e := rt.propKeyString(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return mkbool(o.deleteOwn(name)), nil
	})
	rt.defMethod(ro, "getPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.getPrototypeOf called on non-object")
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
			return mkfalse(), nil
		}
		o.proto = p
		return mktrue(), nil
	})
	rt.defMethod(ro, "defineProperty", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := needObj(arg(args, 0), "defineProperty"); e != nil {
			return mkundef(), e
		}
		name, e := rt.propKeyString(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return mkbool(rt.objectDefineProperty(arg(args, 0), name, arg(args, 2)) == nil), nil
	})
	rt.defMethod(ro, "getOwnPropertyDescriptor", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.getOwnPropertyDescriptor called on non-object")
		}
		name, e := rt.propKeyString(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		d := o.ownDescriptor(name)
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
		return mkbool(o.flags.extensible), nil
	})
	rt.defMethod(ro, "preventExtensions", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.preventExtensions called on non-object")
		}
		o.flags.extensible = false
		return mktrue(), nil
	})
	rt.defMethod(ro, "ownKeys", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil {
			return mkundef(), rt.typeError("Reflect.ownKeys called on non-object")
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
		for _, k := range o.ownKeys() {
			rt.arraySet(ra, ra.arrLen, rt.newString(k))
		}
		return res, nil
	})
	rt.defMethod(ro, "apply", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		if !rt.isCallable(fn) {
			return mkundef(), rt.typeError("Reflect.apply target is not a function")
		}
		var callArgs []Value
		if list := arg(args, 2); !list.IsNullish() {
			var e *ThrowError
			callArgs, e = rt.iterableValues(list)
			if e != nil {
				return mkundef(), e
			}
		}
		return rt.callValue(fn, arg(args, 1), callArgs)
	})
	rt.defMethod(ro, "construct", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		if !rt.isCallable(fn) {
			return mkundef(), rt.typeError("Reflect.construct target is not a constructor")
		}
		var callArgs []Value
		if list := arg(args, 1); !list.IsNullish() {
			var e *ThrowError
			callArgs, e = rt.iterableValues(list)
			if e != nil {
				return mkundef(), e
			}
		}
		return rt.construct(fn, callArgs)
	})

	rt.defGlobal("Reflect", reflect)
}
