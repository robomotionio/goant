package engine

// Primitive coercions (ant src/ant.c ToBoolean/ToNumber/ToString sections).
// Object coercions (ToPrimitive dispatching to valueOf/toString, ToPropertyKey
// on symbols) require the interpreter and land in Phase 3; these cover the
// primitive cases the interpreter builds on.

import "math"

// toBoolean implements ECMAScript ToBoolean.
func (rt *Runtime) toBoolean(v Value) bool {
	switch v.Type() {
	case TUndef, TNull:
		return false
	case TBool:
		return v.Bool()
	case TNum:
		d := v.Number()
		return d != 0 && !math.IsNaN(d)
	case TStr:
		return len(rt.strBytes(v)) != 0
	case TBigInt:
		return rt.bigIntVal(v).Sign() != 0 // 0n is falsy
	case TSymbol:
		return true // symbols are always truthy
	default:
		return true // all objects are truthy
	}
}

// toNumberPrimitive implements ToNumber for non-object values. Objects (which
// require ToPrimitive → valueOf/toString) are handled in the interpreter.
func (rt *Runtime) toNumberPrimitive(v Value) (float64, bool) {
	switch v.Type() {
	case TNum:
		return v.Number(), true
	case TUndef:
		return math.NaN(), true
	case TNull:
		return 0, true
	case TBool:
		if v.Bool() {
			return 1, true
		}
		return 0, true
	case TStr:
		return stringToNumber(rt.strGo(v)), true
	default:
		return 0, false // needs ToPrimitive / throws (symbol, bigint)
	}
}

// toStringPrimitive implements ToString for non-object values, returning a
// flat-string Value. ok=false for values needing ToPrimitive or that throw.
func (rt *Runtime) toStringPrimitive(v Value) (Value, bool) {
	switch v.Type() {
	case TStr:
		return v, true
	case TUndef:
		return rt.internString("undefined"), true
	case TNull:
		return rt.internString("null"), true
	case TBool:
		if v.Bool() {
			return rt.internString("true"), true
		}
		return rt.internString("false"), true
	case TNum:
		return rt.newString(numberToString(v.Number())), true
	default:
		return mkundef(), false // object → ToPrimitive; symbol → TypeError
	}
}

// toPrimitive implements OrdinaryToPrimitive: for objects, tries valueOf then
// toString (default/number hint) or toString then valueOf (string hint).
// Symbol.toPrimitive support lands with Symbol in Phase 5.
func (rt *Runtime) toPrimitive(v Value, hint string) (Value, *ThrowError) {
	if !v.IsObjectType() && v.Type() != TTypedArray {
		return v, nil
	}
	// A Symbol.toPrimitive method overrides the ordinary valueOf/toString order.
	// getElement routes the symbol lookup through a Proxy's [[Get]] trap.
	if rt.symToPrimitive != 0 {
		exotic, e := rt.getElement(v, rt.symToPrimitive)
		if e != nil {
			return mkundef(), e
		}
		// GetMethod(@@toPrimitive): undefined/null falls back to the ordinary
		// algorithm; a present-but-not-callable value is a TypeError.
		if !exotic.IsNullish() {
			if !rt.isCallable(exotic) {
				return mkundef(), rt.typeError("Symbol.toPrimitive method is not a function")
			}
			h := hint
			if h == "" {
				h = "default"
			}
			res, e := rt.callValue(exotic, v, []Value{rt.newString(h)})
			if e != nil {
				return mkundef(), e
			}
			// IsObjectLike, not IsObjectType: T_TYPEDARRAY is not in tObjectMask,
			// so a @@toPrimitive that returned a view would be accepted as a
			// primitive. See ordinaryToPrimitive below for what that cost.
			if res.IsObjectLike() {
				return mkundef(), rt.typeError("Cannot convert object to primitive value")
			}
			return res, nil
		}
	}
	return rt.ordinaryToPrimitive(v, hint)
}

// ordinaryToPrimitive implements OrdinaryToPrimitive(O, hint): try valueOf then
// toString (or toString then valueOf when hint is "string"), skipping a
// non-callable method and returning the first primitive result; a TypeError if
// neither yields a primitive.
func (rt *Runtime) ordinaryToPrimitive(v Value, hint string) (Value, *ThrowError) {
	methods := [2]string{"valueOf", "toString"}
	if hint == "string" {
		methods = [2]string{"toString", "valueOf"}
	}
	for _, m := range methods {
		fn, e := rt.getField(v, m)
		if e != nil {
			return mkundef(), e
		}
		if rt.isCallable(fn) {
			res, e := rt.callValue(fn, v, nil)
			if e != nil {
				return mkundef(), e
			}
			// IsObjectLike rather than IsObjectType, and the difference was a
			// process-killer. Object.prototype.valueOf returns its receiver, so
			// for a TypedArray this asked "is a view an object?" of a predicate
			// that answers no — T_TYPEDARRAY is not in tObjectMask — and returned
			// the view itself as though it were a primitive.
			//
			// `6 == new Int16Array([1,2,3])` then recursed in abstractEquals on
			// unchanged arguments until the Go stack was gone: a fatal error, not
			// a catchable RangeError, not stoppable by Interrupt. Ordinary
			// JavaScript, killing the host process.
			//
			// The same tag being outside the object mask had already produced a
			// silent Dirty() failure for every TypedArray write. It is worth
			// suspecting anywhere IsObjectType guards a "did I get an object
			// back" question rather than a "which family is this" one.
			if !res.IsObjectLike() {
				return res, nil
			}
		}
	}
	return mkundef(), rt.typeError("cannot convert object to primitive value")
}

