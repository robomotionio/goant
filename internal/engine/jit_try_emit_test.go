//go:build amd64 || arm64

package engine

import "testing"

// try/catch in compiled code.
//
// The handler is a compile-time answer rather than a runtime stack: which catch
// a throw belongs to is decided while emitting, recorded per call site, and
// looked up by the address the site would have resumed at. What that has to get
// right is every question the interpreter's handler stack answers — which
// handler is innermost, what a rethrow from inside a catch belongs to, and what
// a `break` out of a try body leaves behind.
//
// The catch also has to be entered correctly, and that is not a branch: the
// runtime jumps into the middle of compiled code from Go, where only the
// context register is set. The first version landed straight on the catch and
// left the locals pointer holding whatever the machine had, which read as a
// frame of nonsense rather than as a crash.
func TestTryCatchAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"catches-a-throw", `function f(o){ try { return o.x.y; } catch (e) { return e.name; } }
			var s=""; for (var i=0;i<3000;i++) s = "" + f({x:{y:i}}) + f({}); s;`},
		{"the-value-is-the-thrown-one", `function f(v){ try { throw v; } catch (e) { return e; } }
			for (var i=0;i<3000;i++) f(i);
			var out=""; var vals=[1,"s",null,undefined,{k:2},[1]];
			for (var j=0;j<vals.length;j++) out += typeof f(vals[j]) + ":"; out;`},
		{"locals-survive-the-throw", `function f(o){ var a = 1, b = 2; try { b = o.x.y; a = 9; } catch (e) { a = a + b; } return a * 100 + b; }
			var s=0; for (var i=0;i<3000;i++) s = f({x:{y:i}}) + f({}); s;`},
		{"no-throw-skips-the-catch", `function f(a){ var n = 0; try { n = a * 2; } catch (e) { n = -1; } return n; }
			var s=0; for (var i=0;i<3000;i++) s = f(i); s;`},
		{"catch-runs-then-falls-through", `function f(o){ var r = ""; try { r += o.x.y; } catch (e) { r += "E"; } r += "|"; return r; }
			var s=""; for (var i=0;i<3000;i++) s = f({x:{y:1}}) + f({}); s;`},
		// A throw from a callee rather than from an operator in this frame: the
		// error travels back through the helper the same way, and has to be
		// caught the same way.
		{"catches-a-throw-from-a-callee", `function g(n){ if (n < 0) throw new RangeError("neg"); return n; }
			function f(n){ try { return g(n); } catch (e) { return e.message; } }
			var s=""; for (var i=0;i<3000;i++) s = "" + f(i) + f(-1); s;`},
		{"catches-a-throw-from-a-getter", `function f(o){ try { return o.p; } catch (e) { return "E"; } }
			var good = {p: 1}, bad = {get p(){ throw new TypeError("g"); }};
			var s=""; for (var i=0;i<3000;i++) s = "" + f(good) + f(bad); s;`},
		{"catches-an-explicit-throw", `function f(a){ try { if (a % 3 === 0) throw a; return "ok"; } catch (e) { return "c" + e; } }
			var s=""; for (var i=0;i<3000;i++) s = f(i % 6) + f((i+1) % 6); s;`},

		// Nesting. The corpus goes two deep, and the inner one has to win.
		{"nested-inner-wins", `function f(o){
				try { try { return o.x.y; } catch (e) { return "inner"; } } catch (e) { return "outer"; }
			}
			var s=""; for (var i=0;i<3000;i++) s = "" + f({x:{y:1}}) + f({}); s;`},
		{"nested-rethrow-reaches-the-outer", `function f(o){
				try { try { return o.x.y; } catch (e) { throw new RangeError("re"); } } catch (e) { return e.name; }
			}
			var s=""; for (var i=0;i<3000;i++) s = "" + f({x:{y:1}}) + f({}); s;`},
		// A throw from inside a catch belongs to the next handler out, not to
		// the one whose catch it is. Without that the handler stack would loop.
		{"throw-from-a-catch-escapes", `function f(o){ try { return o.x.y; } catch (e) { return o.z.w; } }
			var s=""; for (var i=0;i<3000;i++) s = "" + f({x:{y:1}});
			var m=""; try { f({}); } catch (e) { m = e.name; } "" + s + ":" + m;`},
		{"sequential-tries", `function f(o){
				var r = "";
				try { r += o.a.b; } catch (e) { r += "A"; }
				try { r += o.c.d; } catch (e) { r += "C"; }
				return r;
			}
			var s=""; for (var i=0;i<3000;i++) s = f({a:{b:1},c:{d:2}}) + f({a:{b:1}}) + f({}); s;`},

		// A `break` out of a try emits its own TRY_POP, which is why the handler
		// analysis is a dataflow rather than a scan that pairs push with pop:
		// the code after the loop is not protected, and a scan would say it was.
		{"break-out-of-a-try", `function f(o, n){
				var r = "";
				for (var i = 0; i < n; i++) { try { r += o.x.y; break; } catch (e) { r += "E"; } }
				return r;
			}
			var s=""; for (var i=0;i<3000;i++) s = f({x:{y:1}}, 3) + f({}, 3); s;`},
		{"throw-after-a-break-escapes", `function f(o, n){
				for (var i = 0; i < n; i++) { try { return o.x.y; } catch (e) { break; } }
				return o.z.w;
			}
			var s=0; for (var i=0;i<3000;i++) s = f({x:{y:i}}, 3);
			var m=""; try { f({}, 3); } catch (e) { m = e.name; } "" + s + ":" + m;`},
		{"continue-out-of-a-try", `function f(o, n){
				var r = "";
				for (var i = 0; i < n; i++) { try { if (i === 1) continue; r += o.x.y; } catch (e) { r += "E"; } }
				return r;
			}
			var s=""; for (var i=0;i<3000;i++) s = f({x:{y:1}}, 3) + f({}, 3); s;`},
		// `return` out of a try is a control-flow signal in the engine's own
		// plumbing, and a catch must not take it.
		{"return-out-of-a-try", `function f(o){ try { return "r" + o.x.y; } catch (e) { return "E"; } }
			var s=""; for (var i=0;i<3000;i++) s = f({x:{y:1}}) + f({}); s;`},

		// A try in a loop, entered thousands of times, which is also the shape
		// that makes the function hot enough to compile in the first place.
		{"try-inside-a-loop", `function f(a, n){
				var s = 0;
				for (var i = 0; i < n; i++) { try { s += a[i].v; } catch (e) { s -= 1; } }
				return s;
			}
			var arr = [{v:1}, null, {v:3}, undefined, {v:5}];
			var s=0; for (var i=0;i<3000;i++) s = f(arr, 5); s;`},
		{"loop-inside-a-try", `function f(a, n){
				var s = 0;
				try { for (var i = 0; i < n; i++) s += a[i].v; } catch (e) { s = -s; }
				return s;
			}
			var arr = [{v:1}, {v:2}, null, {v:4}];
			var s=0; for (var i=0;i<3000;i++) s = f(arr, 4) + f(arr, 2); s;`},

		// The catch binding is a local like any other, including when the body
		// closes over it.
		{"catch-binding-is-captured", `function f(o){
				var g = null;
				try { o.x.y; } catch (e) { g = function () { return e.name; }; }
				return g ? g() : "none";
			}
			var s=""; for (var i=0;i<3000;i++) s = "" + f({x:{y:1}}) + f({}); s;`},
		{"catch-binding-shadows", `function f(o){ var e = "outer"; try { o.x.y; } catch (e) { return e.name; } return e; }
			var s=""; for (var i=0;i<3000;i++) s = "" + f({x:{y:1}}) + f({}); s;`},

		// A stack deep enough to be partly in memory when the throw happens: the
		// catch resumes at depth zero, so what was in those slots is gone, and
		// nothing must try to read it back.
		{"throw-from-a-deep-stack", `function f(o){
				try { return [1,2,3,4,5,6,7,8,9,10, o.x.y, 12]; } catch (e) { return e.name; }
			}
			var s=""; for (var i=0;i<3000;i++) s = "" + f({x:{y:1}}).length + f({}); s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

func TestTryCatchCompiles(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	for _, src := range []string{
		"function f(o){ try { return o.x; } catch (e) { return 0; } }",
		"function f(o){ var r = 0; try { r = o.x; } catch (e) { r = 1; } return r; }",
		"function f(o){ try { try { return o.x; } catch (e) { return 1; } } catch (e) { return 2; } }",
		"function f(o,n){ var s = 0; for (var i = 0; i < n; i++) { try { s += o[i]; } catch (e) { s -= 1; } } return s; }",
		"function f(o,n){ for (var i = 0; i < n; i++) { try { return o.x; } catch (e) { break; } } return 0; }",
	} {
		var why string
		c := jitCompile(jitFn(t, src), &why)
		if c == nil {
			t.Errorf("refused %q: %s", src, why)
			continue
		}
		c.free()
	}

	// A finally is still refused. Its completion record carries a pending
	// return or break through the handler, which is machinery this tier has
	// none of, and quietly treating it as a catch would swallow them.
	for _, src := range []string{
		"function f(o){ try { return o.x; } finally { o.y = 1; } }",
		"function f(o){ try { return o.x; } catch (e) { return 0; } finally { o.y = 1; } }",
	} {
		if c := jitCompile(jitFn(t, src), nil); c != nil {
			c.free()
			t.Errorf("compiled %q, which has a finally", src)
		}
	}
}
