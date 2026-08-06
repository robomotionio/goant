//go:build amd64 || arm64

package engine

import (
	"fmt"
	"testing"
)

// What the tier costs in executable memory, and what it does with it over time.
//
// This is the risk a benchmark cannot show and a correctness suite has no reason
// to look for. Compiled code is NEVER RELEASED: a block has to outlive every
// entry into it, and nothing in the engine can prove an entry has ended — a
// suspended generator, a frame a call site opened, an outer frame of a recursive
// function — so `jitCode.free()` is called from nothing outside tests. A process
// that runs one script exits before that matters. A host that runs thousands of
// different flows over days accumulates a block for every function that ever got
// hot, plus one more for every recompilation.
//
// "Never freed" is a justified design. "Unbounded and unmeasured" is a different
// thing, and these tests are the difference: they do not assert that memory is
// released, because it is not and should not be. They assert that the amount is
// KNOWN, that it is proportional to the distinct code a host actually ran, and
// that re-running the same code again forever adds nothing.
//
// The numbers here are deliberately generous. They are a smoke alarm for a
// change that makes the tier allocate per call or per entry instead of per
// function — the shape of regression that would take days to notice in
// production and seconds to notice here.

// jitCodeMem runs fn with the tier on and reports what it added to the process's
// code memory.
func jitCodeMem(t *testing.T, threshold int32, fn func()) (blocks, bytes int64) {
	t.Helper()
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, threshold
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	b0, y0, _ := JITCodeMemory()
	fn()
	b1, y1, _ := JITCodeMemory()
	return b1 - b0, y1 - y0
}

// TestCodeMemoryIsAccountedAtAll is the precondition for every other test here:
// a host cannot alarm on a number that does not exist.
func TestCodeMemoryIsAccountedAtAll(t *testing.T) {
	blocks, bytes := jitCodeMem(t, 2, func() {
		rt := New()
		if _, err := rt.RunString("m.js", `
			function hot(n) { var s = 0; for (var i = 0; i < n; i++) s += i * 3; return s; }
			var t = 0; for (var k = 0; k < 200; k++) t += hot(50); t;`); err != nil {
			t.Fatalf("run: %v", err)
		}
	})
	if blocks <= 0 || bytes <= 0 {
		t.Fatalf("compiling a hot function accounted %d blocks and %d bytes; "+
			"the tier allocated executable memory and did not say so", blocks, bytes)
	}
	t.Logf("one hot function: %d blocks, %d bytes", blocks, bytes)
}

// TestCodeMemoryIsPerFunctionNotPerEntry is the regression this file exists for.
//
// A tier that allocated per entry, per call site or per frame would pass every
// correctness suite in the repository and take a production host down in a day.
// So the property is not "the same bytes" — it is that a hundredfold more work
// costs a bounded amount more memory rather than a hundredfold more.
//
// Measured, it is 4096 bytes for 200 calls and 12288 for 20000: three blocks
// against one. The extra two are one-offs, not a rate. A compiled function is
// rebuilt once when its prologue stops checking its parameters (see
// jitNoteDecline), and the script's own top level compiles on a loop back edge
// once it has gone round enough times — both of which need volume to happen at
// all, and neither of which happens twice.
func TestCodeMemoryIsPerFunctionNotPerEntry(t *testing.T) {
	const src = `
		function hot(n) { var s = 0; for (var i = 0; i < n; i++) s += i * 3; return s; }
		var t = 0; for (var k = 0; k < %d; k++) t += hot(50); t;`

	_, few := jitCodeMem(t, 2, func() {
		rt := New()
		if _, err := rt.RunString("few.js", fmt.Sprintf(src, 200)); err != nil {
			t.Fatalf("run: %v", err)
		}
	})
	_, many := jitCodeMem(t, 2, func() {
		rt := New()
		if _, err := rt.RunString("many.js", fmt.Sprintf(src, 20000)); err != nil {
			t.Fatalf("run: %v", err)
		}
	})
	t.Logf("200 calls: %d bytes; 20000 calls: %d bytes", few, many)
	// A hundredfold the calls for at most a handful of extra blocks. Anything
	// approaching proportional is the failure this is watching for.
	if few > 0 && many > few*8 {
		t.Errorf("100x the calls cost %dx the code memory (%d bytes against %d). "+
			"Compilation is supposed to happen per function, not per entry.",
			many/few, many, few)
	}
}

// TestCodeMemoryConvergesOnAPooledRuntime is the host's actual shape: one
// Runtime, the same flow, for the life of the process.
//
// The property is CONVERGENCE, not zero growth. The first few re-runs may still
// compile something — a script's top level reaching its loop threshold, a
// function being rebuilt without its parameter check — and those are one-offs.
// What must not happen is a per-run cost, because a host runs this shape
// millions of times and a per-run cost is a leak with a schedule.
func TestCodeMemoryConvergesOnAPooledRuntime(t *testing.T) {
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 2
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	rt := New()
	if _, err := rt.RunString("first.js", `
		function step(a, b) { var s = 0; for (var i = 0; i < 20; i++) s += a * i + b; return s; }
		function flow(n) { var t = 0; for (var i = 0; i < n; i++) t += step(i, 2); return t; }
		globalThis.flow = flow;
		flow(300);`); err != nil {
		t.Fatalf("first: %v", err)
	}
	rerun := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := rt.RunString("again.js", `flow(300);`); err != nil {
				t.Fatalf("rerun: %v", err)
			}
		}
	}
	rerun(30)
	_, settled, _ := JITCodeMemory()
	rerun(300) // ten times as many again
	_, later, _ := JITCodeMemory()
	if later != settled {
		t.Errorf("after settling, 300 further runs of the same flow allocated %d more bytes. "+
			"That is a per-run cost, which a pooled host pays forever.", later-settled)
	}
}

