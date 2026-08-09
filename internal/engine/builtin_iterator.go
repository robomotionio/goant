package engine

import "math"

// %IteratorPrototype% and the built-in iterator objects returned by
// Array/Map/Set entries/keys/values (ant modules/iterator.c). Each iterator is
// an ordinary object whose `next` is a Go closure over a cursor; it inherits
// [Symbol.iterator]() -> this from %IteratorPrototype% so it is itself iterable.

// initIteratorProto creates %IteratorPrototype% and chains %GeneratorPrototype%
// to it (generators are iterators). Must run after Symbol (symIterator) exists.
func (rt *Runtime) initIteratorProto() {
	proto := rt.newObject(rt.objectProto)
	rt.iteratorProto = proto
	if rt.symIterator != 0 {
		selfIter := rt.newNativeFunc("[Symbol.iterator]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return this, nil
		})
		rt.objPtr(proto).defineOwnSymbol(rt.symIterator.handle(), selfIter, attrWritable|attrConfigurable)
	}
	// Per-kind iterator prototypes (%ArrayIteratorPrototype% etc.) chain to
	// %IteratorPrototype%, so an iterator instance's proto chain is
	// instance -> <Kind>IteratorPrototype -> %IteratorPrototype%.
	mk := func(tag string) Value {
		p := rt.newObject(rt.iteratorProto)
		rt.setStringTag(p, tag)
		return p
	}
	rt.arrayIterProto = mk("Array Iterator")
	rt.mapIterProto = mk("Map Iterator")
	rt.setIterProto = mk("Set Iterator")
	rt.stringIterProto = mk("String Iterator")
	rt.regexpStrIterProto = mk("RegExp String Iterator")

	// %RegExpStringIteratorPrototype%.next (22.2.9.2.1) is a shared method that
	// reads the receiver's iteration state (missing-brand check) and steps the
	// matcher lazily via the abstract RegExpExec so a user exec / Proxy is honored.
	rt.defMethod(rt.objPtr(rt.regexpStrIterProto), "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.regexpStrIterNext(this)
	})

	// %SetIteratorPrototype%.next / %MapIteratorPrototype%.next are shared methods
	// (length 0, name "next") that read the receiver's iteration state and enforce
	// the per-kind brand check, rather than a per-instance closure.
	rt.defMethod(rt.objPtr(rt.setIterProto), "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.collIterNext(this, true)
	})
	rt.defMethod(rt.objPtr(rt.mapIterProto), "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.collIterNext(this, false)
	})
	rt.defMethod(rt.objPtr(rt.arrayIterProto), "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.arrIterNext(this)
	})
	rt.defMethod(rt.objPtr(rt.stringIterProto), "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		st := rt.strIterStates[rt.objPtr(this)]
		if st == nil {
			return mkundef(), rt.typeError("String Iterator.prototype.next called on an incompatible receiver")
		}
		if st.index >= len(st.vals) {
			return rt.genResult(mkundef(), true), nil
		}
		v := st.vals[st.index]
		st.index++
		return rt.genResult(v, false), nil
	})
}

// strIterState is the internal state of a String iterator: a snapshot of the
// string's code points and the current index.
type strIterState struct {
	vals  []Value
	index int
}

// newStringIterator builds a String iterator over a pre-computed code-point slice,
// storing its state for the shared %StringIteratorPrototype%.next.
func (rt *Runtime) newStringIterator(vals []Value) Value {
	v := rt.newObject(rt.stringIterProto)
	if rt.strIterStates == nil {
		rt.strIterStates = map[*object]*strIterState{}
	}
	rt.strIterStates[rt.objPtr(v)] = &strIterState{vals: vals}
	return v
}

// sliceIterator returns an iterator object over a fixed slice of values.
// newIteratorObjectE builds a lazy iterator helper result whose next step may
// throw. Its `return` closes the source iterator (forwarding the close) and
// marks the helper done; `done` is shared with the step closure so an exhausted
// or already-returned helper does not re-close the source.
func (rt *Runtime) newIteratorObjectE(source Value, done *bool, next func() (Value, bool, *ThrowError), held ...[]Value) Value {
	proto := rt.iteratorProto
	if rt.iterHelperProto != 0 {
		proto = rt.iterHelperProto
	}
	v := rt.newObject(proto)
	o := rt.objPtr(v)
	// The step closure is the only thing referring to the source iterator and
	// whatever else the helper carries (its `next` method, a callback, the
	// inner iterator of a flatMap). A Go closure is opaque to the collector, so
	// they are registered as this helper's internal slots. See holdCaptures.
	rt.holdCaptures(v, append(held, []Value{source})...)
	running := false // guards re-entrant next while the helper generator is executing
	rt.defMethod(o, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if running {
			return mkundef(), rt.typeError("Iterator helper is already running")
		}
		running = true
		val, d, e := next()
		running = false
		if e != nil {
			return mkundef(), e
		}
		return rt.genResult(val, d), nil
	})
	rt.defMethod(o, "return", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if running {
			return mkundef(), rt.typeError("Iterator helper is already running")
		}
		if !*done {
			*done = true
			if e := rt.iteratorCloseE(source); e != nil {
				return mkundef(), e
			}
		}
		return rt.genResult(arg(args, 0), true), nil
	})
	return v
}

// iterStepValue calls a source iterator's next method and returns (value, done),
// throwing if the result is not an object.
func (rt *Runtime) iterStepValue(iter, nextMethod Value) (Value, bool, *ThrowError) {
	res, e := rt.callValue(nextMethod, iter, nil)
	if e != nil {
		return mkundef(), false, e
	}
	if !res.IsObjectType() {
		return mkundef(), false, rt.typeError("iterator result is not an object")
	}
	doneV, e := rt.getField(res, "done")
	if e != nil {
		return mkundef(), false, e
	}
	if rt.toBoolean(doneV) {
		return mkundef(), true, nil
	}
	val, e := rt.getField(res, "value")
	if e != nil {
		return mkundef(), false, e
	}
	return val, false, nil
}

// iterHelperCallback validates the receiver (an Object) and a callback (must be
// callable — else the source iterator is closed and a TypeError thrown), then
// performs GetIteratorDirect, returning the source's next method and the callback.
func (rt *Runtime) iterHelperCallback(this, cb Value, name string) (Value, Value, *ThrowError) {
	if !this.IsObjectType() {
		return mkundef(), mkundef(), rt.typeError("Iterator.prototype." + name + " called on a non-object")
	}
	if !rt.isCallable(cb) {
		rt.iteratorClose(this)
		return mkundef(), mkundef(), rt.typeError("Iterator.prototype." + name + " callback is not a function")
	}
	next, e := rt.getField(this, "next")
	if e != nil {
		return mkundef(), mkundef(), e
	}
	return next, cb, nil
}

