//go:build amd64

// Every test here needs a backend: it asserts that something was compiled, or
// measures what compiling it cost. Without the tag the package does not build
// for arm64 at all, which `go build` does not notice because it does not type
// check tests — `GOOS=darwin GOARCH=arm64 go vet ./internal/engine` does.

package engine

import "testing"

// TestTieringAgreesWithTheInterpreter runs whole scripts twice — once with the
// tier on and once without — and requires the same answer.
//
// The unit tests around jitCompile drive compiled code directly, which proves
// the emitter. This proves the integration: that a function crosses the
// threshold, that the frame's locals are what compiled code is handed, and that
// declining lands back in the interpreter with nothing disturbed.
func TestTieringAgreesWithTheInterpreter(t *testing.T) {
	scripts := []string{
		// Compiles, and is entered far more than the threshold.
		`function w(n,m){ var s=0,i=0; while(i<n){ s=s+i*m; i=i+1; } return s; }
		 var r=0; for (var k=0;k<50;k++) r+=w(20,1.5); r;`,
		// Compiles, then is handed something it cannot take, so every later call
		// must decline and interpret.
		`function f(a,b){ return a+b; }
		 var r=0; for (var k=0;k<40;k++) r+=f(k,1);
		 r + f("x","y").length;`,
		// Never compiles: the body calls out.
		`function g(x){ return x*2; }
		 function h(x){ return g(x)+1; }
		 var r=0; for (var k=0;k<40;k++) r+=h(k); r;`,
		// Falls off the end, so returns undefined every time.
		`function u(a){ } var r=0; for (var k=0;k<40;k++) { if (u(k) === undefined) r++; } r;`,
		// A local assigned only inside a branch, which the tier refuses.
		`function p(a){ if (a>0) { var t=a*2; return t; } return 0; }
		 var r=0; for (var k=0;k<40;k++) r+=p(k-10); r;`,
		// Property reads, which leave the tier's Number world: the result is
		// whatever the object held, so it may be stored and returned but never
		// arithmetic'd without the compiler refusing.
		`function r(o){ return o.x; }
		 var o={x:7}, s=0; for (var k=0;k<40;k++) s+=r(o); s;`,
		`function r(o){ var a=o.x; var b=o.y; return a; }
		 var o={x:1,y:2}, s=0; for (var k=0;k<40;k++) s+=r(o); s;`,
		// A read that throws, every time, once the function is compiled.
		`function r(o){ return o.x; }
		 var n=0; for (var k=0;k<40;k++) { try { r(k<20?{x:1}:null); } catch (e) { n++; } } n;`,
		// A getter, which is JavaScript re-entering the engine underneath
		// compiled code that is suspended holding its operand stack.
		`var o = { get x() { return 3; } };
		 function r(p){ return p.x; }
		 var s=0; for (var k=0;k<40;k++) s+=r(o); s;`,
		// A getter that allocates hard enough to collect while the compiled
		// frame above it is suspended holding values only its context refers to.
		`var o = { get x() { var a=[]; for (var i=0;i<200;i++) a.push({i:i}); return a.length; } };
		 function r(p){ return p.x; }
		 var s=0; for (var k=0;k<60;k++) s+=r(o); s;`,
		// Mixes Numbers and values that are not, across a branch.
		`function q(a,b){ var x=null; if (a<b) { return x; } return a-b; }
		 var r=0; for (var k=0;k<40;k++) { var v=q(k,20); r += (v===null) ? 1 : v; } r;`,
	}

	for _, src := range scripts {
		saved := jitEnabled

		jitEnabled = false
		want, errOff := New().RunString("tier.js", src)

		jitEnabled = true
		got, errOn := New().RunString("tier.js", src)

		jitEnabled = saved

		if (errOff == nil) != (errOn == nil) {
			t.Errorf("%q: interpreted err=%v, compiled err=%v", src, errOff, errOn)
			continue
		}
		if errOff != nil {
			continue
		}
		if uint64(got) != uint64(want) {
			t.Errorf("%q: compiled %#016x (%v), interpreted %#016x (%v)",
				src, uint64(got), got.Number(), uint64(want), want.Number())
		}
	}
}

