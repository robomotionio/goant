package engine

import (
	"testing"

	"github.com/robomotionio/goant/internal/jitmem"
)

// A compiled loop has one safepoint: the fuel counter running out. So when the
// byte budget says a collection is due, the runtime has to reach into the
// frames that are running and spend their fuel, or they carry on allocating for
// up to twenty thousand more iterations.
func TestByteBudgetSpendsTheFuelOfLiveCompiledFrames(t *testing.T) {
	rt := New()
	rt.SetHeapLimit(1 << 20)

	frames := []*jitmem.ExecContext{{}, {}, {}}
	for _, f := range frames {
		f.Args[1] = jitFuel
	}
	rt.jitFrames = frames
	rt.jitDepth = 2 // the third is in the chain but not live

	// Not yet due: the budget has room, so nothing is disturbed.
	rt.nextBytes = 1 << 30
	rt.chargeBytes(1024)
	for i, f := range frames {
		if f.Args[1] != jitFuel {
			t.Fatalf("frame %d: fuel %d before the budget was spent, want %d", i, f.Args[1], jitFuel)
		}
	}

	// Now due.
	rt.nextBytes = 2048
	rt.chargeBytes(1 << 20)

	if rt.gc.next != rt.objects.liveN {
		t.Errorf("gc.next = %d, want the live count %d", rt.gc.next, rt.objects.liveN)
	}
	for i := 0; i < rt.jitDepth; i++ {
		if frames[i].Args[1] != 1 {
			t.Errorf("live frame %d: fuel %d, want 1", i, frames[i].Args[1])
		}
	}
	if frames[2].Args[1] != jitFuel {
		t.Errorf("frame past jitDepth: fuel %d, want it untouched at %d", frames[2].Args[1], jitFuel)
	}
}

// One, not zero. The back edge decrements the counter and branches on the
// result, so a zero wraps to 2^64-1 and the loop never reaches a safepoint
// again — the opposite of what this is for, and invisible until something runs
// for hours.
func TestSpentFuelIsOneSoTheNextBackEdgeExits(t *testing.T) {
	rt := New()
	rt.SetHeapLimit(1 << 20)

	ctx := &jitmem.ExecContext{}
	ctx.Args[1] = jitFuel
	rt.jitFrames = []*jitmem.ExecContext{ctx}
	rt.jitDepth = 1

	rt.nextBytes = 1
	rt.chargeBytes(1 << 20)

	if ctx.Args[1] != 1 {
		t.Fatalf("fuel %d, want 1", ctx.Args[1])
	}
	// What the emitted back edge does with it: subtract one, branch if not zero.
	if ctx.Args[1]-1 != 0 {
		t.Fatalf("a back edge would not exit: %d-1 = %d", ctx.Args[1], ctx.Args[1]-1)
	}

	// Already spent, and left alone rather than reset.
	rt.chargeBytes(1 << 20)
	if ctx.Args[1] != 1 {
		t.Fatalf("fuel %d after a second charge, want it still 1", ctx.Args[1])
	}
}

// The property the whole thing is for: with a budget set, the tier must not
// hold more cells than the interpreter does running the same program.
//
// Measured on the pools' chunk storage rather than on process RSS. The pools
// never shrink, so that figure is the high-water mark of simultaneously
// allocated cells — which is exactly what a late collection inflates, and it
// does not move with whatever else the machine is doing.
func TestTierDoesNotOutgrowTheInterpreterUnderAByteBudget(t *testing.T) {
	// One loop, far longer than jitFuel, allocating payload it does not keep.
	// Longer than the fuel window on purpose: a loop that finishes inside one
	// window reaches its safepoint by ending, and would pass this whether or not
	// anything below works.
	const src = `
		function build(n) {
			var kept = [];
			for (var i = 0; i < n; i++) {
				var s = ("row-" + i + "-").repeat(12).toUpperCase();
				if (i % 5000 === 0) kept.push(s);
			}
			return kept.length;
		}
		build(300000);
	`

	reserved := func(jit bool) uint64 {
		saved := jitEnabled
		defer func() { jitEnabled = saved }()
		jitEnabled = jit

		rt := New()
		rt.SetHeapLimit(64 << 20)
		if _, e := rt.RunString("fuel_test.js", src); e != nil {
			t.Fatalf("jit=%v: %v", jit, e)
		}
		_, b := rt.HeapReserved()
		return b
	}

	off, on := reserved(false), reserved(true)
	t.Logf("cell storage held: interpreter %d, tier %d", off, on)
	// A margin rather than equality: the tier allocates a little of its own —
	// feedback vectors, the compiled function's own objects — and the chunk
	// granularity is 4096 cells, so the two are not required to agree exactly.
	if on > off+off/4 {
		t.Errorf("tier holds %d bytes of cell storage, interpreter %d: more than a quarter over", on, off)
	}
}
