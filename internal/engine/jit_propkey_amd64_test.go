//go:build amd64

package engine

import "testing"

// TO_PROPKEY, GLOBAL, DELETE_VAR, PUT_CONST and DEFINE_METHOD.
//
// The first three are one call-out each and the interesting question is the
// plumbing. The last two are not: DEFINE_METHOD leaves its target behind rather
// than producing a value, and PUT_CONST consumes two and produces one while
// opTable records one and none — both are places where a wrong stack effect
// would put the operands in the wrong registers rather than refuse the function.
func TestPropkeyBatchAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		// TO_PROPKEY is what a template literal's `${}` compiles to, and it is
		// ToPrimitive with hint "string" rather than ToString: an object with
		// both toString and valueOf must take the first.
		{"template-prefers-toString", `function f(o){ return ` + "`v=${o}`" + `; }
			var o = {toString:function(){return "S";}, valueOf:function(){return 9;}};
			var s=""; for (var i=0;i<3000;i++) s = f(o); s;`},
		{"template-falls-back-to-valueOf", `function f(o){ return ` + "`v=${o}`" + `; }
			var o = {toString:null, valueOf:function(){return 9;}};
			var s=""; for (var i=0;i<3000;i++) s = f(o); s;`},
		{"template-over-every-type", `function f(a){ return ` + "`[${a}]`" + `; }
			for (var i=0;i<3000;i++) f(i);
			var out=""; var vals=[1,-0,NaN,"s",true,undefined,null,{},[1,2],1n];
			for (var j=0;j<vals.length;j++) out += f(vals[j]); out;`},
		// The throwing arm: ToPrimitive runs user code, and the exception has to
		// leave compiled code the same way it leaves the interpreter.
		{"template-throws", `function f(o){ return ` + "`v=${o}`" + `; }
			for (var i=0;i<3000;i++) f(i);
			var bad = {toString:function(){ throw new RangeError("boom"); }};
			var m=""; try { f(bad); } catch(e) { m = e.name+":"+e.message; } m;`},
		// A Symbol is a valid property key and an invalid string operand, so it
		// distinguishes TO_PROPKEY from a ToString that happened to work.
		{"symbol-key-throws", `function f(a){ return ` + "`${a}`" + `; }
			for (var i=0;i<3000;i++) f(i);
			var m=""; try { f(Symbol("s")); } catch(e) { m = e.name; } m;`},

		// GLOBAL. `this` at the top level of a sloppy function reaches it, and
		// the identity has to be the same object the interpreter would produce.
		{"global-object-identity", `function f(){ return (function(){ return this; })(); }
			var r=null; for (var i=0;i<3000;i++) r = f();
			(r === globalThis) + ":" + (typeof r);`},

		// DELETE_VAR and PUT_CONST are the two halves of a strict assignment to
		// an unqualified name: the first resolves the reference, the second
		// writes and throws if it did not resolve.
		{"strict-assign-to-declared-global", `"use strict";
			var g = 0;
			function f(v){ g = v; return g; }
			var s=0; for (var i=0;i<3000;i++) s = f(i); s;`},
		{"strict-assign-to-undeclared-throws", `"use strict";
			var g = 0;
			function f(v){ g = v; return g; }
			for (var i=0;i<3000;i++) f(i);
			function h(v){ notDeclaredAnywhere = v; }
			var m=""; try { h(1); } catch(e) { m = e.name; } m;`},
		// The reason PUT_CONST re-checks: the right-hand side runs between the
		// resolve and the write, and it can delete the binding it resolved to.
		{"rhs-deletes-the-binding", `
			var strictWrite = function(v){ "use strict"; declaredLater = v; return declaredLater; };
			declaredLater = 0;
			var s = 0; for (var i=0;i<3000;i++) s = strictWrite(i);
			var m = "";
			try { strictWrite((function(){ delete globalThis.declaredLater; return 1; })()); }
			catch (e) { m = e.name; }
			"" + s + ":" + m;`},
		{"strict-assign-to-readonly-throws", `"use strict";
			var target = {};
			Object.defineProperty(globalThis, "frozenGlobal", {value: 1, writable: false, configurable: true});
			var ok = function(v){ "use strict"; mutableGlobal = v; return mutableGlobal; };
			mutableGlobal = 0;
			var s = 0; for (var i=0;i<3000;i++) s = ok(i);
			function bad(v){ "use strict"; frozenGlobal = v; }
			var m=""; try { bad(2); } catch(e) { m = e.name; } "" + s + ":" + m;`},
		// delete on an unqualified name, which is DELETE_VAR without the write.
		{"delete-var", `
			someGlobal = 1;
			function f(){ return delete someGlobal; }
			var r = null; for (var i=0;i<3000;i++) { someGlobal = i; r = f(); }
			"" + r + ":" + (typeof someGlobal);`},

		// DEFINE_METHOD. The target stays on the stack, so a wrong effect here
		// hands the next instruction the method instead of the object.
		{"object-literal-method", `function f(a){ return {m:function(){return a;}, n:a}; }
			var s=0; for (var i=0;i<3000;i++) { var o=f(i); s = o.m()+o.n; } s;`},
		{"object-literal-accessor", `function f(a){ return {get v(){return a*2;}, set v(x){a=x;}}; }
			var s=0; for (var i=0;i<3000;i++) { var o=f(i); o.v = 5; s = o.v; } s;`},
		// A class body, where the accessors are non-enumerable and the object
		// literal's above are not — the flags byte is what tells them apart.
		{"class-method-attributes", `function f(a){
				class C { m(){ return a; } get v(){ return a+1; } }
				return C;
			}
			var out=""; for (var i=0;i<3000;i++) { var C=f(i);
				out = Object.keys(C.prototype).length + ":" +
					JSON.stringify(Object.getOwnPropertyDescriptor(C.prototype,"m")) + ":" +
					new C().m() + ":" + new C().v; }
			out;`},
		// Private methods and accessors, which need the class environment the
		// closure carries rather than the one the caller happens to be in.
		{"private-method", `function f(a){
				class C { #p(){ return a; } get val(){ return this.#p(); } }
				return new C();
			}
			var s=0; for (var i=0;i<3000;i++) s = f(i).val; s;`},
		{"private-accessor", `function f(a){
				class C { get #p(){ return a*3; } read(){ return this.#p; } }
				return new C();
			}
			var s=0; for (var i=0;i<3000;i++) s = f(i).read(); s;`},
		// The throwing arm of DEFINE_METHOD: installing a private member on a
		// non-extensible object is a TypeError, not a silent no-op.
		{"method-on-frozen-target", `function f(o,a){ o.m = function(){ return a; }; return o; }
			var s=0; for (var i=0;i<3000;i++) s = f({}, i).m();
			var m=""; "use strict";
			try { (function(){ "use strict"; var o=Object.freeze({}); o.m=function(){}; })(); }
			catch(e) { m=e.name; } "" + s + ":" + m;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// Each of the five must actually compile. Without this the differential tests
// above pass by running the interpreter twice.
func TestPropkeyBatchCompiles(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	for _, src := range []string{
		"function f(a){ return `x${a}y`; }",
		"function f(){ return (function(){ return this; })(); }",
		"function f(){ return delete someGlobal; }",
		`function f(v){ "use strict"; someGlobal = v; return someGlobal; }`,
		"function f(a){ return {m:function(){return a;}}; }",
		"function f(a){ return {get v(){return a;}}; }",
		"function f(a){ class C { m(){return a;} } return C; }",
		// All five in one body, which is what the analyses have to agree on.
		`function f(a){ "use strict"; someGlobal = ` + "`k${a}`" + `; return {m:function(){return a;}}; }`,
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

// The loop heads: FOR_IN, GET_LENGTH and the re-validation check the
// for-in loop makes before every iteration.
//
// What makes these worth a differential test rather than a smoke test is that
// the enumeration order and the re-validation are both observable. A for-in
// loop whose body deletes a key it has not reached yet must skip it, and one
// that adds a key must not see it — the snapshot and the recheck are two
// separate mechanisms and compiled code has to keep both.
func TestLoopHeadsAgreeWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"for-in-order", `function f(o){ var s=""; for (var k in o) s += k + ","; return s; }
			var o = {b:1, a:2, 2:3, 1:4, c:5};
			var r=""; for (var i=0;i<3000;i++) r = f(o); r;`},
		{"for-in-inherited", `function f(o){ var s=""; for (var k in o) s += k + ","; return s; }
			function P(){ this.own = 1; } P.prototype.inherited = 2;
			var r=""; for (var i=0;i<3000;i++) r = f(new P()); r;`},
		{"for-in-body-deletes-a-later-key", `function f(o){ var s=""; for (var k in o) { delete o.c; s += k + ","; } return s; }
			var r=""; for (var i=0;i<3000;i++) r = f({a:1,b:2,c:3}); r;`},
		{"for-in-body-adds-a-key", `function f(o){ var s=""; for (var k in o) { o.zz = 1; s += k + ","; } return s; }
			var r=""; for (var i=0;i<3000;i++) r = f({a:1,b:2}); r;`},
		{"for-in-over-a-primitive", `function f(v){ var s=""; for (var k in v) s += k + ","; return s; }
			var r=""; for (var i=0;i<3000;i++) r = f("abc") + "|" + f(1) + "|" + f(null) + "|" + f(undefined); r;`},
		{"for-in-over-a-proxy", `function f(o){ var s=""; for (var k in o) s += k + ","; return s; }
			var p = new Proxy({a:1,b:2}, {ownKeys: function(t){ return ["b","a"]; }});
			var r=""; for (var i=0;i<3000;i++) r = f(p); r;`},
		{"for-in-throws", `function f(o){ var s=""; for (var k in o) s += k; return s; }
			for (var i=0;i<3000;i++) f({a:1});
			var p = new Proxy({}, {ownKeys: function(){ throw new RangeError("k"); }});
			var m=""; try { f(p); } catch(e) { m = e.name; } m;`},
		{"length-of-anything", `function f(v){ return v.length; }
			for (var i=0;i<3000;i++) f([1,2,3]);
			var out=""; var vals=[[1,2,3],"abcd",function(a,b){},{length:9},[]];
			for (var j=0;j<vals.length;j++) out += f(vals[j]) + ","; out;`},
		{"length-throws", `function f(v){ return v.length; }
			for (var i=0;i<3000;i++) f([1]);
			var m=""; try { f(null); } catch(e) { m = e.name; } m;`},
		{"length-through-a-getter", `function f(v){ return v.length; }
			for (var i=0;i<3000;i++) f([1]);
			var n=0, o = {get length(){ n++; return n; }};
			f(o) + ":" + f(o) + ":" + n;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

func TestLoopHeadsCompile(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	for _, src := range []string{
		"function f(o){ var s=0; for (var k in o) s++; return s; }",
		"function f(v){ return v.length; }",
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
