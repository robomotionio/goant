//go:build amd64

package engine

import (
	"math"
	"strconv"
	"testing"
)

// jitFn compiles src, which must declare exactly one function, and returns that
// function's bytecode.
func jitFn(t testing.TB, src string) *svFunc {
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
	return top.childFuncs[0]
}

// TestJITAgreesWithTheInterpreter is the gate. Compiled code has to produce not
// merely the right number but the same Value the interpreter would have pushed,
// bit for bit — which is a stricter claim, and the one that catches a NaN whose
// pattern strays into the tag space.
func TestJITAgreesWithTheInterpreter(t *testing.T) {
	cases := []struct {
		src  string
		want func(a, b float64) float64
	}{
		{"function f(a,b){ return a+b; }", func(a, b float64) float64 { return a + b }},
		{"function f(a,b){ return a-b; }", func(a, b float64) float64 { return a - b }},
		{"function f(a,b){ return a*b; }", func(a, b float64) float64 { return a * b }},
		{"function f(a,b){ return a/b; }", func(a, b float64) float64 { return a / b }},
		{"function f(a,b){ return (a+b)*(a-b); }", func(a, b float64) float64 { return (a + b) * (a - b) }},
		{"function f(a,b){ return a*b+a; }", func(a, b float64) float64 { return a*b + a }},
		{"function f(a,b){ return a+1; }", func(a, b float64) float64 { return a + 1 }},
		{"function f(a,b){ return a/b/b; }", func(a, b float64) float64 { return a / b / b }},
	}

	inputs := []struct{ a, b float64 }{
		{1, 2}, {2.5, 0.5}, {-3, 7}, {0, 0}, {1, 0}, {-1, 0},
		{math.Inf(1), math.Inf(1)}, {math.Inf(1), math.Inf(-1)},
		{math.NaN(), 1}, {1, math.NaN()},
		{math.MaxFloat64, math.MaxFloat64}, {math.SmallestNonzeroFloat64, 2},
		{-0, 5}, {5, -0},
	}

	for _, tc := range cases {
		fn := jitFn(t, tc.src)
		c := jitCompile(fn)
		if c == nil {
			t.Errorf("%s: refused to compile", tc.src)
			continue
		}
		for _, in := range inputs {
			locals := make([]Value, fn.maxLocals)
			locals[0], locals[1] = tov(in.a), tov(in.b)
			got, ok := c.jitRun(locals)
			if !ok {
				t.Errorf("%s(%v,%v): bailed on two Numbers", tc.src, in.a, in.b)
				continue
			}
			want := tov(tc.want(in.a, in.b))
			if uint64(got) != uint64(want) {
				t.Errorf("%s(%v,%v) = %#016x (%v), interpreter would give %#016x (%v)",
					tc.src, in.a, in.b, uint64(got), got.Number(), uint64(want), want.Number())
			}
		}
		c.free()
	}
}

// TestJITCanonicalizesNaN pins the specific hazard. x86 hands back
// 0xFFF8000000000000 for 0/0, which is above the NaN-box tag threshold: stored
// raw it would read as a tagged value and the rest of the engine would treat a
// number as an object.
func TestJITCanonicalizesNaN(t *testing.T) {
	fn := jitFn(t, "function f(a,b){ return a/b; }")
	c := jitCompile(fn)
	if c == nil {
		t.Fatal("refused to compile")
	}
	defer c.free()

	locals := make([]Value, fn.maxLocals)
	locals[0], locals[1] = tov(0), tov(0)
	got, ok := c.jitRun(locals)
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
	rt := New()
	want, err := rt.RunString("nan.js", "function f(a,b){ return a/b; } f(0,0);")
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
	fn := jitFn(t, "function f(a,b){ return a+b; }")
	c := jitCompile(fn)
	if c == nil {
		t.Fatal("refused to compile")
	}
	defer c.free()

	for _, bad := range []Value{mkundef(), mknull(), mkbool(true), tEmpty} {
		locals := make([]Value, fn.maxLocals)
		locals[0], locals[1] = bad, tov(1)
		if _, ok := c.jitRun(locals); ok {
			t.Errorf("did not bail on a %v operand", bad.Type())
		}
		locals[0], locals[1] = tov(1), bad
		if _, ok := c.jitRun(locals); ok {
			t.Errorf("did not bail on a %v operand in the second position", bad.Type())
		}
	}
}

// TestJITRefusesWhatItCannotModel checks the other half of the contract:
// refusing is normal, and anything outside straight-line Number arithmetic has
// to be refused rather than mis-compiled.
func TestJITRefusesWhatItCannotModel(t *testing.T) {
	for _, src := range []string{
		"function f(a,b){ return g(a); }",               // a call
		"function f(a,b){ if (a) return 1; return 2; }", // a branch
		"function f(a,b){ a = 1; return a; }",           // a store
		"function f(a,b){ return a.x; }",                // a property
		"function f(a,b){ return 'x'; }",                // a String constant
		"function f(a,b){ }",                            // falls off the end
		"function f(a,b){ return a%b; }",                // modulo is not in this tier
		"function f(a,b){ return -a; }",                 // negation is not in this tier
	} {
		if c := jitCompile(jitFn(t, src)); c != nil {
			c.free()
			t.Errorf("compiled %q, which it should have refused", src)
		}
	}
}

// TestJITMatchesRunningTheSource is the end-to-end check: the same expression
// through the whole engine, and through compiled code, agreeing.
func TestJITMatchesRunningTheSource(t *testing.T) {
	const src = "function f(a,b){ return (a+b)*(a-b)/b; }"
	fn := jitFn(t, src)
	c := jitCompile(fn)
	if c == nil {
		t.Fatal("refused to compile")
	}
	defer c.free()

	rt := New()
	if _, err := rt.RunString("jit_test.js", src); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, in := range []struct{ a, b float64 }{{7, 3}, {1.5, 0.25}, {-4, 9}} {
		v, err := rt.RunString("call.js", src+"; f("+ftoa(in.a)+","+ftoa(in.b)+");")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		locals := make([]Value, fn.maxLocals)
		locals[0], locals[1] = tov(in.a), tov(in.b)
		got, ok := c.jitRun(locals)
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
	fn := jitFn(b, src)
	c := jitCompile(fn)
	if c == nil {
		b.Fatal("refused to compile")
	}
	defer c.free()

	locals := make([]Value, fn.maxLocals)
	locals[0], locals[1] = tov(7), tov(3)

	b.Run("compiled", func(b *testing.B) {
		for b.Loop() {
			if _, ok := c.jitRun(locals); !ok {
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

func benchInterpretedCall(b *testing.B, src string) {
	b.Helper()
	rt := New()
	a, bb := tov(7), tov(3)
	fnVal, err := rt.RunString("bench.js", src+"; f;")
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, e := rt.callValue(fnVal, mkundef(), []Value{a, bb}); e != nil {
			b.Fatal("throw")
		}
	}
}
