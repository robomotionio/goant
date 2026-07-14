package engine

// Promise (ant modules/promise.c). A Promise is an object carrying a settlement
// state machine (pending/fulfilled/rejected) plus a queue of reactions. resolve/
// reject transition a pending promise and schedule its reactions as microtasks;
// then/catch/finally derive a fresh promise. The host drains the microtask queue
// after the top-level script (Runtime.drainMicrotasks in runtime.go).

// enqueueMicrotask appends a job to the promise-reaction queue.
func (rt *Runtime) enqueueMicrotask(fn func()) {
	rt.microtasks = append(rt.microtasks, fn)
}

// drainMicrotasks runs queued jobs FIFO until the queue empties (jobs may
// enqueue further jobs).
func (rt *Runtime) drainMicrotasks() {
	for len(rt.microtasks) > 0 {
		job := rt.microtasks[0]
		rt.microtasks = rt.microtasks[1:]
		job()
	}
}

// maxMacrotasks caps timer fires per run so a self-rescheduling setInterval (or
// setTimeout chain) cannot hang the host; goant has no wall clock to advance.
const maxMacrotasks = 1 << 20

// runEventLoop drains microtasks, then fires timers one at a time in (delay,
// seq) order — draining microtasks after each — until no timers remain. This is
// the host event loop for setTimeout/setInterval under a virtual clock.
func (rt *Runtime) runEventLoop() {
	rt.drainMicrotasks()
	for fired := 0; fired < maxMacrotasks; fired++ {
		// Pick the earliest live task by (delay, seq).
		best := -1
		for i := range rt.macrotasks {
			t := &rt.macrotasks[i]
			if t.cancelled {
				continue
			}
			if best < 0 {
				best = i
				continue
			}
			b := &rt.macrotasks[best]
			if t.delay < b.delay || (t.delay == b.delay && t.seq < b.seq) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		t := rt.macrotasks[best]
		if t.period > 0 {
			// setInterval: re-arm with a fresh seq so siblings interleave fairly.
			rt.timerSeq++
			rt.macrotasks[best].delay = t.delay + t.period
			rt.macrotasks[best].seq = rt.timerSeq
		} else {
			rt.macrotasks = append(rt.macrotasks[:best], rt.macrotasks[best+1:]...)
		}
		if rt.exitCode != nil {
			return
		}
		rt.callValue(t.fn, mkundef(), t.args)
		rt.drainMicrotasks()
	}
}

// scheduleTimer enqueues a timer callback and returns its numeric id.
func (rt *Runtime) scheduleTimer(fn Value, delay, period float64, args []Value) Value {
	rt.timerSeq++
	id := rt.timerSeq
	if delay < 0 || delay != delay {
		delay = 0
	}
	rt.macrotasks = append(rt.macrotasks, macrotask{
		fn: fn, args: args, delay: delay, period: period, seq: id, id: id,
	})
	return mknum(float64(id))
}

// cancelTimer marks a scheduled timer cancelled (clearTimeout/clearInterval).
func (rt *Runtime) cancelTimer(id Value) {
	if id.Type() != TNum {
		return
	}
	n := uint64(id.Number())
	for i := range rt.macrotasks {
		if rt.macrotasks[i].id == n {
			rt.macrotasks[i].cancelled = true
		}
	}
}

// isPromise reports whether v is a promise object (carries settlement state).
func (rt *Runtime) isPromise(v Value) bool {
	o := rt.objPtr(v)
	return o != nil && o.promise != nil
}

// makePromise allocates a fresh pending promise with Promise.prototype.
func (rt *Runtime) makePromise() (Value, *object) {
	v := rt.newObject(rt.promiseProto)
	o := rt.objPtr(v)
	o.promise = &promiseState{state: 0}
	return v, o
}

// newPromiseCapability implements NewPromiseCapability(C): construct a promise
// via `new C(executor)` and capture the resolve/reject functions the executor
// receives, so Promise.all/race/etc. produce a promise of the subclass C.
func (rt *Runtime) newPromiseCapability(C Value) (promise, resolve, reject Value, err *ThrowError) {
	resolve, reject = mkundef(), mkundef()
	executor := rt.newNativeFunc("", 2, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
		// GetCapabilitiesExecutor: once either slot is populated, a further
		// invocation that would overwrite it is a TypeError.
		if !resolve.IsUndefined() || !reject.IsUndefined() {
			return mkundef(), rt.typeError("Promise executor has already supplied resolve/reject functions")
		}
		resolve, reject = arg(a, 0), arg(a, 1)
		return mkundef(), nil
	})
	p, e := rt.construct(C, []Value{executor})
	if e != nil {
		return mkundef(), mkundef(), mkundef(), e
	}
	if !rt.isCallable(resolve) || !rt.isCallable(reject) {
		return mkundef(), mkundef(), mkundef(), rt.typeError("Promise resolve or reject function is not callable")
	}
	return p, resolve, reject, nil
}

