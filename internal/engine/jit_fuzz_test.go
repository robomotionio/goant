//go:build amd64 || arm64

package engine

import (
	"fmt"
	"strings"
	"testing"
)

// Differential fuzzing: the same program, interpreted and compiled, must give
// the same answer.
//
// This exists because the test suites cannot cover what a JIT gets wrong. A
// miscompilation is not a wrong answer to a question anyone thought to ask — it
// is a wrong answer to a combination nobody wrote down, and often it is not a
// wrong answer at all but a fault at an address that has nothing to do with the
// program. test262 and mjsunit check that goant implements JavaScript; they
// were written against an interpreter and they do not know the tier exists.
// What they cannot do is generate the operand orderings, receiver mixtures and
// guard-failure sequences that a compiled guard chain is sensitive to.
//
// The oracle is the interpreter. That is the whole design: goant already has a
// correct implementation of every operation here, so the compiled one is not
// checked against a specification, it is checked against the engine's own
// answer. Any disagreement is a tier bug by construction — and a crash under
// either arm is a finding regardless of what the two would have returned.
//
// go test -fuzz=FuzzJIT ./internal/engine/ -run=xxx
//
// The generated programs are deliberately narrow in shape and wide in
// combination. Every one of them:
//
//   - terminates, because every loop bound is a literal;
//   - is deterministic, because nothing reads the clock, the environment or a
//     random source, and property enumeration order is the only ordering it can
//     observe;
//   - allocates a bounded amount, because sizes are literals too.
//
// Without those three a disagreement means nothing: a timeout, a clock read or
// an OOM would differ between the arms for reasons that are not miscompilation.

// fuzzValues are the receivers and operands that a compiled guard chain has to
// tell apart. They are chosen from what has actually broken this tier: holes and
// frozen arrays (the element store's rejection paths), every TypedArray kind
// (one emitted load each), the doubles that convert badly (NaN, the infinities,
// negative zero, magnitudes past int64), and the prototype chain, which is what
// turns a missing own property into a call.
var fuzzValues = []string{
	`0`, `1`, `-1`, `2`, `0.5`, `-0`, `NaN`, `Infinity`, `-Infinity`,
	`1e300`, `9.5e18`, `2147483647`, `2147483648`, `-2147483648`, `4294967295`,
	`"s"`, `""`, `"1"`, `true`, `false`, `null`, `undefined`,
	`[1,2,3]`, `[]`, `[1,,3]`, `Object.freeze([1,2,3])`, `Object.seal([1,2,3])`,
	`new Int8Array([1,2,3])`, `new Uint8Array([1,2,3])`, `new Int16Array([1,2,3])`,
	`new Uint16Array([1,2,3])`, `new Int32Array([1,2,3])`, `new Uint32Array([1,2,3])`,
	`new Float32Array([1.5,2,3])`, `new Float64Array([1.5,2,3])`,
	`new Uint8ClampedArray([1,2,3])`, `new Float16Array([1,2,3])`,
	`new BigInt64Array([1n,2n])`,
	`({a:1,b:2})`, `({})`, `Object.create({a:9})`, `Object.freeze({a:1})`,
	`(function(){return 1})`, `Symbol.iterator`, `1n`,
}

// fuzzBinOps are every operator whose compiled form has a guard in front of it,
// which is all of them: the tier emits a machine instruction when both operands
// are Numbers and leaves for the runtime otherwise.
var fuzzBinOps = []string{
	"+", "-", "*", "/", "%", "**",
	"<", ">", "<=", ">=", "==", "!=", "===", "!==",
	"&", "|", "^", "<<", ">>", ">>>",
	"&&", "||", "??",
}

var fuzzUnOps = []string{"-", "+", "!", "~", "typeof ", "void "}

// fuzzGen turns fuzzer bytes into a program. Deterministic in the bytes, so a
// failure reproduces from the corpus entry alone.
type fuzzGen struct {
	b   []byte
	i   int
	buf strings.Builder
}

func (g *fuzzGen) next(n int) int {
	if n <= 0 {
		return 0
	}
	if g.i >= len(g.b) {
		g.i = 0
		if len(g.b) == 0 {
			return 0
		}
	}
	v := int(g.b[g.i])
	g.i++
	return v % n
}

func (g *fuzzGen) pick(xs []string) string { return xs[g.next(len(xs))] }

