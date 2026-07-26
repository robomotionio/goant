package engine

// Exported embedding surface.
//
// Everything the engine does internally is unexported, on purpose: the
// interpreter is free to change shape as long as the spec-visible behaviour
// holds. This file is the one place that promises stability to a host program,
// so it is deliberately narrow — object/property access, value conversion,
// host functions, promises and the job queue, and nothing else.
//
// Values are handles into this Runtime's pools, so a Value is only meaningful
// paired with the Runtime that produced it. The host wrapper types keep that
// pairing; nothing here can enforce it.

import (
	"errors"
	"unsafe"
)

// HostFunc is the signature of a Go function callable from JavaScript. It is
// the same signature the built-ins use, so a host function is not a second
// class of callable — it takes the same fast path.
type HostFunc = func(rt *Runtime, this Value, args []Value) (Value, *ThrowError)

// Script is a compiled program, separated from execution so an embedder can
// compile once and run many times (the "unbound script" pattern). The compiled
// form is bound to the Runtime that produced it.
type Script struct {
	fn  *svFunc
	src string
}

// Source returns the text the script was compiled from.
func (s *Script) Source() string { return s.src }

// CompileScript parses and compiles src without running it.
func (rt *Runtime) CompileScript(filename, src string) (*Script, error) {
	prog, err := Parse(filename, src)
	if err != nil {
		return nil, err
	}
	fn, err := rt.Compile(prog, filename, src)
	if err != nil {
		return nil, err
	}
	return &Script{fn: fn, src: src}, nil
}

// RunScript executes a previously compiled script and returns its completion
// value. It does not drain the job queue — a host that cares about promise
// settlement calls DrainJobs afterwards, mirroring the explicit microtask
// checkpoint an embedder is used to.
func (rt *Runtime) RunScript(s *Script) (Value, error) {
	if s == nil || s.fn == nil {
		return mkundef(), errors.New("goant: nil script")
	}
	rt.filename = s.fn.filename
	return rt.execute(s.fn)
}

// DrainJobs runs the microtask queue to completion.
func (rt *Runtime) DrainJobs() { rt.runEventLoop() }

// Global returns the global object (globalThis).
func (rt *Runtime) Global() Value { return rt.global }

// Undefined and Null return the corresponding primitives.
func (rt *Runtime) Undefined() Value { return mkundef() }
func (rt *Runtime) Null() Value      { return mknull() }

// NewString interns s and returns it as a JS string.
//
// Interning makes the string canonical, which is what property keys and
// identifiers need — but the intern table is permanent and shared by every
// realm on this runtime, so an interned string is never reclaimed. Use this
// only for names, never for data.
func (rt *Runtime) NewString(s string) Value { return rt.internString(s) }

// NewStringData returns s as a JS string without interning it.
//
// This is the right constructor for host data — a message payload, a file's
// contents, anything large or unbounded in variety. Interning such a string
// would pin it in the runtime's intern table for the process's life: a host
// that passes in a distinct 50 MB message per call would retain every one of
// them forever, which is the difference between a working embedding and one
// that dies overnight.
func (rt *Runtime) NewStringData(s string) Value { return rt.newString(s) }

// NewStringBytes returns b as a JS string without interning it, taking
// ownership of the slice rather than copying it. The caller must not modify b
// afterwards — JS strings are immutable and the engine will read it directly.
// This is the zero-copy path a cgo binding could not offer.
func (rt *Runtime) NewStringBytes(b []byte) Value { return rt.newStringBytes(b) }

// NewNumber and NewBool wrap Go primitives.
func (rt *Runtime) NewNumber(f float64) Value { return mknum(f) }
func (rt *Runtime) NewBool(b bool) Value      { return mkbool(b) }

// NewObject returns a fresh ordinary object with Object.prototype.
func (rt *Runtime) NewObject() Value { return rt.newObject(rt.objectProto) }

