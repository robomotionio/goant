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

	// String interning table (Phase 2): interned text -> flat-string handle.
	interned map[string]Handle

	// Thrown-value convention (ant thrown_value/thrown_exists).
	thrownValue  Value
	thrownExists bool

	// global is the global object (globalThis); top-level var/function bindings
	// and predefined globals (NaN/Infinity/undefined) live here.
	global Value

	// Core prototype objects (ant isolate proto fields).
	objectProto   Value
	functionProto Value
	arrayProto    Value
	stringProto   Value
	numberProto   Value
	booleanProto  Value
	errorProto    Value
	regexpProto   Value
	mapProto      Value
	setProto      Value
	symbolProto   Value
	promiseProto  Value

	// microtasks is the promise-reaction job queue (ant microtask ring). It is
	// drained after the top-level script and after each callback that may have
	// enqueued jobs.
	microtasks []func()

	// Well-known symbols and the Symbol.for registry.
	symbolCounter    uint64
	symbolRegistry   map[string]Value
	symIterator      Value
	symAsyncIterator Value
	symHasInstance   Value
	symToPrimitive   Value
	symToStringTag   Value

	// errors holds the NativeError constructors/prototypes for internal throws.
	errors errorCtors

	// poison is the poison-pill accessor (throws) for strict caller/callee/
	// arguments (ES5 §13.2.3).
	poison Value

	// frameDepth tracks native call depth for the stack-overflow guard.
	frameDepth int

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

// ErrNotImplemented marks engine surface that is scaffolded but not yet ported.
var ErrNotImplemented = errors.New("goant: not implemented yet")

// New creates a fresh Runtime with empty pools and an initialized global object.
func New() *Runtime {
	rt := &Runtime{
		objects:  newPool[object](),
		strings:  newPool[flatString](),
		symbols:  newPool[symbol](),
		closures: newPool[closure](),
		interned: make(map[string]Handle),
	}
	rt.initPrototypes()
	rt.initGlobal()
	rt.initErrorBuiltin()
	rt.initFunctionBuiltin()
	rt.initBuiltins()
	rt.initMath()
	rt.initObjectBuiltin()
	rt.initArrayBuiltin()
	rt.initStringBuiltin()
	rt.initNumberBuiltin()
	rt.initBooleanBuiltin()
	rt.initDateBuiltin()
	rt.initURIBuiltins()
	rt.initRegExpBuiltin()
	rt.initJSONBuiltin()
	rt.initCollections()
	rt.initSymbolBuiltin()
	rt.initPromiseBuiltin()
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
	rt.global = rt.newObject(mknull())
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
	rt.drainMicrotasks()
	if rt.exitCode != nil {
		return mkundef(), &ExitError{Code: *rt.exitCode}
	}
	return v, err
}
