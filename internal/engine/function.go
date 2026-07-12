package engine

// Function objects & the call machinery (ant src/ant.c function sections +
// engine.c call handling). A JS function is an object (T_FUNC) whose closure
// field references a [closure] (compiled func + captured upvalues).

// newFunction creates a callable function object from a compiled function and
// its captured upvalues.
func (rt *Runtime) newFunction(fn *svFunc, upvals []*upvalue) Value {
	oh, obj := rt.objects.alloc()
	// Function-kind prototype chain (async/generator functions have their own).
	obj.proto = rt.functionProto
	switch {
	case fn.isAsync && fn.isGenerator && rt.asyncGeneratorFnProto != 0:
		obj.proto = rt.asyncGeneratorFnProto
	case fn.isAsync && rt.asyncFunctionProto != 0:
		obj.proto = rt.asyncFunctionProto
	case fn.isGenerator && rt.generatorFuncProto != 0:
		obj.proto = rt.generatorFuncProto
	}
	obj.shape = newShape()
	obj.typeTag = TFunc
	obj.flags.extensible = true
	obj.flags.isCallable = true

	ch, cl := rt.closures.alloc()
	cl.fn = fn
	cl.upvalues = upvals
	obj.closure = ch

	// .length and .name (non-enumerable, configurable) per spec.
	obj.defineOwn("length", mknum(float64(fn.paramCount)), attrConfigurable)
	obj.defineOwn("name", rt.internString(fn.name), attrConfigurable)

	fnVal := mkval(TFunc, uint64(oh))
	// Ordinary and generator functions carry a .prototype; arrow and async
	// functions do not.
	if !fn.isArrow && !fn.isAsync {
		obj.flags.isConstructor = true
		protoParent := rt.objectProto
		if fn.isGenerator {
			protoParent = rt.genProto // generator instances' prototype chain
		}
		proto := rt.newObject(protoParent)
		if !fn.isGenerator {
			rt.objPtr(proto).defineOwn("constructor", fnVal, attrWritable|attrConfigurable)
		}
		obj.defineOwn("prototype", proto, attrWritable)
	}
	return fnVal
}

// closureOf resolves a function Value to its backing closure.
func (rt *Runtime) closureOf(v Value) *closure {
	o := rt.objPtr(v)
	if o == nil || o.typeTag != TFunc {
		return nil
	}
	return rt.closures.get(o.closure)
}

// newNativeFunc creates a callable built-in function object.
func (rt *Runtime) newNativeFunc(name string, length int, fn nativeFunc) Value {
	oh, obj := rt.objects.alloc()
	obj.proto = rt.functionProto
	obj.shape = newShape()
	obj.typeTag = TFunc
	obj.flags.extensible = true
	obj.flags.isCallable = true
	obj.native = fn
	obj.defineOwn("length", mknum(float64(length)), attrConfigurable)
	obj.defineOwn("name", rt.internString(name), attrConfigurable)
	return mkval(TFunc, uint64(oh))
}

// construct implements the `new` operator: allocate an object whose prototype
// is the constructor's .prototype, run the constructor with it as `this`, and
// return the constructor's object result (if any) or the new object.
func (rt *Runtime) construct(fnVal Value, args []Value) (Value, *ThrowError) {
	return rt.constructWithTarget(fnVal, args, fnVal)
}

// newTargetProto returns the [[Prototype]] to use for an object created by the
// current native constructor: new.target.prototype when constructing (so
// Reflect.construct(C, args, newTarget) / subclasses inherit correctly),
// otherwise the given intrinsic fallback.
func (rt *Runtime) newTargetProto(fallback Value) Value {
	nt := rt.pendingNewTarget
	if nt.IsUndefined() {
		return fallback
	}
	// Reuse the prototype constructWithTarget already resolved for this
	// construction (avoids a second observable [[Get]] of "prototype").
	if rt.pendingNewTargetProto != 0 && rt.pendingNewTargetProto.IsObjectType() {
		return rt.pendingNewTargetProto
	}
	if p, e := rt.getField(nt, "prototype"); e == nil && p.IsObjectType() {
		return p
	}
	return fallback
}

