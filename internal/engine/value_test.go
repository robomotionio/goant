package engine

import (
	"math"
	"testing"
)

func TestNumberRoundTrip(t *testing.T) {
	cases := []float64{
		0, math.Copysign(0, -1), 1, -1, 42, -42.5,
		math.MaxFloat64, math.SmallestNonzeroFloat64,
		math.Inf(1), math.Inf(-1),
		3.141592653589793, 1e308, -1e308, 1e-308,
	}
	for _, d := range cases {
		v := mknum(d)
		if !v.IsNumber() {
			t.Fatalf("mknum(%v): IsNumber()=false", d)
		}
		got := v.Number()
		if got != d && !(d == 0 && got == 0) {
			t.Fatalf("round-trip %v: got %v", d, got)
		}
		if v.Type() != TNum {
			t.Fatalf("mknum(%v): Type=%v want TNum", d, v.Type())
		}
	}
}

func TestNaNCanonicalization(t *testing.T) {
	// A plain NaN round-trips to a value that still reports as a number.
	v := mknum(math.NaN())
	if !v.IsNumber() {
		t.Fatalf("NaN: IsNumber()=false")
	}
	if !math.IsNaN(v.Number()) {
		t.Fatalf("NaN: Number()=%v not NaN", v.Number())
	}
	// A NaN bit pattern whose raw bits exceed the prefix must collapse to the
	// canonical quiet NaN so it never aliases a tagged value.
	bad := math.Float64frombits(0xFFF8000000000001) // signaling-ish, > prefix
	v2 := tov(bad)
	if v2.isTagged() {
		t.Fatalf("bad NaN boxed as tagged value: %#x", uint64(v2))
	}
	if uint64(v2) != canonicalNaN {
		t.Fatalf("bad NaN not canonicalized: got %#x", uint64(v2))
	}
}

func TestTagRoundTrip(t *testing.T) {
	tags := []Type{
		TObj, TStr, TArr, TFunc, TCFunc, TPromise, TGenerator,
		TUndef, TNull, TBool, TNum, TBigInt, TSymbol,
		TErr, TTypedArray, TNTArg, TMap, TSet, TWeakMap, TWeakSet,
	}
	for _, tag := range tags {
		if tag == TNum {
			continue // TNum is the untagged case, exercised above
		}
		payloads := []uint64{0, 1, 0xDEAD, nanboxDataMask, nanboxDataMask - 1}
		for _, p := range payloads {
			// mkval(TObj, 0) == nanboxPrefix (-Infinity) is the sole encoding
			// that is not "tagged"; the null handle 0 is reserved so a real
			// TObj value never has data 0. Skip that invalid combination.
			if tag == TObj && p == 0 {
				continue
			}
			v := mkval(tag, p)
			if !v.isTagged() {
				t.Fatalf("mkval(%v,%#x): not tagged", tag, p)
			}
			if v.Type() != tag {
				t.Fatalf("mkval(%v,%#x): Type=%v", tag, p, v.Type())
			}
			if v.Data() != (p & nanboxDataMask) {
				t.Fatalf("mkval(%v,%#x): Data=%#x", tag, p, v.Data())
			}
		}
	}
}

func TestImmediates(t *testing.T) {
	if !mkundef().IsUndefined() {
		t.Fatal("undefined")
	}
	if !mknull().IsNull() {
		t.Fatal("null")
	}
	if !mktrue().Bool() || mkfalse().Bool() {
		t.Fatal("bool payload")
	}
	if !mkbool(true).IsBool() || mkbool(false).Type() != TBool {
		t.Fatal("mkbool")
	}
	if !mknull().IsNullish() || !mkundef().IsNullish() || mknum(0).IsNullish() {
		t.Fatal("nullish")
	}
	if !tEmpty.IsEmpty() || mkundef().IsEmpty() {
		t.Fatal("empty sentinel")
	}
}

func TestObjectTypePredicates(t *testing.T) {
	for _, tag := range []Type{TObj, TArr, TFunc, TPromise, TGenerator} {
		if !mkval(tag, 1).IsObjectType() {
			t.Fatalf("%v should be object type", tag)
		}
	}
	for _, tag := range []Type{TStr, TNum, TBool, TUndef, TNull, TSymbol} {
		if mkval(tag, 1).IsObjectType() && tag != TNum {
			t.Fatalf("%v should not be object type", tag)
		}
	}
	if !mkval(TObj, 1).IsSpecialObject() || !mkval(TArr, 1).IsSpecialObject() {
		t.Fatal("special object")
	}
	if mkval(TFunc, 1).IsSpecialObject() {
		t.Fatal("func is not special object")
	}
}
