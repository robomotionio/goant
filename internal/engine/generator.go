package engine

// Generators and async functions (ant modules/generator.c + the async driver).
//
// A generator/async function does not run its body when called; instead it
// returns a generator object (or, for async, a promise). The body runs on a
// dedicated goroutine that ping-pongs control with the driver over two unbuffered
// channels — exactly one side is ever running, so the shared Runtime is never
// touched concurrently (each channel send/receive is a happens-before edge).
//
// `yield` (OpYield) and `await` (OpAwait) suspend the coroutine: the yielded/
// awaited value is sent to the driver and the goroutine blocks until resumed.
// Resumption carries a kind — normal (send a value back), throw (raise inside the
// generator), or return (unwind the generator) — implementing next/throw/return.
// Async functions reuse this by wrapping the coroutine in a promise-resolving
// auto-driver where `await` behaves like `yield`.

type genResumeKind int

const (
	genNext genResumeKind = iota
	genThrow
	genReturn
)

// genMsg travels coroutine -> driver: a yielded value, or terminal completion.
// await distinguishes an `await` suspension from a `yield` (the async-generator
// driver awaits the former and re-yields the latter).
type genMsg struct {
	value Value
	done  bool
	await bool
	// raw marks a yield whose value IS the IteratorResult object to hand back
	// unchanged. A sync `yield*` re-yields the inner iterator's own result object
	// (GeneratorYield(innerResult)), so its shape — including a missing `done` —
	// is observable and must not be rebuilt.
	raw bool
	// noAwait marks a yield whose value must NOT be awaited before it reaches the
	// consumer. `yield x` in an async generator awaits x (the Yield evaluation
	// does that), but `yield*` hands AsyncGeneratorYield the inner iterator's
	// value directly — so a promise yielded through `yield*` stays a promise.
	noAwait bool
	err     *ThrowError
}

// genResume travels driver -> coroutine: how to resume the suspended point.
type genResume struct {
	kind genResumeKind
	val  Value
}

type genState struct {
	fn      *svFunc
	cl      *closure
	fnVal   Value
	thisVal Value
	args    []Value

	toGen   chan genResume
	fromGen chan genMsg

	started   bool
	completed bool
	running   bool // the coroutine is mid-step: a re-entrant resume is a TypeError
	genDepth  int

	// asyncReqs is the async-generator request queue (AsyncGeneratorRequest
	// records): next/return/throw calls are serviced one at a time, since an
	// internal `await` returns control to the microtask queue mid-step.
	asyncReqs   []asyncGenReq
	asyncActive bool
}

// asyncGenReq is one pending async-generator next/return/throw call and the
// promise that settles it.
type asyncGenReq struct {
	kind genResumeKind
	val  Value
	p    Value
	po   *object
}

// newGenState allocates a fresh (unstarted) coroutine state.
func (rt *Runtime) newGenState(fn *svFunc, cl *closure, fnVal, thisVal Value, args []Value) *genState {
	return &genState{
		fn:       fn,
		cl:       cl,
		fnVal:    fnVal,
		thisVal:  thisVal,
		args:     args,
		toGen:    make(chan genResume),
		fromGen:  make(chan genMsg),
		genDepth: rt.frameDepth,
	}
}

// genRun is the coroutine goroutine body. It waits for the first drive (so the
// driver is safely blocked before it touches rt), runs the frame, and reports
// completion.
func (rt *Runtime) genRun(g *genState) {
	first := <-g.toGen
	switch first.kind {
	case genReturn:
		g.fromGen <- genMsg{value: first.val, done: true}
		return
	case genThrow:
		g.fromGen <- genMsg{err: &ThrowError{Value: first.val, rt: rt}}
		return
	}
	v, err := rt.runFrame(g.fn, g.cl, g.fnVal, g.thisVal, g.args)
	g.fromGen <- genMsg{value: v, done: true, err: err}
}

// genDrive advances a suspended coroutine one step, returning what it produced.
// It saves/restores the driver's curGen and frame depth around the handoff.
func (rt *Runtime) genDrive(g *genState, kind genResumeKind, val Value) genMsg {
	if g.running {
		// GeneratorValidate: resuming a generator that is already executing (a
		// re-entrant next/return/throw from within its own body) is a TypeError.
		return genMsg{err: rt.typeError("Generator is already running")}
	}
	if g.completed {
		switch kind {
		case genReturn:
			return genMsg{value: val, done: true}
		case genThrow:
			return genMsg{err: &ThrowError{Value: val, rt: rt}}
		default:
			return genMsg{value: mkundef(), done: true}
		}
	}
	prevGen := rt.curGen
	mainDepth := rt.frameDepth
	rt.curGen = g
	rt.frameDepth = g.genDepth
	if !g.started {
		g.started = true
		go rt.genRun(g)
	}
	g.running = true
	g.toGen <- genResume{kind: kind, val: val}
	m := <-g.fromGen
	g.running = false
	g.genDepth = rt.frameDepth
	rt.curGen = prevGen
	rt.frameDepth = mainDepth
	if m.done || m.err != nil {
		g.completed = true
	}
	return m
}