// fulfillPromise settles o as fulfilled and schedules its reactions.
func (rt *Runtime) fulfillPromise(o *object, value Value) {
	st := o.promise
	if st.state != 0 {
		return
	}
	st.state = 1
	st.value = value
	rt.flushReactions(st)
}

// rejectPromise settles o as rejected and schedules its reactions.
func (rt *Runtime) rejectPromise(o *object, reason Value) {
	st := o.promise
	if st.state != 0 {
		return
	}
	st.state = 2
	st.value = reason
	rt.flushReactions(st)
}

func (rt *Runtime) flushReactions(st *promiseState) {
	rs := st.handlers
	st.handlers = nil
	for _, r := range rs {
		rt.enqueueMicrotask(func() { rt.runReaction(r, st.state, st.value) })
	}
}

// resolvePromise implements the [[Resolve]] operation: a thenable is adopted,
// otherwise the promise fulfills with value. p is o's own Value (for the
// self-resolution cycle check).
func (rt *Runtime) resolvePromise(p Value, o *object, value Value) {
	if o.promise.state != 0 {
		return
	}
	if value == p {
		rt.rejectPromise(o, rt.makeError(rt.errors.typeProto, "TypeError", "Chaining cycle detected for promise"))
		return
	}
	if value.IsObjectType() {
		then, e := rt.getField(value, "then")
		if e != nil {
			rt.rejectPromise(o, e.Value)
			return
		}
		if rt.isCallable(then) {
			rt.enqueueMicrotask(func() { rt.runThenableJob(p, o, value, then) })
			return
		}
	}
	rt.fulfillPromise(o, value)
}

// runThenableJob drives an adopted thenable, wiring its resolve/reject back into
// promise o (with the first-settle-wins guard from the spec).
func (rt *Runtime) runThenableJob(p Value, o *object, thenable, then Value) {
	done := false
	res := rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if done {
			return mkundef(), nil
		}
		done = true
		rt.resolvePromise(p, o, arg(args, 0))
		return mkundef(), nil
	})
	rej := rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if done {
			return mkundef(), nil
		}
		done = true
		rt.rejectPromise(o, arg(args, 0))
		return mkundef(), nil
	})
	if _, e := rt.callValue(then, thenable, []Value{res, rej}); e != nil && !done {
		done = true
		rt.rejectPromise(o, e.Value)
	}
}

// runReaction executes one settled reaction: apply the matching handler (or pass
// the value/reason straight through) and settle the derived promise.
func (rt *Runtime) runReaction(r promiseReaction, state int, value Value) {
	// Settle the derived promise: through the species capability functions when
	// present, otherwise directly on the ordinary native promise.
	settleFulfil := func(v Value) {
		if rt.isCallable(r.capResolve) {
			rt.callValue(r.capResolve, mkundef(), []Value{v})
		} else {
			rt.resolvePromise(r.result, rt.objPtr(r.result), v)
		}
	}
	settleReject := func(v Value) {
		if rt.isCallable(r.capReject) {
			rt.callValue(r.capReject, mkundef(), []Value{v})
		} else {
			rt.rejectPromise(rt.objPtr(r.result), v)
		}
	}
	handler := r.onFulfilled
	if state == 2 {
		handler = r.onRejected
	}
	if !rt.isCallable(handler) {
		if state == 1 {
			settleFulfil(value)
		} else {
			settleReject(value)
		}
		return
	}
	out, e := rt.callValue(handler, mkundef(), []Value{value})
	if e != nil {
		settleReject(e.Value)
		return
	}
	settleFulfil(out)
}