// expr builds an expression over the variables the prologue defined. depth
// bounds it so a pathological seed cannot produce a program that takes longer to
// parse than to run.
func (g *fuzzGen) expr(depth int) string {
	if depth <= 0 {
		switch g.next(3) {
		case 0:
			return g.pick(fuzzValues)
		case 1:
			return fmt.Sprintf("v%d", g.next(4))
		default:
			return fmt.Sprintf("%d", g.next(8))
		}
	}
	switch g.next(9) {
	case 0:
		return "(" + g.expr(depth-1) + " " + g.pick(fuzzBinOps) + " " + g.expr(depth-1) + ")"
	case 1:
		return "(" + g.pick(fuzzUnOps) + "(" + g.expr(depth-1) + "))"
	case 2:
		return "(" + g.expr(depth-1) + "[" + g.expr(depth-1) + "])"
	case 3:
		return "(" + g.expr(depth-1) + "." + g.pick([]string{"a", "b", "length", "x"}) + ")"
	case 4:
		return fmt.Sprintf("f%d(%s, %s)", g.next(3), g.expr(depth-1), g.expr(depth-1))
	case 5:
		return "(" + g.expr(depth-1) + " ? " + g.expr(depth-1) + " : " + g.expr(depth-1) + ")"
	case 6:
		return "(typeof " + g.expr(depth-1) + ")"
	case 7:
		return "(" + g.expr(depth-1) + ")"
	default:
		return g.pick(fuzzValues)
	}
}

func (g *fuzzGen) stmt(depth int) string {
	// The bound expr has and stmt did not. Without it the `if` arm recurses
	// through negative depths until the Go stack is gone — which the fuzzer found
	// in 1.4 seconds, in the harness rather than in the engine. Worth keeping as
	// a comment: a generator that can diverge reports its own crashes as the
	// subject's.
	if depth <= 0 {
		return fmt.Sprintf("v%d = %s;", g.next(4), g.expr(0))
	}
	switch g.next(8) {
	case 0:
		return fmt.Sprintf("v%d = %s;", g.next(4), g.expr(depth))
	case 1:
		return fmt.Sprintf("try { v%d = %s; } catch (e) { out.push('E'); }", g.next(4), g.expr(depth))
	case 2:
		// A bounded loop: this is what makes a function tier up at all.
		return fmt.Sprintf("for (var i%d = 0; i%d < %d; i%d++) { v%d = %s; }",
			depth, depth, 1+g.next(6), depth, g.next(4), g.expr(depth-1))
	case 3:
		return fmt.Sprintf("if (%s) { %s } else { %s }",
			g.expr(depth-1), g.stmt(depth-1), g.stmt(depth-1))
	case 4:
		return fmt.Sprintf("v%d[%s] = %s;", g.next(4), g.expr(depth-1), g.expr(depth-1))
	case 5:
		return fmt.Sprintf("v%d.%s = %s;", g.next(4),
			g.pick([]string{"a", "b", "x"}), g.expr(depth-1))
	case 6:
		return fmt.Sprintf("out.push(%s);", g.expr(depth-1))
	default:
		return fmt.Sprintf("out.push(String(%s));", g.expr(depth))
	}
}

// program wraps the generated statements in something hot enough to compile and
// reports a string that depends on everything it did.
func (g *fuzzGen) program() string {
	var body strings.Builder
	for n := 1 + g.next(6); n > 0; n-- {
		body.WriteString("\t\t")
		body.WriteString(g.stmt(3))
		body.WriteByte('\n')
	}
	return fmt.Sprintf(`
var out = [];
function f0(a, b) { return a; }
function f1(a, b) { try { return a[b]; } catch (e) { return "E"; } }
function f2(a, b) { return (a === b) ? 1 : 0; }
function body(v0, v1, v2, v3) {
%s	return out.length;
}
for (var round = 0; round < 40; round++) {
	try {
		body(%s, %s, %s, %s);
	} catch (e) {
		out.push("T:" + (e && e.constructor ? e.constructor.name : "?"));
	}
}
out.length + "|" + out.slice(0, 60).join(",");
`, body.String(), g.pick(fuzzValues), g.pick(fuzzValues), g.pick(fuzzValues), g.pick(fuzzValues))
}

// fuzzRun evaluates src on a fresh Runtime with the tier in a given state, and
// converts everything — value, thrown error, or panic — into a comparable
// string. A panic is caught rather than allowed to fail the process, so that the
// arm that panicked can be reported ALONGSIDE what the other arm answered.
func fuzzRun(src string, tier bool, threshold int32) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("PANIC:%v", r)
		}
	}()
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = tier, int32(threshold)
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	rt := New()
	v, err := rt.RunString("fuzz.js", src)
	if err != nil {
		return "ERR:" + err.Error()
	}
	if v.Type() == TStr {
		return "OK:" + string(rt.strBytes(v))
	}
	return fmt.Sprintf("OK:%v", v)
}

