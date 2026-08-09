package engine

// The validation every Atomics operation does before it touches memory, and the
// arithmetic it does once it has.
//
// Two checks, in this order and no other: the array has to be one whose elements
// an atomic operation is defined on, and the index has to be inside it. The
// order is observable, because both throw and they throw different things -- a
// bad array is a TypeError before the index is even coerced, and a bad index is
// a RangeError after it.
//
// The value comes last, after both, and coercing it can run arbitrary code
// through valueOf -- which may detach the buffer under us. So the array is
// checked again afterwards.

import "math/big"

// atomicsAccess is ValidateTypedArray's accessMode. It exists for one case: an
// immutable buffer refuses a write, and it refuses it here -- before the index
// and before the operand -- so neither one's valueOf runs.
type atomicsAccess bool

const (
	atomicsRead  atomicsAccess = false // load, wait, notify
	atomicsWrite atomicsAccess = true  // store, compareExchange, every RMW
)

// atomicsView is ValidateIntegerTypedArray. `waitable` narrows it to the two
// element types an agent can wait on: waiting is defined in terms of a lock
// held on one 32- or 64-bit word, and nothing else is one.
func (rt *Runtime) atomicsView(v Value, waitable bool, mode atomicsAccess) (*object, *ThrowError) {
	o := rt.objPtr(v)
	if o == nil || o.ta == nil {
		return nil, rt.typeError("Atomics operation requires an integer TypedArray")
	}
	if rt.taDetached(o) {
		return nil, rt.typeError("Atomics operation on a detached ArrayBuffer")
	}
	if mode == atomicsWrite && rt.abIsImmutable(rt.objPtr(o.ta.buf)) {
		return nil, rt.typeError("Cannot modify an immutable ArrayBuffer")
	}
	switch k := o.ta.kind; {
	case waitable && (k == taInt32 || k == taBigInt64):
		return o, nil
	case waitable:
		return nil, rt.typeError("Atomics.wait requires an Int32Array or a BigInt64Array")
	case k == taInt8 || k == taUint8 || k == taInt16 || k == taUint16 ||
		k == taInt32 || k == taUint32 || k == taBigInt64 || k == taBigUint64:
		return o, nil
	}
	return nil, rt.typeError("Atomics operation requires an integer TypedArray")
}

// atomicsIndex is ValidateAtomicAccess: an index outside the array is a
// RangeError, not a silent undefined the way an ordinary element read would be.
//
// The length is read BEFORE the index is coerced, and the order is the whole
// point: coercing can run a valueOf, a valueOf can grow the buffer, and the
// bound the index is judged against is the one that held when the operation
// started. A length-tracking view of an empty growable SharedArrayBuffer whose
// index.valueOf grows it to four still has no element 0.
func (rt *Runtime) atomicsIndex(o *object, v Value) (int, *ThrowError) {
	length := rt.taCurrentLen(o)
	i, e := rt.toIndex(v)
	if e != nil {
		return 0, e
	}
	if i >= length {
		return 0, rt.rangeError("Atomics index is out of range")
	}
	return i, nil
}

// atomicsOperand coerces the value an operation is given, the way the element
// type requires: a BigInt array takes BigInts and nothing else, and every other
// integer array takes a number that is then truncated into range.
func (rt *Runtime) atomicsOperand(o *object, v Value) (int64, *big.Int, *ThrowError) {
	if isBigIntKind(o.ta.kind) {
		b, e := rt.toBigInt(v)
		if e != nil {
			return 0, nil, e
		}
		return 0, b, nil
	}
	f, e := rt.toIntegerOrInfinity(v)
	if e != nil {
		return 0, nil, e
	}
	return int64(toUint32(f)), nil, nil
}

// atomicsRead is the element as an exact integer, whatever its width.
func atomicsReadInt(rt *Runtime, o *object, i int) int64 {
	v, _ := rt.taGet(o, i)
	return int64(v.Number())
}

func atomicsReadBig(rt *Runtime, o *object, i int) *big.Int {
	v, _ := rt.taGet(o, i)
	return rt.bigIntVal(v)
}

