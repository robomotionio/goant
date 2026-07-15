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

// toIntegerOrInfinity implements ToIntegerOrInfinity(value): NaN → 0, ±Infinity
// preserved, otherwise truncated toward zero, propagating an abrupt ToNumber.
func (rt *Runtime) toIntegerOrInfinity(v Value) (float64, *ThrowError) {
	n, e := rt.toNumber(v)
	if e != nil {
		return 0, e
	}
	if math.IsNaN(n) {
		return 0, nil
	}
	if math.IsInf(n, 0) {
		return n, nil
	}
	return math.Trunc(n), nil
}

// relativeIndexE clamps a relative index argument to [0, n] with the spec's
// negative-from-end / overflow handling, propagating abrupt coercions.
func (rt *Runtime) relativeIndexE(v Value, n int) (int, *ThrowError) {
	if v.IsUndefined() {
		return 0, nil
	}
	d, e := rt.toIntegerOrInfinity(v)
	if e != nil {
		return 0, e
	}
	if math.IsInf(d, -1) {
		return 0, nil
	}
	if math.IsInf(d, 1) {
		return n, nil
	}
	k := int(d)
	if k < 0 {
		if k += n; k < 0 {
			k = 0
		}
	} else if k > n {
		k = n
	}
	return k, nil
}

