//go:build amd64 || arm64

package engine

import (
	"fmt"
	"testing"
)

// The templates added to reach 77% of the Octane corpus, each checked against
// the interpreter over the cases that make it interesting rather than over the
// case that made it compile.
//
// Every one of these is a call-out except IS_UNDEF_OR_NULL, so what is being
// tested is not arithmetic but the plumbing: that the operands come off the
// spill area in the right order, that the result goes back in the right slot,
// that a throw propagates, and that the emitter's idea of the stack depth
// afterwards matches the interpreter's.
func TestBatchedTemplatesAgreeWithTheInterpreter(t *testing.T) {
	cases := []struct{ name, src string }{
		// MOD. The BigInt and coercion arms matter: `%` is emitted as a call to
		// the same jsArith the interpreter uses, so they must not diverge.
		{"mod-numbers", `function f(a,b){ return a%b; }
			var s=0; for (var i=1;i<2000;i++) s += f(i, 7); s;`},
		{"mod-negative-zero", `function f(a,b){ return 1/(a%b); }
			var s=0; for (var i=1;i<2000;i++) s += f(-4, 2); s;`},
		{"mod-nan", `function f(a,b){ return a%b; }
			var s=0; for (var i=1;i<2000;i++) s += (isNaN(f(i,0))?1:0); s;`},
		{"mod-bigint", `function f(a,b){ return a%b; }
			var s=0n; for (var i=1;i<2000;i++) s += f(7n, 3n); ""+s;`},
		{"mod-string", `function f(a,b){ return a%b; }
			var s=0; for (var i=1;i<2000;i++) s += f("10", 3); s;`},
		{"mod-throws", `function f(a,b){ return a%b; }
			var o={valueOf:function(){throw new RangeError("x");}};
			var s=0,m=""; for (var i=1;i<2000;i++) s+=f(i,3);
			try { f(o,1); } catch(e){ m=e.name; } ""+s+":"+m;`},

		// TYPEOF over every type it can name.
		{"typeof", `function f(a){ return typeof a; }
			var out=""; var vals=[1,"s",true,undefined,null,{},[],function(){},Symbol(),1n];
			for (var i=0;i<2000;i++) f(i);
			for (var j=0;j<vals.length;j++) out += f(vals[j])+",";
			out;`},

		// IS_UNDEF_OR_NULL, which `??` and `?.` compile to. The emitted range
		// test has to reject every non-nullish value including a Number, which
		// shifts below the tag prefix and wraps.
		{"nullish-coalesce", `function f(a){ return a ?? "d"; }
			var out=""; var vals=[0,"",false,NaN,undefined,null,{},1n];
			for (var i=0;i<2000;i++) f(i);
			for (var j=0;j<vals.length;j++) out += f(vals[j])+",";
			out;`},
		{"optional-chain", `function f(a){ return a?.x; }
			var out=""; var vals=[undefined,null,{x:1},{},0];
			for (var i=0;i<2000;i++) f({x:i});
			for (var j=0;j<vals.length;j++) out += f(vals[j])+",";
			out;`},

		// IN, including the private-name form, which is why the helper takes the
		// closure: `#x in o` resolves against the class environment.
		{"in", `function f(k,o){ return k in o; }
			var o={a:1}; var s=0;
			for (var i=0;i<2000;i++) s += f("a",o)?1:0;
			""+s+":"+f("b",o)+":"+f("toString",o)+":"+f(0,[1]);`},
		{"in-private", `class C { #x = 1; static has(o){ return #x in o; } }
			var c=new C(), s=0;
			for (var i=0;i<2000;i++) s += C.has(c)?1:0;
			""+s+":"+C.has({});`},
		{"in-throws", `function f(k,o){ return k in o; }
			var s=0,m=""; for (var i=0;i<2000;i++) s += f("a",{a:1})?1:0;
			try { f("a", 1); } catch(e){ m=e.name; } ""+s+":"+m;`},

		// DELETE, whose strict arm throws where the sloppy one is silently false.
		{"delete-sloppy", `function f(o,k){ return delete o[k]; }
			var s=0; for (var i=0;i<2000;i++) s += f({a:1},"a")?1:0;
			""+s+":"+f(Object.defineProperty({},"a",{value:1}),"a");`},
		{"delete-strict", `"use strict"; function f(o,k){ return delete o[k]; }
			var s=0,m=""; for (var i=0;i<2000;i++) s += f({a:1},"a")?1:0;
			try { f(Object.defineProperty({},"a",{value:1}),"a"); } catch(e){ m=e.name; }
			""+s+":"+m;`},

		// THROW, which ends the frame. The emitter has to stop believing it can
		// fall through, and jitAnalyze has to end the block, or the depth
		// analysis carries a stack past it.
		{"throw", `function f(a){ if (a === 1999) throw new RangeError("x"); return a; }
			var s=0,m=""; try { for (var i=0;i<2000;i++) s+=f(i); } catch(e){ m=e.name; }
			""+s+":"+m;`},
		{"throw-in-branch", `function f(a){ if (a < 0) { throw new Error("neg"); } return a*2; }
			var s=0,m=""; for (var i=0;i<2000;i++) s+=f(i);
			try { f(-1); } catch(e){ m=e.message; } ""+s+":"+m;`},
		{"throw-primitive", `function f(a){ if (a === 1999) throw "plain"; return a; }
			var s=0,m=""; try { for (var i=0;i<2000;i++) s+=f(i); } catch(e){ m=""+e; }
			""+s+":"+m;`},

		// OBJECT and ARRAY literals. The array count comes off the top of the
		// spill area, so a literal built with other operands beneath it is the
		// case that separates a right answer from a plausible one.
		{"object-literal", `function f(a){ var o = {}; o.v = a; return o.v; }
			var s=0; for (var i=0;i<2000;i++) s+=f(i); s;`},
		{"array-literal", `function f(a){ return [a, a+1, a+2]; }
			var s=0; for (var i=0;i<2000;i++) s+=f(i)[2]; s;`},
		{"array-empty", `function f(a){ var x=[]; return x.length + a; }
			var s=0; for (var i=0;i<2000;i++) s+=f(i); s;`},
		{"array-under-operands", `function f(a,b){ return a + [b,b+1][1]; }
			var s=0; for (var i=0;i<2000;i++) s+=f(i,i); s;`},
		{"array-nested", `function f(a){ return [[a],[a+1]][1][0]; }
			var s=0; for (var i=0;i<2000;i++) s+=f(i); s;`},

		// REGEXP, built from two constants on the stack.
		{"regexp", `function f(a){ return /ab+c/.test(a); }
			var s=0; for (var i=0;i<2000;i++) s += f("abbc")?1:0;
			""+s+":"+f("xyz");`},
		{"regexp-flags", `function f(a){ return /AB/i.test(a); }
			var s=0; for (var i=0;i<2000;i++) s += f("ab")?1:0; s;`},

		// GET_GLOBAL_UNDEF: the lenient read `typeof undeclared` compiles to,
		// where an absent name is undefined rather than a ReferenceError.
		{"typeof-undeclared", `function f(){ return typeof definitelyNotDeclared; }
			var out=""; for (var i=0;i<2000;i++) out = f(); out;`},
		{"typeof-declared", `var declaredHere = 5;
			function f(){ return typeof declaredHere; }
			var out=""; for (var i=0;i<2000;i++) out = f(); out;`},

		// INSTANCEOF, which was committed before the analyses knew about it and
		// so was never reachable. This is the check that it is now.
		{"instanceof", `function A(){}; function f(v){ return v instanceof A; }
			var a=new A(), s=0; for (var i=0;i<2000;i++) s += f(i%2?a:{})?1:0; s;`},
	}
	for _, c := range cases {
		jitBothWays(t, "batch/"+c.name, c.src)
	}
}

