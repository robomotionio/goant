//go:build amd64 || arm64

package engine

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// settleCodeMemory runs collections until executable memory stops falling, and
// reports where it landed.
//
// Two things make a single runtime.GC() insufficient. A finalizer is not run by
// the cycle that finds the object unreachable — it is queued then and run on
// another goroutine — and the svFunc that owns the code is reached through the
// Runtime, so the function and the code it owns can take a further cycle to
// become garbage themselves. Looping until the number is stable is what makes
// this a test of reclamation rather than of finalizer scheduling.
func settleCodeMemory(t *testing.T) int64 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	_, prev, _ := JITCodeMemory()
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		runtime.GC()
		_, now, _ := JITCodeMemory()
		if now == prev {
			return now
		}
		prev = now
	}
	_, now, _ := JITCodeMemory()
	return now
}

// What the tier costs in executable memory, and what it does with it over time.
//
// This is the risk a benchmark cannot show and a correctness suite has no reason
// to look for, and it is worth saying what this file used to claim, because a
// twelve-hour fuzzing campaign proved it wrong. It said compiled code is NEVER
// RELEASED — that a block has to outlive every entry into it and nothing can
// prove an entry has ended — and it tested only that the amount was KNOWN and
// proportional to the distinct code a host ran.
//
// Both halves of that were true and the conclusion still cost a machine. A fuzz
// worker replaying 3,543 programs held a flat 5-8 MB JavaScript heap and 180 MB
// of executable memory in 27,461 mappings, and was killed by the OOM killer at
// 1.79 GB while every Runtime inside it was under a 64 MB heap limit. A host
// that keeps loading new flows is the same shape.
//
// The missing proof turned out not to be needed per entry: entering a function's
// code requires reaching the function, so a function no one can reach is a
// function no one can enter. Code is therefore reclaimed with its svFunc — see
// jit_reclaim.go — and these tests pin BOTH halves, because either one failing
// alone is a serious bug:
//
//   - a function that is still reachable keeps its code, forever, even across
//     collections; freeing it early is a use-after-free of executable memory
//   - a function that is gone gives its code back
//
// The numbers here are deliberately generous. They are a smoke alarm for a
// change that makes the tier allocate per call or per entry instead of per
// function — the shape of regression that would take days to notice in
// production and seconds to notice here.

// jitCodeMem runs fn with the tier on and reports what it added to the process's
// code memory.
//
// Two things are needed to make a DELTA meaningful now that code comes back.
// fn returns the Runtime it built and the Runtime is held until after the second
// reading, because a Runtime that goes out of scope inside fn can have its code
// unmapped before the measurement is taken. And the counter is settled first,
// because it is process-wide: an earlier test's garbage being returned in the
// middle of this one subtracts from the delta. Between them these two reported
// zero bytes for two hundred calls, and every assertion written on that number
// passed while measuring nothing.
func jitCodeMem(t *testing.T, threshold int32, fn func() *Runtime) (blocks, bytes int64) {
	t.Helper()
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, threshold
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	settleCodeMemory(t)
	b0, y0, _ := JITCodeMemory()
	rt := fn()
	b1, y1, _ := JITCodeMemory()
	runtime.KeepAlive(rt)
	return b1 - b0, y1 - y0
}