// promiseThen registers a reaction on o and returns a fresh native derived
// promise.
func (rt *Runtime) promiseThen(onF, onR Value, o *object) Value {
	dp, _ := rt.makePromise()
	return rt.promiseThenCap(onF, onR, o, dp, mkundef(), mkundef())
}

// promiseThenCap registers a reaction whose derived promise `result` is settled
// via capResolve/capReject when they are callable (a species capability), or
// directly otherwise.
func (rt *Runtime) promiseThenCap(onF, onR Value, o *object, result, capResolve, capReject Value) Value {
	r := promiseReaction{onFulfilled: onF, onRejected: onR, result: result, capResolve: capResolve, capReject: capReject}
	st := o.promise
	switch st.state {
	case 0:
		st.handlers = append(st.handlers, r)
	default:
		st.handled = true
		state, value := st.state, st.value
		rt.enqueueMicrotask(func() { rt.runReaction(r, state, value) })
	}
	return result
}

// promiseThenSpecies implements Promise.prototype.then's derived-promise
// creation: the ordinary native promise fast-path unless a @@species constructor
// is in play, in which case NewPromiseCapability(species) supplies the result.
func (rt *Runtime) promiseThenSpecies(this Value, o *object, onF, onR Value) (Value, *ThrowError) {
	C, e := rt.speciesConstructor(this, rt.promiseCtor)
	if e != nil {
		return mkundef(), e
	}
	if rt.promiseCtor == 0 || C == rt.promiseCtor {
		return rt.promiseThen(onF, onR, o), nil
	}
	cap, resolveFn, rejectFn, e := rt.newPromiseCapability(C)
	if e != nil {
		return mkundef(), e
	}
	return rt.promiseThenCap(onF, onR, o, cap, resolveFn, rejectFn), nil
}

// speciesConstructor implements SpeciesConstructor(O, defaultConstructor): the
// constructor to use for a derived object, honouring O.constructor[@@species].
func (rt *Runtime) speciesConstructor(obj, defaultCtor Value) (Value, *ThrowError) {
	c, e := rt.getField(obj, "constructor")
	if e != nil {
		return mkundef(), e
	}
	if c.IsUndefined() {
		return defaultCtor, nil
	}
	if !c.IsObjectType() {
		return mkundef(), rt.typeError("constructor is not an object")
	}
	if rt.symSpecies == 0 {
		return c, nil
	}
	s, e := rt.getElement(c, rt.symSpecies)
	if e != nil {
		return mkundef(), e
	}
	if s.IsNullish() {
		return defaultCtor, nil
	}
	if !rt.isCallable(s) {
		return mkundef(), rt.typeError("@@species is not a constructor")
	}
	return s, nil
}

// resolvedPromise returns a promise already resolved with value (adopting a
// promise value directly when possible).
func (rt *Runtime) resolvedPromise(value Value) Value {
	if rt.isPromise(value) {
		return value
	}
	p, o := rt.makePromise()
	rt.resolvePromise(p, o, value)
	return p
}

func (rt *Runtime) rejectedPromise(reason Value) Value {
	p, o := rt.makePromise()
	rt.rejectPromise(o, reason)
	return p
}

// promiseResolve implements the PromiseResolve(C, x) abstract operation: if x is
// already a promise whose "constructor" is C it is returned as-is; otherwise a
// fresh capability is built with `new C(executor)` and resolved with x, so a
// subclass's constructor drives the result.
func (rt *Runtime) promiseResolve(C, x Value) (Value, *ThrowError) {
	if rt.isPromise(x) {
		xctor, e := rt.getField(x, "constructor")
		if e != nil {
			return mkundef(), e
		}
		if xctor == C {
			return x, nil
		}
	}
	cap, resolve, _, e := rt.newPromiseCapability(C)
	if e != nil {
		return mkundef(), e
	}
	if _, e := rt.callValue(resolve, mkundef(), []Value{x}); e != nil {
		return mkundef(), e
	}
	return cap, nil
}

