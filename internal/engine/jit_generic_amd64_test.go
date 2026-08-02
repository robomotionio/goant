//go:build amd64

package engine

import (
	"math"
	"strconv"
	"testing"
)

// The generic operators, checked against the interpreter rather than against
// what they were meant to do.
//
// An operator whose operands have no known type is the one place this tier
// reproduces the language rather than emitting arithmetic, and the language is
// where the surprises are: `+` concatenates for a String and calls valueOf for a
// Date, `<` compares two Strings by code unit, `==` coerces where `===` does
// not, and every relational operator is false against a NaN. None of that is
// reimplemented here — the guard either takes the SSE path or hands both
// operands to the runtime. What these tests check is that the guard divides the
// two correctly, for every kind of value a field can hold.

// jitGenericFn compiles the body of `function f(o){ return o.a EXPR o.b; }`,
// whose operands are field reads and so have no type the tier can know.
//
// It returns the function both ways round: the compiled form, and the callable
// the engine will interpret, so that the two answers come from one Runtime and
// can be compared as Values rather than as text.
func jitGenericFn(t testing.TB, expr string) (*Runtime, *svFunc, *jitCode, Value) {
	t.Helper()
	src := "function f(o){ return o.a " + expr + " o.b; }; f;"
	rt := New()
	fnVal, err := rt.RunString("jit_generic_test.js", src)
	if err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	cl := rt.closureOf(fnVal)
	if cl == nil {
		t.Fatalf("%q did not produce a function", src)
	}
	c := jitCompile(cl.fn, nil)
	if c == nil {
		t.Fatalf("refused to compile %q", src)
	}
	t.Cleanup(c.free)
	return rt, cl.fn, c, fnVal
}

// jitInterpretOnly keeps the arm being compared against the interpreter even
// when the test binary is run with GOANT_JIT=1, which is how the differential
// over the conformance suite is run. Without it the two arms would be the same
// code and every test here would pass by saying nothing.
func jitInterpretOnly(t testing.TB) {
	t.Helper()
	was := jitEnabled
	jitEnabled = false
	t.Cleanup(func() { jitEnabled = was })
}

// jitPair builds the receiver `{a: x, b: y}`.
func jitPair(rt *Runtime, x, y Value) Value {
	o := rt.newObject(rt.objectProto)
	rt.objPtr(o).defineOwn("a", x, attrDefault)
	rt.objPtr(o).defineOwn("b", y, attrDefault)
	return o
}

// jitSame compares two results the way the tests below need to.
//
// Bit equality is right for everything except a String, where two runs produce
// two allocations of the same text, and it is right for the cases a JavaScript
// operator would get wrong: NaN is equal to itself here, and +0 is not equal
// to -0, because the question is whether compiled code produced the same value
// rather than whether the values compare equal.
func jitSame(rt *Runtime, x, y Value) bool {
	if x.IsString() && y.IsString() {
		return string(rt.strBytes(x)) == string(rt.strBytes(y))
	}
	return x == y
}

// jitShow renders a Value for a failure message without running any JavaScript.
func jitShow(rt *Runtime, v Value) string {
	switch {
	case v.IsNumber():
		return strconv.FormatFloat(v.Number(), 'g', -1, 64)
	case v.IsString():
		return strconv.Quote(string(rt.strBytes(v)))
	case v == mkundef():
		return "undefined"
	case v == mknull():
		return "null"
	case v == mkbool(true):
		return "true"
	case v == mkbool(false):
		return "false"
	default:
		return "an object at " + strconv.FormatUint(uint64(v), 16)
	}
}

// jitOperands is one of each kind of value a property can hold, chosen so that
// every arm of every operator below is reached: the numeric edges the fast path
// has to keep getting right, the primitives that look numeric but are not, and
// the objects that make an operator run JavaScript of its own.
func jitOperands(t testing.TB, rt *Runtime) []struct {
	name string
	v    Value
} {
	t.Helper()
	valueOf7, err := rt.RunString("op.js", "({ valueOf: function(){ return 7; } })")
	if err != nil {
		t.Fatalf("valueOf object: %v", err)
	}
	toStr, err := rt.RunString("op.js", "({ toString: function(){ return 'zz'; } })")
	if err != nil {
		t.Fatalf("toString object: %v", err)
	}
	arr, err := rt.RunString("op.js", "[1,2]")
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	return []struct {
		name string
		v    Value
	}{
		{"one", tov(1)},
		{"minus-two-point-five", tov(-2.5)},
		{"zero", tov(0)},
		{"minus-zero", tov(math.Copysign(0, -1))},
		{"nan", tov(math.NaN())},
		{"infinity", tov(math.Inf(1))},
		{"minus-infinity", tov(math.Inf(-1))},
		{"big", tov(1e300)},
		{"undefined", mkundef()},
		{"null", mknull()},
		{"true", mkbool(true)},
		{"false", mkbool(false)},
		{"numeric-string", rt.newString("2")},
		{"text-string", rt.newString("zz")},
		{"empty-string", rt.newString("")},
		{"object", rt.newObject(rt.objectProto)},
		{"array", arr},
		{"valueOf-object", valueOf7},
		{"toString-object", toStr},
	}
}

