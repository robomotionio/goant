package engine

import (
	"math"
	"math/big"
)

// SharedArrayBuffer + Atomics (ant modules/atomics.c). goant is single-threaded,
// so a SharedArrayBuffer is an ordinary byte buffer and the Atomics operations
// are plain (non-interleaved) read-modify-writes over an integer TypedArray view.

func (rt *Runtime) initSharedArrayBuffer() {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)
	po.defineAccessor("byteLength", rt.newNativeFunc("byteLength", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if o := rt.objPtr(this); o != nil {
			return mknum(float64(len(o.abuf))), nil
		}
		return mknum(0), nil
	}), mkundef(), true, false, attrConfigurable)
	rt.defMethod(po, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.abuf == nil {
			return mkundef(), rt.typeError("SharedArrayBuffer.prototype.slice on incompatible receiver")
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
		rt.chargeBytes(uint64(end - start))
		copy(no.abuf, o.abuf[start:end])
		return nb, nil
	})
	rt.setStringTag(proto, "SharedArrayBuffer")

	ctor := rt.newNativeFunc("SharedArrayBuffer", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n := 0
		if a := arg(args, 0); a.IsNumber() {
			n = int(a.Number())
		}
		v := rt.newObject(proto)
		vo := rt.objPtr(v)
		vo.abObj, vo.abShared = true, true
		vo.abuf = make([]byte, n)
		rt.chargeBytes(uint64(n))
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
