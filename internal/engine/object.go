package engine

import (
	"sort"

	"github.com/robomotionio/goant/internal/regexpjs"
)

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
	// arrLenNonWritable marks an array whose "length" property is non-writable
	// (Object.defineProperty(a,'length',{writable:false}) or Object.freeze). The
	// length may not change and the array may not grow past it.
	arrLenNonWritable bool
	// immutableProto marks an object whose [[Prototype]] cannot be changed
	// (%Object.prototype%). A SetPrototypeOf to a different value is rejected.
	immutableProto bool
	// usedAsProto marks an object that a cached property lookup walked through
	// as a prototype. Changing such an object's layout or [[Prototype]] can
	// change what those lookups would find, so it retires the caches; an object
	// no cache depends on carries no flag and pays nothing. Set by
	// icProtoHolder, never cleared — a prototype stays one.
	usedAsProto bool
}

// extraSlot is one internal slot entry (ant ant_extra_slot_t).
type extraSlot struct {
	slot  internalSlot
	value Value
}

// object is a JavaScript object (ant struct ant_object).
type object struct {
	// self is this object's own pool handle. Go code all over the engine holds
	// objects as *object — and the collector, reaching one that way, has no
	// other means of naming the cell it must keep. See collect.go.
	self Handle

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

	// priv holds the object's private class elements (fields, methods,
	// accessors). They live outside the shape/property system so they never
	// appear in reflection (getOwnPropertyNames, hasOwnProperty, for-in, …) and
	// cannot collide with an ordinary "#x" string property.
	priv []privElem

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

	// proxy is set for Proxy objects (their target + trap handler).
	proxy *proxyState

	// abuf is the backing byte store for an ArrayBuffer object; nil once the
	// buffer is detached. For a resizable buffer, len(abuf) is the current byte
	// length and cap(abuf) is abMax, so resize() only re-slices (the storage
	// never moves and existing views stay valid).
	abuf []byte
	// abMax is the maximum byte length; abResizable marks a resizable buffer.
	// For a non-resizable buffer abMax equals its fixed byte length.
	abMax       int
	abResizable bool
	// abObj brands an object as an ArrayBuffer (the [[ArrayBufferData]] slot),
	// staying true after detach so the prototype getters can require the slot on
	// their receiver while still distinguishing a detached buffer from a non-buffer.
	abObj bool
	// argMap is the [[ParameterMap]] of a mapped arguments object: the indices
	// that alias their function's formal parameters (see arguments.go).
	argMap *argumentsMap

	// ta is set for TypedArray views (element kind + window into an ArrayBuffer);
	// dv marks a DataView over a buffer.
	ta *typedArray
	dv *dataView

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

// roots lists every Value a queued job built from this reaction holds, so the
// job can be rooted while it waits. See job in builtin_promise.go.
func (r promiseReaction) roots(settled Value) []Value {
	return []Value{r.onFulfilled, r.onRejected, r.result, r.capResolve, r.capReject, settled}
}

type promiseReaction struct {
	onFulfilled Value
	onRejected  Value
	result      Value // the derived promise to settle
	// capResolve/capReject settle the derived promise via a species constructor's
	// capability functions (set only when the derived promise is not an ordinary
	// native promise); otherwise result is settled directly.
	capResolve Value
	capReject  Value
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
	obj.self = h
	obj.proto = proto
	obj.shape = rt.newShape()
	obj.typeTag = TObj
	obj.flags.extensible = true
	return mkval(TObj, uint64(h))
}

// newArray allocates a fast array with the given length capacity hint.
func (rt *Runtime) newArray() Value {
	h, obj := rt.objects.alloc()
	obj.self = h
	obj.proto = rt.arrayProto
	obj.shape = rt.newShape()
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

// noteLayoutChange retires the inline caches that a change to this object's
// layout could invalidate.
//
// It only does anything for an object some cached lookup walked through as a
// prototype: adding, removing or redefining one of its properties can change
// what that lookup finds, and unlike the receiver's own shape that is not
// something the cache re-checks. Reassigning an existing property's *value*
// needs nothing — the cache holds the holder and reads the slot live.
func (o *object) noteLayoutChange() {
	if o.flags.usedAsProto {
		icEpochBump()
	}
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

// defSpeciesGetter installs a `get [Symbol.species]() { return this }` accessor
// on a constructor (the default @@species that derived-object creation uses).
func (rt *Runtime) defSpeciesGetter(ctor Value) {
	if rt.symSpecies == 0 {
		return
	}
	getter := rt.newNativeFunc("get [Symbol.species]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return this, nil
	})
	if o := rt.objPtr(ctor); o != nil {
		o.defineAccessorSymbol(rt.symSpecies.handle(), getter, mkundef(), true, false, attrConfigurable)
	}
}

// setStringTag installs a non-writable Symbol.toStringTag on obj (for the
// Object.prototype.toString brand of namespace/collection builtins).
func (rt *Runtime) setStringTag(obj Value, name string) {
	if rt.symToStringTag == 0 {
		return
	}
	if o := rt.objPtr(obj); o != nil {
		o.defineOwnSymbol(rt.symToStringTag.handle(), rt.newString(name), attrConfigurable)
	}
}

// hasInProtoChain reports whether proto appears on v's prototype chain.
func (rt *Runtime) hasInProtoChain(v, proto Value) bool {
	o := rt.objPtr(v)
	if o == nil {
		return false
	}
	cur := o.proto
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		if cur == proto {
			return true
		}
		co := rt.objPtr(cur)
		if co == nil {
			return false
		}
		cur = co.proto
	}
	return false
}

// ordinarySetProto implements OrdinarySetPrototypeOf(O, V) for a non-proxy
// object, returning false (a rejected change) when O is non-extensible or has an
// immutable [[Prototype]], or when V would introduce a prototype cycle. A no-op
// (V equals the current prototype) always succeeds.
func (rt *Runtime) ordinarySetProto(o *object, v Value) bool {
	if rt.sameValue(o.proto, v) {
		return true
	}
	if o.immutableProtoOf() || !o.flags.extensible {
		return false
	}
	// Cycle check: walk V's chain; a non-ordinary [[GetPrototypeOf]] (Proxy) stops
	// the walk (its identity can't be reasoned about statically).
	for p := v; !p.IsNull(); {
		po := rt.objPtr(p)
		if po == nil || po.proxy != nil {
			break
		}
		if po == o {
			return false
		}
		p = po.proto
	}
	o.proto = v
	// Re-pointing an object that a cached lookup walked through changes what
	// the rest of that walk would find. (A change to the *receiver's* own
	// prototype needs nothing: the cache compares it on every hit.)
	o.noteLayoutChange()
	return true
}

func (o *object) immutableProtoOf() bool { return o.flags.immutableProto }

// defineOwnSymbol installs a symbol-keyed own property.
func (o *object) defineOwnSymbol(sym uint32, v Value, attrs uint8) bool {
	slot, ok := addSymbolTr(&o.shape, sym, attrs)
	if !ok {
		return false
	}
	// Converting an accessor slot to a data property clears the accessor markers,
	// exactly as the string-keyed defineOwn does — otherwise the slot keeps
	// reporting (and invoking) a getter that the redefinition removed.
	if p := o.shape.propAt(slot); p.isAccessor {
		o.ensureUniqueShape()
		p = o.shape.propAt(slot)
		p.isAccessor, p.hasGetter, p.hasSetter = false, false, false
		p.getter, p.setter = mkundef(), mkundef()
	}
	o.slotSet(slot, v)
	o.noteLayoutChange()
	return true
}

// defineAccessorSymbol installs a symbol-keyed accessor property.
func (o *object) defineAccessorSymbol(sym uint32, getter, setter Value, hasGet, hasSet bool, attrs uint8) bool {
	slot, ok := addSymbolTr(&o.shape, sym, attrs)
	if !ok {
		return false
	}
	o.ensureUniqueShape()
	p := o.shape.propAt(slot)
	p.isAccessor = true
	p.hasGetter, p.hasSetter = hasGet, hasSet
	p.getter, p.setter = getter, setter
	o.slotSet(slot, mkundef())
	o.noteLayoutChange()
	return true
}

// defineOwn installs an own data property with explicit attributes, creating or
// overwriting the slot (ant js_define_own_prop, data-property core).
func (o *object) defineOwn(key string, v Value, attrs uint8) bool {
	slot, ok := addInternedTr(&o.shape, key, attrs)
	if !ok {
		return false
	}
	// Converting an accessor slot to a data property: clear the accessor markers
	// (privatizing the shape first, since they are stamped per-object). Keyed on
	// isAccessor so an accessor with an undefined get/set is also cleared.
	if p := o.shape.propAt(slot); p.isAccessor {
		o.ensureUniqueShape()
		p = o.shape.propAt(slot)
		p.isAccessor, p.hasGetter, p.hasSetter = false, false, false
		p.getter, p.setter = mkundef(), mkundef()
	}
	o.slotSet(slot, v)
	o.noteLayoutChange()
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
	return o.deleteSlot(o.shape.lookupInterned(key))
}

// deleteOwnSymbol removes a symbol-keyed own property.
func (o *object) deleteOwnSymbol(off uint32) bool {
	return o.deleteSlot(o.shape.lookupSymbol(off))
}

func (o *object) deleteSlot(slot int32) bool {
	if slot < 0 {
		return true // deleting an absent property succeeds
	}
	if o.shape.attrsAt(uint32(slot))&attrConfigurable == 0 {
		return false
	}
	o.ensureUniqueShape()
	// Order-preserving removal shifts every slot after the removed one down by
	// one; capture their stored values before mutating the shape (getInobjLimit
	// is fixed per shape, so the slot→storage mapping is stable), then restore.
	n := o.shape.count()
	tail := make([]Value, 0, n-int(slot)-1)
	for i := int(slot) + 1; i < n; i++ {
		tail = append(tail, o.slotGet(uint32(i)))
	}
	if !o.shape.removeSlot(uint32(slot)) {
		return false
	}
	for i, v := range tail {
		o.slotSet(uint32(int(slot)+i), v)
	}
	o.noteLayoutChange()
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
	return orderOwnKeys(keys)
}

// ownKeysEnumerable returns own enumerable string keys in spec order.
func (o *object) ownKeysEnumerable() []string {
	keys := make([]string, 0, o.shape.count())
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		if !p.key.sym && p.attrs&attrEnumerable != 0 {
			keys = append(keys, p.key.str)
		}
	}
	return orderOwnKeys(keys)
}