// iterHelperLimit validates the receiver and a numeric limit for take/drop, in
// spec order: ToNumber(limit) first (closing the source on an abrupt completion),
// then reject — each closing the source first — NaN, a finite limit above
// 2**53-1, and a negative ToIntegerOrInfinity, all with a RangeError; and only
// THEN GetIteratorDirect (read "next"). +∞ maps to an unbounded limit; a
// fractional value truncates toward zero.
func (rt *Runtime) iterHelperLimit(this, limitArg Value) (Value, int, *ThrowError) {
	if !this.IsObjectType() {
		return mkundef(), 0, rt.typeError("Iterator.prototype method called on a non-object")
	}
	num, e := rt.toNumber(limitArg)
	if e != nil {
		rt.iteratorClose(this) // IfAbruptCloseIterator
		return mkundef(), 0, e
	}
	if num != num { // NaN
		rt.iteratorClose(this)
		return mkundef(), 0, rt.rangeError("limit must not be NaN")
	}
	// A limit that cannot be represented exactly is rejected rather than
	// silently rounded — but +∞ stays legal and means "no limit".
	if !math.IsInf(num, 0) && num > 9007199254740991 { // 2**53-1
		rt.iteratorClose(this)
		return mkundef(), 0, rt.rangeError("limit must not exceed 2**53 - 1")
	}
	// ToIntegerOrInfinity truncates toward zero, so only a value ≤ -1 is negative
	// (e.g. -0.5 becomes 0 and is accepted).
	if num <= -1 {
		rt.iteratorClose(this)
		return mkundef(), 0, rt.rangeError("limit must not be negative")
	}
	next, e := rt.getField(this, "next") // GetIteratorDirect, after the limit is valid
	if e != nil {
		return mkundef(), 0, e
	}
	limit := int(^uint(0) >> 1) // +∞ → effectively unbounded
	if num < float64(limit) {
		limit = int(num) // truncates toward zero
	}
	return next, limit, nil
}

// iterChunkSize validates the chunkSize/windowSize argument of
// Iterator.prototype.chunks and .windows. Like includes' skippedElements — and
// unlike take/drop's limit — it is never coerced: a non-Number is a TypeError
// and its valueOf/toString are not called. NaN and ±∞ are not integral and so
// are TypeErrors too, leaving RangeError for a whole number outside the
// inclusive interval [1, 2**32-1]. Every rejection closes the source first.
func (rt *Runtime) iterChunkSize(this, sizeArg Value, what string) (uint64, *ThrowError) {
	if !sizeArg.IsNumber() {
		rt.iteratorClose(this)
		return 0, rt.typeError(what + " must be a Number")
	}
	n := sizeArg.Number()
	if math.IsInf(n, 0) || n != math.Trunc(n) { // Trunc(NaN) is NaN, so NaN lands here
		rt.iteratorClose(this)
		return 0, rt.typeError(what + " must be an integral Number")
	}
	if n < 1 || n > 4294967295 { // 2**32-1; -0 is below 1 and so rejected
		rt.iteratorClose(this)
		return 0, rt.rangeError(what + " must be between 1 and 2**32 - 1")
	}
	return uint64(n), nil
}

// copyArrayPrefix materialises a fresh Array holding src's first arrLen
// elements. Both objects are resolved after the allocation, never across it.
func (rt *Runtime) copyArrayPrefix(src Value) Value {
	out := rt.newArray()
	so, oo := rt.objPtr(src), rt.objPtr(out)
	for i := uint32(0); i < so.arrLen; i++ {
		rt.arraySet(oo, i, so.arr[i])
	}
	return out
}

// iterSkipCount validates the optional skippedElements argument of
// Iterator.prototype.includes. Unlike take/drop's limit it is deliberately NOT
// coerced — a non-Number is a TypeError and its valueOf/toString are never
// called — and only ±∞ or an integral Number is accepted. A negative count is a
// RangeError (but -0 is not negative), as is a finite count above 2**53-1.
// Every rejection closes the source iterator first.
func (rt *Runtime) iterSkipCount(this, skipArg Value) (float64, *ThrowError) {
	if skipArg.IsUndefined() {
		return 0, nil
	}
	if !skipArg.IsNumber() {
		rt.iteratorClose(this)
		return 0, rt.typeError("skippedElements must be an integral Number")
	}
	n := skipArg.Number()
	// NaN falls out here too: Trunc(NaN) is NaN and NaN != NaN.
	if !math.IsInf(n, 0) && n != math.Trunc(n) {
		rt.iteratorClose(this)
		return 0, rt.typeError("skippedElements must be an integral Number")
	}
	if n < 0 { // -∞ and -1 reject; -0 < 0 is false, so -0 is accepted
		rt.iteratorClose(this)
		return 0, rt.rangeError("skippedElements must not be negative")
	}
	if !math.IsInf(n, 0) && n > 9007199254740991 { // 2**53-1
		rt.iteratorClose(this)
		return 0, rt.rangeError("skippedElements must not exceed 2**53 - 1")
	}
	return n, nil
}

// getIteratorFlattenable implements GetIteratorFlattenable(obj, reject-primitives)
// for flatMap: obj must be an Object; if it has a callable @@iterator, call it,
// otherwise use obj itself as the iterator.
// setterIgnoresPrototype implements SetterThatIgnoresPrototypeProperties(this,
// home, key, v): assigning through the home object itself throws (emulating a
// non-writable data property), otherwise the value is created as — or set on —
// an own property of the receiver, bypassing the inherited accessor. Used by the
// %Iterator.prototype% constructor / @@toStringTag setters.
func (rt *Runtime) setterIgnoresPrototype(this, home, key, v Value) *ThrowError {
	if !this.IsObjectLike() {
		return rt.typeError("setter called on a non-object")
	}
	if this == home {
		return rt.typeError("Cannot assign to a read-only property of the prototype")
	}
	exists := false
	o := rt.objPtr(this)
	if o.proxy != nil {
		d, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, key)
		if e != nil {
			return e
		}
		exists = !d.IsUndefined()
	} else {
		_, exists = rt.targetOwnDesc(this, key)
	}
	if !exists {
		return rt.createDataProperty(this, key, v)
	}
	ok, e := rt.ordinarySet(this, key, v, this)
	if e != nil {
		return e
	}
	if !ok {
		return rt.typeError("Cannot assign to read-only property")
	}
	return nil
}

func (rt *Runtime) getIteratorFlattenable(obj Value) (Value, *ThrowError) {
	if !obj.IsObjectType() {
		return mkundef(), rt.typeError("flatMap callback did not return an object")
	}
	if rt.symIterator != 0 {
		m, e := rt.getElement(obj, rt.symIterator) // GetMethod(obj, @@iterator)
		if e != nil {
			return mkundef(), e
		}
		if !m.IsNullish() {
			// A present but non-callable @@iterator is a TypeError, not a fallback.
			if !rt.isCallable(m) {
				return mkundef(), rt.typeError("[Symbol.iterator] is not a function")
			}
			it, e := rt.callValue(m, obj, nil)
			if e != nil {
				return mkundef(), e
			}
			if !it.IsObjectType() {
				return mkundef(), rt.typeError("[Symbol.iterator]() returned a non-object")
			}
			return it, nil
		}
	}
	// No (or nullish) @@iterator: obj is itself the iterator (GetIteratorDirect).
	return obj, nil
}

func (rt *Runtime) sliceIterator(vs []Value) Value {
	i := 0
	return rt.newIteratorObject(func() (Value, bool) {
		if i >= len(vs) {
			return mkundef(), true
		}
		v := vs[i]
		i++
		return v, false
	})
}

