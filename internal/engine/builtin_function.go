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
		bound := rt.newNativeFunc("bound", 0, func(rt *Runtime, _ Value, callArgs []Value) (Value, *ThrowError) {
			full := append(append([]Value{}, boundArgs...), callArgs...)
			return rt.callValue(target, boundThis, full)
		})
		// bound.length = max(0, target.length - boundArgs)
		tlen := 0
		if lv, e := rt.getField(target, "length"); e == nil && lv.Type() == TNum {
			tlen = int(lv.Number())
		}
		blen := tlen - len(boundArgs)
		if blen < 0 {
			blen = 0
		}
		rt.objPtr(bound).defineOwn("length", mknum(float64(blen)), attrConfigurable)
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
	ctor := rt.newNativeFunc("Function", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
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
		src := "globalThis." + tmp + " = (function anonymous(" + strings.Join(params, ",") + "\n) {\n" + body + "\n});"
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
		return v, nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.functionProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defGlobal("Function", ctor)
}
