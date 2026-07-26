package engine

import (
	"errors"
	"math"
	"path/filepath"
)

// Runtime is a single JavaScript isolate — the Go analogue of ant's ant_t
// (include/internal.h struct ant_isolate_t). It owns the non-moving pools that
// back heap Values plus the interning tables and global state.
//
// This struct grows phase by phase; Phase 0 establishes the pools and the
// public entry points. Phases 1–3 wire in the lexer/parser, object model, and
// interpreter.
type Runtime struct {
	// privClassSeq assigns a unique id to each compiled class body, so a private
	// name `#x` in one class is a distinct storage key from `#x` in another
	// (per-class private-name identity / brand check).
	privClassSeq int

	// Heap pools (TODO 0.2). Payload element types are placeholders that fill
	// in as the object model (Phase 2) and strings land.
	objects  *pool[object]
	strings  *pool[flatString]
	symbols  *pool[symbol]
	closures *pool[closure]
	bigints  *pool[bigIntCell]

	// String interning table (Phase 2): interned text -> flat-string handle.
	interned map[string]Handle

	// rootShapes holds this runtime's empty root shape per inobj limit. Every
	// shape descends from one of them through the transition tree, and adding a
	// transition mutates its parent's children map — so these are per-Runtime,
	// not package-wide. See newShapeWithLimit.
	rootShapes [inobjMaxSlots + 1]*shape

	// invWatermark is the object handle boundary for the running invocation:
	// anything below it predates the run and is shared with the next one.
	// invDirty records that the run reached below it. See invocation_dirty.go.
	invWatermark Handle
	invDirty     bool
	// invInterned lists intern-table keys added during the running invocation,
	// so a release can remove them: the table outlives the invocation and would
	// otherwise point at freed cells.
	invInterned []string

	// Thrown-value convention (ant thrown_value/thrown_exists).
	thrownValue  Value
	thrownExists bool

	// global is the global object (globalThis); top-level var/function bindings
	// and predefined globals (NaN/Infinity/undefined) live here.
	global Value

	// importMeta is the module's `import.meta` object, created lazily on first
	// access and returned identically thereafter (one Module per runner process).
	importMeta Value

	// Core prototype objects (ant isolate proto fields).
	objectProto     Value
	functionProto   Value
	arrayProto      Value
	stringProto     Value
	numberProto     Value
	bigintProto     Value
	booleanProto    Value
	errorProto      Value
	regexpProto     Value
	regexpCtor      Value  // %RegExp% constructor (SpeciesConstructor default)
	regexpProtoExec Value  // %RegExp.prototype.exec% (fast paths check for it)
	regexpLastMatch string // RegExp.lastMatch (Annex B legacy static)
	// Remaining Annex B legacy RegExp static state, updated on every successful
	// built-in match (RegExp.input/$_, lastParen/$+, leftContext/$`,
	// rightContext/$', and $1…$9).
	regexpInput           string
	regexpLastParen       string
	regexpLeftContext     string
	regexpRightContext    string
	regexpParen           [9]string
	mapProto              Value
	setProto              Value
	symbolProto           Value
	promiseProto          Value
	promiseCtor           Value // the %Promise% constructor (species fast-path check)
	genProto              Value // %GeneratorPrototype%
	iteratorProto         Value // %IteratorPrototype%
	iterHelperProto       Value // %IteratorHelperPrototype% (map/filter/… results)
	asyncIteratorProto    Value // %AsyncIteratorPrototype%
	asyncGenProto         Value // %AsyncGeneratorPrototype%
	arrayIterProto        Value // %ArrayIteratorPrototype%
	mapIterProto          Value // %MapIteratorPrototype%
	setIterProto          Value // %SetIteratorPrototype%
	stringIterProto       Value // %StringIteratorPrototype%
	regexpStrIterProto    Value // %RegExpStringIteratorPrototype%
	asyncFunctionProto    Value // %AsyncFunction.prototype%
	generatorFuncProto    Value // %GeneratorFunction.prototype%
	asyncGeneratorFnProto Value

	// TypedArray / ArrayBuffer / DataView prototypes.
	arrayBufferProto Value
	dataViewProto    Value
	typedArrayProto  Value   // %TypedArray%.prototype
	typedArrayProtos []Value // per-kind prototypes (indexed by taKind)
	typedArrayCtors  []Value // per-kind constructors (indexed by taKind), for @@species defaults

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

	// FinalizationRegistry [[Cells]], keyed by the registry object. Cleanup
	// callbacks never fire (no tracing GC), but register/unregister bookkeeping
	// and its brand check are program-observable.
	finRegistries map[*object][]finCell

	// collIterStates holds the iteration state of Set/Map iterator objects, keyed
	// by the iterator object, so a shared %<Kind>IteratorPrototype%.next (rather
	// than a per-instance closure) reads it and the missing-brand check works.
	collIterStates map[*object]*collIterState

	// arrIterStates holds the iteration state of Array/TypedArray iterator objects
	// (same shared-prototype-next / brand-check purpose as collIterStates).
	arrIterStates map[*object]*arrIterState

	// strIterStates holds the iteration state of String iterator objects (a
	// code-point snapshot + index) for a shared %StringIteratorPrototype%.next.
	strIterStates map[*object]*strIterState

	// regexpStrIterStates holds the iteration state of RegExp String iterator
	// objects (the matcher, the string, the global/unicode flags, and done) for a
	// shared %RegExpStringIteratorPrototype%.next that steps RegExpExec lazily.
	regexpStrIterStates map[*object]*regexpStrIterState

	// indexedProtoIntercept becomes true once any integer-indexed accessor or
	// non-writable indexed data property is defined anywhere (it may sit on a
	// prototype). While false, an array-index [[Set]] to an absent index can skip
	// the prototype-chain walk for an inherited interceptor and write fast storage
	// directly. Monotonic: a rare over-approximation only costs a chain walk.
	indexedProtoIntercept bool

	// Well-known symbols and the Symbol.for registry.
	symbolCounter    uint64
	symbolRegistry   map[string]Value
	symIterator      Value
	symAsyncIterator Value
	symHasInstance   Value
	symToPrimitive   Value
	symToStringTag   Value

	symMatch              Value
	symMatchAll           Value
	symReplace            Value
	symSearch             Value
	symSplit              Value
	symSpecies            Value
	symIsConcatSpreadable Value
	symUnscopables        Value
	symDispose            Value
	symAsyncDispose       Value

	// modules is the module registry, keyed by resolved absolute path; moduleDir
	// is the directory specifiers resolve against at the entry point.
	modules   map[string]*moduleRecord
	moduleDir string
	// asyncEvalOrder hands out [[AsyncEvaluationOrder]] numbers: the sequence in
	// which modules started waiting, which orders the ones a single completion
	// makes runnable together.
	asyncEvalOrder uint64
	// pendingModule is the record whose body is about to start; runFrame hands
	// it the frame's locals slice so importers keep a live view of its bindings.
	pendingModule *moduleRecord
	// shadowRealms holds the isolated Runtime behind each ShadowRealm instance.
	shadowRealms map[*object]*shadowRealm
	// wrapFailed marks that building a ShadowRealm wrapper hit an abrupt
	// completion inside the other realm, which surfaces here as a TypeError.
	wrapFailed       bool
	shadowRealmProto Value
	// moduleNamespaces marks the module namespace exotic objects. Their exports
	// are stored as accessors so reads see the live binding, but they must be
	// REPORTED as data properties, so descriptor queries consult this set.
	moduleNamespaces map[*object]bool

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

	// callerVarObj / callerWithStack carry the dynamic scope of the frame that is
	// running a direct eval: the caller's variable object (where the eval's `var`
	// declarations create bindings, see svFunc.dynamicVars) and the with-object
	// chain its free names resolve against. Set around the OpEval call only.
	callerVarObj    Value
	callerWithStack []Value
	// callerPrivEnv carries the class-evaluation tag of the frame performing a
	// direct eval, so the eval'd code's private accesses share its identity.
	callerPrivEnv *privScope
	// privEnvSeq allocates class-evaluation tags; 0 means "no class".
	privEnvSeq uint32

	// globalLex is the declarative half of the global environment record: the
	// Script-level let/const/class bindings, which are not properties of the
	// global object but are visible to every later Script and eval (globallex.go).
	globalLex map[string]*globalLexBinding

	// pendingVarObj hands the caller's variable object to the eval frame about to
	// run, so a direct eval nested inside eval code still reaches the variable
	// environment of the function that started the chain.
	pendingVarObj Value

	// pendingNewTargetProto caches the [[Prototype]] that constructWithTarget
	// already resolved from newTarget.prototype, so a native constructor's
	// newTargetProto reuses it instead of performing a second observable [[Get]]
	// of "prototype" (visible through a Proxy newTarget). 0 = not cached.
	// pendingNewTargetProtoErr caches an abrupt from that [[Get]] so it surfaces
	// when the constructor invokes newTargetProtoE at its own spec-defined
	// OrdinaryCreateFromConstructor step (after any earlier argument checks),
	// not eagerly before the body runs.
	pendingNewTargetProto    Value
	pendingNewTargetProtoErr *ThrowError

	// frameDepth tracks native call depth for the stack-overflow guard.
	frameDepth int

	// interrupt is the one piece of Runtime state written from another
	// goroutine — a host's request that the running script stop. See
	// interrupt.go. backEdges counts loop iterations between flag checks and is
	// touched only by the interpreter, so it stays a plain field.
	interrupt *interruptState
	backEdges uint32

	// frameStrict is the strictness of the currently executing JS frame, so a
	// direct eval() (native call) inherits the caller's strict mode.
	frameStrict bool

	// evalFn is the intrinsic %eval% function value. A compiled direct-eval site
	// (OpEval) checks the callee against it: only when the callee is exactly this
	// value is the call a direct eval; otherwise it degrades to an ordinary call.
	evalFn Value

	// exitCode is set by process.exit() to request termination.
	exitCode *int

	// randState is the Math.random xorshift generator state.
	randState uint64

	filename string
}