func (rt *Runtime) initPromiseBuiltin() {
	proto := rt.newObject(rt.objectProto)
	rt.promiseProto = proto
	po := rt.objPtr(proto)

	rt.defMethod(po, "then", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.promise == nil {
			return mkundef(), rt.typeError("Promise.prototype.then called on incompatible receiver")
		}
		return rt.promiseThenSpecies(this, o, arg(args, 0), arg(args, 1))
	})
	rt.defMethod(po, "catch", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Promise.prototype.catch(onRejected) is Invoke(this, "then",
		// «undefined, onRejected»); it works on any thenable, not only native
		// promises.
		return rt.invokeThen(this, mkundef(), arg(args, 0))
	})
	rt.defMethod(po, "finally", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Promise.prototype.finally called on a non-object")
		}
		// C is SpeciesConstructor(this, %Promise%); onFinally's result is turned
		// into a promise via C.resolve and awaited before the value/reason passes
		// through (ES2018 Promise.prototype.finally).
		C, e := rt.speciesConstructor(this, rt.promiseCtor)
		if e != nil {
			return mkundef(), e
		}
		onFinally := arg(args, 0)
		thenFinally, catchFinally := onFinally, onFinally
		if rt.isCallable(onFinally) {
			thenFinally = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				value := arg(a, 0)
				result, e := rt.callValue(onFinally, mkundef(), nil)
				if e != nil {
					return mkundef(), e
				}
				p, e := rt.promiseResolve(C, result)
				if e != nil {
					return mkundef(), e
				}
				thunk := rt.newNativeFunc("", 0, func(rt *Runtime, _ Value, _ []Value) (Value, *ThrowError) { return value, nil })
				return rt.invokeThen1(p, thunk)
			})
			catchFinally = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				reason := arg(a, 0)
				result, e := rt.callValue(onFinally, mkundef(), nil)
				if e != nil {
					return mkundef(), e
				}
				p, e := rt.promiseResolve(C, result)
				if e != nil {
					return mkundef(), e
				}
				thrower := rt.newNativeFunc("", 0, func(rt *Runtime, _ Value, _ []Value) (Value, *ThrowError) {
					return mkundef(), &ThrowError{Value: reason, rt: rt}
				})
				return rt.invokeThen1(p, thrower)
			})
		}
		return rt.invokeThen(this, thenFinally, catchFinally)
	})

	ctor := rt.newNativeFunc("Promise", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Promise() must be invoked with `new` (NewTarget defined); a plain call —
		// including Promise.call(anObject, ...) — is a TypeError before the
		// executor is examined.
		if !rt.constructing() {
			return mkundef(), rt.typeError("Constructor Promise requires 'new'")
		}
		executor := arg(args, 0)
		if !rt.isCallable(executor) {
			return mkundef(), rt.typeError("Promise resolver is not a function")
		}
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("Constructor Promise requires 'new'")
		}
		o.promise = &promiseState{state: 0}
		p := this
		done := false
		resolve := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			if done {
				return mkundef(), nil
			}
			done = true
			rt.resolvePromise(p, o, arg(a, 0))
			return mkundef(), nil
		})
		reject := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			if done {
				return mkundef(), nil
			}
			done = true
			rt.rejectPromise(o, arg(a, 0))
			return mkundef(), nil
		})
		if _, e := rt.callValue(executor, mkundef(), []Value{resolve, reject}); e != nil && !done {
			done = true
			rt.rejectPromise(o, e.Value)
		}
		return this, nil
	})
	cobj := rt.objPtr(ctor)
	rt.promiseCtor = ctor
	cobj.defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	if rt.symToStringTag != 0 {
		po.defineOwnSymbol(rt.symToStringTag.handle(), rt.newString("Promise"), attrConfigurable)
	}

	rt.defMethod(cobj, "resolve", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Promise.resolve(x): C is the this value and must be an Object.
		// PromiseResolve returns x unchanged only when x is a promise whose own
		// "constructor" is C, so a reassigned constructor forces a fresh promise.
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Promise.resolve called on a non-object")
		}
		return rt.promiseResolve(this, arg(args, 0))
	})
	rt.defMethod(cobj, "withResolvers", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// C is the this value; the promise/resolve/reject come from
		// NewPromiseCapability(C) so a subclass drives the result.
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Promise.withResolvers called on a non-object")
		}
		p, resolve, reject, e := rt.newPromiseCapability(this)
		if e != nil {
			return mkundef(), e
		}
		res := rt.newPlainObject()
		ro := rt.objPtr(res)
		ro.defineOwn("promise", p, attrDefault)
		ro.defineOwn("resolve", resolve, attrDefault)
		ro.defineOwn("reject", reject, attrDefault)
		return res, nil
	})
	rt.defMethod(cobj, "reject", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Promise.reject(r): build a capability from C = this and call its reject.
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Promise.reject called on a non-object")
		}
		if this == rt.promiseCtor {
			return rt.rejectedPromise(arg(args, 0)), nil
		}
		cap, _, reject, e := rt.newPromiseCapability(this)
		if e != nil {
			return mkundef(), e
		}
		if _, e := rt.callValue(reject, mkundef(), []Value{arg(args, 0)}); e != nil {
			return mkundef(), e
		}
		return cap, nil
	})
	rt.defMethod(cobj, "all", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.promiseAll(this, arg(args, 0), false)
	})
	rt.defMethod(cobj, "allSettled", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.promiseAll(this, arg(args, 0), true)
	})
	rt.defMethod(cobj, "allKeyed", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Promise.allKeyed called on a non-object")
		}
		return rt.promiseAllKeyed(this, arg(args, 0), false)
	})
	rt.defMethod(cobj, "allSettledKeyed", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Promise.allSettledKeyed called on a non-object")
		}
		return rt.promiseAllKeyed(this, arg(args, 0), true)
	})
	rt.defMethod(cobj, "race", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.promiseRace(this, arg(args, 0), false)
	})
	rt.defMethod(cobj, "any", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.promiseRace(this, arg(args, 0), true)
	})
	rt.defMethod(cobj, "try", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Promise.try(fn, ...args): build a capability from C = this, run fn
		// synchronously, then resolve with its result or reject with a thrown
		// error (ES2025).
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Promise.try called on a non-object")
		}
		cb := arg(args, 0)
		var extra []Value
		if len(args) > 1 {
			extra = args[1:]
		}
		p, resolve, reject, e := rt.newPromiseCapability(this)
		if e != nil {
			return mkundef(), e
		}
		res, ce := rt.callValue(cb, mkundef(), extra)
		if ce != nil {
			rt.callValue(reject, mkundef(), []Value{ce.Value})
		} else {
			rt.callValue(resolve, mkundef(), []Value{res})
		}
		return p, nil
	})

	rt.defSpeciesGetter(ctor)
	rt.defGlobal("Promise", ctor)
}

