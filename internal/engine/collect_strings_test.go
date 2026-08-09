package engine

import (
	"strconv"
	"testing"
)

// The shape that went unwatched: a loop whose only allocation is strings. The
// live OBJECT count never moves, so neither collection trigger ever fired, and
// with no heap limit set chargeBytes is inert too.
const stringChurn = `
	let last = "";
	for (let i = 0; i < 200000; i++) last = ("k" + i).toUpperCase();
	last
`

// TestStringGarbageIsCollectedWithNoHeapLimit is the regression for the gap.
// deskbot sets a limit, so this was invisible from the product; any other
// embedder had nothing watching it at all.
//
// Measured on the loop above, which allocates about 600,000 string cells and
// keeps one: before, 147 chunks reserved and not a single collection. The
// numbers here are the fixed behaviour with room around them, because what has
// to hold is that reserved storage stays FLAT — a longer loop must not reserve
// more — not that it lands on a particular chunk count.
func TestStringGarbageIsCollectedWithNoHeapLimit(t *testing.T) {
	rt := New()
	if rt.heapLimit != 0 {
		t.Fatal("a fresh Runtime already has a heap limit; this test needs none")
	}
	if _, err := rt.RunString("churn.js", stringChurn); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rt.gc.cycles == 0 {
		t.Fatal("600k string allocations and not one collection")
	}
	// Reserved chunks, not live cells: the pools never shrink, so what a host
	// pays for is the high-water mark.
	if chunks := len(rt.strings.chunks); chunks > 24 {
		t.Fatalf("string pool reserved %d chunks (%d cells) for a loop that keeps one string",
			chunks, chunks*poolChunkSize)
	}
}

// TestStringReserveIsFlatInTheNumberOfAllocations is the property the previous
// test only samples. Ten times the work must not mean ten times the storage —
// and it must not mean a square root of it either, which is what triggering on
// the pool RESERVING a chunk gives, since the high-water mark never rewinds.
func TestStringReserveIsFlatInTheNumberOfAllocations(t *testing.T) {
	reserved := func(iters int) int {
		rt := New()
		if _, err := rt.RunString("churn.js", `
			let last = "";
			for (let i = 0; i < `+strconv.Itoa(iters)+`; i++) last = ("k" + i).toUpperCase();
			last
		`); err != nil {
			t.Fatalf("run: %v", err)
		}
		return len(rt.strings.chunks)
	}
	small, large := reserved(50000), reserved(500000)
	if large > small+2 {
		t.Fatalf("10x the allocations reserved %d chunks against %d: storage is tracking work, not live data",
			large, small)
	}
}

// TestHeldStringsSurviveAndCostLogCollections. The other side: strings that are
// genuinely live must not be swept, and holding a large set must not mean
// collecting once per chunk of it.
func TestHeldStringsSurviveAndCostLogCollections(t *testing.T) {
	rt := New()
	if _, err := rt.RunString("hold.js", `
		const held = [];
		for (let i = 0; i < 200000; i++) held.push(("k" + i).toUpperCase());
		held.length
	`); err != nil {
		t.Fatalf("run: %v", err)
	}
	chunks := len(rt.strings.chunks)
	if chunks < 8 {
		t.Fatalf("200k held strings reserved only %d chunks; this workload is not what the test thinks", chunks)
	}
	if rt.gc.cycles > 4*bitLen(chunks) {
		t.Fatalf("%d collections for %d reserved chunks; the threshold is not following what survives",
			rt.gc.cycles, chunks)
	}
	v, err := rt.RunString("hold2.js", `held.length + ":" + held[199999] + ":" + held[0]`)
	if err != nil {
		t.Fatalf("reading the held strings back: %v", err)
	}
	if got, want := rt.strGo(v), "200000:K199999:K0"; got != want {
		t.Fatalf("held strings after collection = %q, want %q", got, want)
	}
}

func bitLen(n int) int {
	b := 0
	for n > 0 {
		b++
		n >>= 1
	}
	return b
}

// TestASmallScriptStillCollectsNothing. The floor exists so a short script — the
// shape a pooled Runtime runs thousands of times — does not pay for a sweep it
// has no use for.
func TestASmallScriptStillCollectsNothing(t *testing.T) {
	rt := New()
	if _, err := rt.RunString("small.js", `
		let s = "";
		for (let i = 0; i < 500; i++) s = ("k" + i).toUpperCase();
		s
	`); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rt.gc.cycles != 0 {
		t.Fatalf("a 500-iteration loop collected %d times", rt.gc.cycles)
	}
}

// TestStringFloorIsStorageNotCells. Counting string cells against the object
// floor would collect nearly three times as often for the same resident bytes.
func TestStringFloorIsStorageNotCells(t *testing.T) {
	if got := gcStringFloor(); got <= gcFloor {
		t.Fatalf("gcStringFloor() = %d, want more than the object floor %d "+
			"(a string cell is smaller, so the same storage is more cells)", got, gcFloor)
	}
}

// TestAHeapLimitKeepsTheOneScheduleItAlreadyHad. The string trigger is the
// no-limit half of chargeBytes, not a second opinion beside it: with a limit
// set, every string's bytes are already charged against a budget derived from
// what survived, and that budget already trips gc.next.
//
// Running both is not free and not theoretical. It is what the first version of
// this did, and on the deskbot orders workload — a large live object graph, a
// small live string set, a 2 GiB limit — it turned 13 collections into 28 and
// cost 10%, while the byte trigger was keeping the pool bounded on its own.
func TestAHeapLimitKeepsTheOneScheduleItAlreadyHad(t *testing.T) {
	withLimit := New()
	withLimit.SetHeapLimit(2 << 30)
	if _, err := withLimit.RunString("churn.js", stringChurn); err != nil {
		t.Fatalf("run: %v", err)
	}

	noLimit := New()
	if _, err := noLimit.RunString("churn.js", stringChurn); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The point is not a particular count — the byte trigger collects on this
	// workload too. It is that adding the cell trigger on top does not multiply
	// them, which is what a second schedule for one resource does.
	if withLimit.gc.cycles > noLimit.gc.cycles {
		t.Errorf("with a heap limit: %d collections; without one: %d. "+
			"The limited Runtime is paying for both schedules.",
			withLimit.gc.cycles, noLimit.gc.cycles)
	}
	if withLimit.gc.cycles == 0 {
		t.Error("a limited Runtime collected nothing at all; chargeBytes is supposed to be the watcher here")
	}
}

// TestStringGrowthIsSilentWithTheCollectorOff. WithGC(false) is a promise, and a
// new trigger must not break it — nor keep calling into the collector for the
// rest of the run once it knows nothing will happen.
func TestStringGrowthIsSilentWithTheCollectorOff(t *testing.T) {
	rt := New()
	rt.SetGCEnabled(false)
	if _, err := rt.RunString("churn.js", stringChurn); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rt.gc.cycles != 0 {
		t.Fatalf("collector disabled, but %d collections ran", rt.gc.cycles)
	}
	// And turning it back on has to unpark the threshold, or the Runtime is
	// permanently blind to strings.
	rt.SetGCEnabled(true)
	if rt.gc.strNext > rt.strings.liveN*gcGrowthFactor+gcStringFloor() {
		t.Fatalf("string threshold still parked at %d after re-enabling the collector", rt.gc.strNext)
	}
	// A block, because the two scripts share one global lexical environment and
	// stringChurn declares `last` at the top level of it.
	if _, err := rt.RunString("churn2.js", "{"+stringChurn+"}"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rt.gc.cycles == 0 {
		t.Fatal("collector re-enabled and still nothing collected")
	}
}
