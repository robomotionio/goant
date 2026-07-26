package engine

// Minimal host builtins needed to run and observe programs: console.*, print,
// and process.exit/argv (ant modules/io.c + process.c subset). The full builtin
// object surface (Object/Array/String/Math/Number/…) lands in Phase 4.

import (
	"fmt"
	"math"
	"os"
	"strings"
)

// ExitError signals a process.exit(code) request bubbling to the CLI/harness.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("process.exit(%d)", e.Code) }

// exitCode holds a pending process.exit code (nil = none).

func (rt *Runtime) initBuiltins() {
	g := rt.objPtr(rt.global)

	// console.log / console.error / console.warn / console.info
	console := rt.newObject(mknull())
	co := rt.objPtr(console)
	logFn := func(w *os.File) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			parts := make([]string, len(args))
			for i, a := range args {
				parts[i] = rt.inspect(a, false)
			}
			fmt.Fprintln(w, strings.Join(parts, " "))
			return mkundef(), nil
		}
	}
	co.defineOwn("log", rt.newNativeFunc("log", 0, logFn(os.Stdout)), attrWritable|attrConfigurable)
	co.defineOwn("info", rt.newNativeFunc("info", 0, logFn(os.Stdout)), attrWritable|attrConfigurable)
	co.defineOwn("error", rt.newNativeFunc("error", 0, logFn(os.Stderr)), attrWritable|attrConfigurable)
	co.defineOwn("warn", rt.newNativeFunc("warn", 0, logFn(os.Stderr)), attrWritable|attrConfigurable)
	g.defineOwn("console", console, attrWritable|attrConfigurable)

	// print (a bare stdout printer, convenient for conformance harnesses)
	g.defineOwn("print", rt.newNativeFunc("print", 0, logFn(os.Stdout)), attrWritable|attrConfigurable)

	// evalScript(source): evaluate source as a new SCRIPT in this realm. This is a
	// host capability rather than an ECMAScript one, and it is NOT eval: a
	// Script's top-level `var` and function declarations become NON-configurable
	// global properties, and its top-level let/const/class join the global lexical
	// environment where the next Script still sees them.
	g.defineOwn("evalScript", rt.newNativeFunc("evalScript", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.evalScriptSource(string(rt.strBytes(sv)))
	}), attrWritable|attrConfigurable)

	// createRealm(): a second realm on the same value pools — a fresh global with
	// its own intrinsics, while objects still pass freely between the two. A host
	// capability, like evalScript, and the one Test262's $262.createRealm needs.
	// The returned object carries the new realm's global and an evalScript bound
	// to IT (a native is otherwise handed the CALLING runtime, which would compile
	// into the wrong realm).
	g.defineOwn("createRealm", rt.newNativeFunc("createRealm", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		realm := rt.NewRealm()
		out := rt.newObject(rt.objectProto)
		oo := rt.objPtr(out)
		oo.defineOwn("global", realm.global, attrWritable|attrEnumerable|attrConfigurable)
		oo.defineOwn("evalScript", rt.newNativeFunc("evalScript", 1, func(caller *Runtime, this Value, args []Value) (Value, *ThrowError) {
			sv, e := caller.toStringValue(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			return realm.evalScriptSource(string(caller.strBytes(sv)))
		}), attrWritable|attrEnumerable|attrConfigurable)
		cr, _ := realm.getField(realm.global, "createRealm")
		oo.defineOwn("createRealm", cr, attrWritable|attrEnumerable|attrConfigurable)
		return out, nil
	}), attrWritable|attrConfigurable)

	// Timers (HTML setTimeout/setInterval). goant runs a virtual clock: callbacks
	// fire from the host event loop in (delay, scheduling-order), after the
	// microtask queue drains. The delay only orders tasks; no time actually
	// elapses. Extra arguments are forwarded to the callback.
	timer := func(period bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			fn := arg(args, 0)
			if !rt.isCallable(fn) {
				return mknum(0), nil
			}
			delay := 0.0
			if len(args) > 1 {
				if n, ok := rt.toNumberPrimitive(args[1]); ok {
					delay = n
				}
			}
			var extra []Value
			if len(args) > 2 {
				extra = append(extra, args[2:]...)
			}
			p := 0.0
			if period {
				if delay <= 0 {
					p = 1 // a zero-delay interval still needs a positive period
				} else {
					p = delay
				}
			}
			return rt.scheduleTimer(fn, delay, p, extra), nil
		}
	}
	g.defineOwn("setTimeout", rt.newNativeFunc("setTimeout", 2, timer(false)), attrWritable|attrConfigurable)
	g.defineOwn("setInterval", rt.newNativeFunc("setInterval", 2, timer(true)), attrWritable|attrConfigurable)
	clear := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		rt.cancelTimer(arg(args, 0))
		return mkundef(), nil
	}
	g.defineOwn("clearTimeout", rt.newNativeFunc("clearTimeout", 1, clear), attrWritable|attrConfigurable)
	g.defineOwn("clearInterval", rt.newNativeFunc("clearInterval", 1, clear), attrWritable|attrConfigurable)
	g.defineOwn("setImmediate", rt.newNativeFunc("setImmediate", 1, timer(false)), attrWritable|attrConfigurable)
	g.defineOwn("clearImmediate", rt.newNativeFunc("clearImmediate", 1, clear), attrWritable|attrConfigurable)

	// queueMicrotask(fn): schedule fn on the promise-reaction (microtask) queue.
	g.defineOwn("queueMicrotask", rt.newNativeFunc("queueMicrotask", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		if !rt.isCallable(fn) {
			return mkundef(), rt.typeError("queueMicrotask argument is not callable")
		}
		rt.enqueueMicrotask(func() { rt.callValue(fn, mkundef(), nil) })
		return mkundef(), nil
	}), attrWritable|attrConfigurable)

	// eval — reaching the native function means an *indirect* eval (the callee was
	// not the syntactic `eval` reference, e.g. `(0,eval)(s)` or `globalThis.eval(s)`),
	// which always evaluates in global scope and sloppy mode. A *direct* eval —
	// `eval(s)` with the intrinsic still bound — is compiled to OpEval and never
	// reaches here; it inherits the caller's scope, strictness, and context.
	evalFn := rt.newNativeFunc("eval", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		if !v.IsString() {
			return v, nil // eval of a non-string returns it unchanged
		}
		return rt.performIndirectEval(string(rt.strBytes(v)))
	})
	rt.evalFn = evalFn
	g.defineOwn("eval", evalFn, attrWritable|attrConfigurable)

	// process.exit / process.argv (subset the harness needs)
	process := rt.newObject(mknull())
	po := rt.objPtr(process)
	po.defineOwn("exit", rt.newNativeFunc("exit", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		code := 0
		if len(args) > 0 {
			if n, ok := rt.toNumberPrimitive(args[0]); ok {
				code = int(n)
			}
		}
		rt.exitCode = &code
		return mkundef(), &ThrowError{Value: mkundef(), rt: rt, control: true}
	}), attrWritable|attrConfigurable)
	g.defineOwn("process", process, attrWritable|attrConfigurable)
}

