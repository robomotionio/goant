package engine

import "testing"

// A queued job is taken off its queue before it runs — it has to be, or a
// re-entrant drain would run it twice — and for the length of that run the only
// reference to what it captured was a Go local. The collector walks the
// Runtime, so it could not see it, and a collection landing in that window
// swept the promise the reaction was about to settle.
//
// It needed roughly twenty thousand queued reactions to show, which is what it
// takes to reach the collector's floor while the queue is still draining. Below
// that the queue empties before anything is ever collected, so every smaller
// version of this test passes whether the roots are held or not.

// withGCFloor lowers the collector's threshold for one test so a collection
// lands mid-drain without needing tens of thousands of live promises.
func withGCFloor(t *testing.T, n int) {
	t.Helper()
	was := gcFloor
	gcFloor = n
	t.Cleanup(func() { gcFloor = was })
}

func TestARunningJobKeepsWhatItCaptured(t *testing.T) {
	withGCFloor(t, 256)

	const src = `
		var done = 0;
		async function step(n) { return n + 1; }
		async function chain(n) {
			var t = 0;
			for (var i = 0; i < n; i++) { t = await step(t); }
			return t;
		}
		for (var k = 0; k < 60; k++) { chain(60).then(function (v) { done += v; }); }
		done;
	`
	rt := New()
	if _, err := rt.RunString("roots.js", src); err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.DrainJobs()

	v, err := rt.RunString("check.js", "done")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// 60 chains of 60 awaits each, every one counting up from zero.
	if got := v.Number(); got != 60*60 {
		t.Fatalf("done = %v, want %v — reactions were lost", got, 60*60)
	}
}

// The same window on the timer path: a one-shot timer is off the list before
// its callback runs, so its function and arguments are unreferenced too.
func TestARunningTimerKeepsItsCallback(t *testing.T) {
	withGCFloor(t, 256)

	const src = `
		var seen = 0;
		for (var i = 0; i < 200; i++) {
			setTimeout(function (a, b) { seen += a + b; }, i, 1, 2);
		}
		seen;
	`
	rt := New()
	if _, err := rt.RunString("timers.js", src); err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.DrainJobs()

	v, err := rt.RunString("check.js", "seen")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := v.Number(); got != 200*3 {
		t.Fatalf("seen = %v, want %v — timer callbacks or their arguments were lost", got, 200*3)
	}
}

// holdInFlight has to nest: a job can drain the queue again beneath itself, and
// the outer job's roots must survive the inner drain's cleanup.
func TestInFlightRootsNest(t *testing.T) {
	rt := New()
	a, b := mkint(1), mkint(2)

	outer := rt.holdInFlight([]Value{a})
	if len(rt.inFlight) != 1 {
		t.Fatalf("outer hold: %d roots, want 1", len(rt.inFlight))
	}
	inner := rt.holdInFlight([]Value{b}, b)
	if len(rt.inFlight) != 3 {
		t.Fatalf("inner hold: %d roots, want 3", len(rt.inFlight))
	}
	inner()
	if len(rt.inFlight) != 1 || rt.inFlight[0] != a {
		t.Fatalf("inner release took the outer roots with it: %v", rt.inFlight)
	}
	outer()
	if len(rt.inFlight) != 0 {
		t.Fatalf("outer release left %d roots", len(rt.inFlight))
	}
	// Cleared rather than only truncated, so a stale Value in the backing array
	// cannot keep a dead object marked.
	if rt.inFlight[:1][0] != 0 {
		t.Fatal("a released root was left in the backing array")
	}
}
