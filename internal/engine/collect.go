package engine

import (
	"os"
	"reflect"
	"strconv"
	"sync"
	"unsafe"

	"github.com/robomotionio/goant/internal/jitmem"
)

// Tracing collection over the handle pools.
//
// A Value carries a 32-bit handle, not a pointer, so the Go collector never
// traces the JavaScript heap — which is what keeps a Value eight bytes wide and
// keeps Go's write barriers out of the interpreter. The price is that Go cannot
// tell a dead object from a live one either: the pool chunk holds a strong
// reference to every cell ever allocated. Until this file existed nothing was
// ever reclaimed inside a single script, and a long-running one simply grew.
// (Octane's RegExp and EarleyBoyer reached five gigabytes.)
//
// So the engine traces its own heap. Only the cells are managed here: freeing
// one zeroes it, and everything hanging off it — a string's bytes, an object's
// overflow slots, its shape, a closure's upvalues — becomes unreachable to the
// Go collector, which reclaims it as usual. This is a collector for handles
// sitting on top of Go's collector for memory, not a replacement for it.
//
// # When a collection may happen
//
// Not at an allocation. Native built-ins are ordinary Go functions holding
// values in ordinary Go locals: `arr := rt.newArray()` followed by another
// allocation would see arr collected out from under it, and making that safe
// would mean rooting intermediates in every built-in in the engine.
//
// Instead a collection happens only at an interpreter safepoint — a function
// entry or a loop back edge — and only when no native is on the Go stack. At
// such a point every value the engine is holding is either published in a
// vmFrame or reachable from the Runtime, and the collector can find all of it.
// A program spending all its time inside one native call does not collect, and
// that is the correct trade: it cannot be made safe cheaply, and it does not
// happen in practice, because a native that runs long enough to matter calls
// back into the interpreter.
//
// # Roots
//
// The Runtime's own fields are found by reflection rather than by a hand-written
// list. A missed root is a use-after-free that shows up as an unrelated crash
// much later, and the Runtime carries hundreds of value-bearing fields that
// change as the engine grows. Reflection cannot forget one. It runs once per
// collection over a few hundred fields, which does not show up next to the
// trace, and the walk is pruned by a memoised "can this type reach a Value at
// all" test so it never descends into bytecode, source text or maps of strings.
//
// Two kinds of reference are invisible to that walk and have to be added by
// hand. A bare Handle looks like any other integer — rt.interned is a map of
// text to string handles, and missing it swept every property name in the
// program. And a Go closure is opaque: a built-in written as one keeps the
// spec's internal slots in captured variables that nothing can enumerate, so
// they are registered separately (holdCaptures, Runtime.asyncFrames, job.roots).
//
// # Debugging
//
// Two environment variables, read once at start-up:
//
//	GOANT_GC_FLOOR=n   collect once n objects are live rather than at the
//	                   default threshold, so a short program collects often.
//	GOANT_GC_POISON=1  do not recycle a swept cell; panic at the first use of
//	                   one instead. Together these turn a missing root from a
//	                   corrupted value much later into a Go stack trace at the
//	                   exact read that used the freed cell.

// gcState is the collector's persistent state: the mark bitsets (reused
// between cycles), the trace worklist, and the trigger threshold.
type gcState struct {
	objMarks markSet
	strMarks markSet
	symMarks markSet
	cloMarks markSet
	bigMarks markSet

	work    []Value
	seenPtr map[uintptr]bool

	// next is the live object count that triggers the next collection, and
	// floor is the smallest it may be set to. After a cycle the threshold moves
	// to a multiple of what survived, so a program with a large live set does
	// not collect continuously, and a small one does not hold on to memory it
	// has stopped using.
	next  int
	floor int

	// strNext is next for the STRING pool: the live string-cell count that is
	// reason enough to collect on its own. See stringsFull.
	strNext int

	// enabled turns automatic collection on. It is on by default.
	enabled bool
	// cycles counts completed collections, for tests and diagnostics.
	cycles int
}

// gcGrowthFactor is how much the live set may grow before the next collection.
// Two is the usual space/time trade: at most half the heap is garbage when a
// cycle starts, and the work per cycle stays proportional to what survives.
const gcGrowthFactor = 2

// gcFloor is the smallest live-object count worth collecting at. It has to sit
// above a realm's own intrinsics, or a script collects repeatedly before it has
// allocated anything of its own. A realm is 1045 cells, so this leaves about
// fifteen times the headroom that needs.
//
// It was 1<<16, and that was too high, for a reason particular to this engine:
// THE CELL POOLS NEVER SHRINK. A chunk allocated at the high-water mark is held
// for the life of the Runtime — truncate only rewinds the watermark — so one
// unlucky peak becomes the process's permanent footprint. Collecting earlier is
// therefore worth more here than in a program whose allocator gives memory
// back, and the usual reasoning about sweep frequency understates it.
//
// Measured on a robot's own workload, a million-record message through a
// Function node, thirteen times in one process:
//
//	floor    tier on            interpreter
//	1<<16    2199 MB, 2478 ms   1562 MB
//	1<<14    1494 MB, 2401 ms   1446 MB
//	1<<12    1486 MB, 2395 ms   1498 MB
//
// The tier gives back 705 MB — 32% — and gets 3% FASTER doing it, because a
// sweep that runs sooner is a smaller sweep over a live set that still fits in
// cache. 1<<12 buys nothing further, so 1<<14 is the knee and not merely a
// smaller number.
//
// The tier was the arm that needed this. It allocates faster, so more garbage
// is in flight whenever a collection triggers, and Go then computes its own
// next heap goal from that inflated figure — the two collectors compounding
// each other. Live heap after a sweep was never the problem and is comparable
// either way; the peak before one was.
var gcFloor = 1 << 14

