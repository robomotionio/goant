//go:build amd64 || arm64

package engine

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Compile time against body size.
//
// Both of the tier's dataflow analyses used to iterate to a fixpoint over a Go
// map of blocks, which hands its keys back in an order unrelated to the flow, so
// propagating a fact along a chain of n blocks took n passes over all n of them.
// That is quadratic, and the comment above one of them said the graph was small.
//
// V8's mjsunit has a function that switches on eighty thousand cases. The
// interpreter runs it in 267ms; compiling it took over two hundred seconds, so
// the tier turned a slow function into a hang. Nothing in Octane is shaped like
// that and no conformance suite times anything, which is why this is the corpus
// that found it.
//
// The bound below is loose on purpose. The quadratic form took 9 seconds for
// 16,000 cases and the linear one takes about 65ms; anything between those is
// still a bug, and a machine having a bad minute is not.
func TestCompileTimeIsLinearInBodySize(t *testing.T) {
	jitInterpretOnly(t)

	compile := func(cases int) time.Duration {
		t.Helper()
		var b strings.Builder
		b.WriteString("function f(x){ switch(x){")
		for i := 0; i < cases; i++ {
			b.WriteString("case ")
			b.WriteString(strconv.Itoa(i))
			b.WriteString(": ")
		}
		b.WriteString("default: 0; } }; f;")

		rt := New()
		fnVal, err := rt.RunString("jit_bigbody_test.js", b.String())
		if err != nil {
			t.Fatalf("%d cases: %v", cases, err)
		}
		cl := rt.closureOf(fnVal)
		if cl == nil {
			t.Fatalf("%d cases: not a function", cases)
		}
		start := time.Now()
		if c := jitCompile(cl.fn, nil); c != nil {
			t.Cleanup(c.free)
		}
		return time.Since(start)
	}

	// 16,000 cases is the largest size the quadratic form could reach at all
	// within a test's patience, and it needed nine seconds to do it.
	const cases = 16000
	if took := compile(cases); took > 5*time.Second {
		t.Fatalf("compiling %d cases took %v; the analyses are super-linear again", cases, took)
	}
}

// Blocks and locals multiply, and the sets this analysis keeps are dense in
// both. Ordering the passes correctly bounds how many times it walks them; it
// does not bound the walk. So there is a budget, and what it has to do is
// decline before allocating rather than after.
//
// Eight thousand try/catch blocks is the shape that reaches it, because each
// catch binding is a local: the two dimensions grow together and the product is
// quadratic in the source. mjsunit has a file like this — two thousand evals,
// each wrapped in its own try — and it took over two hundred seconds to compile
// what the interpreter ran in a few milliseconds.
func TestABodyTooLargeToAnalyseIsDeclined(t *testing.T) {
	jitInterpretOnly(t)

	const blocks = 8000
	var b strings.Builder
	b.WriteString("function f(x){ var r = 0;")
	for i := 0; i < blocks; i++ {
		b.WriteString("try { r = x; } catch (e")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(") { r = 1; }")
	}
	b.WriteString("return r; }; f;")

	rt := New()
	fnVal, err := rt.RunString("jit_bigbody_test.js", b.String())
	if err != nil {
		t.Fatal(err)
	}
	cl := rt.closureOf(fnVal)
	if cl == nil {
		t.Fatal("not a function")
	}

	var why string
	start := time.Now()
	if c := jitCompile(cl.fn, &why); c != nil {
		t.Cleanup(c.free)
	}
	took := time.Since(start)
	if why != "body-too-large" {
		t.Fatalf("%d try blocks: refused with %q, want body-too-large", blocks, why)
	}
	// Declining has to be cheap, or the budget has only moved the cost.
	if took > time.Second {
		t.Fatalf("took %v to decline a body it never analysed", took)
	}
}
