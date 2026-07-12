package engine

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

// icEpoch is the global inline-cache generation counter (ant ant_ic_epoch_counter).
var icEpoch uint32 = 1

func icEpochBump() {
	icEpoch++
	if icEpoch == 0 {
		icEpoch = 1
	}
}

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
	key       propKey
	attrs     uint8
	hasGetter bool
	hasSetter bool
	getter    Value
	setter    Value
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

// rootShapes caches the empty root shape per inobj_limit.
var rootShapes [inobjMaxSlots + 1]*shape

func clampInobjLimit(limit uint8) uint8 {
	if limit > inobjMaxSlots {
		return inobjMaxSlots
	}
	return limit
}

// newShapeWithLimit returns the shared empty root shape for a given inobj
// limit, retained (ant ant_shape_new_with_inobj_limit).
func newShapeWithLimit(inobjLimit uint8) *shape {
	c := clampInobjLimit(inobjLimit)
	if rootShapes[c] == nil {
		rootShapes[c] = &shape{refCount: 1, inobjLimit: c, index: map[propKey]uint32{}}
	}
	rootShapes[c].refCount++
	return rootShapes[c]
}

func newShape() *shape { return newShapeWithLimit(inobjMaxSlots) }

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

// removeSlot deletes a slot via swap-with-last, returning the slot that was
// moved into the hole (ant ant_shape_remove_slot). Callers move the object's
// stored value the same way. Bumps the IC epoch.
func (s *shape) removeSlot(slot uint32) (swappedFrom uint32, ok bool) {
	if s == nil || int(slot) >= len(s.props) {
		return 0, false
	}
	swappedFrom = slot
	delete(s.index, s.props[slot].key)
	last := uint32(len(s.props) - 1)
	if slot != last {
		s.props[slot] = s.props[last]
		s.index[s.props[slot].key] = slot
		swappedFrom = last
	}
	s.props = s.props[:last]
	icEpochBump()
	return swappedFrom, true
}

// clearAccessor clears getter/setter on a slot (ant ant_shape_clear_accessor_slot).
func (s *shape) clearAccessor(slot uint32) bool {
	p := s.propAt(slot)
	if p == nil {
		return false
	}
	p.hasGetter, p.hasSetter, p.getter, p.setter = false, false, 0, 0
	icEpochBump()
	return true
}
