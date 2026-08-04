//go:build amd64 || arm64

package engine

import (
	"math"
	"strconv"
	"testing"
)

// jitRunT enters compiled code from a test, discarding the throw channel that
// only the opcodes calling into the runtime can use.
func jitRunT(t testing.TB, rt *Runtime, c *jitCode, fn *svFunc, locals []Value) (Value, bool) {
	t.Helper()
	v, e, ok := c.jitRun(rt, fn, nil, 0, nil, locals, mkundef())
	if e != nil {
		t.Fatalf("compiled code threw")
	}
	return v, ok
}

// jitFn compiles src, which must declare exactly one function, and returns that
// function's bytecode.
func jitFn(t testing.TB, src string) *svFunc {
	_, fn := jitFnRT(t, src)
	return fn
}

// jitFnRT is jitFn plus the Runtime the function was compiled in, which
// compiled code needs once it can call back into the engine.
func jitFnRT(t testing.TB, src string) (*Runtime, *svFunc) {
	t.Helper()
	rt := New()
	prog, err := Parse("jit_test.js", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	top, err := rt.Compile(prog, "jit_test.js", src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(top.childFuncs) != 1 {
		t.Fatalf("want exactly one function, got %d", len(top.childFuncs))
	}
	return rt, top.childFuncs[0]
}

// interpret runs src's function f through the whole engine with exactly these
// argument Values, so the comparison below is against goant itself rather than
// against Go arithmetic that merely ought to agree with it.
func interpret(t testing.TB, src string, args ...Value) Value {
	t.Helper()
	rt := New()
	fnVal, err := rt.RunString("interp.js", src+"; f;")
	if err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	v, e := rt.callValue(fnVal, mkundef(), args)
	if e != nil {
		t.Fatalf("call %q: threw", src)
	}
	return v
}

// jitStraightLine programs have no loop, so they can be fed the pathological
// values — infinities, NaN, signed zero, the extremes of the range.
var jitStraightLine = []string{
	"function f(a,b){ return a+b; }",
	"function f(a,b){ return a-b; }",
	"function f(a,b){ return a*b; }",
	"function f(a,b){ return a/b; }",
	"function f(a,b){ return (a+b)*(a-b); }",
	"function f(a,b){ return a*b+a; }",
	"function f(a,b){ return a+1; }",
	"function f(a,b){ return a/b/b; }",
	"function f(a,b){ a = a*2; return a+b; }",
	"function f(a,b){ var t = a*b; return t+t; }",
	// Values that are not Numbers. They never reach an arithmetic instruction,
	// so they need no guard — only somewhere to sit while they travel.
	"function f(a,b){ }",
	"function f(a,b){ return undefined; }",
	"function f(a,b){ return null; }",
	"function f(a,b){ return true; }",
	"function f(a,b){ return false; }",
	"function f(a,b){ var x = true; return x; }",
	"function f(a,b){ var x = null; if (a<b) { return x; } return a+b; }",
}

// jitLoops are counted loops, so their inputs have to be values they terminate
// on — an infinity as the bound would hang the interpreter just as surely as the
// compiled code.
var jitLoops = []string{
	"function f(n,m){ var s=0, i=0; while (i<n) { s=s+i; i=i+1; } return s; }",
	"function f(n,m){ var s=1, i=0; while (i<n) { s=s*m; i=i+1; } return s; }",
	"function f(n,m){ var s=0, i=n; while (i>0) { s=s+m; i=i-1; } return s; }",
	"function f(n,m){ var s=0; for (var i=0; i<n; i=i+1) { s=s+i*m; } return s; }",
	"function f(n,m){ var s=0, i=0; while (i<=n) { s=s+m; i=i+1; } return s; }",
	// Declared inside the loop: only the control-flow analysis can prove it
	// assigned before it is read, which the straight-line prefix rule could not.
	"function f(n,m){ var s=0, i=0; while (i<n) { var t=i*m; s=s+t; i=i+1; } return s; }",
	"function f(n,m){ var s=0, i=0; while (i<n) { var t=i+1; var u=t*m; s=s+u; i=t; } return s; }",
}

var jitWildInputs = []struct{ a, b float64 }{
	{1, 2}, {2.5, 0.5}, {-3, 7}, {0, 0}, {1, 0}, {-1, 0},
	{math.Inf(1), math.Inf(1)}, {math.Inf(1), math.Inf(-1)},
	{math.NaN(), 1}, {1, math.NaN()},
	{math.MaxFloat64, math.MaxFloat64}, {math.SmallestNonzeroFloat64, 2},
	{-0, 5}, {5, -0}, {10, 3}, {3.5, 1.25},
}

var jitLoopInputs = []struct{ a, b float64 }{
	{0, 1}, {1, 1}, {2, 3}, {7, 0.5}, {10, -2}, {-1, 4}, {5, math.NaN()},
	{0.5, 2}, {33, 1.25},
}

// TestJITAgreesWithTheInterpreter is the gate. Compiled code has to produce not
// merely the right number but the same Value the interpreter would have pushed,
// bit for bit — a stricter claim, and the one that catches a NaN whose pattern
// strays into the tag space.
func TestJITAgreesWithTheInterpreter(t *testing.T) {
	check := func(src string, inputs []struct{ a, b float64 }) {
		rt, fn := jitFnRT(t, src)
		c := jitCompile(fn, nil)
		if c == nil {
			t.Errorf("%s: refused to compile", src)
			return
		}
		defer c.free()
		for _, in := range inputs {
			av, bv := tov(in.a), tov(in.b)
			locals := make([]Value, fn.maxLocals)
			locals[0], locals[1] = av, bv
			got, ok := jitRunT(t, rt, c, fn, locals)
			if !ok {
				t.Errorf("%s(%v,%v): bailed on two Numbers", src, in.a, in.b)
				continue
			}
			want := interpret(t, src, av, bv)
			if uint64(got) != uint64(want) {
				t.Errorf("%s(%v,%v) = %#016x (%v), interpreter gives %#016x (%v)",
					src, in.a, in.b, uint64(got), got.Number(), uint64(want), want.Number())
			}
		}
	}
	for _, src := range jitStraightLine {
		check(src, jitWildInputs)
	}
	for _, src := range jitLoops {
		check(src, jitLoopInputs)
	}
	for _, src := range jitIntegerOps {
		check(src, jitWildInputs)
		check(src, jitIntegerInputs)
	}
}

// jitIntegerOps are the operators defined on 32 bits rather than on doubles,
// plus the ones that produce a Boolean. What makes them worth a list of their
// own is that every one of them goes through a conversion the arithmetic
// operators do not, and that conversion has a range it cannot do in a register.
var jitIntegerOps = []string{
	"function f(a,b){ return a&b; }",
	"function f(a,b){ return a|b; }",
	"function f(a,b){ return a^b; }",
	"function f(a,b){ return a<<b; }",
	"function f(a,b){ return a>>b; }",
	"function f(a,b){ return a>>>b; }",
	"function f(a,b){ return ~a; }",
	"function f(a,b){ return ~(a&b); }",
	"function f(a,b){ return (a|0)+(b|0); }",
	"function f(a,b){ return (a>>>0)&255; }",
	"function f(a,b){ return ((a<<b)|(a>>>b))&4095; }",
	// Two conversions in one expression, so the second runs while the first has
	// already left a bare integer in a register the collector may scan.
	"function f(a,b){ return (a&b)|(a^b); }",
	"function f(a,b){ return -a; }",
	"function f(a,b){ return -(a*b); }",
	"function f(a,b){ return !a; }",
	"function f(a,b){ return !(a-b); }",
	"function f(a,b){ return a===b; }",
	"function f(a,b){ return a!==b; }",
	"function f(a,b){ return a==b; }",
	"function f(a,b){ return a!=b; }",
	"function f(a,b){ if (a===b) { return 1; } return 2; }",
	"function f(a,b){ if (a!==b) { return 1; } return 2; }",
	"function f(a,b){ if (!(a===b)) { return 1; } return 2; }",
	"function f(a,b){ var x = a===b; return x; }",
}

// jitIntegerInputs are chosen for the conversion rather than for the operator.
//
// The interesting ones are at 2^63, where CVTTSD2SI stops being able to answer
// and the compiled code has to leave for the runtime mid-expression, and just
// below it, where it still can. Shift counts above 31 and negative ones are here
// because the specification masks them to five bits and a compiler that emitted
// a 64-bit shift would not.
var jitIntegerInputs = []struct{ a, b float64 }{
	{0, 0}, {1, 1}, {255, 8}, {-1, 3}, {-256, 4},
	{2147483647, 1}, {-2147483648, 1}, {2147483648, 1}, {4294967295, 1},
	{1e9, 7}, {-1e9, 7}, {1e10, 3}, {1e15, 2},
	// At and beyond what a register conversion can do.
	{9223372036854775808, 1},  // 2^63 exactly
	{9223372036854777856, 1},  // 2^63 + 2^11: the low bits survive the reduction
	{-9223372036854775808, 1}, // -2^63
	{1e20, 3}, {-1e20, 3}, {1e30, 5}, {1.5e300, 2},
	{9007199254740993, 1}, {4503599627370497, 1},
	// Shift counts the mask has to fold.
	{1, 32}, {1, 33}, {1, 63}, {1, 64}, {1, -1}, {255, 100},
	{-1, 31}, {-1, 32},
	// Values that are not integers at all.
	{2.5, 1.5}, {-2.5, 1.5}, {0.9, 0.9}, {-0.9, 3},
	{math.NaN(), 1}, {1, math.NaN()}, {math.Inf(1), 1}, {math.Inf(-1), 1},
	{-0, 0}, {0, -0},
}

// TestJITLoopOutlivesItsFuel drives a loop past the point where compiled code
// hands control back, which is the only safepoint it has. Getting this wrong
// shows up as a wrong answer rather than a hang, because the resume path has to
// re-establish the pinned registers and pick up where it left off.
func TestJITLoopOutlivesItsFuel(t *testing.T) {
	const src = "function f(n,m){ var s=0, i=0; while (i<n) { s=s+i; i=i+1; } return s; }"
	rt, fn := jitFnRT(t, src)
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatal("refused to compile")
	}
	defer c.free()

	for _, n := range []float64{0, 1, 10, jitFuel - 1, jitFuel, jitFuel + 1, 3*jitFuel + 7} {
		locals := make([]Value, fn.maxLocals)
		locals[0], locals[1] = tov(n), tov(0)
		got, ok := jitRunT(t, rt, c, fn, locals)
		if !ok {
			t.Fatalf("n=%v bailed", n)
		}
		want := n * (n - 1) / 2
		if got.Number() != want {
			t.Errorf("sum to %v = %v, want %v", n, got.Number(), want)
		}
	}
}

// TestJITCanonicalizesNaN pins the specific hazard. x86 hands back
// 0xFFF8000000000000 for 0/0, which is above the NaN-box tag threshold: stored
// raw it would read as a tagged value and the rest of the engine would treat a
// number as an object.
func TestJITCanonicalizesNaN(t *testing.T) {
	rt, fn := jitFnRT(t, "function f(a,b){ return a/b; }")
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatal("refused to compile")
	}
	defer c.free()

	locals := make([]Value, fn.maxLocals)
	locals[0], locals[1] = tov(0), tov(0)
	got, ok := jitRunT(t, rt, c, fn, locals)
	if !ok {
		t.Fatal("bailed")
	}
	// Reading as a Number at all is the point: without the canonicalisation the
	// bits would be 0xFFF8000000000000, which is above the tag threshold and so
	// would be taken for a tagged value everywhere downstream.
	if !got.IsNumber() {
		t.Fatalf("0/0 produced %#016x, which does not read as a Number", uint64(got))
	}
	if !math.IsNaN(got.Number()) {
		t.Errorf("0/0 = %v, want NaN", got.Number())
	}

	// The interpreter is the reference, not any particular NaN constant: Go's
	// math.NaN() is 0x7FF8000000000001, which is a different pattern again and
	// would make this test agree with nothing.
	ref := New()
	want, err := ref.RunString("nan.js", "function f(a,b){ return a/b; } f(0,0);")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if uint64(got) != uint64(want) {
		t.Errorf("0/0 = %#016x, interpreter gives %#016x", uint64(got), uint64(want))
	}
}