// initIteratorHelpers installs the ES2025 Iterator global + %IteratorPrototype%
// helper methods (map/filter/take/drop/flatMap/reduce/toArray/forEach/some/
// every/find). Values are materialized eagerly.
func (rt *Runtime) initIteratorHelpers() {
	proto := rt.objPtr(rt.iteratorProto)
	// drain materializes an iterator via GetIteratorDirect (read "next" on the
	// receiver and step it) rather than @@iterator: the reducer-style helpers
	// (toArray/forEach/reduce/some/every/find) operate on `this` as the iterator
	// itself, which need not be iterable.
	drain := func(this Value) ([]Value, *ThrowError) {
		if !this.IsObjectType() {
			return nil, rt.typeError("Iterator.prototype method called on a non-object")
		}
		next, e := rt.getField(this, "next")
		if e != nil {
			return nil, e
		}
		var out []Value
		for {
			v, d, se := rt.iterStepValue(this, next)
			if se != nil {
				return nil, se
			}
			if d {
				break
			}
			out = append(out, v)
		}
		return out, nil
	}

	// %IteratorHelperPrototype%: the [[Prototype]] of a map/filter/take/drop/
	// flatMap result, tagged "Iterator Helper".
	rt.iterHelperProto = rt.newObject(rt.iteratorProto)
	rt.setStringTag(rt.iterHelperProto, "Iterator Helper")

	if rt.symDispose != 0 {
		// Iterator.prototype[@@dispose] closes the iterator (calls its `return`).
		proto.defineOwnSymbol(rt.symDispose.handle(), rt.newNativeFunc("[Symbol.dispose]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if !this.IsObjectType() {
				return mkundef(), rt.typeError("Iterator.prototype[Symbol.dispose] called on a non-object")
			}
			rf, e := rt.getField(this, "return")
			if e != nil {
				return mkundef(), e
			}
			if !rf.IsNullish() {
				if !rt.isCallable(rf) {
					return mkundef(), rt.typeError("'return' is not a function")
				}
				if _, e := rt.callValue(rf, this, nil); e != nil {
					return mkundef(), e
				}
			}
			return mkundef(), nil
		}), attrWritable|attrConfigurable)
	}
	// join(separator): the separator is coerced BEFORE "next" is read, and an
	// abrupt coercion closes the iterator -- so a receiver whose `next` getter
	// throws never has it looked at, and its `return` is still called. Nothing
	// after that closes anything: an exhausted iterator is already done, and an
	// error out of `next` has closed itself.
	rt.defMethod(proto, "join", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Iterator.prototype.join called on a non-object")
		}
		sep := ","
		if sv := arg(args, 0); !sv.IsUndefined() {
			s, e := rt.toStringValue(sv)
			if e != nil {
				rt.iteratorClose(this)
				return mkundef(), e
			}
			sep = rt.strGo(s)
		}
		next, e := rt.getField(this, "next")
		if e != nil {
			return mkundef(), e
		}
		var b []byte
		for first := true; ; first = false {
			v, done, e := rt.iterStepValue(this, next)
			if e != nil {
				return mkundef(), e
			}
			if done {
				return rt.newString(string(b)), nil
			}
			if !first {
				b = append(b, sep...)
			}
			// null and undefined join as nothing at all, the way Array.prototype
			// .join treats a hole.
			if v.IsNullish() {
				continue
			}
			sv, e := rt.toStringValue(v)
			if e != nil {
				rt.iteratorClose(this)
				return mkundef(), e
			}
			b = append(b, rt.strBytes(sv)...)
		}
	})
	rt.defMethod(proto, "toArray", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		res := rt.newArray()
		ro := rt.objPtr(res)
		for i, v := range vs {
			rt.arraySet(ro, uint32(i), v)
		}
		return res, nil
	})
	// The transforming helpers (map/filter/take/drop/flatMap) are lazy: they
	// validate their argument up front (closing the source iterator on failure),
	// read the source's `next` once (GetIteratorDirect), and pull one value per
	// step. A callback that throws closes the source.
	rt.defMethod(proto, "map", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, cb, e := rt.iterHelperCallback(this, arg(args, 0), "map")
		if e != nil {
			return mkundef(), e
		}
		idx, done := 0, false
		return rt.newIteratorObjectE(this, &done, func() (Value, bool, *ThrowError) {
			if done {
				return mkundef(), true, nil
			}
			v, d, e := rt.iterStepValue(this, next)
			if e != nil || d {
				done = true
				return mkundef(), d, e
			}
			r, ce := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(idx))})
			idx++
			if ce != nil {
				done = true
				rt.iteratorClose(this)
				return mkundef(), false, ce
			}
			return r, false, nil
		}, []Value{next, cb}), nil
	})
	rt.defMethod(proto, "filter", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, cb, e := rt.iterHelperCallback(this, arg(args, 0), "filter")
		if e != nil {
			return mkundef(), e
		}
		idx, done := 0, false
		return rt.newIteratorObjectE(this, &done, func() (Value, bool, *ThrowError) {
			for !done {
				v, d, e := rt.iterStepValue(this, next)
				if e != nil || d {
					done = true
					return mkundef(), d, e
				}
				r, ce := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(idx))})
				idx++
				if ce != nil {
					done = true
					rt.iteratorClose(this)
					return mkundef(), false, ce
				}
				if rt.toBoolean(r) {
					return v, false, nil
				}
			}
			return mkundef(), true, nil
		}, []Value{next, cb}), nil
	})
	rt.defMethod(proto, "take", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, limit, e := rt.iterHelperLimit(this, arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		remaining, done := limit, false
		return rt.newIteratorObjectE(this, &done, func() (Value, bool, *ThrowError) {
			if done {
				return mkundef(), true, nil
			}
			if remaining <= 0 {
				done = true
				if ce := rt.iteratorCloseE(this); ce != nil {
					return mkundef(), false, ce
				}
				return mkundef(), true, nil
			}
			remaining--
			v, d, e := rt.iterStepValue(this, next)
			if e != nil || d {
				done = true
				return mkundef(), d, e
			}
			return v, false, nil
		}, []Value{next}), nil
	})
	rt.defMethod(proto, "drop", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, limit, e := rt.iterHelperLimit(this, arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		toDrop, done := limit, false
		return rt.newIteratorObjectE(this, &done, func() (Value, bool, *ThrowError) {
			if done {
				return mkundef(), true, nil
			}
			for toDrop > 0 {
				toDrop--
				_, d, e := rt.iterStepValue(this, next)
				if e != nil || d {
					done = true
					return mkundef(), d, e
				}
			}
			v, d, e := rt.iterStepValue(this, next)
			if e != nil || d {
				done = true
				return mkundef(), d, e
			}
			return v, false, nil
		}, []Value{next}), nil
	})
	// chunks and windows are the only helpers that accumulate their input, so
	// unlike the rest they need somewhere to put it that the collector can see.
	// A Go slice will not do: appending past its capacity reallocates, and
	// holdCaptures only ever recorded the original header, so everything added
	// after the first growth would be invisible. The buffer is a JS array,
	// reached through a one-element slot mutated in place — the same trick
	// flatMap uses for its inner iterator.
	rt.defMethod(proto, "chunks", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Iterator.prototype.chunks called on a non-object")
		}
		size, e := rt.iterChunkSize(this, arg(args, 0), "chunkSize")
		if e != nil {
			return mkundef(), e
		}
		next, e := rt.getField(this, "next") // GetIteratorDirect, after the size is valid
		if e != nil {
			return mkundef(), e
		}
		done := false
		buf := []Value{rt.newArray()}
		return rt.newIteratorObjectE(this, &done, func() (Value, bool, *ThrowError) {
			if done {
				return mkundef(), true, nil
			}
			for {
				v, d, se := rt.iterStepValue(this, next)
				if se != nil {
					done = true
					return mkundef(), false, se
				}
				if d {
					// Running the source dry is not a close: `return` is never read.
					done = true
					if bo := rt.objPtr(buf[0]); bo.arrLen > 0 {
						return buf[0], false, nil // the short final chunk
					}
					return mkundef(), true, nil
				}
				bo := rt.objPtr(buf[0])
				rt.arraySet(bo, bo.arrLen, v)
				if uint64(bo.arrLen) == size {
					full := buf[0]
					buf[0] = rt.newArray() // every chunk is a distinct array
					return full, false, nil
				}
			}
		}, []Value{next}, buf), nil
	})
	rt.defMethod(proto, "windows", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Iterator.prototype.windows called on a non-object")
		}
		size, e := rt.iterChunkSize(this, arg(args, 0), "windowSize")
		if e != nil {
			return mkundef(), e
		}
		// undersized is an exact string, not a coerced one.
		allowPartial := false
		switch u := arg(args, 1); {
		case u.IsUndefined(): // omitted means "only-full"
		case !u.IsString():
			rt.iteratorClose(this)
			return mkundef(), rt.typeError(`undersized must be "only-full" or "allow-partial"`)
		case rt.strGo(u) == "allow-partial":
			allowPartial = true
		case rt.strGo(u) == "only-full":
		default:
			rt.iteratorClose(this)
			return mkundef(), rt.typeError(`undersized must be "only-full" or "allow-partial"`)
		}
		next, e := rt.getField(this, "next") // GetIteratorDirect, after both arguments are valid
		if e != nil {
			return mkundef(), e
		}
		done := false
		buf := []Value{rt.newArray()}
		return rt.newIteratorObjectE(this, &done, func() (Value, bool, *ThrowError) {
			if done {
				return mkundef(), true, nil
			}
			for {
				v, d, se := rt.iterStepValue(this, next)
				if se != nil {
					done = true
					return mkundef(), false, se
				}
				if d {
					done = true
					// A tail too short to fill a window is yielded once, and only
					// when the caller asked for partials.
					if bo := rt.objPtr(buf[0]); allowPartial && bo.arrLen > 0 && uint64(bo.arrLen) < size {
						return rt.copyArrayPrefix(buf[0]), false, nil
					}
					return mkundef(), true, nil
				}
				bo := rt.objPtr(buf[0])
				rt.arraySet(bo, bo.arrLen, v)
				if uint64(bo.arrLen) != size {
					continue
				}
				out := rt.copyArrayPrefix(buf[0])
				// Slide by one. Re-resolve the buffer: allocating the window above
				// is the kind of call after which a stale *object is a bug.
				bo = rt.objPtr(buf[0])
				n := bo.arrLen
				copy(bo.arr[:n-1], bo.arr[1:n])
				bo.arr[n-1] = tEmpty
				bo.arrLen = n - 1
				return out, false, nil
			}
		}, []Value{next}, buf), nil
	})
	rt.defMethod(proto, "flatMap", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, cb, e := rt.iterHelperCallback(this, arg(args, 0), "flatMap")
		if e != nil {
			return mkundef(), e
		}
		idx, done := 0, false
		// The currently-open inner (mapper-result) iterator and its `next`
		// method, in a slice rather than two captured variables: the collector
		// cannot see inside a closure, and these two are reassigned as the
		// helper advances, so a snapshot taken at creation would go stale.
		inner := make([]Value, 2)
		helper := rt.newIteratorObjectE(this, &done, func() (Value, bool, *ThrowError) {
			for !done {
				if inner[1] != 0 {
					iv, id, ie := rt.iterStepValue(inner[0], inner[1])
					if ie != nil {
						done = true
						rt.iteratorClose(this)
						return mkundef(), false, ie
					}
					if !id {
						return iv, false, nil
					}
					inner[1] = 0
				}
				v, d, e := rt.iterStepValue(this, next)
				if e != nil || d {
					done = true
					return mkundef(), d, e
				}
				r, ce := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(idx))})
				idx++
				if ce != nil {
					done = true
					rt.iteratorClose(this)
					return mkundef(), false, ce
				}
				it, ie := rt.getIteratorFlattenable(r)
				if ie != nil {
					done = true
					rt.iteratorClose(this)
					return mkundef(), false, ie
				}
				inner[0] = it
				inner[1], ie = rt.getField(it, "next")
				if ie != nil {
					done = true
					return mkundef(), false, ie
				}
			}
			return mkundef(), true, nil
		}, []Value{next, cb}, inner)
		// flatMap's Return must also close the currently-open inner (mapper-result)
		// iterator, not just the source (IteratorCloseAll over « inner, source »).
		ho := rt.objPtr(helper)
		rt.defMethod(ho, "return", 0, func(rt *Runtime, _ Value, args []Value) (Value, *ThrowError) {
			if !done {
				done = true
				var pending *ThrowError
				if inner[1] != 0 {
					pending = rt.iteratorCloseE(inner[0])
				}
				if ce := rt.iteratorCloseE(this); ce != nil && pending == nil {
					pending = ce
				}
				if pending != nil {
					return mkundef(), pending
				}
			}
			return rt.genResult(arg(args, 0), true), nil
		})
		return helper, nil
	})
	rt.defMethod(proto, "reduce", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, cb, e := rt.iterHelperCallback(this, arg(args, 0), "reduce")
		if e != nil {
			return mkundef(), e
		}
		var acc Value
		idx := 0
		if len(args) >= 2 {
			acc = args[1]
		} else {
			v, d, se := rt.iterStepValue(this, next)
			if se != nil {
				return mkundef(), se
			}
			if d {
				return mkundef(), rt.typeError("Reduce of empty iterator with no initial value")
			}
			acc, idx = v, 1
		}
		for {
			v, d, se := rt.iterStepValue(this, next)
			if se != nil {
				return mkundef(), se
			}
			if d {
				return acc, nil
			}
			r, ce := rt.callValue(cb, mkundef(), []Value{acc, v, mknum(float64(idx))})
			idx++
			if ce != nil {
				rt.iteratorClose(this)
				return mkundef(), ce
			}
			acc = r
		}
	})
	rt.defMethod(proto, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, cb, e := rt.iterHelperCallback(this, arg(args, 0), "forEach")
		if e != nil {
			return mkundef(), e
		}
		idx := 0
		for {
			v, d, se := rt.iterStepValue(this, next)
			if se != nil {
				return mkundef(), se
			}
			if d {
				return mkundef(), nil
			}
			_, ce := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(idx))})
			idx++
			if ce != nil {
				rt.iteratorClose(this)
				return mkundef(), ce
			}
		}
	})
	rt.defMethod(proto, "some", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, cb, e := rt.iterHelperCallback(this, arg(args, 0), "some")
		if e != nil {
			return mkundef(), e
		}
		idx := 0
		for {
			v, d, se := rt.iterStepValue(this, next)
			if se != nil {
				return mkundef(), se
			}
			if d {
				return mkfalse(), nil
			}
			r, ce := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(idx))})
			idx++
			if ce != nil {
				rt.iteratorClose(this)
				return mkundef(), ce
			}
			if rt.toBoolean(r) {
				if ce := rt.iteratorCloseE(this); ce != nil {
					return mkundef(), ce
				}
				return mktrue(), nil
			}
		}
	})
	rt.defMethod(proto, "every", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, cb, e := rt.iterHelperCallback(this, arg(args, 0), "every")
		if e != nil {
			return mkundef(), e
		}
		idx := 0
		for {
			v, d, se := rt.iterStepValue(this, next)
			if se != nil {
				return mkundef(), se
			}
			if d {
				return mktrue(), nil
			}
			r, ce := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(idx))})
			idx++
			if ce != nil {
				rt.iteratorClose(this)
				return mkundef(), ce
			}
			if !rt.toBoolean(r) {
				if ce := rt.iteratorCloseE(this); ce != nil {
					return mkundef(), ce
				}
				return mkfalse(), nil
			}
		}
	})
	rt.defMethod(proto, "find", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, cb, e := rt.iterHelperCallback(this, arg(args, 0), "find")
		if e != nil {
			return mkundef(), e
		}
		idx := 0
		for {
			v, d, se := rt.iterStepValue(this, next)
			if se != nil {
				return mkundef(), se
			}
			if d {
				return mkundef(), nil
			}
			r, ce := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(idx))})
			idx++
			if ce != nil {
				rt.iteratorClose(this)
				return mkundef(), ce
			}
			if rt.toBoolean(r) {
				if ce := rt.iteratorCloseE(this); ce != nil {
					return mkundef(), ce
				}
				return v, nil
			}
		}
	})
	rt.defMethod(proto, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			return mkundef(), rt.typeError("Iterator.prototype.includes called on a non-object")
		}
		toSkip, e := rt.iterSkipCount(this, arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		next, e := rt.getField(this, "next") // GetIteratorDirect, after the count is valid
		if e != nil {
			return mkundef(), e
		}
		search, skipped := arg(args, 0), 0.0
		for {
			v, d, se := rt.iterStepValue(this, next)
			if se != nil {
				return mkundef(), se
			}
			if d {
				// Running the source dry is not a close: `return` is never read.
				return mkfalse(), nil
			}
			if skipped < toSkip { // never true once toSkip is 0; always true for +∞
				skipped++
				continue
			}
			if rt.sameValueZero(search, v) {
				if ce := rt.iteratorCloseE(this); ce != nil {
					return mkundef(), ce
				}
				return mktrue(), nil
			}
		}
	})

	// Iterator global: an abstract constructor (throws unless subclassed) whose
	// prototype is %IteratorPrototype%; Iterator.from(x).
	var ctor Value
	ctor = rt.newNativeFunc("Iterator", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Direct `new Iterator()` / a plain call is a TypeError; a subclass
		// (new.target ≠ Iterator) constructs normally. new.target comes from the
		// construct (pendingNewTarget) or a super() call (activeNewTarget).
		nt := rt.pendingNewTarget
		if nt.IsUndefined() {
			nt = rt.activeNewTarget
		}
		if nt.IsUndefined() || nt == ctor {
			return mkundef(), rt.typeError("Abstract class Iterator not directly constructable")
		}
		return this, nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.iteratorProto, 0)
	// %Iterator.prototype%.constructor is an accessor (Iterator Helpers): the
	// getter always yields %Iterator%, and the setter installs an own data
	// property on the receiver (SetterThatIgnoresPrototypeProperties), throwing
	// when applied to the prototype itself.
	ctorGet := rt.newNativeFunc("get constructor", 0, func(rt *Runtime, _ Value, _ []Value) (Value, *ThrowError) {
		return ctor, nil
	})
	ctorSet := rt.newNativeFunc("set constructor", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkundef(), rt.setterIgnoresPrototype(this, rt.iteratorProto, rt.internString("constructor"), arg(args, 0))
	})
	proto.defineAccessor("constructor", ctorGet, ctorSet, true, true, attrConfigurable)
	// %WrapForValidIteratorPrototype%: the shared [[Prototype]] of the wrappers
	// Iterator.from returns for a foreign iterator (chains to %Iterator.prototype%).
	wrapProto := rt.newObject(rt.iteratorProto)
	rt.defMethod(cobj, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		O := arg(args, 0)
		// GetIteratorFlattenable(O, iterate-string-primitives): non-object non-string
		// primitives are rejected; a String is iterated via its wrapper prototype.
		if !O.IsObjectType() && !O.IsString() {
			return mkundef(), rt.typeError("Iterator.from called on a non-iterable primitive")
		}
		method := mkundef()
		if rt.symIterator != 0 {
			m, e := rt.getElement(O, rt.symIterator) // GetMethod(O, @@iterator) (boxes a String)
			if e != nil {
				return mkundef(), e
			}
			method = m
		}
		var iterator Value
		if method.IsNullish() {
			iterator = O // no @@iterator: O is itself the iterator
		} else if !rt.isCallable(method) {
			return mkundef(), rt.typeError("Iterator.from: @@iterator is not callable")
		} else {
			it, e := rt.callValue(method, O, nil)
			if e != nil {
				return mkundef(), e
			}
			if !it.IsObjectType() {
				return mkundef(), rt.typeError("Iterator.from: @@iterator returned a non-object")
			}
			iterator = it
		}
		// GetIteratorDirect: read "next" exactly once.
		nextMethod, e := rt.getField(iterator, "next")
		if e != nil {
			return mkundef(), e
		}
		// An iterator that already inherits %Iterator.prototype% is returned as-is.
		if rt.hasInProtoChain(iterator, rt.iteratorProto) {
			return iterator, nil
		}
		// Otherwise wrap it so the Iterator helpers apply. next() forwards to the
		// captured next method; return() forwards to the underlying iterator's
		// return (undefined → a done result).
		w := rt.newObject(wrapProto)
		wo := rt.objPtr(w)
		itr, nm := iterator, nextMethod
		rt.defMethod(wo, "next", 0, func(rt *Runtime, _ Value, _ []Value) (Value, *ThrowError) {
			return rt.callValue(nm, itr, nil)
		})
		rt.defMethod(wo, "return", 0, func(rt *Runtime, _ Value, _ []Value) (Value, *ThrowError) {
			rf, e := rt.getField(itr, "return")
			if e != nil {
				return mkundef(), e
			}
			if rf.IsNullish() {
				return rt.genResult(mkundef(), true), nil
			}
			if !rt.isCallable(rf) {
				return mkundef(), rt.typeError("Iterator.from: return is not callable")
			}
			return rt.callValue(rf, itr, nil)
		})
		return w, nil
	})
	rt.defMethod(cobj, "concat", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Eagerly validate every argument (in order): each must be an Object with
		// a callable %Symbol.iterator% method. The method is fetched now but the
		// inner iterators are opened lazily, one segment at a time.
		type openIter struct{ iterable, method Value }
		iterables := make([]openIter, 0, len(args))
		// The same values, flat, so the collector can find them: they live only
		// in the closures below and a func value is opaque. See holdCaptures.
		held := make([]Value, 0, 2*len(args))
		for _, item := range args {
			if !item.IsObjectType() {
				return mkundef(), rt.typeError("Iterator.concat argument is not an object")
			}
			m, e := rt.getElement(item, rt.symIterator) // GetMethod(item, @@iterator)
			if e != nil {
				return mkundef(), e
			}
			if m.IsNullish() || !rt.isCallable(m) {
				return mkundef(), rt.typeError("Iterator.concat argument is not iterable")
			}
			iterables = append(iterables, openIter{item, m})
			held = append(held, item, m)
		}
		idx := 0
		done := false
		running := false // guards against re-entrant next/return (generator "executing")
		// The open inner iterator and its `next`, in a slice rather than two
		// captured variables: they are reassigned per segment, so a snapshot
		// taken at creation would go stale. inner[0] is 0 when none is open.
		inner := make([]Value, 2)
		next := func() (Value, bool, *ThrowError) {
			for !done {
				if inner[1] != 0 {
					v, d, e := rt.iterStepValue(inner[0], inner[1])
					if e != nil {
						done = true
						return mkundef(), false, e
					}
					if !d {
						return v, false, nil
					}
					inner[0], inner[1] = 0, 0 // segment exhausted (already closed by IteratorStep)
				}
				if idx >= len(iterables) {
					done = true
					return mkundef(), true, nil
				}
				it := iterables[idx]
				idx++
				iter, e := rt.callValue(it.method, it.iterable, nil) // Call(method, iterable)
				if e != nil {
					done = true
					return mkundef(), false, e
				}
				if !iter.IsObjectType() {
					done = true
					return mkundef(), false, rt.typeError("Iterator.concat: [Symbol.iterator]() returned a non-object")
				}
				nx, e := rt.getField(iter, "next") // GetIteratorDirect: read next once
				if e != nil {
					done = true
					return mkundef(), false, e
				}
				inner[0], inner[1] = iter, nx
			}
			return mkundef(), true, nil
		}
		proto := rt.iteratorProto
		if rt.iterHelperProto != 0 {
			proto = rt.iterHelperProto
		}
		hv := rt.newObject(proto)
		ho := rt.objPtr(hv)
		rt.holdCaptures(hv, held, inner)
		rt.defMethod(ho, "next", 0, func(rt *Runtime, _ Value, _ []Value) (Value, *ThrowError) {
			if running {
				return mkundef(), rt.typeError("Iterator.concat generator is already running")
			}
			running = true
			val, d, e := next()
			running = false
			if e != nil {
				return mkundef(), e
			}
			return rt.genResult(val, d), nil
		})
		rt.defMethod(ho, "return", 0, func(rt *Runtime, _ Value, args []Value) (Value, *ThrowError) {
			if running {
				return mkundef(), rt.typeError("Iterator.concat generator is already running")
			}
			// Forward Return to the currently-open inner iterator only; before the
			// first segment starts or after exhaustion there is nothing to close.
			if !done {
				done = true
				if inner[0] != 0 {
					running = true
					e := rt.iteratorCloseE(inner[0])
					running = false
					if e != nil {
						return mkundef(), e
					}
				}
			}
			return rt.genResult(arg(args, 0), true), nil
		})
		return hv, nil
	})
	rt.defMethod(cobj, "zip", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iterablesArg := arg(args, 0)
		if !iterablesArg.IsObjectType() {
			return mkundef(), rt.typeError("Iterator.zip called on a non-object")
		}
		mode, paddingOption, e := rt.zipParseOptions(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		// Gather the sub-iterators by iterating `iterables` and flattening each.
		inputIter, e := rt.getSyncIterator(iterablesArg)
		if e != nil {
			return mkundef(), e
		}
		// Rooted from here, not from newZipIterator's holdCaptures at the end.
		// Everything below can run user JavaScript — a `next`, a Symbol.iterator,
		// a getter — and until the helper exists these are the only reference to
		// the input iterator, the sub-iterators, and their `next` methods.
		drv := rt.beginDriver(inputIter)
		defer rt.endDriver(drv)
		inputNext, e := rt.getField(inputIter, "next")
		if e != nil {
			return mkundef(), e
		}
		*drv = append(*drv, inputNext)
		var iters, nexts []Value
		for {
			v, d, se := rt.iterStepValue(inputIter, inputNext)
			if se != nil {
				rt.closeIterList(iters, -1)
				return mkundef(), se
			}
			if d {
				break
			}
			it, fe := rt.getIteratorFlattenable(v)
			if fe != nil {
				// IfAbruptCloseIterators(iter, « inputIterator » + iters): reverse
				// order closes the gathered iters first, then the input iterator.
				rt.closeIterList(iters, -1)
				rt.iteratorCloseE(inputIter)
				return mkundef(), fe
			}
			nx, ne := rt.getField(it, "next")
			if ne != nil {
				rt.closeIterList(iters, -1)
				rt.iteratorCloseE(inputIter)
				return mkundef(), ne
			}
			iters = append(iters, it)
			nexts = append(nexts, nx)
			*drv = append(*drv, it, nx)
		}
		padding, pe := rt.zipResolvePadding(mode, paddingOption, len(iters))
		if pe != nil {
			rt.closeIterList(iters, -1) // close the gathered iterators; pe (a throw) wins
			return mkundef(), pe
		}
		finish := func(results []Value) Value {
			arr := rt.newArray()
			ao := rt.objPtr(arr)
			for i, v := range results {
				rt.arraySet(ao, uint32(i), v)
			}
			return arr
		}
		return rt.newZipIterator(iters, nexts, mode, padding, finish), nil
	})
	rt.defMethod(cobj, "zipKeyed", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iterablesArg := arg(args, 0)
		if !iterablesArg.IsObjectType() {
			return mkundef(), rt.typeError("Iterator.zipKeyed called on a non-object")
		}
		mode, paddingOption, e := rt.zipParseOptions(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		// Gather (key, iterator) for each own enumerable property, in key order.
		allKeys, e := rt.objectOwnKeys(iterablesArg)
		if e != nil {
			return mkundef(), e
		}
		// Same window as zip, and wider: reading a property here can run a
		// getter, so everything gathered below is held across user JavaScript
		// until the helper exists to own it. The keys included — they are Go
		// values too, and `finish` is the only other thing that will hold them.
		drv := rt.beginDriver(allKeys...)
		defer rt.endDriver(drv)
		var keys, iters, nexts, padding []Value
		for _, key := range allKeys {
			en, exists, de := rt.ownKeyEnumerable(iterablesArg, key)
			if de != nil {
				rt.closeIterList(iters, -1)
				return mkundef(), de
			}
			if !exists || !en {
				continue
			}
			value, ge := rt.getElement(iterablesArg, key) // Get(iterables, key)
			if ge != nil {
				rt.closeIterList(iters, -1)
				return mkundef(), ge
			}
			if value.IsUndefined() {
				continue // an undefined property value contributes no key to the zip
			}
			it, fe := rt.getIteratorFlattenable(value)
			if fe != nil {
				rt.closeIterList(iters, -1)
				return mkundef(), fe
			}
			nx, ne := rt.getField(it, "next")
			if ne != nil {
				rt.closeIterList(iters, -1)
				return mkundef(), ne
			}
			keys = append(keys, key)
			iters = append(iters, it)
			nexts = append(nexts, nx)
			*drv = append(*drv, key, it, nx)
		}
		// Longest-mode padding is read by key from the padding object (Get), not
		// drained from an iterator.
		padding = make([]Value, len(iters))
		for i := range padding {
			padding[i] = mkundef()
		}
		if mode == "longest" && !paddingOption.IsUndefined() {
			for i, key := range keys {
				pv, pe := rt.getElement(paddingOption, key)
				if pe != nil {
					rt.closeIterList(iters, -1)
					return mkundef(), pe
				}
				padding[i] = pv
				*drv = append(*drv, pv)
			}
		}
		finish := func(results []Value) Value {
			obj := rt.newObject(mknull())
			oo := rt.objPtr(obj)
			for i, key := range keys {
				if key.IsSymbol() {
					oo.defineOwnSymbol(key.handle(), results[i], attrDefault)
				} else {
					name, _ := rt.propKeyString(key)
					oo.defineOwn(name, results[i], attrDefault)
				}
			}
			return obj
		}
		hv := rt.newZipIterator(iters, nexts, mode, padding, finish)
		// The property keys live only in `finish`, which is a Go closure.
		rt.holdCaptures(hv, keys)
		return hv, nil
	})
	if rt.symToStringTag != 0 {
		// %Iterator.prototype%[@@toStringTag] is likewise an accessor: getter
		// returns "Iterator", setter is SetterThatIgnoresPrototypeProperties.
		ttKey := mkval(TSymbol, uint64(rt.symToStringTag.handle()))
		ttGet := rt.newNativeFunc("get [Symbol.toStringTag]", 0, func(rt *Runtime, _ Value, _ []Value) (Value, *ThrowError) {
			return rt.newString("Iterator"), nil
		})
		ttSet := rt.newNativeFunc("set [Symbol.toStringTag]", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return mkundef(), rt.setterIgnoresPrototype(this, rt.iteratorProto, ttKey, arg(args, 0))
		})
		proto.defineAccessorSymbol(rt.symToStringTag.handle(), ttGet, ttSet, true, true, attrConfigurable)
	}
	rt.defGlobal("Iterator", ctor)
}

