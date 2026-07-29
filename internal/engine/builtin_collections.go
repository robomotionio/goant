package engine

// Map / Set (ant modules/collections.c). Entries are insertion-ordered with a
// canonical-key index implementing SameValueZero equality. WeakMap/WeakSet
// (weak keying by handle+generation) land with the GC phase.

import (
	"math"
	"strconv"
)

// canonicalKey maps a value to a stable string key under SameValueZero (+0/-0
// equal, NaN equal to NaN, strings by content).
func (rt *Runtime) canonicalKey(v Value) string {
	switch v.Type() {
	case TStr:
		return "s:" + rt.strGo(v)
	case TNum:
		d := v.Number()
		if math.IsNaN(d) {
			return "n:NaN"
		}
		if d == 0 {
			return "n:0" // +0 and -0 collapse
		}
		return "n:" + strconv.FormatUint(math.Float64bits(d), 16)
	case TBool:
		if v.Bool() {
			return "b:1"
		}
		return "b:0"
	case TBigInt:
		// BigInt keys compare by value, not by the handle in the Value bits.
		return "i:" + rt.bigIntVal(v).String()
	case TUndef:
		return "u"
	case TNull:
		return "z"
	default:
		return "o:" + strconv.FormatUint(uint64(v), 16)
	}
}

// normMapKey normalizes a would-be Map/Set key: -0 is stored as +0 so iteration
// yields +0 (Map/Set CanonicalizeKeyedCollectionKey). Other values pass through.
func normMapKey(v Value) Value {
	if v.Type() == TNum && v.Number() == 0 {
		return mknum(0)
	}
	return v
}

func (rt *Runtime) initCollections() {
	rt.initMapBuiltin()
	rt.initSetBuiltin()
}

func (rt *Runtime) collOf(this Value, wantSet bool) (*collection, *ThrowError) {
	o := rt.objPtr(this)
	// A weak collection (WeakMap/WeakSet) has a coll but no [[MapData]]/[[SetData]]
	// slot, so a strong Map/Set method must reject it.
	if o == nil || o.coll == nil || o.coll.weak || o.coll.isSet != wantSet {
		name := "Map"
		if wantSet {
			name = "Set"
		}
		return nil, rt.typeError("method called on incompatible receiver (not a " + name + ")")
	}
	return o.coll, nil
}