// TestJITBailsOnNonNumbers checks the guard. Compiled code handles Numbers and
// hands everything else back to the interpreter; because it emits nothing with
// a side effect, re-running from the top is always correct.
func TestJITBailsOnNonNumbers(t *testing.T) {
	rt, fn := jitFnRT(t, "function f(a,b){ return a+b; }")
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatal("refused to compile")
	}
	defer c.free()

	for _, bad := range []Value{mkundef(), mknull(), mkbool(true), tEmpty} {
		locals := make([]Value, fn.maxLocals)
		locals[0], locals[1] = bad, tov(1)
		if _, ok := jitRunT(t, rt, c, fn, locals); ok {
			t.Errorf("did not bail on a %v operand", bad.Type())
		}
		locals[0], locals[1] = tov(1), bad
		if _, ok := jitRunT(t, rt, c, fn, locals); ok {
			t.Errorf("did not bail on a %v operand in the second position", bad.Type())
		}
	}
}

// TestJITRefusesWhatItCannotModel checks the other half of the contract:
// refusing is normal, and anything outside straight-line Number arithmetic has
// to be refused rather than mis-compiled.
func TestJITRefusesWhatItCannotModel(t *testing.T) {
	for _, src := range []string{
		"function f(a,b){ try { return a; } finally { b = 1; } }",  // a finally
		"function f(a,b){ return new.target; }",                    // new.target
		"function f(a,b){ for (var v of a) { b = v; } return b; }", // a live iterator
	} {
		if c := jitCompile(jitFn(t, src), nil); c != nil {
			c.free()
			t.Errorf("compiled %q, which it should have refused", src)
		}
	}
}

