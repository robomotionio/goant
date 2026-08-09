package engine

// Invocations: running one short script with its own globals, cheaply.
//
// A host that runs millions of tiny independent scripts — a message arrives, a
// function transforms it, bytes go out — needs each run isolated from the last.
// The obvious way to get that is a fresh realm per run, and it is what an
// embedder inherits from engines whose contexts are cheap because their initial
// heap is a snapshot.
//
// goant has no snapshot, so building a realm means constructing every prototype
// and every builtin from nothing. Measured: 366 µs and 885 allocations, against
// roughly 6 µs for the work the script was actually asked to do. The isolation
// cost 60 times what the invocation cost.
//
// Almost none of a realm needs isolating. The builtins are identical every time
// and a script that does not modify them cannot tell whether they were rebuilt.
// What genuinely differs per run is the small amount of state a script installs:
// properties on globalThis, top-level var and function declarations, and
// top-level let/const/class bindings.
//
// So an Invocation keeps one realm and gives each run a fresh global object
// whose prototype is the shared one. Builtins resolve up the chain; anything the
// script assigns lands on the fresh object and goes away with it. Measured at
// 111 ns and one allocation — about three thousand times cheaper than a realm,
// and the end-to-end invocation drops from 401 µs to 8.6 µs.
//
// The deliberate deviation: Object.getPrototypeOf(globalThis) is the shared
// global rather than Object.prototype, so the chain is one link longer than a
// script would see elsewhere. Property resolution is unaffected — that is what
// makes this work — and own-key enumeration is unaffected because builtins are
// non-enumerable. Nothing else about the global is observably different.

// Invocation is one script run with its own global state. Begin one, run
// whatever the host needs, then End it; the globals the run installed are
// discarded and the next Invocation starts clean.
//
// Invocations do not nest usefully and are not safe to interleave: a Runtime
// runs one script at a time, which is the same constraint every other engine
// places on an isolate.
type Invocation struct {
	rt *Runtime

	prevGlobal   Value
	prevLex      map[string]*globalLexBinding
	prevWatermrk Handle
	prevInterned []string
	prevHold     Handle
	marks        poolMarks
	ended        bool
}

// poolMarks is where each pool's allocator stood when the invocation began.
type poolMarks struct {
	objects  Handle
	strings  Handle
	symbols  Handle
	closures Handle
	bigints  Handle
}

// BeginInvocation starts a run with a fresh global object.
func (rt *Runtime) BeginInvocation() *Invocation {
	inv := &Invocation{
		rt:           rt,
		prevGlobal:   rt.global,
		prevLex:      rt.globalLex,
		prevWatermrk: rt.invWatermark,
		prevInterned: rt.invInterned,
		marks: poolMarks{
			objects:  rt.objects.next,
			strings:  rt.strings.next,
			symbols:  rt.symbols.next,
			closures: rt.closures.next,
			bigints:  rt.bigints.next,
		},
	}

	// Arm shared-state detection before allocating anything for this run, so the
	// fresh global itself counts as the invocation's own. Taking the watermark
	// afterwards would make every write to globalThis look like a write to
	// shared state.
	rt.beginDirtyTracking()
	rt.invInterned = nil

	// Tell the pool where the boundary is, so that a cell recycled from below it
	// gets marked as this run's own. Without that mark the dirty test cannot
	// tell a new object in a recycled cell from an object older than the run,
	// and reads every write to one as a write to shared state — which costs the
	// caller the whole of region reclamation, and only on the messages big
	// enough to collect. See poolCell.born.
	//
	// Holding those cells back instead of marking them was the other way to do
	// it, and it is wrong: a free list that is set aside at every Begin is never
	// drawn from again, so the pool stops reusing anything and climbs forever.
	// Thirty gigabytes in two minutes, on a soak of short runs.
	inv.prevHold = rt.objects.watermark
	rt.objects.watermark = inv.marks.objects

	// The fresh global inherits from the shared one, so every builtin resolves
	// through the prototype chain while assignments land here and are dropped at
	// End.
	fresh := rt.newObject(rt.global)
	rt.objPtr(fresh).defineOwn("globalThis", fresh, attrWritable|attrConfigurable)
	rt.global = fresh

	// Top-level let/const/class live in the declarative half of the global
	// environment, which is separate from the global object. Without clearing it
	// a second run would see the first run's bindings — and re-declaring one
	// would be an error rather than a fresh binding. Left nil so it is only
	// allocated if the script actually declares something.
	rt.globalLex = nil

	// Swapping the set of global lexical bindings changes which names the
	// global object's own slots are visible under, and GET_GLOBAL's cache
	// records that visibility. Retiring the entries here (and in End) keeps the
	// two invocations from reading each other's answers.
	icEpochBump()

	return inv
}

// End discards the invocation's globals and restores the shared ones. Calling
// it twice is harmless.
func (inv *Invocation) End() {
	if inv == nil || inv.ended {
		return
	}
	inv.ended = true
	inv.rt.global = inv.prevGlobal
	inv.rt.globalLex = inv.prevLex
	icEpochBump()
	inv.rt.invWatermark = inv.prevWatermrk
	inv.rt.invInterned = inv.prevInterned

	// The cells this run was given from below the watermark are nobody's news
	// now: with the run over they are as old as everything else, and the next
	// one must see them that way.
	p := inv.rt.objects
	p.watermark = inv.prevHold
	for _, h := range p.reborn {
		if cl := p.cell(h); cl != nil {
			cl.born = false
		}
	}
	p.reborn = p.reborn[:0]
}

