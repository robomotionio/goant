// Package engine is the pure-Go port of ant's "Silver" JavaScript engine.
//
// This file ports ant's NaN-boxed value representation (include/internal.h,
// src/ant.c). A [Value] is a 64-bit IEEE-754 double whose "not a number"
// bit patterns are reused to smuggle tagged, non-numeric values.
//
// Layout (identical to ant):
//
//	1111 1111 1111 TTTTT DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD
//	[-- prefix ---][type][--------------- 47-bit data --------------------]
//
// Any 64-bit pattern strictly greater than NANBOX_PREFIX (which is the bit
// pattern of -Infinity) is a tagged value; anything <= it is an ordinary
// double, so numeric math is free.
//
// DIVERGENCE FROM ant: in ant the 47-bit data field of a heap-resident type
// (T_OBJ/T_STR/…) holds a raw C pointer. In goant it instead holds a 32-bit
// handle into a chunked, non-moving pool (see pools.go), keeping Values
// pointer-free from the Go GC's perspective — a hard requirement for the JIT
// (native code may hold/copy Values freely) and for our own ported GC.
package engine

import "math"

// Value is a NaN-boxed JavaScript value.
type Value uint64

// NaN-box encoding constants — must stay byte-identical to ant's
// include/internal.h.
const (
	nanboxTypeMask  = 0x1F               // 5-bit tag
	nanboxTypeShift = 0x2F               // 47
	nanboxPrefix    = 0xFFF0000000000000 // -Infinity bit pattern
	nanboxDataMask  = 0x00007FFFFFFFFFFF // low 47 bits
	canonicalNaN    = 0x7FF8000000000000 // quiet NaN, < prefix so reads as double
)

// Type tags — the 5-bit tag stored in bits 51..47. Order and values are
// identical to the anonymous enum in include/internal.h; the JIT and the
// T_*_MASK bit-tests below depend on the exact numbering.
const (
	// heap-resident
	TObj Type = iota // 0
	TStr             // 1
	TArr             // 2

	// objects
	TFunc      // 3
	TCFunc     // 4
	TPromise   // 5
	TGenerator // 6

	// primitives
	TUndef  // 7
	TNull   // 8
	TBool   // 9
	TNum    // 10
	TBigInt // 11
	TSymbol // 12

	// internal
	TErr        // 13
	TTypedArray // 14
	TNTArg      // 15

	// collections
	TMap     // 16
	TSet     // 17
	TWeakMap // 18
	TWeakSet // 19

	TSentinel Type = nanboxTypeMask // 31
)

// Type is a 5-bit NaN-box type tag.
type Type uint8

// String-payload low-2-bit tags, preserved inside a T_STR Value's data field
// (include/internal.h STR_HEAP_TAG_*). Distinguishes flat strings, ropes, and
// builders without a second load.
const (
	strHeapTagMask    = 0x3
	strHeapTagFlat    = 0x0
	strHeapTagRope    = 0x1
	strHeapTagBuilder = 0x2
)

// tagFlagBit maps a Type to its bit in the T_*_MASK bitsets.
func tagFlagBit(t Type) uint32 { return 1 << uint32(t) }

// Bitset masks over the type tags (include/internal.h). Used for the hot
// is_object_type / is_non_numeric style predicates.
const (
	tObjectMask = (1 << TObj) | (1 << TArr) | (1 << TFunc) |
		(1 << TPromise) | (1 << TGenerator)
	tSpecialObjectMask = (1 << TObj) | (1 << TArr)
	tNonNumericMask    = (1 << TStr) | (1 << TArr) | (1 << TFunc) |
		(1 << TCFunc) | (1 << TObj) | (1 << TGenerator)
)

// tEmpty is the array-hole / empty-slot sentinel (include/internal.h T_EMPTY).
const tEmpty Value = nanboxPrefix | (Value(TSentinel) << nanboxTypeShift) | 0xDEAD

// isTagged reports whether v is a tagged (non-double) value.
func (v Value) isTagged() bool { return uint64(v) > nanboxPrefix }

// Type returns v's NaN-box type tag. Untagged (numeric) values report TNum.
func (v Value) Type() Type {
	if v.isTagged() {
		return Type((uint64(v) >> nanboxTypeShift) & nanboxTypeMask)
	}
	return TNum
}

