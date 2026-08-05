//go:build amd64 || arm64

package engine

import (
	"testing"
)

// jitBailPrograms are bodies to hand back from every point in.
//
// Chosen for the state a bail has to carry rather than for what they compute:
// operands live across the bail, locals written before it and read after,
// a loop whose counter is half-advanced, a property read whose receiver is on
// the stack, a call whose arguments are. Each has to be pure, because the sweep
// runs it once per instruction and compares every answer with the same one.
var jitBailPrograms = []struct {
	src  string
	args []Value
}{
	{"function f(a,b){ return a+b; }", []Value{tov(3), tov(4)}},
	{"function f(a,b){ return (a+b)*(a-b); }", []Value{tov(9), tov(2)}},
	{"function f(a,b){ var t = a*b; return t+t+a; }", []Value{tov(3), tov(5)}},
	{"function f(a,b){ if (a<b) { return a; } return b; }", []Value{tov(1), tov(2)}},
	{"function f(a,b){ var s=0, i=0; while (i<a) { s=s+i*b; i=i+1; } return s; }", []Value{tov(10), tov(3)}},
	{"function f(a,b){ var s=0; for (var i=0; i<a; i=i+1) { s=s+i; } return s+b; }", []Value{tov(7), tov(100)}},
	// An operand stack that is not empty where the bail lands: the multiply's
	// left side is computed and waiting while the right side is.
	{"function f(a,b){ return a*(b+(a*(b+a))); }", []Value{tov(2), tov(3)}},
	// A receiver and a property, so the bail can land between the two.
	{"function f(a,b){ var o = {x: a, y: b}; return o.x + o.y; }", []Value{tov(11), tov(22)}},
	// A call, so the bail can land with the callee and its arguments stacked.
	{"function f(a,b){ function g(x,y){ return x-y; } return g(a,b) + g(b,a); }", []Value{tov(8), tov(5)}},
	// A local that holds something that is not a Number, which the resumed
	// interpreter has to find as compiled code left it.
	{"function f(a,b){ var x = true; var s = 0; if (x) { s = a+b; } return s; }", []Value{tov(4), tov(6)}},
	// The two that live in the published frame rather than in the locals, and
	// so are the two a resume can silently put back to what entry computed. Both
	// of these passed the sweep above and failed test262, which is the reason
	// they are here: the chain and the variable object are only wrong when the
	// body has moved them on, and none of the programs above has either.
	{"function f(a,b){ var o = {x: a}; with (o) { return x + b; } }", []Value{tov(5), tov(6)}},
	{"function f(a,b){ var s = a + b; return eval('s + a'); }", []Value{tov(2), tov(3)}},
}

// jitBodyOffsets is every bytecode offset in fn's body, in order.
func jitBodyOffsets(t testing.TB, fn *svFunc) []int {
	t.Helper()
	var out []int
	for ip := fn.startIP; ip < len(fn.code); {
		size := int(opTable[Opcode(fn.code[ip])].Size)
		if size <= 0 {
			t.Fatalf("undecodable opcode at %d", ip)
		}
		out = append(out, ip)
		ip += size
	}
	return out
}

// jitBailRun runs src's f with the tier on and a bail planted at one offset.
func jitBailRun(t testing.TB, src string, bailAt int, args []Value) (Value, uint64) {
	t.Helper()
	defer jitBailSettings(true, 1, bailAt)()
	rt := New()
	fnVal, err := rt.RunString("bail.js", src+"; f;")
	if err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	before := jitStats.bails
	v, e := rt.callValue(fnVal, mkundef(), args)
	if e != nil {
		t.Fatalf("call %q with a bail at %d: threw", src, bailAt)
	}
	return v, jitStats.bails - before
}

// jitBailSettings turns the tier on with a bail planted, and hands back the
// restore. The three are package variables rather than environment variables
// precisely so a sweep can move them between runs.
func jitBailSettings(on bool, threshold int32, bailAt int) func() {
	oldOn, oldT, oldAt := jitEnabled, jitThreshold, jitBailAt
	jitEnabled, jitThreshold, jitBailAt = on, threshold, bailAt
	return func() { jitEnabled, jitThreshold, jitBailAt = oldOn, oldT, oldAt }
}

