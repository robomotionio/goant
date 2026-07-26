package engine

// AsyncIterator (ES2025 async iterator helpers): %AsyncIteratorPrototype%, the
// AsyncIterator abstract constructor, AsyncIterator.from, and the prototype
// helpers. The consuming helpers (toArray/forEach/every/some/find/reduce) drive
// the async iteration by chaining promises off this.next(); the lazy helpers
// (map/filter/take/drop/flatMap) return a fresh async iterator whose next()
// awaits the source.

func (rt *Runtime) initAsyncIterator() {
	proto := rt.newObject(rt.objectProto)
	rt.asyncIteratorProto = proto
	po := rt.objPtr(proto)
	if rt.symAsyncIterator != 0 {
		self := rt.newNativeFunc("[Symbol.asyncIterator]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return this, nil
		})
		po.defineOwnSymbol(rt.symAsyncIterator.handle(), self, attrWritable|attrConfigurable)
	}
	if rt.symAsyncDispose != 0 {
		// %AsyncIteratorPrototype%[@@asyncDispose] closes the iterator by calling its
		// `return` method, resolving the returned promise with the (awaited) result.
		disp := rt.newNativeFunc("[Symbol.asyncDispose]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			p, pp := rt.makePromise()
			if !this.IsObjectType() {
				rt.rejectPromise(pp, rt.typeError("AsyncIterator.prototype[Symbol.asyncDispose] called on a non-object").Value)
				return p, nil
			}
			rf, e := rt.getField(this, "return")
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return p, nil
			}
			if rf.IsNullish() {
				rt.fulfillPromise(pp, mkundef())
				return p, nil
			}
			if !rt.isCallable(rf) {
				rt.rejectPromise(pp, rt.typeError("'return' is not a function").Value)
				return p, nil
			}
			res, e := rt.callValue(rf, this, nil)
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return p, nil
			}
			rt.resolvePromise(p, pp, res)
			return p, nil
		})
		po.defineOwnSymbol(rt.symAsyncDispose.handle(), disp, attrWritable|attrConfigurable)
	}
	rt.setStringTag(proto, "AsyncIterator")

	// Consuming helpers return a promise.
	rt.defMethod(po, "toArray", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		acc := rt.newArray()
		ao := rt.objPtr(acc)
		p, po := rt.makePromise()
		rt.asyncIterLoop(this, func(v Value) (bool, *ThrowError) {
			rt.arraySet(ao, ao.arrLen, v)
			return true, nil
		}, func(e Value) {
			if e == 0 {
				rt.resolvePromise(p, po, acc)
			} else {
				rt.rejectPromise(po, e)
			}
		})
		return p, nil
	})
	rt.defMethod(po, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		p, po := rt.makePromise()
		i := 0
		rt.asyncIterLoop(this, func(v Value) (bool, *ThrowError) {
			_, e := rt.callValue(fn, mkundef(), []Value{v, mknum(float64(i))})
			i++
			return true, e
		}, func(e Value) {
			if e == 0 {
				rt.resolvePromise(p, po, mkundef())
			} else {
				rt.rejectPromise(po, e)
			}
		})
		return p, nil
	})
	rt.defMethod(po, "some", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		p, po := rt.makePromise()
		i := 0
		found := false
		rt.asyncIterLoop(this, func(v Value) (bool, *ThrowError) {
			r, e := rt.callValue(fn, mkundef(), []Value{v, mknum(float64(i))})
			i++
			if e != nil {
				return false, e
			}
			if rt.toBoolean(r) {
				found = true
				return false, nil // stop
			}
			return true, nil
		}, func(e Value) {
			if e == 0 {
				rt.resolvePromise(p, po, mkbool(found))
			} else {
				rt.rejectPromise(po, e)
			}
		})
		return p, nil
	})
	rt.defMethod(po, "every", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		p, po := rt.makePromise()
		i := 0
		all := true
		rt.asyncIterLoop(this, func(v Value) (bool, *ThrowError) {
			r, e := rt.callValue(fn, mkundef(), []Value{v, mknum(float64(i))})
			i++
			if e != nil {
				return false, e
			}
			if !rt.toBoolean(r) {
				all = false
				return false, nil
			}
			return true, nil
		}, func(e Value) {
			if e == 0 {
				rt.resolvePromise(p, po, mkbool(all))
			} else {
				rt.rejectPromise(po, e)
			}
		})
		return p, nil
	})
	rt.defMethod(po, "find", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		p, po := rt.makePromise()
		i := 0
		result := mkundef()
		rt.asyncIterLoop(this, func(v Value) (bool, *ThrowError) {
			r, e := rt.callValue(fn, mkundef(), []Value{v, mknum(float64(i))})
			i++
			if e != nil {
				return false, e
			}
			if rt.toBoolean(r) {
				result = v
				return false, nil
			}
			return true, nil
		}, func(e Value) {
			if e == 0 {
				rt.resolvePromise(p, po, result)
			} else {
				rt.rejectPromise(po, e)
			}
		})
		return p, nil
	})
	rt.defMethod(po, "reduce", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		fn := arg(args, 0)
		acc := arg(args, 1)
		hasAcc := len(args) > 1
		p, po := rt.makePromise()
		i := 0
		rt.asyncIterLoop(this, func(v Value) (bool, *ThrowError) {
			if !hasAcc {
				acc = v
				hasAcc = true
				i++
				return true, nil
			}
			r, e := rt.callValue(fn, mkundef(), []Value{acc, v, mknum(float64(i))})
			i++
			if e != nil {
				return false, e
			}
			acc = r
			return true, nil
		}, func(e Value) {
			if e != 0 {
				rt.rejectPromise(po, e)
			} else if !hasAcc {
				rt.rejectPromise(po, rt.makeError(rt.errors.typeProto, "TypeError", "Reduce of empty async iterator with no initial value"))
			} else {
				rt.resolvePromise(p, po, acc)
			}
		})
		return p, nil
	})

	// Lazy helpers return a fresh async iterator.
	rt.defMethod(po, "map", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.asyncIterTransform(this, arg(args, 0), asyncMap), nil
	})
	rt.defMethod(po, "filter", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.asyncIterTransform(this, arg(args, 0), asyncFilter), nil
	})
	rt.defMethod(po, "flatMap", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.asyncIterFlatMap(this, arg(args, 0)), nil
	})
	rt.defMethod(po, "take", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.asyncIterLimit(this, rt.intArg(args, 0), true), nil
	})
	rt.defMethod(po, "drop", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.asyncIterLimit(this, rt.intArg(args, 0), false), nil
	})

	// AsyncIterator abstract constructor + AsyncIterator.from.
	var ctor Value
	ctor = rt.newNativeFunc("AsyncIterator", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		nt := rt.pendingNewTarget
		if nt.IsUndefined() {
			nt = rt.activeNewTarget
		}
		if nt.IsUndefined() || nt == ctor {
			return mkundef(), rt.typeError("Abstract class AsyncIterator not directly constructable")
		}
		return this, nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defMethod(cobj, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		src := arg(args, 0)
		// If already an async iterator, return it directly.
		if rt.symAsyncIterator != 0 && src.IsObjectType() {
			m, e := rt.getElement(src, rt.symAsyncIterator)
			if e != nil {
				return mkundef(), e
			}
			if rt.isCallable(m) {
				it, e := rt.callValue(m, src, nil)
				if e != nil {
					return mkundef(), e
				}
				return it, nil
			}
		}
		return rt.getAsyncIterator(src)
	})
	rt.defGlobal("AsyncIterator", ctor)

	// %AsyncGeneratorPrototype% chains to %AsyncIteratorPrototype%; an async
	// generator object's next/return/throw drive the coroutine and wrap the
	// result in a promise (a rejection when the body throws).
	agp := rt.newObject(proto)
	rt.asyncGenProto = agp
	ao := rt.objPtr(agp)
	drive := func(kind genResumeKind) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil || o.gen == nil || o.gen.fn == nil || !o.gen.fn.isAsync {
				return rt.rejectedPromise(rt.makeError(rt.errors.typeProto, "TypeError", "not an async generator")), nil
			}
			p, po := rt.makePromise()
			o.gen.asyncReqs = append(o.gen.asyncReqs, asyncGenReq{kind: kind, val: arg(args, 0), p: p, po: po})
			rt.asyncGenDrain(o.gen)
			return p, nil
		}
	}
	rt.defMethod(ao, "next", 1, drive(genNext))
	rt.defMethod(ao, "return", 1, drive(genReturn))
	rt.defMethod(ao, "throw", 1, drive(genThrow))
	rt.setStringTag(agp, "AsyncGenerator")
	// %AsyncGeneratorFunction.prototype%.prototype is %AsyncGeneratorPrototype%,
	// pointing back via constructor ({[[Writable]]:false,[[Enumerable]]:false,
	// [[Configurable]]:true}).
	if rt.asyncGeneratorFnProto != 0 {
		rt.objPtr(rt.asyncGeneratorFnProto).defineOwn("prototype", agp, attrConfigurable)
		ao.defineOwn("constructor", rt.asyncGeneratorFnProto, attrConfigurable)
	}
}

