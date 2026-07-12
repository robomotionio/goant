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

	// Iterator global: prototype is %IteratorPrototype%; Iterator.from(x).
	ctor := rt.newNativeFunc("Iterator", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkundef(), rt.typeError("Abstract class Iterator not directly constructable")
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.iteratorProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defMethod(cobj, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		src := arg(args, 0)
		// Already an iterator with a next method: wrap so it inherits helpers.
		if o := rt.objPtr(src); o != nil {
			if nx, _ := rt.getField(src, "next"); rt.isCallable(nx) && !rt.isIterable(src) {
				return src, nil
			}
		}
		vs, e := rt.iterableValues(src)
		if e != nil {
			return mkundef(), e
		}
		return rt.sliceIterator(vs), nil
	})
	if rt.symToStringTag != 0 {
		proto.defineOwnSymbol(rt.symToStringTag.handle(), rt.newString("Iterator"), attrConfigurable)
	}
	rt.defGlobal("Iterator", ctor)
}

// newIteratorObject wraps a producer closure as a spec IteratorResult-yielding
// iterator. next returns (value, done); once done it must keep returning done.
func (rt *Runtime) newIteratorObject(next func() (Value, bool)) Value {
	v := rt.newObject(rt.iteratorProto)
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
	return rt.newIteratorObject(func() (Value, bool) {
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
func (rt *Runtime) newCollectionIterator(c *collection, kind iterKind) Value {
	i := 0
	return rt.newIteratorObject(func() (Value, bool) {
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
