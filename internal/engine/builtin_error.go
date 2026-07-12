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
	syntaxProto Value
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
		if len(args) > 0 && !args[0].IsUndefined() {
			s, e := rt.toStringValue(args[0])
			if e != nil {
				return mkundef(), e
			}
			rt.objPtr(errObj).defineOwn("message", s, attrWritable|attrConfigurable)
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
	rt.objPtr(e).defineOwn("message", rt.internString(msg), attrWritable|attrConfigurable)
	return e
}
