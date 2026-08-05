package engine

// Exported embedding surface.
//
// Everything the engine does internally is unexported, on purpose: the
// interpreter is free to change shape as long as the spec-visible behaviour
// holds. This file is the one place that promises stability to a host program,
// so it is deliberately narrow — object and property access, array and index
// access, construction, value conversion, host functions, promises and the job
// queue, and nothing else.
//
// Values are handles into this Runtime's pools, so a Value is only meaningful
// paired with the Runtime that produced it. The host wrapper types keep that
// pairing; nothing here can enforce it.

import (
	"errors"
	"math/big"
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
	v, err := rt.execute(s.fn)
	// A blob that could not be fetched raises the interrupt, but a script short
	// enough to finish before the next check point would carry on and return
	// normally — the read that failed is not a place that can report. Checking
	// on the way out means the host always learns, whether the script was
	// stopped mid-flight or simply finished holding a value it never got.
	if rt.BlobResolveFailed() {
		if berr := rt.BlobResolveError(); berr != nil {
			return mkundef(), berr
		}
	}
	return v, err
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
	// Cell headers are only half the story, and for most scripts the smaller
	// half: the bytes are in string payloads, array element storage and
	// ArrayBuffer stores hanging off those cells. With a limit set this is the
	// figure the last sweep judged it against, so the two always agree; without
	// one it is not maintained per cycle and is totalled here instead, since a
	// host asking for Stats is asking for the number, not for the cheap half.
	if rt.heapLimit != 0 {
		b += rt.liveBytes
	} else {
		b += rt.liveePayload()
	}
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

// --- containers -------------------------------------------------------------

// NewArray returns a fresh Array holding vals.
func (rt *Runtime) NewArray(vals ...Value) Value {
	a := rt.newArray()
	o := rt.objPtr(a)
	for i, v := range vals {
		rt.arraySet(o, uint32(i), v)
	}
	return a
}

// GetIndex reads obj[i], honouring getters, the prototype chain and exotic
// index behaviour (arrays, strings, typed arrays).
func (rt *Runtime) GetIndex(obj Value, i int) (Value, error) {
	v, terr := rt.getElement(obj, mknum(float64(i)))
	if terr != nil {
		return mkundef(), terr
	}
	return v, nil
}

// SetIndex writes obj[i].
func (rt *Runtime) SetIndex(obj Value, i int, v Value) error {
	if terr := rt.setElement(obj, mknum(float64(i)), v); terr != nil {
		return terr
	}
	return nil
}

// LengthOf reads obj.length and coerces it the way the array built-ins do. It
// is the length of an array, a string or an array-like; anything without a
// numeric length reports 0.
func (rt *Runtime) LengthOf(obj Value) (int, error) {
	n, terr := rt.lengthOf(obj)
	if terr != nil {
		return 0, terr
	}
	return n, nil
}

// HasProp reports whether obj.name resolves, own or inherited — the `in`
// operator. A Proxy's has trap can throw, so this can fail.
func (rt *Runtime) HasProp(obj Value, name string) (bool, error) {
	has, terr := rt.hasPropE(obj, name)
	if terr != nil {
		return false, terr
	}
	return has, nil
}

// DeleteProp removes obj.name, returning whether it is gone afterwards — the
// `delete` operator, which reports false for a non-configurable property
// rather than throwing (outside strict mode).
func (rt *Runtime) DeleteProp(obj Value, name string) (bool, error) {
	ok, terr := rt.deleteElement(obj, rt.internString(name))
	if terr != nil {
		return false, terr
	}
	return ok, nil
}

// OwnKeys returns obj's own enumerable string keys, in property order: integer
// indices ascending, then the rest in insertion order. This is Object.keys, so
// it excludes symbols, inherited properties and non-enumerable ones.
func (rt *Runtime) OwnKeys(obj Value) ([]string, error) {
	keys, terr := rt.enumerableOwnKeysE(obj)
	if terr != nil {
		return nil, terr
	}
	return keys, nil
}

// Construct invokes fn as a constructor — `new fn(args...)`.
func (rt *Runtime) Construct(fn Value, args []Value) (Value, error) {
	if !rt.isConstructorValue(fn) {
		return mkundef(), errors.New("goant: value is not a constructor")
	}
	v, terr := rt.construct(fn, args)
	if terr != nil {
		return mkundef(), terr
	}
	return v, nil
}

// --- further type predicates ------------------------------------------------

// IsSymbol and IsBigInt cover the two primitives the basic predicates omit.
func (rt *Runtime) IsSymbol(v Value) bool { return v.IsSymbol() }
func (rt *Runtime) IsBigInt(v Value) bool { return v.Type() == TBigInt }

// IsTypedArray reports whether v is an integer-indexed exotic object. Such a
// value is not IsObject — it has its own tag — so a host walking a graph must
// test for it separately.
func (rt *Runtime) IsTypedArray(v Value) bool { return v.Type() == TTypedArray }

// IsDate reports whether v is a Date object, by its internal slot rather than
// by its prototype, so an object merely inheriting from Date.prototype is not
// mistaken for one.
func (rt *Runtime) IsDate(v Value) bool {
	o := rt.objPtr(v)
	return o != nil && o.brandID() == brandDate
}

// IsError reports whether v inherits from Error.prototype.
func (rt *Runtime) IsError(v Value) bool {
	return v.IsObjectType() && rt.hasInProtoChain(v, rt.errorProto)
}

// StrictEquals applies ===.
func (rt *Runtime) StrictEquals(a, b Value) bool { return rt.strictEquals(a, b) }

// --- dates, bigints, bytes --------------------------------------------------

// DateMillis returns a Date's time value in milliseconds since the epoch. ok is
// false if v is not a Date; the value is NaN for an invalid one.
func (rt *Runtime) DateMillis(v Value) (ms float64, ok bool) {
	o := rt.objPtr(v)
	if o == nil || o.brandID() != brandDate {
		return 0, false
	}
	return o.boxed.Number(), true
}

// NewDate builds a Date from milliseconds since the epoch, through the Date
// constructor so it gets the running realm's prototype.
func (rt *Runtime) NewDate(ms float64) (Value, error) {
	ctor, terr := rt.getField(rt.global, "Date")
	if terr != nil {
		return mkundef(), terr
	}
	return rt.Construct(ctor, []Value{mknum(ms)})
}

// BigInt returns a BigInt's value. The returned big.Int is a copy, so the
// caller may keep or modify it.
func (rt *Runtime) BigInt(v Value) (*big.Int, bool) {
	if v.Type() != TBigInt {
		return nil, false
	}
	cell := rt.bigints.get(Handle(v.handle()))
	if cell == nil || cell.v == nil {
		return nil, false
	}
	return new(big.Int).Set(cell.v), true
}

// NewBigIntValue boxes a big.Int. The engine copies it, so the caller keeps
// ownership of x.
func (rt *Runtime) NewBigIntValue(x *big.Int) Value {
	if x == nil {
		return rt.newBigInt(new(big.Int))
	}
	return rt.newBigInt(new(big.Int).Set(x))
}

// NewUint8Array wraps b as a Uint8Array without copying it: the returned view
// reads and writes the caller's slice directly. Like NewStringBytes this trades
// a copy for a contract — b is now the array's backing store, and a host that
// keeps writing to it is writing into live JavaScript state.
//
// Pass a copy if that is not what you want.
func (rt *Runtime) NewUint8Array(b []byte) Value {
	buf := rt.newObject(rt.arrayBufferProto)
	bo := rt.objPtr(buf)
	bo.abuf = b
	bo.abMax = len(b)
	bo.abObj = true

	h, o := rt.objects.alloc()
	o.self = h
	o.proto = rt.typedArrayProtos[taUint8]
	o.shape = rt.newShape()
	o.typeTag = TTypedArray
	o.flags.extensible = true
	o.ta = rt.newTAView(buf, taUint8, 0, len(b), false)
	return mkval(TTypedArray, uint64(h))
}

// IsByteArray reports whether v is a value whose contents are unambiguously
// bytes: an ArrayBuffer, or a one-byte-per-element view of one. A Float64Array
// has bytes too, but they are not what it holds, so it is excluded — a host
// deciding how to represent a value should not turn its numbers into a blob.
func (rt *Runtime) IsByteArray(v Value) bool {
	o := rt.objPtr(v)
	if o == nil {
		return false
	}
	if o.abObj {
		return true
	}
	if o.ta == nil {
		return false
	}
	switch o.ta.kind {
	case taInt8, taUint8, taUint8Clamped:
		return true
	}
	return false
}

// Bytes returns the bytes behind an ArrayBuffer or any typed-array view of one,
// without copying. ok is false for anything else, and for a detached buffer.
//
// For a view the slice covers only that view's window. The bytes are live: a
// write through the returned slice is visible to the script.
func (rt *Runtime) Bytes(v Value) ([]byte, bool) {
	o := rt.objPtr(v)
	if o == nil {
		return nil, false
	}
	if o.abObj {
		if o.abuf == nil {
			return nil, false
		}
		return o.abuf, true
	}
	if o.ta == nil || rt.taDetached(o) {
		return nil, false
	}
	buf := rt.taBytes(o.ta)
	start := o.ta.byteOffset
	end := len(buf)
	if !o.ta.track {
		end = start + o.ta.length*o.ta.size()
	}
	if start > len(buf) || end > len(buf) || start > end {
		return nil, false
	}
	return buf[start:end:end], true
}