// TestTieringCompilesSomething guards against the test above passing because
// nothing was ever compiled.
func TestTieringCompilesSomething(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	rt := New()
	const src = `function w(n,m){ var s=0,i=0; while(i<n){ s=s+i*m; i=i+1; } return s; }
	             var r=0; for (var k=0;k<50;k++) r+=w(20,1.5); w;`
	v, err := rt.RunString("tier.js", src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	o := rt.objPtr(v)
	if o == nil || o.clPtr == nil || o.clPtr.fn == nil {
		t.Fatal("the script's last expression was not a closure")
	}
	fn := o.clPtr.fn
	if fn.jit.code == nil {
		t.Errorf("w was entered 50 times and never compiled (tried=%v, count=%d)",
			fn.jit.tried, fn.jit.count)
	}
}

// TestJITStopsCheckingParametersItKeepsTurningAway is the decline backoff.
//
// The prologue's parameter check is a bet that the caller passes Numbers, and it
// is worth making — a checked parameter needs neither a type guard nor a call
// out. What was missing is the other side: a function whose callers pass objects
// used to enter compiled code, refuse, and hand the frame back, for every call
// for the life of the program. Richards did that 195,342 times against 639
// entries that ran.
func TestJITStopsCheckingParametersItKeepsTurningAway(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	// Arithmetic on the parameter, so the prologue demands it, and a caller that
	// passes a String every time — which the interpreter turns into a number and
	// the prologue turns away.
	const body = `function w(a){ return a * 2 + 1; }
	              var r = 0; for (var k = 0; k < 400; k++) r += w("3");`

	// The answer the interpreter gives, before anything is compiled.
	jitEnabled = false
	want, err := New().RunString("decline.js", body+" r;")
	if err != nil {
		t.Fatalf("interpreted run: %v", err)
	}
	jitEnabled = true

	rt := New()
	const src = body + " w;"
	v, err := rt.RunString("decline.js", src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	o := rt.objPtr(v)
	if o == nil || o.clPtr == nil || o.clPtr.fn == nil {
		t.Fatal("the script's last expression was not a closure")
	}
	fn := o.clPtr.fn
	if fn.jit.code == nil {
		t.Fatal("w was never compiled, so this proves nothing about declining")
	}
	if !fn.jit.unchecked {
		t.Errorf("w declined %d times and still checks its parameters", fn.jit.declines)
	}
	// And it must still be right: the rebuilt code accepts the String, and the
	// generic multiply gives what the interpreter gave.
	got, err := rt.RunString("check.js", "r;")
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if uint64(got) != uint64(want) {
		t.Errorf("compiled %v, interpreted %v", got, want)
	}
}

// TestJITFrameEntryDoesNotAllocate guards the cost of entering compiled code.
//
// The context compiled code shares with the runtime is 160 bytes, and building
// one per call is most of what compiling a small function saved: `dist` in
// docs/jit-plan.md pays it two million times and comes out behind the
// interpreter with a cache serving every one of its reads. The root stack it
// lives on is LIFO, so it is its own free list — and an allocation reappearing
// here is that regression, which nothing else would show.
func TestJITFrameEntryDoesNotAllocate(t *testing.T) {
	rt, fn, c := jitField(t, "x")
	o := rt.newObject(rt.objectProto)
	rt.objPtr(o).defineOwn("x", tov(7), attrDefault)
	locals := make([]Value, fn.maxLocals)
	locals[0] = o

	// Once first, so the cache is warm and the first-call fill is not measured.
	if _, e := jitGet(t, rt, fn, c, o); e != nil {
		t.Fatal("threw")
	}
	if n := testing.AllocsPerRun(200, func() {
		if _, _, ok := c.jitRun(rt, fn, nil, 0, nil, locals, mkundef()); !ok {
			t.Fatal("declined")
		}
	}); n != 0 {
		t.Errorf("entering compiled code allocates %v times per call", n)
	}
}