// suspend is invoked from the interpreter (OpYield/OpAwait) to hand `value` to
// the driver and block until resumed. It returns the resume value or, for a
// throw/return injection, signals the interpreter to unwind.
func (rt *Runtime) suspend(value Value, isAwait bool) (resumed Value, inject *genResume) {
	return rt.suspendMsg(genMsg{value: value, await: isAwait})
}

// suspendYieldNoAwait suspends yielding a value the async-generator driver must
// deliver without awaiting it (async `yield*`).
func (rt *Runtime) suspendYieldNoAwait(value Value) (resumed Value, inject *genResume) {
	return rt.suspendMsg(genMsg{value: value, noAwait: true})
}

// suspendRaw suspends yielding a ready-made IteratorResult object, which the
// driver returns to its caller unchanged (sync `yield*`).
func (rt *Runtime) suspendRaw(result Value) (resumed Value, inject *genResume) {
	return rt.suspendMsg(genMsg{value: result, raw: true})
}

func (rt *Runtime) suspendMsg(msg genMsg) (resumed Value, inject *genResume) {
	g := rt.curGen
	g.fromGen <- msg
	r := <-g.toGen
	if r.kind == genNext {
		return r.val, nil
	}
	rr := r
	return mkundef(), &rr
}

// ---- generator objects ----

// newGenerator creates a generator object wrapping an unstarted coroutine.
func (rt *Runtime) newGenerator(fn *svFunc, cl *closure, fnVal, thisVal Value, args []Value) (Value, *ThrowError) {
	// A generator instance inherits from its function's own .prototype (which
	// chains to %GeneratorPrototype% / %AsyncGeneratorPrototype%). Falls back to the
	// intrinsic prototype only if the function has no object .prototype.
	proto := rt.genProto
	if fn.isAsync && rt.asyncGenProto != 0 {
		proto = rt.asyncGenProto
	}
	v := rt.newObject(proto)
	o := rt.objPtr(v)
	o.gen = rt.newGenState(fn, cl, fnVal, thisVal, args)
	// FunctionDeclarationInstantiation (parameter destructuring / defaults) runs
	// eagerly at call time: drive the coroutine up to the body barrier the
	// compiler emits (OpEmpty;OpYield) right after parameter binding. A parameter
	// error therefore throws synchronously at the call, and the generator is left
	// suspended at the start of its body (the body proper runs on the first
	// resume). The tEmpty sentinel value marks the barrier yield.
	if m := rt.genDrive(o.gen, genNext, mkundef()); m.err != nil {
		return mkundef(), m.err
	}
	// OrdinaryCreateFromConstructor reads the function's .prototype AFTER
	// FunctionDeclarationInstantiation, so a parameter default that mutates
	// fn.prototype is observed (the generator inherits from the current value, or
	// the intrinsic %GeneratorPrototype% when it is no longer an object).
	if fnVal.IsObjectType() {
		if p, e := rt.getField(fnVal, "prototype"); e == nil && p.IsObjectType() {
			o.proto = p
		}
	}
	return v, nil
}

// genResult builds an IteratorResult object { value, done }.
func (rt *Runtime) genResult(value Value, done bool) Value {
	r := rt.newPlainObject()
	ro := rt.objPtr(r)
	ro.defineOwn("value", value, attrDefault)
	ro.defineOwn("done", mkbool(done), attrDefault)
	return r
}

func (rt *Runtime) initGeneratorBuiltin() {
	// %GeneratorPrototype% inherits [Symbol.iterator] from %IteratorPrototype%.
	proto := rt.newObject(rt.iteratorProto)
	rt.genProto = proto
	po := rt.objPtr(proto)

	drive := func(kind genResumeKind) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil || o.gen == nil {
				return mkundef(), rt.typeError("not a generator")
			}
			m := rt.genDrive(o.gen, kind, arg(args, 0))
			if m.err != nil {
				return mkundef(), m.err
			}
			if m.raw {
				return m.value, nil
			}
			return rt.genResult(m.value, m.done), nil
		}
	}
	rt.defMethod(po, "next", 1, drive(genNext))
	rt.defMethod(po, "return", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.gen == nil {
			return mkundef(), rt.typeError("not a generator")
		}
		if !o.gen.started || o.gen.completed {
			o.gen.completed = true
			return rt.genResult(arg(args, 0), true), nil
		}
		m := rt.genDrive(o.gen, genReturn, arg(args, 0))
		if m.err != nil {
			return mkundef(), m.err
		}
		if m.raw {
			return m.value, nil
		}
		return rt.genResult(m.value, m.done), nil
	})
	rt.defMethod(po, "throw", 1, drive(genThrow))

	// %GeneratorPrototype% inherits [Symbol.iterator] from %IteratorPrototype%
	// (it must NOT define its own); it carries only @@toStringTag "Generator".
	if rt.symToStringTag != 0 {
		po.defineOwnSymbol(rt.symToStringTag.handle(), rt.newString("Generator"), attrConfigurable)
	}
	// %GeneratorFunction.prototype%.prototype is %GeneratorPrototype% and points
	// back via constructor ({[[Writable]]:false,[[Enumerable]]:false,
	// [[Configurable]]:true}); the function-family prototypes exist by now.
	if rt.generatorFuncProto != 0 {
		rt.objPtr(rt.generatorFuncProto).defineOwn("prototype", proto, attrConfigurable)
		po.defineOwn("constructor", rt.generatorFuncProto, attrConfigurable)
	}
}