func (rt *Runtime) initMapBuiltin() {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)

	rt.defMethod(po, "set", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		k := normMapKey(arg(args, 0))
		ck := rt.canonicalKey(k)
		if idx, ok := m.index[ck]; ok {
			m.vals[idx] = arg(args, 1)
		} else {
			m.index[ck] = len(m.keys)
			m.keys = append(m.keys, k)
			m.vals = append(m.vals, arg(args, 1))
		}
		return this, nil
	})
	rt.defMethod(po, "get", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		if idx, ok := m.index[rt.canonicalKey(arg(args, 0))]; ok {
			return m.vals[idx], nil
		}
		return mkundef(), nil
	})
	rt.defMethod(po, "getOrInsert", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Map.prototype.getOrInsert(key, value): return the existing value or
		// insert and return the default (upsert proposal).
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		k := normMapKey(arg(args, 0))
		ck := rt.canonicalKey(k)
		if idx, ok := m.index[ck]; ok {
			return m.vals[idx], nil
		}
		v := arg(args, 1)
		m.index[ck] = len(m.keys)
		m.keys = append(m.keys, k)
		m.vals = append(m.vals, v)
		return v, nil
	})
	rt.defMethod(po, "getOrInsertComputed", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		k := normMapKey(arg(args, 0))
		cb := arg(args, 1)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("getOrInsertComputed callbackfn is not a function")
		}
		ck := rt.canonicalKey(k)
		if idx, ok := m.index[ck]; ok {
			return m.vals[idx], nil
		}
		v, e := rt.callValue(cb, mkundef(), []Value{k})
		if e != nil {
			return mkundef(), e
		}
		if idx, ok := m.index[ck]; ok {
			m.vals[idx] = v
			return v, nil
		}
		m.index[ck] = len(m.keys)
		m.keys = append(m.keys, k)
		m.vals = append(m.vals, v)
		return v, nil
	})
	rt.defMethod(po, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		_, ok := m.index[rt.canonicalKey(arg(args, 0))]
		return mkbool(ok), nil
	})
	rt.defMethod(po, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		return mkbool(m.remove(rt.canonicalKey(arg(args, 0)))), nil
	})
	rt.defMethod(po, "clear", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		m.clear()
		return mkundef(), nil
	})
	rt.defMethod(po, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Map.prototype.forEach callback is not a function")
		}
		for i := 0; i < len(m.keys); i++ {
			if m.keys[i].IsEmpty() {
				continue
			}
			if _, e := rt.callValue(cb, arg(args, 1), []Value{m.vals[i], m.keys[i], this}); e != nil {
				return mkundef(), e
			}
		}
		return mkundef(), nil
	})
	po.defineAccessor("size", rt.newNativeFunc("get size", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(m.size())), nil
	}), mkundef(), true, false, attrConfigurable)

	rt.defMethod(po, "keys", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		return rt.newCollectionIterator(m, iterKeys, rt.mapIterProto), nil
	})
	rt.defMethod(po, "values", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		return rt.newCollectionIterator(m, iterValues, rt.mapIterProto), nil
	})
	entriesFn := rt.newNativeFunc("entries", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		return rt.newCollectionIterator(m, iterEntries, rt.mapIterProto), nil
	})
	po.defineOwn("entries", entriesFn, attrWritable|attrConfigurable)
	if rt.symIterator != 0 {
		po.defineOwnSymbol(rt.symIterator.handle(), entriesFn, attrWritable|attrConfigurable)
	}

	ctor := rt.newNativeFunc("Map", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("Constructor Map requires 'new'")
		}
		o.coll = &collection{index: map[string]int{}}
		if it := arg(args, 0); !it.IsNullish() {
			setFn, e := rt.getField(this, "set")
			if e != nil {
				return mkundef(), e
			}
			if !rt.isCallable(setFn) {
				return mkundef(), rt.typeError("Map: 'set' is not callable")
			}
			if e := rt.iterateWithClose(it, func(entry Value) (bool, *ThrowError) {
				// Each iterator value must be an entry object; a primitive aborts
				// the loop with a TypeError, which closes the iterator.
				if !entry.IsObjectType() {
					return false, rt.typeError("Iterator value " + rt.inspect(entry, false) + " is not an entry object")
				}
				k, e := rt.getElement(entry, mknum(0))
				if e != nil {
					return false, e
				}
				v, e := rt.getElement(entry, mknum(1))
				if e != nil {
					return false, e
				}
				_, e = rt.callValue(setFn, this, []Value{k, v})
				return false, e
			}); e != nil {
				return mkundef(), e
			}
		}
		return this, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defMethod(rt.objPtr(ctor), "groupBy", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// GroupBy checks the callback BEFORE iterating, so an empty iterable with a
		// non-callable callback is still a TypeError.
		cb := arg(args, 1)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Map.groupBy callback is not a function")
		}
		items, e := rt.iterableValues(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		res := rt.newObject(proto)
		c := &collection{index: map[string]int{}}
		rt.objPtr(res).coll = c
		for i, it := range items {
			key, e := rt.callValue(cb, mkundef(), []Value{it, mknum(float64(i))})
			if e != nil {
				return mkundef(), e
			}
			ck := rt.canonicalKey(key)
			idx, ok := c.index[ck]
			if !ok {
				grp := rt.newArray()
				c.index[ck] = len(c.keys)
				c.keys = append(c.keys, key)
				c.vals = append(c.vals, grp)
				idx = len(c.keys) - 1
			}
			go2 := rt.objPtr(c.vals[idx])
			rt.arraySet(go2, go2.arrLen, it)
		}
		return res, nil
	})
	rt.defSpeciesGetter(ctor)
	rt.defGlobal("Map", ctor)
	rt.setStringTag(proto, "Map")
	rt.mapProto = proto
}

