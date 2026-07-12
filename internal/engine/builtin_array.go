package engine

// Array constructor + Array.prototype (ant builtin_array). Core mutators and
// iteration methods; the ".generic" (array-like via .call) forms work because
// element access goes through length + getElement.

import (
	"math"
	"sort"
)

// relativeIndex resolves a relative-index argument against length n (ES
// ToIntegerOrInfinity + clamp): negatives count from the end, results clamp to
// [0, n]. A missing/undefined argument yields 0.
func (rt *Runtime) relativeIndex(v Value, n int) int {
	if v.IsUndefined() {
		return 0
	}
	d, _ := rt.toNumber(v)
	if math.IsNaN(d) {
		return 0
	}
	k := int(d)
	if k < 0 {
		k += n
		if k < 0 {
			k = 0
		}
	} else if k > n {
		k = n
	}
	return k
}

// sortValues sorts a slice in place with JS Array.prototype.sort semantics
// (undefined last; comparator or default ToString ordering).
func (rt *Runtime) sortValues(vs []Value, cmp Value) *ThrowError {
	if !cmp.IsUndefined() && !rt.isCallable(cmp) {
		return rt.typeError("The comparison function must be either a function or undefined")
	}
	var sortErr *ThrowError
	sort.SliceStable(vs, func(i, j int) bool {
		if sortErr != nil {
			return false
		}
		a, b := vs[i], vs[j]
		if a.IsUndefined() {
			return false
		}
		if b.IsUndefined() {
			return true
		}
		if rt.isCallable(cmp) {
			r, e := rt.callValue(cmp, mkundef(), []Value{a, b})
			if e != nil {
				sortErr = e
				return false
			}
			nv, _ := rt.toNumberPrimitive(r)
			return nv < 0
		}
		sa, _ := rt.toStringValue(a)
		sb, _ := rt.toStringValue(b)
		return compareStrings(rt.strBytes(sa), rt.strBytes(sb)) < 0
	})
	return sortErr
}

// arraySpeciesCreate implements ArraySpeciesCreate: the result of an array
// method uses this.constructor[Symbol.species] as its constructor when present,
// otherwise a plain Array.
// arrayFromCtor creates the result for Array.of/from: when called as a static on
// a subclass constructor (`this`), it constructs via that constructor; otherwise
// a plain Array. Lets `class C extends Array {}` inherit species-correct statics.
func (rt *Runtime) arrayFromCtor(this Value, length int) (Value, *ThrowError) {
	if rt.isCallable(this) {
		v, e := rt.construct(this, []Value{mknum(float64(length))})
		if e != nil {
			return mkundef(), e
		}
		return v, nil
	}
	return rt.newArray(), nil
}

func (rt *Runtime) arraySpeciesCreate(this Value, length int) (Value, *ThrowError) {
	if this.Type() == TArr && rt.symSpecies != 0 {
		ctor, e := rt.getField(this, "constructor")
		if e != nil {
			return mkundef(), e
		}
		if ctor.IsObjectType() {
			sp := rt.getFieldSymbol(ctor, rt.symSpecies.handle())
			if sp.IsObjectType() && rt.isCallable(sp) {
				return rt.construct(sp, []Value{mknum(float64(length))})
			}
		}
	}
	res := rt.newArray()
	return res, nil
}

