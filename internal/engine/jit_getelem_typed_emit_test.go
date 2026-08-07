//go:build amd64 || arm64

package engine

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// The compiled TypedArray element read, checked against the interpreter.
//
// This chain is emitted from TYPE FEEDBACK rather than from the bytecode, which
// makes the test different in kind from the fast-array one next to it. There,
// compiling the function is enough to get the chain; here the function has to be
// RUN first, because the byte that decides which load is emitted is written by
// the interpreter. A test that compiled directly would exercise nothing — the
// site would have no record and would be given the fast-array chain instead —
// so every case below goes through a whole program.
//
// The three things that can be wrong, and are each covered on purpose:
//
//   - The load. Nine kinds, and the difference between two of them is one
//     instruction: an Int8Array holding -1 must read back as -1 and a
//     Uint8Array holding the same byte as 255. Every kind is filled with values
//     that are wrong under a neighbouring kind's load.
//   - The bound. A detached buffer keeps the view's length and loses its bytes;
//     a resizable buffer can shrink under a fixed view, which puts the WHOLE
//     view out of bounds rather than its tail.
//   - The box. A Float32Array or Float64Array element is already a double, and
//     a negative NaN in one has bits that would read as a tagged value.

// taKindNames is the constructor for each kind the emitter can serve, plus the
// three it must refuse.
var taKindNames = []struct {
	name string
	kind taKind
	vals string // a literal the constructor accepts
}{
	{"Int8Array", taInt8, "[-128, -1, 0, 1, 127]"},
	{"Uint8Array", taUint8, "[0, 1, 127, 128, 255]"},
	{"Uint8ClampedArray", taUint8Clamped, "[0, 1, 127, 128, 255]"},
	{"Int16Array", taInt16, "[-32768, -1, 0, 1, 32767]"},
	{"Uint16Array", taUint16, "[0, 1, 32767, 32768, 65535]"},
	{"Int32Array", taInt32, "[-2147483648, -1, 0, 1, 2147483647]"},
	{"Uint32Array", taUint32, "[0, 1, 2147483647, 2147483648, 4294967295]"},
	{"Float32Array", taFloat32, "[-1.5, 0, 0.5, 1e38, -0]"},
	{"Float64Array", taFloat64, "[-1.5, 0, 0.5, 1e300, -0]"},
	// Not emittable, and the program must still be right.
	{"Float16Array", taFloat16, "[-1.5, 0, 0.5, 1, -0]"},
	{"BigInt64Array", taBigInt64, "[-1n, 0n, 1n, 2n, 3n]"},
	{"BigUint64Array", taBigUint64, "[0n, 1n, 2n, 3n, 4n]"},
}

// jitTypedReadSrc indexes a view over and past its end, hot enough to tier, and
// reports every result as a string so a wrong kind cannot be hidden by a
// coincidence in one slot.
func jitTypedReadSrc(ctor, vals string) string {
	return fmt.Sprintf(`
		function f(a, i) { return a[i]; }
		var a = new %s(%s);
		var out = "";
		for (var k = 0; k < 400; k++) {
			out = "";
			for (var i = -2; i < 8; i++) out += "|" + f(a, i);
			out += "|" + f(a, 0.5) + "|" + f(a, NaN) + "|" + f(a, "1") + "|" + f(a, -0);
		}
		out;`, ctor, vals)
}

func TestJITTypedElementAgreesWithTheInterpreter(t *testing.T) {
	for _, k := range taKindNames {
		t.Run(k.name, func(t *testing.T) {
			jitBothWays(t, k.name+".js", jitTypedReadSrc(k.name, k.vals))
		})
	}
}

