package engine

import (
	"math"
	"testing"
)

func TestGlobalConstants(t *testing.T) {
	rt := New()
	v, err := rt.RunString("t.js", "NaN;")
	if err != nil || !math.IsNaN(v.Number()) {
		t.Errorf("NaN = %v, %v", v.Number(), err)
	}
	v, _ = rt.RunString("t.js", "Infinity;")
	if !math.IsInf(v.Number(), 1) {
		t.Errorf("Infinity = %v", v.Number())
	}
	v, _ = rt.RunString("t.js", "-Infinity;")
	if !math.IsInf(v.Number(), -1) {
		t.Errorf("-Infinity = %v", v.Number())
	}
}

func TestGlobalVarRoundTrip(t *testing.T) {
	runNum(t, "var x = 5; x;", 5)
	runNum(t, "var x = 10; x = x + 5; x;", 15)
	runNum(t, "var a = 1; var b = 2; a + b;", 3)
	runNum(t, "var x = 3; x *= 4; x;", 12)
}

func TestGlobalThis(t *testing.T) {
	rt := New()
	v, err := rt.RunString("t.js", "globalThis;")
	if err != nil {
		t.Fatal(err)
	}
	if v != rt.global {
		t.Error("globalThis should be the global object")
	}
}

func TestNaNIsImmutable(t *testing.T) {
	// Assigning to NaN in sloppy mode silently fails (non-writable global).
	runNum(t, "NaN = 5; var x = NaN; x - x;", math.NaN())
}

func TestUndefinedGlobal(t *testing.T) {
	rt := New()
	v, err := rt.RunString("t.js", "undefined;")
	if err != nil {
		t.Fatal(err)
	}
	if !v.IsUndefined() {
		t.Error("undefined global should be undefined")
	}
}
