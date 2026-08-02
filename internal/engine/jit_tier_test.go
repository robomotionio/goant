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