func (rt *Runtime) initArrayBuiltin() {
	proto := rt.objPtr(rt.arrayProto)

	// Mutators are generic: they read length + elements through the ordinary
	// property protocol, so they work on arrays and array-like objects alike.
	rt.defMethod(proto, "push", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for _, a := range args {
			if e := rt.setElement(this, mknum(float64(n)), a); e != nil {
				return mkundef(), e
			}
			n++
		}
		if e := rt.setField(this, "length", mknum(float64(n))); e != nil {
			return mkundef(), e
		}
		return mknum(float64(n)), nil
	})
	rt.defMethod(proto, "pop", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		if n == 0 {
			rt.setField(this, "length", mknum(0))
			return mkundef(), nil
		}
		v, _ := rt.getElement(this, mknum(float64(n-1)))
		rt.deleteElement(this, mknum(float64(n-1)))
		rt.setField(this, "length", mknum(float64(n-1)))
		return v, nil
	})
	rt.defMethod(proto, "shift", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		if n == 0 {
			rt.setField(this, "length", mknum(0))
			return mkundef(), nil
		}
		first, _ := rt.getElement(this, mknum(0))
		// 23.1.3.27: shift each element down one; a hole propagates as a Delete of
		// the destination rather than an overwrite.
		for k := 1; k < n; k++ {
			from := mknum(float64(k))
			to := mknum(float64(k - 1))
			if rt.hasElem(this, k) {
				v, _ := rt.getElement(this, from)
				if e := rt.setElement(this, to, v); e != nil {
					return mkundef(), e
				}
			} else {
				if _, e := rt.deleteElement(this, to); e != nil {
					return mkundef(), e
				}
			}
		}
		rt.deleteElement(this, mknum(float64(n-1)))
		rt.setField(this, "length", mknum(float64(n-1)))
		return first, nil
	})
	rt.defMethod(proto, "unshift", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		// 23.1.3.32: shift the tail up by argCount (holes Delete their target),
		// then prepend the new items.
		k := len(args)
		if k > 0 {
			for i := n; i > 0; i-- {
				from := i - 1
				to := i - 1 + k
				if rt.hasElem(this, from) {
					v, _ := rt.getElement(this, mknum(float64(from)))
					if e := rt.setElement(this, mknum(float64(to)), v); e != nil {
						return mkundef(), e
					}
				} else {
					if _, e := rt.deleteElement(this, mknum(float64(to))); e != nil {
						return mkundef(), e
					}
				}
			}
			for i := 0; i < k; i++ {
				if e := rt.setElement(this, mknum(float64(i)), args[i]); e != nil {
					return mkundef(), e
				}
			}
		}
		rt.setField(this, "length", mknum(float64(n+k)))
		return mknum(float64(n + k)), nil
	})

	rt.defMethod(proto, "join", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sep := ","
		if len(args) > 0 && !args[0].IsUndefined() {
			s, e := rt.toStringValue(args[0])
			if e != nil {
				return mkundef(), e
			}
			sep = string(rt.strBytes(s))
		}
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		out := make([]byte, 0, n*4)
		for i := 0; i < n; i++ {
			if i > 0 {
				out = append(out, sep...)
			}
			el, e := rt.getElement(this, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			if el.IsNullish() {
				continue
			}
			s, e := rt.toStringValue(el)
			if e != nil {
				return mkundef(), e
			}
			out = append(out, rt.strBytes(s)...)
		}
		return rt.newStringBytes(out), nil
	})
	rt.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		joinFn, _ := rt.getField(this, "join")
		if rt.isCallable(joinFn) {
			return rt.callValue(joinFn, this, nil)
		}
		return rt.internString("[object Array]"), nil
	})

	rt.defMethod(proto, "indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n; i++ {
			el, e := rt.getElement(this, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			if rt.strictEquals(el, target) {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	rt.defMethod(proto, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		start := rt.relativeIndex(arg(args, 1), n)
		for i := start; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			if rt.sameValueZero(el, target) {
				return mktrue(), nil
			}
		}
		return mkfalse(), nil
	})
	rt.defMethod(proto, "flatMap", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("flatMap callback is not a function")
		}
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		res := rt.newArray()
		ro := rt.objPtr(res)
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			mv, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if mv.Type() == TArr {
				mo := rt.objPtr(mv)
				for j := uint32(0); j < mo.arrLen; j++ {
					if int(j) < len(mo.arr) && !mo.arr[j].IsEmpty() {
						rt.arraySet(ro, ro.arrLen, mo.arr[j])
					} else {
						rt.arraySet(ro, ro.arrLen, mkundef())
					}
				}
			} else {
				rt.arraySet(ro, ro.arrLen, mv)
			}
		}
		return res, nil
	})
	rt.defMethod(proto, "findLast", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := n - 1; i >= 0; i-- {
			el, _ := rt.getElement(this, mknum(float64(i)))
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return el, nil
			}
		}
		return mkundef(), nil
	})
	rt.defMethod(proto, "findLastIndex", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := n - 1; i >= 0; i-- {
			el, _ := rt.getElement(this, mknum(float64(i)))
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	rt.defMethod(proto, "copyWithin", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		to := rt.relativeIndex(arg(args, 0), n)
		from := rt.relativeIndex(arg(args, 1), n)
		final := n
		if len(args) > 2 && !arg(args, 2).IsUndefined() {
			final = rt.relativeIndex(arg(args, 2), n)
		}
		count := final - from
		if count > n-to {
			count = n - to
		}
		// 23.1.3.4: copy in place with a direction chosen to avoid clobbering the
		// source when the ranges overlap; a hole in the source Deletes the target.
		dir := 1
		if from < to && to < from+count {
			dir = -1
			from += count - 1
			to += count - 1
		}
		for ; count > 0; count-- {
			fromP := mknum(float64(from))
			toP := mknum(float64(to))
			if rt.hasElem(this, from) {
				v, _ := rt.getElement(this, fromP)
				if e := rt.setElement(this, toP, v); e != nil {
					return mkundef(), e
				}
			} else {
				if _, e := rt.deleteElement(this, toP); e != nil {
					return mkundef(), e
				}
			}
			from += dir
			to += dir
		}
		return this, nil
	})
	rt.defMethod(proto, "entries", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newIndexIterator(this, iterEntries), nil
	})
	rt.defMethod(proto, "keys", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newIndexIterator(this, iterKeys), nil
	})
	valuesFn := rt.newNativeFunc("values", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newIndexIterator(this, iterValues), nil
	})
	proto.defineOwn("values", valuesFn, attrWritable|attrConfigurable)
	if rt.symIterator != 0 {
		proto.defineOwnSymbol(rt.symIterator.handle(), valuesFn, attrWritable|attrConfigurable)
	}
	// ES2023 change-array-by-copy: non-mutating variants returning a new array.
	readAll := func(this Value) ([]Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return nil, e
		}
		out := make([]Value, n)
		for i := 0; i < n; i++ {
			out[i], _ = rt.getElement(this, mknum(float64(i)))
		}
		return out, nil
	}
	fromSlice := func(vs []Value) Value {
		res := rt.newArray()
		ro := rt.objPtr(res)
		for i, v := range vs {
			rt.arraySet(ro, uint32(i), v)
		}
		return res
	}
	rt.defMethod(proto, "toReversed", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := readAll(this)
		if e != nil {
			return mkundef(), e
		}
		for i, j := 0, len(vs)-1; i < j; i, j = i+1, j-1 {
			vs[i], vs[j] = vs[j], vs[i]
		}
		return fromSlice(vs), nil
	})
	rt.defMethod(proto, "toSorted", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := readAll(this)
		if e != nil {
			return mkundef(), e
		}
		cmp := arg(args, 0)
		if e := rt.sortValues(vs, cmp); e != nil {
			return mkundef(), e
		}
		return fromSlice(vs), nil
	})
	rt.defMethod(proto, "with", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := readAll(this)
		if e != nil {
			return mkundef(), e
		}
		idx := int(argNum(rt, args, 0))
		if idx < 0 {
			idx += len(vs)
		}
		if idx < 0 || idx >= len(vs) {
			return mkundef(), rt.rangeError("Invalid index")
		}
		vs[idx] = arg(args, 1)
		return fromSlice(vs), nil
	})
	rt.defMethod(proto, "toSpliced", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		vs, e := readAll(this)
		if e != nil {
			return mkundef(), e
		}
		n := len(vs)
		start := rt.relativeIndex(arg(args, 0), n)
		del := n - start
		if len(args) > 1 {
			del = int(argNum(rt, args, 1))
			if del < 0 {
				del = 0
			}
			if del > n-start {
				del = n - start
			}
		}
		var inserts []Value
		if len(args) > 2 {
			inserts = args[2:]
		}
		out := make([]Value, 0, n-del+len(inserts))
		out = append(out, vs[:start]...)
		out = append(out, inserts...)
		out = append(out, vs[start+del:]...)
		return fromSlice(out), nil
	})

	rt.defMethod(proto, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		start := relIndex(rt, arg(args, 0), n, 0)
		end := relIndex(rt, arg(args, 1), n, n)
		res, e := rt.arraySpeciesCreate(this, end-start)
		if e != nil {
			return mkundef(), e
		}
		out := 0
		for i := start; i < end; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			rt.setElement(res, mknum(float64(out)), el)
			out++
		}
		return res, nil
	})
	rt.defMethod(proto, "concat", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res, e := rt.arraySpeciesCreate(this, 0)
		if e != nil {
			return mkundef(), e
		}
		idx := 0
		appendVal := func(v Value) {
			// Arrays (and isConcatSpreadable objects) are spread; else appended.
			spread := v.Type() == TArr
			if o := rt.objPtr(v); o != nil && rt.symIsConcatSpreadable != 0 {
				if s := rt.getFieldSymbol(v, rt.symIsConcatSpreadable.handle()); !s.IsUndefined() {
					spread = rt.toBoolean(s)
				}
			}
			if spread {
				vn, _ := rt.lengthOf(v)
				for i := 0; i < vn; i++ {
					el, _ := rt.getElement(v, mknum(float64(i)))
					rt.setElement(res, mknum(float64(idx)), el)
					idx++
				}
			} else {
				rt.setElement(res, mknum(float64(idx)), v)
				idx++
			}
		}
		appendVal(this)
		for _, a := range args {
			appendVal(a)
		}
		return res, nil
	})
	rt.defMethod(proto, "reverse", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Generic (23.1.3.26): honors holes via HasProperty so a hole reverses to
		// a hole (Delete), routing every step through Proxy traps.
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for lower := 0; lower < n/2; lower++ {
			upper := n - 1 - lower
			lowerP := mknum(float64(lower))
			upperP := mknum(float64(upper))
			lowerExists := rt.hasElem(this, lower)
			var lowerVal Value
			if lowerExists {
				lowerVal, _ = rt.getElement(this, lowerP)
			}
			upperExists := rt.hasElem(this, upper)
			var upperVal Value
			if upperExists {
				upperVal, _ = rt.getElement(this, upperP)
			}
			switch {
			case lowerExists && upperExists:
				if e := rt.setElement(this, lowerP, upperVal); e != nil {
					return mkundef(), e
				}
				if e := rt.setElement(this, upperP, lowerVal); e != nil {
					return mkundef(), e
				}
			case upperExists:
				if e := rt.setElement(this, lowerP, upperVal); e != nil {
					return mkundef(), e
				}
				if _, e := rt.deleteElement(this, upperP); e != nil {
					return mkundef(), e
				}
			case lowerExists:
				if _, e := rt.deleteElement(this, lowerP); e != nil {
					return mkundef(), e
				}
				if e := rt.setElement(this, upperP, lowerVal); e != nil {
					return mkundef(), e
				}
			}
		}
		return this, nil
	})

	// Iteration methods taking a callback(value, index, array).
	rt.defMethod(proto, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			if _, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this}); e != nil {
				return mkundef(), e
			}
		}
		return mkundef(), nil
	})
	rt.defMethod(proto, "map", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		res, e := rt.arraySpeciesCreate(this, n)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			mapped, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			rt.setElement(res, mknum(float64(i)), mapped)
		}
		return res, nil
	})
	rt.defMethod(proto, "filter", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		res, e := rt.arraySpeciesCreate(this, 0)
		if e != nil {
			return mkundef(), e
		}
		outIdx := 0
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			keep, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(keep) {
				rt.setElement(res, mknum(float64(outIdx)), el)
				outIdx++
			}
		}
		return res, nil
	})
	rt.defMethod(proto, "reduce", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		var acc Value
		i := 0
		if len(args) > 1 {
			acc = args[1]
		} else {
			if n == 0 {
				return mkundef(), rt.typeError("Reduce of empty array with no initial value")
			}
			acc, _ = rt.getElement(this, mknum(0))
			i = 1
		}
		for ; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			acc, e = rt.callValue(cb, mkundef(), []Value{acc, el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
		}
		return acc, nil
	})

	rt.defMethod(proto, "splice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		start := relIndex(rt, arg(args, 0), n, 0)
		delCount := n - start
		if len(args) >= 2 {
			dc := rt.intArg(args, 1)
			if dc < 0 {
				dc = 0
			}
			if dc < delCount {
				delCount = dc
			}
		} else if len(args) == 0 {
			delCount = 0
		}
		// 23.1.3.28: collect removed elements (holes stay holes), shift the tail to
		// make room (Delete for holes), splice in the new items. HasProperty and
		// the Get/Set/Delete steps route through Proxy traps.
		removed := rt.newArray()
		rmo := rt.objPtr(removed)
		for i := 0; i < delCount; i++ {
			if rt.hasElem(this, start+i) {
				el, _ := rt.getElement(this, mknum(float64(start+i)))
				rt.arraySet(rmo, uint32(i), el)
			}
		}
		rmo.arrLen = uint32(delCount)
		var items []Value
		if len(args) > 2 {
			items = args[2:]
		}
		itemCount := len(items)
		switch {
		case itemCount < delCount:
			for i := start; i < n-delCount; i++ {
				from := i + delCount
				to := i + itemCount
				if rt.hasElem(this, from) {
					v, _ := rt.getElement(this, mknum(float64(from)))
					if e := rt.setElement(this, mknum(float64(to)), v); e != nil {
						return mkundef(), e
					}
				} else {
					if _, e := rt.deleteElement(this, mknum(float64(to))); e != nil {
						return mkundef(), e
					}
				}
			}
			for i := n; i > n-delCount+itemCount; i-- {
				rt.deleteElement(this, mknum(float64(i-1)))
			}
		case itemCount > delCount:
			for i := n - delCount; i > start; i-- {
				from := i + delCount - 1
				to := i + itemCount - 1
				if rt.hasElem(this, from) {
					v, _ := rt.getElement(this, mknum(float64(from)))
					if e := rt.setElement(this, mknum(float64(to)), v); e != nil {
						return mkundef(), e
					}
				} else {
					if _, e := rt.deleteElement(this, mknum(float64(to))); e != nil {
						return mkundef(), e
					}
				}
			}
		}
		for i := 0; i < itemCount; i++ {
			rt.setElement(this, mknum(float64(start+i)), items[i])
		}
		rt.setField(this, "length", mknum(float64(n-delCount+itemCount)))
		return removed, nil
	})
	rt.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		joinFn, _ := rt.getField(this, "join")
		if rt.isCallable(joinFn) {
			return rt.callValue(joinFn, this, nil)
		}
		return rt.internString(""), nil
	})

	rt.defMethod(proto, "find", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return el, nil
			}
		}
		return mkundef(), nil
	})
	rt.defMethod(proto, "findIndex", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	rt.defMethod(proto, "fill", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Generic (23.1.3.6): reads length, then Set for each index in range —
		// works on array-likes and routes element writes through Proxy traps.
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		val := arg(args, 0)
		start := relIndex(rt, arg(args, 1), n, 0)
		end := relIndex(rt, arg(args, 2), n, n)
		for i := start; i < end; i++ {
			if e := rt.setElement(this, mknum(float64(i)), val); e != nil {
				return mkundef(), e
			}
		}
		return this, nil
	})
	rt.defMethod(proto, "flat", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		depth := 1
		if !arg(args, 0).IsUndefined() {
			depth = rt.intArg(args, 0)
		}
		res := rt.newArray()
		rt.flattenInto(rt.objPtr(res), this, depth)
		return res, nil
	})

	rt.defMethod(proto, "every", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if !rt.toBoolean(r) {
				return mkfalse(), nil
			}
		}
		return mktrue(), nil
	})
	rt.defMethod(proto, "some", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return mktrue(), nil
			}
		}
		return mkfalse(), nil
	})
	rt.defMethod(proto, "lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := n - 1; i >= 0; i-- {
			el, _ := rt.getElement(this, mknum(float64(i)))
			if rt.strictEquals(el, target) {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	rt.defMethod(proto, "reduceRight", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		var acc Value
		i := n - 1
		if len(args) > 1 {
			acc = args[1]
		} else {
			if n == 0 {
				return mkundef(), rt.typeError("Reduce of empty array with no initial value")
			}
			acc, _ = rt.getElement(this, mknum(float64(i)))
			i--
		}
		for ; i >= 0; i-- {
			el, _ := rt.getElement(this, mknum(float64(i)))
			acc, e = rt.callValue(cb, mkundef(), []Value{acc, el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
		}
		return acc, nil
	})

	rt.defMethod(proto, "sort", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return this, nil
		}
		cmp := arg(args, 0)
		if !cmp.IsUndefined() && !rt.isCallable(cmp) {
			return mkundef(), rt.typeError("The comparison function must be either a function or undefined")
		}
		var sortErr *ThrowError
		sort.SliceStable(o.arr[:o.arrLen], func(i, j int) bool {
			if sortErr != nil {
				return false
			}
			a, b := o.arr[i], o.arr[j]
			if a.IsUndefined() {
				return false
			}
			if b.IsUndefined() {
				return true
			}
			if rt.isCallable(cmp) {
				r, e := rt.callValue(cmp, mkundef(), []Value{a, b})
				if e != nil {
					sortErr = e
					return false
				}
				nv, _ := rt.toNumberPrimitive(r)
				return nv < 0
			}
			sa, _ := rt.toStringValue(a)
			sb, _ := rt.toStringValue(b)
			return compareStrings(rt.strBytes(sa), rt.strBytes(sb)) < 0
		})
		if sortErr != nil {
			return mkundef(), sortErr
		}
		return this, nil
	})

	// Array constructor.
	ctor := rt.newNativeFunc("Array", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res := rt.newArray()
		ro := rt.objPtr(res)
		if len(args) == 1 && args[0].Type() == TNum {
			n := args[0].Number()
			if n < 0 || n != float64(uint32(n)) {
				return mkundef(), rt.rangeError("Invalid array length")
			}
			ro.arr = make([]Value, uint32(n))
			for i := range ro.arr {
				ro.arr[i] = tEmpty
			}
			ro.arrLen = uint32(n)
			return res, nil
		}
		for _, a := range args {
			rt.arraySet(ro, ro.arrLen, a)
		}
		return res, nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.arrayProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defMethod(cobj, "isArray", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkbool(rt.isArrayValue(arg(args, 0))), nil
	})
	rt.defMethod(cobj, "of", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res, e := rt.arrayFromCtor(this, len(args))
		if e != nil {
			return mkundef(), e
		}
		for i, a := range args {
			if e := rt.createDataProperty(res, mknum(float64(i)), a); e != nil {
				return mkundef(), e
			}
		}
		if e := rt.setField(res, "length", mknum(float64(len(args)))); e != nil {
			return mkundef(), e
		}
		return res, nil
	})
	rt.defMethod(cobj, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		src := arg(args, 0)
		mapFn := arg(args, 1)
		var items []Value
		// Iterable (Symbol.iterator: array/string/Map/Set/generators/user) takes
		// precedence over the array-like (length-indexed) path.
		if rt.isIterable(src) {
			it, e := rt.iterableValues(src)
			if e != nil {
				return mkundef(), e
			}
			items = it
		} else if o := rt.objPtr(src); o != nil {
			n, e := rt.lengthOf(src)
			if e != nil {
				return mkundef(), e
			}
			for i := 0; i < n; i++ {
				el, _ := rt.getElement(src, mknum(float64(i)))
				items = append(items, el)
			}
		} else if src.IsNullish() {
			return mkundef(), rt.typeError("Array.from requires an array-like or iterable object")
		}
		res, e := rt.arrayFromCtor(this, len(items))
		if e != nil {
			return mkundef(), e
		}
		for i, it := range items {
			v := it
			if rt.isCallable(mapFn) {
				mv, e := rt.callValue(mapFn, arg(args, 2), []Value{it, mknum(float64(i))})
				if e != nil {
					return mkundef(), e
				}
				v = mv
			}
			if e := rt.createDataProperty(res, mknum(float64(i)), v); e != nil {
				return mkundef(), e
			}
		}
		if e := rt.setField(res, "length", mknum(float64(len(items)))); e != nil {
			return mkundef(), e
		}
		return res, nil
	})
	// Array.prototype[Symbol.unscopables] (with-statement exclusion list).
	if rt.symUnscopables != 0 {
		unsc := rt.newObject(mknull())
		uo := rt.objPtr(unsc)
		for _, m := range []string{"at", "copyWithin", "entries", "fill", "find", "findIndex", "findLast", "findLastIndex", "flat", "flatMap", "includes", "keys", "toReversed", "toSorted", "toSpliced", "values"} {
			uo.defineOwn(m, mktrue(), attrDefault)
		}
		proto.defineOwnSymbol(rt.symUnscopables.handle(), unsc, attrConfigurable)
	}
	rt.defSpeciesGetter(ctor)
	rt.defGlobal("Array", ctor)
}