func (rt *Runtime) initSetBuiltin() {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)

	rt.defMethod(po, "add", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		k := normMapKey(arg(args, 0))
		ck := rt.canonicalKey(k)
		if _, ok := s.index[ck]; !ok {
			s.index[ck] = len(s.keys)
			s.keys = append(s.keys, k)
			s.vals = append(s.vals, mkundef())
		}
		return this, nil
	})
	rt.defMethod(po, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		_, ok := s.index[rt.canonicalKey(arg(args, 0))]
		return mkbool(ok), nil
	})
	rt.defMethod(po, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		return mkbool(s.remove(rt.canonicalKey(arg(args, 0)))), nil
	})
	rt.defMethod(po, "clear", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		s.clear()
		return mkundef(), nil
	})
	rt.defMethod(po, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Set.prototype.forEach callback is not a function")
		}
		for i := 0; i < len(s.keys); i++ {
			if s.keys[i].IsEmpty() {
				continue
			}
			if _, e := rt.callValue(cb, arg(args, 1), []Value{s.keys[i], s.keys[i], this}); e != nil {
				return mkundef(), e
			}
		}
		return mkundef(), nil
	})
	po.defineAccessor("size", rt.newNativeFunc("get size", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(s.size())), nil
	}), mkundef(), true, false, attrConfigurable)

	rt.defMethod(po, "entries", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		return rt.newCollectionIterator(s, iterEntries, rt.setIterProto), nil
	})
	valuesFn := rt.newNativeFunc("values", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		return rt.newCollectionIterator(s, iterValues, rt.setIterProto), nil
	})
	po.defineOwn("values", valuesFn, attrWritable|attrConfigurable)
	po.defineOwn("keys", valuesFn, attrWritable|attrConfigurable) // Set.keys === Set.values
	if rt.symIterator != 0 {
		po.defineOwnSymbol(rt.symIterator.handle(), valuesFn, attrWritable|attrConfigurable)
	}
	rt.defineSetOperations(po)

	ctor := rt.newNativeFunc("Set", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("Constructor Set requires 'new'")
		}
		o.coll = &collection{index: map[string]int{}, isSet: true}
		if it := arg(args, 0); !it.IsNullish() {
			addFn, e := rt.getField(this, "add")
			if e != nil {
				return mkundef(), e
			}
			if !rt.isCallable(addFn) {
				return mkundef(), rt.typeError("Set: 'add' is not callable")
			}
			if e := rt.iterateWithClose(it, func(v Value) (bool, *ThrowError) {
				_, e := rt.callValue(addFn, this, []Value{v})
				return false, e
			}); e != nil {
				return mkundef(), e
			}
		}
		return this, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defSpeciesGetter(ctor)
	rt.defGlobal("Set", ctor)
	rt.setStringTag(proto, "Set")
	rt.setProto = proto
}

// ---- collection helpers ----

func (c *collection) size() int {
	n := 0
	for _, k := range c.keys {
		if !k.IsEmpty() {
			n++
		}
	}
	return n
}

func (c *collection) remove(ck string) bool {
	idx, ok := c.index[ck]
	if !ok {
		return false
	}
	delete(c.index, ck)
	c.keys[idx] = tEmpty // tombstone preserves iteration order
	c.vals[idx] = mkundef()
	return true
}

func (c *collection) clear() {
	c.keys = nil
	c.vals = nil
	c.index = map[string]int{}
}

// setElements returns a Set's live element values (skipping tombstones).
func (rt *Runtime) setElements(s *collection) []Value {
	out := make([]Value, 0, len(s.keys))
	for _, k := range s.keys {
		if !k.IsEmpty() {
			out = append(out, k)
		}
	}
	return out
}

// setLikeElements extracts elements from a Set or any iterable "set-like".
// setLikeElements implements GetSetRecord for the Set-method "other" argument:
// it must be an Object with a numeric non-negative size and callable has/keys;
// the elements come from draining keys(). (The result-only algorithms here read
// keys rather than dispatching has() per element, which is sufficient for the
// membership tests but not the exact has-vs-keys call sequence.)
func (rt *Runtime) setLikeElements(v Value) ([]Value, *ThrowError) {
	if !v.IsObjectType() {
		return nil, rt.typeError("Set operation argument must be an object")
	}
	rawSize, e := rt.getField(v, "size")
	if e != nil {
		return nil, e
	}
	numSize, e := rt.toNumber(rawSize)
	if e != nil {
		return nil, e
	}
	if numSize != numSize { // NaN
		return nil, rt.typeError("Set operation argument has an invalid size")
	}
	if numSize < 0 {
		return nil, rt.rangeError("Set operation argument has a negative size")
	}
	has, e := rt.getField(v, "has")
	if e != nil {
		return nil, e
	}
	if !rt.isCallable(has) {
		return nil, rt.typeError("Set operation argument has no callable 'has' method")
	}
	keysFn, e := rt.getField(v, "keys")
	if e != nil {
		return nil, e
	}
	if !rt.isCallable(keysFn) {
		return nil, rt.typeError("Set operation argument has no callable 'keys' method")
	}
	// Native Set fast path: keys() would yield exactly these values.
	if o := rt.objPtr(v); o != nil && o.coll != nil && o.coll.isSet {
		return rt.setElements(o.coll), nil
	}
	iter, e := rt.callValue(keysFn, v, nil)
	if e != nil {
		return nil, e
	}
	next, e := rt.getField(iter, "next")
	if e != nil {
		return nil, e
	}
	if !rt.isCallable(next) {
		return nil, rt.typeError("Set operation argument keys() did not return an iterator")
	}
	var out []Value
	const maxEager = 1 << 20
	for i := 0; i < maxEager; i++ {
		res, e := rt.callValue(next, iter, nil)
		if e != nil {
			return nil, e
		}
		if !res.IsObjectType() {
			return nil, rt.typeError("iterator result is not an object")
		}
		done, e := rt.getField(res, "done")
		if e != nil {
			return nil, e
		}
		if rt.toBoolean(done) {
			break
		}
		val, e := rt.getField(res, "value")
		if e != nil {
			return nil, e
		}
		out = append(out, val)
	}
	return out, nil
}