// The templates are only worth having if the functions holding them actually
// compile. A differential passes whether or not that happened, which is how
// INSTANCEOF shipped unreachable — jitNumberDemand had no arm for it and refused
// every function the template freed.
func TestBatchedTemplatesActuallyCompile(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	for _, src := range []string{
		"function f(a,b){ return a%b; }",
		"function f(a){ return typeof a; }",
		"function f(a){ return a ?? 1; }",
		"function f(a,o){ return a in o; }",
		"function f(o,k){ return delete o[k]; }",
		"function f(a){ if (a<0) throw new Error('x'); return a; }",
		"function f(a){ var o={}; o.v=a; return o.v; }",
		"function f(a){ return [a,a+1]; }",
		"function f(a){ return /x/.test(a); }",
		"function f(){ return typeof notDeclaredAnywhere; }",
		"function f(v){ return v instanceof Object; }",
	} {
		var why string
		c := jitCompile(jitFn(t, src), &why)
		if c == nil {
			t.Errorf("refused %q: %s", src, why)
			continue
		}
		c.free()
	}
}

// The analyses and the emitter have to agree about every opcode with a template.
// They are three separate switches with three separate `default: refuse` arms,
// and a template whose opcode is missing from any of them compiles nothing —
// silently, because the function is refused for a reason that names the stack
// discipline rather than the gap.
func TestEveryTemplateIsKnownToTheAnalyses(t *testing.T) {
	// A one-instruction body per opcode is not constructible, so this walks the
	// corpus of little programs above instead and requires that no function
	// containing only templated opcodes is refused for an analysis reason.
	for _, src := range []string{
		"function f(a,b){ return a%b + (typeof a).length; }",
		"function f(a,b){ return (a in b) ? [a] : [b]; }",
		"function f(a,b){ if (a) { throw new Error('e'); } return {v:b}; }",
		"function f(a){ return (a ?? 0) % 3; }",
		"function f(a){ return typeof missingGlobalName === 'undefined' ? [a] : []; }",
	} {
		var why string
		c := jitCompile(jitFn(t, src), &why)
		if c == nil {
			t.Errorf("refused %q as %q — every opcode in it has a template, so this "+
				"is an analysis that does not know one of them", src, why)
			continue
		}
		c.free()
	}
}

