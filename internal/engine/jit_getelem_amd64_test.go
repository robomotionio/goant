//go:build amd64

package engine

import (
	"math"
	"testing"
)

// The compiled element read, checked against the interpreter.
//
// `a[i]` has no cache site, so unlike every other access this tier emits there
// is no recorded shape to be wrong about — what can be wrong is the guard chain
// itself, and each case below is one link of it. The index test in particular is
// doing more than it looks: converting to an integer and back and requiring the
// same bits is what rejects a fraction, an infinity, a NaN and negative zero all
// at once, and a test that only tried `a[0]` would never know.

// jitSameValue compares by bits, except for two Strings, which it compares by
// content. Indexing a String allocates a fresh one-character String per read, so
// the two paths agreeing on the value cannot mean agreeing on the handle.
func jitSameValue(rt *Runtime, a, b Value) bool {
	if a.Type() == TStr && b.Type() == TStr {
		return string(rt.strBytes(a)) == string(rt.strBytes(b))
	}
	return uint64(a) == uint64(b)
}

// jitElem compiles `function f(a,i){ return a[i]; }`.
func jitElem(t testing.TB) (*Runtime, *svFunc, *jitCode) {
	t.Helper()
	const src = "function f(a,i){ return a[i]; }"
	rt, fn := jitFnRT(t, src)
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatalf("refused to compile %q", src)
	}
	t.Cleanup(c.free)
	return rt, fn, c
}

func jitReadElem(t testing.TB, rt *Runtime, fn *svFunc, c *jitCode, recv, key Value) (Value, *ThrowError) {
	t.Helper()
	locals := make([]Value, fn.maxLocals)
	locals[0], locals[1] = recv, key
	v, e, ok := c.jitRun(rt, fn, nil, 0, nil, locals, mkundef())
	if !ok {
		t.Fatal("compiled code declined arguments it should have handled")
	}
	return v, e
}

func TestJITElementAgreesWithTheInterpreter(t *testing.T) {
	rt, fn, c := jitElem(t)

	// One array with everything in it that an element read has to tell apart.
	arr, err := rt.RunString("arr.js", `
		var a = [10, 20, 30];
		a[5] = 60;          // leaves a hole at 3 and 4
		a.name = "named";   // a named property on an array
		a;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sparse, err := rt.RunString("sparse.js", "var s = []; s[2] = 7; s;")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	proto, err := rt.RunString("proto.js", `
		var p = [1]; Object.setPrototypeOf(p, {1: "inherited"}); p;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	str, _ := rt.RunString("str.js", "'abc'")
	obj, _ := rt.RunString("obj.js", "({0: 'zero', 1: 'one'})")
	ta, _ := rt.RunString("ta.js", "new Int32Array([4, 5, 6])")
	frozen, _ := rt.RunString("frozen.js", "Object.freeze([1, 2])")

	keys := []Value{
		tov(0), tov(1), tov(2), tov(3), tov(4), tov(5), tov(6), tov(100),
		tov(-1), tov(-0), tov(0.5), tov(1.5), tov(1e10), tov(1e300),
		tov(2147483647), tov(2147483648), tov(4294967295), tov(4294967296),
		tov(math.NaN()), tov(math.Inf(1)), tov(math.Inf(-1)),
		rt.newString("0"), rt.newString("1"), rt.newString("name"),
		rt.newString("length"), rt.newString("nope"),
		mkundef(), mknull(), mkbool(true),
	}
	for _, tc := range []struct {
		name string
		recv Value
	}{
		{"a-fast-array", arr},
		{"a-sparse-array", sparse},
		{"an-array-with-a-prototype", proto},
		{"a-frozen-array", frozen},
		{"a-string", str},
		{"a-plain-object", obj},
		{"a-typed-array", ta},
		{"a-number", tov(7)},
		{"undefined", mkundef()},
		{"null", mknull()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range keys {
				want, wantErr := rt.getElement(tc.recv, k)
				got, gotErr := jitReadElem(t, rt, fn, c, tc.recv, k)
				if (wantErr == nil) != (gotErr == nil) {
					t.Fatalf("key %v: compiled threw %v, runtime threw %v",
						k, gotErr != nil, wantErr != nil)
				}
				if wantErr == nil && !jitSameValue(rt, got, want) {
					t.Errorf("key %v: compiled %#x (%v), runtime %#x (%v)",
						k, uint64(got), got.Type(), uint64(want), want.Type())
				}
			}
		})
	}
}

// TestJITElementProbeActuallyRuns is what stops the agreement above from being
// the runtime agreeing with itself, which is how it would agree if every guard
// were inverted.
func TestJITElementProbeActuallyRuns(t *testing.T) {
	was := jitStats.enabled
	jitStats.enabled = true
	defer func() { jitStats.enabled = was }()

	rt, fn, c := jitElem(t)
	arr, err := rt.RunString("arr.js", "[10, 20, 30, 40]")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	hit0, miss0 := jitStats.elemHit, jitStats.elemMiss
	const runs = 32
	for i := 0; i < runs; i++ {
		got, e := jitReadElem(t, rt, fn, c, arr, tov(float64(i%4)))
		if e != nil || got != tov(float64((i%4+1)*10)) {
			t.Fatalf("run %d returned %v", i, got)
		}
	}
	hits, misses := jitStats.elemHit-hit0, jitStats.elemMiss-miss0
	if hits != runs || misses != 0 {
		t.Errorf("%d hits and %d misses over %d in-range reads of a fast array; every one should be served",
			hits, misses, runs)
	}
}

