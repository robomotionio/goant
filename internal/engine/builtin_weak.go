package engine

// WeakMap / WeakSet (ant modules/collections.c weak variants). Keys must be
// objects (or unregistered symbols); the collection holds no strong-reference
// semantics observable to a program, so — pending the GC phase — they are backed
// by the same insertion-indexed collection as Map/Set, keyed by object identity.
// They expose no iteration, size, or clear.

func (rt *Runtime) initWeakCollections() {
	rt.initWeakMapBuiltin()
	rt.initWeakSetBuiltin()
}

// weakCollOf returns the receiver's weak collection, or a TypeError.
func (rt *Runtime) weakCollOf(this Value, wantSet bool, name string) (*collection, *ThrowError) {
	o := rt.objPtr(this)
	if o == nil || o.coll == nil || !o.coll.weak || o.coll.isSet != wantSet {
		return nil, rt.typeError("method called on incompatible receiver (not a " + name + ")")
	}
	return o.coll, nil
}

// validWeakKey reports whether v may key a weak collection (objects and symbols).
func validWeakKey(v Value) bool {
	return v.IsObjectType() || v.Type() == TSymbol
}

func (rt *Runtime) initWeakMapBuiltin() {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)

	rt.defMethod(po, "set", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.weakCollOf(this, false, "WeakMap")
		if e != nil {
			return mkundef(), e
		}
		k := arg(args, 0)
		if !validWeakKey(k) {
			return mkundef(), rt.typeError("Invalid value used as weak map key")
		}
		ck := rt.canonicalKey(k)
		if idx, ok := m.index[ck]; ok {
			m.vals[idx] = arg(args, 1)
		} else {
			m.index[ck] = len(m.keys)
			m.keys = append(m.keys, k)
			m.vals = append(m.vals, arg(args, 1))
		}
		return this, nil
	})
	rt.defMethod(po, "get", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.weakCollOf(this, false, "WeakMap")
		if e != nil {
			return mkundef(), e
		}
		if idx, ok := m.index[rt.canonicalKey(arg(args, 0))]; ok {
			return m.vals[idx], nil
		}
		return mkundef(), nil
	})
	rt.defMethod(po, "getOrInsert", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// WeakMap.prototype.getOrInsert(key, value) (upsert proposal).
		m, e := rt.weakCollOf(this, false, "WeakMap")
		if e != nil {
			return mkundef(), e
		}
		k := arg(args, 0)
		if !validWeakKey(k) {
			return mkundef(), rt.typeError("Invalid value used as weak map key")
		}
		ck := rt.canonicalKey(k)
		if idx, ok := m.index[ck]; ok {
			return m.vals[idx], nil
		}
		v := arg(args, 1)
		m.index[ck] = len(m.keys)
		m.keys = append(m.keys, k)
		m.vals = append(m.vals, v)
		return v, nil
	})
	rt.defMethod(po, "getOrInsertComputed", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// WeakMap.prototype.getOrInsertComputed(key, callbackfn): insert callbackfn(key)
		// when absent (upsert proposal).
		m, e := rt.weakCollOf(this, false, "WeakMap")
		if e != nil {
			return mkundef(), e
		}
		k := arg(args, 0)
		if !validWeakKey(k) {
			return mkundef(), rt.typeError("Invalid value used as weak map key")
		}
		cb := arg(args, 1)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("getOrInsertComputed callbackfn is not a function")
		}
		ck := rt.canonicalKey(k)
		if idx, ok := m.index[ck]; ok {
			return m.vals[idx], nil
		}
		v, e := rt.callValue(cb, mkundef(), []Value{k})
		if e != nil {
			return mkundef(), e
		}
		// Re-check after the callback (it may have inserted the key).
		if idx, ok := m.index[ck]; ok {
			m.vals[idx] = v
			return v, nil
		}
		m.index[ck] = len(m.keys)
		m.keys = append(m.keys, k)
		m.vals = append(m.vals, v)
		return v, nil
	})
	rt.defMethod(po, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.weakCollOf(this, false, "WeakMap")
		if e != nil {
			return mkundef(), e
		}
		_, ok := m.index[rt.canonicalKey(arg(args, 0))]
		return mkbool(ok), nil
	})
	rt.defMethod(po, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.weakCollOf(this, false, "WeakMap")
		if e != nil {
			return mkundef(), e
		}
		return mkbool(m.remove(rt.canonicalKey(arg(args, 0)))), nil
	})

	ctor := rt.newNativeFunc("WeakMap", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("Constructor WeakMap requires 'new'")
		}
		o.coll = &collection{index: map[string]int{}, weak: true}
		if it := arg(args, 0); !it.IsNullish() {
			setFn, _ := rt.getField(this, "set")
			if e := rt.iterateWithClose(it, func(entry Value) (bool, *ThrowError) {
				k, _ := rt.getElement(entry, mknum(0))
				v, _ := rt.getElement(entry, mknum(1))
				_, e := rt.callValue(setFn, this, []Value{k, v})
				return false, e
			}); e != nil {
				return mkundef(), e
			}
		}
		return this, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	if rt.symToStringTag != 0 {
		po.defineOwnSymbol(rt.symToStringTag.handle(), rt.newString("WeakMap"), attrConfigurable)
	}
	rt.defGlobal("WeakMap", ctor)
}

func (rt *Runtime) initWeakSetBuiltin() {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)

	rt.defMethod(po, "add", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.weakCollOf(this, true, "WeakSet")
		if e != nil {
			return mkundef(), e
		}
		k := arg(args, 0)
		if !validWeakKey(k) {
			return mkundef(), rt.typeError("Invalid value used in weak set")
		}
		ck := rt.canonicalKey(k)
		if _, ok := s.index[ck]; !ok {
			s.index[ck] = len(s.keys)
			s.keys = append(s.keys, k)
			s.vals = append(s.vals, mkundef())
		}
		return this, nil
	})
	rt.defMethod(po, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.weakCollOf(this, true, "WeakSet")
		if e != nil {
			return mkundef(), e
		}
		_, ok := s.index[rt.canonicalKey(arg(args, 0))]
		return mkbool(ok), nil
	})
	rt.defMethod(po, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.weakCollOf(this, true, "WeakSet")
		if e != nil {
			return mkundef(), e
		}
		return mkbool(s.remove(rt.canonicalKey(arg(args, 0)))), nil
	})

	ctor := rt.newNativeFunc("WeakSet", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("Constructor WeakSet requires 'new'")
		}
		o.coll = &collection{index: map[string]int{}, isSet: true, weak: true}
		if it := arg(args, 0); !it.IsNullish() {
			addFn, _ := rt.getField(this, "add")
			if e := rt.iterateWithClose(it, func(v Value) (bool, *ThrowError) {
				_, e := rt.callValue(addFn, this, []Value{v})
				return false, e
			}); e != nil {
				return mkundef(), e
			}
		}
		return this, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	if rt.symToStringTag != 0 {
		po.defineOwnSymbol(rt.symToStringTag.handle(), rt.newString("WeakSet"), attrConfigurable)
	}
	rt.defGlobal("WeakSet", ctor)
}