// TestJITMatchesRunningTheSource is the end-to-end check: the same expression
// through the whole engine, and through compiled code, agreeing.
func TestJITMatchesRunningTheSource(t *testing.T) {
	const src = "function f(a,b){ return (a+b)*(a-b)/b; }"
	rt, fn := jitFnRT(t, src)
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatal("refused to compile")
	}
	defer c.free()

	ref := New()
	if _, err := ref.RunString("jit_test.js", src); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, in := range []struct{ a, b float64 }{{7, 3}, {1.5, 0.25}, {-4, 9}} {
		v, err := ref.RunString("call.js", src+"; f("+ftoa(in.a)+","+ftoa(in.b)+");")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		locals := make([]Value, fn.maxLocals)
		locals[0], locals[1] = tov(in.a), tov(in.b)
		got, ok := jitRunT(t, rt, c, fn, locals)
		if !ok {
			t.Fatalf("f(%v,%v) bailed", in.a, in.b)
		}
		if uint64(got) != uint64(v) {
			t.Errorf("f(%v,%v): compiled %v, interpreted %v", in.a, in.b, got.Number(), v.Number())
		}
	}
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// BenchmarkJITvsInterpreter is what all of this is for.
func BenchmarkJITvsInterpreter(b *testing.B) {
	const src = "function f(a,b){ return (a+b)*(a-b)/b+a*b; }"
	rt, fn := jitFnRT(b, src)
	c := jitCompile(fn, nil)
	if c == nil {
		b.Fatal("refused to compile")
	}
	defer c.free()

	locals := make([]Value, fn.maxLocals)
	locals[0], locals[1] = tov(7), tov(3)

	b.Run("compiled", func(b *testing.B) {
		for b.Loop() {
			if _, ok := jitRunT(b, rt, c, fn, locals); !ok {
				b.Fatal("bailed")
			}
		}
	})

	b.Run("interpreted", func(b *testing.B) {
		benchInterpretedCall(b, src)
	})

	// The comparison above is not like for like: entering compiled code costs a
	// trampoline, while calling the interpreted function costs a whole frame —
	// allocation, argument binding, prologue. This is that floor, measured with
	// a body small enough to be nearly all overhead, so the difference the
	// compiled arithmetic actually accounts for can be read off rather than
	// assumed.
	b.Run("interpreted-call-overhead", func(b *testing.B) {
		benchInterpretedCall(b, "function f(a,b){ return a; }")
	})
}