// TestCodeMemoryIsAccountedAtAll is the precondition for every other test here:
// a host cannot alarm on a number that does not exist.
func TestCodeMemoryIsAccountedAtAll(t *testing.T) {
	blocks, bytes := jitCodeMem(t, 2, func() *Runtime {
		rt := New()
		if _, err := rt.RunString("m.js", `
			function hot(n) { var s = 0; for (var i = 0; i < n; i++) s += i * 3; return s; }
			var t = 0; for (var k = 0; k < 200; k++) t += hot(50); t;`); err != nil {
			t.Fatalf("run: %v", err)
		}
		return rt
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

	_, few := jitCodeMem(t, 2, func() *Runtime {
		rt := New()
		if _, err := rt.RunString("few.js", fmt.Sprintf(src, 200)); err != nil {
			t.Fatalf("run: %v", err)
		}
		return rt
	})
	_, many := jitCodeMem(t, 2, func() *Runtime {
		rt := New()
		if _, err := rt.RunString("many.js", fmt.Sprintf(src, 20000)); err != nil {
			t.Fatalf("run: %v", err)
		}
		return rt
	})
	t.Logf("200 calls: %d bytes; 20000 calls: %d bytes", few, many)
	if few <= 0 {
		t.Fatalf("200 calls compiled nothing (%d bytes); the comparison below "+
			"would be vacuous", few)
	}
	// A hundredfold the calls for at most a handful of extra blocks. Anything
	// approaching proportional is the failure this is watching for.
	if many > few*8 {
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
//
// The test asks whether the number GREW, not whether it is unchanged. It used to
// ask for equality, which stopped being a fair question once code is reclaimed:
// the counter is process-wide, so garbage another test left behind can be
// returned at any moment and the reading goes DOWN. That is the feature working,
// and it failed this test by 4096 bytes.
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
	if later > settled {
		t.Errorf("after settling, 300 further runs of the same flow allocated %d more bytes. "+
			"That is a per-run cost, which a pooled host pays forever.", later-settled)
	}
	runtime.KeepAlive(rt)
}

// TestCodeMemoryGrowsWithDISTINCTFunctions is the other half, and it is written
// to DOCUMENT the growth rather than to forbid it.
//
// A host that keeps loading new flows into one Runtime keeps compiling new
// functions, and every one of them keeps its block for as long as that Runtime
// holds them. That is the accumulation a host has to plan for, so the test
// measures the per-function cost and states it, and fails only if it becomes
// wild. What it costs after the Runtime is dropped is
// TestCodeMemoryIsReclaimedWhenTheFunctionIsGone.
func TestCodeMemoryGrowsWithDISTINCTFunctions(t *testing.T) {
	const n = 200
	blocks, bytes := jitCodeMem(t, 2, func() *Runtime {
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
		return rt
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

// TestCodeMemoryIsReclaimedWhenTheFunctionIsGone is the test the OOM killer
// wrote.
//
// A host that keeps loading new flows compiles new functions forever. Each one
// is a fresh mapping, and until jit_reclaim.go none of them came back: the
// measured shape was 52 KB of executable memory per program, monotonic, with the
// JavaScript heap flat throughout — which is why SetHeapLimit did not save the
// machine that died of it.
//
// The property is that discarding the Runtime discards the code. Not
// immediately, and not to the byte: finalizers run on their own schedule and the
// package's own fixtures have compiled things too. What must be true is that the
// steady state after dropping N programs is far nearer to where it started than
// to where it peaked, because the difference between those two is the difference
// between a host that stays up and one that does not.
func TestCodeMemoryIsReclaimedWhenTheFunctionIsGone(t *testing.T) {
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 2
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	const programs = 120
	start := settleCodeMemory(t)

	// Held for the duration, so the growth measured below is the whole of what
	// 120 programs cost. Letting them go one at a time would measure the fix
	// instead: reclamation happens inside the loop and the high-water mark never
	// rises, which is the right behaviour and the wrong experiment.
	live := make([]*Runtime, 0, programs)
	for i := 0; i < programs; i++ {
		// Distinct source per iteration, so nothing can be served by a cache and
		// every program is genuinely new code.
		src := fmt.Sprintf(`
			function a%d(n){var s=0;for(var i=0;i<n;i++)s+=i*%d+1;return s;}
			function b%d(n){var s=0;for(var i=0;i<n;i++)s+=a%d(i%%7)+%d;return s;}
			b%d(220);`, i, i+1, i, i, i+2, i)
		rt := New()
		if _, err := rt.RunString(fmt.Sprintf("p%d.js", i), src); err != nil {
			t.Fatalf("program %d: %v", i, err)
		}
		live = append(live, rt)
	}
	_, peak, _ := JITCodeMemory()
	runtime.KeepAlive(live)
	live = nil
	settled := settleCodeMemory(t)

	grew := peak - start
	kept := settled - start
	t.Logf("%d programs held: %d bytes; after dropping them: %d "+
		"(grew %d, kept %d)", programs, peak, settled, grew, kept)

	if grew < int64(programs)*1024 {
		t.Fatalf("%d distinct programs grew executable memory by only %d bytes; "+
			"the test is measuring an idle tier and would pass whatever the tier "+
			"did with memory", programs, grew)
	}
	// Generous on purpose. Anything under a quarter retained is reclamation
	// working; the failure being watched for is the old behaviour, where this
	// number was the whole of grew.
	if kept > grew/4 {
		t.Errorf("after dropping %d Runtimes, %d of %d bytes of executable memory "+
			"were still mapped (%d%%). Code is not being reclaimed with its function, "+
			"which is what takes a long-running host down.",
			programs, kept, grew, 100*kept/grew)
	}
}

// TestCodeMemoryIsKeptWhileTheFunctionIsReachable is the other half, and it is
// the one that matters for safety rather than for footprint.
//
// Reclaiming a block that something can still enter is not a leak in reverse —
// it is a jump into an unmapped page, which is a segfault with no Go frame to
// blame it on. So the guarantee is stated as its own test: while the Runtime is
// alive, collections do not take its code away, however many of them run.
func TestCodeMemoryIsKeptWhileTheFunctionIsReachable(t *testing.T) {
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 2
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	var live []*Runtime
	for i := 0; i < 20; i++ {
		rt := New()
		src := fmt.Sprintf(`
			function keep%d(n){var s=0;for(var i=0;i<n;i++)s+=i*%d+3;return s;}
			globalThis.keep = keep%d; keep%d(220);`, i, i+1, i, i)
		if _, err := rt.RunString(fmt.Sprintf("k%d.js", i), src); err != nil {
			t.Fatalf("program %d: %v", i, err)
		}
		live = append(live, rt)
	}
	_, before, _ := JITCodeMemory()
	after := settleCodeMemory(t)
	if after < before {
		t.Errorf("collections released %d bytes of code belonging to %d Runtimes "+
			"that are still alive. A call into any of them is a jump into an "+
			"unmapped page.", before-after, len(live))
	}
	// And they really are still callable — the strongest statement of the same
	// property, since a freed block would fault here rather than answer.
	for i, rt := range live {
		if _, err := rt.RunString("again.js", `keep(220);`); err != nil {
			t.Fatalf("runtime %d could not run its own compiled function after "+
				"%d collections: %v", i, 2, err)
		}
	}
	runtime.KeepAlive(live)
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
