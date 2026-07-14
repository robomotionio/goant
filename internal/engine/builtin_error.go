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
		// Get(O,"name")/Get(O,"message") and their ToString conversions are all
		// observable and must propagate abrupt completions (a throwing accessor or
		// a Symbol value must surface, not be swallowed).
		nv, e := rt.getField(this, "name")
		if e != nil {
			return mkundef(), e
		}
		name := "Error"
		if !nv.IsUndefined() {
			s, e := rt.toStringValue(nv)
			if e != nil {
				return mkundef(), e
			}
			name = string(rt.strBytes(s))
		}
		mv, e := rt.getField(this, "message")
		if e != nil {
			return mkundef(), e
		}
		msg := ""
		if !mv.IsUndefined() {
			s, e := rt.toStringValue(mv)
			if e != nil {
				return mkundef(), e
			}
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

	// Error.prototype.stack (Error Stacks proposal): an accessor property on
	// %Error.prototype% with { [[Enumerable]]: false, [[Configurable]]: true }.
	// The getter returns an implementation-defined stack string for objects
	// carrying an [[ErrorData]] slot (undefined otherwise, without consulting
	// proxies); the setter installs/updates an own "stack" data property on the
	// receiver via SetterThatIgnoresPrototypeProperties, so the inherited
	// accessor is bypassed rather than recursed into.
	getStack := rt.newNativeFunc("get stack", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectLike() {
			return mkundef(), rt.typeError("get Error.prototype.stack called on a non-object")
		}
		o := rt.objPtr(this)
		if o == nil || o.brandID() != brandError {
			return mkundef(), nil // no [[ErrorData]] internal slot
		}
		return rt.newString(rt.errorStackString(this)), nil
	})
	setStack := rt.newNativeFunc("set stack", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectLike() {
			return mkundef(), rt.typeError("set Error.prototype.stack called on a non-object")
		}
		v := arg(args, 0)
		if !v.IsString() {
			return mkundef(), rt.typeError("Error.prototype.stack setter requires a String value")
		}
		// SetterThatIgnoresPrototypeProperties(this, %Error.prototype%, "stack", v).
		// Assigning through the home object itself throws — this emulates
		// assignment to a non-writable data property on the prototype.
		if this == errProto {
			return mkundef(), rt.typeError("Cannot assign to read-only 'stack' of Error.prototype")
		}
		key := rt.internString("stack")
		// desc := this.[[GetOwnProperty]]("stack") (fires the proxy trap).
		exists := false
		o := rt.objPtr(this)
		if o.proxy != nil {
			d, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, key)
			if e != nil {
				return mkundef(), e
			}
			exists = !d.IsUndefined()
		} else {
			_, exists = rt.targetOwnDesc(this, key)
		}
		if !exists {
			// CreateDataPropertyOrThrow(this, "stack", v).
			if e := rt.createDataProperty(this, key, v); e != nil {
				return mkundef(), e
			}
			return mkundef(), nil
		}
		// Set(this, "stack", v, true): update the existing own property (data or
		// accessor), respecting writability and throwing on rejection.
		ok, e := rt.ordinarySet(this, key, v, this)
		if e != nil {
			return mkundef(), e
		}
		if !ok {
			return mkundef(), rt.typeError("Cannot assign to read-only property 'stack'")
		}
		return mkundef(), nil
	})
	ep.defineAccessor("stack", getStack, setStack, true, true, attrConfigurable)

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
		if opts := arg(args, 2); opts.IsObjectType() {
			hc, e := rt.hasPropE(opts, "cause") // HasProperty is observable (proxy trap may throw)
			if e != nil {
				return mkundef(), e
			}
			if hc {
				cause, e := rt.getField(opts, "cause")
				if e != nil {
					return mkundef(), e
				}
				eo.defineOwn("cause", cause, attrWritable|attrConfigurable)
			}
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
func (rt *Runtime) makeErrorCtor(name string, proto, parentCtor Value) Value {
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
		if opts := arg(args, 1); opts.IsObjectType() {
			hc, e := rt.hasPropE(opts, "cause") // HasProperty is observable (proxy trap may throw)
			if e != nil {
				return mkundef(), e
			}
			if hc {
				cause, e := rt.getField(opts, "cause")
				if e != nil {
					return mkundef(), e
				}
				rt.objPtr(errObj).defineOwn("cause", cause, attrWritable|attrConfigurable)
			}
		}
		return errObj, nil
	})
	co := rt.objPtr(ctor)
	co.defineOwn("prototype", proto, 0)
	// A NativeError constructor's [[Prototype]] is the %Error% constructor
	// (Object.getPrototypeOf(TypeError) === Error). The base Error constructor
	// (parentCtor is null) keeps the default %Function.prototype%.
	if parentCtor.IsObjectType() {
		co.proto = parentCtor
	}
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

// errorStackString synthesizes the implementation-defined string returned by
// the Error.prototype.stack getter: the error's "name: message" header (as
// Error.prototype.toString would render it) followed by a single synthetic
// frame. The exact contents are unobservable to the conformance suite, which
// only requires the result to be a String.
func (rt *Runtime) errorStackString(err Value) string {
	name := "Error"
	if nv, e := rt.getField(err, "name"); e == nil && !nv.IsUndefined() {
		if sv, e2 := rt.toStringValue(nv); e2 == nil {
			name = string(rt.strBytes(sv))
		}
	}
	msg := ""
	if mv, e := rt.getField(err, "message"); e == nil && !mv.IsUndefined() {
		if sv, e2 := rt.toStringValue(mv); e2 == nil {
			msg = string(rt.strBytes(sv))
		}
	}
	head := name
	if msg != "" {
		if name == "" {
			head = msg
		} else {
			head = name + ": " + msg
		}
	}
	return head + "\n    at <anonymous>"
}

// brandError marks objects with an [[ErrorData]] internal slot (error instances,
// not the NativeError prototypes).
const brandError = 1002