// gcStringFloor is gcFloor for the string pool: the same amount of STORAGE,
// rather than the same number of cells. A string cell is 88 bytes against an
// object's 224, so counting them alike would collect nearly three times as
// often for the same resident bytes — and string-heavy code is common enough
// that the difference is not academic.
//
// Approximate on purpose. It ignores the out-of-line bytes a string points at,
// which are real but are the part chargeBytes already watches when a host has
// set a limit; this floor exists for the host that has set none, where being
// somewhere near right is the whole requirement. Derived rather than written
// down so that GOANT_GC_FLOOR moves both together.
//
// Making it SLACKER than the object floor is the obvious lever on the cost of
// this trigger, and it was measured and rejected. On the orders workload with
// no limit set, the extra collections cost 5.5%, and eight times the floor
// would have bought nearly all of that back — by reserving about 25 MB more
// string storage than it saved. That is trading the thing this exists for
// against the thing it costs, in the wrong direction: the whole point is
// resident bytes. A host that wants the other trade sets a heap limit, and
// then chargeBytes runs this schedule instead of it.
func gcStringFloor() int {
	n := gcFloor * int(unsafe.Sizeof(poolCell[object]{})) / int(unsafe.Sizeof(poolCell[flatString]{}))
	if n < 1 {
		n = 1
	}
	return n
}

// gcStrParked is what stringsFull leaves in gc.strNext when the collector is
// off: a value no live count reaches, so a Runtime running with WithGC(false)
// pays the test on each string and never the call.
const gcStrParked = 1 << 62

// gcByteFloor is the smallest amount of out-of-line payload worth collecting
// for. Deliberately well under any sane heap limit: the limit can only be
// tested at a collection, so this is also the coarsest granularity at which a
// byte budget can be enforced at all.
const gcByteFloor = 8 << 20

// reserveBytes reports whether an allocation of n bytes fits inside the heap
// limit, stopping the script if it does not. False means "do not allocate".
//
// A budget tested only after the fact cannot save a host from a single
// allocation larger than what it has left, and `s += s` reaches that size in
// about twenty iterations: the doubling that takes the process down is the same
// one the post-sweep check was waiting to complain about.
//
// It deliberately tests gc.liveBytes — what actually survived the last sweep —
// and not allocBytes, so a script that allocates a great deal and keeps almost
// none of it still passes. That promise is the whole reason the limit is judged
// on what survives. Collecting here to sharpen the estimate is not an option:
// outside a safepoint not every value is published, and a collection that
// cannot see them frees them.
func (rt *Runtime) reserveBytes(n uint64) bool {
	if rt.heapLimit == 0 || rt.interrupt == nil {
		return true
	}
	if rt.liveBytes+n <= rt.heapLimit {
		return true
	}
	rt.interrupt.flag.Store(interruptMemory)
	return false
}

// chargeBytes records out-of-line payload. Approximate by design — it is a
// collection TRIGGER, not the accounting the limit is judged on. That comes
// from liveBytes, recomputed from what actually survives each sweep, so an
// over- or under-charge here costs at most an early or late collection and
// never a wrong answer.
// Nothing reads allocBytes without a limit set, so charging is skipped entirely
// there rather than maintaining a counter no one will look at.
//
// When the budget is exhausted this pulls in gc.next — the live-cell threshold
// the interpreter already tests at every safepoint — rather than adding a byte
// test of its own beside it. That is what keeps the cost of this feature off
// the hot path completely: maybeCollect and backEdgeWantsGC are byte for byte
// what they were before the memory limit existed, so a script with no limit set
// cannot pay for one. Reading a second field on every loop back edge is not
// free — it is another cache line in the interpreter's working set, and it
// measured 7-10% on an idle machine.
func (rt *Runtime) chargeBytes(n uint64) {
	if rt.heapLimit == 0 {
		return
	}
	rt.allocBytes += n
	if rt.allocBytes >= rt.nextBytes && rt.objects.liveN > 0 {
		// Due a collection on bytes. Say so in the currency the safepoints
		// already speak: a threshold the live count has reached.
		rt.gc.next = rt.objects.liveN
		if rt.jitDepth > 0 {
			rt.jitCollectSoon()
		}
	}
}

// stringsFull is called when the live string-cell count has reached gc.strNext:
// the string pool has as much in it as it is allowed before a collection.
//
// It exists because both collection triggers — maybeCollect on function entry
// and backEdgeWantsGC on every loop back edge — test the live OBJECT count and
// nothing else. A script that allocates only strings was therefore unwatched:
// four thousand calls to ("k"+i).toUpperCase() reached 2.4 MILLION live string
// cells with zero collections, because the object count never moved. A host that
// had set a heap limit was covered by chargeBytes; an embedder that set none had
// nothing watching at all, and simply grew.
//
// It is the no-limit HALF of chargeBytes, and defers to it whenever there is a
// limit — see the heapLimit check below for what running both cost.
//
// The threshold is the live count and the schedule is twice what survived, which
// is the rule gc.next already follows for objects. Triggering on the pool
// RESERVING another chunk instead reads as the same thing and is not: the high
// water mark never rewinds, so each cycle ends by taking exactly one more chunk
// and reserved storage grows as the square root of total allocations. Measured
// on a 200k-iteration loop that keeps one string: 147 chunks and no collection
// before any of this, 17 and still climbing on reserved growth, 11 flat here.
//
// Cost is one load and a not-taken branch per string created (strings.go) —
// liveN is the field alloc just incremented — and nothing at all at either
// safepoint. Putting a second counter on the back edge is exactly what this
// avoids; that measured 7-10%.
//
//go:noinline
func (rt *Runtime) stringsFull() {
	if !rt.gc.enabled {
		// Nothing will collect, so stop asking for the rest of the run. See
		// SetGCEnabled, which is what puts the threshold back.
		rt.gc.strNext = gcStrParked
		return
	}
	if rt.heapLimit != 0 {
		// Already watched, and watched better. chargeBytes charges every string's
		// bytes against a budget that is twice what SURVIVED the last sweep with
		// an 8 MB floor, and trips the same gc.next this would. Counting cells on
		// top of that is not a second opinion, it is a second schedule for one
		// resource — and it showed up as one: on the deskbot orders workload with
		// its 2 GiB limit, 13 collections became 28 and the run cost 10% more
		// while the byte trigger was already keeping the pool bounded.
		//
		// So this trigger is what a host gets INSTEAD of chargeBytes, not as well
		// as it. Strides past rather than parking, because SetHeapLimit(0) can
		// take the budget away again and nothing would put the mark back.
		rt.gc.strNext = rt.strings.liveN + gcStringFloor()
		return
	}
	// Say it in the currency the safepoints already speak: a threshold the live
	// object count has reached. Exactly what chargeBytes does, and for the same
	// reason — so neither safepoint has to read anything new.
	rt.gc.next = rt.objects.liveN
	if rt.jitDepth > 0 {
		rt.jitCollectSoon()
	}
	// Asked. Move the mark on so the next allocation does not ask again.
	//
	// The collection happens at the next safepoint, not here, and a native can
	// allocate a great many strings before reaching one — JSON.stringify and
	// Array.prototype.join build a whole result inside a single call. Without
	// this, every string after the first would make this call. A floor's worth
	// of headroom bounds it to one call per floor even when nothing collects at
	// all, and collect() overwrites the mark properly as soon as one does.
	rt.gc.strNext = rt.strings.liveN + gcStringFloor()
}