// NewFunction returns a callable object backed by a Go function.
func (rt *Runtime) NewFunction(name string, length int, fn HostFunc) Value {
	return rt.newNativeFunc(name, length, fn)
}

// GetProp reads obj.name, running any getter and honouring the prototype
// chain. A thrown JS exception is returned as an error carrying the value.
func (rt *Runtime) GetProp(obj Value, name string) (Value, error) {
	v, terr := rt.getField(obj, name)
	if terr != nil {
		return mkundef(), terr
	}
	return v, nil
}

// SetProp writes obj.name, running any setter.
func (rt *Runtime) SetProp(obj Value, name string, v Value) error {
	if terr := rt.setField(obj, name, v); terr != nil {
		return terr
	}
	return nil
}

// Call invokes fn with the given this-binding and arguments.
func (rt *Runtime) Call(fn, this Value, args []Value) (Value, error) {
	v, terr := rt.callValue(fn, this, args)
	if terr != nil {
		return mkundef(), terr
	}
	return v, nil
}

// Type predicates. These answer questions about the value as it is, with no
// coercion — IsNumber is false for a String that happens to look numeric.
func (rt *Runtime) IsUndefined(v Value) bool { return v.IsUndefined() }
func (rt *Runtime) IsNull(v Value) bool      { return v.IsNull() }
func (rt *Runtime) IsBool(v Value) bool      { return v.IsBool() }
func (rt *Runtime) IsNumber(v Value) bool    { return v.IsNumber() }
func (rt *Runtime) IsString(v Value) bool    { return v.IsString() }

// IsObject reports whether v is an object (including functions and arrays),
// matching the embedder-facing sense of "object" rather than typeof.
func (rt *Runtime) IsObject(v Value) bool { return v.IsObjectType() }

// IsFunction reports whether v is callable.
func (rt *Runtime) IsFunction(v Value) bool {
	o := rt.objPtr(v)
	return o != nil && o.flags.isCallable
}

// IsArray reports whether v is an Array exotic object (Array.isArray, which
// also sees through a Proxy to its target).
func (rt *Runtime) IsArray(v Value) bool { return rt.isArrayValue(v) }

// IsPromise reports whether v carries promise settlement state.
func (rt *Runtime) IsPromise(v Value) bool { return rt.isPromise(v) }

// Promise settlement states, matching the internal encoding.
const (
	PromisePending   = iota // 0
	PromiseFulfilled        // 1
	PromiseRejected         // 2
)

// PromiseState returns the settlement state and value of a promise. ok is
// false if v is not a promise. For a pending promise the value is undefined.
func (rt *Runtime) PromiseState(v Value) (state int, result Value, ok bool) {
	o := rt.objPtr(v)
	if o == nil || o.promise == nil {
		return PromisePending, mkundef(), false
	}
	return o.promise.state, o.promise.value, true
}

// ToBool applies ToBoolean.
func (rt *Runtime) ToBool(v Value) bool { return rt.toBoolean(v) }

// ToString applies ToString and returns the result as a Go string. It can
// throw, because a value's toString/@@toPrimitive is arbitrary JS.
func (rt *Runtime) ToString(v Value) (string, error) {
	s, terr := rt.toStringValue(v)
	if terr != nil {
		return "", terr
	}
	return rt.strGo(s), nil
}

// ToNumber applies ToNumber.
func (rt *Runtime) ToNumber(v Value) (float64, error) {
	p, terr := rt.toPrimitive(v, "number")
	if terr != nil {
		return 0, terr
	}
	f, _ := rt.toNumberPrimitive(p)
	return f, nil
}

// TypeOf returns the `typeof` string for v.
func (rt *Runtime) TypeOf(v Value) string { return rt.typeofString(v) }

