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

// propIC is one monomorphic inline-cache entry: the receiver shape a named
// property access last saw, and the own data slot the name resolved to.
//
// Without it every obj.name goes through shape.index, a map[propKey]uint32
// whose key holds a string — so a property read costs a string hash. That was
// measured at ~37% of interpreter CPU on a monomorphic read loop.
//
// A hit needs shape identity plus a matching epoch. Shape identity alone is not
// enough because a shape not yet shared (isInTree false) is mutated in place;
// the mutations that can move or retire an existing slot — removeSlot,
// setAttrs, clearAccessor, re-adding a live key — all bump icEpoch, while
// appending a brand new key leaves lower slots where they are and so needs no
// invalidation.
type propIC struct {
	shape  *shape
	epoch  uint32
	slot   uint32
	misses uint8 // shape changes seen; at icMissLimit the site is abandoned
}

// icMissLimit is how many times a site may see a different shape before it stops
// caching for good (shape set to nil, which no live object can match).
//
// A genuinely polymorphic site cannot be served by a one-entry cache, and every
// attempt costs a second shape probe on top of the lookup that already happened
// — measured at +16% on a four-shape read loop. Giving up makes such a site cost
// what it did before the cache existed. Four is the usual industry cut-off;
// eight leaves room for a site whose object legitimately grows a few times
// before settling.
const icMissLimit = 8

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

// shape is a hidden class (ant struct ant_shape).
type shape struct {
	refCount   uint32
	inobjLimit uint8
	props      []shapeProp
	index      map[propKey]uint32
	children   map[childKey]*shape
	parent     *shape
	parentKey  childKey
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
		rt.rootShapes[c] = &shape{refCount: 1, inobjLimit: c, index: map[propKey]uint32{}}
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
	c := &shape{refCount: 1, inobjLimit: clampInobjLimit(s.inobjLimit), index: map[propKey]uint32{}}
	if len(s.props) > 0 {
		c.props = make([]shapeProp, len(s.props))
		copy(c.props, s.props)
		for i, p := range c.props {
			c.index[p.key] = uint32(i)
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
	if slot, ok := s.index[key]; ok {
		return int32(slot)
	}
	return -1
}

func (s *shape) lookupInterned(interned string) int32 { return s.lookup(strKey(interned)) }
func (s *shape) lookupSymbol(off uint32) int32        { return s.lookup(symKey(off)) }

// addKey adds or updates a property key in place, returning its slot
// (ant shape_add_key). Updating an existing key's attrs bumps the IC epoch.
func (s *shape) addKey(key propKey, attrs uint8) (uint32, bool) {
	if s == nil {
		return 0, false
	}
	if slot, ok := s.index[key]; ok {
		s.props[slot].attrs = attrs
		icEpochBump()
		return slot, true
	}
	slot := uint32(len(s.props))
	s.props = append(s.props, shapeProp{key: key, attrs: attrs})
	if s.index == nil {
		s.index = map[propKey]uint32{}
	}
	s.index[key] = slot
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
		return s.addKey(key, attrs)
	}
	ck := childKey{key: key, attrs: attrs}
	if child := s.findChild(ck); child != nil {
		if slot := child.lookup(key); slot >= 0 {
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
	delete(s.index, s.props[slot].key)
	copy(s.props[slot:], s.props[slot+1:])
	s.props = s.props[:len(s.props)-1]
	for i := int(slot); i < len(s.props); i++ {
		s.index[s.props[i].key] = uint32(i)
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