// jitCollectSoon brings every live compiled frame back to Go at its next loop
// back edge, by spending the fuel it has left.
//
// The interpreter answers "a collection is due" on the very next back edge,
// because that is where it tests. Compiled code has no such test: its only
// safepoint is the fuel counter running out, so it kept allocating for up to
// jitFuel more iterations — twenty thousand — past the moment the budget said
// stop. That is not a small overshoot on a loop that allocates two strings a
// row, and it is why the tier's resident peak sat above the interpreter's on
// exactly the workload this engine ships for.
//
// Setting the fuel to one rather than adding a check is what makes this free.
// The decrement and the not-taken branch are already in every loop; nothing new
// is emitted, nothing extra is read, and the steady-state cost is unchanged.
// Sweeping GOANT_JIT_FUEL down to 1 gives the same memory profile by making
// every iteration a safepoint, and pays a round trip per iteration for it.
//
// ONE, not zero: the back edge decrements and branches on the result, so a zero
// here wraps to 2^64-1 and the loop would run for the rest of the process.
//
// Every frame in the chain, not just the innermost: an outer frame resumes with
// whatever fuel it had, and a caller that loops around a callee would otherwise
// carry on to its own expiry. Costing one store each on a chain that is a
// handful deep.
//
// Safe to write from here because a compiled frame only reaches Go through a
// helper, so nothing is executing when this runs, and the back edge re-reads
// the counter from memory every iteration rather than holding it in a register.
//
//go:noinline
func (rt *Runtime) jitCollectSoon() {
	for _, ctx := range rt.jitFrames[:rt.jitDepth] {
		if ctx.Args[1] > 1 {
			ctx.Args[1] = 1
		}
	}
}

// jitGrant is how much fuel a compiled frame gets: enough to reach the next
// collection, and never more than jitFuel.
//
// The byte budget above can say "collect now" and be obeyed, because it is a
// decision the runtime makes while compiled code is stopped in a helper. The
// count trigger cannot: it is not a decision at all, it is a threshold the live
// count drifts past, and the only code watching is the interpreter's back edge.
// A compiled loop crosses it and keeps going until its fuel runs out, which on
// a fifty-thousand-row message is two and a half more windows of allocation.
// Measured on that message: the interpreter collected twenty-two times and the
// tier seven, and held 28% more cell storage for it.
//
// So the grant is sized to the headroom instead of fixed. The rate comes from
// the window that just ended — cells allocated divided by iterations run — and
// the next window is however many iterations that rate says will consume what
// is left. A loop that allocates nothing measures a rate of zero and keeps the
// full twenty thousand, which is the case that must not regress: those loops
// are what the tier is for, and the round trip is the cost it exists to avoid.
//
// Self-correcting rather than exact. Overshooting means one collection happens
// a little late; undershooting means one extra round trip in twenty thousand
// iterations. Both are cheap, and neither can be wrong about anything but
// timing.
func (rt *Runtime) jitGrant(ctx *jitmem.ExecContext) uint64 {
	live := rt.objects.liveN
	f := jitFuel
	if !rt.gc.enabled {
		ctx.FuelBase, ctx.LiveBase = f, int64(live)
		return f
	}
	next := rt.gc.next
	if next == 0 {
		next = gcFloor
	}
	if head := next - live; head <= 0 {
		// Already past it. One iteration, then the back edge takes the exit and
		// the collection happens there.
		f = 1
	} else {
		// A window with a rate behind it runs until the rate says the headroom
		// is gone. One without runs jitProbe iterations to acquire one.
		//
		// The unmeasured window is the one that matters most, and that is not
		// obvious. An overshoot there is not a one-off: the next threshold is
		// twice what SURVIVED the late collection, so a window that ran long
		// raises every threshold after it by the same proportion, and the
		// sequence compounds from there. One unmeasured window at twenty
		// thousand iterations cost 311,296 cells of held storage by the end of a
		// fifty-thousand-row message — forty-five megabytes, out of six thousand
		// cells of overshoot. Guessing a rate instead does not fix it either:
		// this loop allocates four or five cells an iteration, so a guess of one
		// is wrong by that factor and overshoots by it.
		//
		// So do not guess. Measure, on a window short enough that being wrong
		// about it costs nothing.
		est := uint64(jitProbe)
		// used is negative when a collection landed inside the window, and zero
		// for a loop that allocates nothing: no rate either way, so probe again.
		// The second case is the one that must stay cheap — a loop that
		// allocates nothing is what the tier is for — and it does: its next
		// window measures zero, and a rate of zero takes the full grant.
		if used := int64(live) - ctx.LiveBase; used > 0 && ctx.FuelBase > 0 {
			est = uint64(head) * ctx.FuelBase / uint64(used)
		}
		if est < f {
			f = est
			if f == 0 {
				f = 1
			}
		}
	}
	ctx.FuelBase, ctx.LiveBase = f, int64(live)
	return f
}

func osGetenvGCPoison() bool { return envOn("GOANT_GC_POISON") }

func init() {
	if v := os.Getenv("GOANT_GC_FLOOR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			gcFloor = n
		}
	}
}