// sortCompare implements SortCompare: undefined sorts to the end; otherwise a
// comparator's result is ToNumber'd (NaN -> 0), or the default ToString ordering
// is used. Returns <0, 0, or >0 and propagates an abrupt comparator/ToString.
func (rt *Runtime) sortCompare(x, y, cmp Value) (int, *ThrowError) {
	xu, yu := x.IsUndefined(), y.IsUndefined()
	if xu && yu {
		return 0, nil
	}
	if xu {
		return 1, nil
	}
	if yu {
		return -1, nil
	}
	if !cmp.IsUndefined() {
		r, e := rt.callValue(cmp, mkundef(), []Value{x, y})
		if e != nil {
			return 0, e
		}
		nv, e := rt.toNumber(r)
		if e != nil {
			return 0, e
		}
		if math.IsNaN(nv) {
			return 0, nil
		}
		if nv < 0 {
			return -1, nil
		}
		if nv > 0 {
			return 1, nil
		}
		return 0, nil
	}
	sx, e := rt.toStringValue(x)
	if e != nil {
		return 0, e
	}
	sy, e := rt.toStringValue(y)
	if e != nil {
		return 0, e
	}
	return compareStrings(rt.strBytes(sx), rt.strBytes(sy)), nil
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
// arrayFromAsync implements Array.fromAsync(asyncItems, mapfn, thisArg): an async
// function that Awaits each element — from an async iterator, a sync iterator
// (wrapped), or an array-like — before adding it, returning a promise of the
// resulting array. All work is driven through promise continuations since a
// native cannot suspend on await.
func (rt *Runtime) arrayFromAsync(C, asyncItems, mapfn, thisArg Value) Value {
	resultP, resolveCap, rejectCap, _ := rt.newPromiseCapability(rt.promiseCtor)
	resolve := func(v Value) { rt.callValue(resolveCap, mkundef(), []Value{v}) }
	reject := func(v Value) { rt.callValue(rejectCap, mkundef(), []Value{v}) }

	mapping := false
	if !mapfn.IsUndefined() {
		if !rt.isCallable(mapfn) {
			reject(rt.typeError("Array.fromAsync mapper is not a function").Value)
			return resultP
		}
		mapping = true
	}

	// await runs onF(value) once `value` fulfills and onR(reason) if it rejects,
	// implementing the Await of the async function body via PromiseResolve/then.
	await := func(value Value, onF, onR func(Value)) {
		p := rt.resolvedPromise(value)
		fF := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) { onF(arg(a, 0)); return mkundef(), nil })
		fR := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) { onR(arg(a, 0)); return mkundef(), nil })
		rt.promiseThen(fF, fR, rt.objPtr(p))
	}

	// Resolve the iterator: @@asyncIterator, else @@iterator wrapped as async.
	var iter Value
	hasIter := false
	if !asyncItems.IsNullish() {
		asyncM := mkundef()
		if rt.symAsyncIterator != 0 {
			m, e := rt.getElement(asyncItems, rt.symAsyncIterator)
			if e != nil {
				reject(e.Value)
				return resultP
			}
			asyncM = m
		}
		if !asyncM.IsNullish() {
			if !rt.isCallable(asyncM) {
				reject(rt.typeError("[Symbol.asyncIterator] is not a function").Value)
				return resultP
			}
			it, e := rt.callValue(asyncM, asyncItems, nil)
			if e != nil {
				reject(e.Value)
				return resultP
			}
			if !it.IsObjectType() {
				reject(rt.typeError("[Symbol.asyncIterator]() returned a non-object").Value)
				return resultP
			}
			iter, hasIter = it, true
		} else {
			syncM := mkundef()
			if rt.symIterator != 0 {
				m, e := rt.getElement(asyncItems, rt.symIterator)
				if e != nil {
					reject(e.Value)
					return resultP
				}
				syncM = m
			}
			if !syncM.IsNullish() {
				if !rt.isCallable(syncM) {
					reject(rt.typeError("[Symbol.iterator] is not a function").Value)
					return resultP
				}
				syncIt, e := rt.callValue(syncM, asyncItems, nil)
				if e != nil {
					reject(e.Value)
					return resultP
				}
				if !syncIt.IsObjectType() {
					reject(rt.typeError("[Symbol.iterator]() returned a non-object").Value)
					return resultP
				}
				iter, hasIter = rt.createAsyncFromSyncIterator(syncIt), true
			}
		}
	}

	if hasIter {
		nextMethod, e := rt.getField(iter, "next")
		if e != nil {
			reject(e.Value)
			return resultP
		}
		var A Value
		if rt.isCallable(C) {
			a, e := rt.construct(C, nil)
			if e != nil {
				reject(e.Value)
				return resultP
			}
			A = a
		} else {
			A = rt.newArray()
		}
		// closeReject performs AsyncIteratorClose(iter, throw) then rejects with the
		// original reason (the iterator's return result/error is discarded).
		closeReject := func(reason Value) {
			rf, e := rt.getField(iter, "return")
			if e != nil || !rt.isCallable(rf) {
				reject(reason)
				return
			}
			rres, ce := rt.callValue(rf, iter, nil)
			if ce != nil {
				reject(reason)
				return
			}
			await(rres, func(_ Value) { reject(reason) }, func(_ Value) { reject(reason) })
		}
		var step func(k int)
		step = func(k int) {
			nextResult, e := rt.callValue(nextMethod, iter, nil)
			if e != nil {
				reject(e.Value)
				return
			}
			await(nextResult, func(res Value) {
				if !res.IsObjectType() {
					reject(rt.typeError("iterator result is not an object").Value)
					return
				}
				doneV, e := rt.getField(res, "done")
				if e != nil {
					reject(e.Value)
					return
				}
				if rt.toBoolean(doneV) {
					if ok, se := rt.setFieldR(A, "length", mknum(float64(k))); se != nil {
						reject(se.Value)
					} else if !ok {
						reject(rt.typeError("Array.fromAsync: cannot set length").Value)
					} else {
						resolve(A)
					}
					return
				}
				val, e := rt.getField(res, "value")
				if e != nil {
					reject(e.Value)
					return
				}
				add := func(mapped Value) {
					if e := rt.createDataProperty(A, mknum(float64(k)), mapped); e != nil {
						closeReject(e.Value)
						return
					}
					step(k + 1)
				}
				if mapping {
					mv, e := rt.callValue(mapfn, thisArg, []Value{val, mknum(float64(k))})
					if e != nil {
						closeReject(e.Value)
						return
					}
					await(mv, add, closeReject)
				} else {
					add(val)
				}
			}, reject)
		}
		step(0)
		return resultP
	}

	// Array-like path: ToObject, then Await each element in [0, len).
	arrayLike := asyncItems
	if !arrayLike.IsObjectType() {
		o, e := rt.toObjectValue(arrayLike)
		if e != nil {
			reject(e.Value)
			return resultP
		}
		arrayLike = o
	}
	n, e := rt.lengthOf(arrayLike)
	if e != nil {
		reject(e.Value)
		return resultP
	}
	var A Value
	if rt.isCallable(C) {
		a, e := rt.construct(C, []Value{mknum(float64(n))})
		if e != nil {
			reject(e.Value)
			return resultP
		}
		A = a
	} else {
		a, e := rt.arrayCreate(n) // ArrayCreate(len): RangeError when len > 2^32-1
		if e != nil {
			reject(e.Value)
			return resultP
		}
		A = a
	}
	var stepAL func(k int)
	stepAL = func(k int) {
		if k >= n {
			if ok, se := rt.setFieldR(A, "length", mknum(float64(n))); se != nil {
				reject(se.Value)
			} else if !ok {
				reject(rt.typeError("Array.fromAsync: cannot set length").Value)
			} else {
				resolve(A)
			}
			return
		}
		kValue, e := rt.getElement(arrayLike, mknum(float64(k)))
		if e != nil {
			reject(e.Value)
			return
		}
		await(kValue, func(awaited Value) {
			add := func(mapped Value) {
				if e := rt.createDataProperty(A, mknum(float64(k)), mapped); e != nil {
					reject(e.Value)
					return
				}
				stepAL(k + 1)
			}
			if mapping {
				mv, e := rt.callValue(mapfn, thisArg, []Value{awaited, mknum(float64(k))})
				if e != nil {
					reject(e.Value)
					return
				}
				await(mv, add, reject)
			} else {
				add(awaited)
			}
		}, reject)
	}
	stepAL(0)
	return resultP
}

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

// arrayCreate implements ArrayCreate(length): a fresh array of the given length,
// throwing RangeError when length exceeds the 2^32-1 array-length ceiling. The
// length is tracked without materializing holes (sparse), so a large length is
// cheap.
func (rt *Runtime) arrayCreate(length int) (Value, *ThrowError) {
	if length < 0 || length > 0xFFFFFFFF {
		return mkundef(), rt.rangeError("Invalid array length")
	}
	a := rt.newArray()
	rt.objPtr(a).arrLen = uint32(length)
	return a, nil
}

// setLengthOrThrow performs Set(O, "length", n, true): a TypeError if the length
// is not writable. The array mutators always Set length with throw=true.
func (rt *Runtime) setLengthOrThrow(obj Value, n float64) *ThrowError {
	ok, e := rt.setFieldR(obj, "length", mknum(n))
	if e != nil {
		return e
	}
	if !ok {
		return rt.typeError("Cannot assign to read only property 'length'")
	}
	return nil
}

