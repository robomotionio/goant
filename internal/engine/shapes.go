package engine

import "sync/atomic"

// Port of ant src/shapes.c — the hidden-class ("shape") transition system.
// Objects sharing the same property-addition history share a shape, which maps
// property keys to fixed slot indices; this is what makes inline caches and
// monomorphic property access possible.
//
// DIVERGENCE FROM ant: shapes are ordinary Go-GC-managed structs (PLAN.md),
// never stored inside NaN-boxed Values, so we drop ant's manual free/refcount
// memory management. refCount is retained only for the "is this shape shared
// (immutable) vs private (mutable in place)" decision; unreferenced shapes are
// reclaimed by the Go GC (transition-tree pruning lands with Phase 7 GC). Keys
// are compared by value (interned strings are canonical), replacing ant's
// pointer-identity uint64 encoding.

// Property attribute bits (ant shapes.h ANT_PROP_ATTR_*).
const (
	attrWritable     = 1 << 0
	attrEnumerable   = 1 << 1
	attrConfigurable = 1 << 2
	attrDefault      = attrWritable | attrEnumerable | attrConfigurable
)

// inobjMaxSlots is ant's ANT_INOBJ_MAX_SLOTS (inline object slot count).
const inobjMaxSlots = 4

// icEpochCounter is the inline-cache generation counter (ant
// ant_ic_epoch_counter).
//
// It stays process-wide rather than per-Runtime because the shape methods that
// invalidate have no Runtime to hand, but it is atomic: Runtimes on different
// goroutines bump it independently. Sharing it is harmless — a bump from an
// unrelated Runtime only makes another's caches refill, which is a cost, not a
// wrong answer — whereas a torn read or a lost update would be neither.
var icEpochCounter atomic.Uint32

func init() { icEpochCounter.Store(1) }

func icEpoch() uint32 { return icEpochCounter.Load() }

func icEpochBump() {
	if icEpochCounter.Add(1) == 0 {
		icEpochCounter.Store(1)
	}
}

// icWay is one shape a site has seen and where the name resolved to for it.
//
// A hit needs shape identity plus a matching epoch. Shape identity alone is not
// enough because a shape not yet shared (isInTree false) is mutated in place;
// the mutations that can move or retire an existing slot — removeSlot,
// setAttrs, clearAccessor, re-adding a live key — all bump icEpoch, while
// appending a brand new key leaves lower slots where they are.
//
// A way may also serve a property found on the receiver's
// prototype chain, which is where every method lives. Such a site could not be
// cached at all before, so `p.method()` walked the chain and did a shape lookup
// at each link on every call — the dominant cost of any object-oriented
// program.
//
// It is guarded by three facts, all of which must still hold:
//
//   - the receiver's shape is unchanged, so it has grown no own property that
//     would now shadow the inherited one;
//   - the receiver's [[Prototype]] is the same object, so the walk starts where
//     it started before (shapes do not record the prototype, so two objects
//     with one shape may have different prototypes);
//   - the epoch is unchanged, which is what covers everything further along the
//     chain: an object a cached walk passed through is flagged usedAsProto, and
//     changing such an object's layout or prototype bumps the epoch.
//
// The holder is kept as a pointer and its slot read live, so reassigning a
// method (C.prototype.m = f) needs no invalidation at all.
type icWay struct {
	shape *shape
	epoch uint32
	slot  uint32

	// holder is the prototype the property was found on, nil for an own-slot
	// entry; protoVal is the receiver's [[Prototype]] the walk started from.
	holder   *object
	protoVal Value
}

// icWays is how many shapes one site remembers.
//
// One was not enough. A site in an object-oriented program routinely sees a
// small handful of shapes — Octane's Richards passes task control blocks,
// packets and the scheduler itself through the same accessors — and a
// single-entry cache does not merely miss on those, it thrashes: every access
// evicts the entry the previous one installed, so the site pays a full lookup
// AND a refill, which is worse than not caching at all.
//
// Eight, measured. Four is enough for the majority of sites and was the first
// choice, but the sites that matter in an object-oriented program are the ones
// that dispatch over a class hierarchy: DeltaBlue passes six constraint
// subclasses through the same field reads, Richards five task kinds. Those sat
// just past four and fell back to the shape lookup on nearly every access —
// Richards 194 -> 252, DeltaBlue 221 -> 270 on the widening alone. Sixteen is
// no better and sometimes worse: a hit scans the ways linearly, so width is not
// free, and past that a site is megamorphic and better served by giving up
// (see icMissLimit).
const icWays = 8