// promiseResolveFn reads the receiver's own "resolve" (Get(C, "resolve"), which
// must be callable) — used by the combinators to turn each element into a
// promise via C.resolve, so a subclass's overridden resolve is honored. On
// failure it rejects the result promise and returns ok=false.
func (rt *Runtime) promiseResolveFn(C, rejectFn Value) (Value, bool) {
	r, e := rt.getField(C, "resolve")
	if e != nil {
		rt.callValue(rejectFn, mkundef(), []Value{e.Value})
		return mkundef(), false
	}
	if !rt.isCallable(r) {
		rt.callValue(rejectFn, mkundef(), []Value{rt.typeError("Promise.resolve is not a function").Value})
		return mkundef(), false
	}
	return r, true
}

// iterateCombinator drives iter with PerformPromiseAll/Race semantics: it calls
// dispatch(value) for each element. An abrupt completion from IteratorStep /
// IteratorComplete (the "done" getter) / IteratorValue (the "value" getter)
// aborts WITHOUT closing the iterator (its [[Done]] is true or the step failed),
// whereas an abrupt completion from dispatch closes it (the value was obtained,
// so [[Done]] is false). Returns the first abrupt completion, or nil when the
// iterator is exhausted.
func (rt *Runtime) iterateCombinator(iter Value, dispatch func(v Value) *ThrowError) *ThrowError {
	for {
		nextFn, e := rt.getField(iter, "next")
		if e != nil {
			return e
		}
		r, e := rt.callValue(nextFn, iter, nil)
		if e != nil {
			return e // IteratorStep threw: no close
		}
		if !r.IsObjectType() {
			return rt.typeError("iterator result is not an object")
		}
		d, e := rt.getField(r, "done") // IteratorComplete
		if e != nil {
			return e // no close
		}
		if rt.toBoolean(d) {
			return nil // exhausted
		}
		v, e := rt.getField(r, "value") // IteratorValue
		if e != nil {
			return e // no close
		}
		if de := dispatch(v); de != nil {
			rt.iteratorClose(iter) // abrupt while dispatching: close (errors swallowed)
			return de
		}
	}
}