// arrayRejectsGrowth reports whether adding a new index to obj is rejected (a
// non-extensible or non-writable-length array); the growing mutators throw
// up-front rather than partially mutating.
func (rt *Runtime) arrayRejectsGrowth(obj Value) bool {
	if obj.Type() != TArr {
		return false
	}
	o := rt.objPtr(obj)
	return o != nil && (!o.flags.extensible || o.flags.arrLenNonWritable)
}

func (rt *Runtime) arraySpeciesCreate(this Value, length int) (Value, *ThrowError) {
	// ArraySpeciesCreate (23.1.3.1): a non-array uses ArrayCreate(length); otherwise
	// read constructor and its @@species (both via [[Get]], so a Proxy observes
	// them). A non-undefined, non-constructor species is a TypeError.
	if !rt.isArrayValue(this) {
		return rt.arrayCreate(length)
	}
	ctor, e := rt.getField(this, "constructor")
	if e != nil {
		return mkundef(), e
	}
	if ctor.IsObjectType() && rt.symSpecies != 0 {
		// C is replaced by Get(C, @@species): undefined/null species -> default array.
		sp, e := rt.getElement(ctor, rt.symSpecies)
		if e != nil {
			return mkundef(), e
		}
		ctor = sp
		if ctor.IsNull() {
			ctor = mkundef()
		}
	}
	if ctor.IsUndefined() {
		return rt.arrayCreate(length)
	}
	if !rt.isConstructorValue(ctor) {
		return mkundef(), rt.typeError("Array species constructor is not a constructor")
	}
	return rt.construct(ctor, []Value{mknum(float64(length))})
}