// propIC is one inline-cache site: the shapes it has seen and where the name
// resolved to for each.
//
// Without it every obj.name goes through the shape index, which for a large
// shape hashes the property name. That was measured at ~37% of interpreter CPU
// on a monomorphic read loop.
type propIC struct {
	ways   [icWays]icWay
	n      uint8 // ways filled
	misses uint8 // shapes seen beyond what fits; at icMissLimit the site is abandoned
}

// icMissLimit is how many shapes beyond its ways a site may see before it stops
// caching for good.
//
// A truly megamorphic site cannot be served by a fixed set of ways, and every
// attempt costs a probe on top of the lookup that already happened. Giving up
// makes such a site cost what it did before the cache existed.
//
// Thirty-two, not the eight that suited a four-way cache: with eight ways, a
// site that sees nine or ten shapes is not megamorphic, it is merely wider than
// the cache, and replacing the oldest way still hits most of the time. Giving up
// on those cost DeltaBlue 5%, Splay 9%.
const icMissLimit = 32

// icMissSlot marks a shape this site has already tried and cannot cache — most
// often because the property lives on the prototype, which is every method call.
// Without it a method-dispatch site pays the failed own-shape probe on each
// access on top of the normal lookup, which measured slower than no cache at all.
const icMissSlot = ^uint32(0)

// icNoSlot marks a field op that gets no cache (the per-function slot counter
// saturated). Sites past 65534 in one function fall back to the slow path.
const icNoSlot = 0xFFFF

// propKey identifies a property: an interned string, or a symbol handle.
type propKey struct {
	sym bool
	str string // canonical interned name when !sym
	off uint32 // symbol handle when sym
}

func strKey(interned string) propKey { return propKey{str: interned} }
func symKey(off uint32) propKey      { return propKey{sym: true, off: off} }

// eq compares two keys without going through the compiler's generated struct
// equality, which the profile showed being called out of line.
//
// Names are interned, so the string comparison is settled by the pointer test
// inside == and never reaches a byte compare.
func (k propKey) eq(o propKey) bool {
	if k.sym {
		return o.sym && k.off == o.off
	}
	return !o.sym && k.str == o.str
}

// childKey keys a shape transition edge (property key + attributes).
type childKey struct {
	key   propKey
	attrs uint8
}

// shapeProp is one property descriptor within a shape (ant ant_shape_prop_t).
type shapeProp struct {
	key        propKey
	attrs      uint8
	isAccessor bool // an accessor property (may still have an undefined get/set)
	hasGetter  bool
	hasSetter  bool
	getter     Value
	setter     Value
}

// shapeLinearMax is how many properties a shape holds before it builds a hash
// index. Below it, lookup scans props.
//
// props is the source of truth either way; the map is a lookup accelerator that
// most shapes never need. A JS object usually has a handful of named
// properties, and for those the map is a pessimisation twice over: the scan
// compares interned keys by pointer, where the map hashes the whole key string
// first — that hash alone was 18% of Richards — and the map itself is an
// allocation per shape, paid on every transition.
//
// Sixteen is where a scan of same-cost comparisons stops being obviously
// cheaper than one hash; the objects that exceed it (the global object, a
// dictionary built by hand) are exactly the ones that want the map.
const shapeLinearMax = 16

// shape is a hidden class (ant struct ant_shape).
type shape struct {
	refCount   uint32
	inobjLimit uint8
	props      []shapeProp
	// index is nil until props outgrows shapeLinearMax, and complete from then
	// on. Consult it only through lookup/addKey, which know that.
	index     map[propKey]uint32
	children  map[childKey]*shape
	parent    *shape
	parentKey childKey

	// trKey/trChild/trSlot memoise the transition most recently taken out of
	// this shape. Objects are overwhelmingly built the same way as the last one
	// built — parsing a JSON array of records adds the same keys in the same
	// order to every element — so the previous answer is almost always the
	// right one, and taking it skips two map lookups whose keys hash a string.
	// That pair of lookups was 45% of the time spent parsing a document.
	//
	// Sound because the transition tree only ever grows: recordChild never
	// replaces an entry, and a shape in the tree is never restructured (a
	// delete calls ensureUniqueShape first, which detaches a private clone —
	// and clone starts with an empty cache).
	trKey   childKey
	trChild *shape
	trSlot  uint32
}

