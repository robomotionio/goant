package engine

import "math"

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
		no.abuf = make([]byte, end-start)
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
		rt.objPtr(v).abuf = make([]byte, n)
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

	// intView returns the TypedArray receiver and coerced index.
	intView := func(args []Value) (*object, int, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		if o == nil || o.ta == nil {
			return nil, 0, rt.typeError("Atomics operation requires an integer TypedArray")
		}
		return o, int(argNum(rt, args, 1)), nil
	}

	rmw := func(name string, op func(old, v float64) float64) {
		rt.defMethod(ao, name, 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, i, e := intView(args)
			if e != nil {
				return mkundef(), e
			}
			old, _ := rt.taGet(o, i)
			rt.taSet(o, i, op(old.Number(), argNum(rt, args, 2)))
			return old, nil
		})
	}
	rmw("add", func(a, b float64) float64 { return a + b })
	rmw("sub", func(a, b float64) float64 { return a - b })
	rmw("and", func(a, b float64) float64 { return float64(int64(a) & int64(b)) })
	rmw("or", func(a, b float64) float64 { return float64(int64(a) | int64(b)) })
	rmw("xor", func(a, b float64) float64 { return float64(int64(a) ^ int64(b)) })
	rmw("exchange", func(a, b float64) float64 { return b })

	rt.defMethod(ao, "load", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, i, e := intView(args)
		if e != nil {
			return mkundef(), e
		}
		v, _ := rt.taGet(o, i)
		return v, nil
	})
	rt.defMethod(ao, "store", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, i, e := intView(args)
		if e != nil {
			return mkundef(), e
		}
		v := argNum(rt, args, 2)
		rt.taSet(o, i, v)
		return mknum(v), nil
	})
	rt.defMethod(ao, "compareExchange", 4, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, i, e := intView(args)
		if e != nil {
			return mkundef(), e
		}
		old, _ := rt.taGet(o, i)
		if old.Number() == argNum(rt, args, 2) {
			rt.taSet(o, i, argNum(rt, args, 3))
		}
		return old, nil
	})
	rt.defMethod(ao, "isLockFree", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n := int(argNum(rt, args, 0))
		return mkbool(n == 1 || n == 2 || n == 4 || n == 8), nil
	})
	// Single-threaded: a wait never blocks (the value can't change under it).
	rt.defMethod(ao, "wait", 4, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, i, e := intView(args)
		if e != nil {
			return mkundef(), e
		}
		cur, _ := rt.taGet(o, i)
		if cur.Number() != argNum(rt, args, 2) {
			return rt.newString("not-equal"), nil
		}
		return rt.newString("timed-out"), nil
	})
	rt.defMethod(ao, "notify", 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
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