// newSetFrom builds a fresh Set populated with the given elements.
func (rt *Runtime) newSetFrom(elems []Value) Value {
	v := rt.newObject(rt.setProto)
	c := &collection{index: map[string]int{}, isSet: true}
	rt.objPtr(v).coll = c
	for _, e := range elems {
		ck := rt.canonicalKey(e)
		if _, ok := c.index[ck]; !ok {
			c.index[ck] = len(c.keys)
			c.keys = append(c.keys, normMapKey(e)) // canonicalize -0 to +0
			c.vals = append(c.vals, mkundef())
		}
	}
	return v
}

// setRecord is GetSetRecord(obj): the "other" argument of a Set method plus its
// validated numeric size and its has/keys methods, all read once up front.
type setRecord struct {
	obj  Value
	size float64
	has  Value
	keys Value
}

// getSetRecord validates and reads the set-like "other" argument: an Object with
// a non-NaN, non-negative size (ToIntegerOrInfinity) and callable has/keys.
func (rt *Runtime) getSetRecord(v Value) (*setRecord, *ThrowError) {
	if !v.IsObjectType() {
		return nil, rt.typeError("Set method argument must be an object")
	}
	rawSize, e := rt.getField(v, "size")
	if e != nil {
		return nil, e
	}
	numSize, e := rt.toNumber(rawSize)
	if e != nil {
		return nil, e
	}
	// The wording follows V8's. The spec fixes the error types but not their
	// text, and code in the wild — and the tests that come with it — matches on
	// the text it has seen, so there is no value in being different here.
	if math.IsNaN(numSize) {
		return nil, rt.typeError("The .size property is NaN")
	}
	intSize := math.Trunc(numSize)
	if intSize < 0 {
		return nil, rt.rangeError("'" + numberToString(numSize) + "' is an invalid size")
	}
	has, e := rt.getField(v, "has")
	if e != nil {
		return nil, e
	}
	if !rt.isCallable(has) {
		return nil, rt.typeError("Set method argument has no callable 'has' method")
	}
	keys, e := rt.getField(v, "keys")
	if e != nil {
		return nil, e
	}
	if !rt.isCallable(keys) {
		return nil, rt.typeError("Set method argument has no callable 'keys' method")
	}
	return &setRecord{obj: v, size: intSize, has: has, keys: keys}, nil
}

// recordHas dispatches rec.[[Has]](v) → boolean.
func (rt *Runtime) recordHas(rec *setRecord, v Value) (bool, *ThrowError) {
	r, e := rt.callValue(rec.has, rec.obj, []Value{v})
	if e != nil {
		return false, e
	}
	return rt.toBoolean(r), nil
}

// forEachSetRecordKey drives rec.[[Keys]]() as an iterator, calling fn for each
// value; fn returning stop=true closes the iterator and ends the walk.
// setKeysIter is an opened GetKeysIterator(setRecord) — the iterator and its
// next method, resolved.
//
// Opening is separate from iterating because the spec separates them: union and
// symmetricDifference call GetKeysIterator before they copy the receiver's
// [[SetData]], so a keys() that clears the receiver is observed by the copy. Do
// both in one step and that ordering is unobservable.
type setKeysIter struct {
	iter, next Value
}

