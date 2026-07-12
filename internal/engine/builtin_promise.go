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
	dp := rt.objPtr(r.result)
	handler := r.onFulfilled
	if state == 2 {
		handler = r.onRejected
	}
	if !rt.isCallable(handler) {
		if state == 1 {
			rt.resolvePromise(r.result, dp, value)
		} else {
			rt.rejectPromise(dp, value)
		}
		return
	}
	out, e := rt.callValue(handler, mkundef(), []Value{value})
	if e != nil {
		rt.rejectPromise(dp, e.Value)
		return
	}
	rt.resolvePromise(r.result, dp, out)
}

// promiseThen registers a reaction on o and returns the derived promise.
func (rt *Runtime) promiseThen(onF, onR Value, o *object) Value {
	dp, _ := rt.makePromise()
	r := promiseReaction{onFulfilled: onF, onRejected: onR, result: dp}
	st := o.promise
	switch st.state {
	case 0:
		st.handlers = append(st.handlers, r)
	default:
		st.handled = true
		state, value := st.state, st.value
		rt.enqueueMicrotask(func() { rt.runReaction(r, state, value) })
	}
	return dp
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

func (rt *Runtime) initPromiseBuiltin() {
	proto := rt.newObject(rt.objectProto)
	rt.promiseProto = proto
	po := rt.objPtr(proto)

	rt.defMethod(po, "then", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.promise == nil {
			return mkundef(), rt.typeError("Promise.prototype.then called on incompatible receiver")
		}
		return rt.promiseThen(arg(args, 0), arg(args, 1), o), nil
	})
	rt.defMethod(po, "catch", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.promise == nil {
			return mkundef(), rt.typeError("Promise.prototype.catch called on incompatible receiver")
		}
		return rt.promiseThen(mkundef(), arg(args, 0), o), nil
	})
	rt.defMethod(po, "finally", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.promise == nil {
			return mkundef(), rt.typeError("Promise.prototype.finally called on incompatible receiver")
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return rt.promiseThen(cb, cb, o), nil
		}
		onF := rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			v := arg(args, 0)
			if _, e := rt.callValue(cb, mkundef(), nil); e != nil {
				return mkundef(), e
			}
			return v, nil
		})
		onR := rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if _, e := rt.callValue(cb, mkundef(), nil); e != nil {
				return mkundef(), e
			}
			return mkundef(), &ThrowError{Value: arg(args, 0), rt: rt}
		})
		return rt.promiseThen(onF, onR, o), nil
	})

	ctor := rt.newNativeFunc("Promise", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
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
	cobj.defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	if rt.symToStringTag != 0 {
		po.defineOwnSymbol(rt.symToStringTag.handle(), rt.newString("Promise"), attrConfigurable)
	}

	rt.defMethod(cobj, "resolve", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.resolvedPromise(arg(args, 0)), nil
	})
	rt.defMethod(cobj, "withResolvers", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		p, o := rt.makePromise()
		resolve := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			rt.resolvePromise(p, o, arg(a, 0))
			return mkundef(), nil
		})
		reject := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			rt.rejectPromise(o, arg(a, 0))
			return mkundef(), nil
		})
		res := rt.newPlainObject()
		ro := rt.objPtr(res)
		ro.defineOwn("promise", p, attrDefault)
		ro.defineOwn("resolve", resolve, attrDefault)
		ro.defineOwn("reject", reject, attrDefault)
		return res, nil
	})
	rt.defMethod(cobj, "reject", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.rejectedPromise(arg(args, 0)), nil
	})
	rt.defMethod(cobj, "all", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.promiseAll(arg(args, 0), false)
	})
	rt.defMethod(cobj, "allSettled", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.promiseAll(arg(args, 0), true)
	})
	rt.defMethod(cobj, "race", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.promiseRace(arg(args, 0), false)
	})
	rt.defMethod(cobj, "any", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.promiseRace(arg(args, 0), true)
	})
	rt.defMethod(cobj, "try", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Promise.try(fn, ...args): run fn synchronously, resolve with its result
		// or reject with a thrown error (ES2025).
		cb := arg(args, 0)
		var extra []Value
		if len(args) > 1 {
			extra = args[1:]
		}
		p, o := rt.makePromise()
		res, e := rt.callValue(cb, mkundef(), extra)
		if e != nil {
			rt.rejectPromise(o, e.Value)
		} else {
			rt.resolvePromise(p, o, res)
		}
		return p, nil
	})

	rt.defSpeciesGetter(ctor)
	rt.defGlobal("Promise", ctor)
}

