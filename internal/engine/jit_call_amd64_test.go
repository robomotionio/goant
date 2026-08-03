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

// TestJITCallAgreesWithTheInterpreter covers the opcode that had refused every
// function containing it.
//
// A call is where compiled code hands control to something it knows nothing
// about, so the cases are the ways that can go wrong: a callee that is not a
// function, one that throws, one that runs a collection while this frame's
// operands are sitting in the spill area, and one that writes to its own
// `arguments` — which must not reach back into the caller's operand stack.
func TestJITCallAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"no-arguments", `
			function g() { return 7; }
			function f() { return g() + 1; }
			var s = 0; for (var k = 0; k < 200; k++) s += f(); s;`},
		{"several-arguments", `
			function g(a, b, c) { return a + b * c; }
			function f(x) { return g(x, x + 1, x + 2); }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
		{"fewer-arguments-than-parameters", `
			function g(a, b) { return "" + a + "," + b; }
			function f(x) { return g(x); }
			var s = ""; for (var k = 0; k < 200; k++) s = f(k); s;`},
		{"nested-calls", `
			function h(a) { return a * 2; }
			function g(a) { return h(a) + 1; }
			function f(a) { return g(a) + g(a + 1); }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
		{"the-callee-throws", `
			function g(a) { if (a > 100) { throw new Error("no"); } return a; }
			function f(a) { return g(a) + 1; }
			var s = 0, c = 0;
			for (var k = 0; k < 200; k++) { try { s += f(k); } catch (e) { c++; } }
			s * 1000 + c;`},
		{"the-callee-is-not-a-function", `
			var g = 5;
			function f(a) { return g(a); }
			var c = 0; for (var k = 0; k < 200; k++) { try { f(k); } catch (e) { c++; } } c;`},
		{"a-native-callee", `
			function f(a) { return Math.max(a, 3); }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k % 6); s;`},
		// The callee writes to its own mapped arguments object. If the helper
		// handed it a window onto the caller's spill area rather than a slice of
		// its own, this would write into the caller's operand stack.
		{"the-callee-writes-its-arguments", `
			function g(a, b) { arguments[0] = 99; return a + b; }
			function f(x, y) { var t = g(x, y); return t + x + y; }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k, k + 1); s;`},
		// A call is a place a collection can happen with this frame's operands
		// live only in the spill area.
		{"a-collection-inside-the-callee", `
			function g(a) { var junk = []; for (var i = 0; i < 200; i++) junk.push({i: i}); return a; }
			function f(a, b) { return g(a) + b; }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k, k + 1); s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestJITMethodCallAgreesWithTheInterpreter is the pair that `o.m()` compiles
// to: GET_FIELD2, which leaves the receiver behind for CALL_METHOD to bind as
// `this`. Neither is worth anything without the other, which is why the weighted
// diagnostic scored GET_FIELD2 at zero for as long as CALL_METHOD was refused.
func TestJITMethodCallAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"a-method-on-the-prototype", `
			function P(v) { this.v = v; }
			P.prototype.get = function () { return this.v; };
			P.prototype.twice = function () { return this.get() + this.get(); };
			var p = new P(21), s = 0;
			for (var k = 0; k < 200; k++) s += p.twice();
			s;`},
		{"an-own-method-with-arguments", `
			var o = { n: 3, scale: function (a, b) { return this.n * a + b; } };
			function f(x) { return o.scale(x, x + 1); }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
		{"the-receiver-is-bound", `
			var o = { n: 5, who: function () { return this === o ? this.n : -1; } };
			function f() { return o.who(); }
			var s = 0; for (var k = 0; k < 200; k++) s += f(); s;`},
		{"a-builtin-method", `
			function f(a) { return a.toFixed(2); }
			var s = ""; for (var k = 0; k < 200; k++) s = f(k + 0.5); s;`},
		{"the-method-is-missing", `
			var o = { n: 1 };
			function f() { return o.nope(); }
			var c = 0; for (var k = 0; k < 200; k++) { try { f(); } catch (e) { c++; } } c;`},
		{"a-getter-returning-the-method", `
			var calls = 0;
			var o = { n: 4, get m() { calls++; return function () { return this.n; }; } };
			function f() { return o.m(); }
			var s = 0; for (var k = 0; k < 200; k++) s += f(); s * 1000 + calls;`},
		{"chained-through-a-field", `
			var inner = { n: 2, get2: function () { return this.n; } };
			var outer = { inner: inner };
			function f() { return outer.inner.get2(); }
			var s = 0; for (var k = 0; k < 200; k++) s += f(); s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestJITCompilesAFunctionThatCalls stops the agreement above from being
// agreement between the interpreter and itself.
func TestJITCompilesAFunctionThatCalls(t *testing.T) {
	for _, src := range []string{
		"function f(a){ return g(a) + 1; }",
		"function f(o,a){ return o.m(a) + 1; }",
		"function f(o){ return o.m(); }",
	} {
		_, fn := jitFnRT(t, src)
		var why string
		c := jitCompile(fn, &why)
		if c == nil {
			t.Errorf("refused %q: %s", src, why)
			continue
		}
		c.free()
	}
}

// TestCompiledFrameEntryKeepsTheFrameContract is the frame contract a compiled
// entry has to keep, whichever way it is reached.
//
// Each case is one thing the interpreter's frame entry does that a compiled
// frame depends on. Run both ways and compare, because the failures here are
// quiet: a receiver bound wrong reads undefined, and a new.target left in place
// is visible only to whatever the compiled body calls next.
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

// TestJITConstructAgreesWithTheInterpreter covers `new F(...)`, which is CALL
// with a different runtime entry point and everything that makes construction
// different from calling: the object's prototype, what a constructor returning
// an object means, and what happens when the callee is not one.
func TestJITConstructAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"a-plain-constructor", `
			function P(a, b) { this.a = a; this.b = b; }
			function f(k) { var p = new P(k, k + 1); return p.a + p.b; }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
		{"the-prototype-is-bound", `
			function P(v) { this.v = v; }
			P.prototype.twice = function () { return this.v * 2; };
			function f(k) { return new P(k).twice(); }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
		{"a-constructor-returning-an-object", `
			function P(v) { this.v = v; return {v: v * 10}; }
			function f(k) { return new P(k).v; }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
		{"a-constructor-returning-a-primitive", `
			function P(v) { this.v = v; return 7; }
			function f(k) { return new P(k).v; }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
		{"no-arguments", `
			function P() { this.v = 3; }
			function f() { return new P().v; }
			var s = 0; for (var k = 0; k < 200; k++) s += f(); s;`},
		{"not-a-constructor", `
			var g = 5;
			function f() { return new g(); }
			var c = 0; for (var k = 0; k < 200; k++) { try { f(); } catch (e) { c++; } } c;`},
		{"an-arrow-is-not-a-constructor", `
			var g = function () { return 1; };
			var h = (x) => x;
			function f() { return new h(); }
			var c = 0; for (var k = 0; k < 200; k++) { try { f(); } catch (e) { c++; } } c;`},
		{"a-builtin-constructor", `
			function f(k) { return new Array(3).length + new Number(k).valueOf(); }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
		{"nested-construction", `
			function Inner(v) { this.v = v; }
			function Outer(v) { this.inner = new Inner(v); }
			function f(k) { return new Outer(k).inner.v; }
			var s = 0; for (var k = 0; k < 200; k++) s += f(k); s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// The aliasing in jitSpillArgs rests on the callee never retaining or writing
// through the array it is handed.
//
// That used to be structural — `arguments` needs SPECIAL_OBJ, which had no
// template — and it is not any more: a compiled function can build one. The
// property still holds, for a different and narrower reason. A mapped
// `arguments` aliases the frame's **locals**, which every frame owns, and its
// indexed properties are copied out of the argument values. The one place that
// reads the window is jitHelperArguments, and it takes a copy first.
//
// So this checks the narrow thing rather than the structural one: the arguments
// object a compiled frame builds must not change when the caller's spill area is
// overwritten afterwards, which is what a retained window would do.
func TestArgumentsDoesNotAliasTheCallersSpillArea(t *testing.T) {
	src := `
		function callee(a, b) {
			var args = arguments;
			// A call in between reuses the caller's spill area for its own
			// operands; if ` + "`args`" + ` were a window onto it, these would change.
			other(a + 1, b + 1);
			return "" + args[0] + "," + args[1] + "," + args.length;
		}
		function other(x, y) { return x + y; }
		function caller(i) { return callee(i, i * 2); }
		var out = "";
		for (var i = 0; i < 4000; i++) out = caller(i);
		out;
	`
	jitBothWays(t, "args/no-alias", src)
}

// And the behaviour that depends on it, end to end: a sloppy callee whose mapped
// `arguments` writes through to its parameters, called from a compiled caller.
//
// It takes the general path rather than the aliasing one, which is the point —
// the answer has to be the interpreter's either way.
func TestMappedArgumentsThroughACompiledCaller(t *testing.T) {
	const src = `
		function callee(a, b) { arguments[0] = 99; return a + b; }
		function caller(x, y) { return callee(x, y); }
		var s = 0;
		for (var i = 0; i < 5000; i++) s += caller(1, 2);
		s;
	`
	jitBothWays(t, "mapped-args.js", src)
}

// The callee memo is keyed on a Value, and a handle names a cell only until the
// collector frees it — after which the same bits can name a different function.
// icEpoch is what closes that window, so this checks the check: an entry from a
// previous epoch must be re-resolved rather than believed.
func TestCalleeMemoDoesNotSurviveAnEpoch(t *testing.T) {
	rt := New()
	fnVal, err := rt.RunString("memo.js", "(function f(a){ return a + 1; })")
	if err != nil {
		t.Fatal(err)
	}
	fn, cl := rt.jitResolveCallee(fnVal)
	if fn == nil || cl == nil {
		t.Fatal("an ordinary closure did not resolve")
	}
	slot := &rt.calleeMemo[uint32(fnVal.handle())&(calleeMemoSize-1)]
	if slot.callee != fnVal || slot.fn != fn {
		t.Fatal("resolving did not fill the memo")
	}

	// What a recycled handle would look like: the entry still names this callee,
	// but the function behind it is somebody else's.
	other, err := rt.RunString("memo2.js", "(function g(a){ return a + 2; })")
	if err != nil {
		t.Fatal(err)
	}
	otherFn, _ := rt.jitResolveCallee(other)
	if otherFn == nil || otherFn == fn {
		t.Fatal("needed a second, distinct function")
	}
	*slot = calleeMemoEntry{callee: fnVal, fn: otherFn, cl: cl, epoch: icEpoch()}
	if got, _ := rt.jitResolveCallee(fnVal); got != otherFn {
		t.Fatal("the memo is not being consulted, so this test proves nothing")
	}

	icEpochBump()
	if got, _ := rt.jitResolveCallee(fnVal); got != fn {
		t.Fatalf("a stale memo entry survived an epoch bump: got %p, want %p", got, fn)
	}
}

// Natives, proxies and generators must not reach the compiled path at all, memo
// or no memo. A remembered "no" would be as wrong as a remembered wrong answer.
func TestCalleeMemoRefusesWhatTheCompiledPathCannotRun(t *testing.T) {
	rt := New()
	for _, src := range []string{
		"Math.max",
		"(function*(){ yield 1; })",
		"(async function(){ return 1; })",
		"new Proxy(function(){}, {})",
		"({})",
	} {
		v, err := rt.RunString("refuse.js", src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if fn, _ := rt.jitResolveCallee(v); fn != nil {
			t.Errorf("%s resolved to a compilable callee", src)
		}
	}
}
