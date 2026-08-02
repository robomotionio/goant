package engine

// Inline caching for the named-property opcodes (GET_FIELD, GET_FIELD2,
// PUT_FIELD). Each site carries a u16 cache index, reserved in the bytecode
// since the compiler was written; this is what finally reads it.
//
// A site remembers up to icWays shapes, and for each of them either an own slot
// or a holder somewhere up the prototype chain. Invalidation stays a shape
// identity check plus the global epoch; what the epoch does not cover directly
// is described on icWay.

// hit reports whether this way describes o's current layout.
//
// Shape identity does most of the work, but it is not sufficient on its own: a
// shape that is not yet shared (isInTree false) is mutated in place, so a delete
// can shift slots underneath a cache while the pointer stays equal. That is what
// the epoch catches.
//
// The proxy check is defensive rather than load-bearing. A Proxy keeps its
// properties on the target, so its own shape stays empty and can never equal a
// cached one (ways are only filled from a shape holding the property). It costs
// one predictable compare and removes the need to re-derive that argument every
// time this file changes.
func (w *icWay) hit(o *object) bool {
	return w.shape == o.shape && w.epoch == icEpoch() && w.slot != icMissSlot &&
		o.proxy == nil &&
		// The recorded [[Prototype]] must still be the receiver's for any entry
		// whose answer depended on the chain: one that resolved to a holder up
		// there, and one that concluded nothing up there claims the name. A
		// shape does not record the prototype, so two objects with the same
		// shape may have different ones.
		((w.holder == nil && w.toShape == nil) || w.protoVal == o.proto)
}

// read returns the cached property's current value: the receiver's slot for an
// own property, the recorded prototype's for an inherited one.
func (w *icWay) read(o *object) Value {
	if w.holder != nil {
		return w.holder.slotGet(w.slot)
	}
	return o.slotGet(w.slot)
}

// lookup finds the way describing o, or nil.
func (ic *propIC) lookup(o *object) *icWay {
	for i := 0; i < int(ic.n); i++ {
		if ic.ways[i].hit(o) {
			return &ic.ways[i]
		}
	}
	return nil
}

// dead reports that this site has seen too many shapes to be worth caching.
// Nothing revives it; the cost of being wrong is one slow-path lookup, which is
// what the site would have paid anyway.
func (ic *propIC) dead() bool { return ic.misses >= icMissLimit }

// known reports that some way already describes o's shape, hit or miss, so
// there is nothing for a fill to learn. This is what keeps a site from
// re-probing a shape whose answer it has already recorded.
func (ic *propIC) known(o *object) bool {
	for i := 0; i < int(ic.n); i++ {
		if ic.ways[i].shape == o.shape && ic.ways[i].epoch == icEpoch() {
			return true
		}
	}
	return false
}

// way returns the entry to record o's shape in: a stale one if there is any,
// otherwise a fresh one, otherwise nil once the site is full and has been
// pushed past icMissLimit.
//
// Reusing a stale way first is what keeps a site working across an epoch bump
// (a collection retires every way) without charging it a miss: invalidation is
// global and says nothing about this site.
func (ic *propIC) way(o *object) *icWay {
	for i := 0; i < int(ic.n); i++ {
		if ic.ways[i].shape == o.shape || ic.ways[i].epoch != icEpoch() {
			return &ic.ways[i]
		}
	}
	if int(ic.n) < icWays {
		w := &ic.ways[ic.n]
		ic.n++
		return w
	}
	ic.misses++
	if ic.dead() {
		ic.retire()
		return nil
	}
	// Full and still useful: replace the oldest.
	return &ic.ways[0]
}

// retire empties a site that has seen too many shapes to be worth caching.
//
// Setting n to zero is what stops lookup consulting it. Clearing the ways as
// well is what lets a reader that does not carry n — compiled code, which has
// four registers and no room for a bound — decide a way is empty by looking at
// it. A retired way describes a shape that is still real, so consulting one
// would not give a wrong answer; it would give an answer this site has decided
// not to spend probe time on, and the two readers disagreeing about that is
// worth more to avoid than the fills it costs.
func (ic *propIC) retire() {
	ic.ways = [icWays]icWay{}
	ic.n = 0
}

// wayIndex is way() for an entry keyed on a shape the receiver no longer has —
// the pre-store shape of a transition.
func (ic *propIC) wayIndex(o *object, key *shape) int {
	for i := 0; i < int(ic.n); i++ {
		if ic.ways[i].shape == key || ic.ways[i].epoch != icEpoch() {
			return i
		}
	}
	if int(ic.n) < icWays {
		ic.n++
		return int(ic.n) - 1
	}
	ic.misses++
	if ic.dead() {
		ic.retire()
		return -1
	}
	return 0
}

