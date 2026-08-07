//go:build amd64 || arm64

package engine

import "testing"

// The compiled fast-array element store, checked against the interpreter.
//
// Everything the read's chain establishes has to hold, and three things more
// that a TypedArray store never had to answer:
//
//   - The store can be REJECTED. A frozen array's elements are non-writable,
//     silently in sloppy mode and as a TypeError in strict, and both answers
//     belong to the runtime — so the chain has to recognise the case rather than
//     handle it.
//   - The value is a Value, not a number. A handle written into an array older
//     than the invocation is state the next run inherits, so the
//     invocation-dirty pair has to be maintained from machine code.
//   - The slot already holds something, and what it holds decides whether this
//     is a store at all: a hole has to reach the prototype chain, where an
//     inherited setter or a non-writable property may intercept it, and an
//     unparsed JSON span has to be materialised before it can be replaced.
//
// The chain is also deliberately NARROWER than setElementR: it requires the
// index below `length`, so the length can never move and nothing has to
// maintain it. The cases below include the stores that falls through — past the
// end, into the stale region between length and the storage — because being
// narrower is only safe if those actually reach the runtime.

func TestJITArrayElementStoreAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"in-range, and every kind of value", `
			function w(a, i, v) { a[i] = v; }
			var a = [0, 0, 0, 0, 0, 0];
			var vals = [1, -1, 0.5, NaN, Infinity, -0, "s", true, null, undefined, {}, [], function(){}];
			var out = "";
			for (var k = 0; k < 400; k++) {
				out = "";
				for (var j = 0; j < vals.length; j++) { w(a, j % 6, vals[j]); out += "|" + typeof a[j % 6]; }
			}
			out + "|" + a.length;`},
		{"past the end, which grows the array", `
			function w(a, i, v) { a[i] = v; }
			var out = "";
			for (var k = 0; k < 400; k++) {
				var a = [1, 2, 3];
				w(a, 3, 9); w(a, 10, 9);
				out = "" + a.length + "|" + a[3] + "|" + a[10] + "|" + a[5];
			}
			out;`},
		{"into a hole, which the prototype may intercept", `
			function w(a, i, v) { a[i] = v; }
			var out = "";
			for (var k = 0; k < 400; k++) {
				var a = [1, 2, 3]; a[6] = 6;      // holes at 3,4,5
				w(a, 4, 9);
				out = "" + a[4] + "|" + a.length;
			}
			out;`},
		{"into a hole shadowed by an inherited setter", `
			function w(a, i, v) { a[i] = v; }
			var hits = 0;
			var proto = {};
			Object.defineProperty(proto, "4", {set: function (v) { hits++; }, get: function () { return "P"; }});
			var out = "";
			for (var k = 0; k < 400; k++) {
				var a = [1, 2, 3]; a[6] = 6;
				Object.setPrototypeOf(a, proto);
				w(a, 4, 9);
				out = "" + a[4];
			}
			out + "|" + (hits > 0);`},
		{"a frozen array in sloppy mode", `
			function w(a, i, v) { a[i] = v; return a[i]; }
			var out = "";
			for (var k = 0; k < 400; k++) { var a = Object.freeze([1, 2, 3]); out = "" + w(a, 1, 9); }
			out;`},
		{"a frozen array in strict mode", `
			function w(a, i, v) { "use strict"; a[i] = v; }
			var c = 0, ok = 0;
			for (var k = 0; k < 400; k++) {
				var a = Object.freeze([1, 2, 3]);
				try { w(a, 1, 9); ok++; } catch (e) { c++; }
			}
			c + "|" + ok;`},
		{"a sealed array, whose elements ARE writable", `
			function w(a, i, v) { a[i] = v; return a[i]; }
			var out = "";
			for (var k = 0; k < 400; k++) { var a = Object.seal([1, 2, 3]); out = "" + w(a, 1, 9) + "|" + w(a, 7, 9); }
			out;`},
		{"a non-extensible array", `
			function w(a, i, v) { a[i] = v; return a[i]; }
			var out = "";
			for (var k = 0; k < 400; k++) {
				var a = [1, 2, 3]; Object.preventExtensions(a);
				out = "" + w(a, 1, 9) + "|" + w(a, 7, 9) + "|" + a.length;
			}
			out;`},
		{"an index defined non-writable", `
			function w(a, i, v) { a[i] = v; return a[i]; }
			var out = "";
			for (var k = 0; k < 400; k++) {
				var a = [1, 2, 3];
				Object.defineProperty(a, "1", {value: 5, writable: false, configurable: true});
				out = "" + w(a, 1, 9) + "|" + w(a, 0, 9);
			}
			out;`},
		{"the stale region between length and the storage", `
			function w(a, i, v) { a[i] = v; }
			var out = "";
			for (var k = 0; k < 400; k++) {
				var a = [1, 2, 3, 4, 5, 6];
				a.length = 2;                 // 2..5 are stale but still stored
				w(a, 4, 9);
				out = "" + a.length + "|" + a[4];
			}
			out;`},
		{"a length that is not writable", `
			function w(a, i, v) { a[i] = v; }
			var out = "";
			for (var k = 0; k < 400; k++) {
				var a = [1, 2, 3];
				Object.defineProperty(a, "length", {writable: false});
				w(a, 1, 9); w(a, 5, 9);
				out = "" + a.length + "|" + a[1] + "|" + a[5];
			}
			out;`},
		{"the same site over an array and a view", `
			function w(a, i, v) { a[i] = v; }
			var x = [0, 0, 0], y = new Int32Array(3);
			for (var k = 0; k < 400; k++) { w(x, 1, k); w(y, 1, k); }
			"" + x[1] + "|" + y[1];`},
		{"the same site over an array and a plain object", `
			function w(a, i, v) { a[i] = v; }
			var x = [0, 0, 0], o = {};
			for (var k = 0; k < 400; k++) { w(x, 1, k); w(o, 1, k); w(o, "n", k); }
			"" + x[1] + "|" + o[1] + "|" + o.n;`},
		{"a store used as an expression", `
			function w(a, i, v) { return (a[i] = v); }
			var a = [0, 0, 0], out = "";
			for (var k = 0; k < 400; k++) out = "" + w(a, 1, 7) + "|" + a[1] + "|" + w(Object.freeze([1]), 0, 7);
			out;`},
		{"a non-integer or out-of-range key", `
			function w(a, i, v) { a[i] = v; }
			var out = "";
			for (var k = 0; k < 400; k++) {
				var a = [1, 2, 3];
				w(a, 1.5, 9); w(a, -1, 9); w(a, NaN, 9); w(a, "1", 9); w(a, -0, 9); w(a, 4294967295, 9);
				out = "" + a.length + "|" + a[1] + "|" + a[0] + "|" + a["1.5"] + "|" + a[-1];
			}
			out;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// The counter check: every case above would pass just as well if nothing were
// emitted, because the runtime would be agreeing with itself.
//
// Each case warms the site on stores it IS allowed to serve, then measures only
// the ones it is not. Measuring without warming first would prove nothing about
// the ones that must fall through, for a reason worth recording: a store past
// the end GROWS the array, so the second such store is an ordinary in-range one
// and is served correctly. The first draft of this test looped `w(a, 9, k)` over
// a three-element array and reported 293 of 300 served — which was the chain
// being right and the test being wrong.
func TestJITArrayElementStoreChainActuallyRuns(t *testing.T) {
	was, wasEnabled, wasT := jitEnabled, jitStats.enabled, jitThreshold
	// Pinned rather than inherited: the chain is emitted from feedback the
	// INTERPRETER records, so GOANT_JIT_THRESHOLD=1 in the environment compiles
	// before any interpreted pass has run and no site has a kind to emit from.
	// These then reported an empty chain, which was true of the environment and
	// said nothing about the chain. See elemfeedback.go.
	jitEnabled, jitStats.enabled, jitThreshold = true, true, 2
	defer func() {
		jitEnabled, jitStats.enabled, jitThreshold = was, wasEnabled, wasT
	}()

	const warm = `
		function w(a, i, v) { a[i] = v; }
		globalThis.w = w;
		var warmA = [1, 2, 3];
		for (var k = 0; k < 400; k++) w(warmA, 1, k);
		1;`

	for _, tc := range []struct {
		name   string
		src    string
		served bool
	}{
		{"in range, over a live element", `
			var a = [1,2,3]; for (var k=0;k<100;k++) w(a, 1, k);`, true},
		{"past the end", `
			for (var k=0;k<100;k++) { var a = [1,2,3]; w(a, 9, k); }`, false},
		{"into a hole", `
			for (var k=0;k<100;k++) { var a = [1,2,3]; a[9]=9; w(a, 5, k); }`, false},
		{"a frozen array", `
			for (var k=0;k<100;k++) { var a = Object.freeze([1,2,3]); w(a, 1, k); }`, false},
		{"past the length but inside the storage", `
			for (var k=0;k<100;k++) { var a = [1,2,3,4,5,6]; a.length = 2; w(a, 4, k); }`, false},
		{"a fractional key", `
			var a = [1,2,3]; for (var k=0;k<100;k++) w(a, 1.5, k);`, false},
		{"a negative key", `
			var a = [1,2,3]; for (var k=0;k<100;k++) w(a, -1, k);`, false},
		{"a string key", `
			var a = [1,2,3]; for (var k=0;k<100;k++) w(a, "1", k);`, false},
		{"a plain object", `
			var o = {}; for (var k=0;k<100;k++) w(o, 1, k);`, false},
		{"a TypedArray at an array site", `
			var v = new Int32Array(4); for (var k=0;k<100;k++) w(v, 1, k);`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := New()
			if _, err := rt.RunString("warm.js", warm); err != nil {
				t.Fatalf("warm: %v", err)
			}
			hit0 := jitStats.elemPutHit
			if _, err := rt.RunString("s.js", tc.src+"\n1;"); err != nil {
				t.Fatalf("run: %v", err)
			}
			got := jitStats.elemPutHit - hit0
			if tc.served && got == 0 {
				t.Errorf("the emitted chain served none of 100 stores it should answer")
			}
			if !tc.served && got != 0 {
				t.Errorf("the emitted chain served %d of 100 stores it cannot answer correctly", got)
			}
		})
	}
}

// A compiled store into an array older than the invocation has to set Dirty(),
// for the reason the interpreted one does: a host pooling Runtimes reads that
// flag to decide whether the next message can have this one.
//
// Invisible to every test above — both tiers write the same Value and return the
// same result, and the flag is the only thing that differs.
func TestCompiledArrayStoreToAnOlderArrayIsNoticed(t *testing.T) {
	was, wasEnabled, wasT := jitEnabled, jitStats.enabled, jitThreshold
	// Pinned rather than inherited: the chain is emitted from feedback the
	// INTERPRETER records, so GOANT_JIT_THRESHOLD=1 in the environment compiles
	// before any interpreted pass has run and no site has a kind to emit from.
	// These then reported an empty chain, which was true of the environment and
	// said nothing about the chain. See elemfeedback.go.
	jitEnabled, jitStats.enabled, jitThreshold = true, true, 2
	defer func() {
		jitEnabled, jitStats.enabled, jitThreshold = was, wasEnabled, wasT
	}()

	for _, tc := range []struct {
		name  string
		src   string
		dirty bool
	}{
		{"an array older than the invocation", `for (var k = 0; k < 400; k++) w(shared, 1, k); "ok"`, true},
		{"an array it made itself", `var a = [1,2,3];
			for (var k = 0; k < 400; k++) w(a, 1, k); "ok"`, false},
	} {
		rt := New()
		if _, err := rt.RunString("pre.js", `
			function w(a, i, v) { a[i] = v; }
			globalThis.w = w;
			globalThis.shared = [1, 2, 3];
			1;`); err != nil {
			t.Fatalf("%s: pre: %v", tc.name, err)
		}
		hit0 := jitStats.elemPutHit
		inv := rt.BeginInvocation()
		sc, err := rt.CompileScript("d.js", tc.src)
		if err != nil {
			t.Fatalf("%s: compile: %v", tc.name, err)
		}
		if _, err := rt.RunScript(sc); err != nil {
			t.Fatalf("%s: run: %v", tc.name, err)
		}
		got := inv.Dirty()
		inv.End()
		if got != tc.dirty {
			t.Errorf("%s: Dirty() = %v, want %v", tc.name, got, tc.dirty)
		}
		if jitStats.elemPutHit == hit0 {
			t.Errorf("%s: the emitted store never ran, so the flag proves nothing", tc.name)
		}
	}
}
