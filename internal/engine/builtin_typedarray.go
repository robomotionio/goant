package engine

// ArrayBuffer / TypedArray / DataView (ant modules/typedarray.c). ArrayBuffer
// owns a flat byte store; a TypedArray is a typed window over it (element get/set
// route through getElement/setElement so the ordinary index protocol and all the
// %TypedArray%.prototype iteration methods operate on the buffer with the view's
// element coercion). DataView provides explicit typed, endianness-aware access.

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/big"
	"sort"
)

type taKind int

const (
	taInt8 taKind = iota
	taUint8
	taUint8Clamped
	taInt16
	taUint16
	taInt32
	taUint32
	taFloat16
	taFloat32
	taFloat64
	taBigInt64
	taBigUint64
)

// isBigIntKind reports whether a typed-array kind holds BigInt elements.
func isBigIntKind(k taKind) bool { return k == taBigInt64 || k == taBigUint64 }

type taInfo struct {
	name string
	size int
}

var taKinds = []taInfo{
	{"Int8Array", 1}, {"Uint8Array", 1}, {"Uint8ClampedArray", 1},
	{"Int16Array", 2}, {"Uint16Array", 2},
	{"Int32Array", 4}, {"Uint32Array", 4},
	{"Float16Array", 2},
	{"Float32Array", 4}, {"Float64Array", 8},
	{"BigInt64Array", 8}, {"BigUint64Array", 8},
}

type typedArray struct {
	buf        Value // backing ArrayBuffer value (for .buffer); bytes are read
	kind       taKind
	byteOffset int
	length     int  // fixed element length (ignored when track is set)
	track      bool // length-tracking view over a resizable buffer
}

type dataView struct {
	buf        Value
	byteOffset int
	byteLength int  // fixed byte length (ignored when track is set)
	track      bool // length-tracking view over a resizable buffer
}

func (t *typedArray) size() int { return taKinds[t.kind].size }

// decodeElem reads one element of kind from b at off (little-endian).
func decodeElem(b []byte, off int, kind taKind) float64 {
	switch kind {
	case taInt8:
		return float64(int8(b[off]))
	case taUint8, taUint8Clamped:
		return float64(b[off])
	case taInt16:
		return float64(int16(binary.LittleEndian.Uint16(b[off:])))
	case taUint16:
		return float64(binary.LittleEndian.Uint16(b[off:]))
	case taInt32:
		return float64(int32(binary.LittleEndian.Uint32(b[off:])))
	case taUint32:
		return float64(binary.LittleEndian.Uint32(b[off:]))
	case taFloat16:
		return float16ToFloat64(binary.LittleEndian.Uint16(b[off:]))
	case taFloat32:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off:])))
	case taFloat64:
		return math.Float64frombits(binary.LittleEndian.Uint64(b[off:]))
	}
	return 0
}

// encodeElem writes v as kind into b at off (little-endian, with the kind's
// integer wraparound / clamping).
func encodeElem(b []byte, off int, kind taKind, v float64) {
	switch kind {
	case taInt8, taUint8:
		b[off] = byte(int64(toIntWrap(v)))
	case taUint8Clamped:
		b[off] = clampUint8(v)
	case taInt16, taUint16:
		binary.LittleEndian.PutUint16(b[off:], uint16(int64(toIntWrap(v))))
	case taInt32, taUint32:
		binary.LittleEndian.PutUint32(b[off:], uint32(int64(toIntWrap(v))))
	case taFloat16:
		binary.LittleEndian.PutUint16(b[off:], float16FromFloat64(v))
	case taFloat32:
		binary.LittleEndian.PutUint32(b[off:], math.Float32bits(float32(v)))
	case taFloat64:
		binary.LittleEndian.PutUint64(b[off:], math.Float64bits(v))
	}
}

// toIntWrap implements ToInt-style truncation used by integer TypedArray writes.
func toIntWrap(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int64(math.Trunc(v))
}

func clampUint8(v float64) byte {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	// round half to even
	r := math.RoundToEven(v)
	return byte(r)
}

// float16FromFloat64 converts f to an IEEE-754 binary16 (half-precision) bit
// pattern, rounding to nearest with ties to even (the rounding mode the spec's
// SetValueInBuffer/ToFloat16 requires). The conversion is done directly from
// the binary64 representation to avoid the double-rounding a float32 detour
// would introduce.
func float16FromFloat64(f float64) uint16 {
	b := math.Float64bits(f)
	sign := uint16((b >> 48) & 0x8000)
	e := int((b >> 52) & 0x7FF)
	m := b & 0xFFFFFFFFFFFFF // 52-bit trailing significand

	if e == 0x7FF { // Inf / NaN
		if m == 0 {
			return sign | 0x7C00
		}
		h := uint16(m >> 42) // keep the leading NaN payload bits
		if h == 0 {
			h = 1
		}
		return sign | 0x7C00 | h
	}
	if e == 0 { // binary64 zero or subnormal → underflows to half zero
		return sign
	}

	exp := e - 1023  // unbiased binary64 exponent
	he := exp + 15   // biased half exponent for the normal case
	if he >= 31 {    // overflow → infinity
		return sign | 0x7C00
	}

	sig := m | (1 << 52) // 53-bit significand with the implicit leading 1

	if he <= 0 {
		// Subnormal half or underflow. value = sig * 2^(exp-52); express in
		// units of the subnormal quantum 2^-24 by shifting right (28-exp).
		shift := 28 - exp
		if shift > 63 {
			return sign
		}
		q := sig >> shift
		rem := sig & ((uint64(1) << shift) - 1)
		half := uint64(1) << (shift - 1)
		if rem > half || (rem == half && q&1 == 1) {
			q++ // ties to even; a carry into bit 10 yields the smallest normal
		}
		return sign | uint16(q)
	}

	// Normal case: keep 10 mantissa bits, round away the low 42.
	q := sig >> 42
	rem := sig & ((uint64(1) << 42) - 1)
	half := uint64(1) << 41
	if rem > half || (rem == half && q&1 == 1) {
		q++
	}
	mant := q - 0x400 // strip the implicit leading 1 (q ∈ [0x400, 0x800])
	if mant >= 0x400 {
		he++ // mantissa carried out → exponent increment, mantissa 0
		mant = 0
	}
	if he >= 31 {
		return sign | 0x7C00
	}
	return sign | uint16(he)<<10 | uint16(mant)
}

// float16ToFloat64 decodes an IEEE-754 binary16 bit pattern to float64.
func float16ToFloat64(h uint16) float64 {
	sign := 1.0
	if h&0x8000 != 0 {
		sign = -1.0
	}
	exp := int((h >> 10) & 0x1F)
	mant := int(h & 0x3FF)
	switch exp {
	case 0:
		return sign * math.Ldexp(float64(mant), -24)
	case 0x1F:
		if mant == 0 {
			return sign * math.Inf(1)
		}
		return math.NaN()
	default:
		return sign * math.Ldexp(float64(1024+mant), exp-25)
	}
}

// taBytes returns the current backing byte store of a TypedArray's buffer (nil
// if detached). Views never cache the slice: a resizable buffer's resize()
// re-slices abuf in place, so reading it fresh keeps length-tracking correct.
func (rt *Runtime) taBytes(t *typedArray) []byte {
	if b := rt.objPtr(t.buf); b != nil {
		return b.abuf
	}
	return nil
}

// taDetached reports whether o is a TypedArray whose backing ArrayBuffer has
// been detached (its bytes transferred away).
func (rt *Runtime) taDetached(o *object) bool {
	if o == nil || o.ta == nil {
		return false
	}
	b := rt.objPtr(o.ta.buf)
	return b == nil || b.abuf == nil
}

// taOutOfBounds implements IsTypedArrayOutOfBounds(O): true if the buffer is
// detached, or a resizable buffer has shrunk so O's window no longer fits.
// An out-of-bounds view behaves like a detached one (length 0, no valid
// indices, methods throw).
func (rt *Runtime) taOutOfBounds(o *object) bool {
	t := o.ta
	if t == nil {
		return true
	}
	b := rt.objPtr(t.buf)
	if b == nil || b.abuf == nil {
		return true
	}
	bufLen := len(b.abuf)
	if t.byteOffset > bufLen {
		return true
	}
	if !t.track && t.byteOffset+t.length*t.size() > bufLen {
		return true
	}
	return false
}

// taCurrentLen implements TypedArrayLength(O): the effective element count now
// (0 if out of bounds; recomputed from the buffer for length-tracking views).
func (rt *Runtime) taCurrentLen(o *object) int {
	t := o.ta
	if t == nil || rt.taOutOfBounds(o) {
		return 0
	}
	if t.track {
		b := rt.objPtr(t.buf)
		return (len(b.abuf) - t.byteOffset) / t.size()
	}
	return t.length
}

// validateTypedArray implements ValidateTypedArray(O): O must be a TypedArray
// whose buffer is attached and in-bounds, else a TypeError. Used to guard the
// non-generic %TypedArray%.prototype methods.
func (rt *Runtime) validateTypedArray(this Value) *ThrowError {
	o := rt.objPtr(this)
	if o == nil || o.ta == nil {
		return rt.typeError("TypedArray.prototype method called on incompatible receiver")
	}
	if rt.taOutOfBounds(o) {
		return rt.typeError("Cannot perform TypedArray operation on a detached or out-of-bounds ArrayBuffer")
	}
	return nil
}