// zipParseOptions reads the { mode, padding } options object for Iterator.zip /
// zipKeyed. options may be undefined (GetOptionsObject); mode defaults to
// "shortest" and must be one of shortest/longest/strict; padding is only read in
// longest mode and, if present, must be an Object.
func (rt *Runtime) zipParseOptions(optionsArg Value) (mode string, paddingOption Value, err *ThrowError) {
	if optionsArg.IsUndefined() {
		return "shortest", mkundef(), nil
	}
	if !optionsArg.IsObjectType() {
		return "", mkundef(), rt.typeError("Iterator.zip options is not an object")
	}
	mv, e := rt.getField(optionsArg, "mode")
	if e != nil {
		return "", mkundef(), e
	}
	mode = "shortest"
	if !mv.IsUndefined() {
		if !mv.IsString() {
			return "", mkundef(), rt.typeError("Iterator.zip mode is invalid")
		}
		s := rt.strGo(mv)
		if s != "shortest" && s != "longest" && s != "strict" {
			return "", mkundef(), rt.typeError("Iterator.zip mode is invalid")
		}
		mode = s
	}
	if mode == "longest" {
		pv, pe := rt.getField(optionsArg, "padding")
		if pe != nil {
			return "", mkundef(), pe
		}
		if !pv.IsUndefined() && !pv.IsObjectType() {
			return "", mkundef(), rt.typeError("Iterator.zip padding is not an object")
		}
		paddingOption = pv
	}
	return mode, paddingOption, nil
}

