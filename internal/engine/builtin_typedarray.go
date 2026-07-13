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
	buf        Value  // backing ArrayBuffer value (for .buffer)
	bytes      []byte // alias of the ArrayBuffer's byte store
	kind       taKind
	byteOffset int
	length     int
}

type dataView struct {
	buf        Value
	bytes      []byte
	byteOffset int
	byteLength int
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

// taDetached reports whether o is a TypedArray whose backing ArrayBuffer has
// been detached (its bytes transferred away). Element access, length, and
// most prototype methods observe a detached array as empty / throwing.
func (rt *Runtime) taDetached(o *object) bool {
	if o == nil || o.ta == nil {
		return false
	}
	b := rt.objPtr(o.ta.buf)
	return b == nil || b.abuf == nil
}

// validateTypedArray implements ValidateTypedArray(O): O must be a TypedArray
// whose buffer is not detached, else a TypeError. Used to guard the
// non-generic %TypedArray%.prototype methods.
func (rt *Runtime) validateTypedArray(this Value) *ThrowError {
	o := rt.objPtr(this)
	if o == nil || o.ta == nil {
		return rt.typeError("TypedArray.prototype method called on incompatible receiver")
	}
	if rt.taDetached(o) {
		return rt.typeError("Cannot perform TypedArray operation on a detached ArrayBuffer")
	}
	return nil
}

// taValidIndex implements IsValidIntegerIndex(O, i): i addresses a live element
// of typed array o (in bounds, buffer attached).
func (rt *Runtime) taValidIndex(o *object, i int) bool {
	return o != nil && o.ta != nil && !rt.taDetached(o) && i >= 0 && i < o.ta.length
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
	if t == nil || rt.taDetached(o) || i < 0 || i >= t.length {
		return mkundef(), false
	}
	off := t.byteOffset + i*t.size()
	if isBigIntKind(t.kind) {
		u := binary.LittleEndian.Uint64(t.bytes[off:])
		if t.kind == taBigInt64 {
			return rt.newBigInt(big.NewInt(int64(u))), true
		}
		return rt.newBigInt(new(big.Int).SetUint64(u)), true
	}
	return mknum(decodeElem(t.bytes, off, t.kind)), true
}

func (rt *Runtime) taSet(o *object, i int, v float64) bool {
	t := o.ta
	if t == nil || rt.taDetached(o) || i < 0 || i >= t.length {
		return false
	}
	encodeElem(t.bytes, t.byteOffset+i*t.size(), t.kind, v)
	return true
}

// taSetBig writes a BigInt element (BigInt64Array / BigUint64Array) as its
// low-64-bit two's-complement pattern.
func (rt *Runtime) taSetBig(o *object, i int, v *big.Int) bool {
	t := o.ta
	if t == nil || rt.taDetached(o) || i < 0 || i >= t.length {
		return false
	}
	binary.LittleEndian.PutUint64(t.bytes[t.byteOffset+i*t.size():], bigIntAsUintN(64, v).Uint64())
	return true
}

func (rt *Runtime) newArrayBuffer(byteLen int) Value {
	v := rt.newObject(rt.arrayBufferProto)
	o := rt.objPtr(v)
	if byteLen < 0 {
		byteLen = 0
	}
	o.abuf = make([]byte, byteLen)
	return v
}

// newTypedArray builds a view of `kind` from a constructor argument.
func (rt *Runtime) newTypedArray(kind taKind, args []Value) (Value, *ThrowError) {
	h, o := rt.objects.alloc()
	o.proto = rt.typedArrayProtos[kind]
	o.shape = newShape()
	o.typeTag = TTypedArray
	o.flags.extensible = true
	tv := mkval(TTypedArray, uint64(h))

	a0 := arg(args, 0)
	switch {
	case a0.IsNumber() || a0.IsUndefined():
		n := 0
		if a0.IsNumber() {
			n = int(a0.Number())
		}
		buf := rt.newArrayBuffer(n * taKinds[kind].size)
		o.ta = &typedArray{buf: buf, bytes: rt.objPtr(buf).abuf, kind: kind, length: n}
	case rt.isArrayBufferValue(a0):
		bo := rt.objPtr(a0)
		byteOff := 0
		if b := arg(args, 1); b.IsNumber() {
			byteOff = int(b.Number())
		}
		length := (len(bo.abuf) - byteOff) / taKinds[kind].size
		if l := arg(args, 2); l.IsNumber() {
			length = int(l.Number())
		}
		o.ta = &typedArray{buf: a0, bytes: bo.abuf, kind: kind, byteOffset: byteOff, length: length}
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
		buf := rt.newArrayBuffer(len(items) * taKinds[kind].size)
		o.ta = &typedArray{buf: buf, bytes: rt.objPtr(buf).abuf, kind: kind, length: len(items)}
		for i, it := range items {
			n, _ := rt.toNumber(it)
			rt.taSet(o, i, n)
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
			src := arg(args, 0)
			var items []Value
			if rt.isIterable(src) {
				it, e := rt.iterableValues(src)
				if e != nil {
					return mkundef(), e
				}
				items = it
			} else {
				n, _ := rt.lengthOf(src)
				for i := 0; i < n; i++ {
					el, _ := rt.getElement(src, mknum(float64(i)))
					items = append(items, el)
				}
			}
			mapFn := arg(args, 1)
			arrV, _ := rt.newTypedArray(kind, []Value{mknum(float64(len(items)))})
			ao := rt.objPtr(arrV)
			for i, it := range items {
				v := it
				if rt.isCallable(mapFn) {
					mv, e := rt.callValue(mapFn, arg(args, 2), []Value{it, mknum(float64(i))})
					if e != nil {
						return mkundef(), e
					}
					v = mv
				}
				n, _ := rt.toNumber(v)
				rt.taSet(ao, i, n)
			}
			return arrV, nil
		})
		rt.defMethod(cobj, "of", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			arrV, _ := rt.newTypedArray(kind, []Value{mknum(float64(len(args)))})
			ao := rt.objPtr(arrV)
			for i, it := range args {
				n, _ := rt.toNumber(it)
				rt.taSet(ao, i, n)
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
	po.defineAccessor("byteLength", rt.newNativeFunc("byteLength", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if o := rt.objPtr(this); o != nil {
			return mknum(float64(len(o.abuf))), nil
		}
		return mknum(0), nil
	}), mkundef(), true, false, attrConfigurable)
	// detached: an ArrayBuffer whose bytes have been transferred away (abuf nil).
	po.defineAccessor("detached", rt.newNativeFunc("detached", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		return mkbool(o != nil && o.abuf == nil), nil
	}), mkundef(), true, false, attrConfigurable)
	transfer := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.abuf == nil {
			return mkundef(), rt.typeError("Cannot transfer a detached ArrayBuffer")
		}
		newLen := len(o.abuf)
		if a := arg(args, 0); a.IsNumber() {
			newLen = int(a.Number())
		}
		nb := rt.newArrayBuffer(newLen)
		copy(rt.objPtr(nb).abuf, o.abuf)
		o.abuf = nil // detach the source
		return nb, nil
	}
	rt.defMethod(po, "transfer", 0, transfer)
	rt.defMethod(po, "transferToFixedLength", 0, transfer)
	rt.defMethod(po, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.abuf == nil {
			return mkundef(), rt.typeError("ArrayBuffer.prototype.slice on incompatible receiver")
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
		nb := rt.newArrayBuffer(end - start)
		copy(rt.objPtr(nb).abuf, o.abuf[start:end])
		return nb, nil
	})
	rt.setStringTag(proto, "ArrayBuffer")

	ctor := rt.newNativeFunc("ArrayBuffer", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !rt.constructing() {
			return mkundef(), rt.typeError("Constructor ArrayBuffer requires 'new'")
		}
		n := 0
		if a := arg(args, 0); a.IsNumber() {
			n = int(a.Number())
		}
		return rt.newArrayBuffer(n), nil
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
			if rt.dvDetached(o) {
				return mkundef(), rt.typeError("Cannot get value from a detached ArrayBuffer")
			}
			if idx+t.size > o.dv.byteLength {
				return mkundef(), rt.rangeError("Offset is outside the bounds of the DataView")
			}
			return mknum(t.dec(o.dv.bytes, o.dv.byteOffset+idx, le)), nil
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
			if rt.dvDetached(o) {
				return mkundef(), rt.typeError("Cannot set value on a detached ArrayBuffer")
			}
			if idx+t.size > o.dv.byteLength {
				return mkundef(), rt.rangeError("Offset is outside the bounds of the DataView")
			}
			t.enc(o.dv.bytes, o.dv.byteOffset+idx, val, le)
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
			if rt.dvDetached(o) {
				return mkundef(), rt.typeError("Cannot get value from a detached ArrayBuffer")
			}
			if idx+8 > o.dv.byteLength {
				return mkundef(), rt.rangeError("Offset is outside the bounds of the DataView")
			}
			off := o.dv.byteOffset + idx
			u := order(le).Uint64(o.dv.bytes[off:])
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
			if rt.dvDetached(o) {
				return mkundef(), rt.typeError("Cannot set value on a detached ArrayBuffer")
			}
			if idx+8 > o.dv.byteLength {
				return mkundef(), rt.rangeError("Offset is outside the bounds of the DataView")
			}
			order(le).PutUint64(o.dv.bytes[o.dv.byteOffset+idx:], bigIntAsUintN(64, bi).Uint64())
			return mkundef(), nil
		})
	}
	po.defineAccessor("byteLength", rt.newNativeFunc("byteLength", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.dv == nil {
			return mkundef(), rt.typeError("DataView.prototype.byteLength on incompatible receiver")
		}
		if rt.dvDetached(o) {
			return mkundef(), rt.typeError("Cannot read byteLength of a detached ArrayBuffer")
		}
		return mknum(float64(o.dv.byteLength)), nil
	}), mkundef(), true, false, attrConfigurable)
	po.defineAccessor("byteOffset", rt.newNativeFunc("byteOffset", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.dv == nil {
			return mkundef(), rt.typeError("DataView.prototype.byteOffset on incompatible receiver")
		}
		if rt.dvDetached(o) {
			return mkundef(), rt.typeError("Cannot read byteOffset of a detached ArrayBuffer")
		}
		return mknum(float64(o.dv.byteOffset)), nil
	}), mkundef(), true, false, attrConfigurable)
	po.defineAccessor("buffer", rt.newNativeFunc("buffer", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
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
		viewLen := bufLen - offset
		if lv := arg(args, 2); !lv.IsUndefined() {
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
		rt.objPtr(v).dv = &dataView{buf: bufV, bytes: bo.abuf, byteOffset: offset, byteLength: viewLen}
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
	if len(args) == 1 && args[0].IsNumber() && ro.ta.length < int(args[0].Number()) {
		return mkundef(), rt.typeError("Derived TypedArray constructor created an array shorter than requested")
	}
	return res, nil
}

// taLength returns a TypedArray's element length (for lengthOf / methods).
func (rt *Runtime) taLength(o *object) int {
	if o.ta != nil && !rt.taDetached(o) {
		return o.ta.length
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
	tp.defineAccessor("length", rt.newNativeFunc("length", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mknum(float64(length(this))), nil
	}), mkundef(), true, false, attrConfigurable)
	tp.defineAccessor("byteLength", rt.newNativeFunc("byteLength", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if o := rt.objPtr(this); o != nil && o.ta != nil {
			return mknum(float64(o.ta.length * o.ta.size())), nil
		}
		return mknum(0), nil
	}), mkundef(), true, false, attrConfigurable)
	tp.defineAccessor("byteOffset", rt.newNativeFunc("byteOffset", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if o := rt.objPtr(this); o != nil && o.ta != nil {
			return mknum(float64(o.ta.byteOffset)), nil
		}
		return mknum(0), nil
	}), mkundef(), true, false, attrConfigurable)
	tp.defineAccessor("buffer", rt.newNativeFunc("buffer", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if o := rt.objPtr(this); o != nil && o.ta != nil {
			return o.ta.buf, nil
		}
		return mkundef(), nil
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
		l := length(this)
		out, e := rt.typedArraySpeciesCreate(this, []Value{mknum(float64(l))})
		if e != nil {
			return mkundef(), e
		}
		oo := rt.objPtr(out)
		for i := 0; i < l; i++ {
			r, e := rt.callValue(cb, arg(args, 1), []Value{get(this, i), mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			nv, _ := rt.toNumber(r)
			rt.taSet(oo, i, nv)
		}
		return out, nil
	})
	m("filter", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cb := arg(args, 0)
		var keep []float64
		for i, l := 0, length(this); i < l; i++ {
			el := get(this, i)
			r, e := rt.callValue(cb, arg(args, 1), []Value{el, mknum(float64(i)), this})
			if e != nil {
				return mkundef(), e
			}
			if rt.toBoolean(r) {
				keep = append(keep, el.Number())
			}
		}
		out, e := rt.typedArraySpeciesCreate(this, []Value{mknum(float64(len(keep)))})
		if e != nil {
			return mkundef(), e
		}
		oo := rt.objPtr(out)
		for i, v := range keep {
			rt.taSet(oo, i, v)
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
		target, _ := rt.toNumber(arg(args, 0))
		for i, l := 0, length(this); i < l; i++ {
			if get(this, i).Number() == target {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	m("lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target, _ := rt.toNumber(arg(args, 0))
		for i := length(this) - 1; i >= 0; i-- {
			if get(this, i).Number() == target {
				return mknum(float64(i)), nil
			}
		}
		return mknum(-1), nil
	})
	m("includes", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target, _ := rt.toNumber(arg(args, 0))
		l := length(this)
		start := rt.relativeIndex(arg(args, 1), l)
		for i := start; i < l; i++ {
			if rt.sameValueZero(get(this, i), mknum(target)) {
				return mktrue(), nil
			}
		}
		return mkfalse(), nil
	})
	m("join", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sep := ","
		if s := arg(args, 0); !s.IsUndefined() {
			sv, _ := rt.toStringValue(s)
			sep = string(rt.strBytes(sv))
		}
		out := ""
		for i, l := 0, length(this); i < l; i++ {
			if i > 0 {
				out += sep
			}
			out += numberToString(get(this, i).Number())
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
		v, _ := rt.toNumber(arg(args, 0))
		l := length(this)
		start := rt.relativeIndex(arg(args, 1), l)
		end := l
		if !arg(args, 2).IsUndefined() {
			end = rt.relativeIndex(arg(args, 2), l)
		}
		for i := start; i < end; i++ {
			rt.taSet(o, i, v)
		}
		return this, nil
	})
	m("slice", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		start := rt.relativeIndex(arg(args, 0), l)
		end := l
		if !arg(args, 1).IsUndefined() {
			end = rt.relativeIndex(arg(args, 1), l)
		}
		if end < start {
			end = start
		}
		out, e := rt.typedArraySpeciesCreate(this, []Value{mknum(float64(end - start))})
		if e != nil {
			return mkundef(), e
		}
		oo := rt.objPtr(out)
		for i := 0; i < end-start; i++ {
			el, _ := rt.taGet(o, start+i)
			rt.taSet(oo, i, el.Number())
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
	m("subarray", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		start := rt.relativeIndex(arg(args, 0), l)
		end := l
		if !arg(args, 1).IsUndefined() {
			end = rt.relativeIndex(arg(args, 1), l)
		}
		if end < start {
			end = start
		}
		// subarray shares the buffer: SpeciesConstructor is invoked with
		// (buffer, absoluteByteOffset, newLength).
		byteOffset := o.ta.byteOffset + start*o.ta.size()
		return rt.typedArraySpeciesCreate(this, []Value{
			o.ta.buf, mknum(float64(byteOffset)), mknum(float64(end - start)),
		})
	})
	m("copyWithin", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		to := rt.relativeIndex(arg(args, 0), l)
		from := rt.relativeIndex(arg(args, 1), l)
		final := l
		if !arg(args, 2).IsUndefined() {
			final = rt.relativeIndex(arg(args, 2), l)
		}
		count := final - from
		if count > l-to {
			count = l - to
		}
		buf := make([]float64, 0, count)
		for i := 0; i < count; i++ {
			el, _ := rt.taGet(o, from+i)
			buf = append(buf, el.Number())
		}
		for i := 0; i < count; i++ {
			rt.taSet(o, to+i, buf[i])
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
		targetLen := o.ta.length
		bigTarget := isBigIntKind(o.ta.kind)
		src := arg(args, 0)
		// SetTypedArrayFromTypedArray: read the whole source first so overlapping
		// buffers copy correctly.
		if so := rt.objPtr(src); so != nil && so.ta != nil {
			srcLen := so.ta.length
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
		o := rt.objPtr(this)
		l := length(this)
		vals := make([]float64, l)
		for i := 0; i < l; i++ {
			el, _ := rt.taGet(o, i)
			vals[i] = el.Number()
		}
		cmp := arg(args, 0)
		var sortErr *ThrowError
		sort.SliceStable(vals, func(i, j int) bool {
			if rt.isCallable(cmp) {
				r, e := rt.callValue(cmp, mkundef(), []Value{mknum(vals[i]), mknum(vals[j])})
				if e != nil {
					sortErr = e
					return false
				}
				n, _ := rt.toNumber(r)
				return n < 0
			}
			return vals[i] < vals[j]
		})
		if sortErr != nil {
			return mkundef(), sortErr
		}
		for i := 0; i < l; i++ {
			rt.taSet(o, i, vals[i])
		}
		return this, nil
	})
	m("toReversed", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		out, _ := rt.newTypedArray(o.ta.kind, []Value{mknum(float64(l))})
		oo := rt.objPtr(out)
		for i := 0; i < l; i++ {
			el, _ := rt.taGet(o, l-1-i)
			rt.taSet(oo, i, el.Number())
		}
		return out, nil
	})
	m("toSorted", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		vals := make([]float64, l)
		for i := 0; i < l; i++ {
			el, _ := rt.taGet(o, i)
			vals[i] = el.Number()
		}
		cmp := arg(args, 0)
		var sortErr *ThrowError
		sort.SliceStable(vals, func(i, j int) bool {
			if rt.isCallable(cmp) {
				r, e := rt.callValue(cmp, mkundef(), []Value{mknum(vals[i]), mknum(vals[j])})
				if e != nil {
					sortErr = e
					return false
				}
				n, _ := rt.toNumber(r)
				return n < 0
			}
			return vals[i] < vals[j]
		})
		if sortErr != nil {
			return mkundef(), sortErr
		}
		out, _ := rt.newTypedArray(o.ta.kind, []Value{mknum(float64(l))})
		oo := rt.objPtr(out)
		for i, v := range vals {
			rt.taSet(oo, i, v)
		}
		return out, nil
	})
	m("with", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		l := length(this)
		idx := int(argNum(rt, args, 0))
		if idx < 0 {
			idx += l
		}
		if idx < 0 || idx >= l {
			return mkundef(), rt.rangeError("Invalid typed array index")
		}
		out, _ := rt.newTypedArray(o.ta.kind, []Value{mknum(float64(l))})
		oo := rt.objPtr(out)
		for i := 0; i < l; i++ {
			el, _ := rt.taGet(o, i)
			rt.taSet(oo, i, el.Number())
		}
		rt.taSet(oo, idx, argNum(rt, args, 1))
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
	o.ta = &typedArray{buf: t.buf, bytes: t.bytes, kind: t.kind, byteOffset: t.byteOffset + start*t.size(), length: length}
	return uint32(h)
}
