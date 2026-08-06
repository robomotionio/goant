//go:build amd64 || arm64

package engine

import (
	"fmt"
	"testing"
)

// The compiled TypedArray element store, checked against the interpreter.
//
// The store's guard chain is the read's, shared rather than copied, so what is
// new here is the VALUE — and the value is where a store can be wrong in ways a
// read cannot:
//
//   - Truncation. Storing 300 into a Uint8Array must leave 44, not 255 and not
//     0: the spec's answer is the value modulo the width, which is the low bits
//     of an exact integer and nothing else.
//   - Sign. -1 into an Int8Array and 255 into a Uint8Array are the same byte,
//     and reading either back has to give what that view says it is.
//   - The inputs the guard is deliberately narrow about. A fraction, a NaN, an
//     infinity and a magnitude past int64 all have spec answers, and the chain
//     answers none of them — it hands them to the runtime. If it ever answered
//     one itself it would answer it WRONGLY, and differently on the two
//     backends: amd64's convert reports failure as INT64_MIN and arm64's
//     saturates, so +Infinity into an Int32Array would be 0 on one and -1 on the
//     other. The cases below are the ones that would catch it.
//   - Precision. A Float32Array store narrows, so 0.1 comes back as 0.1's
//     nearest float32 and not as 0.1.

// jitTypedWriteSrc stores a range of values through a view and reads every slot
// back, hot enough to tier.
func jitTypedWriteSrc(ctor string, vals string) string {
	return fmt.Sprintf(`
		function w(a, i, v) { a[i] = v; }
		function r(a, i) { return a[i]; }
		var a = new %s(6);
		var vals = %s;
		var out = "";
		for (var k = 0; k < 400; k++) {
			out = "";
			for (var j = 0; j < vals.length; j++) {
				for (var i = -1; i < 7; i++) w(a, i, vals[j]);
				for (var i = 0; i < 6; i++) out += "|" + r(a, i);
			}
		}
		out;`, ctor, vals)
}

func TestJITTypedElementStoreAgreesWithTheInterpreter(t *testing.T) {
	// Values chosen so that every one of them is wrong under a neighbouring
	// kind's store, and so that the four the chain must refuse are present for
	// every kind rather than only the float ones.
	const vals = `[0, 1, -1, 127, 128, 255, 256, 300, -128, -129, 32767, 32768, 65535, 65536,
		2147483647, 2147483648, 4294967295, 4294967296, -2147483648, -2147483649,
		0.5, -0.5, 1.5, -0, NaN, Infinity, -Infinity, 1e300, -1e300, 9.5e18, 1e21,
		0.1, 1/3, 5e-324, 1.7976931348623157e308]`
	for _, k := range taKindNames {
		if k.kind == taBigInt64 || k.kind == taBigUint64 {
			continue // a Number stored into a BigInt view throws, covered below
		}
		t.Run(k.name, func(t *testing.T) {
			jitBothWays(t, k.name+"-store.js", jitTypedWriteSrc(k.name, vals))
		})
	}
}