func clampInobjLimit(limit uint8) uint8 {
	if limit > inobjMaxSlots {
		return inobjMaxSlots
	}
	return limit
}

// newShapeWithLimit returns this runtime's empty root shape for a given inobj
// limit, retained (ant ant_shape_new_with_inobj_limit).
//
// The roots live on the Runtime, not in a package variable. Every shape an
// object ever gets descends from one of them through the transition tree, and
// a transition MUTATES its parent's children map — so a shared root is a shared
// mutable structure, and two Runtimes allocating objects at the same time race
// on it. That is not a theoretical race: it crashes with "concurrent map read
// and map write" inside New(), before any script runs.
//
// A host running scripts in parallel gives each goroutine its own Runtime and
// is entitled to assume they are independent. This is what makes that true.
func (rt *Runtime) newShapeWithLimit(inobjLimit uint8) *shape {
	c := clampInobjLimit(inobjLimit)
	if rt.rootShapes[c] == nil {
		rt.rootShapes[c] = &shape{refCount: 1, inobjLimit: c}
	}
	rt.rootShapes[c].refCount++
	return rt.rootShapes[c]
}

func (rt *Runtime) newShape() *shape { return rt.newShapeWithLimit(inobjMaxSlots) }

func (s *shape) retain() {
	if s != nil {
		s.refCount++
	}
}

// release decrements refCount. Memory is reclaimed by the Go GC, so this only
// maintains the shared/private accounting used by isInTree.
func (s *shape) release() {
	if s != nil && s.refCount > 0 {
		s.refCount--
	}
}

func (s *shape) clone() *shape {
	c := &shape{refCount: 1, inobjLimit: clampInobjLimit(s.inobjLimit)}
	if len(s.props) > 0 {
		c.props = make([]shapeProp, len(s.props))
		copy(c.props, s.props)
		if len(c.props) > shapeLinearMax {
			c.buildIndex()
		}
	}
	return c
}

// isInTree reports whether the shape is shared/committed (must not be mutated
// in place). ant: ref_count > 1 || parent != NULL.
func (s *shape) isInTree() bool {
	return s != nil && (s.refCount > 1 || s.parent != nil)
}

func (s *shape) lookup(key propKey) int32 {
	if s == nil {
		return -1
	}
	if s.index == nil {
		for i := range s.props {
			if s.props[i].key.eq(key) {
				return int32(i)
			}
		}
		return -1
	}
	if slot, ok := s.index[key]; ok {
		return int32(slot)
	}
	return -1
}

// buildIndex populates the hash index from props. Called when a shape grows
// past shapeLinearMax, and when a clone starts out already that large.
func (s *shape) buildIndex() {
	s.index = make(map[propKey]uint32, len(s.props)+4)
	for i := range s.props {
		s.index[s.props[i].key] = uint32(i)
	}
}

func (s *shape) lookupInterned(interned string) int32 { return s.lookup(strKey(interned)) }
func (s *shape) lookupSymbol(off uint32) int32        { return s.lookup(symKey(off)) }

// addKey adds or updates a property key in place, returning its slot
// (ant shape_add_key). Updating an existing key's attrs bumps the IC epoch.
func (s *shape) addKey(key propKey, attrs uint8) (uint32, bool) {
	if s == nil {
		return 0, false
	}
	if slot := s.lookup(key); slot >= 0 {
		s.props[slot].attrs = attrs
		icEpochBump()
		return uint32(slot), true
	}
	slot := uint32(len(s.props))
	s.props = append(s.props, shapeProp{key: key, attrs: attrs})
	switch {
	case s.index != nil:
		s.index[key] = slot
	case len(s.props) > shapeLinearMax:
		s.buildIndex()
	}
	return slot, true
}

func (s *shape) findChild(ck childKey) *shape {
	if s == nil || s.children == nil {
		return nil
	}
	return s.children[ck]
}

func (s *shape) recordChild(ck childKey, child *shape) {
	if s.children == nil {
		s.children = map[childKey]*shape{}
	}
	if _, ok := s.children[ck]; ok {
		return
	}
	child.retain()
	s.children[ck] = child
	child.parent = s
	child.parentKey = ck
}

