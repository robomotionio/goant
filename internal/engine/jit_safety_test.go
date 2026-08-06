//go:build amd64 || arm64

package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/robomotionio/goant/internal/jitmem"
)

// The invariants that turn a compiler bug into a REFUSAL instead of a fault.
//
// A tier fails differently from an interpreter. An interpreter bug is a wrong
// answer: some test somewhere asks the question and the answer is visibly wrong.
// A tier bug is often not an answer at all — it is a jump to an address nothing
// wrote code at, a store past the end of a mapping, or an instruction fetched
// from a page that was still being written. None of those are caught by asking
// the engine questions, because the process is gone before it can answer.
//
// So this file tests the small number of places where the tier is supposed to
// give up. Every one of them is a point where a compiler bug would otherwise
// become a crash, and each is checked by driving it deliberately rather than by
// hoping a workload reaches it. They are cheap and they are the difference
// between "we ran the suites and nothing crashed" and "we know what happens when
// it goes wrong".
//
// What is NOT here, on purpose: a test that generated code is correct. That is
// the differential fuzzer's job (jit_fuzz_test.go) and every emit test's. These
// are about the failure mode, not the answer.

// TestAFunctionTooLargeToCompileIsRefused drives the emitter past what a block
// can hold and past what a branch can reach.
//
// arm64's conditional branch reaches a megabyte and its unconditional one a
// hundred and twenty-eight; amd64 reaches two gigabytes. A function large enough
// to overflow either is a function this tier should not have taken, and the
// answer is to decline it — jitasm records the overflow rather than raising,
// precisely so the caller can. Getting this wrong emits a branch to a computed
// address inside somebody else's code.
func TestAFunctionTooLargeToCompileIsRefused(t *testing.T) {
	was := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = was }()

	// Long enough that the emitted form is far past any block a function is
	// given, built from a shape the tier definitely compiles.
	var b strings.Builder
	b.WriteString("function big(a) { var s = 0;\n")
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, "s += a * %d + %d;\n", i%7+1, i%13)
	}
	b.WriteString("return s; }\nvar t = 0; for (var k = 0; k < 40; k++) t += big(k); t;")

	rt := New()
	got, err := rt.RunString("big.js", b.String())
	if err != nil {
		t.Fatalf("a function the tier cannot compile must still RUN: %v", err)
	}

	// And the answer has to be the interpreter's.
	jitEnabled = false
	want, err := New().RunString("big.js", b.String())
	if err != nil {
		t.Fatalf("interpreted: %v", err)
	}
	if uint64(got) != uint64(want) {
		t.Errorf("compiled %v, interpreted %v", got, want)
	}
}

// TestDeepOperandStacksAreRefusedNotOverrun covers the other size limit. The
// tier keeps operands in a fixed window of registers and refuses a function that
// needs more; a bug there writes through a register the caller was holding.
func TestDeepOperandStacksAreRefusedNotOverrun(t *testing.T) {
	for _, depth := range []int{4, 8, 16, 32, 64, 128} {
		t.Run(fmt.Sprintf("depth-%d", depth), func(t *testing.T) {
			expr := "1"
			for i := 0; i < depth; i++ {
				expr = fmt.Sprintf("(%s + %d)", expr, i)
			}
			src := fmt.Sprintf(`
				function deep(a) { return %s + a; }
				var t = 0; for (var k = 0; k < 300; k++) t += deep(k); t;`, expr)
			jitBothWays(t, fmt.Sprintf("deep-%d.js", depth), src)
		})
	}
}

// TestCodeIsNeverWritableAndExecutableAtOnce is the W^X invariant, checked at
// the boundary that enforces it rather than trusted.
//
// A mapping that is both writable and executable is the thing an attacker needs
// and the thing Apple Silicon refuses outright. The block is mapped RW, filled,
// then flipped to RX — and after the flip a further write has to be an error
// rather than a silent success, because a silent one would mean the flip did not
// take.
func TestCodeIsNeverWritableAndExecutableAtOnce(t *testing.T) {
	blk, err := jitmem.Alloc(64)
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	defer blk.Free()

	if blk.Executable() {
		t.Error("a freshly allocated block is already executable, so it was mapped W+X")
	}
	if _, err := blk.Write([]byte{0x90, 0x90, 0x90, 0x90}); err != nil {
		t.Fatalf("write before protect: %v", err)
	}
	if err := blk.Protect(); err != nil {
		t.Fatalf("protect: %v", err)
	}
	if !blk.Executable() {
		t.Error("Protect returned without making the block executable")
	}
	if _, err := blk.Write([]byte{0x90}); err != jitmem.ErrSealed {
		t.Errorf("writing to a sealed block returned %v, want ErrSealed; "+
			"a write that succeeded here would mean the mapping is still writable", err)
	}
}

