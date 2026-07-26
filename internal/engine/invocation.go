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
	ended        bool
}

// BeginInvocation starts a run with a fresh global object.
func (rt *Runtime) BeginInvocation() *Invocation {
	inv := &Invocation{
		rt:           rt,
		prevGlobal:   rt.global,
		prevLex:      rt.globalLex,
		prevWatermrk: rt.invWatermark,
	}

	// Arm shared-state detection before allocating anything for this run, so the
	// fresh global itself counts as the invocation's own. Taking the watermark
	// afterwards would make every write to globalThis look like a write to
	// shared state.
	rt.beginDirtyTracking()

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
	inv.rt.invWatermark = inv.prevWatermrk
}

// Global returns this invocation's global object, for a host installing
// per-run values on it.
func (inv *Invocation) Global() Value {
	if inv == nil {
		return mkundef()
	}
	return inv.rt.global
}