// toObjectValue implements ES ToObject: objects pass through; primitives box
// into a fresh wrapper with the matching prototype; null/undefined throw.
func (rt *Runtime) toObjectValue(v Value) (Value, *ThrowError) {
	if v.IsNullish() {
		return mkundef(), rt.typeError("Cannot convert undefined or null to object")
	}
	if v.IsObjectType() || v.Type() == TTypedArray {
		return v, nil
	}
	w := rt.newObject(rt.primitiveProto(v))
	o := rt.objPtr(w)
	o.boxed = v
	if v.Type() == TNum || v.Type() == TBigInt {
		// A Number/BigInt wrapper's [[NumberData]]/[[BigIntData]] is marked in
		// slotPrimitive so it is distinguishable from a plain object (whose
		// zero-value boxed reads as the number 0).
		o.setSlot(slotPrimitive, v)
	}
	if v.Type() == TStr {
		// A String exotic object carries a non-writable, non-enumerable,
		// non-configurable "length" own property (matches new String(...)).
		o.defineOwn("length", mknum(float64(rt.strLen16(v))), 0)
	}
	return w, nil
}

// toStringValue implements the full ToString (objects via ToPrimitive+toString).
func (rt *Runtime) toStringValue(v Value) (Value, *ThrowError) {
	if v.IsObjectType() || v.Type() == TTypedArray {
		p, e := rt.toPrimitive(v, "string")
		if e != nil {
			return mkundef(), e
		}
		v = p
	}
	s, ok := rt.toStringPrimitive(v)
	if !ok {
		if v.Type() == TBigInt {
			return rt.newString(bigIntToString(rt.bigIntVal(v), 10)), nil
		}
		if v.IsSymbol() {
			return mkundef(), rt.typeError("cannot convert a Symbol value to a string")
		}
		return mkundef(), rt.typeError("cannot convert value to string")
	}
	return s, nil
}

// sameValue implements the SameValue algorithm (Object.is): like === but NaN
// equals NaN and +0 ≠ -0.
func (rt *Runtime) sameValue(a, b Value) bool {
	if a.Type() == TNum && b.Type() == TNum {
		da, db := a.Number(), b.Number()
		if math.IsNaN(da) && math.IsNaN(db) {
			return true
		}
		if da == 0 && db == 0 {
			return math.Signbit(da) == math.Signbit(db)
		}
		return da == db
	}
	return rt.sameValueNonNumber(a, b)
}

// sameValueZero is SameValue but +0 and -0 are equal (Array.includes, Map/Set).
func (rt *Runtime) sameValueZero(a, b Value) bool {
	if a.Type() == TNum && b.Type() == TNum {
		da, db := a.Number(), b.Number()
		if math.IsNaN(da) && math.IsNaN(db) {
			return true
		}
		return da == db
	}
	return rt.sameValueNonNumber(a, b)
}

func (rt *Runtime) sameValueNonNumber(a, b Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	if a.Type() == TStr {
		return rt.strGo(a) == rt.strGo(b)
	}
	if a.Type() == TBigInt {
		// BigInts compare by value, not by the handle stored in the Value bits.
		x, y := rt.bigIntVal(a), rt.bigIntVal(b)
		return x != nil && y != nil && x.Cmp(y) == 0
	}
	// undefined, null, boolean, symbol, object: identity by raw Value bits.
	return a == b
}

// strictEquals implements the Strict Equality (===) algorithm for the cases not
// requiring further coercion (numbers, strings, and identity). BigInt-specific
// comparison lands in Phase 8.
func (rt *Runtime) strictEquals(a, b Value) bool {
	ta, tb := a.Type(), b.Type()
	if ta == TNum && tb == TNum {
		return a.Number() == b.Number() // NaN != NaN, +0 == -0
	}
	if ta != tb {
		return false
	}
	if ta == TStr {
		return rt.strGo(a) == rt.strGo(b)
	}
	if ta == TBigInt {
		x, y := rt.bigIntVal(a), rt.bigIntVal(b)
		return x != nil && y != nil && x.Cmp(y) == 0
	}
	return a == b
}