func FuzzJITAgreesWithTheInterpreter(f *testing.F) {
	// Seeds chosen to reach the guard chains rather than to be interesting
	// programs. The fuzzer's job is to combine them.
	for _, s := range [][]byte{
		{0}, {1, 2, 3}, {7, 11, 13, 17}, {255, 128, 64, 32, 16},
		{4, 4, 4, 4, 4, 4}, {2, 9, 2, 9, 2, 9}, {31, 41, 59, 26, 53, 58},
		{100, 200, 50, 25, 12, 6, 3, 1}, {9, 8, 7, 6, 5, 4, 3, 2, 1},
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) == 0 || len(b) > 4096 {
			t.Skip()
		}
		g := &fuzzGen{b: b}
		src := g.program()

		interp := fuzzRun(src, false, 8)
		// Threshold 2 rather than 1: it still compiles everything, and it leaves
		// the interpreter one pass to fill the type feedback the element chains
		// are emitted from. At 1 no site has a record and half the tier is never
		// reached — see elemfeedback.go.
		compiled := fuzzRun(src, true, 2)
		if interp != compiled {
			t.Fatalf("tier disagrees with the interpreter\n--- program ---\n%s\n--- interpreted ---\n%s\n--- compiled ---\n%s",
				src, interp, compiled)
		}
	})
}

// TestJITFuzzSeedsAgree runs the seed corpus as an ordinary test, so the
// differential runs in CI without anyone having to remember -fuzz.
func TestJITFuzzSeedsAgree(t *testing.T) {
	for i, b := range [][]byte{
		{0}, {1, 2, 3}, {7, 11, 13, 17}, {255, 128, 64, 32, 16},
		{4, 4, 4, 4, 4, 4}, {2, 9, 2, 9, 2, 9}, {31, 41, 59, 26, 53, 58},
		{100, 200, 50, 25, 12, 6, 3, 1}, {9, 8, 7, 6, 5, 4, 3, 2, 1},
		{13, 27, 3, 99, 41, 6, 200, 77}, {5, 5, 200, 1, 8, 250, 3},
	} {
		g := &fuzzGen{b: b}
		src := g.program()
		interp := fuzzRun(src, false, 8)
		compiled := fuzzRun(src, true, 2)
		if interp != compiled {
			t.Errorf("seed %d disagrees\n--- program ---\n%s\n--- interpreted ---\n%s\n--- compiled ---\n%s",
				i, src, interp, compiled)
		}
	}
}

// The fuzzer is only worth running if its two arms are actually different.
//
// jitEnabled is off unless GOANT_JIT is set, and fuzzRun turns it on by hand. If
// that ever stopped working — a rename, a second gate, a threshold that refuses
// everything — the differential would compare the interpreter with itself, agree
// on every input forever, and report a clean campaign. That is a worse outcome
// than a red one: it is hours of machines saying nothing while looking like
// evidence.
//
// This repo has made exactly that mistake before, in the other direction:
// GOANT_JIT=0 used to mean ON, because any non-empty value was, and weeks of
// "Octane is unchanged with the tier on" were measured against the tier.
//
// So the arms are checked by counting compiled frames rather than trusted.
func TestFuzzArmsAreActuallyDifferent(t *testing.T) {
	was := jitStats.enabled
	jitStats.enabled = true
	defer func() { jitStats.enabled = was }()

	// A seed whose program has a loop in it, so there is something to compile.
	g := &fuzzGen{b: []byte{7, 11, 13, 17, 3, 9}}
	src := g.program()

	c0 := jitStats.compiled
	fuzzRun(src, false, 8)
	c1 := jitStats.compiled
	fuzzRun(src, true, 2)
	c2 := jitStats.compiled

	if c1-c0 != 0 {
		t.Errorf("the interpreted arm compiled %d frames, so the arms are not distinct", c1-c0)
	}
	if c2-c1 == 0 {
		t.Fatal("the compiled arm compiled NOTHING: every campaign this fuzzer has ever " +
			"run was comparing the interpreter with itself")
	}
	t.Logf("interpreted arm: %d frames compiled; tier arm: %d", c1-c0, c2-c1)
}