// flatString is a heap-resident flat string payload (Phase 2 strings).
type flatString struct {
	bytes   []byte
	gostr   string // lazily cached Go view of bytes; see (*Runtime).strGo
	isASCII int8   // STR_ASCII_UNKNOWN/YES/NO
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
	// home is the [[HomeObject]] of an object-literal method that reads super
	// (set when the method is defined on its object); a super-property lookup
	// starts at home's [[Prototype]]. Unset (0) for every other function.
	home Value
	// capturedWith is the `with`-object scope chain captured when a function
	// defined lexically inside a `with` block was created; the function's frame
	// seeds its withStack from this so free names still resolve against those
	// objects when the function is later called (outside the with). nil for the
	// common case of a function not nested in a with.
	capturedWith []Value
	// privEnv is the class-evaluation tag in effect where this function was
	// created — its ClassPrivateEnvironment. 0 outside any class body. It gives
	// the function's private accesses the identity of the evaluation that made
	// it, so a method from one evaluation of a class factory fails the brand
	// check of an instance from another.
	privEnv *privScope
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

		interrupt: &interruptState{},
	}
	rt.initRealm()
	return rt
}

// NewRealm creates a second realm on top of this one's value pools: a fresh
// global object with its own intrinsics, but the SAME representation for values,
// which is what lets an object made in one realm be used from the other. It is
// the relationship a V8 context has with its isolate.
//
// The pieces the spec shares per AGENT rather than per realm — the interned
// string table, the Symbol.for registry, and the well-known symbols — are
// carried over, so `Symbol.iterator` is one symbol everywhere and an object
// made in one realm is still iterable from the other.
func (rt *Runtime) NewRealm() *Runtime {
	// The Symbol.for registry is created lazily; make it exist so both realms
	// share the same map rather than the child starting with nil.
	if rt.symbolRegistry == nil {
		rt.symbolRegistry = map[string]Value{}
	}
	r := &Runtime{
		objects:        rt.objects,
		strings:        rt.strings,
		symbols:        rt.symbols,
		closures:       rt.closures,
		bigints:        rt.bigints,
		interned:       rt.interned,
		symbolRegistry: rt.symbolRegistry,
		symbolCounter:  rt.symbolCounter,
		// The host resolves a specifier the same way in either realm.
		moduleDir: rt.moduleDir,

		symIterator:           rt.symIterator,
		symAsyncIterator:      rt.symAsyncIterator,
		symHasInstance:        rt.symHasInstance,
		symToPrimitive:        rt.symToPrimitive,
		symToStringTag:        rt.symToStringTag,
		symMatch:              rt.symMatch,
		symMatchAll:           rt.symMatchAll,
		symReplace:            rt.symReplace,
		symSearch:             rt.symSearch,
		symSplit:              rt.symSplit,
		symSpecies:            rt.symSpecies,
		symIsConcatSpreadable: rt.symIsConcatSpreadable,
		symUnscopables:        rt.symUnscopables,
		symDispose:            rt.symDispose,
		symAsyncDispose:       rt.symAsyncDispose,

		// One interrupt flag per isolate, shared by every realm in it: a host
		// that cancels does not know or care which realm is currently on the
		// stack, and a realm that ignored the flag would keep the isolate alive.
		interrupt: rt.interrupt,
	}
	r.initRealm()
	return r
}