// taValidIndex implements IsValidIntegerIndex(O, i): i addresses a live element
// of typed array o (in bounds, buffer attached and not out of bounds).
func (rt *Runtime) taValidIndex(o *object, i int) bool {
	return o != nil && o.ta != nil && i >= 0 && i < rt.taCurrentLen(o)
}

// taDefineIndex implements the integer-indexed exotic [[DefineOwnProperty]] for
// element index idx: elements are writable/enumerable/configurable data
// properties, so any descriptor demanding an accessor, non-writable,
// non-enumerable, or non-configurable element — or targeting an invalid index —
// is rejected (a non-nil error, which Object.defineProperty throws and
// Reflect.defineProperty reports as false).
func (rt *Runtime) taDefineIndex(o *object, idx int, descVal Value) *ThrowError {
	if !rt.taValidIndex(o, idx) {
		return rt.typeError("Cannot define property: invalid typed array index")
	}
	field := func(k string) (Value, bool) {
		if rt.hasProp(descVal, k) {
			v, _ := rt.getField(descVal, k)
			return v, true
		}
		return mkundef(), false
	}
	if v, ok := field("configurable"); ok && !rt.toBoolean(v) {
		return rt.typeError("Cannot redefine typed array element as non-configurable")
	}
	if v, ok := field("enumerable"); ok && !rt.toBoolean(v) {
		return rt.typeError("Cannot redefine typed array element as non-enumerable")
	}
	if _, ok := field("get"); ok {
		return rt.typeError("Cannot redefine typed array element as an accessor")
	}
	if _, ok := field("set"); ok {
		return rt.typeError("Cannot redefine typed array element as an accessor")
	}
	if v, ok := field("writable"); ok && !rt.toBoolean(v) {
		return rt.typeError("Cannot redefine typed array element as non-writable")
	}
	if v, ok := field("value"); ok {
		if isBigIntKind(o.ta.kind) {
			bi, e := rt.toBigInt(v)
			if e != nil {
				return e
			}
			rt.taSetBig(o, idx, bi)
		} else {
			n, e := rt.toNumber(v)
			if e != nil {
				return e
			}
			rt.taSet(o, idx, n)
		}
	}
	return nil
}

// taGet/taSet are the element accessors used by getElement/setElement. A valid
// integer index requires an attached buffer (IsValidIntegerIndex).
func (rt *Runtime) taGet(o *object, i int) (Value, bool) {
	t := o.ta
	if t == nil || i < 0 || i >= rt.taCurrentLen(o) {
		return mkundef(), false
	}
	b := rt.taBytes(t)
	off := t.byteOffset + i*t.size()
	if isBigIntKind(t.kind) {
		u := binary.LittleEndian.Uint64(b[off:])
		if t.kind == taBigInt64 {
			return rt.newBigInt(big.NewInt(int64(u))), true
		}
		return rt.newBigInt(new(big.Int).SetUint64(u)), true
	}
	return mknum(decodeElem(b, off, t.kind)), true
}

func (rt *Runtime) taSet(o *object, i int, v float64) bool {
	t := o.ta
	if t == nil || i < 0 || i >= rt.taCurrentLen(o) {
		return false
	}
	encodeElem(rt.taBytes(t), t.byteOffset+i*t.size(), t.kind, v)
	return true
}

// taSetBig writes a BigInt element (BigInt64Array / BigUint64Array) as its
// low-64-bit two's-complement pattern.
func (rt *Runtime) taSetBig(o *object, i int, v *big.Int) bool {
	t := o.ta
	if t == nil || i < 0 || i >= rt.taCurrentLen(o) {
		return false
	}
	binary.LittleEndian.PutUint64(rt.taBytes(t)[t.byteOffset+i*t.size():], bigIntAsUintN(64, v).Uint64())
	return true
}

// maxByteLen caps ArrayBuffer/TypedArray allocations: a larger request throws a
// RangeError (CreateByteDataBlock "impossible to create") instead of letting the
// underlying make() panic on an unsatisfiable size.
const maxByteLen = 0x7FFF_FFFF

func (rt *Runtime) newArrayBuffer(byteLen int) Value {
	v := rt.newObject(rt.arrayBufferProto)
	o := rt.objPtr(v)
	if byteLen < 0 {
		byteLen = 0
	}
	o.abuf = make([]byte, byteLen)
	o.abMax = byteLen
	o.abObj = true
	return v
}

// newResizableArrayBuffer creates an ArrayBuffer of byteLen bytes that can grow
// or shrink up to maxLen. resize() reallocates and copies (the spec's default
// HostResizeArrayBuffer); views read the buffer's current bytes on each access,
// so they follow the reallocation.
func (rt *Runtime) newResizableArrayBuffer(byteLen, maxLen int) Value {
	v := rt.newObject(rt.arrayBufferProto)
	o := rt.objPtr(v)
	o.abuf = make([]byte, byteLen)
	o.abMax = maxLen
	o.abResizable = true
	o.abObj = true
	return v
}