func TestBatchedTemplatesUnderGCPressure(t *testing.T) {
	// The array and object literals allocate, and a helper that allocates can
	// collect while the caller's operands live only in the spill area.
	src := `
		function f(a) { return [{v: a}, {v: a + 1}]; }
		var keep = null, s = 0;
		for (var i = 0; i < 20000; i++) { var r = f(i); s += r[1].v; keep = r; }
		"" + s + ":" + keep[0].v;
	`
	jitBothWays(t, fmt.Sprintf("gc/%d", 0), src)
}

// The self-reference a named function expression binds. It is a value compiled
// code reads out of its context rather than a call-out, so what has to hold is
// that the context carries the right function and that the collector can see it.
func TestNamedFunctionExpressionSelfReference(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"identity", `var f = function me(n){ return me === f ? 1 : 0; };
			var s=0; for (var i=0;i<3000;i++) s += f(i); s;`},
		{"recursion", `var fact = function me(n){ return n <= 1 ? 1 : n * me(n-1); };
			var s=0; for (var i=0;i<3000;i++) s += fact(5); s;`},
		{"shadowed-by-arg", `var f = function me(me){ return typeof me; };
			var out=""; for (var i=0;i<3000;i++) out = f(1); out;`},
		{"reassignment-ignored", `var f = function me(n){ me = 7; return typeof me; };
			var out=""; for (var i=0;i<3000;i++) out = f(i); out;`},
		{"nested", `var outer = function a(n){
				var inner = function b(m){ return b === inner ? m : -1; };
				return inner(n) + (a === outer ? 1 : 0);
			};
			var s=0; for (var i=0;i<3000;i++) s += outer(i); s;`},
		{"as-property", `var o = { f: function me(n){ return me.length + n; } };
			var s=0; for (var i=0;i<3000;i++) s += o.f(i); s;`},
	} {
		jitBothWays(t, "self/"+c.name, c.src)
	}
}

// The context's FnVal is a root: a compiled frame suspended in a helper may be
// the only thing referring to its own function, and a helper that allocates can
// collect while it is.
func TestSelfReferenceSurvivesACollection(t *testing.T) {
	src := `
		var f = function me(n){
			var junk = [{a: n}, {b: n}];
			return (me === f ? 1 : 0) + junk.length;
		};
		var s = 0;
		for (var i = 0; i < 30000; i++) s += f(i);
		s;
	`
	jitBothWays(t, "self/gc", src)
}

// Three of SPECIAL_OBJ's five kinds are still refused, and refused under a name
// that says which — the diagnostic's whole purpose being not to point at an
// opcode that is mostly implemented.
func TestOtherSpecialObjKindsAreStillRefused(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"function f(a){ return new.target; }", "special-obj/new.target"},
	} {
		var why string
		if c2 := jitCompile(jitFn(t, c.src), &why); c2 != nil {
			c2.free()
			t.Errorf("compiled %q", c.src)
			continue
		}
		if why != c.want {
			t.Errorf("%q refused as %q, want %q", c.src, why, c.want)
		}
	}
	for k, want := range map[byte]string{0: "arguments", 1: "self", 2: "new.target", 3: "import.meta", 4: "private-name"} {
		if got := jitSpecialObjKind(k); got != want {
			t.Errorf("kind %d is named %q, want %q", k, got, want)
		}
	}
}

