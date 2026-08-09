package engine

// WeakMap / WeakSet (ant modules/collections.c weak variants). Keys must be
// objects (or unregistered symbols); the collection holds no strong-reference
// semantics observable to a program, so — pending the GC phase — they are backed
// by the same insertion-indexed collection as Map/Set, keyed by object identity.
// They expose no iteration, size, or clear.

func (rt *Runtime) initWeakCollections() {
	rt.initWeakMapBuiltin()
	rt.initWeakSetBuiltin()
	rt.initWeakRefBuiltin()
	rt.initFinalizationRegistryBuiltin()
}

// initWeakRefBuiltin installs WeakRef. Without a tracing GC the referent is held
// strongly (in o.boxed) and deref always returns it — sufficient for the program-
// observable contract short of collection timing.
func (rt *Runtime) initWeakRefBuiltin() {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)
	rt.defMethod(po, "deref", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		// Brand check: a WeakRef instance carries [[WeakRefTarget]] (always an object
		// or symbol, never undefined); an ordinary object (or a differently-boxed one
		// like a String wrapper) has the slot unset, which reads back as undefined.
		if o == nil || o.getSlot(slotWeakRefTarget).IsUndefined() {
			return mkundef(), rt.typeError("WeakRef.prototype.deref called on incompatible receiver")
		}
		return o.getSlot(slotWeakRefTarget), nil
	})
	ctor := rt.newNativeFunc("WeakRef", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || !rt.constructing() {
			return mkundef(), rt.typeError("Constructor WeakRef requires 'new'")
		}
		target := arg(args, 0)
		if !rt.canBeHeldWeakly(target) {
			return mkundef(), rt.typeError("WeakRef: target must be an object or symbol")
		}
		o.boxed = target
		o.setSlot(slotWeakRefTarget, target) // [[WeakRefTarget]] + brand for deref
		pr, e := rt.newTargetProtoE(proto)
		if e != nil {
			return mkundef(), e
		}
		o.proto = pr
		return this, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	if rt.symToStringTag != 0 {
		po.defineOwnSymbol(rt.symToStringTag.handle(), rt.internString("WeakRef"), attrConfigurable)
	}
	rt.defGlobal("WeakRef", ctor)
}

// finCell is one FinalizationRegistry [[Cells]] record. hasToken distinguishes
// an absent unregisterToken (spec ~empty~) from an undefined one.
type finCell struct {
	target   Value
	held     Value
	token    Value
	hasToken bool
}

// finRegistryOf returns the receiver's registry object if it is branded as a
// FinalizationRegistry (present in rt.finRegistries), else a TypeError.
func (rt *Runtime) finRegistryOf(this Value) (*object, *ThrowError) {
	o := rt.objPtr(this)
	if o != nil {
		if _, ok := rt.finRegistries[o]; ok {
			return o, nil
		}
	}
	return nil, rt.typeError("method called on incompatible receiver (not a FinalizationRegistry)")
}