// newTypedArray builds a view of `kind` from a constructor argument.
func (rt *Runtime) newTypedArray(kind taKind, args []Value) (Value, *ThrowError) {
	h, o := rt.objects.alloc()
	// Honor a subclass new.target's prototype (falls back to the intrinsic when
	// not constructing, e.g. internal map/filter/toReversed allocations).
	o.proto = rt.newTargetProto(rt.typedArrayProtos[kind])
	o.shape = newShape()
	o.typeTag = TTypedArray
	o.flags.extensible = true
	tv := mkval(TTypedArray, uint64(h))

	size := taKinds[kind].size
	a0 := arg(args, 0)
	switch {
	case a0.IsNumber() || a0.IsUndefined():
		n, e := rt.toIndex(a0)
		if e != nil {
			return mkundef(), e
		}
		if n > maxByteLen/size {
			return mkundef(), rt.rangeError("Invalid typed array length")
		}
		buf := rt.newArrayBuffer(n * size)
		o.ta = &typedArray{buf: buf, kind: kind, length: n}
	case rt.isArrayBufferValue(a0):
		// InitializeTypedArrayFromArrayBuffer.
		bo := rt.objPtr(a0)
		offset, e := rt.toIndex(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		if offset%size != 0 {
			return mkundef(), rt.rangeError("Start offset is not a multiple of the element size")
		}
		lenArg := arg(args, 2)
		var newLen int
		if !lenArg.IsUndefined() {
			if newLen, e = rt.toIndex(lenArg); e != nil {
				return mkundef(), e
			}
		}
		if bo.abuf == nil {
			return mkundef(), rt.typeError("Cannot construct a TypedArray over a detached ArrayBuffer")
		}
		bufLen := len(bo.abuf)
		ta := &typedArray{buf: a0, kind: kind, byteOffset: offset}
		switch {
		case lenArg.IsUndefined() && bo.abResizable:
			if offset > bufLen {
				return mkundef(), rt.rangeError("Start offset is outside the bounds of the buffer")
			}
			ta.track = true
		case lenArg.IsUndefined():
			if bufLen%size != 0 {
				return mkundef(), rt.rangeError("Byte length of buffer is not a multiple of the element size")
			}
			if offset > bufLen {
				return mkundef(), rt.rangeError("Start offset is outside the bounds of the buffer")
			}
			ta.length = (bufLen - offset) / size
		default:
			if offset+newLen*size > bufLen {
				return mkundef(), rt.rangeError("Invalid typed array length")
			}
			ta.length = newLen
		}
		o.ta = ta
	default:
		// Iterable or array-like source: copy element-wise.
		var items []Value
		if rt.isIterable(a0) {
			it, e := rt.iterableValues(a0)
			if e != nil {
				return mkundef(), e
			}
			items = it
		} else if src := rt.objPtr(a0); src != nil {
			n, _ := rt.lengthOf(a0)
			for i := 0; i < n; i++ {
				el, _ := rt.getElement(a0, mknum(float64(i)))
				items = append(items, el)
			}
		}
		buf := rt.newArrayBuffer(len(items) * size)
		o.ta = &typedArray{buf: buf, kind: kind, length: len(items)}
		if isBigIntKind(kind) {
			for i, it := range items {
				bi, e := rt.toBigInt(it)
				if e != nil {
					return mkundef(), e
				}
				rt.taSetBig(o, i, bi)
			}
		} else {
			for i, it := range items {
				n, e := rt.toNumber(it)
				if e != nil {
					return mkundef(), e
				}
				rt.taSet(o, i, n)
			}
		}
	}
	return tv, nil
}

func (rt *Runtime) isArrayBufferValue(v Value) bool {
	o := rt.objPtr(v)
	return o != nil && o.abuf != nil && o.ta == nil && o.dv == nil
}

func (rt *Runtime) initTypedArrays() {
	// %TypedArray%.prototype shared by all element kinds.
	taProto := rt.newObject(rt.objectProto)
	rt.typedArrayProto = taProto
	tp := rt.objPtr(taProto)
	rt.defineTypedArrayMethods(tp)
	// %TypedArray%.prototype[@@toStringTag] is a getter returning the element-kind
	// name (e.g. "Int8Array"), or undefined for a non-typed-array receiver.
	if rt.symToStringTag != 0 {
		g := rt.newNativeFunc("get [Symbol.toStringTag]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil || o.ta == nil {
				return mkundef(), nil
			}
			return rt.internString(taKinds[o.ta.kind].name), nil
		})
		tp.defineAccessorSymbol(rt.symToStringTag.handle(), g, mkundef(), true, false, attrConfigurable)
	}

	// %TypedArray% — the abstract constructor each per-kind constructor inherits
	// from (23.2.1); it can't be called directly.
	taCtor := rt.newNativeFunc("TypedArray", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkundef(), rt.typeError("Abstract class TypedArray not directly constructable")
	})
	rt.objPtr(taCtor).defineOwn("prototype", taProto, 0)
	tp.defineOwn("constructor", taCtor, attrWritable|attrConfigurable)

	rt.typedArrayProtos = make([]Value, len(taKinds))
	rt.typedArrayCtors = make([]Value, len(taKinds))
	for k := range taKinds {
		kind := taKind(k)
		proto := rt.newObject(taProto)
		rt.typedArrayProtos[k] = proto
		info := taKinds[k]
		ctor := rt.newNativeFunc(info.name, 3, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if !rt.constructing() {
				return mkundef(), rt.typeError("Constructor " + info.name + " requires 'new'")
			}
			return rt.newTypedArray(kind, args)
		})
		rt.typedArrayCtors[k] = ctor
		cobj := rt.objPtr(ctor)
		cobj.proto = taCtor // Int8Array.__proto__ === %TypedArray%
		cobj.defineOwn("prototype", proto, 0)
		cobj.defineOwn("BYTES_PER_ELEMENT", mknum(float64(info.size)), 0)
		rt.objPtr(proto).defineOwn("constructor", ctor, attrWritable|attrConfigurable)
		rt.objPtr(proto).defineOwn("BYTES_PER_ELEMENT", mknum(float64(info.size)), 0)
		// from / of statics.
		rt.defMethod(cobj, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			// `this` is the constructor to build the result with (TypedArrayCreate).
			if !rt.isConstructorValue(this) {
				return mkundef(), rt.typeError("TypedArray.from called on a non-constructor")
			}
			mapFn := arg(args, 1)
			if !mapFn.IsUndefined() && !rt.isCallable(mapFn) {
				return mkundef(), rt.typeError("TypedArray.from mapfn is not callable")
			}
			thisArg := arg(args, 2)
			src := arg(args, 0)
			var items []Value
			if rt.isIterable(src) {
				it, e := rt.iterableValues(src)
				if e != nil {
					return mkundef(), e
				}
				items = it
			} else {
				n, e := rt.lengthOf(src)
				if e != nil {
					return mkundef(), e
				}
				items = make([]Value, n)
				for i := 0; i < n; i++ {
					if items[i], e = rt.getElement(src, mknum(float64(i))); e != nil {
						return mkundef(), e
					}
				}
			}
			arrV, e := rt.typedArrayCreate(this, len(items))
			if e != nil {
				return mkundef(), e
			}
			for i, it := range items {
				v := it
				if rt.isCallable(mapFn) {
					if v, e = rt.callValue(mapFn, thisArg, []Value{it, mknum(float64(i))}); e != nil {
						return mkundef(), e
					}
				}
				if e := rt.setElement(arrV, mknum(float64(i)), v); e != nil {
					return mkundef(), e
				}
			}
			return arrV, nil
		})
		rt.defMethod(cobj, "of", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			arrV, e := rt.typedArrayCreate(this, len(args))
			if e != nil {
				return mkundef(), e
			}
			for i, it := range args {
				if e := rt.setElement(arrV, mknum(float64(i)), it); e != nil {
					return mkundef(), e
				}
			}
			return arrV, nil
		})
		if info.name == "Uint8Array" {
			rt.defUint8ArrayBase64Hex(cobj, rt.objPtr(proto), kind)
		}
		rt.defSpeciesGetter(ctor) // <Kind>[Symbol.species] getter (returns this)
		rt.defGlobal(info.name, ctor)
	}
	rt.defSpeciesGetter(taCtor) // %TypedArray%[Symbol.species]

	rt.initArrayBufferBuiltin()
	rt.initDataViewBuiltin()
}

// defUint8ArrayBase64Hex installs the Uint8Array base64/hex conversions
// (toBase64/fromBase64/setFromBase64, toHex/fromHex/setFromHex — TC39 stage 4).
func (rt *Runtime) defUint8ArrayBase64Hex(cobj, proto *object, kind taKind) {
	readBytes := func(this Value) []byte {
		o := rt.objPtr(this)
		n := rt.taLength(o)
		b := make([]byte, n)
		for i := 0; i < n; i++ {
			v, _ := rt.taGet(o, i)
			b[i] = byte(uint8(v.Number()))
		}
		return b
	}
	newFrom := func(b []byte) Value {
		arrV, _ := rt.newTypedArray(kind, []Value{mknum(float64(len(b)))})
		ao := rt.objPtr(arrV)
		for i, by := range b {
			rt.taSet(ao, i, float64(by))
		}
		return arrV
	}
	// setInto writes decoded bytes into `this`, returning {read, written} where
	// written is capped at the receiver's length.
	setInto := func(this Value, decoded []byte, srcLen int) Value {
		o := rt.objPtr(this)
		n := rt.taLength(o)
		written := len(decoded)
		if written > n {
			written = n
		}
		for i := 0; i < written; i++ {
			rt.taSet(o, i, float64(decoded[i]))
		}
		res := rt.newObject(rt.objectProto)
		ro := rt.objPtr(res)
		ro.defineOwn("read", mknum(float64(srcLen)), attrDefault)
		ro.defineOwn("written", mknum(float64(written)), attrDefault)
		return res
	}

	rt.defMethod(proto, "toHex", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newString(hex.EncodeToString(readBytes(this))), nil
	})
	rt.defMethod(proto, "toBase64", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newString(base64.StdEncoding.EncodeToString(readBytes(this))), nil
	})
	rt.defMethod(cobj, "fromHex", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s := string(rt.strBytes(arg(args, 0)))
		b, err := hex.DecodeString(s)
		if err != nil {
			return mkundef(), rt.syntaxError("Invalid hex string")
		}
		return newFrom(b), nil
	})
	rt.defMethod(cobj, "fromBase64", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, err := base64.StdEncoding.DecodeString(string(rt.strBytes(arg(args, 0))))
		if err != nil {
			return mkundef(), rt.syntaxError("Invalid base64 string")
		}
		return newFrom(b), nil
	})
	rt.defMethod(proto, "setFromHex", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s := string(rt.strBytes(arg(args, 0)))
		b, err := hex.DecodeString(s)
		if err != nil {
			return mkundef(), rt.syntaxError("Invalid hex string")
		}
		return setInto(this, b, len(s)), nil
	})
	rt.defMethod(proto, "setFromBase64", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s := string(rt.strBytes(arg(args, 0)))
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return mkundef(), rt.syntaxError("Invalid base64 string")
		}
		return setInto(this, b, len(s)), nil
	})
}