// ownKeysForIn returns own string keys in spec order together with a map of their
// enumerability. for-in must add every own key to its "seen" set (so a shadowing
// non-enumerable own property hides an inherited enumerable one) while only
// yielding the enumerable ones.
func (o *object) ownKeysForIn() ([]string, map[string]bool) {
	enum := make(map[string]bool, o.shape.count())
	for i := 0; i < o.shape.count(); i++ {
		p := &o.shape.props[i]
		if !p.key.sym {
			enum[p.key.str] = p.attrs&attrEnumerable != 0
		}
	}
	return o.ownKeys(), enum
}

// orderOwnKeys reorders own string keys into ES OwnPropertyKeys order: canonical
// array-index keys first (ascending numeric), then the rest in insertion order.
func orderOwnKeys(keys []string) []string {
	// A record has no array-index keys at all, and then the order is already
	// right and the answer is the input unchanged. Establish that before
	// building anything: partitioning first and discarding the result second
	// copied every key of every object for nothing, which on a message made of
	// uniform records is most of what serializing it allocated.
	hasIdx := false
	for _, k := range keys {
		if _, ok := canonicalIndex(k); ok {
			hasIdx = true
			break
		}
	}
	if !hasIdx {
		return keys
	}

	var idxKeys []string
	var strKeys []string
	for _, k := range keys {
		if _, ok := canonicalIndex(k); ok {
			idxKeys = append(idxKeys, k)
		} else {
			strKeys = append(strKeys, k)
		}
	}
	sort.Slice(idxKeys, func(i, j int) bool {
		a, _ := canonicalIndex(idxKeys[i])
		b, _ := canonicalIndex(idxKeys[j])
		return a < b
	})
	return append(idxKeys, strKeys...)
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

// hasProp implements ordinary [[HasProperty]] (own + inherited). It walks the
// prototype chain itself so a Proxy anywhere in the chain dispatches its `has`
// trap, and so integer-indexed elements (which live in the backing store, not
// the shape) are found.
func (rt *Runtime) hasProp(obj Value, key string) bool {
	idx, isIdx := canonicalIndex(key)
	cur := obj
	// Primitive receiver: objPtr is nil, so an in-range string index counts as an
	// own property and the prototype walk starts at the wrapper prototype (so e.g.
	// inherited Boolean.prototype/Number.prototype properties are observable).
	if !obj.IsObjectType() && obj.Type() != TTypedArray && !obj.IsNullish() {
		if isIdx && obj.Type() == TStr && int(idx) < utf16Len(rt.strBytes(obj)) {
			return true
		}
		cur = rt.primitiveProto(obj)
	}
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		if o.proxy != nil {
			has, _ := rt.proxyHas(o.proxy, rt.internString(key))
			return has
		}
		if isIdx && rt.hasOwnIndex(cur, o, idx) {
			return true
		}
		if s := o.shape.lookupInterned(key); s >= 0 {
			return true
		}
		// An Array's or String exotic object's "length" is an own property but
		// not a shape slot.
		if key == "length" && (cur.Type() == TArr || o.boxed.Type() == TStr) {
			return true
		}
		cur = o.proto
	}
	return false
}