func (rt *Runtime) openSetRecordKeys(rec *setRecord) (setKeysIter, *ThrowError) {
	iter, e := rt.callValue(rec.keys, rec.obj, nil)
	if e != nil {
		return setKeysIter{}, e
	}
	if !iter.IsObjectType() {
		return setKeysIter{}, rt.typeError("Set method argument keys() did not return an object")
	}
	next, e := rt.getField(iter, "next")
	if e != nil {
		return setKeysIter{}, e
	}
	if !rt.isCallable(next) {
		return setKeysIter{}, rt.typeError("Set method argument keys() iterator has no next method")
	}
	return setKeysIter{iter: iter, next: next}, nil
}

func (rt *Runtime) forEachSetRecordKey(rec *setRecord, fn func(Value) (bool, *ThrowError)) *ThrowError {
	ki, e := rt.openSetRecordKeys(rec)
	if e != nil {
		return e
	}
	return rt.iterateSetKeys(ki, fn)
}

func (rt *Runtime) iterateSetKeys(ki setKeysIter, fn func(Value) (bool, *ThrowError)) *ThrowError {
	iter, next := ki.iter, ki.next
	const maxEager = 1 << 24
	for i := 0; i < maxEager; i++ {
		res, e := rt.callValue(next, iter, nil)
		if e != nil {
			return e
		}
		if !res.IsObjectType() {
			return rt.typeError("iterator result is not an object")
		}
		done, e := rt.getField(res, "done")
		if e != nil {
			return e
		}
		if rt.toBoolean(done) {
			return nil
		}
		val, e := rt.getField(res, "value")
		if e != nil {
			return e
		}
		stop, e := fn(val)
		if e != nil {
			return e
		}
		if stop {
			if ret, _ := rt.getField(iter, "return"); rt.isCallable(ret) {
				rt.callValue(ret, iter, nil) // IteratorClose (best effort)
			}
			return nil
		}
	}
	return nil
}