// Release ends the invocation and frees everything it allocated, in one step
// and without tracing anything.
//
// This is region reclamation, and it fits this workload exactly: a run
// allocates a message graph, produces a result, and every object it made dies
// together. There is nothing to mark, no roots to enumerate, no write barriers
// — the allocator simply rewinds.
//
// It is sound only because of the dirty check. If the run never wrote to an
// object that predates it, then nothing outside the region can point into it,
// so the whole region is unreachable by construction. A run that did write to
// shared state cannot be released, and Release reports false without freeing —
// the caller should discard the Runtime instead.
//
// EVERY Value created during the invocation becomes invalid, including the
// script's result. A caller must extract what it needs — serialise the result
// to bytes — BEFORE calling this. Reading a Value afterwards reads a recycled
// cell, which is the one way to get a wrong answer out of this API.
func (inv *Invocation) Release() bool {
	if inv == nil || inv.ended {
		return false
	}
	rt := inv.rt
	if rt.invDirty {
		inv.End()
		return false
	}

	// Side tables keyed by object pointer hold state for iterators, registries
	// and namespaces created during the run. Their keys are about to be recycled
	// cells, so a stale entry could be matched by a future object at the same
	// address. The iterator tables are the agent's, so one clear does them; the
	// rest are per realm, and a realm the script itself made (createRealm) holds
	// keys into the same pools — dropping only this realm's left those behind.
	rt.agent.collIterStates = nil
	rt.agent.arrIterStates = nil
	rt.agent.strIterStates = nil
	rt.agent.regexpStrIterStates = nil
	rt.forEachRealm(func(r *Runtime) {
		r.finRegistries = nil
		r.moduleNamespaces = nil
		r.natCaptures = nil
		r.nativeDrivers = nil
	})

	// The intern table outlives the invocation; drop what this run added.
	for _, k := range rt.invInterned {
		delete(rt.interned, k)
	}

	// The cells this run was handed from BELOW the watermark are its own — that
	// is what the mark on them says — and the rewind below cannot reach them,
	// because it frees upward from the mark and they are under it. Freed here
	// instead, by name: the list is exactly the region's lower half.
	//
	// Without this they are stranded live, one small pile per released run, and
	// a host alternating Release with End accumulates them faster than the
	// collector's threshold rises to notice. Freeing them is sound for the same
	// reason the rewind is: the dirty check has already established that
	// nothing outside the region points into it.
	for _, h := range rt.objects.reborn {
		rt.objects.free(h) // a no-op on one the run already freed
	}

	inv.End()

	rt.objMemo = [objMemoSize]objMemoEntry{}
	rt.objects.truncate(inv.marks.objects)
	rt.strings.truncate(inv.marks.strings)
	rt.symbols.truncate(inv.marks.symbols)
	rt.closures.truncate(inv.marks.closures)
	rt.bigints.truncate(inv.marks.bigints)

	// The collection threshold is a multiple of what was live, and what was live
	// has just gone. Left alone it stays sized for the heap this run built, so
	// the NEXT run allocates that whole peak again before it collects once —
	// and a run that collects late finds more still reachable, which raises the
	// threshold again. Over thirteen messages that ratchet took it from sixteen
	// thousand cells to a hundred and eighty-four thousand, and since the pools
	// never shrink, every step of it was paid resident for good.
	//
	// LOWER ONLY, and that is not a detail. Re-deriving it outright — the floor
	// when what survived is small, which after a rewind it always is — pushes
	// the threshold UP whenever the last collection had set it lower, and a host
	// that releases often then pushes it up again before the live count can ever
	// reach it. The collector simply stops running. It cost 3.6 GB in twenty
	// seconds on goant-soak, against 60 MB with this left alone, and it did not
	// show up on a workload whose garbage is objects: at the time the only
	// trigger counted object cells, so a run whose garbage was strings had
	// nothing else to stop it. That is now stringsFull's job, and its threshold
	// ratchets the same way and is rewound here under the same rule.
	want := rt.objects.liveN * gcGrowthFactor
	if want < gcFloor {
		want = gcFloor
	}
	if want < rt.gc.next {
		rt.gc.next = want
	}
	if strWant := max(rt.strings.liveN*gcGrowthFactor, gcStringFloor()); strWant < rt.gc.strNext {
		rt.gc.strNext = strWant
	}
	rt.allocBytes = 0
	if rt.heapLimit != 0 {
		// Not zero: chargeBytes trips when allocBytes reaches nextBytes, and
		// zero means the first byte the next run allocates is over budget.
		rt.nextBytes = gcByteFloor
	}
	return true
}

// Global returns this invocation's global object, for a host installing
// per-run values on it.
func (inv *Invocation) Global() Value {
	if inv == nil {
		return mkundef()
	}
	return inv.rt.global
}