// TestJITTypedElementStoreEdgeCases covers what the value guard is not, and what
// the window guard has to keep doing for a write.
func TestJITTypedElementStoreEdgeCases(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"a-detached-buffer", `
			function w(a, i, v) { a[i] = v; }
			var b = new ArrayBuffer(16), a = new Int32Array(b);
			for (var k = 0; k < 400; k++) w(a, 1, k);
			b.transfer();
			w(a, 0, 5); w(a, 1, 6);
			"" + a.length + "|" + a[0] + "|" + a[1];`},
		{"a-resizable-buffer-that-shrank", `
			function w(a, i, v) { a[i] = v; }
			var b = new ArrayBuffer(16, {maxByteLength: 32});
			var a = new Int32Array(b, 0, 4);
			for (var k = 0; k < 400; k++) w(a, 0, k);
			b.resize(8);
			w(a, 0, 99);
			var mid = "" + a[0] + "|" + a.length;
			b.resize(16);
			mid + "|" + a[0];`},
		{"an-out-of-range-store-is-silent-in-strict-mode", `
			function w(a, i, v) { "use strict"; a[i] = v; return 1; }
			var a = new Int32Array(2), c = 0, ok = 0;
			for (var k = 0; k < 400; k++) { try { ok += w(a, 9, k); } catch (e) { c++; } }
			c + "|" + ok + "|" + a.length;`},
		{"a-non-number-value-still-coerces", `
			function w(a, i, v) { a[i] = v; }
			var a = new Int32Array(4);
			for (var k = 0; k < 400; k++) { w(a, 0, "7"); w(a, 1, true); w(a, 2, null); w(a, 3, {valueOf: function(){return 42;}}); }
			"" + a[0] + "|" + a[1] + "|" + a[2] + "|" + a[3];`},
		{"a-throwing-valueOf-still-throws", `
			function w(a, i, v) { a[i] = v; }
			var a = new Int32Array(2), c = 0;
			var bad = {valueOf: function(){ throw new Error("no"); }};
			for (var k = 0; k < 400; k++) { try { w(a, 0, k); w(a, 0, bad); } catch (e) { c++; } }
			"" + c + "|" + a[0];`},
		{"a-throwing-valueOf-out-of-range-still-throws", `
			function w(a, i, v) { a[i] = v; }
			var a = new Int32Array(2), c = 0;
			var bad = {valueOf: function(){ throw new Error("no"); }};
			for (var k = 0; k < 400; k++) { try { w(a, 99, k); w(a, 99, bad); } catch (e) { c++; } }
			"" + c;`},
		{"a-bigint-view-rejects-a-number", `
			function w(a, i, v) { a[i] = v; }
			var a = new BigInt64Array(2), c = 0;
			for (var k = 0; k < 400; k++) { try { w(a, 0, 1); } catch (e) { c++; } }
			w(a, 0, 7n);
			c + "|" + a[0];`},
		{"the-same-site-over-an-array-and-a-view", `
			function w(a, i, v) { a[i] = v; }
			var x = [0, 0, 0], y = new Int32Array(3);
			for (var k = 0; k < 400; k++) { w(x, 1, k); w(y, 1, k); w(x, 9, k); w(y, 9, k); }
			"" + x[1] + "|" + y[1] + "|" + x[9] + "|" + y[9] + "|" + x.length;`},
		{"a-store-used-as-an-expression", `
			function w(a, i, v) { return (a[i] = v); }
			var a = new Uint8Array(4), out = "";
			for (var k = 0; k < 400; k++) out = "" + w(a, 0, 300) + "|" + a[0] + "|" + w(a, 9, 300);
			out;`},
		{"a-view-at-a-byte-offset", `
			function w(a, i, v) { a[i] = v; }
			var b = new ArrayBuffer(32);
			var whole = new Int32Array(b), a = new Int32Array(b, 12, 4);
			for (var k = 0; k < 400; k++) { for (var i = -1; i < 5; i++) w(a, i, i + 100); }
			var out = "";
			for (var i = 0; i < 8; i++) out += "|" + whole[i];
			out;`},
		{"a-frozen-view", `
			function w(a, i, v) { a[i] = v; }
			var a = new Int32Array(2), c = 0;
			try { Object.freeze(a); } catch (e) { c++; }
			for (var k = 0; k < 400; k++) w(a, 0, k);
			c + "|" + a[0];`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestJITTypedElementStoreChainActuallyRuns is the counter check: the agreement
// above would hold just as well if nothing were emitted at all, because the
// runtime would then be agreeing with itself.
func TestJITTypedElementStoreChainActuallyRuns(t *testing.T) {
	was, wasEnabled := jitEnabled, jitStats.enabled
	jitEnabled, jitStats.enabled = true, true
	defer func() { jitEnabled, jitStats.enabled = was, wasEnabled }()

	for _, k := range taKindNames {
		emittable := jitStoreKindEmittable(k.kind)
		val := "3"
		if k.kind == taBigInt64 || k.kind == taBigUint64 {
			val = "3n"
		}
		t.Run(k.name, func(t *testing.T) {
			rt := New()
			hit0 := jitStats.elemPutHit
			// 200 in-range stores and 200 out-of-range ones, so a chain that
			// served everything is as visible as one that served nothing.
			_, err := rt.RunString("hot.js", fmt.Sprintf(`
				function w(a, i, v) { a[i] = v; }
				var a = new %s(4);
				for (var k = 0; k < 200; k++) { w(a, 1, %s); w(a, 99, %s); }
				1;`, k.name, val, val))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			hits := jitStats.elemPutHit - hit0
			switch {
			case emittable && hits == 0:
				t.Errorf("%s: the emitted chain served none of 200 in-range stores", k.name)
			case emittable && hits > 200:
				t.Errorf("%s: served %d of 200 in-range stores, so it also served the out-of-range ones",
					k.name, hits)
			case !emittable && hits != 0:
				t.Errorf("%s: served %d stores of a kind no chain can write", k.name, hits)
			}
		})
	}
}

// TestJITTypedElementStoreRefusesWhatItCannotConvert pins the value guard.
//
// Each of these has a spec answer the emitted store does not produce, so it has
// to reach the runtime — and the counter is the only way to tell "went to the
// runtime and got it right" from "answered it and happened to agree".
func TestJITTypedElementStoreRefusesWhatItCannotConvert(t *testing.T) {
	was, wasEnabled := jitEnabled, jitStats.enabled
	jitEnabled, jitStats.enabled = true, true
	defer func() { jitEnabled, jitStats.enabled = was, wasEnabled }()

	for _, tc := range []struct{ name, val string }{
		{"a-fraction", "1.5"},
		{"nan", "NaN"},
		{"positive-infinity", "Infinity"},
		{"negative-infinity", "-Infinity"},
		{"past-int64", "1e300"},
		{"a-string", `"3"`},
		{"a-boolean", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := New()
			// Warm and compile on a value the chain does serve, then measure
			// only the stores of the value it must not.
			if _, err := rt.RunString("warm.js", `
				function w(a, i, v) { a[i] = v; }
				var a = new Int32Array(4);
				for (var k = 0; k < 400; k++) w(a, 1, k);
				globalThis.w = w; globalThis.a = a; 1;`); err != nil {
				t.Fatalf("warm: %v", err)
			}
			hit0 := jitStats.elemPutHit
			if _, err := rt.RunString("cold.js",
				fmt.Sprintf("for (var k = 0; k < 100; k++) w(a, 1, %s); 1;", tc.val)); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := jitStats.elemPutHit - hit0; got != 0 {
				t.Errorf("the chain served %d stores of %s, which it cannot convert correctly",
					got, tc.val)
			}
		})
	}
}