// addKeyTr adds a property key with transition-tree sharing, mutating *sp to
// point at the resulting shape and returning the new slot
// (ant ant_shape_add_interned_tr / add_symbol_tr, unified).
func addKeyTr(sp **shape, key propKey, attrs uint8) (uint32, bool) {
	s := *sp
	if s == nil {
		return 0, false
	}
	if !s.isInTree() {
		// A shape not in the tree is mutated in place, so its pointer does not
		// change when a key is appended. An inline cache keyed on that pointer
		// therefore cannot tell that the object has gained an own property —
		// and a prototype-dispatch entry is precisely a bet that it has not.
		// (splay.js sets SplayTree.prototype.root_ = null and then assigns
		// this.root_, which is that bet coming due.)
		//
		// A transition through the tree needs no bump: it produces a different
		// shape, which every entry compares against.
		icEpochBump()
		return s.addKey(key, attrs)
	}
	ck := childKey{key: key, attrs: attrs}
	// Interned keys make this comparison a pointer test in the common case, so
	// a repeat of the last transition costs that instead of hashing the key
	// twice over.
	if s.trChild != nil && s.trKey == ck {
		s.trChild.retain()
		s.release()
		*sp = s.trChild
		return s.trSlot, true
	}
	if child := s.findChild(ck); child != nil {
		if slot := child.lookup(key); slot >= 0 {
			s.trKey, s.trChild, s.trSlot = ck, child, uint32(slot)
			child.retain()
			s.release()
			*sp = child
			return uint32(slot), true
		}
	}
	shared := s.clone()
	slot, ok := shared.addKey(key, attrs)
	if !ok {
		return 0, false
	}
	s.recordChild(ck, shared)
	shared.release()
	next := shared.clone()
	s.release()
	*sp = next
	return slot, true
}

func addInternedTr(sp **shape, interned string, attrs uint8) (uint32, bool) {
	return addKeyTr(sp, strKey(interned), attrs)
}

func addSymbolTr(sp **shape, off uint32, attrs uint8) (uint32, bool) {
	return addKeyTr(sp, symKey(off), attrs)
}

func (s *shape) count() int {
	if s == nil {
		return 0
	}
	return len(s.props)
}

func (s *shape) propAt(slot uint32) *shapeProp {
	if s == nil || int(slot) >= len(s.props) {
		return nil
	}
	return &s.props[slot]
}

func (s *shape) attrsAt(slot uint32) uint8 {
	if p := s.propAt(slot); p != nil {
		return p.attrs
	}
	return attrDefault
}

func (s *shape) getInobjLimit() uint8 {
	if s == nil {
		return inobjMaxSlots
	}
	return clampInobjLimit(s.inobjLimit)
}

// setAttrs updates a key's attributes in place, bumping the IC epoch.
func (s *shape) setAttrs(key propKey, attrs uint8) bool {
	slot := s.lookup(key)
	if slot < 0 {
		return false
	}
	s.props[slot].attrs = attrs
	icEpochBump()
	return true
}

// removeSlot deletes a slot, shifting every following property down by one so
// the surviving keys keep their insertion order (ES OrdinaryDelete preserves
// relative order — swap-with-last would corrupt enumeration after a delete).
// Callers shift the object's stored values for slots > slot down the same way.
// Bumps the IC epoch.
func (s *shape) removeSlot(slot uint32) (ok bool) {
	if s == nil || int(slot) >= len(s.props) {
		return false
	}
	// Only a private shape reaches here (deleteSlot detaches one first), so the
	// memoised transition cannot already be in use — but clearing it keeps that
	// true for any future caller, since the slots it hands out are about to move.
	s.trChild = nil
	gone := s.props[slot].key
	copy(s.props[slot:], s.props[slot+1:])
	s.props = s.props[:len(s.props)-1]
	if s.index != nil {
		// Shrinking back below the threshold does not drop the index: a shape
		// that once needed it is likely to need it again, and lookup works off
		// whichever representation is present.
		delete(s.index, gone)
		for i := int(slot); i < len(s.props); i++ {
			s.index[s.props[i].key] = uint32(i)
		}
	}
	icEpochBump()
	return true
}

// clearAccessor clears getter/setter on a slot (ant ant_shape_clear_accessor_slot).
func (s *shape) clearAccessor(slot uint32) bool {
	p := s.propAt(slot)
	if p == nil {
		return false
	}
	p.isAccessor, p.hasGetter, p.hasSetter, p.getter, p.setter = false, false, false, 0, 0
	icEpochBump()
	return true
}