// maybeCollect runs a collection if the heap has grown past the threshold and
// the engine is at a point where every live value is published.
//
// Called from the interpreter's safepoints only. The native-depth test is what
// makes the whole scheme sound; see the note above.
func (rt *Runtime) maybeCollect() {
	if !rt.gc.enabled || rt.nativeDepth > 0 {
		return
	}
	if rt.gc.next == 0 {
		rt.gc.next = gcFloor
	}
	// Unchanged from before the memory limit existed, deliberately: a byte
	// threshold tested here would be read on every safepoint by every script,
	// including the ones that set no limit. chargeBytes lowers gc.next instead,
	// so growth in bytes arrives as growth in the count this already tests.
	if rt.objects.liveN < rt.gc.next {
		return
	}
	rt.collect()
}

// liveePayload sums the out-of-line bytes the surviving cells hold: string
// bytes and their cached views, array element storage, ArrayBuffer stores,
// bigint words. Capacity rather than length throughout — a slice holding a
// megabyte of spare capacity is a megabyte the host cannot use for anything
// else, and a doubling accumulator spends half its life in that state.
// Written as two open loops over the chunk vectors rather than through a shared
// helper on pool[T]. Adding a method to that generic type changes the code the
// compiler generates for the pool as a whole — including alloc and free, which
// are the hottest routines in the engine — and measured ~7% on scripts that
// never set a limit and never reach this function. The duplication is the
// cheaper of the two costs.
func (rt *Runtime) liveePayload() uint64 {
	var n uint64
	for c := range rt.strings.chunks {
		chunk := rt.strings.chunks[c]
		base := Handle(c << poolChunkShift)
		for s := range chunk {
			h := base + Handle(s)
			if h == nullHandle {
				continue
			}
			if h >= rt.strings.next {
				break
			}
			if !chunk[s].live {
				continue
			}
			fs := &chunk[s].elem
			n += uint64(cap(fs.bytes)) + uint64(len(fs.gostr)) + uint64(cap(fs.utf16))*4
		}
	}
	for c := range rt.objects.chunks {
		chunk := rt.objects.chunks[c]
		base := Handle(c << poolChunkShift)
		for s := range chunk {
			h := base + Handle(s)
			if h == nullHandle {
				continue
			}
			if h >= rt.objects.next {
				break
			}
			if !chunk[s].live {
				continue
			}
			o := &chunk[s].elem
			n += uint64(cap(o.arr))*uint64(unsafe.Sizeof(Value(0))) + uint64(cap(o.abuf))
			if o.ext != nil {
				// The exotic half, which hangs off the cell rather than living
				// in it. Counted here so that moving it out of the struct did
				// not quietly stop it being counted at all.
				n += uint64(unsafe.Sizeof(objectExt{}))
			}
		}
	}
	return n
}

// enforceHeapLimit stops the running script if what survived the collection is
// over budget.
//
// It runs only after a collection, which is the whole design: the question a
// host actually needs answered is not "has this script allocated a lot" — every
// script does — but "is this script holding a lot", and only a completed mark
// and sweep can tell the difference. A loop that builds and discards a million
// objects passes; one that builds and keeps them does not.
//
// Stopping here rather than at the allocation is also what makes the failure
// reportable. The alternative is Go's own out-of-memory, which is a runtime
// throw: no panic, no recover, no deferred anything, and the process is gone
// along with every other flow sharing it.
// The byte accounting lives here rather than in collect, and that placement is
// load-bearing. collect already called this at the end of a cycle and this
// already returned immediately without a limit, so folding the work in leaves
// collect byte for byte as it was. Writing the same four lines directly into
// collect instead — even guarded, even never executed — cost 5-8% on scripts
// that set no limit, because a call site added to collect changes the code the
// compiler generates for the collector, and the collector is hot. It is not
// enough for new work to be skipped at run time; on this path it has to be
// absent from the function.
func (rt *Runtime) enforceHeapLimit() {
	if rt.heapLimit == 0 || rt.interrupt == nil {
		return
	}
	// Total what survived is HOLDING, not merely how many cells survived. Done
	// now because "live" is exact only between the sweep and the next
	// allocation.
	rt.liveBytes = rt.liveePayload()
	rt.allocBytes = 0
	rt.nextBytes = rt.liveBytes * gcGrowthFactor
	if rt.nextBytes < gcByteFloor {
		rt.nextBytes = gcByteFloor
	}
	if _, bytes := rt.HeapUsage(); bytes > rt.heapLimit {
		rt.interrupt.flag.Store(interruptMemory)
	}
}

// SetGCEnabled turns automatic collection on or off. It is on by default; turn
// it off for a run short enough that nothing needs reclaiming, or one that ends
// with Invocation.Release.
func (rt *Runtime) SetGCEnabled(on bool) {
	rt.gc.enabled = on
	if on && rt.gc.strNext == gcStrParked {
		// Nothing was measuring what is live while the collector was off, and
		// liveN is now mostly garbage, so deriving a threshold from it would put
		// the goalpost past everything the run has accumulated. Start from the
		// floor and let the first sweep set the real one.
		rt.gc.strNext = gcStringFloor()
	}
}

// GCCycles reports how many collections have completed. For tests.
func (rt *Runtime) GCCycles() int { return rt.gc.cycles }

// LiveObjects reports how many object cells are currently allocated.
func (rt *Runtime) LiveObjects() int { return rt.objects.liveN }

// Collect runs a full mark-and-sweep immediately, whether or not automatic
// collection is enabled. Exposed for hosts that know they have just finished
// with a large working set, and used by the tests.
//
// It is only safe at a point where the engine is not inside a native call; the
// interpreter's safepoints are such points, and so is a return to the host.
func (rt *Runtime) Collect() {
	if rt.nativeDepth > 0 {
		return
	}
	rt.collect()
}