// invokeThen performs Invoke(this, "then", [onF, onR]) and returns its result —
// used by catch/finally, which operate on any thenable receiver.
func (rt *Runtime) invokeThen(this, onF, onR Value) (Value, *ThrowError) {
	thenFn, e := rt.getField(this, "then")
	if e != nil {
		return mkundef(), e
	}
	return rt.callValue(thenFn, this, []Value{onF, onR})
}

// invokeThen1 performs Invoke(this, "then", « onF ») with a single argument, as
// the finally then/catch closures do (they must not pass a second undefined,
// which is observable via arguments.length).
func (rt *Runtime) invokeThen1(this, onF Value) (Value, *ThrowError) {
	thenFn, e := rt.getField(this, "then")
	if e != nil {
		return mkundef(), e
	}
	return rt.callValue(thenFn, this, []Value{onF})
}

// invokePromiseThen performs Invoke(p, "then", [onF, onR]) observably (used per
// element by the combinators so a thenable's `then` is read and called).
func (rt *Runtime) invokePromiseThen(p, onF, onR Value) *ThrowError {
	thenFn, e := rt.getField(p, "then")
	if e != nil {
		return e
	}
	_, e = rt.callValue(thenFn, p, []Value{onF, onR})
	return e
}

// promiseAll implements Promise.all / Promise.allSettled. The result promise is
// built from the receiver C (NewPromiseCapability), so a Promise subclass gets a
// subclass instance back; each element goes through C.resolve(...).then(...),
// and every resolve/reject element function fulfills at most once. The iterable
// is walked lazily so the iterator is closed on any abrupt completion raised
// while dispatching an element (PerformPromiseAll + IteratorClose).
func (rt *Runtime) promiseAll(C, iterable Value, settled bool) (Value, *ThrowError) {
	result, resolveFn, rejectFn, e := rt.newPromiseCapability(C)
	if e != nil {
		return mkundef(), e
	}
	promiseResolve, ok := rt.promiseResolveFn(C, rejectFn)
	if !ok {
		return result, nil
	}
	iter, ie := rt.getSyncIterator(iterable)
	if ie != nil {
		rt.callValue(rejectFn, mkundef(), []Value{ie.Value}) // IfAbruptRejectPromise (GetIterator)
		return result, nil
	}
	results := rt.newArray()
	ra := rt.objPtr(results)
	remaining := 1 // the extra count is retired after the iteration completes
	tryFinish := func() *ThrowError {
		remaining--
		if remaining == 0 {
			if _, e := rt.callValue(resolveFn, mkundef(), []Value{results}); e != nil {
				return e
			}
		}
		return nil
	}
	idx := 0
	perr := rt.iterateCombinator(iter, func(v Value) *ThrowError {
		i := idx
		idx++
		rt.arraySet(ra, uint32(i), mkundef())
		nextP, ce := rt.callValue(promiseResolve, C, []Value{v})
		if ce != nil {
			return ce // closes the iterator, then rejects below
		}
		remaining++
		alreadyCalled := false
		onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			if alreadyCalled {
				return mkundef(), nil
			}
			alreadyCalled = true
			if settled {
				o := rt.newPlainObject()
				oo := rt.objPtr(o)
				oo.defineOwn("status", rt.newString("fulfilled"), attrDefault)
				oo.defineOwn("value", arg(a, 0), attrDefault)
				rt.arraySet(ra, uint32(i), o)
			} else {
				rt.arraySet(ra, uint32(i), arg(a, 0))
			}
			return mkundef(), tryFinish()
		})
		var onR Value
		if settled {
			onR = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				if alreadyCalled {
					return mkundef(), nil
				}
				alreadyCalled = true
				o := rt.newPlainObject()
				oo := rt.objPtr(o)
				oo.defineOwn("status", rt.newString("rejected"), attrDefault)
				oo.defineOwn("reason", arg(a, 0), attrDefault)
				rt.arraySet(ra, uint32(i), o)
				return mkundef(), tryFinish()
			})
		} else {
			onR = rejectFn // the shared reject settles the result on the first rejection
		}
		if te := rt.invokePromiseThen(nextP, onF, onR); te != nil {
			return te // closes the iterator, then rejects below
		}
		return nil
	})
	if perr != nil {
		rt.callValue(rejectFn, mkundef(), []Value{perr.Value})
		return result, nil
	}
	if fe := tryFinish(); fe != nil {
		rt.callValue(rejectFn, mkundef(), []Value{fe.Value}) // IfAbruptRejectPromise
	}
	return result, nil
}