// TestAFrameHandedBackAtAnyPointFinishesTheSame is the whole claim of jitbail.go,
// checked one instruction at a time.
//
// A bail is unlike anything else in the tier in that it cannot be tested where
// it will be used: every real one sits behind a guard written not to fail, so
// the interesting paths are the ones no corpus reaches. And it fails quietly —
// a handover that loses an operand, or resumes one instruction late, computes a
// number rather than crashing.
//
// So the guard is removed from the question. Plant an unconditional bail before
// each instruction of the body in turn, run the function through the whole
// engine, and require the answer the interpreter alone gives — bit for bit,
// because that is the claim, not "close enough".
func TestAFrameHandedBackAtAnyPointFinishesTheSame(t *testing.T) {
	for _, prog := range jitBailPrograms {
		t.Run(prog.src, func(t *testing.T) {
			want := interpret(t, prog.src, prog.args...)

			// The offsets are read from the function this runtime compiled
			// rather than from a separately compiled copy, so they are the
			// offsets the sweep will actually plant at.
			rt := New()
			fnVal, err := rt.RunString("bail.js", prog.src+"; f;")
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			fn, _ := rt.jitResolveCallee(fnVal)
			if fn == nil {
				t.Fatalf("no function behind f")
			}
			offsets := jitBodyOffsets(t, fn)

			fired := uint64(0)
			for _, at := range offsets {
				got, n := jitBailRun(t, prog.src, at, prog.args)
				if uint64(got) != uint64(want) {
					t.Fatalf("bail at %d (%s): got %v (%#x), interpreter says %v (%#x)",
						at, Opcode(fn.code[at]), got.Number(), uint64(got),
						want.Number(), uint64(want))
				}
				fired += n
			}
			// Without this the test passes just as well when nothing compiled:
			// every answer would be the interpreter's because every run was.
			if fired == 0 {
				t.Fatalf("no bail fired across %d offsets — the sweep proved nothing", len(offsets))
			}
			t.Logf("%d offsets, %d bails taken", len(offsets), fired)
		})
	}
}

// TestACalleeEnteredInMachineCodeCanStillBail is the half of the handover that
// has no interpreted frame under it.
//
// A frame a compiled call site opened lives in a context and nothing else — no
// vmFrame, no locals slab, nothing on the Go stack below it — so finishing one
// in the interpreter means building the frame first. Withholding the machine
// entry instead was the first answer, and it was the wrong one: it made
// speculating in a function cost that function its compiled call.
//
// The caller here is machine code suspended mid-call. It must get its answer in
// Ret and resume where it saved, exactly as if the callee had returned — so the
// check is that the whole expression is still the interpreter's.
func TestACalleeEnteredInMachineCodeCanStillBail(t *testing.T) {
	// The callee is entered in a loop so its site fills and the call is made in
	// machine code rather than through the runtime; the loop is what makes this
	// test about the machine call rather than about jitCallCompiled.
	const src = `function f(n){
		function g(x,y){ var t = x*y; return t+x; }
		var s = 0;
		for (var i = 0; i < n; i = i + 1) { s = s + g(i, 3); }
		return s;
	}`
	want := interpret(t, src, tov(200))

	rt := New()
	fnVal, err := rt.RunString("bail.js", src+"; f;")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	outer, _ := rt.jitResolveCallee(fnVal)
	if outer == nil || len(outer.childFuncs) != 1 {
		t.Fatalf("want f with one inner function")
	}
	callee := outer.childFuncs[0]

	// A machine entry for the callee is what the test needs to exist at all.
	restore := jitBailSettings(true, 1, -1)
	c := jitCompile(callee, nil)
	restore()
	callee.jit = jitAttempt{}
	if c == nil || c.mentry == 0 {
		t.Fatalf("the callee has no machine entry, so this test proves nothing")
	}

	// The machine-call counter is emitted only when the diagnostics are on, and
	// it is emitted at compile time — so this has to be set before the runs
	// below compile anything, not merely before they are read.
	oldStats := jitStats.enabled
	jitStats.enabled = true
	defer func() { jitStats.enabled = oldStats }()

	bails, withCall := uint64(0), 0
	for _, at := range jitBodyOffsets(t, callee) {
		before := jitStats.callFast
		got, n := jitBailRun(t, src, at, []Value{tov(200)})
		if uint64(got) != uint64(want) {
			t.Fatalf("callee bail at %d (%s): got %v, interpreter says %v",
				at, Opcode(callee.code[at]), got.Number(), want.Number())
		}
		bails += n
		if n > 0 && jitStats.callFast > before {
			withCall++
		}
	}
	if bails == 0 {
		t.Fatalf("no bail fired at all across %d offsets", len(jitBodyOffsets(t, callee)))
	}
	if withCall == 0 {
		t.Fatalf("%d bails fired but never with a machine call live", bails)
	}
	t.Logf("%d bails, %d offsets with a machine call live", bails, withCall)
}

// TestABailInsideATryIsRefused pins the one shape the handover does not cover.
//
// The interpreter's handler stack is a Go local of runFrameBody, so a frame
// resumed inside a try would run with none — and a body that does not throw
// behaves identically either way, which is exactly why this is a compile-time
// refusal rather than something left to be noticed.
func TestABailInsideATryIsRefused(t *testing.T) {
	const src = "function f(a,b){ try { return a+b; } catch (e) { return 0; } }"
	rt := New()
	fnVal, err := rt.RunString("bail.js", src+"; f;")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	fn, _ := rt.jitResolveCallee(fnVal)
	if fn == nil {
		t.Fatalf("no function behind f")
	}
	refused := false
	for _, at := range jitBodyOffsets(t, fn) {
		restore := jitBailSettings(true, 1, at)
		var why string
		if jitCompile(fn, &why) == nil && why == "bail-inside-try" {
			refused = true
		}
		restore()
		fn.jit = jitAttempt{}
	}
	if !refused {
		t.Fatalf("a try body accepted a bail at every offset")
	}
}