// zipResolvePadding produces the per-index padding List for longest mode: all
// undefined when no padding object was given, otherwise drained from the padding
// iterable (missing entries → undefined), which is then closed. Non-longest modes
// need no padding.
func (rt *Runtime) zipResolvePadding(mode string, paddingOption Value, iterCount int) ([]Value, *ThrowError) {
	padding := make([]Value, iterCount)
	for i := range padding {
		padding[i] = mkundef()
	}
	if mode != "longest" || paddingOption.IsUndefined() {
		return padding, nil
	}
	pit, e := rt.getSyncIterator(paddingOption)
	if e != nil {
		return nil, e
	}
	// The padding iterator, its `next`, and the values drained so far are held
	// in Go locals and nowhere else, across a call that runs user JavaScript.
	//
	// test262's basic-longest.js passes an object whose Symbol.iterator is a
	// generator looping `while (true)`, and a loop back edge is a safepoint that
	// collects — so the iterator this function is in the middle of driving was
	// swept out from under it. It surfaced far away, as ordinary JavaScript
	// calling a value that was no longer a function, with no zip frame anywhere
	// on the stack to point at the cause.
	d := rt.beginDriver(pit)
	defer rt.endDriver(d)
	pnext, e := rt.getField(pit, "next")
	if e != nil {
		return nil, e
	}
	*d = append(*d, pnext)
	for i := 0; i < iterCount; i++ {
		v, de, se := rt.iterStepValue(pit, pnext)
		if se != nil {
			return nil, se
		}
		if de {
			return padding, nil // remaining stay undefined; iterator already exhausted
		}
		padding[i] = v
		*d = append(*d, v)
	}
	// Not exhausted: close it. A close error (normal completion) propagates.
	if ce := rt.iteratorCloseE(pit); ce != nil {
		return nil, ce
	}
	return padding, nil
}

