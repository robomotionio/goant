package engine

import (
	"errors"
	"math"
)

// Runtime is a single JavaScript isolate — the Go analogue of ant's ant_t
// (include/internal.h struct ant_isolate_t). It owns the non-moving pools that
// back heap Values plus the interning tables and global state.
//
// This struct grows phase by phase; Phase 0 establishes the pools and the
// public entry points. Phases 1–3 wire in the lexer/parser, object model, and
// interpreter.
type Runtime struct {
	// Heap pools (TODO 0.2). Payload element types are placeholders that fill
	// in as the object model (Phase 2) and strings land.
	objects  *pool[object]
	strings  *pool[flatString]
	symbols  *pool[symbol]
	closures *pool[closure]
	bigints  *pool[bigIntCell]

	// String interning table (Phase 2): interned text -> flat-string handle.
	interned map[string]Handle

	// Thrown-value convention (ant thrown_value/thrown_exists).
	thrownValue  Value
	thrownExists bool

	// global is the global object (globalThis); top-level var/function bindings
	// and predefined globals (NaN/Infinity/undefined) live here.
	global Value

	// Core prototype objects (ant isolate proto fields).
	objectProto           Value
	functionProto         Value
	arrayProto            Value
	stringProto           Value
	numberProto           Value
	bigintProto           Value
	booleanProto          Value
	errorProto            Value
	regexpProto           Value
	regexpCtor            Value // %RegExp% constructor (SpeciesConstructor default)
	mapProto              Value
	setProto              Value
	symbolProto           Value
	promiseProto          Value
	promiseCtor           Value // the %Promise% constructor (species fast-path check)
	genProto              Value // %GeneratorPrototype%
	iteratorProto         Value // %IteratorPrototype%
	asyncIteratorProto    Value // %AsyncIteratorPrototype%
	asyncGenProto         Value // %AsyncGeneratorPrototype%
	arrayIterProto        Value // %ArrayIteratorPrototype%
	mapIterProto          Value // %MapIteratorPrototype%
	setIterProto          Value // %SetIteratorPrototype%
	stringIterProto       Value // %StringIteratorPrototype%
	asyncFunctionProto    Value // %AsyncFunction.prototype%
	generatorFuncProto    Value // %GeneratorFunction.prototype%
	asyncGeneratorFnProto Value

	// TypedArray / ArrayBuffer / DataView prototypes.
	arrayBufferProto Value
	dataViewProto    Value
	typedArrayProto  Value   // %TypedArray%.prototype
	typedArrayProtos []Value // per-kind prototypes (indexed by taKind)

	// curGen is the coroutine currently executing on the JS "thread" (nil on the
	// main frame). Generator/async suspension hands control between the driver
	// and the coroutine goroutine via curGen's channels.
	curGen *genState

	// microtasks is the promise-reaction job queue (ant microtask ring). It is
	// drained after the top-level script and after each callback that may have
	// enqueued jobs.
	microtasks []func()

	// macrotasks is the timer queue (setTimeout/setInterval). goant has no real
	// clock: tasks run in (delay, insertion) order after microtasks drain.
	macrotasks []macrotask
	timerSeq   uint64

	// Well-known symbols and the Symbol.for registry.
	symbolCounter         uint64
	symbolRegistry        map[string]Value
	symIterator           Value
	symAsyncIterator      Value
	symHasInstance        Value
	symToPrimitive        Value
	symToStringTag        Value
	symMatch              Value
	symReplace            Value
	symSearch             Value
	symSplit              Value
	symSpecies            Value
	symIsConcatSpreadable Value
	symUnscopables        Value
	symDispose            Value
	symAsyncDispose       Value

	// errors holds the NativeError constructors/prototypes for internal throws.
	errors errorCtors

	// poison is the poison-pill accessor (throws) for strict caller/callee/
	// arguments (ES5 §13.2.3).
	poison Value

	// pendingNewTarget carries new.target from construct into the next runFrame;
	// activeNewTarget holds the executing class constructor's new.target so a
	// super() call (an ordinary call) can propagate it to the parent ctor.
	pendingNewTarget Value
	activeNewTarget  Value

	// pendingNewTargetProto caches the [[Prototype]] that constructWithTarget
	// already resolved from newTarget.prototype, so a native constructor's
	// newTargetProto reuses it instead of performing a second observable [[Get]]
	// of "prototype" (visible through a Proxy newTarget). 0 = not cached.
	pendingNewTargetProto Value

	// frameDepth tracks native call depth for the stack-overflow guard.
	frameDepth int

	// frameStrict is the strictness of the currently executing JS frame, so a
	// direct eval() (native call) inherits the caller's strict mode.
	frameStrict bool

	// exitCode is set by process.exit() to request termination.
	exitCode *int

	// randState is the Math.random xorshift generator state.
	randState uint64

	filename string
}

// flatString is a heap-resident flat string payload (Phase 2 strings).
type flatString struct {
	bytes   []byte
	isASCII int8 // STR_ASCII_UNKNOWN/YES/NO
}

type symbol struct {
	desc Value
	id   uint64
}

// upvalue captures a variable shared between a closure and its defining frame
// (ant sv_upvalue). While "open" it points into a live frame's locals; when the
// frame returns it is "closed" — the value is copied into its own cell.
type upvalue struct {
	location *Value
	closed   Value
}

func (u *upvalue) get() Value  { return *u.location }
func (u *upvalue) set(v Value) { *u.location = v }
func (u *upvalue) closeUp()    { u.closed = *u.location; u.location = &u.closed }

// closure binds a compiled function to its captured upvalues (ant sv_closure).
type closure struct {
	fn       *svFunc
	upvalues []*upvalue
}