// TestJITTypedElementSeesTheBufferChange is the guard that is not a shape.
//
// A view has no cache to invalidate: the bytes it reads belong to a buffer that
// can be detached or resized while the compiled site is bound to nothing but the
// element kind. Each step here breaks the window a different way, and the answer
// after it must be undefined rather than a stale element.
func TestJITTypedElementSeesTheBufferChange(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"a-detached-buffer", `
			function f(a, i) { return a[i]; }
			var b = new ArrayBuffer(16), a = new Int32Array(b);
			var out = "";
			for (var k = 0; k < 400; k++) out = "" + f(a, 1);
			b.transfer();
			out + "|" + f(a, 0) + "|" + f(a, 1) + "|" + a.length;`},
		{"a-resizable-buffer-that-shrank", `
			function f(a, i) { return a[i]; }
			var b = new ArrayBuffer(16, {maxByteLength: 32});
			var a = new Int32Array(b, 0, 4);
			a[0] = 7; a[3] = 9;
			var out = "";
			for (var k = 0; k < 400; k++) out = "" + f(a, 0) + "," + f(a, 3);
			b.resize(8);
			out + "|" + f(a, 0) + "|" + f(a, 3) + "|" + a.length;`},
		{"a-resizable-buffer-that-grew-back", `
			function f(a, i) { return a[i]; }
			var b = new ArrayBuffer(16, {maxByteLength: 32});
			var a = new Int32Array(b, 0, 4);
			a[2] = 5;
			var out = "";
			for (var k = 0; k < 400; k++) out = "" + f(a, 2);
			b.resize(8);
			var mid = "" + f(a, 2);
			b.resize(16);
			out + "|" + mid + "|" + f(a, 2);`},
		{"a-length-tracking-view", `
			function f(a, i) { return a[i]; }
			var b = new ArrayBuffer(16, {maxByteLength: 32});
			var a = new Int32Array(b);
			a[0] = 1; a[3] = 4;
			var out = "";
			for (var k = 0; k < 400; k++) out = "" + f(a, 0) + "," + f(a, 3);
			b.resize(8);
			var mid = "" + f(a, 0) + "," + f(a, 3);
			b.resize(32);
			out + "|" + mid + "|" + f(a, 3) + "|" + f(a, 7);`},
		{"a-view-at-a-byte-offset", `
			function f(a, i) { return a[i]; }
			var b = new ArrayBuffer(32);
			var whole = new Int32Array(b);
			for (var i = 0; i < 8; i++) whole[i] = i * 11;
			var a = new Int32Array(b, 12, 4);
			var out = "";
			for (var k = 0; k < 400; k++) { out = ""; for (var i = -1; i < 5; i++) out += "|" + f(a, i); }
			out;`},
		{"the-same-site-over-two-kinds", `
			function f(a, i) { return a[i]; }
			var x = new Int8Array([-1, 2]), y = new Uint8Array([255, 2]);
			var out = "";
			for (var k = 0; k < 400; k++) out = "" + f(x, 0) + "," + f(y, 0);
			out;`},
		{"the-same-site-over-an-array-and-a-view", `
			function f(a, i) { return a[i]; }
			var x = [1, 2, 3], y = new Int32Array([4, 5, 6]);
			var out = "";
			for (var k = 0; k < 400; k++) out = "" + f(x, 1) + "," + f(y, 1) + "," + f(x, 9) + "," + f(y, 9);
			out;`},
		{"a-nan-element", `
			function f(a, i) { return a[i]; }
			var a = new Float64Array(4), b = new Float32Array(4);
			a[0] = NaN; a[1] = -(0/0); a[2] = Infinity; a[3] = -Infinity;
			b[0] = NaN; b[1] = -(0/0); b[2] = Infinity; b[3] = -Infinity;
			var out = "";
			for (var k = 0; k < 400; k++) {
				out = "";
				for (var i = 0; i < 4; i++) out += "|" + f(a, i) + "," + (f(a, i) !== f(a, i));
				for (var i = 0; i < 4; i++) out += "|" + f(b, i) + "," + (f(b, i) !== f(b, i));
			}
			out;`},
		{"a-view-with-a-prototype-index", `
			function f(a, i) { return a[i]; }
			var a = new Int32Array([1, 2]);
			Object.setPrototypeOf(a, {5: "inherited"});
			var out = "";
			for (var k = 0; k < 400; k++) out = "" + f(a, 1) + "," + f(a, 5);
			out;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestJITTypedElementChainActuallyRuns is what stops the agreement above from
// being the runtime agreeing with itself.
//
// The counter is the only thing that can tell an emitted chain that answers from
// a chain that falls through to the helper on every access, and the whole point
// of this work is which of the two is happening. A read that must NOT be served
// is checked in the same run, because a chain that answered everything would
// also make the hit count go up.
func TestJITTypedElementChainActuallyRuns(t *testing.T) {
	was, wasEnabled := jitEnabled, jitStats.enabled
	jitEnabled, jitStats.enabled = true, true
	defer func() { jitEnabled, jitStats.enabled = was, wasEnabled }()

	for _, k := range taKindNames {
		emittable := jitElemKindEmittable(k.kind)
		t.Run(k.name, func(t *testing.T) {
			rt := New()
			hit0 := jitStats.elemHit
			// 200 warm-up reads that the chain cannot serve either way, so the
			// count below is only the 200 in-range ones.
			_, err := rt.RunString("hot.js", fmt.Sprintf(`
				function f(a, i) { return a[i]; }
				var a = new %s(%s);
				for (var k = 0; k < 200; k++) { f(a, 1); f(a, 99); }
				1;`, k.name, k.vals))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			hits := jitStats.elemHit - hit0
			switch {
			case emittable && hits == 0:
				t.Errorf("%s: the emitted chain served none of 200 in-range reads", k.name)
			case emittable && hits > 200:
				t.Errorf("%s: served %d of 200 in-range reads, so it also served the out-of-range ones",
					k.name, hits)
			case !emittable && hits != 0:
				t.Errorf("%s: served %d reads of a kind no chain can load", k.name, hits)
			}
		})
	}
}

// TestJITTypedElementFeedbackPicksTheChain pins the decision itself: a site that
// only ever sees fast arrays must keep the fast-array chain, and one that only
// ever sees views must get the other. Getting this backwards is not a wrong
// answer — both chains are correct — it is a permanent exit at the site, which
// no comparison against the interpreter would ever notice.
func TestJITTypedElementFeedbackPicksTheChain(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want uint8
	}{
		{"only-fast-arrays", "var a = [1,2,3]; for (var k=0;k<400;k++) f(a, 1);", elemKindArr},
		{"only-views", "var a = new Int32Array([1,2,3]); for (var k=0;k<400;k++) f(a, 1);",
			uint8(taInt32) + 1},
		{"both", `var a = [1,2,3], b = new Int32Array([1,2,3]);
			for (var k=0;k<400;k++) { f(a, 1); f(b, 1); }`, elemKindPoly},
		{"a-tracking-view", `var buf = new ArrayBuffer(16, {maxByteLength: 32});
			var a = new Int32Array(buf); for (var k=0;k<400;k++) f(a, 1);`, elemKindPoly},
		{"neither", "var a = {1: 'x'}; for (var k=0;k<400;k++) f(a, 1);", elemKindNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was, wasT := jitEnabled, jitThreshold
			jitEnabled = true
			// The threshold is pinned rather than inherited, because what is
			// being measured is the record the INTERPRETER builds and compiling
			// stops it: a compiled site records nothing further. With
			// GOANT_JIT_THRESHOLD=2 in the environment the "both" case compiled
			// after its first sample and reported a monomorphic site, which is
			// a true statement about a threshold of 2 and says nothing about
			// the decision this test is here to pin.
			jitThreshold = 1 << 30
			defer func() { jitEnabled, jitThreshold = was, wasT }()

			rt := New()
			fnVal, err := rt.RunString("pick.js",
				"function f(a, i) { return a[i]; }\n"+tc.src+"\nf;")
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			o := rt.objPtr(fnVal)
			if o == nil || o.clPtr == nil {
				t.Fatal("no function f")
			}
			fn := o.clPtr.fn
			got := elemKindNone
			for i, k := range fn.elemKinds {
				if k != elemKindNone {
					if got != elemKindNone {
						t.Fatalf("two recorded sites, at %d and elsewhere", i)
					}
					got = k
				}
			}
			if got != tc.want {
				t.Errorf("recorded %d, want %d", got, tc.want)
			}
		})
	}
}

// TestTypedArrayJITKindMatchesTheEmitter is the agreement the chain rests on: the
// runtime decides which views compiled code may read, and the emitter decides
// which kinds it can load. A kind either side allows and the other does not is
// either a view read by a chain that has no load for it, or a permanently unused
// chain — so they are checked against each other rather than trusted to match.
func TestTypedArrayJITKindMatchesTheEmitter(t *testing.T) {
	rt := New()
	for kind := taInt8; int(kind) < len(taKinds); kind++ {
		name := taKinds[kind].name
		src := fmt.Sprintf("new %s(4)", name)
		if strings.HasPrefix(name, "Big") {
			src = fmt.Sprintf("new %s(4)", name)
		}
		v, err := rt.RunString("k.js", src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ta := rt.objPtr(v).ta
		if got, want := ta.jitKind != 0, jitElemKindEmittable(kind); got != want {
			t.Errorf("%s: runtime says emittable=%v, emitter says %v", name, got, want)
		}
		if ta.jitKind != 0 && ta.jitKind != uint8(kind)+1 {
			t.Errorf("%s: jitKind %d, want %d", name, ta.jitKind, uint8(kind)+1)
		}
		if ta.bufPtr == nil {
			t.Errorf("%s: bufPtr not resolved", name)
		}
	}

	// A tracking view is refused whatever its kind, because its length is
	// recomputed from the buffer on every access.
	v, err := rt.RunString("track.js",
		"new Int32Array(new ArrayBuffer(16, {maxByteLength: 32}))")
	if err != nil {
		t.Fatalf("tracking: %v", err)
	}
	if ta := rt.objPtr(v).ta; !ta.track || ta.jitKind != 0 {
		t.Errorf("tracking view: track=%v jitKind=%d, want true and 0", ta.track, ta.jitKind)
	}
}

// TestJITTypedElementBoundsAreOnTheBytes covers the one shortcut the chain must
// not take. The view's own length is not a bound on the buffer: detaching keeps
// the length and takes the bytes away, so a chain that trusted length would read
// freed memory rather than answer undefined.
func TestJITTypedElementBoundsAreOnTheBytes(t *testing.T) {
	was := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = was }()

	rt := New()
	got, err := rt.RunString("detach.js", `
		function f(a, i) { return a[i]; }
		var b = new ArrayBuffer(16), a = new Int32Array(b);
		for (var k = 0; k < 400; k++) f(a, 1);
		var lenBefore = a.length;
		b.transfer();
		var after = "";
		for (var i = 0; i < 4; i++) after += "|" + f(a, i);
		lenBefore + ":" + a.length + after;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if s := string(rt.strBytes(got)); s != "4:0|undefined|undefined|undefined|undefined" {
		t.Errorf("after detaching: %q", s)
	}
	_ = math.NaN
}
