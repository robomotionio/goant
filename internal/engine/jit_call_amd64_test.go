//go:build amd64

package engine

import "testing"

// What it costs to enter a compiled function, which is the question the Octane
// result asked.
//
// DeltaBlue runs 22.4% of its frame entries as machine code and scores exactly
// what the interpreter scores, so the work inside a compiled frame is not where
// the time is. This measures the other half: the fixed cost of getting into one
// and back out, with a body small enough that it is almost all of the number.
//
// The two arms differ in nothing but `jitEnabled`, so the difference is what
// compiling this function is worth end to end — and how much of it the frame
// setup takes back.
func BenchmarkCallIntoATinyFunction(b *testing.B) {
	const src = "function id(a){ return a + 1; } id;"

	run := func(b *testing.B, on bool) {
		saved := jitEnabled
		jitEnabled = on
		defer func() { jitEnabled = saved }()

		rt := New()
		fnVal, err := rt.RunString("call.js", src)
		if err != nil {
			b.Fatal(err)
		}
		// Warm past the tier's threshold so the measured calls are the steady
		// state rather than the compile.
		args := []Value{tov(1)}
		for i := 0; i < 4*jitThreshold; i++ {
			if _, e := rt.callValue(fnVal, mkundef(), args); e != nil {
				b.Fatal("threw")
			}
		}
		if on {
			o := rt.objPtr(fnVal)
			if o == nil || o.clPtr == nil || o.clPtr.fn.jit.code == nil {
				b.Fatal("id did not compile, so this measures nothing")
			}
		}
		b.ResetTimer()
		for b.Loop() {
			if _, e := rt.callValue(fnVal, mkundef(), args); e != nil {
				b.Fatal("threw")
			}
		}
	}

	b.Run("interpreted", func(b *testing.B) { run(b, false) })
	b.Run("compiled", func(b *testing.B) { run(b, true) })
}

// TestCompiledFrameEntryKeepsTheFrameContract is what runCompiledFrame has to
// earn by skipping most of the interpreter's frame entry.
//
// Each case is one thing the full path does that the short one either has to do
// as well or is allowed to leave out because jitEligible already refused the
// function. Run both ways and compare, because the failures here are quiet: a
// receiver bound wrong reads undefined, and a new.target left in place is
// visible only to whatever the compiled body calls next.
func TestCompiledFrameEntryKeepsTheFrameContract(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		// new.target is consumed by the frame that was constructed, so an
		// ordinary call the constructor makes must not see it.
		{"new-target-is-consumed", `
			function inner() { return typeof new.target; }
			function Outer(v) { this.v = v; this.saw = inner(); }
			var s = "";
			for (var k = 0; k < 200; k++) { var p = new Outer(k); s = p.v + ":" + p.saw; }
			s;`},
		// The same function reached by `new` and by an ordinary call.
		{"constructed-and-called", `
			function P(a) { this.a = a; }
			var s = "";
			for (var k = 0; k < 200; k++) { s = (new P(k)).a + "/" + typeof P.call({}, k); }
			s;`},
		// A sloppy function's nullish receiver becomes the global object, and a
		// primitive one is boxed. Both are done by the short path by hand.
		{"sloppy-receiver-is-bound", `
			function f() { return typeof this; }
			var s = "";
			for (var k = 0; k < 200; k++) s = f() + "," + f.call(1) + "," + f.call("x") + "," + f.call(null);
			s;`},
		{"strict-receiver-is-left-alone", `
			function f() { "use strict"; return typeof this; }
			var s = "";
			for (var k = 0; k < 200; k++) s = f() + "," + f.call(1) + "," + f.call(null);
			s;`},
		// Fewer and more arguments than parameters, since the short path copies
		// them itself.
		{"argument-count", `
			function f(a, b, c) { return "" + a + "," + b + "," + c; }
			var s = "";
			for (var k = 0; k < 200; k++) s = f(1) + "|" + f(1, 2, 3, 4);
			s;`},
		// A compiled frame that throws still has to unwind through the caller's
		// handlers.
		{"a-throw-unwinds", `
			function f(o) { return o.x; }
			var caught = 0;
			for (var k = 0; k < 200; k++) { try { f(null); } catch (e) { caught++; } }
			caught;`},
		// Recursion, which reuses the frame slab at successive depths.
		{"recursion", `
			function down(n) { if (n <= 0) { return 0; } return n + down(n - 1); }
			var s = 0; for (var k = 0; k < 200; k++) s = down(30); s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// BenchmarkCallIntoALoop is the same call against a body big enough to pay for
// the entry, which is the shape the tier was built for. The gap between this
// ratio and the one above is the frame setup.
func BenchmarkCallIntoALoop(b *testing.B) {
	const src = "function w(n){ var s=0,i=0; while(i<n){ s=s+i*1.5; i=i+1; } return s; } w;"

	run := func(b *testing.B, on bool) {
		saved := jitEnabled
		jitEnabled = on
		defer func() { jitEnabled = saved }()

		rt := New()
		fnVal, err := rt.RunString("loop.js", src)
		if err != nil {
			b.Fatal(err)
		}
		args := []Value{tov(50)}
		for i := 0; i < 4*jitThreshold; i++ {
			if _, e := rt.callValue(fnVal, mkundef(), args); e != nil {
				b.Fatal("threw")
			}
		}
		b.ResetTimer()
		for b.Loop() {
			if _, e := rt.callValue(fnVal, mkundef(), args); e != nil {
				b.Fatal("threw")
			}
		}
	}

	b.Run("interpreted", func(b *testing.B) { run(b, false) })
	b.Run("compiled", func(b *testing.B) { run(b, true) })
}
