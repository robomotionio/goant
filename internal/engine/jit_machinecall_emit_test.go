//go:build amd64 || arm64

package engine

import (
	"strings"
	"testing"

	"github.com/robomotionio/goant/internal/jitmem"
)

// The compiled call: a compiled function entering a compiled function without
// the runtime in between.
//
// Every case here is a way that can be wrong that the ordinary call tests
// cannot see, because the answer is the same either way and only the path
// differs. So each one both checks the answer against the interpreter and
// asserts that the machine path is the one that produced it — an agreement test
// that silently stopped taking the fast path would keep passing forever.

// jitMachineCalls runs src with the tier on and reports how many of its calls
// the call sites made themselves.
//
// The counter is only emitted when GOANT_JIT_STATS is set at compile time, so
// the flag is turned on around the compile rather than read from the
// environment — which also keeps the count to this test's own calls.
func jitMachineCalls(t *testing.T, name, src string) (Value, uint64, uint64) {
	t.Helper()
	savedJIT, savedStats := jitEnabled, jitStats.enabled
	jitEnabled, jitStats.enabled = true, true
	fast, slow := jitStats.callFast, jitStats.callSlow
	defer func() {
		jitEnabled, jitStats.enabled = savedJIT, savedStats
	}()
	v, err := New().RunString(name, src)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v, jitStats.callFast - fast, jitStats.callSlow - slow
}

// TestMachineCallTakesTheMachinePath is the floor: without it every other test
// in this file could be testing the runtime path twice.
func TestMachineCallTakesTheMachinePath(t *testing.T) {
	const src = `
		function add(a, b) { return a + b; }
		function twice(x) { return add(x, x); }
		var s = 0; for (var i = 0; i < 400; i++) s = add(s, twice(i));
		s;`
	got, fast, slow := jitMachineCalls(t, "machine.js", src)
	if got != tov(159600) {
		t.Errorf("got %v, want 159600", got)
	}
	if fast == 0 {
		t.Fatalf("no call was made by a call site (%d went through the runtime)", slow)
	}
	// Two of the three calls per iteration are to compiled functions of matching
	// arity, so the great majority must take it once the sites have filled.
	if fast*4 < slow {
		t.Errorf("%d calls by the call site against %d through the runtime", fast, slow)
	}
}

