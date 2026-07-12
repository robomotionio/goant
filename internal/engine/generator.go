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
type genMsg struct {
	value Value
	done  bool
	err   *ThrowError
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
	genDepth  int
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
	g.toGen <- genResume{kind: kind, val: val}
	m := <-g.fromGen
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
func (rt *Runtime) suspend(value Value) (resumed Value, inject *genResume) {
	g := rt.curGen
	g.fromGen <- genMsg{value: value, done: false}
	r := <-g.toGen
	if r.kind == genNext {
		return r.val, nil
	}
	rr := r
	return mkundef(), &rr
}

// ---- generator objects ----

// newGenerator creates a generator object wrapping an unstarted coroutine.
func (rt *Runtime) newGenerator(fn *svFunc, cl *closure, fnVal, thisVal Value, args []Value) Value {
	v := rt.newObject(rt.genProto)
	o := rt.objPtr(v)
	o.gen = rt.newGenState(fn, cl, fnVal, thisVal, args)
	return v
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
		return rt.genResult(m.value, m.done), nil
	})
	rt.defMethod(po, "throw", 1, drive(genThrow))

	// %GeneratorPrototype%[Symbol.iterator]() returns this.
	if rt.symIterator != 0 {
		selfIter := rt.newNativeFunc("[Symbol.iterator]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return this, nil
		})
		po.defineOwnSymbol(rt.symIterator.handle(), selfIter, attrConfigurable)
		if rt.symToStringTag != 0 {
			po.defineOwnSymbol(rt.symToStringTag.handle(), rt.newString("Generator"), attrConfigurable)
		}
	}
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