// hasPropE is hasProp that propagates a Proxy trap's abrupt completion (the
// [[HasProperty]] invariant TypeErrors), used where the spec's HasProperty must
// be observable — e.g. a `with` statement's binding resolution.
func (rt *Runtime) hasPropE(obj Value, key string) (bool, *ThrowError) {
	idx, isIdx := canonicalIndex(key)
	cur := obj
	if !obj.IsObjectType() && obj.Type() != TTypedArray && !obj.IsNullish() {
		if isIdx && obj.Type() == TStr && int(idx) < utf16Len(rt.strBytes(obj)) {
			return true, nil
		}
		cur = rt.primitiveProto(obj)
	}
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		if o.proxy != nil {
			return rt.proxyHas(o.proxy, rt.internString(key))
		}
		if isIdx && rt.hasOwnIndex(cur, o, idx) {
			return true, nil
		}
		if s := o.shape.lookupInterned(key); s >= 0 {
			return true, nil
		}
		// An Array's or String exotic object's "length" is an own property but
		// not a shape slot.
		if key == "length" && (cur.Type() == TArr || o.boxed.Type() == TStr) {
			return true, nil
		}
		cur = o.proto
	}
	return false, nil
}

// hasOwnPropertyOf implements HasOwnProperty(O, key): [[GetOwnProperty]] on the
// object only (no prototype walk), routing through a Proxy's trap.
func (rt *Runtime) hasOwnPropertyOf(obj Value, key string) (bool, *ThrowError) {
	o := rt.objPtr(obj)
	if o == nil {
		return false, nil
	}
	if o.proxy != nil {
		d, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, rt.internString(key))
		if e != nil {
			return false, e
		}
		return !d.IsUndefined(), nil
	}
	if idx, ok := canonicalIndex(key); ok && rt.hasOwnIndex(obj, o, idx) {
		return true, nil
	}
	return o.hasOwn(key), nil
}

