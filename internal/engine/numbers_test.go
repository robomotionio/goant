package engine

import (
	"math"
	"testing"
)

func TestNumberToString(t *testing.T) {
	cases := []struct {
		d    float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"},
		{1, "1"},
		{-1, "-1"},
		{150, "150"},
		{5, "5"},
		{0.5, "0.5"},
		{0.001, "0.001"},
		{1.5, "1.5"},
		{100, "100"},
		{123456789, "123456789"},
		{1e21, "1e+21"},
		{1e-7, "1e-7"},
		{1e20, "100000000000000000000"},
		{-0.0000001, "-1e-7"},
		{1234.5678, "1234.5678"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{0.1, "0.1"},
		{0.2, "0.2"},
		{123456789012345680000, "123456789012345680000"},
		{1e-6, "0.000001"},
	}
	for _, c := range cases {
		if got := numberToString(c.d); got != c.want {
			t.Errorf("numberToString(%v) = %q want %q", c.d, got, c.want)
		}
	}
	// 0.1+0.2 must be computed at runtime (Go folds the constant to exact 0.3).
	a, b := 0.1, 0.2
	if got := numberToString(a + b); got != "0.30000000000000004" {
		t.Errorf("numberToString(0.1+0.2) = %q want 0.30000000000000004", got)
	}
}

func TestNumberToStringRadix(t *testing.T) {
	cases := []struct {
		d     float64
		radix int
		want  string
	}{
		{255, 16, "ff"},
		{255, 2, "11111111"},
		{8, 8, "10"},
		{35, 36, "z"},
		{-255, 16, "-ff"},
		{10, 10, "10"},
		{0, 16, "0"},
	}
	for _, c := range cases {
		if got := numberToStringRadix(c.d, c.radix); got != c.want {
			t.Errorf("(%v).toString(%d) = %q want %q", c.d, c.radix, got, c.want)
		}
	}
	// 0.5 in binary is 0.1
	if got := numberToStringRadix(0.5, 2); got != "0.1" {
		t.Errorf("(0.5).toString(2) = %q want 0.1", got)
	}
}

func TestStringToNumber(t *testing.T) {
	cases := []struct {
		s    string
		want float64
	}{
		{"", 0},
		{"   ", 0},
		{"42", 42},
		{"  42  ", 42},
		{"3.14", 3.14},
		{"-5", -5},
		{"+5", 5},
		{"0x1F", 31},
		{"0b101", 5},
		{"0o17", 15},
		{"Infinity", math.Inf(1)},
		{"-Infinity", math.Inf(-1)},
		{"1e3", 1000},
		{".5", 0.5},
		{"5.", 5},
	}
	for _, c := range cases {
		got := stringToNumber(c.s)
		if got != c.want {
			t.Errorf("stringToNumber(%q) = %v want %v", c.s, got, c.want)
		}
	}
	// Invalid inputs → NaN.
	for _, s := range []string{"abc", "12abc", "1_000", "0x", "0xG", "1.2.3", "--5", "0x1.5"} {
		if got := stringToNumber(s); !math.IsNaN(got) {
			t.Errorf("stringToNumber(%q) = %v want NaN", s, got)
		}
	}
}

func TestCoercions(t *testing.T) {
	rt := New()
	// ToBoolean
	if rt.toBoolean(mkundef()) || rt.toBoolean(mknull()) || rt.toBoolean(mknum(0)) ||
		rt.toBoolean(mknum(math.NaN())) || rt.toBoolean(rt.newString("")) {
		t.Error("falsy values reported truthy")
	}
	if !rt.toBoolean(mknum(1)) || !rt.toBoolean(rt.newString("x")) || !rt.toBoolean(mktrue()) {
		t.Error("truthy values reported falsy")
	}
	// ToNumber
	if n, _ := rt.toNumberPrimitive(rt.newString("42")); n != 42 {
		t.Error("ToNumber string")
	}
	if n, _ := rt.toNumberPrimitive(mknull()); n != 0 {
		t.Error("ToNumber null")
	}
	if n, _ := rt.toNumberPrimitive(mkundef()); !math.IsNaN(n) {
		t.Error("ToNumber undefined")
	}
	// ToString
	s, _ := rt.toStringPrimitive(mknum(1.5))
	if rt.strGo(s) != "1.5" {
		t.Errorf("ToString number = %q", rt.strBytes(s))
	}
	s, _ = rt.toStringPrimitive(mknull())
	if rt.strGo(s) != "null" {
		t.Error("ToString null")
	}
}

func TestSameValueAndStrictEquals(t *testing.T) {
	rt := New()
	nan := mknum(math.NaN())
	pz, nz := mknum(0), mknum(math.Copysign(0, -1))

	if !rt.sameValue(nan, nan) {
		t.Error("SameValue(NaN,NaN) should be true")
	}
	if rt.sameValue(pz, nz) {
		t.Error("SameValue(+0,-0) should be false")
	}
	if !rt.sameValueZero(pz, nz) {
		t.Error("SameValueZero(+0,-0) should be true")
	}
	if rt.strictEquals(nan, nan) {
		t.Error("NaN === NaN should be false")
	}
	if !rt.strictEquals(pz, nz) {
		t.Error("+0 === -0 should be true")
	}
	// String equality by content, not handle.
	a := rt.newString("hi")
	b := rt.newString("hi")
	if !rt.strictEquals(a, b) || !rt.sameValue(a, b) {
		t.Error("equal strings should compare equal")
	}
}
