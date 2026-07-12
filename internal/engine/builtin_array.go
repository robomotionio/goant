package engine

// Array constructor + Array.prototype (ant builtin_array). Core mutators and
// iteration methods; the ".generic" (array-like via .call) forms work because
// element access goes through length + getElement.

import "sort"

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
		for i := 1; i < n; i++ {
			v, _ := rt.getElement(this, mknum(float64(i)))
			rt.setElement(this, mknum(float64(i-1)), v)
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
		k := len(args)
		for i := n - 1; i >= 0; i-- {
			v, _ := rt.getElement(this, mknum(float64(i)))
			rt.setElement(this, mknum(float64(i+k)), v)
		}
		for i := 0; i < k; i++ {
			rt.setElement(this, mknum(float64(i)), args[i])
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
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			if rt.sameValueZero(el, target) {
				return mktrue(), nil
			}
		}
		return mkfalse(), nil
	})

	rt.defMethod(proto, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		start := relIndex(rt, arg(args, 0), n, 0)
		end := relIndex(rt, arg(args, 1), n, n)
		res := rt.newArray()
		ro := rt.objPtr(res)
		for i := start; i < end; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			rt.arraySet(ro, ro.arrLen, el)
		}
		return res, nil
	})
	rt.defMethod(proto, "concat", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res := rt.newArray()
		ro := rt.objPtr(res)
		appendVal := func(v Value) {
			if v.Type() == TArr {
				vo := rt.objPtr(v)
				for i := uint32(0); i < vo.arrLen; i++ {
					el := mkundef()
					if int(i) < len(vo.arr) {
						el = vo.arr[i]
					}
					rt.arraySet(ro, ro.arrLen, el)
				}
			} else {
				rt.arraySet(ro, ro.arrLen, v)
			}
		}
		appendVal(this)
		for _, a := range args {
			appendVal(a)
		}
		return res, nil
	})
	rt.defMethod(proto, "reverse", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Generic: works on arrays and array-likes via length + element access.
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n/2; i++ {
			j := n - 1 - i
			a, _ := rt.getElement(this, mknum(float64(i)))
			b, _ := rt.getElement(this, mknum(float64(j)))
			if e := rt.setElement(this, mknum(float64(i)), b); e != nil {
				return mkundef(), e
			}
			if e := rt.setElement(this, mknum(float64(j)), a); e != nil {
				return mkundef(), e
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
		res := rt.newArray()
		ro := rt.objPtr(res)
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			mapped, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			rt.arraySet(ro, ro.arrLen, mapped)
		}
		return res, nil
	})
	rt.defMethod(proto, "filter", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		res := rt.newArray()
		ro := rt.objPtr(res)
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(this, mknum(float64(i)))
			keep, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(keep) {
				rt.arraySet(ro, ro.arrLen, el)
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
		removed := rt.newArray()
		rmo := rt.objPtr(removed)
		for i := 0; i < delCount; i++ {
			el, _ := rt.getElement(this, mknum(float64(start+i)))
			rt.arraySet(rmo, uint32(i), el)
		}
		var items []Value
		if len(args) > 2 {
			items = args[2:]
		}
		itemCount := len(items)
		switch {
		case itemCount < delCount:
			for i := start; i < n-delCount; i++ {
				v, _ := rt.getElement(this, mknum(float64(i+delCount)))
				rt.setElement(this, mknum(float64(i+itemCount)), v)
			}
			for i := n; i > n-delCount+itemCount; i-- {
				rt.deleteElement(this, mknum(float64(i-1)))
			}
		case itemCount > delCount:
			for i := n - delCount; i > start; i-- {
				v, _ := rt.getElement(this, mknum(float64(i+delCount-1)))
				rt.setElement(this, mknum(float64(i+itemCount-1)), v)
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
		o := rt.objPtr(this)
		if o == nil {
			return this, nil
		}
		val := arg(args, 0)
		n := int(o.arrLen)
		start := relIndex(rt, arg(args, 1), n, 0)
		end := relIndex(rt, arg(args, 2), n, n)
		for i := start; i < end; i++ {
			rt.arraySet(o, uint32(i), val)
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
		return mkbool(arg(args, 0).Type() == TArr), nil
	})
	rt.defMethod(cobj, "of", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res := rt.newArray()
		ro := rt.objPtr(res)
		for _, a := range args {
			rt.arraySet(ro, ro.arrLen, a)
		}
		return res, nil
	})
	rt.defMethod(cobj, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		src := arg(args, 0)
		mapFn := arg(args, 1)
		res := rt.newArray()
		ro := rt.objPtr(res)
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
		for i, it := range items {
			v := it
			if rt.isCallable(mapFn) {
				mv, e := rt.callValue(mapFn, arg(args, 2), []Value{it, mknum(float64(i))})
				if e != nil {
					return mkundef(), e
				}
				v = mv
			}
			rt.arraySet(ro, ro.arrLen, v)
		}
		return res, nil
	})
	rt.defGlobal("Array", ctor)
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