// hasOwnIndex reports whether obj owns integer index idx in its element backing
// store (array elements, typed-array slots, or string code units). It keys off
// the object's own kind (o.typeTag/o.ta/o.boxed), not the Value's tag: a stored
// prototype handle can lose its TArr tag, so switching on obj.Type() would miss
// an inherited array element during a prototype walk.
func (rt *Runtime) hasOwnIndex(obj Value, o *object, idx uint32) bool {
	if o == nil {
		return obj.Type() == TStr && int(idx) < utf16Len(rt.strBytes(obj))
	}
	switch {
	case o.ta != nil:
		_, ok := rt.taGet(o, int(idx))
		return ok
	case o.typeTag == TArr:
		return idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty()
	case o.boxed.Type() == TStr:
		// String exotic object (new String(...)): indices below the wrapped
		// string's length are own data properties.
		return int(idx) < utf16Len(rt.strBytes(o.boxed))
	}
	return false
}

// ownIndexElement reads obj's own integer-indexed element from its backing store
// (array/typed-array/string-wrapper), returning ok=false when idx is a hole or
// out of range. Like hasOwnIndex it keys off the object's own kind so a prototype
// walk observes inherited array elements even through an untagged proto handle.
func (rt *Runtime) ownIndexElement(o *object, obj Value, idx uint32) (Value, bool) {
	if o == nil {
		if obj.Type() == TStr {
			b := rt.strBytes(obj)
			if int(idx) < utf16Len(b) {
				return rt.charAt(b, int(idx)), true
			}
		}
		return mkundef(), false
	}
	switch {
	case o.ta != nil:
		return rt.taGet(o, int(idx))
	case o.typeTag == TArr:
		if idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
			return o.arr[idx], true
		}
	case o.boxed.Type() == TStr:
		b := rt.strBytes(o.boxed)
		if int(idx) < utf16Len(b) {
			return rt.charAt(b, int(idx)), true
		}
	}
	return mkundef(), false
}

