//go:build amd64

package engine

import "testing"

// `with`, in compiled code.
//
// Every free name in a function containing one resolves through a chain of
// objects at run time rather than to a slot, and the resolution is a spec
// algorithm with an observable trap on nearly every step: HasBinding does a
// HasProperty, consults @@unscopables only if that said yes, and GetBindingValue
// then does its *own* HasProperty before the Get. A Proxy can count all three
// and an @@unscopables getter can falsify the property between them.
//
// The compiled tier calls the same withGetVar/withPutVar/withDelVar the
// interpreter does, so what these check is the plumbing around them: that the
// chain is on the frame where the collector can see it, that reference mode
// produces two operands and the plain form one, and that the compiler's baked-in
// lexical fallback is reached with the right index.
func TestWithAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"reads-through-the-object", `function f(o, a){ with (o) { return x + a; } }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({x: i}, 1); s;`},
		{"falls-back-to-a-local", `function f(o, a){ var x = 5; with (o) { return x + a; } }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({}, i) + f({x: 100}, 1); s;`},
		{"falls-back-to-a-global", `
			globalName = 7;
			function f(o){ with (o) { return globalName; } }
			var s = 0; for (var i = 0; i < 3000; i++) { globalName = i; s = f({}) + f({globalName: 1}); } s;`},
		{"falls-back-to-an-upvalue", `function outer(u){ return function (o){ with (o) { return u; } }; }
			var g = outer(9);
			var s = 0; for (var i = 0; i < 3000; i++) s = g({}) + g({u: 1}); s;`},
		{"unresolvable-throws", `function f(o){ with (o) { return notBoundAnywhere; } }
			for (var i = 0; i < 3000; i++) f({notBoundAnywhere: i});
			var m = ""; try { f({}); } catch (e) { m = e.name; } m;`},
		{"typeof-is-lenient", `function f(o){ with (o) { return typeof notBoundAnywhere; } }
			var s = ""; for (var i = 0; i < 3000; i++) s = f({}) + ":" + f({notBoundAnywhere: 1}); s;`},

		// Writing. The plain form re-walks the chain; the reference form writes
		// back through the base its read produced, which is what makes a
		// compound assignment touch one binding rather than two.
		{"writes-through-the-object", `function f(o){ with (o) { x = 42; } return o.x; }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({x: i}); s;`},
		{"writes-to-the-local-fallback", `function f(o){ var x = 1; with (o) { x = 42; } return x; }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({}) + f({x: 0}); s;`},
		{"compound-assignment", `function f(o){ with (o) { x += 10; } return o.x; }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({x: i}); s;`},
		{"compound-assignment-on-the-fallback", `function f(o){ var x = 1; with (o) { x += 10; } return x; }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({}); s;`},
		{"increment", `function f(o){ with (o) { x++; } return o.x; }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({x: i}); s;`},
		{"strict-write-to-undeclared-throws", `function f(o){ "use strict"; with (o) { return 1; } }
			var s = 0;
			function g(o){ with (o) { notDeclaredHere = 1; } }
			for (var i = 0; i < 3000; i++) g({notDeclaredHere: i});
			var m = ""; try { (function(){ "use strict"; var o = {}; with (o) { alsoNotDeclared = 1; } })(); }
			catch (e) { m = e.name; } m;`},

		// delete of a name, which is the only delete whose target is a binding.
		{"delete-a-bound-name", `function f(o){ with (o) { return delete x; } }
			var s = ""; for (var i = 0; i < 3000; i++) { var o = {x: i}; s = "" + f(o) + ("x" in o); } s;`},
		{"delete-an-unbound-name", `function f(o){ with (o) { return delete neverDeclared; } }
			var s = ""; for (var i = 0; i < 3000; i++) s = "" + f({}); s;`},

		// Nesting, and the chain walking outward.
		{"nested-with", `function f(a, b){ with (a) { with (b) { return x + y; } } }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({x: i, y: 1}, {y: 2}); s;`},
		{"exit-restores-the-chain", `function f(o){ var r = 0; with (o) { r = x; } return r + (typeof x === "undefined" ? 100 : 0); }
			x = 1;
			var s = 0; for (var i = 0; i < 3000; i++) s = f({x: i}); s;`},

		// The traps. Each of these gives a different answer if a HasProperty is
		// skipped or done twice.
		{"unscopables-hides-a-name", `function f(o, a){ with (o) { return x + a; } }
			var x = 50;
			var o = {x: 1}; o[Symbol.unscopables] = {x: true};
			var s = 0; for (var i = 0; i < 3000; i++) s = f(o, i); s;`},
		{"proxy-counts-its-traps", `function f(o){ with (o) { return x; } }
			var n = 0;
			var p = new Proxy({x: 1}, {
				has: function (t, k) { if (k === "x") n++; return k in t; },
				get: function (t, k) { return t[k]; }
			});
			var s = 0; for (var i = 0; i < 3000; i++) s = f(p);
			"" + s + ":" + n;`},
		{"the-has-trap-throws", `function f(o){ with (o) { return x; } }
			for (var i = 0; i < 3000; i++) f({x: i});
			var p = new Proxy({}, {has: function(){ throw new RangeError("h"); }});
			var m = ""; try { f(p); } catch (e) { m = e.name; } m;`},
		{"a-getter-throws", `function f(o){ with (o) { return x; } }
			for (var i = 0; i < 3000; i++) f({x: i});
			var m = ""; try { f({get x(){ throw new TypeError("g"); }}); } catch (e) { m = e.name; } m;`},
		{"with-over-a-primitive", `function f(v){ with (v) { return length; } }
			var s = 0; for (var i = 0; i < 3000; i++) s = f("abcd"); s;`},
		{"with-over-nullish-throws", `function f(v){ with (v) { return 1; } }
			for (var i = 0; i < 3000; i++) f({});
			var m = ""; try { f(null); } catch (e) { m = e.name; } m;`},
		// A `with` inside a loop, which is what makes the function hot enough to
		// tier, and also what exercises EXIT_WITH more than once per frame.
		{"with-inside-a-loop", `function f(o, n){ var s = 0; for (var i = 0; i < n; i++) { with (o) { s += x; } } return s; }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({x: 2}, 4); s;`},
		// A closure created inside a `with` captures the chain, and the emitter
		// refuses to compile such a child — this checks the parent still runs.
		{"closure-captures-the-chain", `function f(o){ with (o) { return function(){ return x; }; } }
			var s = 0; for (var i = 0; i < 3000; i++) s = f({x: i})(); s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

func TestWithCompiles(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	for _, src := range []string{
		"function f(o, a){ with (o) { return x + a; } }",
		"function f(o){ var x = 1; with (o) { x = 2; } return x; }",
		"function f(o){ with (o) { x += 1; } return o.x; }",
		"function f(o){ with (o) { return delete x; } }",
		"function f(o, n){ var s = 0; for (var i = 0; i < n; i++) { with (o) { s += x; } } return s; }",
	} {
		var why string
		fn := jitFn(t, src)
		c := jitCompile(fn, &why)
		if c == nil {
			t.Errorf("refused %q: %s", src, why)
			continue
		}
		c.free()
		// And the body really does resolve through the chain rather than to a
		// slot, or these test nothing.
		found := false
		for ip := 0; ip < len(fn.code); {
			op := Opcode(fn.code[ip])
			switch op {
			case OpWithGetVar, OpWithPutVar, OpWithDelVar:
				found = true
			}
			ip += int(opTable[op].Size)
		}
		if !found {
			t.Errorf("%q has no WITH_ opcode: the fixture no longer tests one", src)
		}
	}
}
