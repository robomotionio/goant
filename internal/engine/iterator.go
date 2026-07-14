package engine

// Iteration protocol support (ant modules/iterator.c). The Phase 5 slice
// materializes iterable values eagerly for arrays, strings, and (later)
// Map/Set; the lazy Symbol.iterator + generator protocol is layered on when
// Symbol and generators land.

// getSyncIterator implements GetIterator(obj, sync): call obj[@@iterator]() and
// return the iterator object (for the lazy for-of loop, which closes it on an
// abrupt completion).
func (rt *Runtime) getSyncIterator(source Value) (Value, *ThrowError) {
	if rt.symIterator != 0 {
		m, e := rt.getElement(source, rt.symIterator)
		if e != nil {
			return mkundef(), e
		}
		if rt.isCallable(m) {
			it, e := rt.callValue(m, source, nil)
			if e != nil {
				return mkundef(), e
			}
			if !it.IsObjectType() {
				return mkundef(), rt.typeError("[Symbol.iterator]() returned a non-object")
			}
			return it, nil
		}
	}
	return mkundef(), rt.typeError(rt.typeofString(source) + " " + rt.inspect(source, false) + " is not iterable")
}

// iteratorClose calls iter.return() for the normal-completion case, swallowing
// any error/result (IteratorClose, 7.4.8).
func (rt *Runtime) iteratorClose(iter Value) {
	if !iter.IsObjectType() {
		return
	}
	if rf, e := rt.getField(iter, "return"); e == nil && rt.isCallable(rf) {
		rt.callValue(rf, iter, nil)
	}
}

// iteratorCloseE closes an iterator for a NORMAL completion, propagating an
// abrupt completion from Get(return)/the return call (used by the Iterator
// helpers where the pending completion is not itself a throw, e.g. take
// exhaustion, some/find/every early exit, and a helper's own `return`).
func (rt *Runtime) iteratorCloseE(iter Value) *ThrowError {
	if !iter.IsObjectType() {
		return nil
	}
	rf, e := rt.getField(iter, "return")
	if e != nil {
		return e
	}
	if rf.IsNullish() {
		return nil
	}
	if !rt.isCallable(rf) {
		return rt.typeError("'return' is not a function")
	}
	_, e = rt.callValue(rf, iter, nil)
	return e
}

// iterateWithClose drives source's iterator, calling fn per value. If fn returns
// an error or stop=true, the iterator is closed (return()) before returning —
// this is the spec pattern for operations that may abort mid-iteration
// (AddEntriesFromIterable, Array.from, destructuring, …).
func (rt *Runtime) iterateWithClose(source Value, fn func(v Value) (stop bool, err *ThrowError)) *ThrowError {
	iter, e := rt.getSyncIterator(source)
	if e != nil {
		return e
	}
	return rt.iterateIteratorWithClose(iter, fn)
}

// iterateIteratorWithClose drives an already-obtained iterator object with the
// same close-on-abrupt-completion semantics as iterateWithClose. It lets a
// caller that already performed GetMethod(@@iterator) (e.g. Array.from) avoid a
// second observable [[Get]] of the iterator method.
func (rt *Runtime) iterateIteratorWithClose(iter Value, fn func(v Value) (stop bool, err *ThrowError)) *ThrowError {
	for {
		nextFn, e := rt.getField(iter, "next")
		if e != nil {
			return e
		}
		r, e := rt.callValue(nextFn, iter, nil)
		if e != nil {
			return e
		}
		if !r.IsObjectType() {
			return rt.typeError("iterator result is not an object")
		}
		d, e := rt.getField(r, "done")
		if e != nil {
			return e
		}
		if rt.toBoolean(d) {
			return nil
		}
		// IteratorValue is a plain ? (propagate, no IteratorClose): the abrupt
		// came from the iterator's own result object, not the loop body.
		val, e := rt.getField(r, "value")
		if e != nil {
			return e
		}
		stop, ferr := fn(val)
		if ferr != nil {
			rt.iteratorClose(iter)
			return ferr
		}
		if stop {
			rt.iteratorClose(iter)
			return nil
		}
	}
}