// initFinalizationRegistryBuiltin installs FinalizationRegistry. Cleanup
// callbacks never fire (no tracing GC), but the register/unregister [[Cells]]
// bookkeeping and brand checks are program-observable and implemented to spec.
func (rt *Runtime) initFinalizationRegistryBuiltin() {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)
	rt.defMethod(po, "register", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.finRegistryOf(this)
		if e != nil {
			return mkundef(), e
		}
		target, held, token := arg(args, 0), arg(args, 1), arg(args, 2)
		if !rt.canBeHeldWeakly(target) {
			return mkundef(), rt.typeError("FinalizationRegistry.register: target cannot be held weakly")
		}
		if rt.sameValue(target, held) {
			return mkundef(), rt.typeError("FinalizationRegistry.register: heldValue must not be the target")
		}
		cell := finCell{target: target, held: held}
		if !token.IsUndefined() {
			if !rt.canBeHeldWeakly(token) {
				return mkundef(), rt.typeError("FinalizationRegistry.register: unregisterToken cannot be held weakly")
			}
			cell.token, cell.hasToken = token, true
		}
		rt.finRegistries[o] = append(rt.finRegistries[o], cell)
		return mkundef(), nil
	})
	rt.defMethod(po, "unregister", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.finRegistryOf(this)
		if e != nil {
			return mkundef(), e
		}
		token := arg(args, 0)
		if !rt.canBeHeldWeakly(token) {
			return mkundef(), rt.typeError("FinalizationRegistry.unregister: unregisterToken cannot be held weakly")
		}
		cells := rt.finRegistries[o]
		kept := cells[:0]
		removed := false
		for _, c := range cells {
			if c.hasToken && rt.sameValue(c.token, token) {
				removed = true
				continue
			}
			kept = append(kept, c)
		}
		rt.finRegistries[o] = kept
		return mkbool(removed), nil
	})
	ctor := rt.newNativeFunc("FinalizationRegistry", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || !rt.constructing() {
			return mkundef(), rt.typeError("Constructor FinalizationRegistry requires 'new'")
		}
		if !rt.isCallable(arg(args, 0)) {
			return mkundef(), rt.typeError("FinalizationRegistry: cleanup callback must be callable")
		}
		o.boxed = arg(args, 0)
		pr, e := rt.newTargetProtoE(proto)
		if e != nil {
			return mkundef(), e
		}
		o.proto = pr
		if rt.finRegistries == nil {
			rt.finRegistries = map[*object][]finCell{}
		}
		rt.finRegistries[o] = nil // brand: a registry with an empty [[Cells]]
		return this, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	if rt.symToStringTag != 0 {
		po.defineOwnSymbol(rt.symToStringTag.handle(), rt.internString("FinalizationRegistry"), attrConfigurable)
	}
	rt.defGlobal("FinalizationRegistry", ctor)
}

// weakCollOf returns the receiver's weak collection, or a TypeError.
func (rt *Runtime) weakCollOf(this Value, wantSet bool, name string) (*collection, *ThrowError) {
	o := rt.objPtr(this)
	if o == nil || o.coll() == nil || !o.coll().weak || o.coll().isSet != wantSet {
		return nil, rt.typeError("method called on incompatible receiver (not a " + name + ")")
	}
	return o.coll(), nil
}

// canBeHeldWeakly implements CanBeHeldWeakly: an Object, or a Symbol that is not
// registered in the global symbol registry (Symbol.for symbols cannot be held
// weakly, since they are never collected).
func (rt *Runtime) canBeHeldWeakly(v Value) bool {
	if v.IsObjectType() {
		return true
	}
	if v.Type() == TSymbol {
		for _, sym := range rt.symbolRegistry {
			if sym == v {
				return false
			}
		}
		return true
	}
	return false
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
		if !rt.canBeHeldWeakly(k) {
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
		if !rt.canBeHeldWeakly(k) {
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
		if !rt.canBeHeldWeakly(k) {
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
		o.extend().coll = &collection{index: map[string]int{}, weak: true}
		pr, e := rt.newTargetProtoE(proto)
		if e != nil {
			return mkundef(), e
		}
		o.proto = pr
		if it := arg(args, 0); !it.IsNullish() {
			setFn, e := rt.getField(this, "set")
			if e != nil {
				return mkundef(), e
			}
			if !rt.isCallable(setFn) {
				return mkundef(), rt.typeError("WeakMap: 'set' is not callable")
			}
			if e := rt.iterateWithClose(it, func(entry Value) (bool, *ThrowError) {
				if !entry.IsObjectType() {
					return false, rt.typeError("Iterator value " + rt.inspect(entry, false) + " is not an entry object")
				}
				k, e := rt.getElement(entry, mknum(0))
				if e != nil {
					return false, e
				}
				v, e := rt.getElement(entry, mknum(1))
				if e != nil {
					return false, e
				}
				_, e = rt.callValue(setFn, this, []Value{k, v})
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
		if !rt.canBeHeldWeakly(k) {
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
		o.extend().coll = &collection{index: map[string]int{}, isSet: true, weak: true}
		pr, e := rt.newTargetProtoE(proto)
		if e != nil {
			return mkundef(), e
		}
		o.proto = pr
		if it := arg(args, 0); !it.IsNullish() {
			addFn, e := rt.getField(this, "add")
			if e != nil {
				return mkundef(), e
			}
			if !rt.isCallable(addFn) {
				return mkundef(), rt.typeError("WeakSet: 'add' is not callable")
			}
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
