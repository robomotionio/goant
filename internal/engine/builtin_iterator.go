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