// BenchmarkJITLoop is the shape the tier was extended for: hot numeric code
// that spends its time going round rather than being called.
func BenchmarkJITLoop(b *testing.B) {
	const src = "function f(n,m){ var s=0, i=0; while (i<n) { s=s+i*m; i=i+1; } return s; }"
	rt, fn := jitFnRT(b, src)
	c := jitCompile(fn, nil)
	if c == nil {
		b.Fatal("refused to compile")
	}
	defer c.free()

	locals := make([]Value, fn.maxLocals)
	locals[0], locals[1] = tov(1000), tov(1.5)

	b.Run("compiled", func(b *testing.B) {
		for b.Loop() {
			if _, ok := jitRunT(b, rt, c, fn, locals); !ok {
				b.Fatal("bailed")
			}
		}
	})
	b.Run("interpreted", func(b *testing.B) {
		benchInterpretedCall(b, src, tov(1000), tov(1.5))
	})
}

func benchInterpretedCall(b *testing.B, src string, args ...Value) {
	b.Helper()
	rt := New()
	if len(args) == 0 {
		args = []Value{tov(7), tov(3)}
	}
	fnVal, err := rt.RunString("bench.js", src+"; f;")
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, e := rt.callValue(fnVal, mkundef(), args); e != nil {
			b.Fatal("throw")
		}
	}
}