// closeIterList closes each iterator in the list (skipping index skip and any
// that error), swallowing close errors — used to unwind partially-gathered
// iterators after an abrupt completion whose original error must win.
func (rt *Runtime) closeIterList(iters []Value, skip int) {
	for i := len(iters) - 1; i >= 0; i-- {
		if i == skip {
			continue
		}
		rt.iteratorCloseE(iters[i])
	}
}

// newZipIterator builds the lazy iterator-helper that drives IteratorZip over the
// gathered sub-iterators under the given mode/padding, formatting each round with
// finish (an array for zip, a keyed object for zipKeyed).
func (rt *Runtime) newZipIterator(iters, nexts []Value, mode string, padding []Value, finish func([]Value) Value) Value {
	iterCount := len(iters)
	open := make([]bool, iterCount)
	for i := range open {
		open[i] = true
	}
	openCount := iterCount
	isNull := make([]bool, iterCount) // longest: exhausted → yields padding[i]
	done := false
	running := false
	yielded := false // true once a value has been yielded (suspended-yield vs -start)

	// closeRemaining closes every still-open iterator except `skip`, in reverse
	// (IteratorCloseAll). A close error is adopted only while the pending
	// completion is normal (nil); once the completion is a throw, later close
	// errors are ignored (IteratorClose returns the incoming throw unchanged).
	closeRemaining := func(skip int, pending *ThrowError) *ThrowError {
		for i := iterCount - 1; i >= 0; i-- {
			if i == skip || !open[i] {
				continue
			}
			open[i] = false
			if ce := rt.iteratorCloseE(iters[i]); ce != nil && pending == nil {
				pending = ce
			}
		}
		return pending
	}

	next := func() (Value, bool, *ThrowError) {
		if done {
			return mkundef(), true, nil
		}
		results := make([]Value, iterCount)
		for i := 0; i < iterCount; i++ {
			if isNull[i] {
				results[i] = padding[i]
				continue
			}
			v, d, e := rt.iterStepValue(iters[i], nexts[i])
			if e != nil {
				done = true
				open[i] = false
				return mkundef(), false, closeRemaining(-1, e) // step throw wins; close errors ignored
			}
			if !d {
				results[i] = v
				continue
			}
			// iters[i] is exhausted.
			open[i] = false
			openCount--
			switch mode {
			case "shortest":
				done = true
				if ce := closeRemaining(-1, nil); ce != nil {
					return mkundef(), false, ce
				}
				return mkundef(), true, nil
			case "strict":
				done = true
				if i != 0 {
					mismatch := rt.typeError("Iterator.zip: strict mode requires equal-length iterators")
					return mkundef(), false, closeRemaining(-1, mismatch)
				}
				// iters[0] finished first: every other iterator must finish now too.
				for k := 1; k < iterCount; k++ {
					_, d2, e2 := rt.iterStepValue(iters[k], nexts[k])
					if e2 != nil {
						open[k] = false
						return mkundef(), false, closeRemaining(-1, e2)
					}
					if !d2 {
						mismatch := rt.typeError("Iterator.zip: strict mode requires equal-length iterators")
						return mkundef(), false, closeRemaining(-1, mismatch)
					}
					open[k] = false
				}
				return mkundef(), true, nil
			default: // longest
				isNull[i] = true
				results[i] = padding[i]
			}
		}
		if openCount == 0 {
			done = true
			return mkundef(), true, nil
		}
		yielded = true
		return finish(results), false, nil
	}

	proto := rt.iteratorProto
	if rt.iterHelperProto != 0 {
		proto = rt.iterHelperProto
	}
	hv := rt.newObject(proto)
	ho := rt.objPtr(hv)
	// The sub-iterators, their `next` methods and the padding exist only inside
	// the closures below, which the collector cannot look into. See holdCaptures.
	rt.holdCaptures(hv, iters, nexts, padding)
	rt.defMethod(ho, "next", 0, func(rt *Runtime, _ Value, _ []Value) (Value, *ThrowError) {
		if running {
			return mkundef(), rt.typeError("Iterator.zip generator is already running")
		}
		running = true
		val, d, e := next()
		running = false
		if e != nil {
			return mkundef(), e
		}
		return rt.genResult(val, d), nil
	})
	rt.defMethod(ho, "return", 0, func(rt *Runtime, _ Value, args []Value) (Value, *ThrowError) {
		if running {
			return mkundef(), rt.typeError("Iterator.zip generator is already running")
		}
		// A return from suspended-start moves straight to "completed", so a
		// re-entrant next/return during the close observes a done generator. A
		// return from suspended-yield resumes the body ("executing"), so a
		// re-entrant call during the close is a TypeError (running guard).
		if !done {
			done = true
			if yielded {
				running = true
			}
			ce := closeRemaining(-1, nil)
			running = false
			if ce != nil {
				return mkundef(), ce
			}
		}
		return rt.genResult(arg(args, 0), true), nil
	})
	return hv
}