func (rt *Runtime) collect() {
	g := &rt.gc
	g.objMarks = rt.objects.newMarks(g.objMarks)
	g.strMarks = rt.strings.newMarks(g.strMarks)
	g.symMarks = rt.symbols.newMarks(g.symMarks)
	g.cloMarks = rt.closures.newMarks(g.cloMarks)
	g.bigMarks = rt.bigints.newMarks(g.bigMarks)
	g.work = g.work[:0]

	rt.markRoots()
	rt.drain()
	rt.sweepWeakTables()

	rt.objects.sweep(g.objMarks)
	rt.strings.sweep(g.strMarks)
	rt.symbols.sweep(g.symMarks)
	rt.closures.sweep(g.cloMarks)
	rt.bigints.sweep(g.bigMarks)

	rt.forEachRealm(func(r *Runtime) { r.objMemo = [objMemoSize]objMemoEntry{} })

	// An inline cache holds its prototype holder as a raw *object, which may now
	// name a freed cell. Retiring every entry is what guarantees it is never
	// dereferenced again; the caches refill within a few iterations.
	icEpochBump()

	g.cycles++
	if g.floor == 0 {
		g.floor = gcFloor
	}
	g.next = rt.objects.liveN * gcGrowthFactor
	if g.next < g.floor {
		g.next = g.floor
	}

	// The same rule for the string pool, over its own floor.
	g.strNext = rt.strings.liveN * gcGrowthFactor
	if f := gcStringFloor(); g.strNext < f {
		g.strNext = f
	}

	// Total what survived is HOLDING, not just how many cells survived. Done
	// here because "live" is exact only between the sweep and the next
	// allocation — but only when a limit is set, because the scan is O(pool
	// capacity) and nothing without a budget to enforce needs the answer on
	// every cycle. HeapUsage computes it on demand for those.

	// Checked here rather than in maybeCollect so that every collection counts:
	// a loop that never calls a function collects straight from the back edge
	// (backEdgeWantsGC) and would otherwise grow unwatched, which is exactly
	// the shape that runs a host out of memory.
	rt.enforceHeapLimit()
}

// ---- marking ----

// markValue records v's cell and queues it for tracing.
func (rt *Runtime) markValue(v Value) {
	if !v.isTagged() {
		return
	}
	g := &rt.gc
	h := Handle(v.handle())
	if h == nullHandle {
		return
	}
	switch v.Type() {
	case TObj, TArr, TFunc, TPromise, TGenerator, TTypedArray, TErr,
		TMap, TSet, TWeakMap, TWeakSet:
		if !g.objMarks.set(h) {
			g.work = append(g.work, v)
		}
	case TStr:
		// A string Value does not carry a bare handle: its payload is
		// (handle << 2) | a two-bit flat/rope/builder tag, so the handle has to
		// be unpacked. Marking v.handle() marks the bit for cell h*4 instead —
		// which is to say it marks nothing that exists and leaves every string
		// the program computed unmarked. Interned names survived (markRoots
		// marks the intern table by handle), which is why this only ever
		// surfaced as a computed string turning into an unrelated one.
		g.strMarks.set(strHandle(v))
	case TSymbol:
		if !g.symMarks.set(h) {
			g.work = append(g.work, v)
		}
	case TBigInt:
		g.bigMarks.set(h)
	}
}

// drain traces everything reachable from the worklist.
//
// Iterative rather than recursive: a long prototype chain or a deep cons list
// would otherwise decide how much Go stack a collection needs.
func (rt *Runtime) drain() {
	g := &rt.gc
	for len(g.work) > 0 {
		v := g.work[len(g.work)-1]
		g.work = g.work[:len(g.work)-1]
		switch v.Type() {
		case TSymbol:
			if s := rt.symbols.get(Handle(v.handle())); s != nil {
				rt.markValue(s.desc)
			}
		default:
			rt.traceObject(rt.objects.get(Handle(v.handle())))
		}
	}
}

// traceObject marks everything an object refers to.
func (rt *Runtime) traceObject(o *object) {
	if o == nil {
		return
	}
	rt.markValue(o.proto)
	rt.markValue(o.boxed)
	for i := range o.inobj {
		rt.markValue(o.inobj[i])
	}
	rt.markSlice(o.overflow)
	rt.markSlice(o.arr)
	for i := range o.extra() {
		rt.markValue(o.extra()[i].value)
	}
	// Accessor getters and setters live in the shape, not in a slot, so an
	// object's own shape has to be walked even though shapes are Go-managed.
	rt.traceShape(o.shape)
	for i := range o.priv() {
		rt.tracePrivElem(&o.priv()[i])
	}
	if o.closure != nullHandle {
		rt.traceClosure(o.closure)
	}
	if o.coll() != nil {
		rt.markSlice(o.coll().keys)
		rt.markSlice(o.coll().vals)
	}
	if o.promise() != nil {
		rt.markValue(o.promise().value)
		for i := range o.promise().handlers {
			h := &o.promise().handlers[i]
			rt.markValue(h.onFulfilled)
			rt.markValue(h.onRejected)
			rt.markValue(h.result)
			rt.markValue(h.capResolve)
			rt.markValue(h.capReject)
		}
	}
	if o.gen() != nil {
		rt.traceGen(o.gen())
	}
	if o.proxy != nil {
		rt.traceAny(reflect.ValueOf(o.proxy))
	}
	if o.argMap() != nil {
		rt.markSlice(o.argMap().locals)
	}
	if o.ta != nil || o.dv() != nil {
		// A view's own fields are handles into Go-owned storage plus the buffer
		// object it reads through; reflection covers both without this file
		// having to track the view layouts.
		if o.ta != nil {
			rt.traceAny(reflect.ValueOf(o.ta))
		}
		if o.dv() != nil {
			rt.traceAny(reflect.ValueOf(o.dv()))
		}
	}
}

func (rt *Runtime) traceShape(s *shape) {
	if s == nil {
		return
	}
	for i := range s.props {
		p := &s.props[i]
		if p.isAccessor {
			rt.markValue(p.getter)
			rt.markValue(p.setter)
		}
	}
}

func (rt *Runtime) tracePrivElem(p *privElem) {
	rt.traceAny(reflect.ValueOf(p).Elem())
}