// objectOwnKeys returns obj's own property keys (strings then symbols) as a
// slice of key Values, dispatching the ownKeys trap for a Proxy. It is the
// [[OwnPropertyKeys]] source for the keyed Promise combinators.
func (rt *Runtime) objectOwnKeys(obj Value) ([]Value, *ThrowError) {
	o := rt.objPtr(obj)
	if o == nil {
		return nil, nil
	}
	if o.proxy != nil {
		return rt.proxyOwnKeys(o.proxy)
	}
	var keys []Value
	for _, k := range o.ownKeys() {
		keys = append(keys, rt.newString(k))
	}
	for _, off := range o.ownSymbolKeys() {
		keys = append(keys, mkval(TSymbol, uint64(off)))
	}
	return keys, nil
}

// promiseAllKeyed implements Promise.allKeyed / Promise.allSettledKeyed: like
// Promise.all/allSettled but iterating the input object's own enumerable keys
// (strings and symbols, in [[OwnPropertyKeys]] order) and fulfilling with a
// null-prototype object mapping each key to its resolved value (or, for the
// settled form, to a { status, value|reason } record).
func (rt *Runtime) promiseAllKeyed(C, obj Value, settled bool) (Value, *ThrowError) {
	result, resolveFn, rejectFn, e := rt.newPromiseCapability(C)
	if e != nil {
		return mkundef(), e
	}
	promiseResolve, ok := rt.promiseResolveFn(C, rejectFn)
	if !ok {
		return result, nil
	}
	if !obj.IsObjectLike() {
		rt.callValue(rejectFn, mkundef(), []Value{rt.typeError("Promise.allKeyed called on a non-object").Value})
		return result, nil
	}
	keys, ke := rt.objectOwnKeys(obj)
	if ke != nil {
		rt.callValue(rejectFn, mkundef(), []Value{ke.Value}) // IfAbruptRejectPromise
		return result, nil
	}
	resObj := rt.newObject(mknull())
	ro := rt.objPtr(resObj)
	setResult := func(key, v Value) {
		if key.IsSymbol() {
			ro.defineOwnSymbol(key.handle(), v, attrDefault)
			return
		}
		name, _ := rt.propKeyString(key)
		ro.defineOwn(name, v, attrDefault)
	}
	remaining := 1
	// tryFinish decrements the pending count and, on reaching zero, resolves the
	// capability with the accumulated result object. Calling the capability's
	// resolve is `? Call` in the spec, so a throw propagates: the synchronous
	// final call rejects the capability, and a call from an element function
	// surfaces as that reaction's rejection.
	tryFinish := func() *ThrowError {
		remaining--
		if remaining == 0 {
			if _, e := rt.callValue(resolveFn, mkundef(), []Value{resObj}); e != nil {
				return e
			}
		}
		return nil
	}
	for _, key := range keys {
		en, exists, de := rt.ownKeyEnumerable(obj, key)
		if de != nil {
			rt.callValue(rejectFn, mkundef(), []Value{de.Value})
			return result, nil
		}
		if !exists || !en {
			continue
		}
		val, ge := rt.getElement(obj, key)
		if ge != nil {
			rt.callValue(rejectFn, mkundef(), []Value{ge.Value})
			return result, nil
		}
		k := key
		setResult(k, mkundef()) // establish key order before settlement
		remaining++
		nextP, ce := rt.callValue(promiseResolve, C, []Value{val})
		if ce != nil {
			rt.callValue(rejectFn, mkundef(), []Value{ce.Value})
			return result, nil
		}
		alreadyCalled := false
		onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			if alreadyCalled {
				return mkundef(), nil
			}
			alreadyCalled = true
			if settled {
				rec := rt.newPlainObject()
				recO := rt.objPtr(rec)
				recO.defineOwn("status", rt.newString("fulfilled"), attrDefault)
				recO.defineOwn("value", arg(a, 0), attrDefault)
				setResult(k, rec)
			} else {
				setResult(k, arg(a, 0))
			}
			return mkundef(), tryFinish()
		})
		var onR Value
		if settled {
			onR = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				if alreadyCalled {
					return mkundef(), nil
				}
				alreadyCalled = true
				rec := rt.newPlainObject()
				recO := rt.objPtr(rec)
				recO.defineOwn("status", rt.newString("rejected"), attrDefault)
				recO.defineOwn("reason", arg(a, 0), attrDefault)
				setResult(k, rec)
				return mkundef(), tryFinish()
			})
		} else {
			onR = rejectFn // first rejection settles the result promise
		}
		if te := rt.invokePromiseThen(nextP, onF, onR); te != nil {
			rt.callValue(rejectFn, mkundef(), []Value{te.Value})
			return result, nil
		}
	}
	if fe := tryFinish(); fe != nil {
		rt.callValue(rejectFn, mkundef(), []Value{fe.Value}) // IfAbruptRejectPromise
	}
	return result, nil
}

