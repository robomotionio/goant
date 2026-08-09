package engine

import "testing"

// A run that collects must still be releasable.
//
// The whole of region reclamation rests on one comparison: a handle below the
// invocation's watermark names an object older than the invocation. A
// collection used to break it. The realm's own construction leaves garbage
// behind, the first sweep inside a run reclaimed it, and those low handles went
// back on the free list — so the next object the SCRIPT allocated was handed
// one, and the first write to it was read as a write to shared state.
//
// The consequences were all invisible. Release refused, so nothing was
// reclaimed and the pools grew for the life of the process; and a host that
// discards a dirty Runtime — which is what the contract tells it to do — threw
// away its warm engine after every message large enough to collect, which is
// exactly the messages where warmth was worth something.
func TestARunThatCollectsIsNotDirty(t *testing.T) {
	// Both arms, because a compiled property store does the watermark
	// comparison in machine code rather than calling out, and fixing only the
	// Go path leaves the tier reporting every large run dirty — which shows up
	// as the host not getting its memory back, and nowhere else.
	for _, jit := range []bool{false, true} {
		name := "interpreter"
		if jit {
			name = "tier"
		}
		t.Run(name, func(t *testing.T) {
			saved := jitEnabled
			defer func() { jitEnabled = saved }()
			jitEnabled = jit

			rt := New()

			// Enough allocation to cross the collection threshold several times
			// over, and nothing shared touched: every object is made and dropped
			// inside the run. Wrapped in a function entered often enough to be
			// compiled, so the stores go through emitted code in the tier arm.
			const src = `
				function build(n) {
					let s = 0;
					for (let i = 0; i < n; i++) {
						const o = {};
						o["k" + (i % 4)] = i;
						s += o["k" + (i % 4)];
					}
					return s;
				}
				let n = 0;
				for (let k = 0; k < 12; k++) n += build(5000);
				n;
			`

			inv := rt.BeginInvocation()
			if _, err := rt.RunString("recycle_test.js", src); err != nil {
				t.Fatalf("run: %v", err)
			}
			if rt.gc.cycles == 0 {
				t.Fatal("the script did not collect, so this proves nothing — raise the loop count")
			}
			if inv.Dirty() {
				t.Error("reported as having modified shared state; it modified nothing shared")
			}
			if !inv.Release() {
				t.Error("Release refused")
			}
		})
	}
}

// The invariant itself, stated directly: while a run is in progress, nothing it
// allocates may land below the watermark.
func TestNothingAllocatedDuringARunLandsBelowTheWatermark(t *testing.T) {
	rt := New()

	// Give the pool free cells below the watermark to hand out, which is the
	// situation the bug needed: a collection with dead realm scaffolding in it.
	rt.collect()
	if len(rt.objects.freeList) == 0 {
		t.Skip("no free cells to recycle, so there is nothing to get wrong here")
	}

	inv := rt.BeginInvocation()
	mark := inv.marks.objects

	// Recycling is allowed — a free list that stood aside at every Begin would
	// never be drawn from again and the pool would climb forever — so what has
	// to hold is the weaker and sufficient thing: a recycled cell is marked as
	// this run's own, and writing to it is not a shared mutation.
	low := 0
	for i := 0; i < 4096; i++ {
		v := rt.newObject(rt.objectProto)
		if h := Handle(v.handle()); h < mark {
			low++
			if !rt.bornInRun(h) {
				t.Fatalf("allocation %d recycled handle %d from below the watermark %d without marking it", i, h, mark)
			}
			rt.noteSharedMutation(v)
			if inv.Dirty() {
				t.Fatalf("writing to the object at recycled handle %d was read as a shared mutation", h)
			}
		}
	}
	if low == 0 {
		t.Skip("nothing was recycled, so this proves nothing")
	}
	inv.End()

	// And the marks go away with the run: those cells are as old as anything
	// else now, and the next run must see them that way.
	if n := len(rt.objects.reborn); n != 0 {
		t.Errorf("%d cells still marked as this run's after it ended", n)
	}
	for h := Handle(1); h < mark; h++ {
		if cl := rt.objects.cell(h); cl != nil && cl.born {
			t.Fatalf("handle %d still marked after End", h)
		}
	}
}