func (rt *Runtime) traceClosure(h Handle) {
	if rt.gc.cloMarks.set(h) {
		return
	}
	cl := rt.closures.get(h)
	if cl == nil {
		return
	}
	rt.markValue(cl.home)
	rt.markSlice(cl.capturedWith)
	for _, u := range cl.upvalues {
		if u != nil {
			rt.markValue(u.get())
		}
	}
	rt.traceFunc(cl.fn)
	rt.tracePrivScope(cl.privEnv)
}

// traceFunc marks a compiled function's constant pool.
//
// Constants are values — interned names, literal strings, regexp sources — that
// the bytecode reaches by index and nothing else refers to.
func (rt *Runtime) traceFunc(fn *svFunc) {
	for fn != nil {
		rt.markSlice(fn.constants)
		for i := range fn.childFuncs {
			rt.traceFunc(fn.childFuncs[i])
		}
		fn = fn.moduleHoistFn
	}
}

func (rt *Runtime) tracePrivScope(p *privScope) {
	if p != nil {
		rt.traceAny(reflect.ValueOf(p))
	}
}

func (rt *Runtime) traceGen(g *genState) {
	if g == nil {
		return
	}
	rt.markValue(g.fnVal)
	rt.markValue(g.thisVal)
	rt.markValue(g.awaiting)
	rt.markValue(g.completion)
	rt.markSlice(g.args)
	if g.cl != nil {
		rt.markValue(g.cl.home)
		rt.markSlice(g.cl.capturedWith)
		for _, u := range g.cl.upvalues {
			if u != nil {
				rt.markValue(u.get())
			}
		}
		rt.traceFunc(g.cl.fn)
	}
	rt.traceFunc(g.fn)
	// A suspended coroutine's frames are live and its per-depth buffers hold
	// their values, which nothing else refers to while it is parked.
	rt.markSlabs(g.slabs)
	rt.markFrames(g.frames)
	rt.traceAny(reflect.ValueOf(&g.asyncReqs).Elem())
}

func (rt *Runtime) markSlice(vs []Value) {
	for i := range vs {
		rt.markValue(vs[i])
	}
}

// parkedFrames is one driver's frame state, set aside while a coroutine runs on
// its behalf. See Runtime.parked and genDrive.
type parkedFrames struct {
	frames []vmFrame
	slabs  []frameSlab
}

func (rt *Runtime) markSlabs(slabs []frameSlab) {
	for i := range slabs {
		s := &slabs[i]
		rt.markSlice(s.locals[:cap(s.locals)])
		rt.markSlice(s.stack[:cap(s.stack)])
	}
}

// ---- roots ----

func (rt *Runtime) markFrames(frames []vmFrame) {
	for i := range frames {
		f := &frames[i]
		rt.traceFunc(f.fn)
		if f.cl != nil {
			rt.markValue(f.cl.home)
			rt.markSlice(f.cl.capturedWith)
			for _, u := range f.cl.upvalues {
				if u != nil {
					rt.markValue(u.get())
				}
			}
			rt.traceFunc(f.cl.fn)
		}
		rt.markSlice(f.locals)
		rt.markSlice(f.stack[:cap(f.stack)])
		rt.markSlice(f.args)
		rt.markSlice(f.withStack)
		rt.markValue(f.thisVal)
		rt.markValue(f.fnVal)
		rt.markValue(f.varObj)
		rt.markValue(f.newTarget)
		rt.markValue(f.pending)
		rt.markValue(f.completed)
	}
}

// forEachRealm runs f over every realm sharing these pools, which is the unit
// the collector works in: a handle names the same cell in all of them.
func (rt *Runtime) forEachRealm(f func(*Runtime)) {
	if rt.agent == nil {
		f(rt)
		return
	}
	for _, r := range rt.agent.realms {
		f(r)
	}
}

func (rt *Runtime) markRoots() {
	if rt.gc.seenPtr == nil {
		rt.gc.seenPtr = make(map[uintptr]bool, 256)
	}
	clear(rt.gc.seenPtr)

	rt.forEachRealm(func(r *Runtime) {
		// Live frames first: their values are the ones with no other reference.
		rt.markFrames(r.frames)
		// Retired frame buffers are scanned too: a frame at a depth the collector
		// cannot see (past the published range, or one that allocated its own) may
		// still be running.
		rt.markSlabs(r.slabs)

		// And the frames of every driver that has handed the engine to a
		// coroutine. rt.frames above is whichever set is CURRENT — while a
		// generator runs, that is the generator's, and the chain that called it
		// is here. Missing these swept the caller's own locals out from under it.
		for i := range r.parked {
			rt.markFrames(r.parked[i].frames)
			rt.markSlabs(r.parked[i].slabs)
		}

		// The intern table maps text to a string cell by Handle, not by Value, so
		// the reflective walk cannot see it — a Handle is a bare uint32 and looks
		// like any other integer. Nothing else refers to an interned string, so
		// missing it swept every property name in the program, and lookups started
		// failing against an empty name. Anything else that holds a Handle rather
		// than a Value belongs here too.
		for _, h := range r.interned {
			rt.gc.strMarks.set(h)
		}

		// A compiled frame suspended in a helper holds its operand stack in the
		// ExecContext, and nothing else refers to those values: the registers
		// they came from are gone, and the frame's locals slice does not contain
		// them. Only the slots compiled code declared live are traced — a stale
		// one from an earlier call holds a handle to a cell that may since have
		// been freed. Args[0] and Args[1] are a pointer and a counter, and
		// Args[3] is an immediate, so none of the three is a Value.
		// The live part of the chain, and only that: contexts are built ahead of
		// where anything has run and are never freed, so past this depth they
		// hold whatever the last frame there left behind — see jitCtxAt.
		for _, ctx := range r.jitFrames[:r.jitDepth] {
			rt.markValue(Value(ctx.Args[2]))
			rt.markValue(Value(ctx.Ret))
			// The receiver, which reaches compiled code as an integer and is
			// otherwise reachable only from the interpreter frame that entered
			// it — a frame this walk does not descend into.
			rt.markValue(Value(ctx.This))
			// The running function itself, for the self-reference a named
			// function expression binds.
			rt.markValue(Value(ctx.FnVal))
			// The operand stack the frame left behind when it called out.
			// StackN is compiled code's own statement of how much of it is
			// live; past that the slots hold whatever an earlier frame at this
			// depth wrote, which is exactly what must not be traced.
			for _, v := range jitFrameStack(ctx) {
				rt.markValue(v)
			}
			// And its variables, for a frame a compiled call site opened. Those
			// have no vmFrame and no entry in the locals slab — the context is
			// the whole of what they published — so this is the only place they
			// are reachable from. NLocals is zero for every other frame.
			for _, v := range jitCtxLocals(ctx) {
				rt.markValue(v)
			}
		}

		// A suspended async function has no object to hang its coroutine off, so
		// asyncFrames is what keeps it alive; the reflective walk below finds it
		// by descending into the map's keys. This pass is still needed on top of
		// that: reflection walks a slice to its length, and a frame's operand
		// stack has to be scanned to its capacity (see markSlabs).
		for g := range r.asyncFrames {
			rt.traceGen(g)
		}

		rt.traceAny(reflect.ValueOf(r).Elem())
	})
}