// defineSetOperations installs the ES2025 Set-theory methods, following the
// spec's observable has-vs-keys dispatch (a method that iterates its own,
// smaller set calls other.has() per element; otherwise it drains other.keys()).
func (rt *Runtime) defineSetOperations(po *object) {
	rt.defMethod(po, "union", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		rec, e := rt.getSetRecord(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		// GetKeysIterator is step 4 and the copy of O.[[SetData]] is step 5, in
		// that order: a keys() that mutates the receiver does so before the copy
		// is taken, and the copy must see the result.
		ki, e := rt.openSetRecordKeys(rec)
		if e != nil {
			return mkundef(), e
		}
		out := append([]Value(nil), rt.setElements(s)...)
		if e := rt.iterateSetKeys(ki, func(v Value) (bool, *ThrowError) {
			out = append(out, v)
			return false, nil
		}); e != nil {
			return mkundef(), e
		}
		return rt.newSetFrom(out), nil
	})
	rt.defMethod(po, "intersection", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		rec, e := rt.getSetRecord(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		thisElems := rt.setElements(s)
		var out []Value
		if float64(len(thisElems)) <= rec.size {
			for _, el := range thisElems {
				in, e := rt.recordHas(rec, el)
				if e != nil {
					return mkundef(), e
				}
				if in {
					out = append(out, el)
				}
			}
		} else {
			// SetDataHas(O.[[SetData]], nextValue) reads this set as it is now, not
			// as it was when the branch was chosen: other's keys() runs first and
			// may have emptied or refilled the receiver, and the elements it left
			// behind are the ones that count. Only the size comparison above is
			// taken from before.
			seen := map[string]bool{}
			if e := rt.forEachSetRecordKey(rec, func(v Value) (bool, *ThrowError) {
				ck := rt.canonicalKey(v)
				if _, live := s.index[ck]; live && !seen[ck] {
					seen[ck] = true
					out = append(out, v)
				}
				return false, nil
			}); e != nil {
				return mkundef(), e
			}
		}
		return rt.newSetFrom(out), nil
	})
	rt.defMethod(po, "difference", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		rec, e := rt.getSetRecord(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		thisElems := rt.setElements(s)
		var out []Value
		if float64(len(thisElems)) <= rec.size {
			for _, el := range thisElems {
				in, e := rt.recordHas(rec, el)
				if e != nil {
					return mkundef(), e
				}
				if !in {
					out = append(out, el)
				}
			}
		} else {
			remove := map[string]bool{}
			if e := rt.forEachSetRecordKey(rec, func(v Value) (bool, *ThrowError) {
				remove[rt.canonicalKey(v)] = true
				return false, nil
			}); e != nil {
				return mkundef(), e
			}
			for _, el := range thisElems {
				if !remove[rt.canonicalKey(el)] {
					out = append(out, el)
				}
			}
		}
		return rt.newSetFrom(out), nil
	})
	rt.defMethod(po, "symmetricDifference", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		rec, e := rt.getSetRecord(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		// resultSetData starts as a copy of this set's elements, taken after
		// GetKeysIterator — calling keys() is step 4 and the copy is step 5, so a
		// keys() that clears the receiver empties what gets copied. Each key
		// drained from other then toggles membership against both the result and
		// the LIVE this set: other's iterator may add to or delete from this set
		// as it goes, and those mutations are observed too (SetDataHas).
		ki, e := rt.openSetRecordKeys(rec)
		if e != nil {
			return mkundef(), e
		}
		var result []Value
		resultKeys := map[string]bool{}
		for _, el := range rt.setElements(s) {
			result = append(result, el)
			resultKeys[rt.canonicalKey(el)] = true
		}
		if e := rt.iterateSetKeys(ki, func(v Value) (bool, *ThrowError) {
			ck := rt.canonicalKey(v)
			inResult := resultKeys[ck]
			_, inThis := s.index[ck] // live membership in this set
			if inThis {
				if inResult {
					resultKeys[ck] = false
					for i, el := range result {
						if rt.canonicalKey(el) == ck {
							result = append(result[:i], result[i+1:]...)
							break
						}
					}
				}
			} else if !inResult {
				result = append(result, v)
				resultKeys[ck] = true
			}
			return false, nil
		}); e != nil {
			return mkundef(), e
		}
		return rt.newSetFrom(result), nil
	})
	rt.defMethod(po, "isSubsetOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		rec, e := rt.getSetRecord(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		if float64(len(rt.setElements(s))) > rec.size {
			return mkfalse(), nil
		}
		// Iterate this set's [[SetData]] live by index (skipping tombstoned slots),
		// bounded by the length captured up front: rec.[[Has]] may delete elements
		// from this set, and a deleted element must not be visited (SetDataIndex).
		thisSize := len(s.keys)
		for index := 0; index < thisSize && index < len(s.keys); index++ {
			el := s.keys[index]
			if el.IsEmpty() {
				continue
			}
			in, e := rt.recordHas(rec, el)
			if e != nil {
				return mkundef(), e
			}
			if !in {
				return mkfalse(), nil
			}
		}
		return mktrue(), nil
	})
	rt.defMethod(po, "isSupersetOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		rec, e := rt.getSetRecord(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		// The size comparison is made before other's keys() runs; the membership
		// tests after it are SetDataHas against this set as it stands then, which
		// keys() may have rewritten.
		if float64(len(rt.setElements(s))) < rec.size {
			return mkfalse(), nil
		}
		result := true
		if e := rt.forEachSetRecordKey(rec, func(v Value) (bool, *ThrowError) {
			if _, live := s.index[rt.canonicalKey(v)]; !live {
				result = false
				return true, nil // stop
			}
			return false, nil
		}); e != nil {
			return mkundef(), e
		}
		return mkbool(result), nil
	})
	rt.defMethod(po, "isDisjointFrom", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		rec, e := rt.getSetRecord(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		thisElems := rt.setElements(s)
		if float64(len(thisElems)) <= rec.size {
			// Iterate this set's [[SetData]] live by index: rec.[[Has]] may delete
			// elements from this set, and a deleted element must not be visited.
			thisSize := len(s.keys)
			for index := 0; index < thisSize && index < len(s.keys); index++ {
				el := s.keys[index]
				if el.IsEmpty() {
					continue
				}
				in, e := rt.recordHas(rec, el)
				if e != nil {
					return mkundef(), e
				}
				if in {
					return mkfalse(), nil
				}
			}
			return mktrue(), nil
		}
		// As in the branch above, SetDataHas reads this set live: other's keys()
		// runs before the first test and may have changed what is in it.
		result := true
		if e := rt.forEachSetRecordKey(rec, func(v Value) (bool, *ThrowError) {
			if _, live := s.index[rt.canonicalKey(v)]; live {
				result = false
				return true, nil // stop
			}
			return false, nil
		}); e != nil {
			return mkundef(), e
		}
		return mkbool(result), nil
	})
}
