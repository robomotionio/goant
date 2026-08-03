//go:build amd64

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