// ---- async generators ----

// asyncGenDrain services the front async-generator request when the coroutine
// is idle (one request at a time; an internal `await` returns to the microtask
// queue mid-step).
func (rt *Runtime) asyncGenDrain(g *genState) {
	if g.asyncActive || len(g.asyncReqs) == 0 {
		return
	}
	g.asyncActive = true
	req := g.asyncReqs[0]
	rt.asyncGenStep(g, req.kind, req.val)
}

// asyncGenStep services the front request. A return() completion is first run
// through AsyncGeneratorAwaitReturn — its value is awaited so return(promise)
// resolves with the unwrapped value (and a broken/rejected value rejects the
// request) — then resumes like any other completion.
func (rt *Runtime) asyncGenStep(g *genState, kind genResumeKind, val Value) {
	if kind == genReturn {
		req := g.asyncReqs[0]
		// Await = PromiseResolve(%Promise%, value): reading a native promise's
		// "constructor" is observable and its abrupt rejects the request.
		awaited, e := rt.promiseResolve(rt.promiseCtor, val)
		if e != nil {
			rt.rejectPromise(req.po, e.Value)
			rt.asyncGenFinishReq(g)
			return
		}
		onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			rt.asyncGenResume(g, genReturn, arg(a, 0))
			return mkundef(), nil
		})
		onR := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			rt.rejectPromise(req.po, arg(a, 0))
			rt.asyncGenFinishReq(g)
			return mkundef(), nil
		})
		rt.promiseThen(onF, onR, rt.objPtr(awaited))
		return
	}
	rt.asyncGenResume(g, kind, val)
}

// asyncGenResume resumes the coroutine: on a completion it settles the front
// request; on an `await`/`yield` it awaits the operand, then resumes the body
// (await) or delivers the awaited value to the consumer (yield).
func (rt *Runtime) asyncGenResume(g *genState, kind genResumeKind, val Value) {
	m := rt.genDrive(g, kind, val)
	req := g.asyncReqs[0]
	if m.err != nil {
		rt.rejectPromise(req.po, m.err.Value)
		rt.asyncGenFinishReq(g)
		return
	}
	if m.done {
		rt.resolvePromise(req.p, req.po, rt.genResult(m.value, true))
		rt.asyncGenFinishReq(g)
		return
	}
	// A `yield*` delegation hands the inner value straight to the consumer:
	// AsyncGeneratorYield performs no Await of its own.
	if m.noAwait {
		rt.resolvePromise(req.p, req.po, rt.genResult(m.value, false))
		rt.asyncGenFinishReq(g)
		return
	}
	// Both `await x` and `yield x` first Await(x); await rejection resumes the
	// body with a throw at the suspension point.
	resume := m.await
	awaited := rt.resolvedPromise(m.value)
	onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
		if resume {
			rt.asyncGenStep(g, genNext, arg(a, 0))
		} else {
			rt.resolvePromise(req.p, req.po, rt.genResult(arg(a, 0), false))
			rt.asyncGenFinishReq(g)
		}
		return mkundef(), nil
	})
	onR := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
		rt.asyncGenStep(g, genThrow, arg(a, 0))
		return mkundef(), nil
	})
	rt.promiseThen(onF, onR, rt.objPtr(awaited))
}

// asyncGenFinishReq dequeues the settled request and services the next.
func (rt *Runtime) asyncGenFinishReq(g *genState) {
	if len(g.asyncReqs) > 0 {
		g.asyncReqs = g.asyncReqs[1:]
	}
	g.asyncActive = false
	rt.asyncGenDrain(g)
}

// ---- async functions ----

// runAsync executes an async function body as a coroutine driven by promise
// resolution, returning a promise for its completion (ant async spawn).
func (rt *Runtime) runAsync(fn *svFunc, cl *closure, fnVal, thisVal Value, args []Value) Value {
	g := rt.newGenState(fn, cl, fnVal, thisVal, args)
	p, po := rt.makePromise()

	var step func(kind genResumeKind, val Value)
	step = func(kind genResumeKind, val Value) {
		m := rt.genDrive(g, kind, val)
		if m.err != nil {
			rt.rejectPromise(po, m.err.Value)
			return
		}
		if m.done {
			rt.resolvePromise(p, po, m.value)
			return
		}
		// m.value is the awaited operand: adopt it as a promise, then resume.
		awaited := rt.resolvedPromise(m.value)
		ao := rt.objPtr(awaited)
		onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			step(genNext, arg(a, 0))
			return mkundef(), nil
		})
		onR := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			step(genThrow, arg(a, 0))
			return mkundef(), nil
		})
		rt.promiseThen(onF, onR, ao)
	}
	step(genNext, mkundef())
	return p
}