func (rt *Runtime) initRealm() {
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
	rt.installFunctionHasInstance() // needs %Symbol.hasInstance% (registered above)
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
	rt.initShadowRealmBuiltin()
	rt.initTypedArrays()
	rt.initIteratorHelpers()
	rt.initAsyncIterator()
	rt.initDisposableStack()
	rt.initSharedArrayBuffer()
	rt.initAtomics()
	rt.initIntl()
	rt.initAnnexB()
	rt.markNativeConstructors()
}

// markNativeConstructors flags the built-in functions that have a [[Construct]]
// internal method. A native carrying its own "prototype" data property is a
// constructor; prototype/static methods and accessor getters have none, so they
// stay call-only and `new`/Reflect.construct on them throws a TypeError.
// (Symbol and BigInt also own a "prototype" and DO have [[Construct]] per
// IsConstructor — it simply throws when actually invoked — so they are flagged.)
func (rt *Runtime) markNativeConstructors() {
	mark := func(v Value, name string) {
		o := rt.objPtr(v)
		if o == nil || o.native == nil || !o.flags.isCallable {
			return
		}
		// Proxy is the one standard constructor with no own "prototype" property.
		if _, ok := o.getOwn("prototype"); ok || name == "Proxy" {
			o.flags.isConstructor = true
		}
	}
	markMembers := func(container Value) {
		co := rt.objPtr(container)
		if co == nil {
			return
		}
		for _, k := range co.ownKeys() {
			if v, ok := co.getOwn(k); ok {
				mark(v, k)
			}
		}
	}
	markMembers(rt.global)
	if intlV, ok := rt.objPtr(rt.global).getOwn("Intl"); ok {
		markMembers(intlV)
	}
}

