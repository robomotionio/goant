package engine

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
}

// sliceIterator returns an iterator object over a fixed slice of values.
// newIteratorObjectE builds a lazy iterator helper result whose next step may
// throw. Its `return` closes the source iterator (forwarding the close) and
// marks the helper done; `done` is shared with the step closure so an exhausted
// or already-returned helper does not re-close the source.
func (rt *Runtime) newIteratorObjectE(source Value, done *bool, next func() (Value, bool, *ThrowError)) Value {
	v := rt.newObject(rt.iteratorProto)
	o := rt.objPtr(v)
	rt.defMethod(o, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		val, d, e := next()
		if e != nil {
			return mkundef(), e
		}
		return rt.genResult(val, d), nil
	})
	rt.defMethod(o, "return", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !*done {
			*done = true
			rt.iteratorClose(source)
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

// iterHelperLimit validates the receiver and a numeric limit for take/drop:
// GetIteratorDirect, then ToNumber(limit) (closing the source on an abrupt
// completion), rejecting NaN and negatives with a RangeError (closing first).
// +∞ maps to an unbounded limit.
func (rt *Runtime) iterHelperLimit(this, limitArg Value) (Value, int, *ThrowError) {
	if !this.IsObjectType() {
		return mkundef(), 0, rt.typeError("Iterator.prototype method called on a non-object")
	}
	next, e := rt.getField(this, "next")
	if e != nil {
		return mkundef(), 0, e
	}
	num, e := rt.toNumber(limitArg)
	if e != nil {
		rt.iteratorClose(this)
		return mkundef(), 0, e
	}
	if num != num { // NaN
		rt.iteratorClose(this)
		return mkundef(), 0, rt.rangeError("limit must not be NaN")
	}
	if num < 0 {
		rt.iteratorClose(this)
		return mkundef(), 0, rt.rangeError("limit must not be negative")
	}
	limit := int(^uint(0) >> 1) // +∞ (or huge) → effectively unbounded
	if num < float64(limit) {
		limit = int(num)
	}
	return next, limit, nil
}

// getIteratorFlattenable implements GetIteratorFlattenable(obj, reject-primitives)
// for flatMap: obj must be an Object; if it has a callable @@iterator, call it,
// otherwise use obj itself as the iterator.
func (rt *Runtime) getIteratorFlattenable(obj Value) (Value, *ThrowError) {
	if !obj.IsObjectType() {
		return mkundef(), rt.typeError("flatMap callback did not return an object")
	}
	if rt.symIterator != 0 {
		m, e := rt.getElement(obj, rt.symIterator)
		if e != nil {
			return mkundef(), e
		}
		if rt.isCallable(m) {
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
	drain := func(this Value) ([]Value, *ThrowError) { return rt.iterableValues(this) }

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
		}), nil
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
		}), nil
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
				rt.iteratorClose(this)
				return mkundef(), true, nil
			}
			remaining--
			v, d, e := rt.iterStepValue(this, next)
			if e != nil || d {
				done = true
				return mkundef(), d, e
			}
			return v, false, nil
		}), nil
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
		}), nil
	})
	rt.defMethod(proto, "flatMap", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		next, cb, e := rt.iterHelperCallback(this, arg(args, 0), "flatMap")
		if e != nil {
			return mkundef(), e
		}
		idx, done := 0, false
		var innerNext Value // the current inner iterator's next method (0 when none)
		var inner Value
		return rt.newIteratorObjectE(this, &done, func() (Value, bool, *ThrowError) {
			for !done {
				if innerNext != 0 {
					iv, id, ie := rt.iterStepValue(inner, innerNext)
					if ie != nil {
						done = true
						rt.iteratorClose(this)
						return mkundef(), false, ie
					}
					if !id {
						return iv, false, nil
					}
					innerNext = 0
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
				inner = it
				innerNext, ie = rt.getField(it, "next")
				if ie != nil {
					done = true
					return mkundef(), false, ie
				}
			}
			return mkundef(), true, nil
		}), nil
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
				rt.iteratorClose(this)
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
				rt.iteratorClose(this)
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
				rt.iteratorClose(this)
				return v, nil
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
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defMethod(cobj, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		src := arg(args, 0)
		if !src.IsObjectType() {
			vs, e := rt.iterableValues(src)
			if e != nil {
				return mkundef(), e
			}
			return rt.sliceIterator(vs), nil
		}
		// Resolve the underlying iterator (via @@iterator when present).
		it := src
		if rt.symIterator != 0 {
			if m, _ := rt.getElement(src, rt.symIterator); rt.isCallable(m) {
				r, e := rt.callValue(m, src, nil)
				if e != nil {
					return mkundef(), e
				}
				it = r
			}
		}
		// If it already inherits %IteratorPrototype%, return it; else wrap so the
		// Iterator helpers apply (%WrapForValidIteratorPrototype%).
		if rt.hasInProtoChain(it, rt.iteratorProto) {
			return it, nil
		}
		return rt.wrapIterator(it), nil
	})
	if rt.symToStringTag != 0 {
		proto.defineOwnSymbol(rt.symToStringTag.handle(), rt.newString("Iterator"), attrConfigurable)
	}
	rt.defGlobal("Iterator", ctor)
}

// wrapIterator wraps a raw iterator (an object with a next method) so it
// inherits %IteratorPrototype% and thus the Iterator helpers, delegating
// next()/return() to the underlying iterator (%WrapForValidIteratorPrototype%).
func (rt *Runtime) wrapIterator(src Value) Value {
	wrap := rt.newObject(rt.iteratorProto)
	o := rt.objPtr(wrap)
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
func (rt *Runtime) newIndexIterator(src Value, kind iterKind) Value {
	i := 0
	return rt.newIteratorObjectP(rt.arrayIterProto, func() (Value, bool) {
		n, _ := rt.lengthOf(src)
		if i >= n {
			return mkundef(), true
		}
		idx := i
		i++
		switch kind {
		case iterKeys:
			return mknum(float64(idx)), false
		case iterEntries:
			el, _ := rt.getElement(src, mknum(float64(idx)))
			pair := rt.newArray()
			po := rt.objPtr(pair)
			rt.arraySet(po, 0, mknum(float64(idx)))
			rt.arraySet(po, 1, el)
			return pair, false
		default:
			el, _ := rt.getElement(src, mknum(float64(idx)))
			return el, false
		}
	})
}

// newCollectionIterator builds a Map/Set iterator over a snapshot of entries.
func (rt *Runtime) newCollectionIterator(c *collection, kind iterKind, proto Value) Value {
	i := 0
	return rt.newIteratorObjectP(proto, func() (Value, bool) {
		for i < len(c.keys) {
			if c.keys[i].IsEmpty() {
				i++
				continue
			}
			k, val := c.keys[i], c.vals[i]
			i++
			switch kind {
			case iterKeys:
				return k, false
			case iterValues:
				if c.isSet {
					return k, false
				}
				return val, false
			default: // entries
				pair := rt.newArray()
				po := rt.objPtr(pair)
				rt.arraySet(po, 0, k)
				if c.isSet {
					rt.arraySet(po, 1, k)
				} else {
					rt.arraySet(po, 1, val)
				}
				return pair, false
			}
		}
		return mkundef(), true
	})
}
