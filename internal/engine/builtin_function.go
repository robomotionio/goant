package engine

import "strings"

// Function constructor + Function.prototype (ant builtin_function): call, apply,
// bind, toString. Function.prototype is itself callable (returns undefined).

func (rt *Runtime) initFunctionBuiltin() {
	proto := rt.objPtr(rt.functionProto)
	// Function.prototype is a callable no-op (ant behavior).
	proto.flags.isCallable = true
	proto.native = func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkundef(), nil
	}

	// Poison-pill accessor: accessing strict caller/callee/arguments throws.
	rt.poison = rt.newNativeFunc("", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkundef(), rt.typeError("'caller', 'callee', and 'arguments' properties may not be accessed on strict mode functions")
	})
	proto.defineAccessor("caller", rt.poison, rt.poison, true, true, 0)
	proto.defineAccessor("arguments", rt.poison, rt.poison, true, true, 0)

	rt.defMethod(proto, "call", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !rt.isCallable(this) {
			return mkundef(), rt.typeError("Function.prototype.call called on non-callable")
		}
		thisArg := arg(args, 0)
		var callArgs []Value
		if len(args) > 1 {
			callArgs = args[1:]
		}
		return rt.callValue(this, thisArg, callArgs)
	})

	rt.defMethod(proto, "apply", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !rt.isCallable(this) {
			return mkundef(), rt.typeError("Function.prototype.apply called on non-callable")
		}
		thisArg := arg(args, 0)
		var callArgs []Value
		if a := arg(args, 1); !a.IsNullish() {
			n, e := rt.lengthOf(a)
			if e != nil {
				return mkundef(), e
			}
			callArgs = make([]Value, n)
			for i := 0; i < n; i++ {
				callArgs[i], _ = rt.getElement(a, mknum(float64(i)))
			}
		}
		return rt.callValue(this, thisArg, callArgs)
	})

	rt.defMethod(proto, "bind", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !rt.isCallable(this) {
			return mkundef(), rt.typeError("Function.prototype.bind called on non-callable")
		}
		target := this
		boundThis := arg(args, 0)
		var boundArgs []Value
		if len(args) > 1 {
			boundArgs = append(boundArgs, args[1:]...)
		}
		bound := rt.newNativeFunc("", 0, func(rt *Runtime, _ Value, callArgs []Value) (Value, *ThrowError) {
			full := append(append([]Value{}, boundArgs...), callArgs...)
			return rt.callValue(target, boundThis, full)
		})
		// A bound function's [[Prototype]] is the target's [[Prototype]] (19.2.3.2),
		// and it has [[Construct]] iff the target does.
		if to := rt.objPtr(target); to != nil {
			rt.objPtr(bound).proto = to.proto
		}
		rt.objPtr(bound).flags.isConstructor = rt.isConstructorValue(target)
		// Spec order: SetFunctionLength first (HasOwnProperty then Get "length"),
		// then SetFunctionName (Get "name"). Both Gets are observable via a trap.
		tlen := 0
		if has, e := rt.hasOwnPropertyOf(target, "length"); e != nil {
			return mkundef(), e
		} else if has {
			if lv, e := rt.getField(target, "length"); e == nil && lv.Type() == TNum {
				tlen = int(lv.Number())
			}
		}
		blen := tlen - len(boundArgs)
		if blen < 0 {
			blen = 0
		}
		rt.objPtr(bound).defineOwn("length", mknum(float64(blen)), attrConfigurable)
		targetName := ""
		if nv, e := rt.getField(target, "name"); e != nil {
			return mkundef(), e
		} else if nv.IsString() {
			targetName = string(rt.strBytes(nv))
		}
		rt.objPtr(bound).defineOwn("name", rt.internString("bound "+targetName), attrConfigurable)
		return bound, nil
	})

	rt.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || !o.flags.isCallable {
			return mkundef(), rt.typeError("Function.prototype.toString called on non-function")
		}
		name := ""
		if nv, ok := o.getOwn("name"); ok && nv.IsString() {
			name = string(rt.strBytes(nv))
		}
		// Native functions render as "function name() { [native code] }".
		if o.native != nil || o.closure == 0 {
			return rt.newString("function " + name + "() { [native code] }"), nil
		}
		// User functions retain their source slice (ant Function.prototype.toString).
		if cl := rt.closures.get(o.closure); cl != nil && cl.fn.source != "" &&
			cl.fn.srcEnd > cl.fn.srcStart && cl.fn.srcEnd <= len(cl.fn.source) {
			return rt.newString(cl.fn.source[cl.fn.srcStart:cl.fn.srcEnd]), nil
		}
		return rt.newString("function " + name + "() { }"), nil
	})

	// Function constructor: compile `function anonymous(params) { body }` in the
	// global scope and return the resulting function (ant dynamic Function).
	ctor := rt.newNativeFunc("Function", 1, rt.dynamicFunctionCtor("function", rt.functionProto))
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.functionProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defGlobal("Function", ctor)
}

