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
		return "s:" + string(rt.strBytes(v))
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
	case TUndef:
		return "u"
	case TNull:
		return "z"
	default:
		return "o:" + strconv.FormatUint(uint64(v), 16)
	}
}

func (rt *Runtime) initCollections() {
	rt.initMapBuiltin()
	rt.initSetBuiltin()
}

func (rt *Runtime) collOf(this Value, wantSet bool) (*collection, *ThrowError) {
	o := rt.objPtr(this)
	if o == nil || o.coll == nil || o.coll.isSet != wantSet {
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
		k := arg(args, 0)
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
	po.defineAccessor("size", rt.newNativeFunc("size", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
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
		return rt.newCollectionIterator(m, iterKeys), nil
	})
	rt.defMethod(po, "values", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		return rt.newCollectionIterator(m, iterValues), nil
	})
	entriesFn := rt.newNativeFunc("entries", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		m, e := rt.collOf(this, false)
		if e != nil {
			return mkundef(), e
		}
		return rt.newCollectionIterator(m, iterEntries), nil
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
			vals, e := rt.iterableValues(it)
			if e != nil {
				return mkundef(), e
			}
			setFn, _ := rt.getField(this, "set")
			for _, entry := range vals {
				k, _ := rt.getElement(entry, mknum(0))
				v, _ := rt.getElement(entry, mknum(1))
				if _, e := rt.callValue(setFn, this, []Value{k, v}); e != nil {
					return mkundef(), e
				}
			}
		}
		return this, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defGlobal("Map", ctor)
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
		k := arg(args, 0)
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
	po.defineAccessor("size", rt.newNativeFunc("size", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
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
		return rt.newCollectionIterator(s, iterEntries), nil
	})
	valuesFn := rt.newNativeFunc("values", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.collOf(this, true)
		if e != nil {
			return mkundef(), e
		}
		return rt.newCollectionIterator(s, iterValues), nil
	})
	po.defineOwn("values", valuesFn, attrWritable|attrConfigurable)
	po.defineOwn("keys", valuesFn, attrWritable|attrConfigurable) // Set.keys === Set.values
	if rt.symIterator != 0 {
		po.defineOwnSymbol(rt.symIterator.handle(), valuesFn, attrWritable|attrConfigurable)
	}

	ctor := rt.newNativeFunc("Set", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("Constructor Set requires 'new'")
		}
		o.coll = &collection{index: map[string]int{}, isSet: true}
		if it := arg(args, 0); !it.IsNullish() {
			vals, e := rt.iterableValues(it)
			if e != nil {
				return mkundef(), e
			}
			addFn, _ := rt.getField(this, "add")
			for _, v := range vals {
				if _, e := rt.callValue(addFn, this, []Value{v}); e != nil {
					return mkundef(), e
				}
			}
		}
		return this, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defGlobal("Set", ctor)
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
