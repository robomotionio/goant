package engine

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// runInterrupted compiles and runs src on a fresh Runtime, firing Interrupt
// after the script has had a moment to get going, and returns the result. The
// helper exists so each case states only what it is testing; a case that hangs
// fails the test binary by timeout, which is the honest failure mode here —
// there is nothing to assert if the interrupt never lands.
func runInterrupted(t *testing.T, src string) error {
	t.Helper()
	rt := New()
	s, err := rt.CompileScript("interrupt.js", src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		rt.Interrupt()
	}()
	_, err = rt.RunScript(s)
	return err
}

func TestInterruptStopsInfiniteLoop(t *testing.T) {
	err := runInterrupted(t, `for (;;) {}`)
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v, want ErrTerminated", err)
	}
}

func TestInterruptStopsWhileLoop(t *testing.T) {
	err := runInterrupted(t, `var i = 0; while (true) { i++; }`)
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v, want ErrTerminated", err)
	}
}

// A script must not be able to swallow its own cancellation. This is the whole
// reason termination is a control throw rather than a JS exception.
func TestInterruptIsNotCatchable(t *testing.T) {
	err := runInterrupted(t, `
		var caught = 0;
		for (;;) {
			try { for (;;) {} } catch (e) { caught++; }
		}
	`)
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v, want ErrTerminated", err)
	}
}

// finally runs on the way out of an ordinary throw; it must not get a chance to
// resume a terminated script either.
func TestInterruptIsNotResumableByFinally(t *testing.T) {
	err := runInterrupted(t, `
		for (;;) {
			try { for (;;) {} } finally { }
		}
	`)
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v, want ErrTerminated", err)
	}
}

// Unbounded recursion spins without ever taking a backward jump, so it is
// caught by the frame-entry check rather than the back-edge counter.
func TestInterruptStopsRecursion(t *testing.T) {
	err := runInterrupted(t, `
		function f() { try { return f(); } catch (e) { return f(); } }
		f();
	`)
	// Recursion may hit the stack-depth guard before the interrupt lands; either
	// way it must terminate rather than hang, and if the interrupt won the race
	// it must be reported as termination.
	if err == nil {
		t.Fatal("expected the script to stop, got a normal completion")
	}
	if !errors.Is(err, ErrTerminated) && !strings.Contains(err.Error(), "call stack") {
		t.Fatalf("got %v, want ErrTerminated or a stack-overflow error", err)
	}
}

// An interrupt that is never fired must not perturb an ordinary run, and the
// counter must not accumulate into a spurious termination across many loops.
func TestNoInterruptRunsNormally(t *testing.T) {
	rt := New()
	s, err := rt.CompileScript("normal.js", `
		var sum = 0;
		for (var i = 0; i < 200000; i++) { sum += i; }
		sum;
	`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	v, err := rt.RunScript(s)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := rt.ToNumber(v)
	if err != nil {
		t.Fatalf("ToNumber: %v", err)
	}
	if want := float64(199999) * 200000 / 2; got != want {
		t.Fatalf("sum = %v, want %v", got, want)
	}
	if rt.Interrupted() {
		t.Fatal("runtime reports an interrupt that was never requested")
	}
}

// ClearInterrupt must make the runtime usable again, otherwise a host that
// times one script out could never reuse the isolate — which is exactly what
// the pooled-isolate embedding does.
func TestClearInterruptAllowsReuse(t *testing.T) {
	rt := New()
	loop, err := rt.CompileScript("loop.js", `for (;;) {}`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		rt.Interrupt()
	}()
	if _, err := rt.RunScript(loop); !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v, want ErrTerminated", err)
	}

	rt.ClearInterrupt()
	after, err := rt.CompileScript("after.js", `var n = 0; for (var i = 0; i < 5000; i++) n++; n;`)
	if err != nil {
		t.Fatalf("compile after: %v", err)
	}
	v, err := rt.RunScript(after)
	if err != nil {
		t.Fatalf("run after clear: %v", err)
	}
	if n, _ := rt.ToNumber(v); n != 5000 {
		t.Fatalf("after clear got %v, want 5000", n)
	}
}

// While an interrupt is still pending, further runs must refuse to start rather
// than silently execute — a host that forgets to clear should see the failure.
func TestPendingInterruptBlocksNewRuns(t *testing.T) {
	rt := New()
	rt.Interrupt()
	s, err := rt.CompileScript("blocked.js", `1 + 1;`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := rt.RunScript(s); !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v, want ErrTerminated", err)
	}
}

// Every interruptible shape again, with the compiled tier forced on.
//
// The bug this pins ran for four months and the suite was green throughout,
// because jitEnabled is off unless GOANT_JIT is set and so nothing above ever
// reached compiled code. A tiered `for (;;) {}` ignored Interrupt entirely: the
// engine's interrupt checks are all at FUNCTION ENTRY, and a loop that calls
// nothing never reaches one. The interpreter is safe because it also checks on a
// back edge; compiled code took its back edge in machine code.
//
// The fix costs nothing because the safepoint already existed — the fuel counter
// at each back edge was already returning to Go periodically to let the
// collector run. It simply was not being asked whether anything wanted to stop.
//
// Threshold 1 rather than the default, so a loop is compiled on the spot instead
// of after the interrupt has already had its chance in the interpreter.
func TestInterruptStopsCompiledLoops(t *testing.T) {
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 1
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	for _, tc := range []struct{ name, src string }{
		{"an empty loop, which calls nothing at all", `for (;;) {}`},
		{"a loop with a body", `var i = 0; while (true) { i++; }`},
		{"a loop inside a function, called once", `
			function spin() { for (;;) {} }
			spin();`},
		{"a loop inside a function that ran hot first", `
			function spin(n) { for (var i = 0; i < n; i++) {} return n; }
			for (var k = 0; k < 100; k++) spin(10);
			spin(Infinity);`},
		{"a loop the script tries to protect with catch", `
			for (;;) { try { for (;;) {} } catch (e) {} }`},
		{"a loop the script tries to resume in finally", `
			for (;;) { try { for (;;) {} } finally {} }`},
		{"a nested loop", `for (;;) { for (;;) {} }`},
		{"a loop calling a compiled function", `
			function f(x) { return x + 1; }
			var i = 0; for (;;) { i = f(i); }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- runInterrupted(t, tc.src) }()
			select {
			case err := <-done:
				if !errors.Is(err, ErrTerminated) {
					t.Fatalf("got %v, want ErrTerminated", err)
				}
			case <-time.After(20 * time.Second):
				// Reported rather than left to the binary's own timeout, which
				// kills the whole package and says only that something hung.
				t.Fatalf("compiled code ignored the interrupt: still running after 20s")
			}
		})
	}
}

// The interrupt is checked at the back edge, so a script that never loops must
// still not pay for it — and one that finishes normally must not be stopped by a
// flag left over from something else.
func TestCompiledLoopsRunToCompletionWithoutAnInterrupt(t *testing.T) {
	wasEnabled, wasThreshold := jitEnabled, jitThreshold
	jitEnabled, jitThreshold = true, 1
	defer func() { jitEnabled, jitThreshold = wasEnabled, wasThreshold }()

	rt := New()
	v, err := rt.RunString("sum.js", `
		function sum(n) { var s = 0; for (var i = 0; i < n; i++) s += i; return s; }
		var t = 0;
		for (var k = 0; k < 50; k++) t += sum(20000);
		t;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := float64(50) * (19999.0 * 20000.0 / 2); v.Number() != want {
		t.Errorf("got %v, want %v", v.Number(), want)
	}
}
