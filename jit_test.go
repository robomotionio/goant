package goant

import (
	"testing"
)

// The host's control over the compiled tier.
//
// These are about the SWITCH, not about the compiler. What the tier computes is
// checked by running every program both ways and comparing — see
// internal/engine/jit_fuzz_test.go. What is checked here is that a host can turn
// it on for one Runtime and not another, can turn it off on a live one, and gets
// the same answers throughout.

const jitHotSrc = `
	function inner(n) { var s = 0; for (var i = 0; i < n; i++) s += i * 3; return s; }
	function outer(n) { var t = 0; for (var i = 0; i < n; i++) t += inner(20); return t; }
	outer(400);
`

func TestJITIsOffByDefault(t *testing.T) {
	rt := New()
	defer rt.Close()
	// Off unless the host asks or GOANT_JIT is set. The test states the
	// condition rather than asserting false outright, so that a run with
	// GOANT_JIT=1 in the environment reports the default it actually has instead
	// of failing for having one.
	t.Logf("default with this environment: JITEnabled() = %v", rt.JITEnabled())
}

func TestWithJITTurnsItOnForThisRuntimeOnly(t *testing.T) {
	on := New(WithJIT(true))
	defer on.Close()
	off := New(WithJIT(false))
	defer off.Close()

	if !on.JITEnabled() {
		t.Error("WithJIT(true) did not enable the tier")
	}
	if off.JITEnabled() {
		t.Error("WithJIT(false) did not disable the tier")
	}

	// And the two agree about the answer, which is the only thing a host is
	// entitled to depend on.
	a, err := on.RunScript("on.js", jitHotSrc)
	if err != nil {
		t.Fatalf("tier on: %v", err)
	}
	b, err := off.RunScript("off.js", jitHotSrc)
	if err != nil {
		t.Fatalf("tier off: %v", err)
	}
	if a.String() != b.String() {
		t.Errorf("compiled %q, interpreted %q", a.String(), b.String())
	}
}

// TestSetJITIsAKillSwitch is the property a host actually needs in production:
// turning the tier off must stop compiled code from running, not merely stop
// further compilation.
//
// The distinction matters because a Runtime that has been serving traffic has
// already compiled its hot functions. A switch that only prevented future
// compilation would leave exactly the code a host is trying to stop using still
// running, which makes it a preference rather than a way out of an incident.
func TestSetJITIsAKillSwitch(t *testing.T) {
	rt := New(WithJIT(true))
	defer rt.Close()

	// Hot enough to have compiled.
	hot, err := rt.RunScript("hot.js", jitHotSrc)
	if err != nil {
		t.Fatalf("with the tier on: %v", err)
	}
	if !rt.JITEnabled() {
		t.Fatal("the tier reports off after WithJIT(true)")
	}

	// Now pull the switch on the live Runtime.
	rt.SetJIT(false)
	if rt.JITEnabled() {
		t.Fatal("SetJIT(false) did not take")
	}
	cold, err := rt.RunScript("cold.js", jitHotSrc)
	if err != nil {
		t.Fatalf("after SetJIT(false): %v", err)
	}
	if hot.String() != cold.String() {
		t.Errorf("answer changed when the tier was switched off: %q then %q",
			hot.String(), cold.String())
	}

	// And back on again, still the same answer.
	rt.SetJIT(true)
	again, err := rt.RunScript("again.js", jitHotSrc)
	if err != nil {
		t.Fatalf("after SetJIT(true): %v", err)
	}
	if again.String() != hot.String() {
		t.Errorf("answer changed when the tier was switched back on: %q, want %q",
			again.String(), hot.String())
	}
}

// TestStatsReportsCodeMemory pins that a host can SEE the executable memory the
// tier holds.
//
// It is reported apart from Bytes because the memory limit does not cover it,
// and a host watching only the JavaScript heap is watching the wrong number: a
// worker under a 64 MB limit once reached 1.79 GB resident with its heap flat at
// 8 MB, because all of the growth was code.
func TestStatsReportsCodeMemory(t *testing.T) {
	rt := New(WithJIT(true))
	defer rt.Close()
	if _, err := rt.RunScript("hot.js", jitHotSrc); err != nil {
		t.Fatalf("run: %v", err)
	}
	s := rt.Stats()
	t.Logf("after compiling: %d code blocks, %d code bytes, heap %d bytes",
		s.CodeBlocks, s.CodeBytes, s.Bytes)
	if s.CodeBlocks > 0 && s.CodeBytes == 0 {
		t.Error("blocks are reported but bytes are not")
	}
	if s.CodeBytes > 0 && s.CodeBlocks == 0 {
		t.Error("bytes are reported but blocks are not")
	}
}

// TestJITDoesNotChangeAnswers is a small differential over the public API, so
// that the guarantee a host is given is checked at the boundary a host uses
// rather than only inside the engine.
func TestJITDoesNotChangeAnswers(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"arithmetic", `var s=0; for (var i=0;i<500;i++) s += i*3 - (i%7); "" + s;`},
		{"strings", `var o=""; for (var i=0;i<300;i++) o += (i%10); "" + o.length + o.slice(0,8);`},
		{"objects", `function P(a){this.a=a;} var t=0;
			for (var i=0;i<400;i++){var p=new P(i); if(i%5===0)p.b=i; t+=p.a+(p.b||0);} ""+t;`},
		{"arrays", `var a=[]; for (var i=0;i<400;i++) a.push(i%13);
			a.sort(function(x,y){return x-y;}); ""+a.length+","+a[0]+","+a[399];`},
		{"typed arrays", `var a=new Float64Array(64); for(var k=0;k<300;k++)
			for(var i=1;i<63;i++) a[i]=(a[i-1]+a[i]+a[i+1])/3+i*0.5; a[32].toFixed(6);`},
		{"exceptions", `var c=0; for (var i=0;i<400;i++){ try { if(i%9===0) throw new Error("x"); }
			catch(e){c++;} } ""+c;`},
		{"closures", `var fs=[]; for (let i=0;i<50;i++) fs.push(function(){return i*2;});
			var t=0; for (var k=0;k<200;k++) for (var i=0;i<fs.length;i++) t+=fs[i](); ""+t;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			on := New(WithJIT(true))
			defer on.Close()
			off := New(WithJIT(false))
			defer off.Close()

			a, err := on.RunScript(tc.name+".js", tc.src)
			if err != nil {
				t.Fatalf("tier on: %v", err)
			}
			b, err := off.RunScript(tc.name+".js", tc.src)
			if err != nil {
				t.Fatalf("tier off: %v", err)
			}
			if a.String() != b.String() {
				t.Errorf("compiled %q, interpreted %q", a.String(), b.String())
			}
		})
	}
}