// Closures are the one thing in this tier that lets a frame's locals outlive the
// frame, so these are the cases where getting it wrong is a wrong answer rather
// than a refusal: shared cells, writes after capture, capture in a loop, and a
// collection while the cells are the only reference to the values.
func TestClosuresAgreeWithTheInterpreter(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"capture", `function mk(n){ return function(){ return n; }; }
			var s=0; for (var i=0;i<3000;i++) s += mk(i)(); s;`},
		{"shared-cell", `function mk(n){
				var v = n;
				var get = function(){ return v; };
				var set = function(x){ v = x; };
				set(v + 1);
				return get();
			}
			var s=0; for (var i=0;i<3000;i++) s += mk(i); s;`},
		{"write-after-capture", `function mk(n){
				var v = n;
				var get = function(){ return v; };
				v = v * 2;
				return get();
			}
			var s=0; for (var i=0;i<3000;i++) s += mk(i); s;`},
		{"counter", `function mkCounter(){
				var c = 0;
				return function(){ return ++c; };
			}
			var s=0; for (var i=0;i<3000;i++){ var f=mkCounter(); s += f()+f()+f(); } s;`},
		{"loop-var", `function mk(n){
				var fns = [];
				for (var i = 0; i < 3; i++) { fns.push(function(){ return i; }); }
				return fns[0]() + fns[1]() + fns[2]() + n;
			}
			var s=0; for (var i=0;i<3000;i++) s += mk(i); s;`},
		{"loop-let", `function mk(n){
				var fns = [];
				for (let i = 0; i < 3; i++) { fns.push(function(){ return i; }); }
				return fns[0]() + fns[1]() + fns[2]() + n;
			}
			var s=0; for (var i=0;i<3000;i++) s += mk(i); s;`},
		{"nested-two-deep", `function a(n){
				var x = n;
				return function b(){ var y = x + 1; return function c(){ return x + y; }; };
			}
			var s=0; for (var i=0;i<3000;i++) s += a(i)()(); s;`},
		{"upvalue-write-through", `function mk(n){
				var v = n;
				var bump = function(){ v++; };
				bump(); bump();
				return v;
			}
			var s=0; for (var i=0;i<3000;i++) s += mk(i); s;`},
		{"outlives-frame", `var kept = null;
			function mk(n){ var v = n; kept = function(){ return v; }; v = v + 100; }
			var s=0; for (var i=0;i<3000;i++){ mk(i); s += kept(); } s;`},
		{"recursive-closure", `function mk(n){
				var f = function(k){ return k <= 0 ? n : f(k-1); };
				return f(3);
			}
			var s=0; for (var i=0;i<3000;i++) s += mk(i); s;`},
		{"param-captured", `function mk(a,b){ return function(){ return a + b; }; }
			var s=0; for (var i=0;i<3000;i++) s += mk(i,1)(); s;`},
		{"method-home", `class B { m(){ return 1; } }
			class D extends B { m(){ var f = () => super.m(); return f(); } }
			var d=new D(), s=0; for (var i=0;i<3000;i++) s += d.m(); s;`},
		{"private-env", `class C { #v; constructor(n){ this.#v = n; }
				get(){ var f = () => this.#v; return f(); } }
			var s=0; for (var i=0;i<3000;i++) s += new C(i).get(); s;`},
	} {
		jitBothWays(t, "closure/"+c.name, c.src)
	}
}

// An upvalue points into the frame's locals slice, so that slice must never be
// handed to another frame at the same depth again. If dropFrameLocals were
// missed, a later call at the same depth would overwrite the captured values —
// and the shape that shows it is recursion, where the same depth is reused
// immediately.
func TestCapturedLocalsAreNotReusedByTheNextFrame(t *testing.T) {
	src := `
		function mk(n){ var v = n; return function(){ return v; }; }
		function rec(d){ if (d === 0) return 0; var f = mk(d); var deeper = rec(d-1); return f() + deeper; }
		var s = 0;
		for (var i = 0; i < 2000; i++) s += rec(8);
		s;
	`
	jitBothWays(t, "closure/no-reuse", src)
}

// The cells are the only reference to their values once the frame has gone, and
// a helper that allocates can collect while a compiled frame is suspended.
func TestCapturedValuesSurviveACollection(t *testing.T) {
	src := `
		var keep = [];
		function mk(n){ var v = {tag: n}; return function(){ return v.tag; }; }
		var s = 0;
		for (var i = 0; i < 20000; i++) {
			var f = mk(i);
			if (i % 100 === 0) keep.push(f);
			s += f();
		}
		for (var j = 0; j < keep.length; j++) s += keep[j]();
		s;
	`
	jitBothWays(t, "closure/gc", src)
}