// constructing reports whether the current native builtin was invoked via
// [[Construct]] (new.target is set). Native constructors read this first thing
// to enforce "requires 'new'", since a method call like global.ArrayBuffer(n)
// still passes an object `this`.
func (rt *Runtime) constructing() bool {
	return !rt.pendingNewTarget.IsUndefined()
}

// constructWithTarget implements [[Construct]] with an explicit new.target: the
// new object's prototype comes from newTarget.prototype, and new.target inside
// the constructor is newTarget (Reflect.construct's third argument).
func (rt *Runtime) constructWithTarget(fnVal Value, args []Value, newTarget Value) (Value, *ThrowError) {
	o := rt.objPtr(fnVal)
	if o == nil || !o.flags.isCallable {
		return mkundef(), rt.typeError("value is not a constructor")
	}
	if o.proxy != nil {
		nt := newTarget
		if nt == 0 {
			nt = fnVal
		}
		return rt.proxyConstruct(o.proxy, args, nt)
	}
	if cl := rt.closures.get(o.closure); cl != nil && (cl.fn.isGenerator || cl.fn.isAsync || cl.fn.isArrow) {
		nm := cl.fn.name
		if nm == "" {
			nm = "Function"
		}
		return mkundef(), rt.typeError(nm + " is not a constructor")
	}
	if newTarget == 0 {
		newTarget = fnVal
	}
	proto := mknull()
	if p, e := rt.getField(newTarget, "prototype"); e == nil && p.IsObjectType() {
		proto = p
	}
	thisObj := rt.newObject(proto)
	rt.pendingNewTarget = newTarget
	// Cache the resolved prototype so a native ctor's newTargetProto reuses it
	// rather than re-reading newTarget.prototype (a second observable [[Get]]).
	savedNTProto := rt.pendingNewTargetProto
	rt.pendingNewTargetProto = proto
	ret, e := rt.callValue(fnVal, thisObj, args)
	rt.pendingNewTarget = mkundef()
	rt.pendingNewTargetProto = savedNTProto
	if e != nil {
		return mkundef(), e
	}
	if ret.IsObjectType() || ret.Type() == TTypedArray {
		return ret, nil
	}
	return thisObj, nil
}

// isCallable reports whether v can be called.
func (rt *Runtime) isCallable(v Value) bool {
	t := v.Type()
	if t == TFunc || t == TCFunc {
		return true
	}
	if o := rt.objPtr(v); o != nil {
		return o.flags.isCallable
	}
	return false
}

// callValue invokes a callable with the given this-binding and arguments
// (ant sv_call_native / the interpreter call path).
func (rt *Runtime) callValue(fnVal, thisVal Value, args []Value) (Value, *ThrowError) {
	o := rt.objPtr(fnVal)
	if o == nil || !o.flags.isCallable {
		return mkundef(), rt.typeError("value is not a function")
	}
	if o.proxy != nil {
		return rt.proxyApply(o.proxy, thisVal, args)
	}
	if o.native != nil {
		rt.frameDepth++
		if rt.frameDepth > maxFrameDepth {
			rt.frameDepth--
			return mkundef(), rt.rangeError("Maximum call stack size exceeded")
		}
		v, e := o.native(rt, thisVal, args)
		rt.frameDepth--
		return v, e
	}
	cl := rt.closures.get(o.closure)
	if cl == nil {
		return mkundef(), rt.typeError("value is not a function")
	}
	if cl.fn.isGenerator {
		return rt.newGenerator(cl.fn, cl, fnVal, thisVal, args), nil
	}
	if cl.fn.isAsync {
		return rt.runAsync(cl.fn, cl, fnVal, thisVal, args), nil
	}
	return rt.runFrame(cl.fn, cl, fnVal, thisVal, args)
}