// asyncIterLoop drives an async iterator: it repeatedly awaits this.next() and
// feeds each value to onValue (return false to stop). done reports completion
// (e == 0 for normal end, else the rejection reason). Everything runs through
// promise reactions so the microtask queue advances the iteration.
func (rt *Runtime) asyncIterLoop(iter Value, onValue func(Value) (bool, *ThrowError), done func(Value)) {
	var step func()
	step = func() {
		nextFn, e := rt.getField(iter, "next")
		if e != nil {
			done(e.Value)
			return
		}
		res, e := rt.callValue(nextFn, iter, nil)
		if e != nil {
			done(e.Value)
			return
		}
		// res is a promise (or thenable/plain result). Adopt it via a fresh
		// promise so we can attach reactions uniformly.
		rp := rt.resolvedPromise(res)
		onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			r := arg(a, 0)
			if !r.IsObjectType() {
				done(rt.makeError(rt.errors.typeProto, "TypeError", "iterator result is not an object"))
				return mkundef(), nil
			}
			dv, _ := rt.getField(r, "done")
			if rt.toBoolean(dv) {
				done(0)
				return mkundef(), nil
			}
			val, _ := rt.getField(r, "value")
			cont, e := onValue(val)
			if e != nil {
				done(e.Value)
				return mkundef(), nil
			}
			if !cont {
				done(0)
				return mkundef(), nil
			}
			step()
			return mkundef(), nil
		})
		onR := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			done(arg(a, 0))
			return mkundef(), nil
		})
		rt.holdCaptures(onF, []Value{iter, rp})
		rt.holdCaptures(onR, []Value{iter, rp})
		rt.promiseThen(onF, onR, rt.objPtr(rp))
	}
	step()
}