// promiseRace implements Promise.race / Promise.any, building the result promise
// from the receiver C (NewPromiseCapability) for subclass support. The iterable
// is walked lazily and the iterator is closed on any abrupt completion raised
// while dispatching an element.
func (rt *Runtime) promiseRace(C, iterable Value, any bool) (Value, *ThrowError) {
	result, resolveFn, rejectFn, e := rt.newPromiseCapability(C)
	if e != nil {
		return mkundef(), e
	}
	promiseResolve, ok := rt.promiseResolveFn(C, rejectFn)
	if !ok {
		return result, nil
	}
	iter, ie := rt.getSyncIterator(iterable)
	if ie != nil {
		rt.callValue(rejectFn, mkundef(), []Value{ie.Value}) // IfAbruptRejectPromise (GetIterator)
		return result, nil
	}
	errs := rt.newArray()
	ea := rt.objPtr(errs)
	remaining := 1 // for `any`: retired after the iteration completes
	rejectAggregate := func() *ThrowError {
		agg := rt.makeError(rt.errors.aggProto, "AggregateError", "All promises were rejected")
		if eo := rt.objPtr(agg); eo != nil {
			eo.defineOwn("errors", errs, attrWritable|attrConfigurable)
		}
		_, e := rt.callValue(rejectFn, mkundef(), []Value{agg})
		return e
	}
	idx := 0
	perr := rt.iterateCombinator(iter, func(v Value) *ThrowError {
		i := idx
		idx++
		nextP, ce := rt.callValue(promiseResolve, C, []Value{v})
		if ce != nil {
			return ce // closes the iterator, then rejects below
		}
		var onF, onR Value
		if any {
			onF = resolveFn // the shared resolve settles on the first fulfillment
			remaining++
			alreadyCalled := false
			onR = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				if alreadyCalled {
					return mkundef(), nil
				}
				alreadyCalled = true
				rt.arraySet(ea, uint32(i), arg(a, 0))
				remaining--
				if remaining == 0 {
					return mkundef(), rejectAggregate()
				}
				return mkundef(), nil
			})
		} else {
			// race: the shared resolve/reject settle on the first settlement.
			onF, onR = resolveFn, rejectFn
		}
		if te := rt.invokePromiseThen(nextP, onF, onR); te != nil {
			return te // closes the iterator, then rejects below
		}
		return nil
	})
	if perr != nil {
		rt.callValue(rejectFn, mkundef(), []Value{perr.Value})
		return result, nil
	}
	if any {
		remaining--
		if remaining == 0 {
			rejectAggregate() // no elements, or all synchronously rejected
		}
	}
	return result, nil
}