// promiseAll implements Promise.all / Promise.allSettled.
func (rt *Runtime) promiseAll(iterable Value, settled bool) (Value, *ThrowError) {
	vals, e := rt.iterableValues(iterable)
	if e != nil {
		return mkundef(), e
	}
	result, ro := rt.makePromise()
	results := rt.newArray()
	ra := rt.objPtr(results)
	remaining := len(vals) + 1
	tryFinish := func() {
		remaining--
		if remaining == 0 {
			rt.resolvePromise(result, ro, results)
		}
	}
	for i, v := range vals {
		rt.arraySet(ra, uint32(i), mkundef())
		p := rt.resolvedPromise(v)
		po := rt.objPtr(p)
		onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			if settled {
				o := rt.newPlainObject()
				oo := rt.objPtr(o)
				oo.defineOwn("status", rt.newString("fulfilled"), attrDefault)
				oo.defineOwn("value", arg(a, 0), attrDefault)
				rt.arraySet(ra, uint32(i), o)
			} else {
				rt.arraySet(ra, uint32(i), arg(a, 0))
			}
			tryFinish()
			return mkundef(), nil
		})
		var onR Value
		if settled {
			onR = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				o := rt.newPlainObject()
				oo := rt.objPtr(o)
				oo.defineOwn("status", rt.newString("rejected"), attrDefault)
				oo.defineOwn("reason", arg(a, 0), attrDefault)
				rt.arraySet(ra, uint32(i), o)
				tryFinish()
				return mkundef(), nil
			})
		} else {
			onR = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				rt.rejectPromise(ro, arg(a, 0))
				return mkundef(), nil
			})
		}
		rt.promiseThen(onF, onR, po)
	}
	tryFinish()
	return result, nil
}

// promiseRace implements Promise.race / Promise.any.
func (rt *Runtime) promiseRace(iterable Value, any bool) (Value, *ThrowError) {
	vals, e := rt.iterableValues(iterable)
	if e != nil {
		return mkundef(), e
	}
	result, ro := rt.makePromise()
	remaining := len(vals)
	errs := rt.newArray()
	ea := rt.objPtr(errs)
	for i, v := range vals {
		p := rt.resolvedPromise(v)
		po := rt.objPtr(p)
		var onF, onR Value
		if any {
			onF = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				rt.resolvePromise(result, ro, arg(a, 0))
				return mkundef(), nil
			})
			onR = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				rt.arraySet(ea, uint32(i), arg(a, 0))
				remaining--
				if remaining == 0 {
					agg := rt.makeError(rt.errors.aggProto, "AggregateError", "All promises were rejected")
					if eo := rt.objPtr(agg); eo != nil {
						eo.defineOwn("errors", errs, attrWritable|attrConfigurable)
					}
					rt.rejectPromise(ro, agg)
				}
				return mkundef(), nil
			})
		} else {
			onF = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				rt.resolvePromise(result, ro, arg(a, 0))
				return mkundef(), nil
			})
			onR = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				rt.rejectPromise(ro, arg(a, 0))
				return mkundef(), nil
			})
		}
		rt.promiseThen(onF, onR, po)
	}
	if any && len(vals) == 0 {
		agg := rt.makeError(rt.errors.aggProto, "AggregateError", "All promises were rejected")
		rt.rejectPromise(ro, agg)
	}
	return result, nil
}
