package engine

// Error hierarchy: Error + the standard NativeErrors (TypeError, RangeError,
// SyntaxError, ReferenceError, EvalError, URIError) (ant errors.c / builtin).
// Each NativeError.prototype chains to Error.prototype.

// errorCtors caches the runtime's NativeError constructors for internal throws.
type errorCtors struct {
	base        Value
	typeErr     Value
	rangeErr    Value
	syntaxErr   Value
	refErr      Value
	evalErr     Value
	uriErr      Value
	typeProto   Value
	rangeProto  Value
	syntaxProto     Value
	aggProto        Value
	suppressedProto Value
}

func (rt *Runtime) initErrorBuiltin() {
	// Error.prototype and constructor.
	errProto := rt.errorProto
	ep := rt.objPtr(errProto)
	ep.defineOwn("name", rt.internString("Error"), attrWritable|attrConfigurable)
	ep.defineOwn("message", rt.internString(""), attrWritable|attrConfigurable)
	rt.defMethod(ep, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("Error.prototype.toString called on non-object")
		}
		name := "Error"
		if nv, e := rt.getField(this, "name"); e == nil && !nv.IsUndefined() {
			s, _ := rt.toStringValue(nv)
			name = string(rt.strBytes(s))
		}
		msg := ""
		if mv, e := rt.getField(this, "message"); e == nil && !mv.IsUndefined() {
			s, _ := rt.toStringValue(mv)
			msg = string(rt.strBytes(s))
		}
		if msg == "" {
			return rt.newString(name), nil
		}
		if name == "" {
			return rt.newString(msg), nil
		}
		return rt.newString(name + ": " + msg), nil
	})

	base := rt.makeErrorCtor("Error", errProto, mknull())
	rt.errors.base = base

	// NativeError subclasses.
	mk := func(name string) (Value, Value) {
		proto := rt.newObject(errProto)
		po := rt.objPtr(proto)
		po.defineOwn("name", rt.internString(name), attrWritable|attrConfigurable)
		po.defineOwn("message", rt.internString(""), attrWritable|attrConfigurable)
		ctor := rt.makeErrorCtor(name, proto, base)
		return ctor, proto
	}
	_ = base
	rt.errors.typeErr, rt.errors.typeProto = mk("TypeError")
	rt.errors.rangeErr, rt.errors.rangeProto = mk("RangeError")
	rt.errors.syntaxErr, rt.errors.syntaxProto = mk("SyntaxError")
	rt.errors.refErr, _ = mk("ReferenceError")
	rt.errors.evalErr, _ = mk("EvalError")
	rt.errors.uriErr, _ = mk("URIError")

	// AggregateError(errors, message, options): errors is an iterable; message
	// is the second argument (not the first).
	aggProto := rt.newObject(errProto)
	apo := rt.objPtr(aggProto)
	apo.defineOwn("name", rt.internString("AggregateError"), attrWritable|attrConfigurable)
	apo.defineOwn("message", rt.internString(""), attrWritable|attrConfigurable)
	aggCtor := rt.newNativeFunc("AggregateError", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		errObj := this
		if !this.IsObjectType() {
			errObj = rt.newObject(aggProto)
		}
		eo := rt.objPtr(errObj)
		eo.setSlot(slotBrand, mknum(brandError)) // [[ErrorData]]
		errsArr := rt.newArray()
		if it := arg(args, 0); !it.IsNullish() {
			vals, e := rt.iterableValues(it)
			if e != nil {
				return mkundef(), e
			}
			ea := rt.objPtr(errsArr)
			for _, v := range vals {
				rt.arraySet(ea, ea.arrLen, v)
			}
		}
		eo.defineOwn("errors", errsArr, attrWritable|attrConfigurable)
		if msg := arg(args, 1); !msg.IsUndefined() {
			s, e := rt.toStringValue(msg)
			if e != nil {
				return mkundef(), e
			}
			eo.defineOwn("message", s, attrWritable|attrConfigurable)
		}
		if opts := arg(args, 2); opts.IsObjectType() && rt.hasProp(opts, "cause") {
			cause, e := rt.getField(opts, "cause")
			if e != nil {
				return mkundef(), e
			}
			eo.defineOwn("cause", cause, attrWritable|attrConfigurable)
		}
		return errObj, nil
	})
	rt.objPtr(aggCtor).defineOwn("prototype", aggProto, 0)
	apo.defineOwn("constructor", aggCtor, attrWritable|attrConfigurable)
	rt.errors.aggProto = aggProto
	rt.defGlobal("AggregateError", aggCtor)

	// SuppressedError(error, suppressed, message): carries the disposal error and
	// the value it suppressed (Explicit Resource Management).
	supProto := rt.newObject(errProto)
	spo := rt.objPtr(supProto)
	spo.defineOwn("name", rt.internString("SuppressedError"), attrWritable|attrConfigurable)
	spo.defineOwn("message", rt.internString(""), attrWritable|attrConfigurable)
	supCtor := rt.newNativeFunc("SuppressedError", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		errObj := this
		if !this.IsObjectType() {
			errObj = rt.newObject(rt.newTargetProto(supProto))
		} else if p := rt.newTargetProto(supProto); p != supProto {
			rt.objPtr(errObj).proto = p
		}
		eo := rt.objPtr(errObj)
		eo.setSlot(slotBrand, mknum(brandError)) // [[ErrorData]]
		eo.defineOwn("error", arg(args, 0), attrWritable|attrConfigurable)
		eo.defineOwn("suppressed", arg(args, 1), attrWritable|attrConfigurable)
		if msg := arg(args, 2); !msg.IsUndefined() {
			s, e := rt.toStringValue(msg)
			if e != nil {
				return mkundef(), e
			}
			eo.defineOwn("message", s, attrWritable|attrConfigurable)
		}
		return errObj, nil
	})
	rt.objPtr(supCtor).defineOwn("prototype", supProto, 0)
	spo.defineOwn("constructor", supCtor, attrWritable|attrConfigurable)
	rt.errors.suppressedProto = supProto
	rt.defGlobal("SuppressedError", supCtor)

	// Error.isError(arg): whether arg has an [[ErrorData]] internal slot (ES2025).
	// A Proxy is unwrapped to its target; a revoked Proxy throws a TypeError.
	rt.defMethod(rt.objPtr(base), "isError", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		for {
			o := rt.objPtr(v)
			if o == nil {
				return mkfalse(), nil
			}
			if o.proxy != nil {
				if o.proxy.revoked {
					return mkundef(), rt.typeError("Cannot perform 'isError' on a proxy that has been revoked")
				}
				v = o.proxy.target
				continue
			}
			return mkbool(o.brandID() == brandError), nil
		}
	})

	rt.defGlobal("Error", base)
	rt.defGlobal("TypeError", rt.errors.typeErr)
	rt.defGlobal("RangeError", rt.errors.rangeErr)
	rt.defGlobal("SyntaxError", rt.errors.syntaxErr)
	rt.defGlobal("ReferenceError", rt.errors.refErr)
	rt.defGlobal("EvalError", rt.errors.evalErr)
	rt.defGlobal("URIError", rt.errors.uriErr)
}

