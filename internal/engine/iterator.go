package engine

// Iteration protocol support (ant modules/iterator.c). The Phase 5 slice
// materializes iterable values eagerly for arrays, strings, and (later)
// Map/Set; the lazy Symbol.iterator + generator protocol is layered on when
// Symbol and generators land.

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
		// Custom iterables (Map/Set/generators/Symbol.iterator) land later.
		if it := rt.objectIterableValues(v); it != nil {
			return it, nil
		}
		return nil, rt.typeError(rt.typeofString(v) + " is not iterable")
	}
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