// The compiled store maintains the invocation-dirty pair, and the interpreted
// one is what it has to agree with.
//
// This is the failure mode a tier introduces that no correctness test finds: both
// tiers store the same bytes, both answer the same value, and the only difference
// is a flag nothing in the program can read. A host pooling Runtimes reads it,
// and recycles one whose compiled code quietly edited state the next message
// inherits.
//
// Warmed past the threshold so the store is genuinely emitted — jitStats confirms
// the chain served it, because a test that fell back to the helper would pass
// while proving nothing.
func TestCompiledStoreToAnOlderViewIsNoticed(t *testing.T) {
	was, wasEnabled := jitEnabled, jitStats.enabled
	jitEnabled, jitStats.enabled = true, true
	defer func() { jitEnabled, jitStats.enabled = was, wasEnabled }()

	for _, tc := range []struct {
		name  string
		src   string
		dirty bool
	}{
		{"a view older than the invocation", `for (var k = 0; k < 400; k++) w(view, 1, k); "ok"`, true},
		{"a view it made itself", `var v = new Int32Array(4);
			for (var k = 0; k < 400; k++) w(v, 1, k); "ok"`, false},
		{"an out-of-range store into an older view", `
			for (var k = 0; k < 400; k++) w(view, 99, k); "ok"`, true},
	} {
		rt := New()
		if _, err := rt.RunString("pre.js", `
			function w(a, i, v) { a[i] = v; }
			globalThis.w = w;
			globalThis.view = new Int32Array(4);
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
		// The in-range cases must actually have been served by the emitted chain,
		// or this test is measuring the helper.
		if tc.name != "an out-of-range store into an older view" &&
			jitStats.elemPutHit == hit0 {
			t.Errorf("%s: the emitted store never ran, so the flag proves nothing", tc.name)
		}
	}
}