// defMethod defines a non-enumerable method on an object (ant defmethod).
func (rt *Runtime) defMethod(o *object, name string, length int, fn nativeFunc) {
	o.defineOwn(name, rt.newNativeFunc(name, length, fn), attrWritable|attrConfigurable)
}

// defGlobal installs a value as a writable/configurable global property.
func (rt *Runtime) defGlobal(name string, v Value) {
	rt.objPtr(rt.global).defineOwn(name, v, attrWritable|attrConfigurable)
}

// arg returns args[i] or undefined.
func arg(args []Value, i int) Value {
	if i < len(args) {
		return args[i]
	}
	return mkundef()
}

// toObject coerces this to an object (ant ToObject); primitives box via their
// wrapper (approximated by returning the value for method receivers).
func (rt *Runtime) lengthOf(v Value) (int, *ThrowError) {
	lv, e := rt.getField(v, "length")
	if e != nil {
		return 0, e
	}
	n, e := rt.toNumber(lv)
	if e != nil {
		return 0, e
	}
	// ToLength: ToIntegerOrInfinity, clamp to [0, 2^53-1].
	if n != n { // NaN
		return 0, nil
	}
	n = math.Trunc(n)
	if n <= 0 {
		return 0, nil
	}
	const maxLen = 1<<53 - 1
	if n >= maxLen {
		return maxLen, nil
	}
	return int(n), nil
}

