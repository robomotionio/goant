package engine

import (
	"math"
	"math/big"
)

// SharedArrayBuffer + Atomics (ant modules/atomics.c). goant is single-threaded,
// so a SharedArrayBuffer is an ordinary byte buffer and the Atomics operations
// are plain (non-interleaved) read-modify-writes over an integer TypedArray view.

// sharedBuf brands the receiver of a SharedArrayBuffer.prototype member. An
// ordinary ArrayBuffer is refused here for the same reason a shared one is
// refused over there: the two are different types that happen to hold bytes.
func (rt *Runtime) sharedBuf(this Value, member string) (*object, *ThrowError) {
	o := rt.objPtr(this)
	if o == nil || !o.abObj || !o.abShared {
		return nil, rt.typeError("SharedArrayBuffer.prototype." + member + " on incompatible receiver")
	}
	return o, nil
}

func (rt *Runtime) initSharedArrayBuffer() {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)
	po.defineAccessor("byteLength", rt.newNativeFunc("get byteLength", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.sharedBuf(this, "byteLength")
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(len(o.abuf))), nil
	}), mkundef(), true, false, attrConfigurable)
	// A growable SharedArrayBuffer only ever grows. There is no shrinking and no
	// detaching, because another agent may be looking at the bytes and has no way
	// to be told they went away.
	po.defineAccessor("growable", rt.newNativeFunc("get growable", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.sharedBuf(this, "growable")
		if e != nil {
			return mkundef(), e
		}
		return mkbool(o.abResizable), nil
	}), mkundef(), true, false, attrConfigurable)
	po.defineAccessor("maxByteLength", rt.newNativeFunc("get maxByteLength", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.sharedBuf(this, "maxByteLength")
		if e != nil {
			return mkundef(), e
		}
		if o.abResizable {
			return mknum(float64(o.abMax)), nil
		}
		return mknum(float64(len(o.abuf))), nil
	}), mkundef(), true, false, attrConfigurable)
	rt.defMethod(po, "grow", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.sharedBuf(this, "grow")
		if e != nil {
			return mkundef(), e
		}
		if !o.abResizable {
			return mkundef(), rt.typeError("SharedArrayBuffer.prototype.grow on a non-growable buffer")
		}
		n, e := rt.toIndex(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		if n > o.abMax || n < len(o.abuf) {
			return mkundef(), rt.rangeError("SharedArrayBuffer.prototype.grow: invalid length")
		}
		// Grown in place where the capacity allows, so that a view another agent
		// already holds keeps pointing at the same bytes.
		if n <= cap(o.abuf) {
			o.abuf = o.abuf[:n]
			return mkundef(), nil
		}
		nb := make([]byte, n)
		copy(nb, o.abuf)
		o.abuf = nb
		rt.chargeBytes(uint64(n))
		return mkundef(), nil
	})
	rt.defMethod(po, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.sharedBuf(this, "slice")
		if e != nil {
			return mkundef(), e
		}
		n := len(o.abuf)
		start := rt.relativeIndex(arg(args, 0), n)
		end := n
		if !arg(args, 1).IsUndefined() {
			end = rt.relativeIndex(arg(args, 1), n)
		}
		if end < start {
			end = start
		}
		nb := rt.newObject(proto)
		no := rt.objPtr(nb)
		no.abObj, no.abShared = true, true
		no.abuf = make([]byte, end-start)
		no.abMax = end - start
		rt.chargeBytes(uint64(end - start))
		copy(no.abuf, o.abuf[start:end])
		return nb, nil
	})
	rt.setStringTag(proto, "SharedArrayBuffer")

	ctor := rt.newNativeFunc("SharedArrayBuffer", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !rt.constructing() {
			return mkundef(), rt.typeError("Constructor SharedArrayBuffer requires 'new'")
		}
		n, e := rt.toIndex(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		maxLen := -1
		if opts := arg(args, 1); opts.IsObjectType() {
			mv, e := rt.getField(opts, "maxByteLength")
			if e != nil {
				return mkundef(), e
			}
			if !mv.IsUndefined() {
				if maxLen, e = rt.toIndex(mv); e != nil {
					return mkundef(), e
				}
			}
		}
		// Compared before the object exists, like ArrayBuffer's.
		if maxLen >= 0 && n > maxLen {
			return mkundef(), rt.rangeError("SharedArrayBuffer length exceeds maxByteLength")
		}
		// The prototype is resolved -- and a throwing new.target.prototype getter
		// observed -- before any bytes are asked for, so a bad prototype beats the
		// allocation RangeError.
		ntProto, e := rt.newTargetProtoE(proto)
		if e != nil {
			return mkundef(), e
		}
		// A length Go cannot allocate is a RangeError, not a panic: makeslice
		// takes an int, and every one of these tests handed it a number that does
		// not fit one.
		limit := n
		if maxLen >= 0 {
			limit = maxLen
		}
		if limit > maxByteLen {
			return mkundef(), rt.rangeError("SharedArrayBuffer allocation failed: length too large")
		}
		v := rt.newObject(proto)
		vo := rt.objPtr(v)
		vo.abObj, vo.abShared = true, true
		if maxLen >= 0 {
			// Reserved to its maximum up front. Growing must not move the bytes:
			// another agent's view is a window onto them and cannot be told.
			vo.abuf = make([]byte, n, maxLen)
			vo.abMax, vo.abResizable = maxLen, true
		} else {
			vo.abuf = make([]byte, n)
			vo.abMax = n
		}
		rt.chargeBytes(uint64(n))
		if ntProto.IsObjectType() {
			vo.proto = ntProto
		}
		return v, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defSpeciesGetter(ctor)
	rt.defGlobal("SharedArrayBuffer", ctor)
}

func (rt *Runtime) initAtomics() {
	atomics := rt.newObject(rt.objectProto)
	ao := rt.objPtr(atomics)

	rmw := func(name string, small func(a, b int64) int64, large func(a, b *big.Int) *big.Int) {
		rt.defMethod(ao, name, 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return rt.atomicsRMW(args, small, large)
		})
	}
	bigOp := func(f func(z, a, b *big.Int) *big.Int) func(a, b *big.Int) *big.Int {
		return func(a, b *big.Int) *big.Int { return f(new(big.Int), a, b) }
	}
	rmw("add", func(a, b int64) int64 { return a + b }, bigOp((*big.Int).Add))
	rmw("sub", func(a, b int64) int64 { return a - b }, bigOp((*big.Int).Sub))
	rmw("and", func(a, b int64) int64 { return a & b }, bigOp((*big.Int).And))
	rmw("or", func(a, b int64) int64 { return a | b }, bigOp((*big.Int).Or))
	rmw("xor", func(a, b int64) int64 { return a ^ b }, bigOp((*big.Int).Xor))
	rmw("exchange", func(a, b int64) int64 { return b }, func(a, b *big.Int) *big.Int { return b })

	rt.defMethod(ao, "load", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.atomicsView(arg(args, 0), false)
		if e != nil {
			return mkundef(), e
		}
		i, e := rt.atomicsIndex(o, arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		v, _ := rt.taGet(o, i)
		return v, nil
	})
	rt.defMethod(ao, "store", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.atomicsView(arg(args, 0), false)
		if e != nil {
			return mkundef(), e
		}
		i, e := rt.atomicsIndex(o, arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		// A store answers with the value it was GIVEN, coerced but not narrowed:
		// storing 2**32 into an Int32Array stores zero and returns 4294967296.
		if isBigIntKind(o.ta.kind) {
			b, e := rt.toBigInt(arg(args, 2))
			if e != nil {
				return mkundef(), e
			}
			if de := rt.atomicsRecheck(o, i); de != nil {
				return mkundef(), de
			}
			rt.taSetBig(o, i, b)
			return rt.newBigInt(b), nil
		}
		f, e := rt.toIntegerOrInfinity(arg(args, 2))
		if e != nil {
			return mkundef(), e
		}
		if de := rt.atomicsRecheck(o, i); de != nil {
			return mkundef(), de
		}
		rt.taSet(o, i, f)
		if f == 0 {
			f = 0 // normalise -0, which a store reports as +0
		}
		return mknum(f), nil
	})
	rt.defMethod(ao, "compareExchange", 4, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.atomicsView(arg(args, 0), false)
		if e != nil {
			return mkundef(), e
		}
		i, e := rt.atomicsIndex(o, arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		expN, expB, e := rt.atomicsOperand(o, arg(args, 2))
		if e != nil {
			return mkundef(), e
		}
		repN, repB, e := rt.atomicsOperand(o, arg(args, 3))
		if e != nil {
			return mkundef(), e
		}
		if de := rt.atomicsRecheck(o, i); de != nil {
			return mkundef(), de
		}
		if isBigIntKind(o.ta.kind) {
			old := atomicsReadBig(rt, o, i)
			if old.Cmp(atomicsWrapBig(o.ta.kind, expB)) == 0 {
				rt.taSetBig(o, i, repB)
			}
			return rt.newBigInt(old), nil
		}
		old := atomicsReadInt(rt, o, i)
		// The expected value is compared as the element type reads it, so that
		// 2**32 and 0 are the same expectation for an Int32Array.
		if old == atomicsNarrow(o.ta.kind, expN) {
			rt.taSet(o, i, float64(repN))
		}
		return mknum(float64(old)), nil
	})
	rt.defMethod(ao, "isLockFree", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		f, e := rt.toIntegerOrInfinity(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		n := int(f)
		return mkbool(float64(n) == f && (n == 1 || n == 2 || n == 4 || n == 8)), nil
	})
	// wait / notify. There is one agent here, so nothing can change the cell
	// while this one is stopped looking at it: a wait that would block has
	// already waited as long as it ever will, and a notify wakes nobody.
	rt.defMethod(ao, "wait", 4, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res, e := rt.atomicsWaitResult(args)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(res), nil
	})
	rt.defMethod(ao, "waitAsync", 4, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		res, e := rt.atomicsWaitResult(args)
		if e != nil {
			return mkundef(), e
		}
		out := rt.newPlainObject()
		oo := rt.objPtr(out)
		oo.defineOwn("async", mkbool(false), attrDefault)
		oo.defineOwn("value", rt.newString(res), attrDefault)
		return out, nil
	})
	rt.defMethod(ao, "notify", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.atomicsView(arg(args, 0), true)
		if e != nil {
			return mkundef(), e
		}
		i, e := rt.atomicsIndex(o, arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		if _, e := rt.toIntegerOrInfinity(arg(args, 2)); e != nil {
			return mkundef(), e
		}
		_ = i
		return mknum(0), nil
	})
	// Atomics.pause([iterationNumber]): a spin-loop hint. iterationNumber, if
	// present, must be an integral Number; the operation itself is a no-op here
	// (goant is single-threaded) and returns undefined.
	rt.defMethod(ao, "pause", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if n := arg(args, 0); !n.IsUndefined() {
			v := n.Number()
			if !n.IsNumber() || math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
				return mkundef(), rt.typeError("Atomics.pause: iterationNumber must be an integral Number")
			}
		}
		return mkundef(), nil
	})
	rt.setStringTag(atomics, "Atomics")
	rt.defGlobal("Atomics", atomics)
}