func (ic *propIC) record(o *object, slot uint32) {
	if w := ic.way(o); w != nil {
		*w = icWay{shape: o.shape, epoch: icEpoch(), slot: slot}
	}
}

// recordProto points a way at a property reached through o's prototype chain,
// held on holder at the given slot.
func (ic *propIC) recordProto(o, holder *object, slot uint32) {
	if w := ic.way(o); w != nil {
		*w = icWay{shape: o.shape, epoch: icEpoch(), slot: slot,
			holder: holder, protoVal: o.proto}
	}
}

// icReceiver returns the object to consult for an inline-cached read, or nil if
// this receiver can never use one.
func (rt *Runtime) icReceiver(v Value) *object {
	if !v.IsObjectType() {
		return nil
	}
	return rt.objPtr(v)
}

// icFillGet records name's own data slot on o, if caching it is sound.
//
// The conditions, each guarding a way getField can answer something other than
// "read this slot":
//
//   - own, non-accessor slot — the slot behind an accessor holds undefined, so
//     caching it would replace every getter call with undefined; a slot found on
//     a prototype would additionally need the chain guarded;
//   - not a canonical index — an index name reaches array and TypedArray element
//     storage, and the mapped-arguments parameter map, before the shape is
//     consulted at all;
//   - not a Proxy, TypedArray or DataView, whose [[Get]] is not a slot read.
//
// The parameter-map check is redundant given the index rule (a mapped arguments
// object only aliases index names) and kept as a cheap statement of intent.
//
// A miss simply leaves the entry alone, so a polymorphic site keeps working at
// slow-path speed rather than thrashing.
func (rt *Runtime) icFillGet(ic *propIC, o *object, name string) {
	if o == nil || ic.known(o) {
		return
	}
	// Receiver-specific disqualifiers. These are properties of this object, not
	// of its shape, so they must not be recorded as a miss — doing so would poison
	// the site for every other object that happens to share the shape. All are
	// pointer compares made before the expensive lookup, so re-checking is free.
	if o.proxy != nil || o.ta != nil || o.dv != nil || o.argMap != nil {
		return
	}
	// From here the answer follows from the shape and the (constant) name, so a
	// failure is worth remembering.
	if _, isIdx := canonicalIndex(name); isIdx {
		ic.record(o, icMissSlot)
		return
	}
	if slot := o.shape.lookupInterned(name); slot >= 0 {
		if o.isAccessorSlot(uint32(slot)) {
			ic.record(o, icMissSlot)
			return
		}
		ic.record(o, uint32(slot))
		return
	}
	if h, slot, ok := rt.icProtoHolder(o, name); ok {
		ic.recordProto(o, h, slot)
		return
	}
	ic.record(o, icMissSlot)
}

// icProtoHolder walks o's prototype chain for a cacheable inherited data slot,
// flagging every object it passes as one an inline cache now depends on.
//
// The flag is set here rather than wherever an object first becomes some
// object's prototype, because this is precisely the set of objects whose layout
// a cache has come to rely on. An object no cached lookup ever passed through
// carries no flag and pays nothing when it is mutated.
//
// Anything the fast path could not reproduce disqualifies the walk: an
// accessor, an exotic holder, a Proxy, or an array the walk would have to
// continue past — getField answers an array's exotic "length" and its element
// storage before reaching the chain below it.
func (rt *Runtime) icProtoHolder(o *object, name string) (*object, uint32, bool) {
	cur := o
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		next := rt.objPtr(cur.proto)
		if next == nil {
			return nil, 0, false
		}
		if next.proxy != nil || next.ta != nil || next.dv != nil || next.argMap != nil {
			return nil, 0, false
		}
		next.flags.usedAsProto = true
		if slot := next.shape.lookupInterned(name); slot >= 0 {
			if next.isAccessorSlot(uint32(slot)) {
				return nil, 0, false
			}
			return next, uint32(slot), true
		}
		if next.typeTag == TArr {
			return nil, 0, false
		}
		cur = next
	}
	return nil, 0, false
}

