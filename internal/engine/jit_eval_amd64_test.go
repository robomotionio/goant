//go:build amd64

package engine

import "testing"

// Direct eval, in compiled code.
//
// A function containing one is the hardest frame shape this tier compiles. Its
// free names do not resolve to slots — they route through a `with` chain whose
// innermost object is a variable object the frame builds on entry, because the
// eval's own `var` declarations land there and the function has to see them.
// And code inside the eval can read this frame's `this`, close over its locals,
// and declare into it.
//
// So what these check is that a compiled frame supplies everything an
// interpreted one does: the chain, the variable object, the receiver, and a
// capture that shares cells with the frame's own closures.
func TestDirectEvalAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"evaluates-an-expression", `function f(s, a){ return eval(s) + a; }
			var r = 0; for (var i = 0; i < 3000; i++) r = f("1+2", i); r;`},
		{"sees-a-local", `function f(s){ var x = 41; return eval(s); }
			var r = 0; for (var i = 0; i < 3000; i++) r = f("x + 1"); r;`},
		{"sees-a-parameter", `function f(a, s){ return eval(s); }
			var r = 0; for (var i = 0; i < 3000; i++) r = f(i, "a * 2"); r;`},
		{"writes-a-local", `function f(s){ var x = 1; eval(s); return x; }
			var r = 0; for (var i = 0; i < 3000; i++) r = f("x = 99"); r;`},
		{"declares-a-var-the-function-then-reads", `function f(s){ eval(s); return typeof declaredInEval + ":" + declaredInEval; }
			var r = ""; for (var i = 0; i < 3000; i++) r = f("var declaredInEval = 7;"); r;`},
		{"declares-a-function", `function f(s){ eval(s); return g(3); }
			var r = 0; for (var i = 0; i < 3000; i++) r = f("function g(n){ return n * 3; }"); r;`},
		{"sees-this", `function f(s){ return eval(s); }
			var o = {k: 5, m: function (s) { return eval(s); }};
			var r = 0; for (var i = 0; i < 3000; i++) r = o.m("this.k"); r;`},
		// A closure created inside the eval over one of the frame's locals must
		// share the cell with the frame, or a later write is invisible to it.
		{"closes-over-a-local", `function f(s){ var x = 1; var g = eval(s); x = 42; return g(); }
			var r = 0; for (var i = 0; i < 3000; i++) r = f("(function(){ return x; })"); r;`},
		{"shares-the-cell-with-the-frames-own-closure", `function f(s){
				var x = 1;
				var mine = function(){ return x; };
				var theirs = eval(s);
				x = 7;
				return mine() * 100 + theirs();
			}
			var r = 0; for (var i = 0; i < 3000; i++) r = f("(function(){ return x; })"); r;`},
		// The callee is checked at run time: reassigning the binding makes the
		// site an ordinary call, and the eval's scope is not handed over.
		{"reassigned-eval-is-an-ordinary-call", `function f(s){ return eval(s); }
			var r = 0; for (var i = 0; i < 3000; i++) r = f("1+1");
			var save = eval;
			eval = function(x){ return "called:" + x; };
			var out = f("1+1");
			eval = save;
			"" + r + ":" + out;`},
		{"indirect-eval-is-global", `var g = 1;
			function f(s){ var g = 2; return (0, eval)(s); }
			var r = 0; for (var i = 0; i < 3000; i++) r = f("g"); r;`},
		// Arguments that are not strings pass straight through, and zero
		// arguments is undefined.
		{"non-string-passes-through", `function f(v){ return eval(v); }
			for (var i = 0; i < 3000; i++) f("1");
			var out = ""; var vals = [1, null, true, {}, [1]];
			for (var j = 0; j < vals.length; j++) out += typeof f(vals[j]) + ",";
			out;`},
		{"no-arguments", `function f(){ return eval(); }
			var r = ""; for (var i = 0; i < 3000; i++) r = "" + f(); r;`},
		// Throwing, both from the parse and from the body.
		{"syntax-error", `function f(s){ return eval(s); }
			for (var i = 0; i < 3000; i++) f("1");
			var m = ""; try { f("this is not javascript ("); } catch (e) { m = e.name; } m;`},
		{"the-body-throws", `function f(s){ return eval(s); }
			for (var i = 0; i < 3000; i++) f("1");
			var m = ""; try { f("throw new RangeError('inside')"); } catch (e) { m = e.name + ":" + e.message; } m;`},
		{"caught-inside-the-function", `function f(s){ try { return eval(s); } catch (e) { return "caught:" + e.name; } }
			var r = ""; for (var i = 0; i < 3000; i++) r = f("1+1") + "|" + f("throw new TypeError('x')"); r;`},
		// Strict eval gets its own variable environment, so a `var` in it must
		// NOT leak into the function.
		{"strict-eval-does-not-leak", `function f(s){ "use strict"; eval(s); return typeof notLeaked; }
			var r = ""; for (var i = 0; i < 3000; i++) r = f("var notLeaked = 1;"); r;`},
		{"nested-eval", `function f(s){ var x = 3; return eval(s); }
			var r = 0; for (var i = 0; i < 3000; i++) r = f("eval('x * 2')"); r;`},
		// eval inside a loop, which is what makes the function hot, and eval
		// beside a `with`, which is the chain with two objects on it.
		{"eval-inside-a-loop", `function f(n){ var t = 0; for (var i = 0; i < n; i++) { t += eval("i + 1"); } return t; }
			var r = 0; for (var i = 0; i < 3000; i++) r = f(4); r;`},
		{"eval-inside-a-with", `function f(o, s){ with (o) { return eval(s); } }
			var r = 0; for (var i = 0; i < 3000; i++) r = f({x: i}, "x + 1"); r;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

func TestDirectEvalCompiles(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	for _, src := range []string{
		"function f(s){ return eval(s); }",
		"function f(s){ var x = 1; eval(s); return x; }",
		"function f(o, s){ with (o) { return eval(s); } }",
	} {
		var why string
		fn := jitFn(t, src)
		if !jitEligible(fn) {
			t.Errorf("%q refused by the frame gate", src)
			continue
		}
		c := jitCompile(fn, &why)
		if c == nil {
			t.Errorf("refused %q: %s", src, why)
			continue
		}
		c.free()
	}

	// Eval CODE is still refused: it adopts its caller's variable object, which
	// arrives through runtime state a compiled frame is not part of.
	rt := New()
	if _, err := rt.RunString("e.js", "eval('var q = 1; q')"); err != nil {
		t.Fatalf("run: %v", err)
	}
}
