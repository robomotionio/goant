package engine

// Minimal host builtins needed to run and observe programs: console.*, print,
// and process.exit/argv (ant modules/io.c + process.c subset). The full builtin
// object surface (Object/Array/String/Math/Number/…) lands in Phase 4.

import (
	"fmt"
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

	// eval — indirect (global-scope) evaluation. Direct-eval local-scope access
	// is a later refinement; this handles the common expression/statement cases.
	g.defineOwn("eval", rt.newNativeFunc("eval", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		if !v.IsString() {
			return v, nil // eval of a non-string returns it unchanged
		}
		src := string(rt.strBytes(v))
		prog, perr := Parse("<eval>", src)
		if perr != nil {
			ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(perr.Error())})
			return mkundef(), &ThrowError{Value: ev, rt: rt}
		}
		fn, cerr := rt.CompileEval(prog, "<eval>", src)
		if cerr != nil {
			ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(cerr.Error())})
			return mkundef(), &ThrowError{Value: ev, rt: rt}
		}
		return rt.runFrame(fn, nil, mkundef(), rt.global, nil)
	}), attrWritable|attrConfigurable)

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
	if n < 0 || n != n {
		return 0, nil
	}
	return int(n), nil
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