// icFillPut records name's own writable data slot on o for a store.
//
// Everything icFillGet rules out applies, plus the slot must be writable.
// Writability lives in the shape and every path that clears it bumps the epoch,
// so it is safe to resolve once here and rely on for the entry's lifetime. That
// also covers Object.freeze, which clears the writable bit. Object.seal does
// not — a sealed object's existing properties stay writable — so sealing
// deliberately does not disable the cache.
//
// The writable test is belt-and-braces: the caller only fills after a store that
// reported success, and a store to a non-writable slot reports failure, so it
// should be unreachable. It is kept because it costs one bit test and because
// that caller-side guarantee is easy to lose.
//
// A store that CREATES the property is served by icFillPutTransition instead;
// this one records the post-store shape, which a site initialising a fresh
// object never sees again.
//
// Nothing here inspects the prototype chain, and it does not need to:
// OrdinarySet only consults the chain when the receiver has no own property of
// that name. Having found an own writable data slot, an inherited setter or an
// inherited non-writable property cannot affect the store — so the own-slot
// requirement is exactly what makes ignoring the chain correct.
func (rt *Runtime) icFillPut(ic *propIC, o *object, name string) {
	if o == nil || ic.known(o) {
		return
	}
	if o.proxy != nil || o.ta != nil || o.dv != nil || o.argMap != nil {
		return
	}
	if _, isIdx := canonicalIndex(name); isIdx {
		ic.record(o, icMissSlot)
		return
	}
	slot := o.shape.lookupInterned(name)
	if slot < 0 || o.isAccessorSlot(uint32(slot)) ||
		o.shape.attrsAt(uint32(slot))&attrWritable == 0 {
		ic.record(o, icMissSlot)
		return
	}
	ic.record(o, uint32(slot))
}

// icFillPutTransition records the layout change a store made when it created
// the property, so the next object arriving at this site with the same shape
// takes it directly: set the shape, write the slot.
//
// pre is the receiver's shape before the store. The site is keyed on it, which
// is what makes the entry usable — the shape a fresh object has when the
// constructor reaches this line is the same every time.
//
// What is being cached is not just the slot but the CONCLUSION of the prototype
// walk: that nothing up the chain claims this name in a way that would change
// where the value goes. Two things keep that true. Every object the walk passed
// is flagged usedAsProto, so a later change to any of them bumps the epoch and
// retires this entry; and the receiver's own [[Prototype]] is recorded, so an
// object with the same shape but a different chain misses.
//
// The hit path still has to test extensibility, because that is a property of
// the object rather than of its shape: Object.preventExtensions leaves the
// shape alone.
func (rt *Runtime) icFillPutTransition(ic *propIC, o *object, pre *shape, name string) {
	if o == nil || pre == nil || o.shape == pre {
		return
	}
	if o.proxy != nil || o.ta != nil || o.dv != nil || o.argMap != nil || o.typeTag == TArr {
		return
	}
	if _, isIdx := canonicalIndex(name); isIdx {
		return
	}
	// The store must have added an ordinary, writable, non-accessor own slot.
	slot := o.shape.lookupInterned(name)
	if slot < 0 || o.isAccessorSlot(uint32(slot)) ||
		o.shape.attrsAt(uint32(slot))&attrDefault != attrDefault {
		return
	}
	// The resulting shape must be a shared one from the transition tree. A
	// private shape belongs to this object alone — defineOwn privatizes when it
	// converts an accessor slot to data — and handing it to a sibling would give
	// the two of them one mutable layout.
	if !o.shape.isInTree() {
		return
	}
	if !rt.icProtoChainClean(o, name) {
		return
	}
	if i := ic.wayIndex(o, pre); i >= 0 {
		ic.ways[i] = icWay{shape: pre, epoch: icEpoch(), slot: uint32(slot),
			toShape: o.shape, protoVal: o.proto}
	}
}

// icProtoChainClean reports whether nothing on o's prototype chain claims name
// in a way that would redirect a store, flagging every object it passes so a
// later change to one of them retires the caches that depend on it.
//
// An inherited writable data property is clean: OrdinarySet still creates an own
// property on the receiver. An accessor or a non-writable property is not.
func (rt *Runtime) icProtoChainClean(o *object, name string) bool {
	cur := o
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		next := rt.objPtr(cur.proto)
		if next == nil {
			return true // reached the end of the chain
		}
		if next.proxy != nil || next.ta != nil || next.dv != nil || next.argMap != nil {
			return false
		}
		next.flags.usedAsProto = true
		if slot := next.shape.lookupInterned(name); slot >= 0 {
			return !next.isAccessorSlot(uint32(slot)) &&
				next.shape.attrsAt(uint32(slot))&attrWritable != 0
		}
		if next.typeTag == TArr {
			return false
		}
		cur = next
	}
	return false
}

// frameICs returns fn's cache array, allocating it on first entry. The array
// hangs off the compiled function, not the frame, so entries survive across
// calls — a cache rebuilt per call would never pay for itself.
func frameICs(fn *svFunc) []propIC {
	if fn.ics == nil && fn.icCount > 0 {
		fn.ics = make([]propIC, fn.icCount)
	}
	return fn.ics
}
