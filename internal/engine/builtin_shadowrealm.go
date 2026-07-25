package engine

// ShadowRealm: a second, isolated global environment.
//
// goant Values are indices into per-Runtime pools, so a value from one Runtime
// means nothing in another. That would normally rule out a second realm — but
// ShadowRealm only ever lets primitives and callables cross the boundary
// (anything else is a TypeError), and both marshal cleanly: a primitive by
// value, a callable by wrapping it in a native function that marshals its
// arguments and result the same way. So the realm really is a separate Runtime.

// shadowRealm is the state behind a ShadowRealm instance.
type shadowRealm struct {
	rt *Runtime // the isolated realm
}

// marshalOut copies a value produced inside the shadow realm out to the caller.
// Primitives cross by value; a callable becomes a wrapped function; anything
// else is a TypeError, which is what keeps the two object graphs apart.
func (rt *Runtime) marshalOut(inner *Runtime, v Value) (Value, *ThrowError) {
	switch v.Type() {
	case TUndef, TNull, TBool, TNum:
		return v, nil // these carry their payload inline, not a pool handle
	case TStr:
		return rt.newStringBytes(inner.strBytes(v)), nil
	}
	if inner.isCallable(v) {
		return rt.wrapRealmFunction(inner, v), nil
	}
	if v.IsSymbol() {
		return mkundef(), rt.typeError("a symbol cannot cross a ShadowRealm boundary")
	}
	return mkundef(), rt.typeError("only primitives and callables may cross a ShadowRealm boundary")
}

// marshalIn copies a caller value into the shadow realm, by the same rules.
func (rt *Runtime) marshalIn(inner *Runtime, v Value) (Value, *ThrowError) {
	switch v.Type() {
	case TUndef, TNull, TBool, TNum:
		return v, nil
	case TStr:
		return inner.newStringBytes(rt.strBytes(v)), nil
	}
	if rt.isCallable(v) {
		return inner.wrapRealmFunction(rt, v), nil
	}
	return mkundef(), rt.typeError("only primitives and callables may cross a ShadowRealm boundary")
}

// wrapRealmFunction returns a function in rt that calls fn (which belongs to
// other), marshalling arguments in and the result back out.
func (rt *Runtime) wrapRealmFunction(other *Runtime, fn Value) Value {
	return rt.newNativeFunc("wrapped", 0, func(rt *Runtime, _ Value, args []Value) (Value, *ThrowError) {
		inner := make([]Value, 0, len(args))
		for _, a := range args {
			mv, e := rt.marshalIn(other, a)
			if e != nil {
				return mkundef(), e
			}
			inner = append(inner, mv)
		}
		res, e := other.callValue(fn, mkundef(), inner)
		if e != nil {
			// An error must not escape as the other realm's object: it is
			// re-thrown as a TypeError belonging to this realm.
			return mkundef(), rt.typeError("wrapped function threw inside its ShadowRealm")
		}
		return rt.marshalOut(other, res)
	})
}

func (rt *Runtime) initShadowRealmBuiltin() {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)

	ctor := rt.newNativeFunc("ShadowRealm", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if rt.objPtr(this) == nil {
			return mkundef(), rt.typeError("Constructor ShadowRealm requires 'new'")
		}
		o := rt.objPtr(this)
		if rt.shadowRealms == nil {
			rt.shadowRealms = map[*object]*shadowRealm{}
		}
		rt.shadowRealms[o] = &shadowRealm{rt: New()}
		return this, nil
	})

	realmOf := func(this Value) (*shadowRealm, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || rt.shadowRealms[o] == nil {
			return nil, rt.typeError("ShadowRealm.prototype method called on an incompatible receiver")
		}
		return rt.shadowRealms[o], nil
	}

	rt.defMethod(po, "evaluate", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sr, e := realmOf(this)
		if e != nil {
			return mkundef(), e
		}
		src := arg(args, 0)
		if src.Type() != TStr { // no coercion: the argument must already be a string
			return mkundef(), rt.typeError("ShadowRealm.prototype.evaluate expects a string")
		}
		v, err := sr.rt.RunString("<shadowrealm>", string(rt.strBytes(src)))
		if err != nil {
			// A SyntaxError inside the realm surfaces as this realm's SyntaxError;
			// any other abrupt completion becomes a TypeError, since the thrown
			// value itself may not cross.
			if _, isSyntax := err.(*SyntaxError); isSyntax {
				return mkundef(), rt.syntaxError(err.Error())
			}
			return mkundef(), rt.typeError("ShadowRealm evaluation threw: " + err.Error())
		}
		return rt.marshalOut(sr.rt, v)
	})

	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	if rt.symToStringTag != 0 {
		po.defineOwnSymbol(rt.symToStringTag.handle(), rt.internString("ShadowRealm"), attrConfigurable)
	}
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", proto, 0)
	rt.shadowRealmProto = proto
	rt.defGlobal("ShadowRealm", ctor)
}