type asyncTransformKind int

const (
	asyncMap asyncTransformKind = iota
	asyncFilter
)

// asyncIterTransform returns a lazy async iterator that applies fn (map/filter)
// to each value of the source. next() returns a promise resolving to the next
// transformed IteratorResult.
func (rt *Runtime) asyncIterTransform(src, fn Value, kind asyncTransformKind) Value {
	wrap := rt.newObject(rt.asyncIteratorProto)
	o := rt.objPtr(wrap)
	rt.holdCaptures(wrap, []Value{src, fn})
	i := 0
	rt.defMethod(o, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		p, pp := rt.makePromise()
		var pump func()
		pump = func() {
			nextFn, e := rt.getField(src, "next")
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return
			}
			res, e := rt.callValue(nextFn, src, nil)
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return
			}
			rp := rt.resolvedPromise(res)
			onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				r := arg(a, 0)
				dv, _ := rt.getField(r, "done")
				if rt.toBoolean(dv) {
					rt.resolvePromise(p, pp, rt.genResult(mkundef(), true))
					return mkundef(), nil
				}
				val, _ := rt.getField(r, "value")
				idx := i
				i++
				mv, e := rt.callValue(fn, mkundef(), []Value{val, mknum(float64(idx))})
				if e != nil {
					rt.rejectPromise(pp, e.Value)
					return mkundef(), nil
				}
				if kind == asyncFilter {
					if rt.toBoolean(mv) {
						rt.resolvePromise(p, pp, rt.genResult(val, false))
					} else {
						pump() // skip; fetch the next source value
					}
					return mkundef(), nil
				}
				rt.resolvePromise(p, pp, rt.genResult(mv, false))
				return mkundef(), nil
			})
			onR := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				rt.rejectPromise(pp, arg(a, 0))
				return mkundef(), nil
			})
			rt.holdCaptures(onF, []Value{p, rp})
			rt.holdCaptures(onR, []Value{p, rp})
			rt.promiseThen(onF, onR, rt.objPtr(rp))
		}
		pump()
		return p, nil
	})
	return wrap
}