// initPrototypes creates the core prototype objects. Object.prototype is the
// root (null proto); the others chain to it.
func (rt *Runtime) initPrototypes() {
	rt.objectProto = rt.newObject(mknull())
	rt.objPtr(rt.objectProto).flags.immutableProto = true // %Object.prototype% [[Prototype]] is immutable
	rt.functionProto = rt.newObject(rt.objectProto)
	// Array.prototype is itself an Array exotic object (a length-0 array), so
	// Array.isArray(Array.prototype) is true and it carries array [[DefineOwn]].
	{
		h, obj := rt.objects.alloc()
		obj.proto = rt.objectProto
		obj.shape = rt.newShape()
		obj.typeTag = TArr
		obj.flags.extensible = true
		obj.flags.fastArray = true
		rt.arrayProto = mkval(TArr, uint64(h))
	}
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

// RunModule parses src in the Module goal (strict) and evaluates it. Preludes
// (harness scripts) should be run via RunString into the same Runtime first so
// their globals are visible. Static imports are not yet linked.
func (rt *Runtime) RunModule(filename, src string) (Value, error) {
	rt.filename = filename
	if abs, aerr := filepath.Abs(filename); aerr == nil {
		filename = abs
	}
	// Bare specifiers resolve against the entry module's directory, unless a base
	// was configured explicitly.
	if rt.moduleDir == "" {
		rt.moduleDir = filepath.Dir(filename)
	}
	prog, err := parseMode(filename, src, true, true) // a Module: strict + module goal
	if err != nil {
		return mkundef(), err
	}
	fn, err := rt.CompileModule(prog, filename, src)
	if err != nil {
		return mkundef(), err
	}
	// The entry point is a module record like any other, registered under its own
	// path so that a module importing it (directly or through a cycle) shares this
	// instance rather than loading a second copy.
	m := newModuleRecord(filename, fn)
	if rt.modules == nil {
		rt.modules = map[string]*moduleRecord{}
	}
	rt.modules[filename] = m
	for _, req := range m.requestedSpecifiers() {
		_, se, e := rt.instantiateModule(req, filename)
		if e != nil {
			return mkundef(), e
		}
		if se != nil {
			se.Filename = filename
			return mkundef(), se
		}
	}
	if se := rt.linkModule(m, map[string]bool{}); se != nil {
		// A resolution failure is an early error, not a thrown exception: no module
		// body has run. Report it like a parse error so it stays distinguishable
		// from a runtime throw.
		se.Filename = filename
		return mkundef(), se
	}
	if e := rt.hoistModuleGraph(m, map[string]bool{}); e != nil {
		return mkundef(), e
	}
	// A Module evaluates asynchronously: its body runs as an async coroutine (so
	// top-level await suspends) and the loop is driven until the completion
	// promise settles; a rejection is the module's evaluation error.
	if e := rt.evaluateModule(m); e != nil {
		return mkundef(), e
	}
	if err == nil {
		if p, e := rt.getField(rt.global, "__asyncTestPending"); e == nil && rt.toBoolean(p) {
			return mkundef(), &ExitError{Code: 1}
		}
	}
	if rt.exitCode != nil {
		return mkundef(), &ExitError{Code: *rt.exitCode}
	}
	return mkundef(), nil
}