// valueType is what the reflective walk is looking for; objectPtrType and the
// weak-table key type are the two it has to treat specially.
var (
	valueType       = reflect.TypeOf(Value(0))
	objectPtrType   = reflect.TypeOf((*object)(nil))
	jitCallSiteType = reflect.TypeOf(jitCallSite{})
)

// traceAny walks an arbitrary Go value and marks every engine Value in it.
//
// This is how the Runtime's own fields are rooted, and how the handful of
// side structures with fiddly layouts (proxies, typed-array views, private
// scopes) are traced. It is not on any hot path: it runs once per collection.
func (rt *Runtime) traceAny(rv reflect.Value) {
	if !canHoldValue(rv.Type()) {
		return
	}
	rt.traceHolder(rv)
}

// traceHolder is traceAny for a value whose type has already been found to hold
// Values. The split exists for the slice case: asking the question per element
// meant asking it thousands of times about one type, and it was 5% of
// earley-boyer — a lookup in the type cache on every element of every slab.
func (rt *Runtime) traceHolder(rv reflect.Value) {
	switch rv.Kind() {
	case reflect.Uint64:
		if rv.Type() == valueType {
			rt.markValue(Value(rv.Uint()))
		}
	case reflect.Struct:
		// Per field, because each has its own type and most of them stop here.
		for i := 0; i < rv.NumField(); i++ {
			rt.traceAny(rv.Field(i))
		}
	case reflect.Array, reflect.Slice:
		// The element type is the same for every element, so it is asked about
		// once. An element's own kind may still be a pointer or an interface,
		// whose dynamic type is not known from the static one — those go back
		// through traceAny below.
		if !canHoldValue(rv.Type().Elem()) {
			return
		}
		for i, n := 0, rv.Len(); i < n; i++ {
			rt.traceHolder(rv.Index(i))
		}
	case reflect.Pointer:
		if rv.IsNil() {
			return
		}
		if rv.Type() == objectPtrType {
			// A strong reference to an object held as a Go pointer. Only the
			// object itself knows which cell it is, which is what self is for.
			rt.markValue(mkval(TObj, uint64((*object)(rv.UnsafePointer()).self)))
			return
		}
		p := rv.Pointer()
		if rt.gc.seenPtr[p] {
			return
		}
		rt.gc.seenPtr[p] = true
		rt.traceAny(rv.Elem())
	case reflect.Interface:
		if !rv.IsNil() {
			rt.traceAny(rv.Elem())
		}
	case reflect.Map:
		if rv.Type().Key() == objectPtrType {
			// An object-keyed side table (iterator state, finalization cells).
			// These hold state ON BEHALF of an object and must not be what keeps
			// it alive, or every iterator ever made would be immortal. Handled
			// after the main trace, in sweepWeakTables.
			return
		}
		iter := rv.MapRange()
		for iter.Next() {
			rt.traceAny(iter.Key())
			rt.traceAny(iter.Value())
		}
	}
}

// canHoldValue reports whether a type can transitively contain a Value, so the
// walk can stop at bytecode, source text, and every other structure that
// cannot.
//
// A pool is excluded deliberately: it IS the heap, and walking it would mark
// every cell that ever existed, which is the opposite of collecting. Its live
// cells are reached through the roots like everything else.
func canHoldValue(t reflect.Type) bool {
	if v, ok := valueBearing.Load(t); ok {
		return v.(bool)
	}
	// Assume yes while recursing, so a cyclic type resolves rather than
	// looping; the entry is corrected on the way out.
	valueBearing.Store(t, true)
	r := computeHoldsValue(t)
	valueBearing.Store(t, r)
	return r
}

func computeHoldsValue(t reflect.Type) bool {
	if t == valueType {
		return true
	}
	switch t.Kind() {
	case reflect.Struct:
		if isPoolType(t) {
			return false
		}
		if t == jitCallSiteType {
			// A compiled call site's cached callee is NOT a root, and tracing it
			// was wrong in both directions.
			//
			// It cannot be used after a collection: the emitted guard compares
			// the site's epoch against the global counter and collect() bumps
			// that counter, so every site is retired and refills on its next
			// call. See jitCallSite.
			//
			// And the walk only ever reached it by accident — through
			// Runtime.frames, so a site was traced when its function happened to
			// be on the stack and not otherwise. That made it an intermittent
			// root: a callee dropped while its caller was idle got swept, and the
			// next collection with the caller running marked a dead cell. Under
			// GOANT_GC_POISON that is a panic in the collector itself; without it,
			// a recycled handle silently kept an unrelated object alive.
			return false
		}
		for i := 0; i < t.NumField(); i++ {
			if canHoldValue(t.Field(i).Type) {
				return true
			}
		}
		return false
	case reflect.Pointer, reflect.Slice, reflect.Array:
		if isPoolType(t.Elem()) {
			return false
		}
		return canHoldValue(t.Elem())
	case reflect.Map:
		return canHoldValue(t.Key()) || canHoldValue(t.Elem())
	case reflect.Interface:
		// An interface's dynamic type is unknown statically, so it has to be
		// followed. There are few of these and none on a hot path.
		return true
	default:
		return false
	}
}