// JSONStringify applies JSON.stringify(v). ok is false when the result is
// undefined — JSON.stringify(undefined) is not the string "undefined", it is
// no value at all, and collapsing the two would corrupt a host round-trip.
func (rt *Runtime) JSONStringify(v Value) (s string, ok bool, err error) {
	jsonObj, terr := rt.getField(rt.global, "JSON")
	if terr != nil {
		return "", false, terr
	}
	fn, terr := rt.getField(jsonObj, "stringify")
	if terr != nil {
		return "", false, terr
	}
	res, terr := rt.callValue(fn, jsonObj, []Value{v})
	if terr != nil {
		return "", false, terr
	}
	if res.IsUndefined() {
		return "", false, nil
	}
	return rt.strGo(res), true, nil
}

// JSONParse applies JSON.parse(s).
func (rt *Runtime) JSONParse(s string) (Value, error) {
	jsonObj, terr := rt.getField(rt.global, "JSON")
	if terr != nil {
		return mkundef(), terr
	}
	fn, terr := rt.getField(jsonObj, "parse")
	if terr != nil {
		return mkundef(), terr
	}
	v, terr := rt.callValue(fn, jsonObj, []Value{rt.internString(s)})
	if terr != nil {
		return mkundef(), terr
	}
	return v, nil
}

// HeapUsage reports this runtime's own allocation, by live cell count and an
// estimate of the bytes those cells occupy.
//
// This is the runtime's occupancy, not the process's: an embedder running many
// runtimes needs to know which one is growing, and Go's process-wide MemStats
// cannot tell it. The byte figure is cells times their element size and so
// excludes payloads hanging off them (string bytes, array backing stores), which
// makes it a floor rather than a total.
//
// It is a meaningful number to retire a pooled runtime on, because there is
// currently no collector: cells are reclaimed only when the whole Runtime is
// dropped and Go collects the pools (PLAN.md Phase 7). Until that lands, this
// count only goes up over a runtime's life, so an embedder that reuses runtimes
// must watch it.
func (rt *Runtime) HeapUsage() (cells int, bytes uint64) {
	if rt == nil {
		return 0, 0
	}
	n := rt.objects.len() + rt.strings.len() + rt.symbols.len() +
		rt.closures.len() + rt.bigints.len()
	b := uint64(rt.objects.len())*uint64(unsafe.Sizeof(object{})) +
		uint64(rt.strings.len())*uint64(unsafe.Sizeof(flatString{})) +
		uint64(rt.symbols.len())*uint64(unsafe.Sizeof(symbol{})) +
		uint64(rt.closures.len())*uint64(unsafe.Sizeof(closure{})) +
		uint64(rt.bigints.len())*uint64(unsafe.Sizeof(bigIntCell{}))
	return n, b
}

// InternedCount returns how many strings are in this runtime's intern table.
//
// The table is permanent and shared by every realm, so this number only rises.
// It is exposed so a host can tell the difference between memory that is merely
// uncollected and memory that is pinned forever — and so a test can prove that
// passing data in does not pin it.
func (rt *Runtime) InternedCount() int {
	if rt == nil {
		return 0
	}
	return len(rt.interned)
}

// NewError builds an Error object with the given message, for a host function
// that wants to throw something the script can catch and inspect.
func (rt *Runtime) NewError(msg string) Value {
	return rt.makeError(rt.errorProto, "Error", msg)
}

// Throw wraps a value as a thrown JS exception, for returning from a HostFunc.
func (rt *Runtime) Throw(v Value) *ThrowError {
	return &ThrowError{Value: v, rt: rt}
}

// ThrowError builds and wraps an Error in one step.
func (rt *Runtime) ThrowError(msg string) *ThrowError {
	return rt.Throw(rt.NewError(msg))
}

// ExceptionValue extracts the thrown JS value from an error returned by this
// package, so a host can inspect it rather than only read its message. ok is
// false for errors that are not JS exceptions (parse errors, for instance).
func ExceptionValue(err error) (Value, bool) {
	var terr *ThrowError
	if errors.As(err, &terr) {
		return terr.Value, true
	}
	return mkundef(), false
}