// createListFromArrayLike implements CreateListFromArrayLike(obj) with the
// default element-type list (any value): the argument must be an Object (else a
// TypeError), and elements 0..ToLength(obj.length)-1 are read with abrupt
// propagation. Callers that permit a nullish argument (e.g. Function.prototype.
// apply) must guard it before calling.
func (rt *Runtime) createListFromArrayLike(a Value) ([]Value, *ThrowError) {
	if rt.objPtr(a) == nil {
		return nil, rt.typeError("CreateListFromArrayLike called on non-object")
	}
	n, e := rt.lengthOf(a)
	if e != nil {
		return nil, e
	}
	list := make([]Value, n)
	for i := 0; i < n; i++ {
		v, e := rt.getElement(a, mknum(float64(i)))
		if e != nil {
			return nil, e
		}
		list[i] = v
	}
	return list, nil
}

// hasElem implements the spec HasProperty(O, i) for an integer index, used by
// the generic array algorithms to skip holes (and to route through a Proxy's
// [[HasProperty]] trap).
func (rt *Runtime) hasElem(obj Value, i int) bool {
	return rt.hasProp(obj, numberToString(float64(i)))
}

// hasElemE is HasProperty(O, i) that propagates an abrupt completion from a
// Proxy [[HasProperty]] trap (hasElem swallows it), for algorithms that must
// ReturnIfAbrupt on the HasProperty step (e.g. copyWithin).
func (rt *Runtime) hasElemE(obj Value, i int) (bool, *ThrowError) {
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		return rt.proxyHas(o.proxy, rt.internString(numberToString(float64(i))))
	}
	return rt.hasProp(obj, numberToString(float64(i))), nil
}

// inspect renders a value for console output. quoted controls whether strings
// nested inside containers are quoted (top-level strings print bare).
func (rt *Runtime) inspect(v Value, quoted bool) string {
	switch v.Type() {
	case TUndef:
		return "undefined"
	case TNull:
		return "null"
	case TBool:
		if v.Bool() {
			return "true"
		}
		return "false"
	case TNum:
		return numberToString(v.Number())
	case TStr:
		s := string(rt.strBytes(v))
		if quoted {
			return "'" + s + "'"
		}
		return s
	case TArr:
		o := rt.objPtr(v)
		parts := make([]string, 0, o.arrLen)
		for i := uint32(0); i < o.arrLen; i++ {
			el := mkundef()
			if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
				el = o.arr[i]
			}
			parts = append(parts, rt.inspect(el, true))
		}
		return "[ " + strings.Join(parts, ", ") + " ]"
	case TFunc, TCFunc:
		o := rt.objPtr(v)
		name := ""
		if o != nil {
			if nv, ok := o.getOwn("name"); ok && nv.IsString() {
				name = string(rt.strBytes(nv))
			}
		}
		if name == "" {
			return "[Function (anonymous)]"
		}
		return "[Function: " + name + "]"
	default:
		if v.IsObjectType() {
			o := rt.objPtr(v)
			keys := o.ownKeysEnumerable()
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				val, _ := o.getOwn(k)
				parts = append(parts, k+": "+rt.inspect(val, true))
			}
			if len(parts) == 0 {
				return "{}"
			}
			return "{ " + strings.Join(parts, ", ") + " }"
		}
		return typeName(v.Type())
	}
}

// evalScriptSource evaluates src as a new Script in THIS realm. It is a method
// rather than an inline closure so createRealm can hand another realm a copy
// bound to it — a native is otherwise passed the calling runtime.
func (rt *Runtime) evalScriptSource(src string) (Value, *ThrowError) {
	prog, perr := Parse("<script>", src)
	if perr != nil {
		ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(perr.Error())})
		return mkundef(), &ThrowError{Value: ev, rt: rt}
	}
	fn, cerr := rt.Compile(prog, "<script>", src)
	if gde, ok := cerr.(*GlobalDeclError); ok {
		return mkundef(), rt.typeError(gde.Msg)
	}
	if cerr != nil {
		ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(cerr.Error())})
		return mkundef(), &ThrowError{Value: ev, rt: rt}
	}
	return rt.runFrame(fn, nil, mkundef(), rt.global, nil)
}
