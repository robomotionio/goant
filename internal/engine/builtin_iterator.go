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
	rt.defMethod(proto, "map", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		out := make([]Value, len(vs))
		for i, v := range vs {
			r, e := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(i))})
			if e != nil {
				return mkundef(), e
			}
			out[i] = r
		}
		return rt.sliceIterator(out), nil
	})
	rt.defMethod(proto, "filter", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		var out []Value
		for i, v := range vs {
			r, e := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(i))})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				out = append(out, v)
			}
		}
		return rt.sliceIterator(out), nil
	})
	rt.defMethod(proto, "take", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		n := int(argNum(rt, args, 0))
		if n < 0 {
			n = 0
		}
		if n > len(vs) {
			n = len(vs)
		}
		return rt.sliceIterator(vs[:n]), nil
	})
	rt.defMethod(proto, "drop", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		n := int(argNum(rt, args, 0))
		if n < 0 {
			n = 0
		}
		if n > len(vs) {
			n = len(vs)
		}
		return rt.sliceIterator(vs[n:]), nil
	})
	rt.defMethod(proto, "flatMap", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		var out []Value
		for i, v := range vs {
			r, e := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(i))})
			if e != nil {
				return mkundef(), e
			}
			if rt.isIterable(r) {
				sub, e := rt.iterableValues(r)
				if e != nil {
					return mkundef(), e
				}
				out = append(out, sub...)
			} else {
				out = append(out, r)
			}
		}
		return rt.sliceIterator(out), nil
	})
	rt.defMethod(proto, "reduce", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		acc := arg(args, 1)
		start := 0
		if len(args) < 2 {
			if len(vs) == 0 {
				return mkundef(), rt.typeError("Reduce of empty iterator with no initial value")
			}
			acc = vs[0]
			start = 1
		}
		for i := start; i < len(vs); i++ {
			r, e := rt.callValue(cb, mkundef(), []Value{acc, vs[i], mknum(float64(i))})
			if e != nil {
				return mkundef(), e
			}
			acc = r
		}
		return acc, nil
	})
	rt.defMethod(proto, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		for i, v := range vs {
			if _, e := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(i))}); e != nil {
				return mkundef(), e
			}
		}
		return mkundef(), nil
	})
	rt.defMethod(proto, "some", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		for i, v := range vs {
			r, e := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(i))})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return mktrue(), nil
			}
		}
		return mkfalse(), nil
	})
	rt.defMethod(proto, "every", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		for i, v := range vs {
			r, e := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(i))})
			if e != nil {
				return mkundef(), e
			}
			if !rt.toBoolean(r) {
				return mkfalse(), nil
			}
		}
		return mktrue(), nil
	})
	rt.defMethod(proto, "find", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := drain(this)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		for i, v := range vs {
			r, e := rt.callValue(cb, mkundef(), []Value{v, mknum(float64(i))})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return v, nil
			}
		}
		return mkundef(), nil
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
