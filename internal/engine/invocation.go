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

	inv.End()

	rt.objMemo = [objMemoSize]objMemoEntry{}
	rt.objects.truncate(inv.marks.objects)
	rt.strings.truncate(inv.marks.strings)
	rt.symbols.truncate(inv.marks.symbols)
	rt.closures.truncate(inv.marks.closures)
	rt.bigints.truncate(inv.marks.bigints)
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