// TestJITElementDeclinesWhatItCannotServe pins the other half: the guards have
// to send these to the runtime rather than answer them, and the counter is the
// only thing that can tell the difference.
func TestJITElementDeclinesWhatItCannotServe(t *testing.T) {
	was := jitStats.enabled
	jitStats.enabled = true
	defer func() { jitStats.enabled = was }()

	rt, fn, c := jitElem(t)
	arr, err := rt.RunString("arr.js", "var a = [10, 20]; a[4] = 50; a;")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, tc := range []struct {
		name string
		key  Value
	}{
		{"a-hole", tov(2)},
		{"past-the-end", tov(9)},
		{"negative", tov(-1)},
		{"a-fraction", tov(1.5)},
		{"not-a-number", rt.newString("0")},
		{"nan", tov(math.NaN())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit0 := jitStats.elemHit
			if _, e := jitReadElem(t, rt, fn, c, arr, tc.key); e != nil {
				t.Fatal("threw")
			}
			if jitStats.elemHit != hit0 {
				t.Errorf("the probe served %v, which it cannot answer correctly", tc.key)
			}
		})
	}
}

// TestJITElementSeesAMutatedArray is the guard that is not a shape: an array's
// length and its storage both change under a site that has no cache to
// invalidate, so every read has to look at both.
func TestJITElementSeesAMutatedArray(t *testing.T) {
	rt, fn, c := jitElem(t)
	arr, err := rt.RunString("arr.js", "[1, 2, 3]")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	check := func(step string) {
		t.Helper()
		for i := -1; i < 8; i++ {
			k := tov(float64(i))
			want, _ := rt.getElement(arr, k)
			got, e := jitReadElem(t, rt, fn, c, arr, k)
			if e != nil {
				t.Fatalf("%s: index %d threw", step, i)
			}
			if uint64(got) != uint64(want) {
				t.Fatalf("%s: index %d compiled %#x, runtime %#x",
					step, i, uint64(got), uint64(want))
			}
		}
	}
	check("as built")
	rt.RunString("grow.js", "a = null;")
	rt.setField(arr, "5", tov(60))
	check("after a hole was punched by growing")
	rt.setField(arr, "length", tov(2))
	check("after the length was cut")
	rt.setField(arr, "length", tov(9))
	check("after the length was extended past the storage")
}

// TestJITElementWriteAgreesWithTheInterpreter covers `a[i] = v` and the stack
// shuffle in front of it.
//
// The write goes to the runtime rather than being emitted — everything the
// read's guard chain establishes has to hold, plus writability and
// extensibility — so what is being checked here is the operand plumbing:
// INSERT3 rotates four registers, and getting that wrong stores the index into
// the array or the array into the slot.
func TestJITElementWriteAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"in-range-and-past-the-end", `
			function f(a, i, v) { a[i] = v; return a[i]; }
			var out = "";
			for (var k = 0; k < 200; k++) {
				var arr = [1, 2, 3];
				out = "" + f(arr, 0, 9) + f(arr, 2, 8) + f(arr, 7, 7) + arr.length;
			}
			out;`},
		{"used-as-an-expression", `
			function f(a, i) { var t = (a[i] = i * 2); return t + a[i]; }
			var s = 0, arr = [0, 0, 0, 0];
			for (var k = 0; k < 200; k++) s = f(arr, k % 4);
			s;`},
		{"a-frozen-array-in-sloppy-mode", `
			function f(a, i, v) { a[i] = v; return a[i]; }
			var out = "";
			for (var k = 0; k < 200; k++) { var arr = Object.freeze([1, 2]); out = "" + f(arr, 0, 9); }
			out;`},
		{"a-frozen-array-in-strict-mode", `
			function f(a, i, v) { "use strict"; a[i] = v; return a[i]; }
			var c = 0;
			for (var k = 0; k < 200; k++) {
				var arr = Object.freeze([1, 2]);
				try { f(arr, 0, 9); } catch (e) { c++; }
			}
			c;`},
		{"a-plain-object-and-a-string-key", `
			function f(o, k, v) { o[k] = v; return o[k]; }
			var out = "";
			for (var k = 0; k < 200; k++) { var o = {}; out = "" + f(o, "x", 1) + f(o, 2, 3) + o.x; }
			out;`},
		{"a-global-assignment", `
			var g = 0;
			function f(v) { g = v; return g; }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
		{"a-strict-global-that-does-not-exist", `
			function f(v) { "use strict"; nope = v; return 1; }
			var c = 0; for (var k = 0; k < 200; k++) { try { f(k); } catch (e) { c++; } } c;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}
