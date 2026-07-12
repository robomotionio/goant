package engine

import "goant/internal/regexpjs"

// Port of ant include/object.h + the property protocol in src/ant.c. An object
// stores its named properties in shape-assigned slots: the first inobjMaxSlots
// live inline (inobj), the rest in an overflow slice. The shape maps property
// keys to slot indices (see shapes.go).
//
// DIVERGENCE FROM ant: the C union (array / closure / boxed) and the low-bit-
// tagged sidecar pointer become explicit Go fields; internal slots are a small
// slice rather than a realloc'd sidecar array.

// objFlags mirrors ant_object_flags_t (include/object.h).
type objFlags struct {
	extensible        bool
	frozen            bool
	sealed            bool
	isExotic          bool
	isConstructor     bool
	isCallable        bool
	fastArray         bool
	mayHaveHoles      bool
	mayHaveDenseElems bool
	gcPermanent       bool
}

// extraSlot is one internal slot entry (ant ant_extra_slot_t).
type extraSlot struct {
	slot  internalSlot
	value Value
}

// object is a JavaScript object (ant struct ant_object).
type object struct {
	proto Value
	shape *shape

	inobj    [inobjMaxSlots]Value
	overflow []Value

	// T_ARR fast-array backing (ant u.array).
	arr    []Value
	arrLen uint32

	// T_FUNC closure (ant u.func.closure) — Phase 3.
	closure Handle

	// boxed primitive / misc data cell (ant u.data.value).
	boxed Value

	// internal slots (ant extra_slots sidecar).
	extra []extraSlot

	// native is set for built-in functions implemented in Go (ant cfunc).
	native nativeFunc

	// regex is set for RegExp objects (the compiled pattern).
	regex *regexpjs.Regexp

	// coll is set for Map/Set objects (their entries).
	coll *collection

	// promise is set for Promise objects (their settlement state).
	promise *promiseState

	// gen is set for generator objects (their suspended coroutine).
	gen *genState

	typeTag Type
	flags   objFlags
}

// promiseState is a Promise's settlement state (ant ant_promise_state_t).
type promiseState struct {
	state    int // 0 pending, 1 fulfilled, 2 rejected
	value    Value
	handlers []promiseReaction
	handled  bool
}

type promiseReaction struct {
	onFulfilled Value
	onRejected  Value
	result      Value // the derived promise to settle
}

// collection backs Map and Set (insertion-ordered entries + canonical-key
// index for SameValueZero lookup).
type collection struct {
	keys  []Value
	vals  []Value // parallel to keys; unused (undefined) for Set
	index map[string]int
	isSet bool
	weak  bool // WeakMap/WeakSet: object-only keys, no iteration/size/clear
}

// nativeFunc is a built-in function implemented in Go (ant ant_cfunc_t).
type nativeFunc func(rt *Runtime, this Value, args []Value) (Value, *ThrowError)

// ---- allocation ----

// newObject allocates a plain object with the given prototype (ant js_mkobj).
func (rt *Runtime) newObject(proto Value) Value {
	h, obj := rt.objects.alloc()
	obj.proto = proto
	obj.shape = newShape()
	obj.typeTag = TObj
	obj.flags.extensible = true
	return mkval(TObj, uint64(h))
}

// newArray allocates a fast array with the given length capacity hint.
func (rt *Runtime) newArray() Value {
	h, obj := rt.objects.alloc()
	obj.proto = rt.arrayProto
	obj.shape = newShape()
	obj.typeTag = TArr
	obj.flags.extensible = true
	obj.flags.fastArray = true
	return mkval(TArr, uint64(h))
}

// objPtr resolves an object-family Value to its backing *object (nil if not an
// object or a dangling handle).
func (rt *Runtime) objPtr(v Value) *object {
	if !v.IsObjectType() && v.Type() != TTypedArray {
		return nil
	}
	return rt.objects.get(Handle(v.handle()))
}

// ---- slot storage ----

func (o *object) inobjLimit() int { return int(o.shape.getInobjLimit()) }

func (o *object) slotGet(slot uint32) Value {
	limit := o.inobjLimit()
	if int(slot) < limit {
		return o.inobj[slot]
	}
	idx := int(slot) - limit
	if idx < len(o.overflow) {
		return o.overflow[idx]
	}
	return mkundef()
}

func (o *object) slotSet(slot uint32, v Value) {
	limit := o.inobjLimit()
	if int(slot) < limit {
		o.inobj[slot] = v
		return
	}
	idx := int(slot) - limit
	for len(o.overflow) <= idx {
		o.overflow = append(o.overflow, mkundef())
	}
	o.overflow[idx] = v
}

// ensureUniqueShape clones the object's shape if it is shared, so it can be
// mutated in place (ant js_obj_ensure_unique_shape). Returns the private shape.
func (o *object) ensureUniqueShape() {
	if o.shape.isInTree() {
		o.shape.release()
		o.shape = o.shape.clone()
	}
}

