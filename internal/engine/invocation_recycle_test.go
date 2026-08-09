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
	rt := New()

	// Enough allocation to cross the collection threshold several times over,
	// and nothing shared touched: every object is made and dropped inside the
	// run.
	const src = `
		let n = 0;
		for (let i = 0; i < 60000; i++) {
			const o = {};
			o["k" + (i % 4)] = i;
			n += o["k" + (i % 4)];
		}
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

	// Allocate through the point where the free list would have been consulted.
	for i := 0; i < 4096; i++ {
		v := rt.newObject(rt.objectProto)
		if h := Handle(v.handle()); h < mark {
			t.Fatalf("allocation %d got handle %d, below the watermark %d", i, h, mark)
		}
	}
	inv.End()

	// And the held cells come back, so they are not lost to the Runtime.
	if len(rt.objects.freeList) == 0 {
		t.Error("the cells held during the run were not returned to the free list")
	}
	if len(rt.objects.held) != 0 {
		t.Errorf("%d cells still held after the run ended", len(rt.objects.held))
	}
}

// Release rewinds the whole region. A cell recycled from below the watermark
// would not have been rewound — truncate only frees upward — so the object
// would have outlived the run that made it.
func TestReleaseLeavesNothingTheRunAllocated(t *testing.T) {
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
	if after := rt.objects.liveN; after > before {
		t.Errorf("%d cells survived the release that did not exist before the run", after-before)
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