// TestMachineCallAgreesWithTheInterpreter is the differential over the shapes
// the machine path has to get right on its own, having no runtime frame to do
// it for them.
func TestMachineCallAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		// The locals a call site does not fill. The context is reused at its
		// depth, so a slot read before its first store holds the previous frame's
		// value unless the entry stub clears it.
		{"locals-not-filled-by-arguments", `
			function g(a) { var t; if (a % 2) { t = a; } return t === undefined ? -1 : t; }
			function f(a) { return g(a); }
			var s = ""; for (var k = 0; k < 400; k++) s = s.length < 40 ? s + f(k) + "," : s; s;`},
		// A sloppy callee reached with no receiver has to see the global object,
		// and one reached with a primitive has to see its wrapper — which the
		// entry stub cannot build, so it declines and the call is made again.
		{"a-sloppy-callee-with-no-receiver", `
			var g = 11;
			function h() { return this.g; }
			function f() { return h(); }
			var s = 0; for (var k = 0; k < 400; k++) s += f(); s;`},
		{"a-primitive-receiver", `
			function m() { return typeof this; }
			var o = 5;
			function f() { return m.call(o); }
			var s = ""; for (var k = 0; k < 400; k++) s = f(); s;`},
		{"an-array-receiver", `
			var a = [1, 2, 3];
			a.total = function () { return this.length; };
			function f() { return a.total(); }
			var s = 0; for (var k = 0; k < 400; k++) s += f(); s;`},
		// The callee declines its arguments at the parameter guard, which the
		// call site has to treat as "nothing happened" rather than as an answer.
		{"the-callee-declines-its-arguments", `
			function g(a) { return a * 2; }
			function f(a) { return g(a); }
			var s = ""; for (var k = 0; k < 400; k++) s = "" + f(k) + f("x"); s;`},
		// A throw crossing two compiled frames, caught in the third.
		{"a-throw-through-two-machine-frames", `
			function c(n) { if (n % 5 === 0) { throw new Error("boom"); } return n; }
			function b(n) { return c(n) + 1; }
			function a(n) { try { return b(n); } catch (e) { return -1; } }
			var s = 0; for (var k = 0; k < 400; k++) s += a(k); s;`},
		// A collection with a chain of compiled frames live. Their locals are in
		// their contexts and nowhere else, so a collector that did not trace them
		// would free what they are holding.
		{"a-collection-under-a-machine-chain", `
			function leaf(o) { var junk = []; for (var i = 0; i < 60; i++) junk.push({i: i}); return o.n; }
			function mid(o) { var keep = {n: o.n + 1}; return leaf(keep) + keep.n; }
			function top(k) { var mine = {n: k}; return mid(mine) + mine.n; }
			var s = 0; for (var k = 0; k < 400; k++) s += top(k); s;`},
		// Deeper than the machine-frame budget, so the chain has to fall back to
		// the runtime path partway down and pick up again on the other side.
		{"deeper-than-the-nesting-budget", `
			function down(n) { return n <= 0 ? 0 : down(n - 1) + 1; }
			var s = 0; for (var k = 0; k < 200; k++) s += down(40); s;`},
		// A callee that returns through a loop, so the fuel exit fires with
		// machine frames above it on the stack.
		{"the-callee-yields-to-the-runtime", `
			function spin(n) { var t = 0; for (var i = 0; i < n; i++) t += i; return t; }
			function f(n) { return spin(n) + 1; }
			var s = 0; for (var k = 0; k < 30; k++) s += f(30000); s;`},
		// An upvalue read from a machine-called frame, whose array the site
		// caches against the closure it was filled from.
		{"an-upvalue-in-the-callee", `
			function make(n) { return function (a) { return a + n; }; }
			var add3 = make(3), add4 = make(4);
			function f(a) { return add3(a) + add4(a); }
			var s = 0; for (var k = 0; k < 400; k++) s += f(k); s;`},
		// A site reached again, with a different callee, from inside a frame it
		// opened — which is what refills it while that frame is live. See
		// TestARefillDoesNotReassignALiveFrame for the invariant this rests on.
		{"the-site-refills-under-a-live-frame", `
			function down(k) { var r = k <= 0 ? 0 : outer(k - 1); shapes[k % 4].tag = r; return r + 1; }
			function flat(k) { return k * 2; }
			function pick(k) { return k % 2 ? down : flat; }
			function outer(k) { return pick(k)(k); }
			var shapes = [{tag: 0}, {a: 1, tag: 0}, {b: 1, a: 1, tag: 0}, {c: 1, tag: 0}];
			var s = 0; for (var k = 0; k < 600; k++) s += outer(k % 9); s;`},
		// The same site called with two different closures of one function, which
		// the guard must tell apart because their upvalues differ.
		{"a-polymorphic-call-site", `
			function make(n) { return function (a) { return a + n; }; }
			var fns = [make(1), make(2), make(3)];
			function f(k) { return fns[k % 3](k); }
			var s = 0; for (var k = 0; k < 400; k++) s += f(k); s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestMachineCallRefusesWhatItCannotFrame pins the predicate that decides which
// functions a call site may enter, because every one of them is a thing the
// frame it builds does not have.
func TestMachineCallRefusesWhatItCannotFrame(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"a-plain-function", "function f(a){ return a + 1; }", true},
		{"a-method", "function f(a){ return this.x + a; }", true},
		// Closes over a local, so the locals have to outlive the frame.
		{"closes-over-a-local", "function f(a){ var t = a; return function(){ return t; }; }", false},
		// `arguments` aliases them for the same reason.
		{"names-arguments", "function f(a){ return arguments.length + a; }", false},
		// Resolves names against something the frame carries.
		{"contains-a-direct-eval", "function f(a){ return eval('a + 1'); }", false},
		{"contains-a-with", "function f(o){ with (o) { return x; } }", false},
		// Takes over a frame, and the frame a call site builds is not one to take.
		{"contains-a-tail-call", "'use strict'; function f(a){ return g(a); }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fn := jitFnRT(t, tc.src)
			if got := jitMachineCallable(fn); got != tc.want {
				t.Errorf("jitMachineCallable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMachineCallableFitsTheContext is the other half of the predicate: a frame
// whose locals do not fit the context's array cannot live in one.
func TestMachineCallableFitsTheContext(t *testing.T) {
	var b strings.Builder
	b.WriteString("function f(a){ ")
	for i := 0; i < jitmem.InlineLocals+4; i++ {
		b.WriteString("var v")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(string(rune('A' + i/26)))
		b.WriteString(" = a; ")
	}
	b.WriteString("return a; }")
	_, fn := jitFnRT(t, b.String())
	if fn.maxLocals <= jitmem.InlineLocals {
		t.Skipf("the generated function has only %d locals", fn.maxLocals)
	}
	if jitMachineCallable(fn) {
		t.Errorf("a function with %d locals may be entered by a call site, "+
			"which has room for %d", fn.maxLocals, jitmem.InlineLocals)
	}
}

// TestContextChainIsLinkedAndBounded covers what a call site relies on to find
// the callee's frame: that the chain is built ahead of the deepest frame that
// has run, and that it stops.
func TestContextChainIsLinkedAndBounded(t *testing.T) {
	rt := New()
	ctx := rt.jitCtxAt(0)
	if ctx.Next == 0 {
		t.Fatal("the first context has nowhere to call into, so no call site ever would")
	}
	if ctx.Deep != 0 || ctx.Stack == nil {
		t.Error("a fresh context does not point at its own inline operand stack")
	}
	// Every link has to reach the context the runtime would hand out at that
	// depth, or a compiled call and the runtime would disagree about which frame
	// they are talking about.
	for d := 0; d+1 < len(rt.jitFrames); d++ {
		if rt.jitFrames[d].Next == 0 {
			continue
		}
		if rt.jitFrames[d].Next != uintptrOf(rt.jitFrames[d+1]) {
			t.Fatalf("context %d links to something other than context %d", d, d+1)
		}
	}
	// And past the cap it stops, so runaway recursion is a RangeError from the
	// runtime's own depth check rather than a chain of contexts.
	rt.jitCtxAt(jitMaxChain - 1)
	if got := rt.jitFrames[jitMaxChain-1].Next; got != 0 {
		t.Errorf("the chain continues past %d contexts", jitMaxChain)
	}
}

// uintptrOf is the address of a context as the chain records it.
func uintptrOf(ctx *jitmem.ExecContext) uintptr { return ctx.Addr() }

// TestARefillDoesNotReassignALiveFrame is the invariant a frame's identity rests
// on, and it is not the kind of thing an agreement test finds: a frame carries
// what the site resolved to when it was entered, and a site is a cache that is
// refilled — after a collection retires it, and whenever it is reached with a
// different callee.
//
// The two are reachable from one another. `outer` calls whatever `pick` returns,
// and one of the things it returns calls `outer` again, so the site is refilled
// from inside a frame it opened. When the frame's identity was the site itself,
// that reassigned it: the frame went on running one function while every helper
// serving it was handed another's constant pool and caches. TypeScript found it
// as an index into an empty pool, at a call depth of three and only after two
// million calls had gone right.
func TestARefillDoesNotReassignALiveFrame(t *testing.T) {
	saved, savedT := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 2
	defer func() { jitEnabled, jitThreshold = saved, savedT }()

	rt := New()
	if _, err := rt.RunString("refill.js", `
		function first(a) { return a + 1; }
		function second(a) { return a + 2; }
		var s = 0;
		for (var i = 0; i < 400; i++) { s += first(i); s += second(i); }
		s;`); err != nil {
		t.Fatal(err)
	}
	fnOf := func(name string) (Value, *svFunc, *closure) {
		v, err := rt.RunString("pick.js", name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f, c := rt.jitResolveCallee(v)
		if f == nil || f.jit.code == nil || f.jit.code.mentry == 0 {
			t.Fatalf("%s did not compile into something a call site may enter", name)
		}
		return v, f, c
	}
	aVal, aFn, aCl := fnOf("first")
	bVal, bFn, bCl := fnOf("second")

	site := &jitCallSite{argc: 1}
	rt.jitFillSite(site, aFn.isStrict, aVal, aFn, aCl)
	held := site.bind
	if held == nil || held.fn != aFn {
		t.Fatalf("the site did not fill with the function it was given")
	}
	rt.jitFillSite(site, bFn.isStrict, bVal, bFn, bCl)
	if site.bind == held {
		t.Fatal("a refill wrote through the record a live frame would be holding")
	}
	if held.fn != aFn || held.cl != aCl || held.code != aFn.jit.code {
		t.Errorf("the first record now describes %q rather than the function "+
			"the frame holding it is running", held.fn.name)
	}
	if site.bind.fn != bFn {
		t.Errorf("the site did not refill")
	}
}

// A callee that is rebuilt is still the same callee.
//
// A call site tolerates a bounded number of different callees before it gives
// the machine path up, which is right for a site that really is polymorphic and
// wrong for one whose callee simply recompiled itself. The two were
// indistinguishable, because the record a site holds names the block: a rebuild
// changes it, and eight rebuilds retired the site for good.
//
// The tier rebuilds a function whenever a bet it compiled in has been lost —
// the parameter check today, and anything the feedback says tomorrow. So a
// function that recompiles itself could talk every one of its callers out of
// calling it in machine code, which is worth 12% of DeltaBlue.
func TestARebuiltCalleeDoesNotRetireItsCallSites(t *testing.T) {
	saved, savedT := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 2
	defer func() { jitEnabled, jitThreshold = saved, savedT }()

	rt := New()
	if _, err := rt.RunString("rebuild.js", `
		function callee(a) { return a + 1; }
		var s = 0;
		for (var i = 0; i < 400; i++) s += callee(i);
		s;`); err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunString("pick.js", "callee")
	if err != nil {
		t.Fatal(err)
	}
	fn, cl := rt.jitResolveCallee(v)
	if fn == nil || fn.jit.code == nil || fn.jit.code.mentry == 0 {
		t.Fatal("callee did not compile into something a call site may enter")
	}

	site := &jitCallSite{argc: 1}
	rt.jitFillSite(site, fn.isStrict, v, fn, cl)
	if site.entry == 0 {
		t.Fatal("the site did not fill at all")
	}

	// Rebuild it well past the limit, refilling each time — which is what the
	// runtime does when the callee's block is replaced under a live site.
	for i := 0; i < jitSiteRebindLimit*3; i++ {
		c := jitCompile(fn, nil)
		if c == nil {
			t.Fatalf("rebuild %d: refused to compile", i)
		}
		fn.jit.retired = append(fn.jit.retired, fn.jit.code)
		fn.jit.code = c
		site.callee = 0 // a retired site, as an epoch bump would leave it
		rt.jitFillSite(site, fn.isStrict, v, fn, cl)
		if site.entry != c.mentry {
			t.Fatalf("rebuild %d: the site points at %#x, want the new block's %#x",
				i, site.entry, c.mentry)
		}
		if site.callee != v {
			t.Fatalf("rebuild %d: the site stopped describing its callee", i)
		}
	}
	if site.rebinds != 0 {
		t.Errorf("%d rebuilds of one callee counted as %d changes of mind",
			jitSiteRebindLimit*3, site.rebinds)
	}

	// And a site that really does see different callees is still stopped.
	if _, err := rt.RunString("other.js", `
		function other(a) { return a + 2; }
		for (var i = 0; i < 400; i++) other(i);`); err != nil {
		t.Fatal(err)
	}
	ov, err := rt.RunString("pick2.js", "other")
	if err != nil {
		t.Fatal(err)
	}
	ofn, ocl := rt.jitResolveCallee(ov)
	for i := 0; i <= jitSiteRebindLimit; i++ {
		if i%2 == 0 {
			rt.jitFillSite(site, ofn.isStrict, ov, ofn, ocl)
		} else {
			rt.jitFillSite(site, fn.isStrict, v, fn, cl)
		}
	}
	if site.rebinds < jitSiteRebindLimit {
		t.Errorf("a site alternating between two callees counted %d changes of mind", site.rebinds)
	}
}