// TestJITGenericOperatorsAgreeWithTheInterpreter is the gate for this whole
// file: every operator, over every pair of operand kinds, has to produce what
// running the same function through the engine produces — or throw where it
// throws.
func TestJITGenericOperatorsAgreeWithTheInterpreter(t *testing.T) {
	jitInterpretOnly(t)
	for _, expr := range []string{"+", "-", "*", "/", "<", "<=", ">", ">=", "==", "!=", "===", "!=="} {
		t.Run(expr, func(t *testing.T) {
			rt, fn, c, fnVal := jitGenericFn(t, expr)
			ops := jitOperands(t, rt)
			for _, x := range ops {
				for _, y := range ops {
					o := jitPair(rt, x.v, y.v)

					locals := make([]Value, fn.maxLocals)
					locals[0] = o
					got, gotErr, ok := c.jitRun(rt, fn, nil, locals, mkundef())
					if !ok {
						t.Fatalf("%s %s %s: compiled code declined a receiver", x.name, expr, y.name)
					}

					want, wantErr := rt.callValue(fnVal, mkundef(), []Value{o})

					switch {
					case gotErr != nil && wantErr != nil:
						// Both threw, which is the agreement being checked.
					case gotErr != nil:
						t.Errorf("%s %s %s: compiled code threw, the interpreter returned %s",
							x.name, expr, y.name, jitShow(rt, want))
					case wantErr != nil:
						t.Errorf("%s %s %s: compiled code returned %s, the interpreter threw",
							x.name, expr, y.name, jitShow(rt, got))
					case !jitSame(rt, got, want):
						t.Errorf("%s %s %s: compiled %s, interpreted %s",
							x.name, expr, y.name, jitShow(rt, got), jitShow(rt, want))
					}
				}
			}
		})
	}
}

// TestJITGenericComparisonsBranchLikeTheInterpreter covers the shape the
// emitter treats quite differently: a comparison whose result a branch
// consumes never produces a Boolean at all, so the runtime's answer has to be
// turned back into a branch rather than into a value.
func TestJITGenericComparisonsBranchLikeTheInterpreter(t *testing.T) {
	jitInterpretOnly(t)
	for _, body := range []string{
		"if (o.a < o.b) return 1; return 2;",
		"if (o.a >= o.b) return 1; return 2;",
		"if (!(o.a === o.b)) return 1; return 2;",
		"if (o.a != o.b) return 1; return 2;",
		"var n = 0; for (var i = 0; i < 3; i = i + 1) { if (o.a < o.b) n = n + 1; } return n;",
	} {
		src := "function f(o){ " + body + " }; f;"
		rt := New()
		fnVal, err := rt.RunString("jit_generic_test.js", src)
		if err != nil {
			t.Fatalf("run %q: %v", src, err)
		}
		cl := rt.closureOf(fnVal)
		c := jitCompile(cl.fn, nil)
		if c == nil {
			t.Fatalf("refused to compile %q", src)
		}

		ops := jitOperands(t, rt)
		for _, x := range ops {
			for _, y := range ops {
				o := jitPair(rt, x.v, y.v)
				locals := make([]Value, cl.fn.maxLocals)
				locals[0] = o
				got, gotErr, ok := c.jitRun(rt, cl.fn, nil, locals, mkundef())
				if !ok {
					t.Fatalf("%s: compiled code declined a receiver", body)
				}
				want, wantErr := rt.callValue(fnVal, mkundef(), []Value{o})
				if (gotErr != nil) != (wantErr != nil) {
					t.Errorf("%s with %s/%s: threw in only one of the two", body, x.name, y.name)
					continue
				}
				if gotErr == nil && !jitSame(rt, got, want) {
					t.Errorf("%s with %s/%s: compiled %s, interpreted %s",
						body, x.name, y.name, jitShow(rt, got), jitShow(rt, want))
				}
			}
		}
		c.free()
	}
}