// macrotask is a pending timer callback (setTimeout/setInterval). goant has no
// real clock, so ordering is by (delay, seq): earlier delay first, ties broken
// by scheduling order. period > 0 marks a setInterval task (re-armed on fire).
type macrotask struct {
	fn        Value
	args      []Value
	delay     float64
	period    float64
	seq       uint64
	id        uint64
	cancelled bool
}

// ErrNotImplemented marks engine surface that is scaffolded but not yet ported.
var ErrNotImplemented = errors.New("goant: not implemented yet")

// New creates a fresh Runtime with empty pools and an initialized global object.
func New() *Runtime {
	rt := &Runtime{
		objects:  newPool[object](),
		strings:  newPool[flatString](),
		symbols:  newPool[symbol](),
		closures: newPool[closure](),
		bigints:  newPool[bigIntCell](),
		interned: make(map[string]Handle),
	}
	// Value(0) decodes as the number 0, not undefined; new.target slots must start
	// as a real undefined so "not constructing" is detectable.
	rt.pendingNewTarget = mkundef()
	rt.activeNewTarget = mkundef()
	rt.initPrototypes()
	rt.initGlobal()
	rt.initErrorBuiltin()
	rt.initFunctionBuiltin()
	// Symbol + %IteratorPrototype% must precede any builtin that installs a
	// [Symbol.iterator] method (Array/String/Collections).
	rt.initSymbolBuiltin()
	rt.initIteratorProto()
	// %AsyncFunction.prototype% / %GeneratorFunction.prototype% (proto chain +
	// Symbol.toStringTag) for async/generator function objects.
	rt.asyncFunctionProto = rt.newObject(rt.functionProto)
	rt.setStringTag(rt.asyncFunctionProto, "AsyncFunction")
	rt.generatorFuncProto = rt.newObject(rt.functionProto)
	rt.setStringTag(rt.generatorFuncProto, "GeneratorFunction")
	rt.asyncGeneratorFnProto = rt.newObject(rt.functionProto)
	rt.setStringTag(rt.asyncGeneratorFnProto, "AsyncGeneratorFunction")
	rt.initFunctionFamily()
	rt.initBuiltins()
	rt.initMath()
	rt.initObjectBuiltin()
	rt.initArrayBuiltin()
	rt.initStringBuiltin()
	rt.initNumberBuiltin()
	rt.initBigIntBuiltin()
	rt.initBooleanBuiltin()
	rt.initDateBuiltin()
	rt.initURIBuiltins()
	rt.initRegExpBuiltin()
	rt.initJSONBuiltin()
	rt.initCollections()
	rt.initWeakCollections()
	rt.initPromiseBuiltin()
	rt.initGeneratorBuiltin()
	rt.initReflectBuiltin()
	rt.initProxyBuiltin()
	rt.initTypedArrays()
	rt.initIteratorHelpers()
	rt.initAsyncIterator()
	rt.initDisposableStack()
	rt.initSharedArrayBuffer()
	rt.initAtomics()
	rt.initIntl()
	rt.initAnnexB()
	return rt
}

// initPrototypes creates the core prototype objects. Object.prototype is the
// root (null proto); the others chain to it.
func (rt *Runtime) initPrototypes() {
	rt.objectProto = rt.newObject(mknull())
	rt.functionProto = rt.newObject(rt.objectProto)
	rt.arrayProto = rt.newObject(rt.objectProto)
	rt.stringProto = rt.newObject(rt.objectProto)
	rt.numberProto = rt.newObject(rt.objectProto)
	rt.booleanProto = rt.newObject(rt.objectProto)
	rt.errorProto = rt.newObject(rt.objectProto)
}

// newPlainObject creates an object with Object.prototype (object literals).
func (rt *Runtime) newPlainObject() Value {
	if rt.objectProto == 0 {
		return rt.newObject(mknull())
	}
	return rt.newObject(rt.objectProto)
}

// initGlobal creates the global object and its predefined value properties
// (ant setup in ant.c). Constructors/builtin functions are added in Phase 4.
func (rt *Runtime) initGlobal() {
	// The global object's [[Prototype]] is Object.prototype, so bare references
	// like `hasOwnProperty` / `__defineGetter__` resolve through it.
	rt.global = rt.newObject(rt.objectProto)
	g := rt.objPtr(rt.global)
	g.defineOwn("globalThis", rt.global, attrWritable|attrConfigurable)
	// Immutable value properties (ES5 §15.1.1): non-writable, non-configurable.
	g.defineOwn("undefined", mkundef(), 0)
	g.defineOwn("NaN", mknum(math.NaN()), 0)
	g.defineOwn("Infinity", mknum(math.Inf(1)), 0)
}

// RunString parses, compiles, and evaluates source, returning the script
// completion value (ant's parse → sv_compile → sv_execute pipeline).
func (rt *Runtime) RunString(filename, src string) (Value, error) {
	rt.filename = filename
	prog, err := Parse(filename, src)
	if err != nil {
		return mkundef(), err
	}
	fn, err := rt.Compile(prog, filename, src)
	if err != nil {
		return mkundef(), err
	}
	v, err := rt.execute(fn)
	rt.runEventLoop()
	// Async conformance protocol: a test sets globalThis.__asyncTestPending and
	// clears it from asyncTestPassed(); if still pending after the microtask
	// queue drains, the async assertion never succeeded.
	if err == nil {
		if p, e := rt.getField(rt.global, "__asyncTestPending"); e == nil && rt.toBoolean(p) {
			code := 1
			return mkundef(), &ExitError{Code: code}
		}
	}
	if rt.exitCode != nil {
		return mkundef(), &ExitError{Code: *rt.exitCode}
	}
	return v, err
}