func (rt *Runtime) initArrayBufferBuiltin() {
	proto := rt.newObject(rt.objectProto)
	rt.arrayBufferProto = proto
	po := rt.objPtr(proto)
	po.defineAccessor("byteLength", rt.newNativeFunc("get byteLength", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || !o.abObj {
			return mkundef(), rt.typeError("ArrayBuffer.prototype.byteLength getter called on a non-ArrayBuffer")
		}
		return mknum(float64(len(o.abuf))), nil // 0 when detached
	}), mkundef(), true, false, attrConfigurable)
	// detached: an ArrayBuffer whose bytes have been transferred away (abuf nil).
	po.defineAccessor("detached", rt.newNativeFunc("get detached", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || !o.abObj {
			return mkundef(), rt.typeError("ArrayBuffer.prototype.detached getter called on a non-ArrayBuffer")
		}
		return mkbool(o.abuf == nil), nil
	}), mkundef(), true, false, attrConfigurable)
	// ArrayBufferCopyAndDetach: copy into a new buffer and detach the source.
	// transfer preserves resizability (same maxByteLength); transferToFixedLength
	// always yields a non-resizable buffer.
	transfer := func(preserveResizability bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil || (o.ta != nil || o.dv != nil) {
				return mkundef(), rt.typeError("ArrayBuffer.prototype.transfer on incompatible receiver")
			}
			newLen := len(o.abuf)
			if a := arg(args, 0); !a.IsUndefined() {
				n, e := rt.toIndex(a)
				if e != nil {
					return mkundef(), e
				}
				newLen = n
			}
			if o.abuf == nil {
				return mkundef(), rt.typeError("Cannot transfer a detached ArrayBuffer")
			}
			var nb Value
			if preserveResizability && o.abResizable {
				max := o.abMax
				if newLen > max {
					return mkundef(), rt.rangeError("Transfer length exceeds maxByteLength")
				}
				nb = rt.newResizableArrayBuffer(newLen, max)
			} else {
				nb = rt.newArrayBuffer(newLen)
			}
			copy(rt.objPtr(nb).abuf, o.abuf)
			o.abuf = nil // detach the source
			return nb, nil
		}
	}
	rt.defMethod(po, "transfer", 0, transfer(true))
	rt.defMethod(po, "transferToFixedLength", 0, transfer(false))
	rt.defMethod(po, "resize", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.ta != nil || o.dv != nil || !o.abResizable {
			return mkundef(), rt.typeError("ArrayBuffer.prototype.resize called on a non-resizable ArrayBuffer")
		}
		n, e := rt.toIndex(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		if o.abuf == nil {
			return mkundef(), rt.typeError("Cannot resize a detached ArrayBuffer")
		}
		if n > o.abMax {
			return mkundef(), rt.rangeError("ArrayBuffer.prototype.resize length exceeds maxByteLength")
		}
		// Reallocate and copy min(old, new) bytes; grown bytes start zeroed, and
		// shrink-then-grow reveals zeros — matching the spec's default resize.
		nb := make([]byte, n)
		copy(nb, o.abuf)
		o.abuf = nb
		return mkundef(), nil
	})
	rt.defMethod(po, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.ta != nil || o.dv != nil {
			return mkundef(), rt.typeError("ArrayBuffer.prototype.slice on incompatible receiver")
		}
		if o.abuf == nil {
			return mkundef(), rt.typeError("Cannot slice a detached ArrayBuffer")
		}
		n := len(o.abuf)
		// ToIntegerOrInfinity + clamp to [0, n]; undefined end defaults to n.
		clamp := func(v Value, def int) (int, *ThrowError) {
			if v.IsUndefined() {
				return def, nil
			}
			d, e := rt.toIntegerOrInfinity(v)
			if e != nil {
				return 0, e
			}
			switch {
			case math.IsInf(d, -1):
				return 0, nil
			case math.IsInf(d, 1):
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
		first, e := clamp(arg(args, 0), 0)
		if e != nil {
			return mkundef(), e
		}
		final, e := clamp(arg(args, 1), n)
		if e != nil {
			return mkundef(), e
		}
		newLen := final - first
		if newLen < 0 {
			newLen = 0
		}
		// SpeciesConstructor(O, %ArrayBuffer%) builds the result.
		def, _ := rt.getField(rt.global, "ArrayBuffer")
		ctor, e := rt.speciesConstructor(this, def)
		if e != nil {
			return mkundef(), e
		}
		nbV, e := rt.construct(ctor, []Value{mknum(float64(newLen))})
		if e != nil {
			return mkundef(), e
		}
		nb := rt.objPtr(nbV)
		if !rt.isArrayBufferValue(nbV) { // covers non-ArrayBuffer and detached results
			return mkundef(), rt.typeError("ArrayBuffer species constructor did not return an ArrayBuffer")
		}
		if nbV == this {
			return mkundef(), rt.typeError("ArrayBuffer species constructor returned the same ArrayBuffer")
		}
		if len(nb.abuf) < newLen {
			return mkundef(), rt.typeError("ArrayBuffer species constructor returned too small a buffer")
		}
		if o.abuf == nil { // the species constructor may have detached the source
			return mkundef(), rt.typeError("The source ArrayBuffer was detached during slice")
		}
		// first+newLen (not final): when start exceeds end, newLen is 0 and final
		// may be < first, which would make o.abuf[first:final] panic.
		copy(nb.abuf, o.abuf[first:first+newLen])
		return nbV, nil
	})
	po.defineAccessor("maxByteLength", rt.newNativeFunc("get maxByteLength", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.ta != nil || o.dv != nil {
			return mkundef(), rt.typeError("ArrayBuffer.prototype.maxByteLength on incompatible receiver")
		}
		if o.abuf == nil {
			return mknum(0), nil // detached
		}
		return mknum(float64(o.abMax)), nil
	}), mkundef(), true, false, attrConfigurable)
	po.defineAccessor("resizable", rt.newNativeFunc("get resizable", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.ta != nil || o.dv != nil {
			return mkundef(), rt.typeError("ArrayBuffer.prototype.resizable on incompatible receiver")
		}
		return mkbool(o.abResizable), nil
	}), mkundef(), true, false, attrConfigurable)
	rt.setStringTag(proto, "ArrayBuffer")

	ctor := rt.newNativeFunc("ArrayBuffer", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !rt.constructing() {
			return mkundef(), rt.typeError("Constructor ArrayBuffer requires 'new'")
		}
		n, e := rt.toIndex(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		// GetArrayBufferMaxByteLengthOption: an options object with a defined
		// maxByteLength makes the buffer resizable.
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
		var buf Value
		if maxLen < 0 {
			if n > maxByteLen {
				return mkundef(), rt.rangeError("ArrayBuffer allocation failed: length too large")
			}
			buf = rt.newArrayBuffer(n)
		} else {
			if n > maxLen {
				return mkundef(), rt.rangeError("ArrayBuffer length exceeds maxByteLength")
			}
			if maxLen > maxByteLen {
				return mkundef(), rt.rangeError("ArrayBuffer allocation failed: maxByteLength too large")
			}
			buf = rt.newResizableArrayBuffer(n, maxLen)
		}
		// Honor a subclass new.target's prototype.
		if p := rt.newTargetProto(rt.arrayBufferProto); p.IsObjectType() {
			rt.objPtr(buf).proto = p
		}
		return buf, nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", proto, 0)
	rt.defMethod(cobj, "isView", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(arg(args, 0))
		return mkbool(o != nil && (o.ta != nil || o.dv != nil)), nil
	})
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defSpeciesGetter(ctor)
	rt.defGlobal("ArrayBuffer", ctor)
}

// dvDetached reports whether DataView o's backing ArrayBuffer has been detached.
func (rt *Runtime) dvDetached(o *object) bool {
	if o == nil || o.dv == nil {
		return false
	}
	b := rt.objPtr(o.dv.buf)
	return b == nil || b.abuf == nil
}

// dvBytes returns the DataView's current backing byte store (nil if detached).
func (rt *Runtime) dvBytes(o *object) []byte {
	if b := rt.objPtr(o.dv.buf); b != nil {
		return b.abuf
	}
	return nil
}

// dvOutOfBounds implements IsViewOutOfBounds: detached, or a resizable buffer
// shrank so the view's window no longer fits.
func (rt *Runtime) dvOutOfBounds(o *object) bool {
	d := o.dv
	b := rt.objPtr(d.buf)
	if b == nil || b.abuf == nil {
		return true
	}
	bufLen := len(b.abuf)
	if d.byteOffset > bufLen {
		return true
	}
	if !d.track && d.byteOffset+d.byteLength > bufLen {
		return true
	}
	return false
}

// dvCurrentLen implements GetViewByteLength: the effective byte length now
// (recomputed from the buffer for a length-tracking view).
func (rt *Runtime) dvCurrentLen(o *object) int {
	d := o.dv
	if d.track {
		b := rt.objPtr(d.buf)
		return len(b.abuf) - d.byteOffset
	}
	return d.byteLength
}