// isArrayValue implements IsArray(v): true for an Array exotic object or a
// Proxy whose (transitive) target is one (23.1.2.2 / 7.2.2).
func (rt *Runtime) isArrayValue(v Value) bool {
	for i := 0; i < maxProtoChainDepth; i++ {
		if v.Type() == TArr {
			return true
		}
		o := rt.objPtr(v)
		if o == nil || o.proxy == nil {
			return false
		}
		v = o.proxy.target
	}
	return false
}

// flattenInto appends the elements of arr into dst, recursing into nested
// arrays up to depth levels (Array.prototype.flat).
func (rt *Runtime) flattenInto(dst *object, arr Value, depth int) {
	o := rt.objPtr(arr)
	if o == nil {
		return
	}
	for i := uint32(0); i < o.arrLen; i++ {
		if int(i) >= len(o.arr) || o.arr[i].IsEmpty() {
			continue
		}
		el := o.arr[i]
		if depth > 0 && el.Type() == TArr {
			rt.flattenInto(dst, el, depth-1)
		} else {
			rt.arraySet(dst, dst.arrLen, el)
		}
	}
}

// relIndex resolves a slice/splice relative index (negative counts from end).
func relIndex(rt *Runtime, v Value, length, dflt int) int {
	if v.IsUndefined() {
		return dflt
	}
	n, _ := rt.toNumberPrimitive(v)
	i := int(n)
	if i < 0 {
		i += length
		if i < 0 {
			i = 0
		}
	}
	if i > length {
		i = length
	}
	return i
}