// What Release leaves behind, and why that is allowed.
//
// The rewind frees upward from the watermark, so a cell the run was handed from
// BELOW it is not rewound and the object in it outlives the run. That is the
// price of letting the free list be reused at all, and it is a small and
// self-clearing one: a handful of cells, holding objects nothing outside the
// region can reach — which the dirty test has already established, since a run
// that wrote to anything older than itself cannot be released — so the next
// collection takes them.
//
// The property to hold is therefore not "nothing survives" but "nothing
// survives that a collection will not take".
func TestReleaseLeavesNothingReachableBehind(t *testing.T) {
	rt := New()
	rt.collect()

	inv := rt.BeginInvocation()
	before := rt.objects.liveN
	if _, err := rt.RunString("release_test.js", `
		let n = 0;
		for (let i = 0; i < 60000; i++) { const o = { i: i }; n += o.i; }
		n;
	`); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !inv.Release() {
		t.Fatal("Release refused")
	}
	stranded := rt.objects.liveN - before
	if stranded > 64 {
		t.Errorf("%d cells survived the release, which is more than recycling can account for", stranded)
	}
	rt.collect()
	if after := rt.objects.liveN; after > before {
		t.Errorf("%d cells survived a collection after the release: they are reachable from somewhere", after-before)
	}
}

// A run that really does modify shared state must still be caught. The fix
// removes false positives; it must not remove the true one.
func TestWritingToASharedObjectIsStillDirty(t *testing.T) {
	rt := New()
	rt.collect()

	inv := rt.BeginInvocation()
	// Allocate enough to have gone through the free list, then reach a builtin.
	if _, err := rt.RunString("dirty_test.js", `
		for (let i = 0; i < 60000; i++) { const o = {}; o.x = i; }
		Array.prototype.polluted = 1;
	`); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !inv.Dirty() {
		t.Error("a write to Array.prototype was not reported")
	}
	if inv.Release() {
		t.Error("Release freed the region of a run that wrote to shared state")
	}
}

// Release rewinds the heap, so the threshold sized for it has to go too.
// Leaving it in place made the next run allocate the whole of the last run's
// peak before collecting once — and a run that collects late holds more, which
// raises the threshold again. Thirteen messages of that took it from sixteen
// thousand cells to a hundred and eighty-four thousand.
func TestReleaseResizesTheCollectionThreshold(t *testing.T) {
	rt := New()

	inv := rt.BeginInvocation()
	if _, err := rt.RunString("ratchet_test.js", `
		const kept = [];
		for (let i = 0; i < 60000; i++) kept.push({ i: i });
		kept.length;
	`); err != nil {
		t.Fatalf("run: %v", err)
	}
	grown := rt.gc.next
	if !inv.Release() {
		t.Fatal("Release refused")
	}
	if rt.gc.next >= grown {
		t.Errorf("threshold still %d after the release that freed the heap it was sized for", rt.gc.next)
	}
	if want := rt.objects.liveN * gcGrowthFactor; rt.gc.next < gcFloor && rt.gc.next != want {
		t.Errorf("threshold %d, want the floor %d or twice what survived (%d)", rt.gc.next, gcFloor, want)
	}
}

// The list of recycled cells is per cell, not per allocation. A long run that
// takes and drops the same cell over and over would otherwise append to it
// every time, and a run does not have to end for that to matter.
func TestRecycledCellsAreRecordedOncePerCell(t *testing.T) {
	rt := New()
	rt.collect()
	if len(rt.objects.freeList) == 0 {
		t.Skip("no free cells to recycle, so there is nothing to bound")
	}

	inv := rt.BeginInvocation()
	free := len(rt.objects.freeList)
	for i := 0; i < 20000; i++ {
		v := rt.newObject(rt.objectProto)
		rt.freeObject(Handle(v.handle()))
	}
	if n := len(rt.objects.reborn); n > free {
		t.Errorf("recorded %d recycled cells from %d distinct free ones", n, free)
	}
	inv.End()
}

// Release lowers the collection threshold and must never raise it.
//
// Re-deriving it outright looks equivalent and is not. After a rewind what
// survived is small, so the derived figure is the floor — and a host that
// releases often pushes the threshold back up to the floor before the live
// count can ever reach it, so the collector never runs again. On goant-soak
// that was 3.6 GB in twenty seconds against 60 MB.
//
// It does not show up on a workload whose garbage is objects. The trigger
// counts object cells, so a run whose garbage is strings has nothing else to
// stop it, which is why this is asserted directly rather than left to a
// benchmark to notice.
func TestReleaseNeverRaisesTheCollectionThreshold(t *testing.T) {
	rt := New()

	// A threshold below the floor, which is what a small realm's own collection
	// leaves behind and exactly the case the re-derivation got wrong.
	rt.gc.next = 2000

	inv := rt.BeginInvocation()
	if _, err := rt.RunString("floor_test.js", `
		let s = "";
		for (let i = 0; i < 2000; i++) s = ("k" + i).toUpperCase();
		s;
	`); err != nil {
		t.Fatalf("run: %v", err)
	}
	before := rt.gc.next
	if !inv.Release() {
		t.Fatal("Release refused")
	}
	if rt.gc.next > before {
		t.Errorf("threshold raised from %d to %d: a host that releases often would never collect again",
			before, rt.gc.next)
	}
}
