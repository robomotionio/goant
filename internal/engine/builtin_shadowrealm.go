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
	// The two realms belong to one agent and share the value pools, so a
	// primitive — including a symbol, whose IDENTITY the Symbol.for registry
	// makes observable across realms — crosses unchanged.
	switch v.Type() {
	case TUndef, TNull, TBool, TNum, TStr, TBigInt:
		return v, nil
	}
	if v.IsSymbol() {
		return v, nil
	}
	if inner.isCallable(v) {
		return rt.wrapRealmFunction(inner, v), nil
	}
	return mkundef(), rt.typeError("only primitives and callables may cross a ShadowRealm boundary")
}

// marshalIn copies a caller value into the shadow realm, by the same rules.
func (rt *Runtime) marshalIn(inner *Runtime, v Value) (Value, *ThrowError) {
	switch v.Type() {
	case TUndef, TNull, TBool, TNum, TStr, TBigInt:
		return v, nil
	}
	if v.IsSymbol() {
		return v, nil
	}
	if rt.isCallable(v) {
		return inner.wrapRealmFunction(rt, v), nil
	}
	return mkundef(), rt.typeError("only primitives and callables may cross a ShadowRealm boundary")
}

// wrapRealmFunction returns a function in rt that calls fn (which belongs to
// other), marshalling arguments in and the result back out.
func (rt *Runtime) wrapRealmFunction(other *Runtime, fn Value) Value {
	w := rt.newNativeFunc("", 0, func(rt *Runtime, _ Value, args []Value) (Value, *ThrowError) {
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
	// A wrapped function copies the target's `length` and `name` — the only two
	// of its properties that cross — as non-writable, configurable data.
	if wo := rt.objPtr(w); wo != nil {
		// CopyNameAndLength asks HasOwnProperty("length") first, which is what a
		// Proxy's getOwnPropertyDescriptor trap sees — a throwing one must fail the
		// wrap, even though a plain [[Get]] would have succeeded.
		length := float64(0)
		hasLen, le := other.hasOwnPropertyOf(fn, "length")
		if le == nil && hasLen {
			var lv Value
			if lv, le = other.getField(fn, "length"); le == nil && lv.Type() == TNum && lv.Number() > 0 {
				length = lv.Number()
			}
		}
		name := ""
		nv, ne := other.getField(fn, "name")
		if ne == nil && nv.Type() == TStr {
			name = string(other.strBytes(nv))
		}
		if le != nil || ne != nil {
			// Reading the target's own length/name threw inside its realm; the
			// wrapper cannot be created, and the error may not cross as-is.
			rt.wrapFailed = true
		}
		wo.defineOwn("length", mknum(length), attrConfigurable)
		wo.defineOwn("name", rt.newString(name), attrConfigurable)
	}
	return w
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
		// A ShadowRealm is a realm of the SAME agent: it shares the value pools,
		// the interned strings, the well-known symbols and the Symbol.for registry
		// (all per-agent in the spec) while getting its own global and intrinsics.
		rt.shadowRealms[o] = &shadowRealm{rt: rt.NewRealm()}
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
		// PerformShadowRealmEval runs the source as an INDIRECT EVAL in the other
		// realm, not as a Script: its top-level let/const belong to that eval, so
		// two evaluations may declare the same name, and its `var`s are
		// configurable global bindings.
		srcText := string(rt.strBytes(src))
		// Only a SyntaxError from PARSING this source crosses as a SyntaxError. Once
		// the code is running, every abrupt completion — including a SyntaxError a
		// nested eval raises — becomes a TypeError here, since the thrown value
		// itself may not cross realms.
		if _, perr := parseMode("<shadowrealm>", srcText, false, false); perr != nil {
			return mkundef(), rt.syntaxError(perr.Error())
		}
		v, terr := sr.rt.evalInGlobalScope(srcText, false)
		sr.rt.runEventLoop()
		if terr != nil {
			name, msg := "", ""
			if nv, e2 := sr.rt.getField(terr.Value, "name"); e2 == nil && nv.IsString() {
				name = string(sr.rt.strBytes(nv))
			}
			if mv, e2 := sr.rt.getField(terr.Value, "message"); e2 == nil && mv.IsString() {
				msg = string(sr.rt.strBytes(mv))
			}
			return mkundef(), rt.typeError("ShadowRealm evaluation threw: " + name + ": " + msg)
		}
		out, me := rt.marshalOut(sr.rt, v)
		if me != nil {
			return mkundef(), me
		}
		if rt.wrapFailed {
			rt.wrapFailed = false
			return mkundef(), rt.typeError("could not wrap the value returned from the ShadowRealm")
		}
		return out, nil
	})

	rt.defMethod(po, "importValue", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sr, e := realmOf(this)
		if e != nil {
			return mkundef(), e
		}
		// The specifier is coerced (a throwing toString propagates synchronously,
		// before the promise is created).
		spec, e2 := rt.toStringValue(arg(args, 0))
		if e2 != nil {
			return mkundef(), e2
		}
		// The export name, unlike the specifier, is NOT coerced: it must already
		// be a string.
		name := arg(args, 1)
		if name.Type() != TStr {
			return mkundef(), rt.typeError("ShadowRealm.prototype.importValue expects a string export name")
		}
		// The module is loaded INSIDE the realm, then the one requested export is
		// marshalled out. The result is a promise in this realm either way.
		m, le := sr.rt.loadModule(string(rt.strBytes(spec)), "")
		if le != nil {
			return rt.rejectedPromise(rt.typeError("ShadowRealm.prototype.importValue could not load the module").Value), nil
		}
		inner, ok := m.exportValue(string(rt.strBytes(name)))
		if !ok {
			return rt.rejectedPromise(rt.typeError("the module has no export named '" + string(rt.strBytes(name)) + "'").Value), nil
		}
		out, me := rt.marshalOut(sr.rt, inner)
		if me != nil {
			return rt.rejectedPromise(me.Value), nil
		}
		return rt.resolvedPromise(out), nil
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