// isAccessorSlot reports whether a shape slot is an accessor property.
func (o *object) isAccessorSlot(slot uint32) bool {
	p := o.shape.propAt(slot)
	return p != nil && p.isAccessor
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
		// A missing get/set is undefined, not the zero-value Value (which reads as
		// the number 0).
		d.getter, d.setter = mkundef(), mkundef()
		if p.hasGetter {
			d.getter = p.getter
		}
		if p.hasSetter {
			d.setter = p.setter
		}
	} else {
		d.value = o.slotGet(uint32(slot))
		// A mapped arguments index reports the CURRENT value of the parameter it
		// aliases, not the one stored when the object was made.
		if i := o.argMap.index(key); i >= 0 {
			d.value = o.argMap.get(i)
		}
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

// defineMethodComputed installs a class/object-literal member whose name was
// computed at runtime. flags: 0=data method (non-enumerable, writable,
// configurable), 1=getter, 2=setter (merged with an existing accessor). The key
// may be a symbol or a string (numeric keys become array/index properties).
// setInferredNameFromKey implements SetFunctionName(fn, key) for a computed
// property key: a symbol key yields "[description]" (or "" when it has none),
// any other key yields its property-key string. Used by OpSetNameComp.
func (rt *Runtime) setInferredNameFromKey(fn, key Value) {
	o := rt.objPtr(fn)
	if o == nil || !o.flags.isCallable {
		return
	}
	k := rt.toPropertyKeyValue(key)
	var name string
	if k.IsSymbol() {
		if desc := rt.symbolDesc(k); !desc.IsUndefined() {
			name = "[" + rt.strGo(desc) + "]"
		}
	} else if s, e := rt.toStringValue(k); e == nil {
		name = rt.strGo(s)
	}
	o.defineOwn("name", rt.newString(name), attrConfigurable)
}

func (rt *Runtime) defineMethodComputed(target, key, accFn Value, flags byte) *ThrowError {
	o := rt.objPtr(target)
	if o == nil {
		return nil
	}
	enumerable := flags&4 != 0 // bit 4: object-literal accessor (enumerable)
	flags &= 3
	accAttrs := uint8(attrConfigurable)
	if enumerable {
		accAttrs |= attrEnumerable
	}
	// ToPropertyKey(key) is observable and may throw (a computed key whose value
	// cannot be converted to a property key).
	k, e := rt.toPropertyKey(key)
	if e != nil {
		return e
	}
	if k.IsSymbol() {
		sym := k.handle()
		if flags == 3 { // enumerable own data property (CreateDataProperty)
			o.defineOwnSymbol(sym, accFn, attrWritable|attrEnumerable|attrConfigurable)
			return nil
		}
		if flags == 0 {
			o.defineOwnSymbol(sym, accFn, attrWritable|attrConfigurable)
			return nil
		}
		g, s := mkundef(), mkundef()
		hg, hs := false, false
		if d := o.ownDescriptorSym(sym); d.exists && d.isAccessor {
			g, s = d.getter, d.setter
			hg, hs = !d.getter.IsUndefined(), !d.setter.IsUndefined()
		}
		if flags == 1 {
			g, hg = accFn, true
		} else {
			s, hs = accFn, true
		}
		o.defineAccessorSymbol(sym, g, s, hg, hs, accAttrs)
		return nil
	}
	name, e := rt.propKeyString(k)
	if e != nil {
		return e
	}
	// DefinePropertyOrThrow / CreateDataPropertyOrThrow: no define — method,
	// accessor, or an enumerable data field (flags==3) — can redefine a
	// non-configurable own property, so a static [ "prototype" ] on the constructor
	// throws. (A same-named configurable property is legitimately replaced.)
	if d := o.ownDescriptor(name); d.exists && !d.configable {
		return rt.typeError("Cannot redefine property: " + name)
	}
	if flags == 3 { // enumerable own data property (CreateDataProperty), bypassing
		// any inherited setter such as Object.prototype's __proto__.
		o.defineOwn(name, accFn, attrWritable|attrEnumerable|attrConfigurable)
		return nil
	}
	if flags == 0 {
		// DefineMethod uses [[DefineOwnProperty]], not [[Set]]: a computed method
		// named "length"/"name" must replace the constructor's own non-writable
		// default (a Set would silently fail), and no inherited setter runs.
		o.defineOwn(name, accFn, attrWritable|attrConfigurable)
		return nil
	}
	g, s := mkundef(), mkundef()
	hg, hs := false, false
	if d := o.ownDescriptor(name); d.exists && d.isAccessor {
		g, s = d.getter, d.setter
		hg, hs = !d.getter.IsUndefined(), !d.setter.IsUndefined()
	}
	if flags == 1 {
		g, hg = accFn, true
	} else {
		s, hs = accFn, true
	}
	o.defineAccessor(name, g, s, hg, hs, accAttrs)
	return nil
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
		// A missing get/set is undefined, not the zero-value Value (which reads as
		// the number 0).
		d.getter, d.setter = mkundef(), mkundef()
		if p.hasGetter {
			d.getter = p.getter
		}
		if p.hasSetter {
			d.setter = p.setter
		}
	} else {
		d.value = o.slotGet(uint32(slot))
	}
	return d
}

// setAttrsOwn updates just the attribute bits of an existing own property.
func (o *object) setAttrsOwn(key string, attrs uint8) {
	o.ensureUniqueShape()
	o.shape.setAttrs(strKey(key), attrs)
	o.noteLayoutChange()
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
			// A mapped index writes through to the parameter it aliases.
			if i := ro.argMap.index(key); i >= 0 {
				ro.argMap.set(i, v)
			}
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
	p.isAccessor = true
	p.hasGetter, p.hasSetter = hasGet, hasSet
	p.getter, p.setter = getter, setter
	o.slotSet(slot, mkundef())
	o.noteLayoutChange()
	return true
}