// asyncIterFlatMap returns a lazy async iterator that maps each source value to
// an (async) iterable and yields all of its values before advancing the source.
func (rt *Runtime) asyncIterFlatMap(src, fn Value) Value {
	wrap := rt.newObject(rt.asyncIteratorProto)
	o := rt.objPtr(wrap)
	// inner[0] is the current inner async iterator, 0 when between sources. It
	// is a slice, not a captured variable, because it is reassigned as the
	// helper advances and the collector cannot see inside a closure.
	inner := make([]Value, 1)
	rt.holdCaptures(wrap, []Value{src, fn}, inner)
	i := 0
	rt.defMethod(o, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		p, pp := rt.makePromise()
		var pumpOuter func()
		var pumpInner func()
		pumpInner = func() {
			nextFn, e := rt.getField(inner[0], "next")
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return
			}
			res, e := rt.callValue(nextFn, inner[0], nil)
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return
			}
			rp := rt.resolvedPromise(res)
			onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				r := arg(a, 0)
				if dv, _ := rt.getField(r, "done"); rt.toBoolean(dv) {
					inner[0] = 0
					pumpOuter()
					return mkundef(), nil
				}
				val, _ := rt.getField(r, "value")
				rt.resolvePromise(p, pp, rt.genResult(val, false))
				return mkundef(), nil
			})
			onR := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				rt.rejectPromise(pp, arg(a, 0))
				return mkundef(), nil
			})
			rt.holdCaptures(onF, []Value{p, rp})
			rt.holdCaptures(onR, []Value{p, rp})
			rt.promiseThen(onF, onR, rt.objPtr(rp))
		}
		pumpOuter = func() {
			nextFn, e := rt.getField(src, "next")
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return
			}
			res, e := rt.callValue(nextFn, src, nil)
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return
			}
			rp := rt.resolvedPromise(res)
			onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				r := arg(a, 0)
				if dv, _ := rt.getField(r, "done"); rt.toBoolean(dv) {
					rt.resolvePromise(p, pp, rt.genResult(mkundef(), true))
					return mkundef(), nil
				}
				val, _ := rt.getField(r, "value")
				idx := i
				i++
				mv, e := rt.callValue(fn, mkundef(), []Value{val, mknum(float64(idx))})
				if e != nil {
					rt.rejectPromise(pp, e.Value)
					return mkundef(), nil
				}
				it, e := rt.getAsyncIterator(mv)
				if e != nil {
					rt.rejectPromise(pp, e.Value)
					return mkundef(), nil
				}
				inner[0] = it
				pumpInner()
				return mkundef(), nil
			})
			onR := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				rt.rejectPromise(pp, arg(a, 0))
				return mkundef(), nil
			})
			rt.holdCaptures(onF, []Value{p, rp})
			rt.holdCaptures(onR, []Value{p, rp})
			rt.promiseThen(onF, onR, rt.objPtr(rp))
		}
		if inner[0] != 0 {
			pumpInner()
		} else {
			pumpOuter()
		}
		return p, nil
	})
	return wrap
}

// asyncIterLimit returns a lazy async iterator that takes (limit) or drops
// (limit) elements from the source.
func (rt *Runtime) asyncIterLimit(src Value, limit int, take bool) Value {
	wrap := rt.newObject(rt.asyncIteratorProto)
	o := rt.objPtr(wrap)
	rt.holdCaptures(wrap, []Value{src})
	seen := 0
	rt.defMethod(o, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		p, pp := rt.makePromise()
		if take && seen >= limit {
			rt.resolvePromise(p, pp, rt.genResult(mkundef(), true))
			return p, nil
		}
		var pump func()
		pump = func() {
			nextFn, e := rt.getField(src, "next")
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return
			}
			res, e := rt.callValue(nextFn, src, nil)
			if e != nil {
				rt.rejectPromise(pp, e.Value)
				return
			}
			rp := rt.resolvedPromise(res)
			onF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				r := arg(a, 0)
				dv, _ := rt.getField(r, "done")
				if rt.toBoolean(dv) {
					rt.resolvePromise(p, pp, rt.genResult(mkundef(), true))
					return mkundef(), nil
				}
				val, _ := rt.getField(r, "value")
				if !take && seen < limit {
					seen++
					pump() // drop this value, fetch next
					return mkundef(), nil
				}
				seen++
				rt.resolvePromise(p, pp, rt.genResult(val, false))
				return mkundef(), nil
			})
			onR := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				rt.rejectPromise(pp, arg(a, 0))
				return mkundef(), nil
			})
			rt.holdCaptures(onF, []Value{p, rp})
			rt.holdCaptures(onR, []Value{p, rp})
			rt.promiseThen(onF, onR, rt.objPtr(rp))
		}
		pump()
		return p, nil
	})
	return wrap
}
