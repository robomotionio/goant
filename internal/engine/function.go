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
	obj.shape = rt.newShape()
	obj.typeTag = TFunc
	obj.flags.extensible = true
	obj.flags.isCallable = true

	ch, cl := rt.closures.alloc()
	cl.fn = fn
	cl.upvalues = upvals
	obj.closure = ch

	// .length and .name (non-enumerable, configurable) per spec.
	obj.defineOwn("length", mknum(float64(fn.fnLength)), attrConfigurable)
	obj.defineOwn("name", rt.internString(fn.name), attrConfigurable)

	fnVal := mkval(TFunc, uint64(oh))
	// Annex B legacy: an ORDINARY non-strict function declaration or expression
	// carries its own `caller` and `arguments`, which shadow the %ThrowTypeError%
	// accessors on Function.prototype. Every other function kind — strict,
	// generator, async, arrow, method/accessor, class constructor — has none and
	// keeps throwing through the prototype. goant tracks no caller chain, so the
	// value is undefined — which the tests read as "this host does not implement
	// the extension" and skip, whereas null reads as a real (non-callable) caller.
	if !fn.isStrict && !fn.isArrow && !fn.isAsync && !fn.isGenerator &&
		!fn.isMethod && !fn.isClassCtor {
		obj.defineOwn("caller", mkundef(), attrConfigurable)
		obj.defineOwn("arguments", mkundef(), attrConfigurable)
	}
	// Ordinary and generator functions carry a .prototype; arrow, async, and
	// concise methods / accessors do not (and methods/accessors have no
	// [[Construct]]).
	if !fn.isArrow && !fn.isAsync && !fn.isMethod {
		obj.flags.isConstructor = true
		protoParent := rt.objectProto
		if fn.isGenerator {
			protoParent = rt.genProto // generator instances' prototype chain
		}
		proto := rt.newObject(protoParent)
		if !fn.isGenerator {
			rt.objPtr(proto).defineOwn("constructor", fnVal, attrWritable|attrConfigurable)
		}
		// A class constructor's `prototype` is non-writable (and non-enumerable,
		// non-configurable); an ordinary function's is writable.
		var protoAttr uint8 = attrWritable
		if fn.isClassCtor {
			protoAttr = 0
		}
		obj.defineOwn("prototype", proto, protoAttr)
	} else if fn.isAsync && fn.isGenerator && rt.asyncGenProto != 0 {
		// An async generator function has its OWN .prototype (writable, not a
		// constructor) whose [[Prototype]] is %AsyncGeneratorPrototype% — mirroring a
		// sync generator, so `g.prototype`'s chain reaches %AsyncIteratorPrototype%.
		obj.defineOwn("prototype", rt.newObject(rt.asyncGenProto), attrWritable)
	} else if fn.isGenerator && fn.isMethod {
		// A sync generator method (concise `*m(){}`, no [[Construct]]) still has its
		// own writable, non-enumerable, non-configurable .prototype whose
		// [[Prototype]] is %GeneratorPrototype% — like a generator function, minus the
		// "constructor" back-reference.
		obj.defineOwn("prototype", rt.newObject(rt.genProto), attrWritable)
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

// nameMethodFromKey performs SetFunctionName on an anonymous method/accessor
// from an already-coerced property key (string or symbol): a symbol contributes
// "[description]", and an accessor takes a "get "/"set " prefix. The "name"
// property is non-writable, non-enumerable, configurable.
func (rt *Runtime) nameMethodFromKey(fn, k Value, prefix string) {
	o := rt.objPtr(fn)
	if o == nil || !o.flags.isCallable {
		return
	}
	var name string
	if k.IsSymbol() {
		if desc := rt.symbolDesc(k); !desc.IsUndefined() {
			name = "[" + rt.strGo(desc) + "]"
		}
	} else {
		name = rt.strGo(k)
	}
	o.defineOwn("name", rt.newString(prefix+name), attrConfigurable)
}

// setMethodHome records a method's [[HomeObject]] when the method reads super
// outside a class scope. It is a no-op for every other function, so it is safe
// to call for every object-literal method definition.
func (rt *Runtime) setMethodHome(fnVal, home Value) {
	if cl := rt.closureOf(fnVal); cl != nil && cl.fn.usesSuper {
		cl.home = home
	}
}

// newNativeFunc creates a callable built-in function object.
func (rt *Runtime) newNativeFunc(name string, length int, fn nativeFunc) Value {
	oh, obj := rt.objects.alloc()
	obj.proto = rt.functionProto
	obj.shape = rt.newShape()
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

// derivedCtorReturn applies a derived class constructor's [[Construct]]
// result rule to a return value r, given the current `this` binding: an Object
// is returned as-is; undefined resolves to GetThisBinding (a ReferenceError if
// super() has not run); any other value is a TypeError.
func (rt *Runtime) derivedCtorReturn(r, thisBinding Value) (Value, *ThrowError) {
	if r.IsObjectType() || r.Type() == TTypedArray {
		return r, nil
	}
	if !r.IsUndefined() {
		return mkundef(), rt.typeError("Derived constructors may only return object or undefined")
	}
	if thisBinding.IsEmpty() {
		return mkundef(), rt.referenceError("Must call super constructor in derived class before accessing 'this' or returning from derived constructor")
	}
	return thisBinding, nil
}

// boundArgsOf returns a fresh copy of a bound function's [[BoundArguments]]
// (stored as a JS array in slotBoundArgs).
func (rt *Runtime) boundArgsOf(o *object) []Value {
	ao := rt.objPtr(o.getSlot(slotBoundArgs))
	if ao == nil {
		return nil
	}
	out := make([]Value, ao.arrLen)
	for i := uint32(0); i < ao.arrLen; i++ {
		if int(i) < len(ao.arr) {
			out[i] = ao.arr[i]
		}
	}
	return out
}

// isConstructorValue reports whether v has a [[Construct]] internal method
// (IsConstructor). Ordinary function constructors, flagged native constructors,
// and proxies wrapping a constructor qualify; methods, arrows, generators,
// async functions, getters, and call-only natives do not.
func (rt *Runtime) isConstructorValue(v Value) bool {
	o := rt.objPtr(v)
	if o == nil || !o.flags.isCallable {
		return false
	}
	if o.proxy != nil {
		return rt.isConstructorValue(o.proxy.target)
	}
	if o.native != nil {
		return o.flags.isConstructor
	}
	if cl := rt.closures.get(o.closure); cl != nil {
		return !(cl.fn.isGenerator || cl.fn.isAsync || cl.fn.isArrow || cl.fn.isMethod)
	}
	return o.flags.isConstructor
}

// newTargetProto returns the [[Prototype]] to use for an object created by the
// current native constructor: new.target.prototype when constructing (so
// Reflect.construct(C, args, newTarget) / subclasses inherit correctly),
// otherwise the given intrinsic fallback.
func (rt *Runtime) newTargetProto(fallback Value) Value {
	if rt.pendingNewTarget.IsUndefined() {
		return fallback
	}
	// constructWithTarget already performed the single observable [[Get]] of
	// new.target.prototype and cached it; an object result is the instance's
	// prototype, anything else falls back to the intrinsic default.
	if rt.pendingNewTargetProto.IsObjectType() {
		return rt.pendingNewTargetProto
	}
	return fallback
}

// newTargetProtoE is GetPrototypeFromConstructor at the constructor's own
// OrdinaryCreateFromConstructor step: it surfaces the abrupt cached from the
// single ? Get(newTarget, "prototype") (a throwing getter is observed exactly
// once, in spec order after earlier argument checks), otherwise returns the
// resolved prototype or the intrinsic fallback.
func (rt *Runtime) newTargetProtoE(fallback Value) (Value, *ThrowError) {
	if rt.pendingNewTarget.IsUndefined() {
		return fallback, nil
	}
	if rt.pendingNewTargetProtoErr != nil {
		return mkundef(), rt.pendingNewTargetProtoErr
	}
	if rt.pendingNewTargetProto.IsObjectType() {
		return rt.pendingNewTargetProto, nil
	}
	return fallback, nil
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
	// A bound function forwards [[Construct]] to its [[BoundTargetFunction]] with
	// [[BoundArguments]] prepended; when new.target is the bound function itself
	// it is replaced by the target (BoundFunctionCreate's [[Construct]]).
	if o.native != nil && o.flags.isConstructor {
		if bt := o.getSlot(slotTargetFunc); bt.IsObjectType() {
			full := append(rt.boundArgsOf(o), args...)
			nt := newTarget
			if nt == 0 || nt == fnVal {
				nt = bt
			}
			return rt.constructWithTarget(bt, full, nt)
		}
	}
	if cl := rt.closures.get(o.closure); cl != nil && (cl.fn.isGenerator || cl.fn.isAsync || cl.fn.isArrow || cl.fn.isMethod) {
		nm := cl.fn.name
		if nm == "" {
			nm = "Function"
		}
		return mkundef(), rt.typeError(nm + " is not a constructor")
	}
	// A native function has [[Construct]] only if flagged a constructor; built-in
	// methods, getters, Symbol/BigInt, %TypedArray%, etc. are call-only.
	if o.native != nil && !o.flags.isConstructor {
		nm := "value"
		if nv, ok := o.getOwn("name"); ok && nv.IsString() {
			if s := rt.strGo(nv); s != "" {
				nm = s
			}
		}
		return mkundef(), rt.typeError(nm + " is not a constructor")
	}
	if newTarget == 0 {
		newTarget = fnVal
	}
	// new.target (Reflect.construct's third argument) must itself be a constructor.
	if !rt.isConstructorValue(newTarget) {
		return mkundef(), rt.typeError("new.target is not a constructor")
	}
	// GetPrototypeFromConstructor performs a single observable ? Get(newTarget,
	// "prototype"). We read it once here to pre-create `this`, caching both the
	// value and any abrupt; a native constructor surfaces the abrupt when it
	// calls newTargetProtoE at its own OrdinaryCreateFromConstructor step, so a
	// throwing getter is observed exactly once and in spec order (after any
	// earlier argument validation).
	p, perr := rt.getField(newTarget, "prototype")
	proto := mknull()
	if perr == nil && p.IsObjectType() {
		proto = p
	}
	// OrdinaryCreateFromConstructor: an ordinary [[Construct]] whose
	// newTarget.prototype is not an object (`F.prototype = 1`) gives the instance
	// %Object.prototype%, not a null prototype. (pendingNewTargetProto keeps the raw
	// result so a native constructor still falls back to its own intrinsic default.)
	thisProto := proto
	if !thisProto.IsObjectType() {
		// GetPrototypeFromConstructor falls back to the constructor's realm's
		// intrinsic — and GetFunctionRealm throws for a revoked Proxy, which is the
		// only way that step is observable in a single-realm engine.
		if rerr := rt.checkFunctionRealm(newTarget); rerr != nil {
			return mkundef(), rerr
		}
		thisProto = rt.objectProto
	}
	thisObj := rt.newObject(thisProto)
	rt.pendingNewTarget = newTarget
	savedNTProto, savedNTErr := rt.pendingNewTargetProto, rt.pendingNewTargetProtoErr
	rt.pendingNewTargetProto, rt.pendingNewTargetProtoErr = proto, perr
	ret, e := rt.callValue(fnVal, thisObj, args)
	rt.pendingNewTarget = mkundef()
	rt.pendingNewTargetProto, rt.pendingNewTargetProtoErr = savedNTProto, savedNTErr
	if e != nil {
		return mkundef(), e
	}
	if ret.IsObjectType() || ret.Type() == TTypedArray {
		return ret, nil
	}
	return thisObj, nil
}

// checkFunctionRealm performs the observable part of GetFunctionRealm(obj):
// walking bound functions and proxies to the function underneath, and throwing
// a TypeError when a Proxy on that path has been revoked.
func (rt *Runtime) checkFunctionRealm(v Value) *ThrowError {
	for i := 0; i < maxProtoChainDepth; i++ {
		o := rt.objPtr(v)
		if o == nil {
			return nil
		}
		if o.proxy != nil {
			if o.proxy.revoked {
				return rt.typeError("Cannot get the realm of a revoked Proxy")
			}
			v = o.proxy.target
			continue
		}
		if bt := o.getSlot(slotTargetFunc); bt.IsObjectLike() {
			v = bt
			continue
		}
		return nil
	}
	return nil
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
		return rt.newGenerator(cl.fn, cl, fnVal, thisVal, args)
	}
	if cl.fn.isAsync {
		return rt.runAsync(cl.fn, cl, fnVal, thisVal, args), nil
	}
	return rt.runFrame(cl.fn, cl, fnVal, thisVal, args)
}
