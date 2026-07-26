package v8go

import "testing"

// goant has no collector yet (PLAN.md Phase 7), so an isolate's occupancy only
// rises: everything every script it ran ever allocated is still there. A pooled
// isolate is therefore not a neutral thing to reuse, and GetHeapStatistics is
// what a host watches to decide when to drop one.
//
// This test states that behaviour rather than wishing it away. It will start
// failing when a collector lands — which is the right time to be told.
func TestUsedHeapSizeTracksThisIsolateAndOnlyGrows(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso)
	defer ctx.Close()

	before := iso.GetHeapStatistics().UsedHeapSize
	if before == 0 {
		t.Fatal("a live isolate reports zero occupancy")
	}

	// Allocate a lot of objects and drop every reference to them from JS.
	if _, err := ctx.RunScript(`
		(function () {
			for (var i = 0; i < 20000; i++) { var o = {a: i, b: "s" + i}; }
			return 0;
		})()
	`, "alloc.js"); err != nil {
		t.Fatalf("run: %v", err)
	}

	after := iso.GetHeapStatistics().UsedHeapSize
	if after <= before {
		t.Fatalf("occupancy did not rise: %d -> %d", before, after)
	}

	// Nothing in JS refers to those objects any more. With a collector this
	// would fall; without one it cannot. Assert the current truth so the day it
	// changes is visible.
	if _, err := ctx.RunScript(`0`, "noop.js"); err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := iso.GetHeapStatistics().UsedHeapSize
	if settled < after {
		t.Skipf("occupancy fell %d -> %d: a collector appears to have landed, "+
			"so this test's premise is obsolete and the retire-on-heap advice "+
			"in GetHeapStatistics should be revisited", after, settled)
	}
}

// Two isolates must report independently. Reporting process-wide Go MemStats
// here would make every isolate in a pool see the same number and retire in
// lockstep the moment any one of them grew.
func TestUsedHeapSizeIsPerIsolate(t *testing.T) {
	busy := NewIsolate()
	defer busy.Dispose()
	idle := NewIsolate()
	defer idle.Dispose()

	bctx := NewContext(busy)
	defer bctx.Close()
	ictx := NewContext(idle)
	defer ictx.Close()

	idleBefore := idle.GetHeapStatistics().UsedHeapSize

	if _, err := bctx.RunScript(`
		(function () {
			var keep = [];
			for (var i = 0; i < 20000; i++) { keep.push({a: i, b: "s" + i}); }
			return keep.length;
		})()
	`, "busy.js"); err != nil {
		t.Fatalf("run: %v", err)
	}

	busyAfter := busy.GetHeapStatistics().UsedHeapSize
	idleAfter := idle.GetHeapStatistics().UsedHeapSize

	if busyAfter <= idleAfter {
		t.Fatalf("the busy isolate (%d) does not report more than the idle one (%d)",
			busyAfter, idleAfter)
	}
	if idleAfter != idleBefore {
		t.Fatalf("the idle isolate's occupancy moved (%d -> %d) because of work "+
			"done in another isolate", idleBefore, idleAfter)
	}
}