// makeErrorCtor builds an error constructor wired to the given prototype.
func (rt *Runtime) makeErrorCtor(name string, proto, _parentCtor Value) Value {
	ctor := rt.newNativeFunc(name, 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Works as both `Error(msg)` and `new Error(msg)`: this is the new
		// object under construct(); otherwise allocate one.
		var errObj Value
		if this.IsObjectType() {
			errObj = this
		} else {
			errObj = rt.newObject(proto)
		}
		rt.objPtr(errObj).setSlot(slotBrand, mknum(brandError)) // [[ErrorData]]
		if len(args) > 0 && !args[0].IsUndefined() {
			s, e := rt.toStringValue(args[0])
			if e != nil {
				return mkundef(), e
			}
			rt.objPtr(errObj).defineOwn("message", s, attrWritable|attrConfigurable)
		}
		// ES2022 error cause: an options object with an own "cause" property.
		if opts := arg(args, 1); opts.IsObjectType() && rt.hasProp(opts, "cause") {
			cause, e := rt.getField(opts, "cause")
			if e != nil {
				return mkundef(), e
			}
			rt.objPtr(errObj).defineOwn("cause", cause, attrWritable|attrConfigurable)
		}
		return errObj, nil
	})
	co := rt.objPtr(ctor)
	co.defineOwn("prototype", proto, 0)
	rt.objPtr(proto).defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	return ctor
}

// makeError constructs a NativeError object with the given message (used by the
// interpreter's internal throws).
func (rt *Runtime) makeError(proto Value, name, msg string) Value {
	if proto == 0 || !proto.IsObjectType() {
		// Errors not yet initialized (early bootstrap): fall back to a string.
		return rt.newString(name + ": " + msg)
	}
	e := rt.newObject(proto)
	rt.objPtr(e).setSlot(slotBrand, mknum(brandError)) // [[ErrorData]]
	rt.objPtr(e).defineOwn("message", rt.internString(msg), attrWritable|attrConfigurable)
	return e
}

// brandError marks objects with an [[ErrorData]] internal slot (error instances,
// not the NativeError prototypes).
const brandError = 1002