// ---- internal slots ----

func (o *object) getSlot(slot internalSlot) Value {
	for i := range o.extra {
		if o.extra[i].slot == slot {
			return o.extra[i].value
		}
	}
	return mkundef()
}

func (o *object) setSlot(slot internalSlot, v Value) {
	for i := range o.extra {
		if o.extra[i].slot == slot {
			o.extra[i].value = v
			return
		}
	}
	o.extra = append(o.extra, extraSlot{slot, v})
}

func (o *object) brandID() int {
	b := o.getSlot(slotBrand)
	if b.Type() == TNum {
		return int(b.Number())
	}
	return brandNone
}

// ---- own-property protocol (data properties, interned string keys) ----

// getOwn returns the own data-property value for key (undefined,false if
// absent). Accessor resolution happens in the property-get path (Phase 3+).
func (o *object) getOwn(key string) (Value, bool) {
	slot := o.shape.lookupInterned(key)
	if slot < 0 {
		return mkundef(), false
	}
	return o.slotGet(uint32(slot)), true
}

// getOwnSymbol returns the own value keyed by a symbol handle.
func (o *object) getOwnSymbol(sym uint32) (Value, bool) {
	slot := o.shape.lookupSymbol(sym)
	if slot < 0 {
		return mkundef(), false
	}
	return o.slotGet(uint32(slot)), true
}

// hasOwn reports whether key is an own property.
func (o *object) hasOwn(key string) bool { return o.shape.lookupInterned(key) >= 0 }

// defineOwnSymbol installs a symbol-keyed own property.
func (o *object) defineOwnSymbol(sym uint32, v Value, attrs uint8) bool {
	slot, ok := addSymbolTr(&o.shape, sym, attrs)
	if !ok {
		return false
	}
	o.slotSet(slot, v)
	return true
}

// defineOwn installs an own data property with explicit attributes, creating or
// overwriting the slot (ant js_define_own_prop, data-property core).
func (o *object) defineOwn(key string, v Value, attrs uint8) bool {
	slot, ok := addInternedTr(&o.shape, key, attrs)
	if !ok {
		return false
	}
	o.slotSet(slot, v)
	return true
}

// setOwn performs an ordinary [[Set]] of an own data property: updates an
// existing writable slot, or (if extensible) adds a new default-attribute
// property. Returns false if the write is rejected (non-writable / non-
// extensible). Accessor/proto-chain semantics are layered on in Phase 3.
func (o *object) setOwn(key string, v Value) bool {
	slot := o.shape.lookupInterned(key)
	if slot >= 0 {
		if o.shape.attrsAt(uint32(slot))&attrWritable == 0 {
			return false
		}
		o.slotSet(uint32(slot), v)
		return true
	}
	if !o.flags.extensible {
		return false
	}
	return o.defineOwn(key, v, attrDefault)
}

// deleteOwn removes an own property (ant js_delete_prop core). Uses shape
// swap-with-last removal, mirroring the moved slot's stored value.
func (o *object) deleteOwn(key string) bool {
	slot := o.shape.lookupInterned(key)
	if slot < 0 {
		return true // deleting an absent property succeeds
	}
	if o.shape.attrsAt(uint32(slot))&attrConfigurable == 0 {
		return false
	}
	o.ensureUniqueShape()
	swappedFrom, ok := o.shape.removeSlot(uint32(slot))
	if !ok {
		return false
	}
	// Mirror the shape's swap-with-last on the object's stored values.
	if swappedFrom != uint32(slot) {
		o.slotSet(uint32(slot), o.slotGet(swappedFrom))
	}
	return true
}

// ownKeys returns own string-keyed property names in insertion (slot) order.
// Integer-index ordering (spec array-index-first) is layered on in Phase 4.
func (o *object) ownKeys() []string {
	keys := make([]string, 0, o.shape.count())
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		if !p.key.sym {
			keys = append(keys, p.key.str)
		}
	}
	return keys
}

// ownKeysEnumerable returns own enumerable string keys in slot order.
func (o *object) ownKeysEnumerable() []string {
	keys := make([]string, 0, o.shape.count())
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		if !p.key.sym && p.attrs&attrEnumerable != 0 {
			keys = append(keys, p.key.str)
		}
	}
	return keys
}

// maxProtoChainDepth guards prototype-chain walks (ant MAX_PROTO_CHAIN_DEPTH).
const maxProtoChainDepth = 256

// resolveProp walks the prototype chain from start looking for a string key,
// returning the holder object and its slot (ant lkp_proto). found=false if the
// key is absent from the whole chain.
func (rt *Runtime) resolveProp(start Value, key string) (holder *object, slot uint32, found bool) {
	cur := start
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		if s := o.shape.lookupInterned(key); s >= 0 {
			return o, uint32(s), true
		}
		cur = o.proto
	}
	return nil, 0, false
}

