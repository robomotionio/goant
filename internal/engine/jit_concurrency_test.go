//go:build amd64 || arm64

package engine

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// The tier under concurrency, which is the shape a host actually runs.
//
// Everything else in this package tests one Runtime on one goroutine. A host
// embedding goant runs many flows at once, each with its own Runtime, and the
// tier has state that is NOT per-Runtime:
//
//   - icEpochCounter is one counter for the whole process, read by generated
//     code as a bare load and bumped by every collection in every Runtime;
//   - jitmem's block and byte accounting is process-wide;
//   - and since jit_reclaim.go, a finalizer running on the collector's
//     goroutine unmaps executable memory while other goroutines are executing
//     other compiled code.
//
// That last one is the reason this file exists. Reclamation turned "compiled
// code is immortal" into "compiled code is freed by a goroutine you do not
// control, at a time you do not choose", and the argument that it is safe is a
// reachability argument. A reachability argument is exactly the kind that holds
// on one goroutine and fails on eight, so it is worth a test rather than a
// paragraph.
//
// Run under -race, this also covers the cross-Runtime state above: a
// non-atomic epoch bump or a torn accounting update would be reported here and
// nowhere else in the package.

// jitConcurrentSrc is one flow: hot enough to compile, recursive so its call
// sites close the cycle that reclamation has to survive, and self-checking so a
// miscompilation under load is a failed assertion rather than a wrong number
// nobody looks at.
func jitConcurrentSrc(id int) (string, string) {
	src := fmt.Sprintf(`
		function down%d(n) { return n <= 0 ? 0 : down%d(n - 1) + 1; }
		function even%d(n) { return n === 0 ? 1 : odd%d(n - 1); }
		function odd%d(n)  { return n === 0 ? 0 : even%d(n - 1); }
		function work%d(n) {
			var s = 0, a = new Float64Array(8), b = [1, 2, 3, 4];
			for (var i = 0; i < n; i++) {
				a[i %% 8] = i * 1.5;
				b[i %% 4] = i;
				s += a[i %% 8] + b[i %% 4] + down%d(4) + even%d(4);
			}
			return s;
		}
		var t = 0;
		for (var k = 0; k < 200; k++) t += work%d(16);
		"" + t;`, id, id, id, id, id, id, id, id, id, id)
	return src, ""
}

// TestTheTierIsSafeAcrossGoroutines runs many Runtimes at once, each compiling
// and discarding its own code, with collections forced underneath them.
//
// The Runtimes are dropped as the test goes, so finalizers unmap blocks WHILE
// other goroutines are entering blocks. Every goroutine checks its own answer
// against the same answer computed by the interpreter, so a fault, a wrong
// result and a hang are all distinguishable from a pass.
func TestTheTierIsSafeAcrossGoroutines(t *testing.T) {
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 2
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	// The expected answers, computed with the tier off, one at a time. Doing this
	// first means the concurrent phase compares against the interpreter rather
	// than against itself.
	const flows = 8
	want := make([]string, flows)
	func() {
		jitEnabled = false
		defer func() { jitEnabled = true }()
		for i := 0; i < flows; i++ {
			src, _ := jitConcurrentSrc(i)
			rt := New()
			v, err := rt.RunString(fmt.Sprintf("want%d.js", i), src)
			if err != nil {
				t.Fatalf("flow %d interpreted: %v", i, err)
			}
			want[i] = string(rt.strBytes(v))
			runtime.KeepAlive(rt)
		}
	}()

	const rounds = 12
	var wg sync.WaitGroup
	errs := make(chan string, flows*rounds)
	for i := 0; i < flows; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			src, _ := jitConcurrentSrc(id)
			for r := 0; r < rounds; r++ {
				// A fresh Runtime every round, dropped at the end of it: this is
				// what puts compilation and reclamation on the same wall clock.
				rt := New()
				v, err := rt.RunString(fmt.Sprintf("f%d-%d.js", id, r), src)
				if err != nil {
					errs <- fmt.Sprintf("flow %d round %d: %v", id, r, err)
					return
				}
				if got := string(rt.strBytes(v)); got != want[id] {
					errs <- fmt.Sprintf("flow %d round %d: got %q, interpreted %q",
						id, r, got, want[id])
					return
				}
			}
		}(i)
	}

	// A collector running against all of it. Reclamation happens on this
	// goroutine, so forcing it is what makes the test test anything.
	done := make(chan struct{})
	var gcWG sync.WaitGroup
	gcWG.Add(1)
	go func() {
		defer gcWG.Done()
		for {
			select {
			case <-done:
				return
			default:
				runtime.GC()
			}
		}
	}()

	wg.Wait()
	close(done)
	gcWG.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// And the accounting survived being maintained from many goroutines at once.
	blocks, bytes, peak := JITCodeMemory()
	t.Logf("%d flows x %d rounds: %d blocks / %d bytes still mapped, peak %d",
		flows, rounds, blocks, bytes, peak)
	if blocks < 0 || bytes < 0 {
		t.Errorf("code accounting went negative under concurrency: %d blocks, %d bytes",
			blocks, bytes)
	}
}

// TestTheTierSurvivesRuntimesDyingUnderLoad is the same hazard stated as
// bluntly as it can be: one goroutine is entering compiled code continuously
// while another creates and abandons Runtimes as fast as it can, so the
// collector has a steady supply of blocks to unmap.
//
// If reachability is ever wrong — if a block can be freed while a call site, a
// suspended frame or a jitCallee can still reach it — this is where it faults.
func TestTheTierSurvivesRuntimesDyingUnderLoad(t *testing.T) {
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 2
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	// The long-lived side: one Runtime, compiled, entered over and over.
	steady := New()
	if _, err := steady.RunString("steady.js", `
		function inner(n) { return n <= 0 ? 0 : inner(n - 1) + 1; }
		function outer(n) { var s = 0; for (var i = 0; i < n; i++) s += inner(6); return s; }
		globalThis.outer = outer;
		outer(200);`); err != nil {
		t.Fatalf("steady: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // churn: compile and abandon
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			rt := New()
			src, _ := jitConcurrentSrc(i % 8)
			if _, err := rt.RunString("churn.js", src); err != nil {
				t.Errorf("churn %d: %v", i, err)
				return
			}
		}
	}()

	wg.Add(1)
	go func() { // gc: unmap what churn abandoned
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()

	// The steady side keeps entering its own compiled code throughout.
	for i := 0; i < 400; i++ {
		v, err := steady.RunString("again.js", `"" + outer(200);`)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("steady round %d: %v", i, err)
		}
		if got := string(steady.strBytes(v)); got != "1200" {
			close(stop)
			wg.Wait()
			t.Fatalf("steady round %d: outer(200) = %q, want 1200", i, got)
		}
	}
	close(stop)
	wg.Wait()
	runtime.KeepAlive(steady)
}
