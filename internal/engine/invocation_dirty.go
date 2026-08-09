package engine

// Detecting when a run made its Runtime unfit to reuse.
//
// An Invocation gives each run a fresh global, which isolates everything the
// run *installs*. It does not isolate what a run *modifies*: the builtins are
// shared, so `Array.prototype.polluted = 1` in one run is visible to the next.
// Under a rebuilt-per-call realm that could not happen, and it is the one thing
// the cheap approach gives up.
//
// Rather than pay to prevent it, detect it. A Runtime whose shared state a
// script touched is marked dirty and must be discarded instead of reused; a
// clean one — the overwhelmingly common case, since almost no script modifies a
// builtin — is reused at full speed. That is the same shape of bargain a
// speculative optimisation makes: fast when the guess holds, correct when it
// does not.
//
// The test is a handle comparison. Pool handles are allocated in increasing
// order, so every object that existed when the invocation began has a handle
// below the watermark taken at that moment, and everything the script allocates
// has one above it. Mutating an object below the watermark is, by definition,
// reaching into state the next run will inherit.
//
// What this covers is script-driven mutation: the paths a script's property
// writes, defines, deletes and prototype changes actually take. Engine-internal
// code that manipulates *object directly is not routed through here, which is
// sound because by the time an invocation runs the realm is already built — the
// engine is not still constructing shared objects.

// beginDirtyTracking arms the watermark for a new invocation.
func (rt *Runtime) beginDirtyTracking() {
	rt.invWatermark = rt.objects.next
	rt.invDirty = false
}

// noteSharedMutation marks the invocation dirty if obj predates it.
//
// One unsigned compare on a value the caller already has. It is deliberately
// not a method on *object: an object does not know its own handle, and adding
// one would cost four bytes on every object to serve a check that only the
// Value-carrying entry points need.
func (rt *Runtime) noteSharedMutation(obj Value) {
	if rt.invWatermark == 0 || rt.invDirty {
		return
	}
	if !obj.IsObjectType() {
		return
	}
	if h := Handle(obj.handle()); h < rt.invWatermark && !rt.bornInRun(h) {
		rt.invDirty = true
	}
}

// noteSharedMutationOf is noteSharedMutation for a caller holding the object
// rather than a Value that names it.
//
// Two callers need it and neither can use the Value form. An element write
// reaches its array as an *object, and a TypedArray write cannot go through the
// Value form at all: IsObjectType() is false for T_TYPEDARRAY — the tag is not
// in tObjectMask — so noteSharedMutation returns early for every view handed to
// it, which is how a whole family of writes went unnoticed.
func (rt *Runtime) noteSharedMutationOf(o *object) {
	if rt.invWatermark == 0 || rt.invDirty || o == nil {
		return
	}
	if o.self < rt.invWatermark && !rt.bornInRun(o.self) {
		rt.invDirty = true
	}
}

// bornInRun reports that this cell was recycled from below the watermark during
// the running invocation, so the object in it is the run's own however low its
// handle is.
//
// Off the fast path by construction: it is only reached once the handle
// comparison has already said "older than this run", which for almost every
// write it is not.
func (rt *Runtime) bornInRun(h Handle) bool {
	cl := rt.objects.cell(h)
	return cl != nil && cl.born
}

// Dirty reports whether this invocation modified state that predates it, which
// means the next run on this Runtime would inherit the change.
//
// A host pooling Runtimes must not reuse one whose last invocation reported
// true. Discarding it costs a fresh Runtime (~700 µs) on the rare run that
// monkey-patches a builtin, and nothing at all on every run that does not.
func (inv *Invocation) Dirty() bool {
	return inv != nil && inv.rt.invDirty
}