// jitOpCost is what one more of an operator costs, in bytes of machine code:
// two functions differing by a single occurrence, subtracted.
//
// Comparing whole functions would say nothing, because the generic one also
// reads a field. The difference between two of them isolates the operator.
func jitOpCost(t *testing.T, one, two string) int {
	t.Helper()
	a := jitCompile(jitFn(t, one), nil)
	if a == nil {
		t.Fatalf("refused %q", one)
	}
	defer a.free()
	b := jitCompile(jitFn(t, two), nil)
	if b == nil {
		t.Fatalf("refused %q", two)
	}
	defer b.free()
	return b.block.Len() - a.block.Len()
}

// TestKnownNumbersStillSkipTheGuard is the regression this change could most
// easily have caused.
//
// The guard is for operands whose type is not known. Emitting it for the ones
// that are would slow down exactly the code this tier exists for, and it would
// do it invisibly — the answers would all still be right. So this measures what
// one `+` costs in each case.
//
// The two differences are the same shape — one more operand read and one more
// operator — so the read cancels between them and what is left is the guard.
// The absolute bound is the sequence itself rather than a number that happened
// to come out: the operand read, two MOVQ in, ADDSD, one MOVQ out, and the NaN
// canonicalisation's compare, branch and 64-bit constant.
func TestKnownNumbersStillSkipTheGuard(t *testing.T) {
	checked := jitOpCost(t,
		"function f(a,b){ return a+b; }",
		"function f(a,b){ return a+b+b; }")
	generic := jitOpCost(t,
		"function f(o){ var x = o.a; return x+x; }",
		"function f(o){ var x = o.a; return x+x+x; }")

	t.Logf("one add costs %d bytes between known Numbers and %d bytes generically", checked, generic)
	if checked > 48 {
		t.Errorf("an add between two known Numbers costs %d bytes; it should be a read and the bare SSE sequence", checked)
	}
	if generic < checked+16 {
		t.Errorf("a generic add costs %d bytes and a checked one %d; the guard and the call out cannot fit in the difference", generic, checked)
	}
}

// TestJITStringConstantSurvivesCollection checks the one thing baking a
// constant into machine code depends on.
//
// A String constant reaches compiled code as a handle in an immediate, which is
// only sound because the collector marks fn.constants and the pool does not
// move. Running the function, collecting, and running it again is what that
// claim looks like from outside.
func TestJITStringConstantSurvivesCollection(t *testing.T) {
	rt, fn := jitFnRT(t, "function f(a,b){ return 'tail'; }")
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatal("refused a function returning a String constant")
	}
	defer c.free()

	locals := make([]Value, fn.maxLocals)
	first, _, ok := c.jitRun(rt, fn, nil, locals, mkundef())
	if !ok {
		t.Fatal("compiled code declined")
	}
	if s := string(rt.strBytes(first)); s != "tail" {
		t.Fatalf("got %q before collecting", s)
	}
	rt.Collect()
	second, _, ok := c.jitRun(rt, fn, nil, locals, mkundef())
	if !ok {
		t.Fatal("compiled code declined after a collection")
	}
	if s := string(rt.strBytes(second)); s != "tail" {
		t.Fatalf("got %q after collecting; the constant was not rooted", s)
	}
}

// TestJITGenericOperandsSurviveACollection is the collector's side of the
// generic path.
//
// Calling out spills the operand stack into the context and records its depth,
// which is what makes the operands reachable while the helper runs. That never
// mattered before, because a spilled operand was always a Number and a Number
// refers to nothing. Now one can be a String, and a valueOf on the other side
// can run a collection — so this runs one at exactly that moment and requires
// the answer to be unchanged.
func TestJITGenericOperandsSurviveACollection(t *testing.T) {
	rt, fn, c, _ := jitGenericFn(t, "+")

	collector := rt.newNativeFunc("valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		rt.collect()
		return tov(1), nil
	})
	trigger := rt.newObject(rt.objectProto)
	rt.objPtr(trigger).defineOwn("valueOf", collector, attrDefault)

	// A String left of the object, so the operand the collector could lose is
	// the one still sitting in the spill slots.
	o := jitPair(rt, rt.newString("head-"), trigger)
	locals := make([]Value, fn.maxLocals)
	locals[0] = o
	got, e, ok := c.jitRun(rt, fn, nil, locals, mkundef())
	if !ok || e != nil {
		t.Fatal("compiled code did not produce a value")
	}
	if s := string(rt.strBytes(got)); s != "head-1" {
		t.Fatalf("got %q, want %q; an operand did not survive the collection", s, "head-1")
	}
}

