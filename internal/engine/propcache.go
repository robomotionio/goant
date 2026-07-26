package engine

// Inline caching for the named-property opcodes (GET_FIELD, GET_FIELD2,
// PUT_FIELD). Each site carries a u16 cache index, reserved in the bytecode
// since the compiler was written; this is what finally reads it.
//
// The cache is deliberately the narrowest useful one: monomorphic, and only for
// a property found in the *receiver's own* shape. That keeps invalidation to a
// shape-identity check plus the global epoch, with no dependence on the
// prototype chain — so nothing here has to reason about setPrototypeOf, about a
// method being added to a prototype later, or about shadowing.

// hit reports whether this entry describes o's current layout.
//
// Shape identity does most of the work, but it is not sufficient on its own: a
// shape that is not yet shared (isInTree false) is mutated in place, so a delete
// can shift slots underneath a cache while the pointer stays equal. That is what
// the epoch catches, and it is the only guard here without which the tests fail.
//
// The proxy check is defensive rather than load-bearing. A Proxy keeps its
// properties on the target, so its own shape stays empty and can never equal a
// cached one (entries are only filled from a shape holding the property). It
// costs one predictable compare and removes the need to re-derive that argument
// every time this file changes.
func (ic *propIC) hit(o *object) bool {
	return ic.shape == o.shape && ic.epoch == icEpoch() && ic.slot != icMissSlot && o.proxy == nil
}

// dead reports that this site has seen too many shapes to be worth caching.
// Nothing revives it; the cost of being wrong is one slow-path lookup, which is
// what the site would have paid anyway.
func (ic *propIC) dead() bool { return ic.misses >= icMissLimit }

// known reports that this entry already describes o's shape, hit or miss, so
// there is nothing for a fill to learn. This is what keeps a site whose property
// is inherited from re-probing the receiver's own shape on every access.
func (ic *propIC) known(o *object) bool {
	return ic.shape == o.shape && ic.epoch == icEpoch()
}

// record points the entry at o's shape, first charging a miss if it is being
// aimed somewhere new. Re-pointing at the same shape after an epoch bump is not
// a miss — invalidation is global and says nothing about this site.
func (ic *propIC) record(o *object, slot uint32) {
	if ic.shape != nil && ic.shape != o.shape {
		ic.misses++
		if ic.dead() {
			ic.shape = nil // no live object has a nil shape, so hit() stays false
			return
		}
	}
	ic.shape, ic.epoch, ic.slot = o.shape, icEpoch(), slot
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
	slot := o.shape.lookupInterned(name)
	if slot < 0 || o.isAccessorSlot(uint32(slot)) {
		ic.record(o, icMissSlot)
		return
	}
	ic.record(o, uint32(slot))
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
// Note this only ever helps stores to a property that already exists: the fill
// runs after the store, so a store that *creates* the property records the
// post-store shape, which the site will not see again until the next object has
// already been through the slow path.
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

// frameICs returns fn's cache array, allocating it on first entry. The array
// hangs off the compiled function, not the frame, so entries survive across
// calls — a cache rebuilt per call would never pay for itself.
func frameICs(fn *svFunc) []propIC {
	if fn.ics == nil && fn.icCount > 0 {
		fn.ics = make([]propIC, fn.icCount)
	}
	return fn.ics
}