// TestWritingPastTheEndOfABlockIsRefused is the bounds check on the one buffer
// the engine fills with raw bytes. Overrunning it corrupts whatever the
// allocator put after the mapping.
func TestWritingPastTheEndOfABlockIsRefused(t *testing.T) {
	blk, err := jitmem.Alloc(16)
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	defer blk.Free()

	// Alloc rounds up to a page, so fill to whatever it actually gave us.
	if _, err := blk.Write(make([]byte, blk.Cap())); err != nil {
		t.Fatalf("filling the block exactly: %v", err)
	}
	if _, err := blk.Write([]byte{0x90}); err != jitmem.ErrFull {
		t.Errorf("writing one byte past a full block returned %v, want ErrFull", err)
	}
	if blk.Len() != blk.Cap() {
		t.Errorf("the refused write moved the cursor: len %d, cap %d", blk.Len(), blk.Cap())
	}
}

// TestFreeingTwiceDoesNotUnmapSomethingElse. Free is called from a cleanup path
// and from the tier's own retirement; a second Free that unmapped again would
// eventually unmap an address the allocator had handed to someone else, and the
// crash would be somewhere with no connection to the JIT.
func TestFreeingTwiceDoesNotUnmapSomethingElse(t *testing.T) {
	blk, err := jitmem.Alloc(64)
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	b0, y0, _ := jitmem.Accounting()
	if err := blk.Free(); err != nil {
		t.Fatalf("first free: %v", err)
	}
	if err := blk.Free(); err != nil {
		t.Errorf("second free returned %v; it must be a no-op", err)
	}
	b1, y1, _ := jitmem.Accounting()
	if b1 != b0-1 || y1 != y0-int64(4096) && y1 >= y0 {
		t.Errorf("double free was counted twice: blocks %d->%d, bytes %d->%d", b0, b1, y0, y1)
	}
}