// getAsyncIterator implements GetIterator(obj, async): prefer @@asyncIterator,
// otherwise wrap the sync @@iterator via CreateAsyncFromSyncIterator. The
// returned object's next() yields a promise of an IteratorResult.
func (rt *Runtime) getAsyncIterator(source Value) (Value, *ThrowError) {
	if rt.symAsyncIterator != 0 {
		m, e := rt.getElement(source, rt.symAsyncIterator)
		if e != nil {
			return mkundef(), e
		}
		// GetMethod semantics: a present @@asyncIterator that is not callable is a
		// TypeError (the sync-iterator fallback applies only when it is absent).
		if !m.IsNullish() {
			if !rt.isCallable(m) {
				return mkundef(), rt.typeError("[Symbol.asyncIterator] is not a function")
			}
			it, e := rt.callValue(m, source, nil)
			if e != nil {
				return mkundef(), e
			}
			if !it.IsObjectType() {
				return mkundef(), rt.typeError("[Symbol.asyncIterator]() returned a non-object")
			}
			return it, nil
		}
	}
	if rt.symIterator != 0 {
		m, e := rt.getElement(source, rt.symIterator)
		if e != nil {
			return mkundef(), e
		}
		if rt.isCallable(m) {
			syncIt, e := rt.callValue(m, source, nil)
			if e != nil {
				return mkundef(), e
			}
			return rt.createAsyncFromSyncIterator(syncIt), nil
		}
	}
	return mkundef(), rt.typeError("value is not async iterable")
}

// createAsyncFromSyncIterator wraps a sync iterator so its next()/return() yield
// promises of IteratorResults (25.1.4.1). The wrapper's next awaits the sync
// value and re-wraps it with the sync done flag.
func (rt *Runtime) createAsyncFromSyncIterator(syncIt Value) Value {
	proto := rt.objectProto
	if rt.asyncIteratorProto != 0 {
		proto = rt.asyncIteratorProto
	}
	wrap := rt.newObject(proto)
	o := rt.objPtr(wrap)
	step := func(method string) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			fn, e := rt.getField(syncIt, method)
			if e != nil {
				return mkundef(), e
			}
			if !rt.isCallable(fn) {
				if method == "next" {
					return rt.resolvedPromise(rt.genResult(mkundef(), true)), nil
				}
				return rt.resolvedPromise(rt.genResult(arg(args, 0), true)), nil
			}
			res, e := rt.callValue(fn, syncIt, args)
			if e != nil {
				return rt.rejectedPromise(e.Value), nil
			}
			if !res.IsObjectType() {
				return rt.rejectedPromise(rt.makeError(rt.errors.typeProto, "TypeError", "iterator result is not an object")), nil
			}
			doneV, _ := rt.getField(res, "done")
			val, _ := rt.getField(res, "value")
			done := rt.toBoolean(doneV)
			// AsyncFromSyncIteratorContinuation: Await the sync value (so a thenable
			// value resolves) before re-wrapping it in an IteratorResult carrying the
			// sync `done` flag. For next(), a rejecting value closes the sync iterator
			// (closeOnRejection); for return() it does not.
			valP := rt.resolvedPromise(val)
			unwrap := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
				return rt.genResult(arg(a, 0), done), nil
			})
			onRej := mkundef()
			if method == "next" && !done {
				onRej = rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
					rt.iteratorClose(syncIt) // close on rejection; swallow the close outcome
					return mkundef(), &ThrowError{Value: arg(a, 0), rt: rt}
				})
			}
			return rt.promiseThen(unwrap, onRej, rt.objPtr(valP)), nil
		}
	}
	rt.defMethod(o, "next", 1, step("next"))
	rt.defMethod(o, "return", 1, step("return"))
	if rt.symAsyncIterator != 0 {
		self := rt.newNativeFunc("[Symbol.asyncIterator]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return this, nil
		})
		o.defineOwnSymbol(rt.symAsyncIterator.handle(), self, attrWritable|attrConfigurable)
	}
	return wrap
}