// TestJITEntersARunningLoop covers the trigger rather than the code: a function
// called once, whose loop is hot, has to end up compiled anyway.
//
// Without an on-stack entry this is the case a call-count tier cannot see at
// all, and the loop runs to completion in the interpreter however long it takes.
func TestJITEntersARunningLoop(t *testing.T) {
	const src = "function f(n){ var s=0, i=0; while (i<n) { s=(s+i*3)|0; i=i+1; } return s; }"
	fn := jitFn(t, src)
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatal("refused to compile")
	}
	defer c.free()
	if len(c.osr) == 0 {
		t.Fatal("no loop-header entry stub was emitted")
	}

	// Enter at the header with the locals an interpreter would have left
	// part-way through, and check the whole loop still finishes correctly.
	for _, start := range []float64{0, 1, 7, 1000} {
		const n = 5000
		var header int
		for h := range c.osr {
			header = h
		}
		locals := make([]Value, fn.maxLocals)
		locals[0] = tov(n)
		// s and i as of `start` iterations, computed the way the body would.
		var s float64
		for k := float64(0); k < start; k++ {
			s = float64(int32(s + k*3))
		}
		sSlot, iSlot := jitVarSlots(fn)
		locals[sSlot], locals[iSlot] = tov(s), tov(start)
		got, _, ok := c.jitRunOSR(New(), fn, nil, 0, nil, locals, mkundef(), header)
		if !ok {
			t.Fatalf("start=%v: the entry stub declined Numbers", start)
		}
		want := s
		for k := start; k < n; k++ {
			want = float64(int32(want + k*3))
		}
		if got.Number() != want {
			t.Errorf("start=%v: resumed loop gave %v, want %v", start, got.Number(), want)
		}
	}
}

// TestJITLoopEntryDeclinesNonNumbers checks the other half: entering part-way
// through inherits whatever the interpreter put in the locals, so the guards
// that the ordinary entry applies to parameters have to apply here to every
// local the body does arithmetic on.
func TestJITLoopEntryDeclinesNonNumbers(t *testing.T) {
	const src = "function f(n){ var s=0, i=0; while (i<n) { s=(s+i*3)|0; i=i+1; } return s; }"
	fn := jitFn(t, src)
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatal("refused to compile")
	}
	defer c.free()
	var header int
	for h := range c.osr {
		header = h
	}
	for _, bad := range []Value{mkundef(), mknull(), mkbool(true)} {
		locals := make([]Value, fn.maxLocals)
		locals[0] = tov(10)
		sSlot, iSlot := jitVarSlots(fn)
		locals[sSlot], locals[iSlot] = bad, tov(0)
		if _, _, ok := c.jitRunOSR(New(), fn, nil, 0, nil, locals, mkundef(), header); ok {
			t.Errorf("entered a running loop with a %v accumulator", bad.Type())
		}
	}
}

// jitVarSlots finds the two var slots of the loop functions above, by reading
// the stores the prologue makes rather than assuming they follow the
// parameters — a non-arrow body binds `this` to a local of its own first.
func jitVarSlots(fn *svFunc) (int, int) {
	var seen []int
	for ip := fn.startIP; ip < len(fn.code); {
		op := Opcode(fn.code[ip])
		if op == OpPutLocal {
			s := int(readU16(fn.code, ip+1))
			if s != fn.thisSlot {
				seen = append(seen, s)
			}
		}
		ip += int(opTable[op].Size)
	}
	if len(seen) < 2 {
		return fn.paramCount, fn.paramCount + 1
	}
	return seen[0], seen[1]
}