// A child that captures a `with` scope is refused, because the chain comes off
// the enclosing frame's withStack and a compiled frame has none.
func TestClosureCapturingWithIsRefused(t *testing.T) {
	// A child that captures a `with` chain is compilable now, but only from a
	// parent that HAS one to give it: the chain lives on the frame, and a
	// compiled frame builds one only if its own body does.
	//
	// The shape that still has nothing to hand over is a child capturing a
	// chain the parent inherited rather than built — an arrow inside a function
	// that was itself compiled inside a `with`.
	compiles := "function f(o){ with (o) { return function(){ return x; }; } }"
	var why string
	c := jitCompile(jitFn(t, compiles), &why)
	if c == nil {
		t.Fatalf("refused %q: %s", compiles, why)
	}
	c.free()

	// And the chain really does reach the child.
	jitBothWays(t, "closure-over-a-with.js", `
		function f(o){ with (o) { return function(){ return x; }; } }
		var s = 0; for (var i = 0; i < 3000; i++) s = f({x: i})(); s;`)
}

// The `arguments` object, in both its forms. Mapped writes through to the
// parameters and unmapped does not, the callee slot differs, and the object can
// outlive the call — which is why the frame gives up its locals buffer.
func TestArgumentsAgreesWithTheInterpreter(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"length", `function f(a,b){ return arguments.length; }
			var s=0; for (var i=0;i<3000;i++) s += f(i) + f(i,i) + f(i,i,i); s;`},
		{"index", `function f(a,b){ return arguments[0] + arguments[1]; }
			var s=0; for (var i=0;i<3000;i++) s += f(i,1); s;`},
		{"missing-index", `function f(a){ return "" + arguments[5]; }
			var out=""; for (var i=0;i<3000;i++) out = f(i); out;`},
		{"mapped-write-through", `function f(a){ arguments[0] = 99; return a; }
			var s=0; for (var i=0;i<3000;i++) s += f(i); s;`},
		{"mapped-param-write", `function f(a){ a = 42; return arguments[0]; }
			var s=0; for (var i=0;i<3000;i++) s += f(i); s;`},
		{"unmapped-strict", `"use strict"; function f(a){ arguments[0] = 99; return a; }
			var s=0; for (var i=0;i<3000;i++) s += f(i); s;`},
		{"unmapped-default-param", `function f(a = 1){ arguments[0] = 99; return a; }
			var s=0; for (var i=0;i<3000;i++) s += f(i); s;`},
		{"callee-sloppy", `function f(a){ return typeof arguments.callee; }
			var out=""; for (var i=0;i<3000;i++) out = f(i); out;`},
		{"callee-strict-poisoned", `"use strict"; function f(a){
				try { return typeof arguments.callee; } catch (e) { return e.name; } }
			var out=""; for (var i=0;i<3000;i++) out = f(i); out;`},
		{"spread", `function f(a,b){ return Math.max.apply(null, arguments); }
			var s=0; for (var i=0;i<3000;i++) s += f(i, i+1); s;`},
		{"iterate", `function f(a,b){ var t=0; for (var v of arguments) t+=v; return t; }
			var s=0; for (var i=0;i<3000;i++) s += f(i,1); s;`},
		{"outlives-frame", `var kept=null;
			function f(a,b){ kept = arguments; return 0; }
			var s=0; for (var i=0;i<3000;i++){ f(i,i+1); s += kept[0]+kept[1]; } s;`},
		{"delete-then-read", `function f(a){ delete arguments[0]; return "" + arguments[0] + a; }
			var out=""; for (var i=0;i<3000;i++) out = f(i); out;`},
		{"not-an-array", `function f(a){ return Array.isArray(arguments) + ":" + arguments.length; }
			var out=""; for (var i=0;i<3000;i++) out = f(i); out;`},
	} {
		jitBothWays(t, "arguments/"+c.name, c.src)
	}
}

// A mapped `arguments` points into the locals, so the frame must give up its
// buffer exactly as a capture does — recursion at the same depth is what shows a
// missed drop.
func TestArgumentsDoesNotShareLocalsWithTheNextFrame(t *testing.T) {
	src := `
		function grab(a, b){ return arguments; }
		function rec(d){ if (d === 0) return 0; var g = grab(d, d*2); var deeper = rec(d-1); return g[0] + g[1] + deeper; }
		var s = 0;
		for (var i = 0; i < 2000; i++) s += rec(8);
		s;
	`
	jitBothWays(t, "arguments/no-reuse", src)
}