// BenchmarkJITGenericAdd measures what the guard costs when it is not needed
// and what the call out costs when it is.
func BenchmarkJITGenericAdd(b *testing.B) {
	rt, fn, c, _ := jitGenericFn(b, "+")
	for _, tc := range []struct {
		name string
		x, y Value
	}{
		{"two-numbers", tov(1.5), tov(2.5)},
		{"two-strings", rt.newString("ab"), rt.newString("cd")},
	} {
		o := jitPair(rt, tc.x, tc.y)
		locals := make([]Value, fn.maxLocals)
		locals[0] = o
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, _, ok := c.jitRun(rt, fn, nil, locals, mkundef()); !ok {
					b.Fatal("declined")
				}
			}
		})
	}
}

// TestJITGenericOperatorsInALongLoop runs the generic path past the points
// where compiled code has to leave and come back.
//
// A loop gives up the machine every jitFuel iterations and re-enters at the
// back edge, and the slow path leaves through the helper protocol on every
// iteration that takes it — allocating as it goes, which is what makes the
// collector run under a compiled frame rather than beside one. Fifty thousand
// iterations crosses the fuel boundary twice.
func TestJITGenericOperatorsInALongLoop(t *testing.T) {
	jitInterpretOnly(t)
	for _, tc := range []struct {
		name string
		n    int
		seed func(rt *Runtime) (Value, Value)
	}{
		{"numbers", 50000, func(rt *Runtime) (Value, Value) { return tov(0), tov(1) }},
		{"strings", 5000, func(rt *Runtime) (Value, Value) { return rt.newString(""), rt.newString("x") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "function f(o,n){ var s = o.a; var i = 0; while (i < n) { s = s + o.b; i = i + 1; } return s; }; f;"
			rt := New()
			fnVal, err := rt.RunString("jit_generic_test.js", src)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			cl := rt.closureOf(fnVal)
			c := jitCompile(cl.fn, nil)
			if c == nil {
				t.Fatal("refused to compile the loop")
			}
			defer c.free()

			a, b := tc.seed(rt)
			o := jitPair(rt, a, b)
			locals := make([]Value, cl.fn.maxLocals)
			locals[0], locals[1] = o, tov(float64(tc.n))
			got, e, ok := c.jitRun(rt, cl.fn, nil, locals, mkundef())
			if !ok || e != nil {
				t.Fatal("compiled code did not produce a value")
			}
			want, e := rt.callValue(fnVal, mkundef(), []Value{o, tov(float64(tc.n))})
			if e != nil {
				t.Fatal("the interpreter threw")
			}
			if !jitSame(rt, got, want) {
				t.Fatalf("compiled %s, interpreted %s", jitShow(rt, got), jitShow(rt, want))
			}
		})
	}
}

// TestJITGenericOperatorsActuallyRun is the test the property cache needed and
// did not have until it had already been written.
//
// Everything else here checks that the generic path gives the right answer.
// This checks that it is the path being taken at all: a guard that always sends
// its operands to the runtime and one that never does produce identical
// results, and the difference between them is the whole point. The counters are
// the only thing that can tell them apart, so both arms are driven and both
// counters are checked.
func TestJITGenericOperatorsActuallyRun(t *testing.T) {
	jitInterpretOnly(t)
	was := jitStats.enabled
	jitStats.enabled = true // before compiling: the increment is emitted, not conditional
	t.Cleanup(func() { jitStats.enabled = was })

	rt, fn, c, _ := jitGenericFn(t, "+")
	run := func(x, y Value, n int) (fast, slow uint64) {
		o := jitPair(rt, x, y)
		locals := make([]Value, fn.maxLocals)
		locals[0] = o
		before, beforeSlow := jitStats.genFast, jitStats.genSlow
		for i := 0; i < n; i++ {
			if _, _, ok := c.jitRun(rt, fn, nil, locals, mkundef()); !ok {
				t.Fatal("compiled code declined")
			}
		}
		return jitStats.genFast - before, jitStats.genSlow - beforeSlow
	}

	if fast, slow := run(tov(1), tov(2), 10); fast != 10 || slow != 0 {
		t.Errorf("two Numbers through a generic add: %d took the instruction and %d went to the runtime, want 10 and 0", fast, slow)
	}
	if fast, slow := run(rt.newString("a"), rt.newString("b"), 10); fast != 0 || slow != 10 {
		t.Errorf("two Strings through a generic add: %d took the instruction and %d went to the runtime, want 0 and 10", fast, slow)
	}
}