// getProp implements ordinary [[Get]] for data properties across the prototype
// chain. Accessor invocation (calling the getter) requires the interpreter and
// is layered on in Phase 3; for an accessor property this returns the raw slot.
func (rt *Runtime) getProp(obj Value, key string) (Value, bool) {
	holder, slot, found := rt.resolveProp(obj, key)
	if !found {
		return mkundef(), false
	}
	return holder.slotGet(slot), true
}

// hasProp implements ordinary [[HasProperty]] (own + inherited).
func (rt *Runtime) hasProp(obj Value, key string) bool {
	_, _, found := rt.resolveProp(obj, key)
	return found
}

// isAccessorSlot reports whether a shape slot is an accessor property.
func (o *object) isAccessorSlot(slot uint32) bool {
	p := o.shape.propAt(slot)
	return p != nil && (p.hasGetter || p.hasSetter)
}

// ownDescriptor returns the full property descriptor for an own key.
type ownDesc struct {
	exists     bool
	isAccessor bool
	value      Value
	getter     Value
	setter     Value
	writable   bool
	enumerable bool
	configable bool
}

func (o *object) ownDescriptor(key string) ownDesc {
	slot := o.shape.lookupInterned(key)
	if slot < 0 {
		return ownDesc{}
	}
	attrs := o.shape.attrsAt(uint32(slot))
	d := ownDesc{
		exists:     true,
		writable:   attrs&attrWritable != 0,
		enumerable: attrs&attrEnumerable != 0,
		configable: attrs&attrConfigurable != 0,
	}
	if o.isAccessorSlot(uint32(slot)) {
		p := o.shape.propAt(uint32(slot))
		d.isAccessor = true
		d.getter, d.setter = p.getter, p.setter
	} else {
		d.value = o.slotGet(uint32(slot))
	}
	return d
}

// ownSymbolKeys returns the object's own symbol keys (as symbol handles) in
// slot order.
func (o *object) ownSymbolKeys() []uint32 {
	var out []uint32
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		if p.key.sym {
			out = append(out, p.key.off)
		}
	}
	return out
}

// ownDescriptorSym is ownDescriptor for a symbol-keyed property.
func (o *object) ownDescriptorSym(off uint32) ownDesc {
	slot := o.shape.lookupSymbol(off)
	if slot < 0 {
		return ownDesc{}
	}
	attrs := o.shape.attrsAt(uint32(slot))
	d := ownDesc{
		exists:     true,
		writable:   attrs&attrWritable != 0,
		enumerable: attrs&attrEnumerable != 0,
		configable: attrs&attrConfigurable != 0,
	}
	if o.isAccessorSlot(uint32(slot)) {
		p := o.shape.propAt(uint32(slot))
		d.isAccessor = true
		d.getter, d.setter = p.getter, p.setter
	} else {
		d.value = o.slotGet(uint32(slot))
	}
	return d
}

// setAttrsOwn updates just the attribute bits of an existing own property.
func (o *object) setAttrsOwn(key string, attrs uint8) {
	o.ensureUniqueShape()
	o.shape.setAttrs(strKey(key), attrs)
}

// setProp implements ordinary [[Set]] for data properties (OrdinarySet). An
// inherited non-writable data property blocks the write; a writable/absent one
// creates or updates an own property on the receiver. Accessor setters are
// invoked in Phase 3. Returns false if the write is rejected.
func (rt *Runtime) setProp(receiver Value, key string, v Value) bool {
	ro := rt.objPtr(receiver)
	if ro == nil {
		return false
	}
	holder, slot, found := rt.resolveProp(receiver, key)
	if found {
		if holder.isAccessorSlot(slot) {
			return true // accessor setter — Phase 3
		}
		if holder.shape.attrsAt(slot)&attrWritable == 0 {
			return false // non-writable data property (own or inherited)
		}
		if holder == ro {
			holder.slotSet(slot, v)
			return true
		}
		// Inherited writable data property: create an own property on receiver.
	}
	if !ro.flags.extensible {
		return false
	}
	return ro.defineOwn(key, v, attrDefault)
}

// defineAccessor installs an accessor property (getter/setter) on the object.
func (o *object) defineAccessor(key string, getter, setter Value, hasGet, hasSet bool, attrs uint8) bool {
	slot, ok := addInternedTr(&o.shape, key, attrs)
	if !ok {
		return false
	}
	// The getter/setter identity is not part of the shape transition key, so a
	// shared shape must be privatized before stamping them, or sibling objects
	// sharing the shape would see this object's accessors.
	o.ensureUniqueShape()
	p := o.shape.propAt(slot)
	p.hasGetter, p.hasSetter = hasGet, hasSet
	p.getter, p.setter = getter, setter
	o.slotSet(slot, mkundef())
	return true
}