// Data returns v's 47-bit payload (a pool handle, immediate, or tagged
// pointer, depending on the type).
func (v Value) Data() uint64 { return uint64(v) & nanboxDataMask }

// mkval builds a tagged value from a type and 47-bit payload (ant's mkval).
func mkval(t Type, data uint64) Value {
	return Value(nanboxPrefix | (uint64(t) << nanboxTypeShift) | (data & nanboxDataMask))
}

// tov boxes a float64, canonicalizing NaN exactly as ant's tov does: a NaN
// whose raw bits land above the prefix (and would thus read as a tagged value)
// collapses to the canonical quiet NaN.
func tov(d float64) Value {
	bits := math.Float64bits(d)
	if d != d { // NaN
		if bits > nanboxPrefix {
			return canonicalNaN
		}
		return Value(bits)
	}
	return Value(bits)
}

// tod reinterprets v's bits as a float64 (valid only when !isTagged).
func tod(v Value) float64 { return math.Float64frombits(uint64(v)) }

// ---- immediate / primitive constructors (ant's js_mk* sugar) ----

func mkundef() Value { return mkval(TUndef, 0) }
func mknull() Value  { return mkval(TNull, 0) }
func mkbool(b bool) Value {
	if b {
		return mkval(TBool, 1)
	}
	return mkval(TBool, 0)
}
func mktrue() Value  { return mkval(TBool, 1) }
func mkfalse() Value { return mkval(TBool, 0) }

// mknum boxes a JS number.
func mknum(d float64) Value { return tov(d) }

// mkint boxes an integer-valued JS number.
func mkint(i int64) Value { return tov(float64(i)) }

// ---- predicates (include/internal.h inline helpers) ----

func (v Value) IsUndefined() bool { return v.Type() == TUndef }
func (v Value) IsNull() bool      { return v.Type() == TNull }
func (v Value) IsNullish() bool   { t := v.Type(); return t == TUndef || t == TNull }
func (v Value) IsErr() bool       { return v.Type() == TErr }
func (v Value) IsNumber() bool    { return !v.isTagged() }
func (v Value) IsBool() bool      { return v.Type() == TBool }
func (v Value) IsString() bool    { return v.Type() == TStr }
func (v Value) IsSymbol() bool    { return v.Type() == TSymbol }
func (v Value) IsEmpty() bool     { return v == tEmpty }

// IsObjectType reports whether v is one of the object-family tags
// (obj/arr/func/promise/generator).
func (v Value) IsObjectType() bool { return (tagFlagBit(v.Type()) & tObjectMask) != 0 }

// IsSpecialObject reports whether v is a plain object or array.
func (v Value) IsSpecialObject() bool { return (tagFlagBit(v.Type()) & tSpecialObjectMask) != 0 }

// IsNonNumeric mirrors ant's is_non_numeric bit-test.
func (v Value) IsNonNumeric() bool { return (tagFlagBit(v.Type()) & tNonNumericMask) != 0 }

// Bool extracts the boolean payload (undefined behavior if not a TBool).
func (v Value) Bool() bool { return v.Data() != 0 }

// Number extracts the float64 (undefined behavior if tagged).
func (v Value) Number() float64 { return tod(v) }

// handle returns the low-32-bit pool handle for heap-resident values.
func (v Value) handle() uint32 { return uint32(v.Data()) }

// typeName returns the ant typestr() name for a tag (used in diagnostics).
func typeName(t Type) string {
	switch t {
	case TObj:
		return "object"
	case TStr:
		return "string"
	case TArr:
		return "array"
	case TFunc, TCFunc:
		return "function"
	case TPromise:
		return "promise"
	case TGenerator:
		return "generator"
	case TUndef:
		return "undefined"
	case TNull:
		return "object" // typeof null === "object"
	case TBool:
		return "boolean"
	case TNum:
		return "number"
	case TBigInt:
		return "bigint"
	case TSymbol:
		return "symbol"
	case TErr:
		return "error"
	case TTypedArray:
		return "object"
	case TMap:
		return "map"
	case TSet:
		return "set"
	case TWeakMap:
		return "weakmap"
	case TWeakSet:
		return "weakset"
	default:
		return "unknown"
	}
}