func isPoolType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct && len(t.Name()) >= 4 && t.Name()[:4] == "pool"
}

// valueBearing memoises canHoldValue. It is a sync.Map rather than a plain one
// because the answer depends only on the type, so Runtimes on different
// goroutines may fill it concurrently.
var valueBearing sync.Map

// sweepWeakTables settles the object-keyed side tables.
//
// They hold state on behalf of an object — an iterator's position, a
// FinalizationRegistry's cells — and are keyed by it. Treating them as ordinary
// roots would make every object that ever had such state immortal, which for a
// program that iterates in a loop is most of the heap. So they are weak: an
// entry survives only if its key survived on its own account, and then whatever
// the entry refers to is marked too.
//
// Marking an entry's value can resurrect a key of another entry, so this
// repeats until a pass adds nothing. Entries whose key did not survive are
// deleted, which is what stops the tables themselves from growing without
// bound.
func (rt *Runtime) sweepWeakTables() {
	for {
		before := len(rt.gc.work)
		rt.markLiveEntries()
		if len(rt.gc.work) == before {
			break
		}
		rt.drain()
	}
	rt.dropDeadEntries()
}

func (rt *Runtime) markLiveEntries() {
	rt.markAgentLiveEntries()
	rt.forEachRealm(rt.markRealmLiveEntries)
}

// markAgentLiveEntries settles the tables the whole agent shares — one pass, not
// one per realm, since every realm names the same map.
func (rt *Runtime) markAgentLiveEntries() {
	a := rt.agent
	if a == nil {
		return
	}
	for o, st := range a.arrIterStates {
		if rt.objAlive(o) {
			rt.markValue(st.src)
		}
	}
	for o, st := range a.collIterStates {
		if rt.objAlive(o) {
			rt.traceAny(reflect.ValueOf(st).Elem())
		}
	}
	for o, st := range a.strIterStates {
		if rt.objAlive(o) {
			rt.traceAny(reflect.ValueOf(st).Elem())
		}
	}
	for o, st := range a.regexpStrIterStates {
		if rt.objAlive(o) {
			rt.traceAny(reflect.ValueOf(st).Elem())
		}
	}
}

// markRealmLiveEntries settles one realm's weak tables. The marks belong to the
// Runtime driving the collection, the tables to the realm being scanned.
func (rt *Runtime) markRealmLiveEntries(r *Runtime) {
	for o, cells := range r.finRegistries {
		if rt.objAlive(o) {
			rt.traceAny(reflect.ValueOf(cells))
		}
	}
	for o, groups := range r.natCaptures {
		if rt.objAlive(o) {
			for _, g := range groups {
				rt.markSlice(g)
			}
		}
	}
}

func (rt *Runtime) dropDeadEntries() {
	rt.dropAgentDeadEntries()
	rt.forEachRealm(rt.dropRealmDeadEntries)
}

// dropAgentDeadEntries is dropDeadEntries for the agent-wide tables; see
// markAgentLiveEntries.
func (rt *Runtime) dropAgentDeadEntries() {
	a := rt.agent
	if a == nil {
		return
	}
	for o := range a.arrIterStates {
		if !rt.objAlive(o) {
			delete(a.arrIterStates, o)
		}
	}
	for o := range a.collIterStates {
		if !rt.objAlive(o) {
			delete(a.collIterStates, o)
		}
	}
	for o := range a.strIterStates {
		if !rt.objAlive(o) {
			delete(a.strIterStates, o)
		}
	}
	for o := range a.regexpStrIterStates {
		if !rt.objAlive(o) {
			delete(a.regexpStrIterStates, o)
		}
	}
}

func (rt *Runtime) dropRealmDeadEntries(r *Runtime) {
	for o := range r.finRegistries {
		if !rt.objAlive(o) {
			delete(r.finRegistries, o)
		}
	}
	for o := range r.natCaptures {
		if !rt.objAlive(o) {
			delete(r.natCaptures, o)
		}
	}
}

func (rt *Runtime) objAlive(o *object) bool { return o != nil && rt.gc.objMarks.has(o.self) }

// beginDriver roots the working set of a self-driving native built-in until
// endDriver is called, and returns the handle to extend and to release.
//
// Unlike holdCaptures there is no object to key this on: a driver like
// Array.fromAsync exists only as a chain of promise reactions, and even the
// promise it will settle is reachable only from its own closures. Append to
// *d as later values appear; call endDriver on every path that settles, or the
// working set is immortal.
func (rt *Runtime) beginDriver(vals ...Value) *[]Value {
	d := &vals
	if rt.nativeDrivers == nil {
		rt.nativeDrivers = map[*[]Value]bool{}
	}
	rt.nativeDrivers[d] = true
	return d
}

// endDriver releases a working set rooted by beginDriver.
func (rt *Runtime) endDriver(d *[]Value) { delete(rt.nativeDrivers, d) }

// holdCaptures records values that a built-in written as a Go closure keeps on
// behalf of owner.
//
// The spec gives such built-ins internal slots — an iterator helper's
// [[UnderlyingIterators]], a promise resolve function's [[Promise]]. goant
// mostly implements them as captured Go variables instead, and a func value is
// opaque: neither the reflective root walk nor a hand-written trace can see
// inside one. Registering them here is what makes them findable, and keying by
// the owning object is what gives them the right lifetime — the state dies with
// the object it belongs to, and no earlier.
//
// The slices are held by reference, so a closure that overwrites an element
// stays in step with the collector. A closure that reassigns a whole captured
// variable must keep that variable in a one-element slice registered here, or
// the collector will keep tracing the value it replaced.
func (rt *Runtime) holdCaptures(owner Value, groups ...[]Value) {
	o := rt.objPtr(owner)
	if o == nil {
		return
	}
	if rt.natCaptures == nil {
		rt.natCaptures = map[*object][][]Value{}
	}
	rt.natCaptures[o] = append(rt.natCaptures[o], groups...)
}