func (rt *Runtime) initArrayBuiltin() {
	proto := rt.objPtr(rt.arrayProto)

	// Mutators are generic: they read length + elements through the ordinary
	// property protocol, so they work on arrays and array-like objects alike.
	rt.defMethod(proto, "push", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		if float64(n)+float64(len(args)) > 9007199254740991 {
			return mkundef(), rt.typeError("Pushing past the maximum array length")
		}
		if len(args) > 0 && rt.arrayRejectsGrowth(obj) {
			return mkundef(), rt.typeError("Cannot add property " + numberToString(float64(n)) + ", object is not extensible")
		}
		for _, a := range args {
			if e := rt.setElement(obj, mknum(float64(n)), a); e != nil {
				return mkundef(), e
			}
			n++
		}
		if e := rt.setLengthOrThrow(obj, float64(n)); e != nil {
			return mkundef(), e
		}
		return mknum(float64(n)), nil
	})
	rt.defMethod(proto, "pop", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		// A locked (non-writable) array length rejects the shrink up-front, before
		// any element is deleted.
		if o := rt.objPtr(obj); o != nil && obj.Type() == TArr && o.flags.arrLenNonWritable {
			return mkundef(), rt.typeError("Cannot assign to read only property 'length'")
		}
		if n == 0 {
			return mkundef(), rt.setLengthOrThrow(obj, 0)
		}
		v, e := rt.getElement(obj, mknum(float64(n-1)))
		if e != nil {
			return mkundef(), e
		}
		if _, e := rt.deleteElement(obj, mknum(float64(n-1))); e != nil {
			return mkundef(), e
		}
		if e := rt.setLengthOrThrow(obj, float64(n-1)); e != nil {
			return mkundef(), e
		}
		return v, nil
	})
	rt.defMethod(proto, "shift", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		if o := rt.objPtr(obj); o != nil && obj.Type() == TArr && o.flags.arrLenNonWritable {
			return mkundef(), rt.typeError("Cannot assign to read only property 'length'")
		}
		if n == 0 {
			return mkundef(), rt.setLengthOrThrow(obj, 0)
		}
		first, e := rt.getElement(obj, mknum(0))
		if e != nil {
			return mkundef(), e
		}
		// 23.1.3.27: shift each element down one; a hole propagates as a Delete of
		// the destination rather than an overwrite.
		for k := 1; k < n; k++ {
			from := mknum(float64(k))
			to := mknum(float64(k - 1))
			present, e := rt.hasElemE(obj, k)
			if e != nil {
				return mkundef(), e
			}
			if present {
				v, e := rt.getElement(obj, from)
				if e != nil {
					return mkundef(), e
				}
				if e := rt.setElement(obj, to, v); e != nil {
					return mkundef(), e
				}
			} else {
				if _, e := rt.deleteElement(obj, to); e != nil {
					return mkundef(), e
				}
			}
		}
		if _, e := rt.deleteElement(obj, mknum(float64(n-1))); e != nil {
			return mkundef(), e
		}
		if e := rt.setLengthOrThrow(obj, float64(n-1)); e != nil {
			return mkundef(), e
		}
		return first, nil
	})
	rt.defMethod(proto, "unshift", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		// 23.1.3.32: shift the tail up by argCount (holes Delete their target),
		// then prepend the new items.
		k := len(args)
		if k > 0 {
			if float64(n)+float64(k) > 9007199254740991 {
				return mkundef(), rt.typeError("Unshifting past the maximum array length")
			}
			if rt.arrayRejectsGrowth(obj) {
				return mkundef(), rt.typeError("Cannot add property " + numberToString(float64(n)) + ", object is not extensible")
			}
			for i := n; i > 0; i-- {
				from := i - 1
				to := i - 1 + k
				present, e := rt.hasElemE(obj, from)
				if e != nil {
					return mkundef(), e
				}
				if present {
					v, e := rt.getElement(obj, mknum(float64(from)))
					if e != nil {
						return mkundef(), e
					}
					if e := rt.setElement(obj, mknum(float64(to)), v); e != nil {
						return mkundef(), e
					}
				} else {
					if _, e := rt.deleteElement(obj, mknum(float64(to))); e != nil {
						return mkundef(), e
					}
				}
			}
			for i := 0; i < k; i++ {
				if e := rt.setElement(obj, mknum(float64(i)), args[i]); e != nil {
					return mkundef(), e
				}
			}
		}
		if e := rt.setLengthOrThrow(obj, float64(n+k)); e != nil {
			return mkundef(), e
		}
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
		if n == 0 {
			return mknum(-1), nil
		}
		// fromIndex: relativeIndexE gives the spec's k (+Inf -> n so the loop
		// is skipped and we return -1; -Inf -> 0; negative counts from the end).
		k, e := rt.relativeIndexE(arg(args, 1), n)
		if e != nil {
			return mkundef(), e
		}
		for i := k; i < n; i++ {
			if !rt.hasElem(this, i) { // indexOf skips holes (HasProperty)
				continue
			}
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
		if n == 0 {
			return mkfalse(), nil
		}
		start, e := rt.relativeIndexE(arg(args, 1), n)
		if e != nil {
			return mkundef(), e
		}
		for i := start; i < n; i++ {
			el, ee := rt.getElement(this, mknum(float64(i)))
			if ee != nil {
				return mkundef(), ee
			}
			if rt.sameValueZero(el, target) {
				return mktrue(), nil
			}
		}
		return mkfalse(), nil
	})
	rt.defMethod(proto, "at", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		d, e := rt.toNumber(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(d) {
			d = 0
		}
		k := int(math.Trunc(d))
		if k < 0 {
			k += n
		}
		if k < 0 || k >= n {
			return mkundef(), nil
		}
		return rt.getElement(this, mknum(float64(k)))
	})
	rt.defMethod(proto, "flatMap", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("flatMap mapper is not a function")
		}
		res, e := rt.arraySpeciesCreate(obj, 0)
		if e != nil {
			return mkundef(), e
		}
		// FlattenIntoArray with depth 1 and the mapper applied per source element.
		if _, e := rt.flattenIntoArray(res, obj, n, 0, 1, cb, arg(args, 1)); e != nil {
			return mkundef(), e
		}
		return res, nil
	})
	rt.defMethod(proto, "findLast", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.findLast predicate is not a function")
		}
		for i := n - 1; i >= 0; i-- {
			el, e := rt.getElement(this, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
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
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.findLastIndex predicate is not a function")
		}
		for i := n - 1; i >= 0; i-- {
			el, e := rt.getElement(this, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
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
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		to, e := rt.relativeIndexE(arg(args, 0), n)
		if e != nil {
			return mkundef(), e
		}
		from, e := rt.relativeIndexE(arg(args, 1), n)
		if e != nil {
			return mkundef(), e
		}
		final := n
		if len(args) > 2 && !arg(args, 2).IsUndefined() {
			if final, e = rt.relativeIndexE(arg(args, 2), n); e != nil {
				return mkundef(), e
			}
		}
		count := final - from
		if count > n-to {
			count = n - to
		}
		// 23.1.3.4: copy in place with a direction chosen to avoid clobbering the
		// source when the ranges overlap; a hole in the source Deletes the target
		// (DeletePropertyOrThrow), and HasProperty/Get abrupts propagate.
		dir := 1
		if from < to && to < from+count {
			dir = -1
			from += count - 1
			to += count - 1
		}
		for ; count > 0; count-- {
			fromP := mknum(float64(from))
			toP := mknum(float64(to))
			present, e := rt.hasElemE(obj, from)
			if e != nil {
				return mkundef(), e
			}
			if present {
				v, e := rt.getElement(obj, fromP)
				if e != nil {
					return mkundef(), e
				}
				if e := rt.setElement(obj, toP, v); e != nil {
					return mkundef(), e
				}
			} else {
				ok, e := rt.deleteElement(obj, toP)
				if e != nil {
					return mkundef(), e
				}
				if !ok {
					return mkundef(), rt.typeError("Cannot delete property '" + numberToString(float64(to)) + "'")
				}
			}
			from += dir
			to += dir
		}
		return obj, nil
	})
	// CreateArrayIterator is called on ? ToObject(this), so null/undefined (and a
	// throwing coercion) reject at call time rather than being swallowed later.
	rt.defMethod(proto, "entries", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newIndexIterator(o, iterEntries), nil
	})
	rt.defMethod(proto, "keys", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newIndexIterator(o, iterKeys), nil
	})
	valuesFn := rt.newNativeFunc("values", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newIndexIterator(o, iterValues), nil
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
		// The by-copy methods ArrayCreate a result of this length: a length above
		// 2**32-1 is a RangeError, thrown before any element is read.
		if n > 0xFFFFFFFF {
			return nil, rt.rangeError("Invalid array length")
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
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		start, e := rt.relativeIndexE(arg(args, 0), n)
		if e != nil {
			return mkundef(), e
		}
		end := n
		if len(args) > 1 && !arg(args, 1).IsUndefined() {
			if end, e = rt.relativeIndexE(arg(args, 1), n); e != nil {
				return mkundef(), e
			}
		}
		count := end - start
		if count < 0 {
			count = 0
		}
		res, e := rt.arraySpeciesCreate(obj, count)
		if e != nil {
			return mkundef(), e
		}
		// Copy each present index (HasProperty walks the prototype) via
		// CreateDataPropertyOrThrow, then Set the result length.
		out := 0
		for k := start; k < end; k++ {
			present, e := rt.hasElemE(obj, k)
			if e != nil {
				return mkundef(), e
			}
			if present {
				el, e := rt.getElement(obj, mknum(float64(k)))
				if e != nil {
					return mkundef(), e
				}
				if e := rt.createDataProperty(res, mknum(float64(out)), el); e != nil {
					return mkundef(), e
				}
			}
			out++
		}
		if e := rt.setField(res, "length", mknum(float64(out))); e != nil {
			return mkundef(), e
		}
		return res, nil
	})
	rt.defMethod(proto, "concat", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		res, e := rt.arraySpeciesCreate(obj, 0)
		if e != nil {
			return mkundef(), e
		}
		idx := 0
		appendVal := func(v Value) *ThrowError {
			// IsConcatSpreadable: @@isConcatSpreadable (via [[Get]]) overrides the
			// default, which is IsArray(v).
			spread := rt.isArrayValue(v)
			if v.IsObjectType() && rt.symIsConcatSpreadable != 0 {
				s, e := rt.getElement(v, rt.symIsConcatSpreadable)
				if e != nil {
					return e
				}
				if !s.IsUndefined() {
					spread = rt.toBoolean(s)
				}
			}
			if spread {
				vn, e := rt.lengthOf(v)
				if e != nil {
					return e
				}
				for i := 0; i < vn; i++ {
					if rt.hasElem(v, i) {
						el, e := rt.getElement(v, mknum(float64(i)))
						if e != nil {
							return e
						}
						if e := rt.createDataProperty(res, mknum(float64(idx)), el); e != nil {
							return e
						}
					}
					idx++
				}
			} else {
				if e := rt.createDataProperty(res, mknum(float64(idx)), v); e != nil {
					return e
				}
				idx++
			}
			return nil
		}
		if e := appendVal(obj); e != nil {
			return mkundef(), e
		}
		for _, a := range args {
			if e := appendVal(a); e != nil {
				return mkundef(), e
			}
		}
		rt.setField(res, "length", mknum(float64(idx)))
		return res, nil
	})
	rt.defMethod(proto, "reverse", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Generic (23.1.3.26): honors holes via HasProperty so a hole reverses to
		// a hole (Delete), routing every step through Proxy traps.
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		for lower := 0; lower < n/2; lower++ {
			upper := n - 1 - lower
			lowerP := mknum(float64(lower))
			upperP := mknum(float64(upper))
			lowerExists, e := rt.hasElemE(obj, lower)
			if e != nil {
				return mkundef(), e
			}
			var lowerVal Value
			if lowerExists {
				if lowerVal, e = rt.getElement(obj, lowerP); e != nil {
					return mkundef(), e
				}
			}
			upperExists, e := rt.hasElemE(obj, upper)
			if e != nil {
				return mkundef(), e
			}
			var upperVal Value
			if upperExists {
				if upperVal, e = rt.getElement(obj, upperP); e != nil {
					return mkundef(), e
				}
			}
			switch {
			case lowerExists && upperExists:
				if e := rt.setElement(obj, lowerP, upperVal); e != nil {
					return mkundef(), e
				}
				if e := rt.setElement(obj, upperP, lowerVal); e != nil {
					return mkundef(), e
				}
			case upperExists:
				if e := rt.setElement(obj, lowerP, upperVal); e != nil {
					return mkundef(), e
				}
				if _, e := rt.deleteElement(obj, upperP); e != nil {
					return mkundef(), e
				}
			case lowerExists:
				if _, e := rt.deleteElement(obj, lowerP); e != nil {
					return mkundef(), e
				}
				if e := rt.setElement(obj, upperP, lowerVal); e != nil {
					return mkundef(), e
				}
			}
		}
		return obj, nil
	})

	// Iteration methods taking a callback(value, index, array).
	rt.defMethod(proto, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.forEach callback is not a function")
		}
		for i := 0; i < n; i++ {
			present, e := rt.hasElemE(obj, i) // forEach skips holes
			if e != nil {
				return mkundef(), e
			}
			if !present {
				continue
			}
			el, e := rt.getElement(obj, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			if _, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), obj}); e != nil {
				return mkundef(), e
			}
		}
		return mkundef(), nil
	})
	rt.defMethod(proto, "map", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.map callback is not a function")
		}
		res, e := rt.arraySpeciesCreate(obj, n)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n; i++ {
			present, e := rt.hasElemE(obj, i) // map skips holes (result keeps the hole)
			if e != nil {
				return mkundef(), e
			}
			if !present {
				continue
			}
			el, e := rt.getElement(obj, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			mapped, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), obj})
			if e != nil {
				return mkundef(), e
			}
			if e := rt.createDataProperty(res, mknum(float64(i)), mapped); e != nil {
				return mkundef(), e
			}
		}
		return res, nil
	})
	rt.defMethod(proto, "filter", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.filter callback is not a function")
		}
		res, e := rt.arraySpeciesCreate(obj, 0)
		if e != nil {
			return mkundef(), e
		}
		outIdx := 0
		for i := 0; i < n; i++ {
			present, e := rt.hasElemE(obj, i) // filter skips holes
			if e != nil {
				return mkundef(), e
			}
			if !present {
				continue
			}
			el, e := rt.getElement(obj, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			keep, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), obj})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(keep) {
				if e := rt.createDataProperty(res, mknum(float64(outIdx)), el); e != nil {
					return mkundef(), e
				}
				outIdx++
			}
		}
		return res, nil
	})
	rt.defMethod(proto, "reduce", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.reduce callback is not a function")
		}
		var acc Value
		i := 0
		if len(args) > 1 {
			acc = args[1]
		} else {
			// Seed the accumulator with the first present element (holes skipped).
			found := false
			for ; i < n; i++ {
				present, e := rt.hasElemE(obj, i)
				if e != nil {
					return mkundef(), e
				}
				if present {
					if acc, e = rt.getElement(obj, mknum(float64(i))); e != nil {
						return mkundef(), e
					}
					i++
					found = true
					break
				}
			}
			if !found {
				return mkundef(), rt.typeError("Reduce of empty array with no initial value")
			}
		}
		for ; i < n; i++ {
			present, e := rt.hasElemE(obj, i) // reduce skips holes
			if e != nil {
				return mkundef(), e
			}
			if !present {
				continue
			}
			el, e := rt.getElement(obj, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			if acc, e = rt.callValue(cb, mkundef(), []Value{acc, el, mknum(float64(i)), obj}); e != nil {
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
		start, e := rt.relativeIndexE(arg(args, 0), n)
		if e != nil {
			return mkundef(), e
		}
		delCount := 0
		if len(args) == 1 {
			delCount = n - start
		} else if len(args) >= 2 {
			// actualDeleteCount = clamp(ToIntegerOrInfinity(deleteCount), 0, len-start).
			dcF, e := rt.toIntegerOrInfinity(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			if dcF >= float64(n-start) {
				delCount = n - start
			} else if dcF > 0 {
				delCount = int(dcF)
			}
		}
		itemCount := 0
		if len(args) > 2 {
			itemCount = len(args) - 2
		}
		// The resulting length must not exceed 2^53-1 (integer-index limit).
		if float64(n)+float64(itemCount)-float64(delCount) > 9007199254740991 {
			return mkundef(), rt.typeError("Invalid array length")
		}
		// 23.1.3.28: collect removed elements (holes stay holes), shift the tail to
		// make room (Delete for holes), splice in the new items. HasProperty and
		// the Get/Set/Delete steps route through Proxy traps; the removed array is
		// built via ArraySpeciesCreate (reads constructor/@@species).
		removed, e := rt.arraySpeciesCreate(this, delCount)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < delCount; i++ {
			if rt.hasElem(this, start+i) {
				el, e := rt.getElement(this, mknum(float64(start+i)))
				if e != nil {
					return mkundef(), e
				}
				if e := rt.createDataProperty(removed, mknum(float64(i)), el); e != nil {
					return mkundef(), e
				}
			}
		}
		rt.setField(removed, "length", mknum(float64(delCount)))
		var items []Value
		if len(args) > 2 {
			items = args[2:]
		}
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
		if e := rt.setLengthOrThrow(this, float64(n-delCount+itemCount)); e != nil {
			return mkundef(), e
		}
		return removed, nil
	})
	rt.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(o)
		if e != nil {
			return mkundef(), e
		}
		var out []byte
		for i := 0; i < n; i++ {
			if i > 0 {
				out = append(out, ',')
			}
			el, e := rt.getElement(o, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			if el.IsNullish() { // undefined/null contribute the empty string
				continue
			}
			// ToString(? Invoke(element, "toLocaleString")).
			tls, e := rt.getField(el, "toLocaleString")
			if e != nil {
				return mkundef(), e
			}
			if !rt.isCallable(tls) {
				return mkundef(), rt.typeError("element toLocaleString is not a function")
			}
			r, e := rt.callValue(tls, el, nil)
			if e != nil {
				return mkundef(), e
			}
			s, e := rt.toStringValue(r)
			if e != nil {
				return mkundef(), e
			}
			out = append(out, rt.strBytes(s)...)
		}
		return rt.newStringBytes(out), nil
	})

	rt.defMethod(proto, "find", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		n, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.find predicate is not a function")
		}
		// find visits every index via Get (no hole skipping); a hole reads through
		// the prototype and the predicate still runs.
		for i := 0; i < n; i++ {
			el, e := rt.getElement(this, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
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
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.findIndex predicate is not a function")
		}
		for i := 0; i < n; i++ {
			el, e := rt.getElement(this, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
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
		// Generic (23.1.3.6): ToObject, read length, then Set for each index in
		// range — works on array-likes and routes writes through Proxy traps. An
		// abrupt ToIntegerOrInfinity of start/end (Symbol, throwing valueOf) propagates.
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		val := arg(args, 0)
		start, e := rt.relativeIndexE(arg(args, 1), n)
		if e != nil {
			return mkundef(), e
		}
		end := n
		if len(args) > 2 && !arg(args, 2).IsUndefined() {
			if end, e = rt.relativeIndexE(arg(args, 2), n); e != nil {
				return mkundef(), e
			}
		}
		for i := start; i < end; i++ {
			if e := rt.setElement(obj, mknum(float64(i)), val); e != nil {
				return mkundef(), e
			}
		}
		return obj, nil
	})
	rt.defMethod(proto, "flat", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		depth := 1
		if !arg(args, 0).IsUndefined() {
			d, e := rt.toIntegerOrInfinity(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			switch {
			case d > 0x7FFFFFFF:
				depth = 0x7FFFFFFF
			case d < 0:
				depth = 0
			default:
				depth = int(d)
			}
		}
		res, e := rt.arraySpeciesCreate(obj, 0)
		if e != nil {
			return mkundef(), e
		}
		if _, e := rt.flattenIntoArray(res, obj, n, 0, depth, mkundef(), mkundef()); e != nil {
			return mkundef(), e
		}
		return res, nil
	})

	rt.defMethod(proto, "every", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.every callback is not a function")
		}
		for i := 0; i < n; i++ {
			present, e := rt.hasElemE(obj, i) // every skips holes
			if e != nil {
				return mkundef(), e
			}
			if !present {
				continue
			}
			el, e := rt.getElement(obj, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), obj})
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
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.some callback is not a function")
		}
		for i := 0; i < n; i++ {
			present, e := rt.hasElemE(obj, i) // some skips holes
			if e != nil {
				return mkundef(), e
			}
			if !present {
				continue
			}
			el, e := rt.getElement(obj, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), obj})
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
		length, e := rt.lengthOf(this)
		if e != nil {
			return mkundef(), e
		}
		if length == 0 {
			return mknum(-1), nil
		}
		// Default start is the last index; an explicit fromIndex (present, even if
		// undefined) is ToIntegerOrInfinity'd. -Inf (or a negative that lands before
		// index 0) yields -1; +Inf / a too-large start clamps to len-1.
		start := length - 1
		if len(args) > 1 {
			fromF, e := rt.toIntegerOrInfinity(args[1])
			if e != nil {
				return mkundef(), e
			}
			if math.IsInf(fromF, -1) {
				return mknum(-1), nil
			}
			if fromF >= 0 {
				if fromF < float64(length-1) {
					start = int(fromF)
				}
			} else {
				nf := float64(length) + fromF
				if nf < 0 {
					return mknum(-1), nil
				}
				start = int(nf)
			}
		}
		for i := start; i >= 0; i-- {
			if !rt.hasElem(this, i) { // lastIndexOf skips holes
				continue
			}
			el, ee := rt.getElement(this, mknum(float64(i)))
			if ee != nil {
				return mkundef(), ee
			}
			if rt.strictEquals(el, target) {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	rt.defMethod(proto, "reduceRight", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("Array.prototype.reduceRight callback is not a function")
		}
		var acc Value
		i := n - 1
		if len(args) > 1 {
			acc = args[1]
		} else {
			found := false
			for ; i >= 0; i-- {
				present, e := rt.hasElemE(obj, i)
				if e != nil {
					return mkundef(), e
				}
				if present {
					if acc, e = rt.getElement(obj, mknum(float64(i))); e != nil {
						return mkundef(), e
					}
					i--
					found = true
					break
				}
			}
			if !found {
				return mkundef(), rt.typeError("Reduce of empty array with no initial value")
			}
		}
		for ; i >= 0; i-- {
			present, e := rt.hasElemE(obj, i) // reduceRight skips holes
			if e != nil {
				return mkundef(), e
			}
			if !present {
				continue
			}
			el, e := rt.getElement(obj, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			if acc, e = rt.callValue(cb, mkundef(), []Value{acc, el, mknum(float64(i)), obj}); e != nil {
				return mkundef(), e
			}
		}
		return acc, nil
	})

	rt.defMethod(proto, "sort", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cmp := arg(args, 0)
		if !cmp.IsUndefined() && !rt.isCallable(cmp) {
			return mkundef(), rt.typeError("The comparison function must be either a function or undefined")
		}
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		n, e := rt.lengthOf(obj)
		if e != nil {
			return mkundef(), e
		}
		// SortIndexedProperties (skip-holes): collect the present elements via Get,
		// sort, write them back with Set, then Delete the trailing holes. This runs
		// entirely through the ordinary property protocol (getters/setters, the
		// prototype chain, Proxy traps) unlike an in-place dense sort.
		items := make([]Value, 0, n)
		for k := 0; k < n; k++ {
			present, e := rt.hasElemE(obj, k)
			if e != nil {
				return mkundef(), e
			}
			if present {
				el, e := rt.getElement(obj, mknum(float64(k)))
				if e != nil {
					return mkundef(), e
				}
				items = append(items, el)
			}
		}
		itemCount := len(items)
		var sortErr *ThrowError
		sort.SliceStable(items, func(i, j int) bool {
			if sortErr != nil {
				return false
			}
			c, e := rt.sortCompare(items[i], items[j], cmp)
			if e != nil {
				sortErr = e
				return false
			}
			return c < 0
		})
		if sortErr != nil {
			return mkundef(), sortErr
		}
		for j := 0; j < itemCount; j++ {
			if e := rt.setElement(obj, mknum(float64(j)), items[j]); e != nil {
				return mkundef(), e
			}
		}
		for j := itemCount; j < n; j++ {
			ok, e := rt.deleteElement(obj, mknum(float64(j)))
			if e != nil {
				return mkundef(), e
			}
			if !ok {
				return mkundef(), rt.typeError("Cannot delete array index during sort")
			}
		}
		return obj, nil
	})

	// Array constructor.
	ctor := rt.newNativeFunc("Array", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res := rt.newArray()
		ro := rt.objPtr(res)
		ro.proto = rt.newTargetProto(rt.arrayProto) // honor new.target (subclassing)
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
	// Array.isTemplateObject: a template-strings object is a frozen array with a
	// frozen own `raw` array (as produced for a tagged template). A plain or
	// user-frozen array without such a `raw` is not one.
	rt.defMethod(cobj, "isTemplateObject", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		if v.Type() != TArr {
			return mkbool(false), nil
		}
		o := rt.objPtr(v)
		if o == nil || o.flags.extensible {
			return mkbool(false), nil
		}
		raw, ok := o.getOwn("raw")
		if !ok || raw.Type() != TArr {
			return mkbool(false), nil
		}
		ro := rt.objPtr(raw)
		return mkbool(ro != nil && !ro.flags.extensible), nil
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
		mapEach := func(it Value, i int) (Value, *ThrowError) {
			if rt.isCallable(mapFn) {
				return rt.callValue(mapFn, arg(args, 2), []Value{it, mknum(float64(i))})
			}
			return it, nil
		}
		// usingIterator = GetMethod(items, @@iterator) — an observable [[Get]] that
		// precedes the array-like fallback (a Proxy's get trap sees @@iterator
		// first). When it is callable, the iterable path takes precedence and the
		// mapper runs per element so a throw closes the iterator.
		var usingIterator Value = mkundef()
		if !src.IsNullish() && rt.symIterator != 0 {
			m, e := rt.getElement(src, rt.symIterator)
			if e != nil {
				return mkundef(), e
			}
			usingIterator = m
		}
		if rt.isCallable(usingIterator) {
			res, e := rt.arrayFromCtor(this, 0)
			if e != nil {
				return mkundef(), e
			}
			it, e := rt.callValue(usingIterator, src, nil)
			if e != nil {
				return mkundef(), e
			}
			if !it.IsObjectType() {
				return mkundef(), rt.typeError("[Symbol.iterator]() returned a non-object")
			}
			i := 0
			if e := rt.iterateIteratorWithClose(it, func(el Value) (bool, *ThrowError) {
				v, e := mapEach(el, i)
				if e != nil {
					return false, e
				}
				if e := rt.createDataProperty(res, mknum(float64(i)), v); e != nil {
					return false, e
				}
				i++
				return false, nil
			}); e != nil {
				return mkundef(), e
			}
			rt.setField(res, "length", mknum(float64(i)))
			return res, nil
		}
		if src.IsNullish() {
			return mkundef(), rt.typeError("Array.from requires an array-like or iterable object")
		}
		n, e := rt.lengthOf(src)
		if e != nil {
			return mkundef(), e
		}
		res, e := rt.arrayFromCtor(this, n)
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < n; i++ {
			el, _ := rt.getElement(src, mknum(float64(i)))
			v, e := mapEach(el, i)
			if e != nil {
				return mkundef(), e
			}
			if e := rt.createDataProperty(res, mknum(float64(i)), v); e != nil {
				return mkundef(), e
			}
		}
		rt.setField(res, "length", mknum(float64(n)))
		return res, nil
	})
	rt.defMethod(cobj, "fromAsync", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.arrayFromAsync(this, arg(args, 0), arg(args, 1), arg(args, 2)), nil
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
// flattenIntoArray implements FlattenIntoArray(target, source, sourceLen, start,
// depth[, mapper, thisArg]) and returns the next target index. It is generic
// (HasProperty/Get on array-likes, CreateDataPropertyOrThrow into target) and a
// large depth (flat(Infinity)) is passed as a saturating int (real arrays can't
// nest 2^31 deep). depth <= 0 stops flattening.
func (rt *Runtime) flattenIntoArray(target, source Value, sourceLen, start, depth int, mapper, thisArg Value) (int, *ThrowError) {
	targetIndex := start
	for i := 0; i < sourceLen; i++ {
		present, e := rt.hasElemE(source, i)
		if e != nil {
			return 0, e
		}
		if !present {
			continue
		}
		el, e := rt.getElement(source, mknum(float64(i)))
		if e != nil {
			return 0, e
		}
		if !mapper.IsUndefined() {
			if el, e = rt.callValue(mapper, thisArg, []Value{el, mknum(float64(i)), source}); e != nil {
				return 0, e
			}
		}
		if depth > 0 && rt.isArrayValue(el) {
			elLen, e := rt.lengthOf(el)
			if e != nil {
				return 0, e
			}
			newDepth := depth - 1
			if depth >= 0x7FFFFFFF { // saturating +Infinity
				newDepth = depth
			}
			if targetIndex, e = rt.flattenIntoArray(target, el, elLen, targetIndex, newDepth, mkundef(), mkundef()); e != nil {
				return 0, e
			}
		} else {
			if float64(targetIndex) >= 9007199254740991 {
				return 0, rt.typeError("Array flatten result exceeds the maximum array length")
			}
			if e := rt.createDataProperty(target, mknum(float64(targetIndex)), el); e != nil {
				return 0, e
			}
			targetIndex++
		}
	}
	return targetIndex, nil
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