// wrapIterator wraps a raw iterator (an object with a next method) so it
// inherits %IteratorPrototype% and thus the Iterator helpers, delegating
// next()/return() to the underlying iterator (%WrapForValidIteratorPrototype%).
func (rt *Runtime) wrapIterator(src Value) Value {
	wrap := rt.newObject(rt.iteratorProto)
	o := rt.objPtr(wrap)
	rt.holdCaptures(wrap, []Value{src})
	rt.defMethod(o, "next", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		nx, e := rt.getField(src, "next")
		if e != nil {
			return mkundef(), e
		}
		return rt.callValue(nx, src, args)
	})
	rt.defMethod(o, "return", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		rf, e := rt.getField(src, "return")
		if e != nil {
			return mkundef(), e
		}
		if rt.isCallable(rf) {
			return rt.callValue(rf, src, args)
		}
		return rt.genResult(arg(args, 0), true), nil
	})
	return wrap
}

// newIteratorObject wraps a producer closure as a spec IteratorResult-yielding
// iterator. next returns (value, done); once done it must keep returning done.
func (rt *Runtime) newIteratorObject(next func() (Value, bool)) Value {
	return rt.newIteratorObjectP(rt.iteratorProto, next)
}

// newIteratorObjectP is newIteratorObject with an explicit [[Prototype]] (one of
// the per-kind %…IteratorPrototype% objects).
func (rt *Runtime) newIteratorObjectP(proto Value, next func() (Value, bool)) Value {
	v := rt.newObject(proto)
	o := rt.objPtr(v)
	rt.defMethod(o, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		val, done := next()
		return rt.genResult(val, done), nil
	})
	return v
}