func (rt *Runtime) initDataViewBuiltin() {
	proto := rt.newObject(rt.objectProto)
	rt.dataViewProto = proto
	po := rt.objPtr(proto)

	type dvType struct {
		name string
		size int
		enc  func(b []byte, off int, v float64, le bool)
		dec  func(b []byte, off int, le bool) float64
	}
	order := func(le bool) binary.ByteOrder {
		if le {
			return binary.LittleEndian
		}
		return binary.BigEndian
	}
	types := []dvType{
		{"Int8", 1, func(b []byte, o int, v float64, le bool) { b[o] = byte(int64(toIntWrap(v))) }, func(b []byte, o int, le bool) float64 { return float64(int8(b[o])) }},
		{"Uint8", 1, func(b []byte, o int, v float64, le bool) { b[o] = byte(int64(toIntWrap(v))) }, func(b []byte, o int, le bool) float64 { return float64(b[o]) }},
		{"Int16", 2, func(b []byte, o int, v float64, le bool) { order(le).PutUint16(b[o:], uint16(int64(toIntWrap(v)))) }, func(b []byte, o int, le bool) float64 { return float64(int16(order(le).Uint16(b[o:]))) }},
		{"Uint16", 2, func(b []byte, o int, v float64, le bool) { order(le).PutUint16(b[o:], uint16(int64(toIntWrap(v)))) }, func(b []byte, o int, le bool) float64 { return float64(order(le).Uint16(b[o:])) }},
		{"Int32", 4, func(b []byte, o int, v float64, le bool) { order(le).PutUint32(b[o:], uint32(int64(toIntWrap(v)))) }, func(b []byte, o int, le bool) float64 { return float64(int32(order(le).Uint32(b[o:]))) }},
		{"Uint32", 4, func(b []byte, o int, v float64, le bool) { order(le).PutUint32(b[o:], uint32(int64(toIntWrap(v)))) }, func(b []byte, o int, le bool) float64 { return float64(order(le).Uint32(b[o:])) }},
		{"Float16", 2, func(b []byte, o int, v float64, le bool) { order(le).PutUint16(b[o:], float16FromFloat64(v)) }, func(b []byte, o int, le bool) float64 { return float16ToFloat64(order(le).Uint16(b[o:])) }},
		{"Float32", 4, func(b []byte, o int, v float64, le bool) { order(le).PutUint32(b[o:], math.Float32bits(float32(v))) }, func(b []byte, o int, le bool) float64 { return float64(math.Float32frombits(order(le).Uint32(b[o:]))) }},
		{"Float64", 8, func(b []byte, o int, v float64, le bool) { order(le).PutUint64(b[o:], math.Float64bits(v)) }, func(b []byte, o int, le bool) float64 { return math.Float64frombits(order(le).Uint64(b[o:])) }},
	}
	for _, t := range types {
		t := t
		rt.defMethod(po, "get"+t.name, 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil || o.dv == nil {
				return mkundef(), rt.typeError("DataView.prototype.get" + t.name + " on incompatible receiver")
			}
			idx, e := rt.toIndex(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			le := rt.toBoolean(arg(args, 1))
			if rt.dvOutOfBounds(o) {
				return mkundef(), rt.typeError("Cannot get value from a detached ArrayBuffer")
			}
			if idx+t.size > rt.dvCurrentLen(o) {
				return mkundef(), rt.rangeError("Offset is outside the bounds of the DataView")
			}
			return mknum(t.dec(rt.dvBytes(o), o.dv.byteOffset+idx, le)), nil
		})
		rt.defMethod(po, "set"+t.name, 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil || o.dv == nil {
				return mkundef(), rt.typeError("DataView.prototype.set" + t.name + " on incompatible receiver")
			}
			idx, e := rt.toIndex(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			val, e := rt.toNumber(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			le := rt.toBoolean(arg(args, 2))
			if rt.dvOutOfBounds(o) {
				return mkundef(), rt.typeError("Cannot set value on a detached ArrayBuffer")
			}
			if idx+t.size > rt.dvCurrentLen(o) {
				return mkundef(), rt.rangeError("Offset is outside the bounds of the DataView")
			}
			t.enc(rt.dvBytes(o), o.dv.byteOffset+idx, val, le)
			return mkundef(), nil
		})
	}
	// 64-bit BigInt views (BigInt-valued, so separate from the float64 loop).
	for _, bt := range []struct {
		name   string
		signed bool
	}{{"BigInt64", true}, {"BigUint64", false}} {
		bt := bt
		rt.defMethod(po, "get"+bt.name, 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil || o.dv == nil {
				return mkundef(), rt.typeError("DataView.prototype.get" + bt.name + " on incompatible receiver")
			}
			idx, e := rt.toIndex(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			le := rt.toBoolean(arg(args, 1))
			if rt.dvOutOfBounds(o) {
				return mkundef(), rt.typeError("Cannot get value from a detached ArrayBuffer")
			}
			if idx+8 > rt.dvCurrentLen(o) {
				return mkundef(), rt.rangeError("Offset is outside the bounds of the DataView")
			}
			off := o.dv.byteOffset + idx
			u := order(le).Uint64(rt.dvBytes(o)[off:])
			if bt.signed {
				return rt.newBigInt(big.NewInt(int64(u))), nil
			}
			return rt.newBigInt(new(big.Int).SetUint64(u)), nil
		})
		rt.defMethod(po, "set"+bt.name, 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil || o.dv == nil {
				return mkundef(), rt.typeError("DataView.prototype.set" + bt.name + " on incompatible receiver")
			}
			idx, e := rt.toIndex(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			bi, e := rt.toBigInt(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			le := rt.toBoolean(arg(args, 2))
			if rt.dvOutOfBounds(o) {
				return mkundef(), rt.typeError("Cannot set value on a detached ArrayBuffer")
			}
			if idx+8 > rt.dvCurrentLen(o) {
				return mkundef(), rt.rangeError("Offset is outside the bounds of the DataView")
			}
			order(le).PutUint64(rt.dvBytes(o)[o.dv.byteOffset+idx:], bigIntAsUintN(64, bi).Uint64())
			return mkundef(), nil
		})
	}
	po.defineAccessor("byteLength", rt.newNativeFunc("get byteLength", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.dv == nil {
			return mkundef(), rt.typeError("DataView.prototype.byteLength on incompatible receiver")
		}
		if rt.dvOutOfBounds(o) {
			return mkundef(), rt.typeError("Cannot read byteLength of a detached ArrayBuffer")
		}
		return mknum(float64(rt.dvCurrentLen(o))), nil
	}), mkundef(), true, false, attrConfigurable)
	po.defineAccessor("byteOffset", rt.newNativeFunc("get byteOffset", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.dv == nil {
			return mkundef(), rt.typeError("DataView.prototype.byteOffset on incompatible receiver")
		}
		if rt.dvOutOfBounds(o) {
			return mkundef(), rt.typeError("Cannot read byteOffset of a detached ArrayBuffer")
		}
		return mknum(float64(o.dv.byteOffset)), nil
	}), mkundef(), true, false, attrConfigurable)
	po.defineAccessor("buffer", rt.newNativeFunc("get buffer", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.dv == nil {
			return mkundef(), rt.typeError("DataView.prototype.buffer on incompatible receiver")
		}
		return o.dv.buf, nil
	}), mkundef(), true, false, attrConfigurable)
	rt.setStringTag(proto, "DataView")

	ctor := rt.newNativeFunc("DataView", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !rt.constructing() {
			return mkundef(), rt.typeError("Constructor DataView requires 'new'")
		}
		bufV := arg(args, 0)
		bo := rt.objPtr(bufV)
		// RequireInternalSlot([[ArrayBufferData]]): reject non-ArrayBuffers (and
		// buffers already detached, which have no usable storage).
		if bo == nil || bo.abuf == nil {
			return mkundef(), rt.typeError("First argument to DataView constructor must be an ArrayBuffer")
		}
		// ToIndex(byteOffset) may run user code (valueOf) that detaches the buffer.
		offset, e := rt.toIndex(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		if bo.abuf == nil {
			return mkundef(), rt.typeError("Cannot construct DataView on a detached ArrayBuffer")
		}
		bufLen := len(bo.abuf)
		if offset > bufLen {
			return mkundef(), rt.rangeError("Start offset is outside the bounds of the buffer")
		}
		// An omitted length over a resizable buffer makes a length-tracking view.
		lv := arg(args, 2)
		track := lv.IsUndefined() && bo.abResizable
		viewLen := bufLen - offset
		if !lv.IsUndefined() {
			viewLen, e = rt.toIndex(lv)
			if e != nil {
				return mkundef(), e
			}
			if bo.abuf == nil {
				return mkundef(), rt.typeError("Cannot construct DataView on a detached ArrayBuffer")
			}
			if offset+viewLen > bufLen {
				return mkundef(), rt.rangeError("Invalid DataView length")
			}
		}
		// OrdinaryCreateFromConstructor honors a subclass new.target; resolving its
		// prototype can run a user getter that detaches the buffer, re-checked next.
		viewProto := rt.newTargetProto(proto)
		if bo.abuf == nil {
			return mkundef(), rt.typeError("Cannot construct DataView on a detached ArrayBuffer")
		}
		v := rt.newObject(viewProto)
		rt.objPtr(v).dv = &dataView{buf: bufV, byteOffset: offset, byteLength: viewLen, track: track}
		return v, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defGlobal("DataView", ctor)
}

func argNum(rt *Runtime, args []Value, i int) float64 {
	n, _ := rt.toNumber(arg(args, i))
	return n
}

// toIndex implements ToIndex(value): ToIntegerOrInfinity, then a RangeError if
// the result is negative or exceeds 2^53-1. undefined maps to 0.
func (rt *Runtime) toIndex(v Value) (int, *ThrowError) {
	if v.IsUndefined() {
		return 0, nil
	}
	n, e := rt.toNumber(v)
	if e != nil {
		return 0, e
	}
	if math.IsNaN(n) {
		n = 0
	} else {
		n = math.Trunc(n)
	}
	if n < 0 || n > 9007199254740991 {
		return 0, rt.rangeError("Invalid typed array or DataView index")
	}
	return int(n), nil
}

// typedArraySpeciesCreate implements TypedArraySpeciesCreate(exemplar, args):
// construct a new TypedArray using exemplar.constructor[@@species] (defaulting
// to the intrinsic for exemplar's element kind). The result must be a
// non-detached TypedArray, and — for a single numeric length argument — at
// least that long.
func (rt *Runtime) typedArraySpeciesCreate(this Value, args []Value) (Value, *ThrowError) {
	o := rt.objPtr(this)
	if o == nil || o.ta == nil {
		return mkundef(), rt.typeError("not a TypedArray")
	}
	def := rt.typedArrayCtors[o.ta.kind]
	C, e := rt.speciesConstructor(this, def)
	if e != nil {
		return mkundef(), e
	}
	res, e := rt.construct(C, args)
	if e != nil {
		return mkundef(), e
	}
	ro := rt.objPtr(res)
	if ro == nil || ro.ta == nil {
		return mkundef(), rt.typeError("TypedArray species constructor did not return a TypedArray")
	}
	if rt.taDetached(ro) {
		return mkundef(), rt.typeError("TypedArray species constructor returned a detached TypedArray")
	}
	if len(args) == 1 && args[0].IsNumber() && rt.taCurrentLen(ro) < int(args[0].Number()) {
		return mkundef(), rt.typeError("Derived TypedArray constructor created an array shorter than requested")
	}
	return res, nil
}

// typedArrayCreate implements TypedArrayCreateFromConstructor(C, «length»): C
// must be a constructor; the result must be a non-out-of-bounds TypedArray at
// least length elements long. Used by %TypedArray%.from / of, whose `this` is
// the constructor to build the result with.
func (rt *Runtime) typedArrayCreate(C Value, length int) (Value, *ThrowError) {
	if !rt.isConstructorValue(C) {
		return mkundef(), rt.typeError("TypedArray.from/of called on a non-constructor")
	}
	res, e := rt.construct(C, []Value{mknum(float64(length))})
	if e != nil {
		return mkundef(), e
	}
	ro := rt.objPtr(res)
	if ro == nil || ro.ta == nil {
		return mkundef(), rt.typeError("TypedArray constructor did not return a TypedArray")
	}
	if rt.taOutOfBounds(ro) {
		return mkundef(), rt.typeError("TypedArray constructor returned a detached or out-of-bounds TypedArray")
	}
	if rt.taCurrentLen(ro) < length {
		return mkundef(), rt.typeError("Derived TypedArray constructor created an array shorter than requested")
	}
	return res, nil
}

// taStrictEq reports whether a TypedArray element el strictly equals a search
// value (the comparison used by indexOf/lastIndexOf). The search value is not
// coerced, so a Number search never matches a BigInt array element and vice
// versa; NaN never equals NaN and +0 equals -0 (IEEE ==).
func taStrictEq(rt *Runtime, el, search Value, bigKind bool) bool {
	if bigKind {
		return el.Type() == TBigInt && search.Type() == TBigInt && rt.bigIntVal(el).Cmp(rt.bigIntVal(search)) == 0
	}
	return search.IsNumber() && el.Number() == search.Number()
}

// taDefaultCompare is the default TypedArray SortCompare: numbers order with
// NaN last and -0 before +0; BigInts by value.
func taDefaultCompare(rt *Runtime, a, b Value, bigKind bool) int {
	if bigKind {
		return rt.bigIntVal(a).Cmp(rt.bigIntVal(b))
	}
	x, y := a.Number(), b.Number()
	xn, yn := math.IsNaN(x), math.IsNaN(y)
	switch {
	case xn && yn:
		return 0
	case xn:
		return 1
	case yn:
		return -1
	case x < y:
		return -1
	case x > y:
		return 1
	case x == 0 && y == 0:
		switch {
		case math.Signbit(x) && !math.Signbit(y):
			return -1
		case !math.Signbit(x) && math.Signbit(y):
			return 1
		}
	}
	return 0
}

// taLength returns a TypedArray's effective element length (for lengthOf /
// methods): 0 when detached or out of bounds, and recomputed from the buffer
// for length-tracking views.
func (rt *Runtime) taLength(o *object) int {
	if o.ta != nil {
		return rt.taCurrentLen(o)
	}
	return 0
}

// defineTypedArrayMethods installs the shared %TypedArray%.prototype methods.
// They read/write elements through getElement/setElement (typed coercion) and
// use taLength, so they work uniformly across every element kind.
func (rt *Runtime) defineTypedArrayMethods(tp *object) {
	length := func(this Value) int {
		if o := rt.objPtr(this); o != nil {
			return rt.taLength(o)
		}
		return 0
	}
	// These getters require a TypedArray receiver ([[TypedArrayName]] internal
	// slot) — a non-TypedArray this is a TypeError — and their function name is
	// "get <prop>".
	taGetter := func(prop string, fn nativeFunc) Value {
		return rt.newNativeFunc("get "+prop, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(this)
			if o == nil || o.ta == nil {
				return mkundef(), rt.typeError("TypedArray.prototype." + prop + " getter called on a non-TypedArray")
			}
			return fn(rt, this, args)
		})
	}
	tp.defineAccessor("length", taGetter("length", func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mknum(float64(length(this))), nil
	}), mkundef(), true, false, attrConfigurable)
	tp.defineAccessor("byteLength", taGetter("byteLength", func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		return mknum(float64(rt.taCurrentLen(o) * o.ta.size())), nil
	}), mkundef(), true, false, attrConfigurable)
	tp.defineAccessor("byteOffset", taGetter("byteOffset", func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// byteOffset reads as 0 for an out-of-bounds (or detached) view.
		if o := rt.objPtr(this); !rt.taOutOfBounds(o) {
			return mknum(float64(o.ta.byteOffset)), nil
		}
		return mknum(0), nil
	}), mkundef(), true, false, attrConfigurable)
	tp.defineAccessor("buffer", taGetter("buffer", func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.objPtr(this).ta.buf, nil
	}), mkundef(), true, false, attrConfigurable)

	get := func(this Value, i int) Value { v, _ := rt.getElement(this, mknum(float64(i))); return v }

	// Every %TypedArray%.prototype method is non-generic and begins with
	// ValidateTypedArray(this): it throws a TypeError when called on a
	// non-TypedArray receiver or one whose buffer has been detached.
	m := func(name string, n int, fn nativeFunc) {
		rt.defMethod(tp, name, n, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.validateTypedArray(this); e != nil {
				return mkundef(), e
			}
			return fn(rt, this, args)
		})
	}

	m("forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		for i, l := 0, length(this); i < l; i++ {
			if _, e := rt.callValue(cb, arg(args, 1), []Value{get(this, i), mknum(float64(i)), this}); e != nil {
				return mkundef(), e
			}
		}
		return mkundef(), nil
	})
	m("map", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("TypedArray.prototype.map callback is not a function")
		}
		l := length(this)
		out, e := rt.typedArraySpeciesCreate(this, []Value{mknum(float64(l))})
		if e != nil {
			return mkundef(), e
		}
		for i := 0; i < l; i++ {
			r, e := rt.callValue(cb, arg(args, 1), []Value{get(this, i), mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			// setElement coerces to the destination kind (ToNumber / ToBigInt).
			if e := rt.setElement(out, mknum(float64(i)), r); e != nil {
				return mkundef(), e
			}
		}
		return out, nil
	})
	m("filter", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("TypedArray.prototype.filter callback is not a function")
		}
		var keep []Value
		for i, l := 0, length(this); i < l; i++ {
			el := get(this, i)
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				keep = append(keep, el)
			}
		}
		out, e := rt.typedArraySpeciesCreate(this, []Value{mknum(float64(len(keep)))})
		if e != nil {
			return mkundef(), e
		}
		for i, v := range keep {
			if e := rt.setElement(out, mknum(float64(i)), v); e != nil {
				return mkundef(), e
			}
		}
		return out, nil
	})
	reduce := func(this Value, args []Value, right bool) (Value, *ThrowError) {
		cb := arg(args, 0)
		l := length(this)
		acc := arg(args, 1)
		hasAcc := len(args) > 1
		idx := func(k int) int {
			if right {
				return l - 1 - k
			}
			return k
		}
		start := 0
		if !hasAcc {
			if l == 0 {
				return mkundef(), rt.typeError("Reduce of empty array with no initial value")
			}
			acc = get(this, idx(0))
			start = 1
		}
		for k := start; k < l; k++ {
			i := idx(k)
			r, e := rt.callValue(cb, mkundef(), []Value{acc, get(this, i), mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			acc = r
		}
		return acc, nil
	}
	m("reduce", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) { return reduce(this, args, false) })
	m("reduceRight", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) { return reduce(this, args, true) })
	m("every", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		for i, l := 0, length(this); i < l; i++ {
			r, e := rt.callValue(cb, arg(args, 1), []Value{get(this, i), mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if !rt.toBoolean(r) {
				return mkfalse(), nil
			}
		}
		return mktrue(), nil
	})
	m("some", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		for i, l := 0, length(this); i < l; i++ {
			r, e := rt.callValue(cb, arg(args, 1), []Value{get(this, i), mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return mktrue(), nil
			}
		}
		return mkfalse(), nil
	})
	m("find", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		for i, l := 0, length(this); i < l; i++ {
			el := get(this, i)
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
	m("findIndex", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		for i, l := 0, length(this); i < l; i++ {
			r, e := rt.callValue(cb, arg(args, 1), []Value{get(this, i), mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	m("findLast", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		for i := length(this) - 1; i >= 0; i-- {
			el := get(this, i)
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
	m("findLastIndex", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		for i := length(this) - 1; i >= 0; i-- {
			r, e := rt.callValue(cb, arg(args, 1), []Value{get(this, i), mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	// %TypedArray%.prototype.toString IS the same function object as
	// Array.prototype.toString (which calls this.join()).
	if ts, ok := rt.objPtr(rt.arrayProto).getOwn("toString"); ok {
		tp.defineOwn("toString", ts, attrWritable|attrConfigurable)
	}
	m("indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// searchElement is NOT coerced; compare each element by strict equality.
		// len is captured before ToIntegerOrInfinity(fromIndex), whose valueOf may
		// resize/detach; out-of-bounds indices then read as "not present".
		o := rt.objPtr(this)
		search := arg(args, 0)
		l := length(this)
		if l == 0 {
			return mknum(-1), nil
		}
		n, e := rt.toIntegerOrInfinity(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		if math.IsInf(n, 1) {
			return mknum(-1), nil
		}
		k := 0
		if !math.IsInf(n, -1) {
			if k = int(n); k < 0 {
				if k += l; k < 0 {
					k = 0
				}
			}
		}
		bigKind := isBigIntKind(o.ta.kind)
		for i := k; i < l; i++ {
			if el, ok := rt.taGet(o, i); ok && taStrictEq(rt, el, search, bigKind) {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	m("lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		search := arg(args, 0)
		l := length(this)
		if l == 0 {
			return mknum(-1), nil
		}
		k := l - 1
		if len(args) > 1 {
			n, e := rt.toIntegerOrInfinity(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			if math.IsInf(n, -1) {
				return mknum(-1), nil
			}
			if math.IsInf(n, 1) || int(n) >= l {
				k = l - 1
			} else if k = int(n); k < 0 {
				k += l
			}
		}
		bigKind := isBigIntKind(o.ta.kind)
		for i := k; i >= 0; i-- {
			if el, ok := rt.taGet(o, i); ok && taStrictEq(rt, el, search, bigKind) {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	m("includes", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// includes uses SameValueZero and reads out-of-bounds indices as undefined
		// (so includes(undefined) can be true after a mid-coercion detach).
		search := arg(args, 0)
		l := length(this)
		if l == 0 {
			return mkfalse(), nil
		}
		n, e := rt.toIntegerOrInfinity(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		if math.IsInf(n, 1) {
			return mkfalse(), nil
		}
		k := 0
		if !math.IsInf(n, -1) {
			if k = int(n); k < 0 {
				if k += l; k < 0 {
					k = 0
				}
			}
		}
		bigKind := isBigIntKind(rt.objPtr(this).ta.kind)
		for i := k; i < l; i++ {
			el := get(this, i)
			// BigInt equality goes through taStrictEq (SameValueZero on BigInts is
			// plain equality); Numbers use SameValueZero so NaN matches NaN.
			if (bigKind && taStrictEq(rt, el, search, true)) || (!bigKind && rt.sameValueZero(el, search)) {
				return mktrue(), nil
			}
		}
		return mkfalse(), nil
	})
	m("join", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this) // captured before ToString(separator) may resize
		sep := ","
		if s := arg(args, 0); !s.IsUndefined() {
			sv, e := rt.toStringValue(s)
			if e != nil {
				return mkundef(), e
			}
			sep = string(rt.strBytes(sv))
		}
		out := ""
		for i := 0; i < l; i++ {
			if i > 0 {
				out += sep
			}
			// An out-of-bounds index (buffer shrank during coercion) reads as
			// undefined → the empty string.
			if el, ok := rt.taGet(o, i); ok {
				s, e := rt.toStringValue(el)
				if e != nil {
					return mkundef(), e
				}
				out += string(rt.strBytes(s))
			}
		}
		return rt.newString(out), nil
	})
	m("reverse", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		for i := 0; i < l/2; i++ {
			a, _ := rt.taGet(o, i)
			b, _ := rt.taGet(o, l-1-i)
			rt.taSet(o, i, b.Number())
			rt.taSet(o, l-1-i, a.Number())
		}
		return this, nil
	})
	m("fill", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		// Coerce value (BigInt vs Number) then the bounds, propagating aborts.
		var fv float64
		var bv *big.Int
		var e *ThrowError
		if isBigIntKind(o.ta.kind) {
			bv, e = rt.toBigInt(arg(args, 0))
		} else {
			fv, e = rt.toNumber(arg(args, 0))
		}
		if e != nil {
			return mkundef(), e
		}
		start, e := rt.relativeIndexE(arg(args, 1), l)
		if e != nil {
			return mkundef(), e
		}
		end := l
		if !arg(args, 2).IsUndefined() {
			if end, e = rt.relativeIndexE(arg(args, 2), l); e != nil {
				return mkundef(), e
			}
		}
		// Coercion may have detached or resized the buffer.
		if rt.taOutOfBounds(o) {
			return mkundef(), rt.typeError("Cannot fill a detached or out-of-bounds TypedArray")
		}
		if l2 := rt.taCurrentLen(o); end > l2 {
			end = l2
			if start > l2 {
				start = l2
			}
		}
		for i := start; i < end; i++ {
			if bv != nil {
				rt.taSetBig(o, i, bv)
			} else {
				rt.taSet(o, i, fv)
			}
		}
		return this, nil
	})
	m("slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		start, e := rt.relativeIndexE(arg(args, 0), l)
		if e != nil {
			return mkundef(), e
		}
		end := l
		if !arg(args, 1).IsUndefined() {
			if end, e = rt.relativeIndexE(arg(args, 1), l); e != nil {
				return mkundef(), e
			}
		}
		count := end - start
		if count < 0 {
			count = 0
		}
		out, e := rt.typedArraySpeciesCreate(this, []Value{mknum(float64(count))})
		if e != nil {
			return mkundef(), e
		}
		if count > 0 {
			// The species constructor may have detached or resized the source.
			if rt.taOutOfBounds(o) {
				return mkundef(), rt.typeError("Cannot slice a detached or out-of-bounds TypedArray")
			}
			if l2 := rt.taCurrentLen(o); start+count > l2 {
				if count = l2 - start; count < 0 {
					count = 0
				}
			}
			for i := 0; i < count; i++ {
				el, _ := rt.taGet(o, start+i)
				if err := rt.setElement(out, mknum(float64(i)), el); err != nil {
					return mkundef(), err
				}
			}
		}
		return out, nil
	})
	m("at", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		d, e := rt.toNumber(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(d) {
			d = 0
		}
		k := int(math.Trunc(d))
		if k < 0 {
			k += l
		}
		if k < 0 || k >= l {
			return mkundef(), nil
		}
		v, _ := rt.taGet(o, k)
		return v, nil
	})
	// subarray only requires the [[TypedArrayName]] slot (NOT ValidateTypedArray):
	// it works on an out-of-bounds view (srcLength treated as 0) and its begin/end
	// coercions stay observable even on a detached buffer — so it bypasses `m`.
	rt.defMethod(tp, "subarray", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.ta == nil {
			return mkundef(), rt.typeError("TypedArray.prototype.subarray called on incompatible receiver")
		}
		l := length(this) // 0 when out of bounds
		start, e := rt.relativeIndexE(arg(args, 0), l)
		if e != nil {
			return mkundef(), e
		}
		byteOffset := o.ta.byteOffset + start*o.ta.size()
		// SpeciesConstructor is invoked with (buffer, absoluteByteOffset[, newLength]).
		// A length-tracking source with no end argument passes only 2 arguments so
		// the result is itself length-tracking.
		ctorArgs := []Value{o.ta.buf, mknum(float64(byteOffset))}
		if !(o.ta.track && arg(args, 1).IsUndefined()) {
			end := l
			if !arg(args, 1).IsUndefined() {
				if end, e = rt.relativeIndexE(arg(args, 1), l); e != nil {
					return mkundef(), e
				}
			}
			if end < start {
				end = start
			}
			ctorArgs = append(ctorArgs, mknum(float64(end-start)))
		}
		return rt.typedArraySpeciesCreate(this, ctorArgs)
	})
	m("copyWithin", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		to, e := rt.relativeIndexE(arg(args, 0), l)
		if e != nil {
			return mkundef(), e
		}
		from, e := rt.relativeIndexE(arg(args, 1), l)
		if e != nil {
			return mkundef(), e
		}
		final := l
		if !arg(args, 2).IsUndefined() {
			if final, e = rt.relativeIndexE(arg(args, 2), l); e != nil {
				return mkundef(), e
			}
		}
		// Coercion may have detached or resized the buffer; re-validate and clamp.
		if rt.taOutOfBounds(o) {
			return mkundef(), rt.typeError("Cannot copyWithin a detached or out-of-bounds TypedArray")
		}
		if l2 := rt.taCurrentLen(o); l2 < l {
			l = l2
			for _, p := range []*int{&to, &from, &final} {
				if *p > l {
					*p = l
				}
			}
		}
		count := final - from
		if count > l-to {
			count = l - to
		}
		bigKind := isBigIntKind(o.ta.kind)
		tmp := make([]Value, count)
		for i := 0; i < count; i++ {
			tmp[i], _ = rt.taGet(o, from+i)
		}
		for i := 0; i < count; i++ {
			if bigKind {
				bi, _ := rt.toBigInt(tmp[i])
				rt.taSetBig(o, to+i, bi)
			} else {
				rt.taSet(o, to+i, tmp[i].Number())
			}
		}
		return this, nil
	})
	m("set", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		// targetOffset = ToIntegerOrInfinity(offset); a negative offset throws.
		offN, e := rt.toNumber(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(offN) {
			offN = 0
		} else {
			offN = math.Trunc(offN)
		}
		if offN < 0 {
			return mkundef(), rt.rangeError("Start offset is negative")
		}
		// ToNumber(offset) may have detached/shrunk the target buffer: re-validate
		// before the range check (a detached target is a TypeError, not RangeError).
		if rt.taOutOfBounds(o) {
			return mkundef(), rt.typeError("Cannot set into a detached or out-of-bounds TypedArray")
		}
		targetLen := rt.taCurrentLen(o)
		bigTarget := isBigIntKind(o.ta.kind)
		src := arg(args, 0)
		// SetTypedArrayFromTypedArray: read the whole source first so overlapping
		// buffers copy correctly.
		if so := rt.objPtr(src); so != nil && so.ta != nil {
			srcLen := rt.taCurrentLen(so)
			if math.IsInf(offN, 1) || float64(srcLen)+offN > float64(targetLen) {
				return mkundef(), rt.rangeError("Source array is too large")
			}
			if bigTarget != isBigIntKind(so.ta.kind) {
				return mkundef(), rt.typeError("Cannot mix BigInt and non-BigInt typed arrays")
			}
			off := int(offN)
			if bigTarget {
				tmp := make([]*big.Int, srcLen)
				for i := 0; i < srcLen; i++ {
					v, _ := rt.taGet(so, i)
					tmp[i], _ = rt.toBigInt(v)
				}
				for i := 0; i < srcLen; i++ {
					rt.taSetBig(o, off+i, tmp[i])
				}
			} else {
				tmp := make([]float64, srcLen)
				for i := 0; i < srcLen; i++ {
					v, _ := rt.taGet(so, i)
					tmp[i] = v.Number()
				}
				for i := 0; i < srcLen; i++ {
					rt.taSet(o, off+i, tmp[i])
				}
			}
			return mkundef(), nil
		}
		// SetTypedArrayFromArrayLike.
		n, e := rt.lengthOf(src)
		if e != nil {
			return mkundef(), e
		}
		if math.IsInf(offN, 1) || float64(n)+offN > float64(targetLen) {
			return mkundef(), rt.rangeError("Source array is too large")
		}
		off := int(offN)
		for i := 0; i < n; i++ {
			el, e := rt.getElement(src, mknum(float64(i)))
			if e != nil {
				return mkundef(), e
			}
			if bigTarget {
				bi, e := rt.toBigInt(el)
				if e != nil {
					return mkundef(), e
				}
				rt.taSetBig(o, off+i, bi)
			} else {
				nv, e := rt.toNumber(el)
				if e != nil {
					return mkundef(), e
				}
				rt.taSet(o, off+i, nv)
			}
		}
		return mkundef(), nil
	})
	m("sort", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cmp := arg(args, 0)
		if !cmp.IsUndefined() && !rt.isCallable(cmp) {
			return mkundef(), rt.typeError("The comparison function must be either a function or undefined")
		}
		o := rt.objPtr(this)
		l := length(this)
		bigKind := isBigIntKind(o.ta.kind)
		vals := make([]Value, l) // snapshot before sorting (comparator may resize)
		for i := 0; i < l; i++ {
			vals[i], _ = rt.taGet(o, i)
		}
		var sortErr *ThrowError
		sort.SliceStable(vals, func(i, j int) bool {
			if sortErr != nil {
				return false
			}
			if rt.isCallable(cmp) {
				r, e := rt.callValue(cmp, mkundef(), []Value{vals[i], vals[j]})
				if e != nil {
					sortErr = e
					return false
				}
				n, e := rt.toNumber(r)
				if e != nil {
					sortErr = e
					return false
				}
				return n < 0 // NaN → not-less (n<0 false), matching +0 tie
			}
			return taDefaultCompare(rt, vals[i], vals[j], bigKind) < 0
		})
		if sortErr != nil {
			return mkundef(), sortErr
		}
		for i := 0; i < l && i < rt.taCurrentLen(o); i++ {
			if bigKind {
				bi, _ := rt.toBigInt(vals[i])
				rt.taSetBig(o, i, bi)
			} else {
				rt.taSet(o, i, vals[i].Number())
			}
		}
		return this, nil
	})
	m("toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		l := length(this)
		out := ""
		for i := 0; i < l; i++ {
			if i > 0 {
				out += ","
			}
			el := get(this, i)
			// Call the element's own toLocaleString, then ToString the result.
			m, e := rt.getField(el, "toLocaleString")
			if e != nil {
				return mkundef(), e
			}
			r, e := rt.callValue(m, el, nil)
			if e != nil {
				return mkundef(), e
			}
			s, e := rt.toStringValue(r)
			if e != nil {
				return mkundef(), e
			}
			out += string(rt.strBytes(s))
		}
		return rt.newString(out), nil
	})
	m("toReversed", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		out, _ := rt.newTypedArray(o.ta.kind, []Value{mknum(float64(l))})
		oo := rt.objPtr(out)
		bigKind := isBigIntKind(o.ta.kind)
		for i := 0; i < l; i++ {
			el, _ := rt.taGet(o, l-1-i)
			if bigKind {
				bi, _ := rt.toBigInt(el)
				rt.taSetBig(oo, i, bi)
			} else {
				rt.taSet(oo, i, el.Number())
			}
		}
		return out, nil
	})
	m("toSorted", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cmp := arg(args, 0)
		if !cmp.IsUndefined() && !rt.isCallable(cmp) {
			return mkundef(), rt.typeError("The comparison function must be either a function or undefined")
		}
		o := rt.objPtr(this)
		l := length(this)
		bigKind := isBigIntKind(o.ta.kind)
		vals := make([]Value, l)
		for i := 0; i < l; i++ {
			vals[i], _ = rt.taGet(o, i)
		}
		var sortErr *ThrowError
		sort.SliceStable(vals, func(i, j int) bool {
			if sortErr != nil {
				return false
			}
			if rt.isCallable(cmp) {
				r, e := rt.callValue(cmp, mkundef(), []Value{vals[i], vals[j]})
				if e != nil {
					sortErr = e
					return false
				}
				n, e := rt.toNumber(r)
				if e != nil {
					sortErr = e
					return false
				}
				return n < 0
			}
			return taDefaultCompare(rt, vals[i], vals[j], bigKind) < 0
		})
		if sortErr != nil {
			return mkundef(), sortErr
		}
		out, _ := rt.newTypedArray(o.ta.kind, []Value{mknum(float64(l))})
		oo := rt.objPtr(out)
		for i, v := range vals {
			if bigKind {
				bi, _ := rt.toBigInt(v)
				rt.taSetBig(oo, i, bi)
			} else {
				rt.taSet(oo, i, v.Number())
			}
		}
		return out, nil
	})
	m("with", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		bigKind := isBigIntKind(o.ta.kind)
		// Spec order: ToIntegerOrInfinity(index), then coerce the value.
		rel, e := rt.toIntegerOrInfinity(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		actual := rel
		if rel < 0 {
			actual = float64(l) + rel
		}
		var fv float64
		var bv *big.Int
		if bigKind {
			bv, e = rt.toBigInt(arg(args, 1))
		} else {
			fv, e = rt.toNumber(arg(args, 1))
		}
		if e != nil {
			return mkundef(), e
		}
		// IsValidIntegerIndex against the current length (value coercion may resize).
		cur := rt.taCurrentLen(o)
		if actual < 0 || actual >= float64(cur) {
			return mkundef(), rt.rangeError("Invalid typed array index")
		}
		actualIndex := int(actual)
		out, _ := rt.newTypedArray(o.ta.kind, []Value{mknum(float64(l))})
		oo := rt.objPtr(out)
		for i := 0; i < l; i++ {
			if i == actualIndex {
				if bigKind {
					rt.taSetBig(oo, i, bv)
				} else {
					rt.taSet(oo, i, fv)
				}
				continue
			}
			el, ok := rt.taGet(o, i)
			if !ok {
				el = mkundef()
			}
			if e := rt.setElement(out, mknum(float64(i)), el); e != nil {
				return mkundef(), e
			}
		}
		return out, nil
	})
	m("keys", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newIndexIterator(this, iterKeys), nil
	})
	m("entries", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newIndexIterator(this, iterEntries), nil
	})
	valuesFn := rt.newNativeFunc("values", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if e := rt.validateTypedArray(this); e != nil {
			return mkundef(), e
		}
		return rt.newIndexIterator(this, iterValues), nil
	})
	tp.defineOwn("values", valuesFn, attrWritable|attrConfigurable)
	if rt.symIterator != 0 {
		tp.defineOwnSymbol(rt.symIterator.handle(), valuesFn, attrWritable|attrConfigurable)
	}
}

// newTypedArrayView allocates a TypedArray sharing t's buffer (subarray).
func (rt *Runtime) newTypedArrayView(t *typedArray, start, length int) uint32 {
	h, o := rt.objects.alloc()
	o.proto = rt.typedArrayProtos[t.kind]
	o.shape = newShape()
	o.typeTag = TTypedArray
	o.flags.extensible = true
	o.ta = &typedArray{buf: t.buf, kind: t.kind, byteOffset: t.byteOffset + start*t.size(), length: length}
	return uint32(h)
}