// TestCodeMemoryGrowsWithDISTINCTFunctions is the other half, and it is written
// to DOCUMENT the growth rather than to forbid it.
//
// A host that keeps loading new flows into one Runtime keeps compiling new
// functions, and every one of them keeps its block for the life of the process.
// That is the accumulation a host has to plan for, so the test measures the
// per-function cost and states it, and fails only if it becomes wild.
func TestCodeMemoryGrowsWithDISTINCTFunctions(t *testing.T) {
	const n = 200
	blocks, bytes := jitCodeMem(t, 2, func() {
		rt := New()
		var src string
		for i := 0; i < n; i++ {
			src += fmt.Sprintf(
				"function g%d(a){var s=0;for(var i=0;i<8;i++)s+=a*i+%d;return s;}\n", i, i)
		}
		src += "var t=0;for(var k=0;k<30;k++){"
		for i := 0; i < n; i++ {
			src += fmt.Sprintf("t+=g%d(k);", i)
		}
		src += "}t;"
		if _, err := rt.RunString("many.js", src); err != nil {
			t.Fatalf("run: %v", err)
		}
	})
	if blocks < n/2 {
		t.Errorf("%d distinct hot functions produced only %d blocks; "+
			"either they did not compile or the accounting missed them", n, blocks)
	}
	perFn := bytes / int64(max(blocks, 1))
	t.Logf("%d distinct hot functions: %d blocks, %d bytes, %d bytes per function",
		n, blocks, bytes, perFn)
	// A page each is the floor, since a mapping is page-granular. Well past that
	// means a function is being given far more than it writes.
	if perFn > 128*1024 {
		t.Errorf("%d bytes of executable mapping per compiled function is more than "+
			"a long-running host can absorb", perFn)
	}
}

// TestRecompilationRetainsTheOldBlockAndSaysSo pins the second accumulation,
// which is easy to forget because it is invisible from the outside: a function
// that is recompiled keeps its previous block in fn.jit.retired, because a call
// site in another function may still hold a pointer into it and a suspended
// frame may still be inside it.
//
// The test asserts the retention is ACCOUNTED, not that it does not happen.
func TestRecompilationRetainsTheOldBlockAndSaysSo(t *testing.T) {
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 2
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	rt := New()
	// A site that goes polymorphic is the ordinary way a function is rebuilt.
	if _, err := rt.RunString("warm.js", `
		function f(o) { return o.a; }
		globalThis.f = f;
		var a = {a:1}, b = {b:1, a:2}, c = {c:1, b:1, a:3};
		for (var k = 0; k < 400; k++) f(a);
		1;`); err != nil {
		t.Fatalf("warm: %v", err)
	}
	_, before, _ := JITCodeMemory()
	if _, err := rt.RunString("shift.js", `
		var xs = [{a:1},{b:1,a:2},{c:1,a:3},{d:1,a:4},{e:1,a:5}];
		for (var k = 0; k < 2000; k++) f(xs[k % xs.length]);
		1;`); err != nil {
		t.Fatalf("shift: %v", err)
	}
	_, after, _ := JITCodeMemory()
	// Either nothing was rebuilt (fine) or something was and it is counted.
	if after < before {
		t.Errorf("code memory went DOWN by %d bytes without anything being freed; "+
			"the accounting is wrong", before-after)
	}
	t.Logf("after a receiver-shape shift: %d bytes, was %d", after, before)
}

// TestCodeMemoryAccountingSurvivesFree is the arithmetic check. Nothing in the
// engine frees a block, but the tests do, and a counter that only ever goes up
// would report a leak that is not there.
func TestCodeMemoryAccountingSurvivesFree(t *testing.T) {
	b0, y0, _ := JITCodeMemory()
	fn := jitFn(t, "function f(a){ return a + 1; }")
	c := jitCompile(fn, nil)
	if c == nil {
		t.Skip("refused to compile")
	}
	b1, y1, _ := JITCodeMemory()
	if b1 <= b0 || y1 <= y0 {
		t.Fatalf("compiling accounted nothing: blocks %d->%d, bytes %d->%d", b0, b1, y0, y1)
	}
	c.free()
	b2, y2, _ := JITCodeMemory()
	if b2 != b0 || y2 != y0 {
		t.Errorf("freeing did not return the accounting to where it started: "+
			"blocks %d want %d, bytes %d want %d", b2, b0, y2, y0)
	}
}