// initFunctionFamily wires the dynamic-function cousins — %GeneratorFunction%,
// %AsyncFunction%, %AsyncGeneratorFunction% — which build `function*` /
// `async function` / `async function*` sources respectively. Each is reachable
// as `f.constructor` of the matching function kind (via its prototype's
// constructor slot) and is itself a subclass of Function (its [[Prototype]] is
// the Function constructor). It runs after the family prototypes are created.
func (rt *Runtime) initFunctionFamily() {
	ctorV, _ := rt.getField(rt.global, "Function")
	installFamily := func(name, keyword string, famProto Value) {
		fc := rt.newNativeFunc(name, 1, rt.dynamicFunctionCtor(keyword, famProto))
		fo := rt.objPtr(fc)
		fo.proto = ctorV // %GeneratorFunction% extends Function
		fo.flags.isConstructor = true
		fo.defineOwn("prototype", famProto, 0)
		fo.defineOwn("name", rt.newString(name), attrConfigurable)
		rt.objPtr(famProto).defineOwn("constructor", fc, attrConfigurable)
	}
	installFamily("GeneratorFunction", "function*", rt.generatorFuncProto)
	installFamily("AsyncFunction", "async function", rt.asyncFunctionProto)
	installFamily("AsyncGeneratorFunction", "async function*", rt.asyncGeneratorFnProto)
}

// dynamicFunctionCtor returns the native implementation shared by Function and
// its %GeneratorFunction% / %AsyncFunction% / %AsyncGeneratorFunction% cousins.
// keyword is the source prefix ("function", "function*", "async function",
// "async function*"); defaultProto is the [[Prototype]] handed to the result
// when new.target is absent.
func (rt *Runtime) dynamicFunctionCtor(keyword string, defaultProto Value) nativeFunc {
	return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Capture new.target's prototype now: compiling/executing the body below
		// runs a frame that clears pendingNewTarget.
		resultProto := rt.newTargetProto(defaultProto)
		var params []string
		body := ""
		if len(args) > 0 {
			for i := 0; i < len(args)-1; i++ {
				s, e := rt.toStringValue(args[i])
				if e != nil {
					return mkundef(), e
				}
				params = append(params, string(rt.strBytes(s)))
			}
			bs, e := rt.toStringValue(args[len(args)-1])
			if e != nil {
				return mkundef(), e
			}
			body = string(rt.strBytes(bs))
		}
		// Compile in the global scope and capture the function via a temporary
		// global binding (the script completion value drops function-expression
		// statements, so we read the binding back instead).
		const tmp = "__goant_Function__"
		src := "globalThis." + tmp + " = (" + keyword + " anonymous(" + strings.Join(params, ",") + "\n) {\n" + body + "\n});"
		prog, perr := Parse("<anonymous>", src)
		if perr != nil {
			return mkundef(), &ThrowError{Value: rt.makeError(rt.errors.syntaxProto, "SyntaxError", perr.Error()), rt: rt}
		}
		fn, cerr := rt.Compile(prog, "<anonymous>", src)
		if cerr != nil {
			return mkundef(), &ThrowError{Value: rt.makeError(rt.errors.syntaxProto, "SyntaxError", cerr.Error()), rt: rt}
		}
		if _, xerr := rt.execute(fn); xerr != nil {
			if te, ok := xerr.(*ThrowError); ok {
				return mkundef(), te
			}
			return mkundef(), rt.typeError(xerr.Error())
		}
		v, _ := rt.getField(rt.global, tmp)
		rt.objPtr(rt.global).deleteOwn(tmp)
		if o := rt.objPtr(v); o != nil { // honor new.target (subclassing)
			o.proto = resultProto
		}
		return v, nil
	}
}
