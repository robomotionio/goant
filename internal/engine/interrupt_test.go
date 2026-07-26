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