// TestTheTierNeverAnswersWhatItCannotCompile is the whole-engine version of the
// same idea: whatever the tier declines, the program still runs and still gets
// the interpreter's answer.
//
// The list is the shapes the emitter refuses, each of which is a place where
// "declined" and "emitted something wrong" are one edit apart.
func TestTheTierNeverAnswersWhatItCannotCompile(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"a generator", `
			function* g(n) { for (var i = 0; i < n; i++) yield i * 2; }
			var t = 0; for (var k = 0; k < 200; k++) for (var v of g(5)) t += v; t;`},
		{"an async function driven to completion", `
			var done = 0;
			async function a(x) { return x + 1; }
			for (var k = 0; k < 200; k++) a(k).then(function (v) { done += v; });
			done;`},
		{"a class constructor", `
			class C { constructor(a) { this.a = a; } get2() { return this.a * 2; } }
			var t = 0; for (var k = 0; k < 300; k++) t += new C(k).get2(); t;`},
		{"arguments, mapped", `
			function f(a, b) { arguments[0] = 9; return a + b + arguments.length; }
			var t = 0; for (var k = 0; k < 300; k++) t += f(k, 1); t;`},
		{"a closure over a loop variable", `
			function mk() { var fs = []; for (let i = 0; i < 3; i++) fs.push(function () { return i; }); return fs; }
			var t = 0; for (var k = 0; k < 300; k++) { var fs = mk(); t += fs[0]() + fs[1]() + fs[2](); } t;`},
		{"with", `
			function f(o) { with (o) { return a + b; } }
			var t = 0; for (var k = 0; k < 300; k++) t += f({a: k, b: 1}); t;`},
		{"a direct eval", `
			function f(x) { return eval("x + 1"); }
			var t = 0; for (var k = 0; k < 300; k++) t += f(k); t;`},
		{"try/finally with a return in both", `
			function f(x) { try { return x; } finally { if (x < 0) return -1; } }
			var t = 0; for (var k = 0; k < 300; k++) t += f(k); t;`},
		{"deep recursion that unwinds", `
			function r(n) { return n <= 0 ? 0 : n + r(n - 1); }
			var t = 0; for (var k = 0; k < 200; k++) t += r(60); t;`},
		{"a throw from deep inside a loop", `
			function f(n) { var s = 0; for (var i = 0; i < n; i++) { if (i === 17) throw new Error("x"); s += i; } return s; }
			var c = 0; for (var k = 0; k < 300; k++) { try { f(40); } catch (e) { c++; } } c;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestCompilingEveryFunctionInTheCorpusDoesNotFault is the blunt one, and it is
// the closest thing here to what production does.
//
// Threshold 1 compiles every function on its second entry, so this runs a wide
// spread of shapes through the emitter and executes what comes out. It is not
// checking answers — the suites do that — it is checking that nothing faults,
// which is the failure mode the suites cannot see.
func TestCompilingEveryFunctionInTheCorpusDoesNotFault(t *testing.T) {
	was, wasT := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 1
	defer func() { jitEnabled, jitThreshold = was, wasT }()

	for _, src := range []string{
		`var a = []; for (var i = 0; i < 500; i++) a.push({x: i, y: "" + i});
		 a.map(function (o) { return o.x * 2; }).filter(function (v) { return v % 3; }).reduce(function (p, c) { return p + c; }, 0);`,
		`var s = ""; for (var i = 0; i < 300; i++) s += i.toString(16) + ","; s.split(",").length;`,
		`var m = new Map(), st = new Set();
		 for (var i = 0; i < 300; i++) { m.set("k" + i, i); st.add(i % 50); }
		 m.size + st.size;`,
		`JSON.parse(JSON.stringify({a: [1, 2, {b: "c"}], d: {e: [true, null, 1.5]}})).a.length;`,
		`var re = /(\w+)@(\w+)\.com/g, t = 0, s = "a@b.com c@d.com";
		 for (var i = 0; i < 300; i++) { re.lastIndex = 0; var m; while ((m = re.exec(s))) t += m[1].length; } t;`,
		`function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); } fib(20);`,
		`var ta = new Float64Array(256); for (var k = 0; k < 200; k++) for (var i = 0; i < 256; i++) ta[i] = ta[(i + 1) & 255] + i; ta[0];`,
		`var o = {}; for (var i = 0; i < 200; i++) o["p" + i] = i;
		 var t = 0; for (var k in o) t += o[k]; t;`,
		`var d = [1, [2, [3, [4, [5]]]]];
		 function flat(x) { return Array.isArray(x) ? x.map(flat).join(",") : "" + x; }
		 var t = ""; for (var k = 0; k < 300; k++) t = flat(d); t.length;`,
		`class A { constructor() { this.v = 1; } m() { return this.v; } }
		 class B extends A { m() { return super.m() + 1; } }
		 var t = 0; for (var k = 0; k < 300; k++) t += new B().m(); t;`,
	} {
		rt := New()
		if _, err := rt.RunString("corpus.js", src); err != nil {
			t.Errorf("faulted or threw with every function compiled: %v\n%s", err, src)
		}
	}
}

// A throw from a FUSED comparison inside a try must reach that try's catch.
//
// This is the bug the differential fuzzer found in five seconds, and it is worth
// its own test rather than only a corpus entry, because the shape is invisible:
// the program is correct, the tier compiles it, and the only symptom is that a
// catch does not catch.
//
// Which throw reaches which catch is decided at COMPILE time here — each call out
// of compiled code carries a fixup naming the catch that was in force when it was
// emitted. The stamping ran at the bottom of the emitter's instruction loop, and
// three arms leave that loop with `continue`. One of them is a relational fused
// with the branch that consumes it, which is what `(a >= b) ? x : y` and every
// `if (a < b)` compile to — so a comparison that threw inside a try had no catch
// recorded and walked straight out of the function.
//
// The cases below are the operators that fuse, against operands that make them
// throw: a Symbol has no ordering and no arithmetic, and an object whose valueOf
// throws turns any of them into a call that does.
func TestAThrowFromAFusedComparisonIsCaught(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"a ternary on a relational", `
			function f(v) { try { return (v >= -Infinity) ? 1 : 2; } catch (e) { return "caught"; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(Symbol.iterator); out;`},
		{"an if on a relational", `
			function f(v) { try { if (v < 1) { return 1; } return 2; } catch (e) { return "caught"; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(Symbol.iterator); out;`},
		{"a while on a relational", `
			function f(v) { try { while (v > 0) { return 1; } return 2; } catch (e) { return "caught"; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(Symbol.iterator); out;`},
		{"a for-condition on a relational", `
			function f(v) { try { for (var i = 0; i <= v; i++) {} return 1; } catch (e) { return "caught"; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(Symbol.iterator); out;`},
		{"a throwing valueOf in a fused comparison", `
			var bad = {valueOf: function () { throw new Error("no"); }};
			function f(v) { try { return (v < 1) ? 1 : 2; } catch (e) { return "caught"; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(bad); out;`},
		{"a fused equality", `
			var bad = {valueOf: function () { throw new Error("no"); }};
			function f(v) { try { return (v == 1) ? 1 : 2; } catch (e) { return "caught"; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(bad); out;`},
		{"arithmetic that throws inside a try", `
			var bad = {valueOf: function () { throw new Error("no"); }};
			function f(v) { try { return v * 2 + 1; } catch (e) { return "caught"; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(bad); out;`},
		{"a property read on null inside a try", `
			function f(v) { try { return v.x; } catch (e) { return "caught"; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(null); out;`},
		{"a nested try, innermost wins", `
			function f(v) { try { try { return (v >= 0) ? 1 : 2; } catch (e) { return "inner"; } }
			                catch (e) { return "outer"; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(Symbol.iterator); out;`},
		{"a fused comparison AFTER the try has been left", `
			function f(v) { try { } catch (e) { return "caught"; } return (v >= 0) ? 1 : 2; }
			var c = 0; for (var k = 0; k < 200; k++) { try { f(Symbol.iterator); } catch (e) { c++; } } c;`},
		{"finally still runs", `
			var ran = 0;
			function f(v) { try { return (v >= 0) ? 1 : 2; } catch (e) { return "caught"; } finally { ran++; } }
			var out = ""; for (var k = 0; k < 200; k++) out = "" + f(Symbol.iterator); out + "|" + (ran > 0);`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}