// iterKind selects what an index iterator yields.
type iterKind int

const (
	iterValues iterKind = iota
	iterKeys
	iterEntries
)

// newIndexIterator builds an Array/TypedArray iterator over live length.
// arrIterState is the internal state of an Array/TypedArray iterator: its source,
// current index, and kind. Held in rt.arrIterStates keyed by the iterator object.
type arrIterState struct {
	src   Value
	index int
	kind  iterKind
	done  bool // exhausted: the spec detaches the source, so later growth isn't seen
}

func (rt *Runtime) newIndexIterator(src Value, kind iterKind) Value {
	v := rt.newObject(rt.arrayIterProto)
	if rt.arrIterStates == nil {
		rt.arrIterStates = map[*object]*arrIterState{}
	}
	rt.arrIterStates[rt.objPtr(v)] = &arrIterState{src: src, kind: kind}
	return v
}

// arrIterNext is the shared %ArrayIteratorPrototype%.next: it reads the receiver's
// iteration state (a TypeError if absent — the missing-brand check) and yields the
// next index/value/entry over the source's live length.
func (rt *Runtime) arrIterNext(this Value) (Value, *ThrowError) {
	st := rt.arrIterStates[rt.objPtr(this)]
	if st == nil {
		return mkundef(), rt.typeError("Array Iterator.prototype.next called on an incompatible receiver")
	}
	if st.done {
		return rt.genResult(mkundef(), true), nil
	}
	// A TypedArray source whose buffer has been detached (or shrunk out of bounds)
	// throws a TypeError when stepped (23.1.5.2.1 step 8).
	if o := rt.objPtr(st.src); o != nil && o.ta != nil && rt.taOutOfBounds(o) {
		return mkundef(), rt.typeError("Cannot iterate a TypedArray backed by a detached buffer")
	}
	n, _ := rt.lengthOf(st.src)
	if st.index >= n {
		st.done = true
		return rt.genResult(mkundef(), true), nil
	}
	idx := st.index
	st.index++
	switch st.kind {
	case iterKeys:
		return rt.genResult(mknum(float64(idx)), false), nil
	case iterEntries:
		el, e := rt.getElement(st.src, mknum(float64(idx)))
		if e != nil {
			return mkundef(), e
		}
		pair := rt.newArray()
		po := rt.objPtr(pair)
		rt.arraySet(po, 0, mknum(float64(idx)))
		rt.arraySet(po, 1, el)
		return rt.genResult(pair, false), nil
	default:
		el, e := rt.getElement(st.src, mknum(float64(idx)))
		if e != nil {
			return mkundef(), e
		}
		return rt.genResult(el, false), nil
	}
}

// newCollectionIterator builds a Map/Set iterator over a snapshot of entries.
// collIterState is the internal state of a Set/Map iterator: its target
// collection, the current index, and the iteration kind. Held in rt.collIterStates
// (keyed by the iterator object) so a shared prototype next can read it.
type collIterState struct {
	c     *collection
	index int
	kind  iterKind
	done  bool // exhausted: the spec detaches [[IteratedSet]], so later adds aren't seen
}

func (rt *Runtime) newCollectionIterator(c *collection, kind iterKind, proto Value) Value {
	v := rt.newObject(proto)
	if rt.collIterStates == nil {
		rt.collIterStates = map[*object]*collIterState{}
	}
	rt.collIterStates[rt.objPtr(v)] = &collIterState{c: c, kind: kind}
	return v
}

// collIterNext is the shared %SetIteratorPrototype%/%MapIteratorPrototype% next.
// wantSet brand-checks the receiver: a Set-iterator next rejects a Map iterator
// (and vice versa) and any non-iterator, throwing a TypeError.
func (rt *Runtime) collIterNext(this Value, wantSet bool) (Value, *ThrowError) {
	st := rt.collIterStates[rt.objPtr(this)]
	if st == nil || st.c.isSet != wantSet {
		return mkundef(), rt.typeError("next called on an incompatible iterator receiver")
	}
	if st.done {
		return rt.genResult(mkundef(), true), nil
	}
	c := st.c
	for st.index < len(c.keys) {
		if c.keys[st.index].IsEmpty() {
			st.index++
			continue
		}
		k, val := c.keys[st.index], c.vals[st.index]
		st.index++
		switch st.kind {
		case iterKeys:
			return rt.genResult(k, false), nil
		case iterValues:
			if c.isSet {
				return rt.genResult(k, false), nil
			}
			return rt.genResult(val, false), nil
		default: // entries
			pair := rt.newArray()
			po := rt.objPtr(pair)
			rt.arraySet(po, 0, k)
			if c.isSet {
				rt.arraySet(po, 1, k)
			} else {
				rt.arraySet(po, 1, val)
			}
			return rt.genResult(pair, false), nil
		}
	}
	st.done = true
	return rt.genResult(mkundef(), true), nil
}
