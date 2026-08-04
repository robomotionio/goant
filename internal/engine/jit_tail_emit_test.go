//go:build amd64 || arm64

package engine

import (
	"strings"
	"testing"
)

// A proper tail call has one observable property and it is not the value: the
// stack does not grow. Everything else about TAIL_CALL is CALL, so what these
// check is the frame, at a depth no ordinary call could survive.
//
// $MAX_ITERATIONS in test262's tcoHelper is 100,000, so that is the bar. The
// engine's own limit is maxFrameDepth, which these would hit within a few
// thousand if the trampoline were not there.
func TestTailCallsDoNotGrowTheStack(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"self-recursion", `"use strict";
			var count = 0;
			(function f(n) { if (n === 0) { count += 1; return 0; } return f(n - 1); })(100000);
			count;`},
		{"through-a-method", `"use strict";
			var o = { n: 0, step: function (n) { if (n === 0) { this.n = 1; return 0; } return o.step(n - 1); } };
			o.step(100000);
			o.n;`},
		{"mutual-recursion", `"use strict";
			function even(n) { if (n === 0) return 1; return odd(n - 1); }
			function odd(n) { if (n === 0) return 0; return even(n - 1); }
			even(100000);`},
		{"arguments-carried-along", `"use strict";
			function f(n, acc) { if (n === 0) return acc; return f(n - 1, acc + n); }
			f(100000, 0);`},
		// The tail call is the last thing a block, an if or a loop body does,
		// and each of those is a separate has-call-in-tail-position rule.
		{"inside-a-block", `"use strict";
			var c = 0;
			(function f(n) { if (n === 0) { c = 1; return 0; } { return f(n - 1); } })(100000);
			c;`},
		{"through-a-conditional", `"use strict";
			function f(n) { return n === 0 ? 0 : f(n - 1); }
			f(100000);`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// The value a tail call produces, over the callees the trampoline cannot take
// over: a native, a bound function, a proxy, a getter-backed callee, and one
// that throws. Each falls through to an ordinary call, and each has to give the
// same answer the interpreter does.
func TestTailCallsToWhatCannotBeTrampolined(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"native-callee", `"use strict";
			function f(a) { return Math.max(a, 3); }
			var s = 0; for (var i = 0; i < 3000; i++) s = f(i % 7); s;`},
		{"bound-callee", `"use strict";
			function g(a, b) { return a + b + this.k; }
			var b = g.bind({k: 10}, 5);
			function f(a) { return b(a); }
			var s = 0; for (var i = 0; i < 3000; i++) s = f(i % 7); s;`},
		{"proxy-callee", `"use strict";
			var p = new Proxy(function (a) { return a * 2; }, {});
			function f(a) { return p(a); }
			var s = 0; for (var i = 0; i < 3000; i++) s = f(i % 7); s;`},
		{"generator-callee", `"use strict";
			function* g(a) { yield a; }
			function f(a) { return g(a); }
			var s = ""; for (var i = 0; i < 3000; i++) s = "" + f(i).next().value; s;`},
		{"callee-throws", `"use strict";
			function g(a) { if (a === 5) throw new RangeError("five"); return a; }
			function f(a) { return g(a); }
			var s = 0, m = "";
			for (var i = 0; i < 3000; i++) { try { s += f(i % 4); } catch (e) { m = e.name; } }
			try { f(5); } catch (e) { m = e.name + ":" + e.message; }
			"" + s + ":" + m;`},
		{"callee-is-not-callable", `"use strict";
			function f(g) { return g(1); }
			var s = 0, m = "";
			for (var i = 0; i < 3000; i++) s += f(function (x) { return x; });
			try { f(7); } catch (e) { m = e.name; }
			"" + s + ":" + m;`},
		{"receiver-is-preserved", `"use strict";
			function f(o) { return o.m(); }
			var s = 0;
			for (var i = 0; i < 3000; i++) s = f({k: i, m: function () { return this.k; }});
			s;`},
		{"sloppy-callee-gets-a-boxed-receiver", `
			function g() { return typeof this; }
			function f() { "use strict"; return g.call(7); }
			var s = ""; for (var i = 0; i < 3000; i++) s = f(); s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// A tail call hands its frame to the callee, so anything the caller's frame
// still owned would be handed over with it. A closure over a local is the case
// that matters: the cell points into the locals slab, and the callee is about
// to be given a slab slice at the same depth.
func TestATailCallDoesNotHandOverItsLocals(t *testing.T) {
	const src = `"use strict";
		function make(a) {
			var seen = a * 2;
			var probe = function () { return seen; };
			return step(probe, 40);
		}
		function step(probe, n) { if (n === 0) return probe(); return step(probe, n - 1); }
		var out = "";
		for (var i = 0; i < 3000; i++) out = "" + make(i);
		out;`
	jitBothWays(t, "tail-call-keeps-captured-locals.js", src)
}

func TestTailCallsCompile(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	for _, src := range []string{
		`function f(n){ "use strict"; return f(n - 1); }`,
		`function f(o,n){ "use strict"; return o.m(n); }`,
		`function f(n){ "use strict"; return n === 0 ? 0 : f(n - 1); }`,
	} {
		var why string
		fn := jitFn(t, src)
		c := jitCompile(fn, &why)
		if c == nil {
			t.Errorf("refused %q: %s", src, why)
			continue
		}
		c.free()
		// And it really is a tail call rather than a call the compiler turned
		// into an ordinary one — otherwise the tests above measure nothing.
		found := false
		for ip := 0; ip < len(fn.code); {
			op := Opcode(fn.code[ip])
			if op == OpTailCall || op == OpTailCallMethod {
				found = true
			}
			ip += int(opTable[op].Size)
		}
		if !found {
			t.Errorf("%q has no TAIL_CALL: the fixture no longer tests one", src)
		}
	}
}

// Compiled code must not be reached at all for a tail call the tier cannot
// model, and the shape that matters is a callee whose own frame is special.
func TestTailCallDepthIsBounded(t *testing.T) {
	// Not a tail call: an ordinary call in tail position in SLOPPY code, which
	// the language does not optimise. It has to run out of stack rather than
	// loop forever, and with the engine's error rather than Go's.
	_, err := New().RunString("deep.js", `
		function f(n) { return f(n + 1); }
		f(0);`)
	if err == nil {
		t.Fatal("unbounded sloppy recursion returned")
	}
	if !strings.Contains(err.Error(), "Maximum call stack size exceeded") {
		t.Fatalf("wrong error for unbounded sloppy recursion: %v", err)
	}
}