// iterableValues returns the values produced by iterating v (for for-of and
// spread). Arrays yield their elements; strings yield their code points.
func (rt *Runtime) iterableValues(v Value) ([]Value, *ThrowError) {
	switch v.Type() {
	case TArr:
		o := rt.objPtr(v)
		out := make([]Value, 0, o.arrLen)
		for i := uint32(0); i < o.arrLen; i++ {
			if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
				out = append(out, o.arr[i])
			} else {
				out = append(out, mkundef())
			}
		}
		return out, nil
	case TTypedArray:
		o := rt.objPtr(v)
		n := rt.taLength(o)
		out := make([]Value, n)
		for i := 0; i < n; i++ {
			out[i], _ = rt.taGet(o, i)
		}
		return out, nil
	case TStr:
		b := rt.strBytes(v)
		var out []Value
		n := utf16Len(b)
		for i := 0; i < n; {
			cp := utf16CodepointAt(b, i)
			out = append(out, rt.newStringBytes(wtf8Encode(nil, cp)))
			if cp >= 0x10000 {
				i += 2 // astral code point spans two UTF-16 units
			} else {
				i++
			}
		}
		return out, nil
	default:
		// Built-in Map/Set fast path.
		if it := rt.objectIterableValues(v); it != nil {
			return it, nil
		}
		// General Symbol.iterator protocol (generators, user iterables).
		if out, ok, e := rt.iterateProtocol(v); ok || e != nil {
			return out, e
		}
		return nil, rt.typeError(rt.typeofString(v) + " is not iterable")
	}
}

// isIterable reports whether v can be iterated (has a Symbol.iterator method or
// is a built-in iterable: array, string, Map, Set).
func (rt *Runtime) isIterable(v Value) bool {
	switch v.Type() {
	case TArr, TStr, TTypedArray:
		return true
	}
	if o := rt.objPtr(v); o != nil && o.coll != nil {
		return true
	}
	if (v.IsObjectType() || v.Type() == TTypedArray) && rt.symIterator != 0 {
		it, _ := rt.getFieldSymbol(v, rt.symIterator.handle())
		return rt.isCallable(it)
	}
	return false
}

// iterateProtocol drains an object implementing the Symbol.iterator protocol,
// returning (values, true, nil). ok is false when v has no Symbol.iterator
// method (so the caller can fall through to the not-iterable error).
func (rt *Runtime) iterateProtocol(v Value) ([]Value, bool, *ThrowError) {
	if !v.IsObjectType() || rt.symIterator == 0 {
		return nil, false, nil
	}
	itFn, e := rt.getFieldSymbol(v, rt.symIterator.handle())
	if e != nil {
		return nil, true, e
	}
	if !rt.isCallable(itFn) {
		return nil, false, nil
	}
	iter, e := rt.callValue(itFn, v, nil)
	if e != nil {
		return nil, true, e
	}
	next, e := rt.getField(iter, "next")
	if e != nil {
		return nil, true, e
	}
	if !rt.isCallable(next) {
		return nil, true, rt.typeError("iterator.next is not a function")
	}
	var out []Value
	// Eager materialization cap: the slice compiler lowers for-of/spread to an
	// array, so an unbounded iterator that a `break` would have stopped cannot be
	// consumed lazily yet. Cap to avoid wedging the process; lazy for-of (pull +
	// break/close) is the real fix.
	const maxEager = 1 << 20
	for iters := 0; iters < maxEager; iters++ {
		res, e := rt.callValue(next, iter, nil)
		if e != nil {
			return nil, true, e
		}
		done, e := rt.getField(res, "done")
		if e != nil {
			return nil, true, e
		}
		if rt.toBoolean(done) {
			return out, true, nil
		}
		val, e := rt.getField(res, "value")
		if e != nil {
			return nil, true, e
		}
		out = append(out, val)
	}
	return out, true, rt.rangeError("iterator produced too many values (eager-iteration cap)")
}

// objectIterableValues handles built-in iterable objects (Map/Set); returns
// nil for non-iterables. Map yields [key,value] pairs; Set yields values.
func (rt *Runtime) objectIterableValues(v Value) []Value {
	o := rt.objPtr(v)
	if o == nil || o.coll == nil {
		return nil
	}
	c := o.coll
	var out []Value
	for i := 0; i < len(c.keys); i++ {
		if c.keys[i].IsEmpty() {
			continue
		}
		if c.isSet {
			out = append(out, c.keys[i])
		} else {
			pair := rt.newArray()
			po := rt.objPtr(pair)
			rt.arraySet(po, 0, c.keys[i])
			rt.arraySet(po, 1, c.vals[i])
			out = append(out, pair)
		}
	}
	return out
}