// atomicsRMW runs one read-modify-write. The two halves of the signature are the
// two families: everything up to 32 bits is exact in an int64, and the 64-bit
// element types need big.Int because they are not.
func (rt *Runtime) atomicsRMW(args []Value, small func(old, v int64) int64,
	large func(old, v *big.Int) *big.Int) (Value, *ThrowError) {
	o, e := rt.atomicsView(arg(args, 0), false, atomicsWrite)
	if e != nil {
		return mkundef(), e
	}
	i, e := rt.atomicsIndex(o, arg(args, 1))
	if e != nil {
		return mkundef(), e
	}
	n, b, e := rt.atomicsOperand(o, arg(args, 2))
	if e != nil {
		return mkundef(), e
	}
	// Coercing the operand may have detached the buffer or shrunk it.
	if rt.taDetached(o) {
		return mkundef(), rt.typeError("Atomics operation on a detached ArrayBuffer")
	}
	if i >= rt.taCurrentLen(o) {
		return mkundef(), rt.rangeError("Atomics index is out of range")
	}
	if isBigIntKind(o.ta.kind) {
		old := atomicsReadBig(rt, o, i)
		rt.taSetBig(o, i, large(old, b))
		return rt.newBigInt(atomicsWrapBig(o.ta.kind, old)), nil
	}
	old := atomicsReadInt(rt, o, i)
	rt.taSet(o, i, float64(small(old, n)))
	return mknum(float64(old)), nil
}

// atomicsWrapBig brings a 64-bit result back into its element type's range, so
// that what an operation returns is what a read of the same cell would give.
func atomicsWrapBig(k taKind, v *big.Int) *big.Int {
	if k == taBigUint64 {
		return bigIntAsUintN(64, v)
	}
	return bigIntAsIntN(64, v)
}

// atomicsRecheck is the second look at the array, after coercing an operand ran
// whatever a valueOf wanted to run. The buffer may be gone, or shorter.
func (rt *Runtime) atomicsRecheck(o *object, i int) *ThrowError {
	if rt.taDetached(o) {
		return rt.typeError("Atomics operation on a detached ArrayBuffer")
	}
	if i >= rt.taCurrentLen(o) {
		return rt.rangeError("Atomics index is out of range")
	}
	return nil
}

// atomicsNarrow reads an integer the way an element of this kind would, so that
// a comparison against a stored value asks the same question the storage does:
// 2**32 and 0 are one expectation for an Int32Array.
func atomicsNarrow(k taKind, n int64) int64 {
	switch k {
	case taInt8:
		return int64(int8(n))
	case taUint8:
		return int64(uint8(n))
	case taInt16:
		return int64(int16(n))
	case taUint16:
		return int64(uint16(n))
	case taInt32:
		return int64(int32(n))
	case taUint32:
		return int64(uint32(n))
	}
	return n
}

// atomicsWaitResult is the shared front half of wait and waitAsync: everything
// they must validate, and the answer that follows when there is nobody else to
// change the cell. A wait on a buffer no one else can see is not a wait at all,
// which is why it is a TypeError rather than a very long pause.
func (rt *Runtime) atomicsWaitResult(args []Value) (string, *ThrowError) {
	o, e := rt.atomicsView(arg(args, 0), true, atomicsRead)
	if e != nil {
		return "", e
	}
	if b := o.ta.bufPtr; b == nil || !b.abShared {
		return "", rt.typeError("Atomics.wait requires a SharedArrayBuffer")
	}
	i, e := rt.atomicsIndex(o, arg(args, 1))
	if e != nil {
		return "", e
	}
	var matches bool
	if isBigIntKind(o.ta.kind) {
		want, e := rt.toBigInt(arg(args, 2))
		if e != nil {
			return "", e
		}
		matches = atomicsReadBig(rt, o, i).Cmp(atomicsWrapBig(o.ta.kind, want)) == 0
	} else {
		f, e := rt.toIntegerOrInfinity(arg(args, 2))
		if e != nil {
			return "", e
		}
		matches = atomicsReadInt(rt, o, i) == atomicsNarrow(o.ta.kind, int64(toUint32(f)))
	}
	// The timeout is coerced whatever the comparison said: it is observable.
	if _, e := rt.toNumber(arg(args, 3)); e != nil {
		return "", e
	}
	if !matches {
		return "not-equal", nil
	}
	return "timed-out", nil
}
