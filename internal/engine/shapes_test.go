package engine

import "testing"

func TestShapeSlotAssignment(t *testing.T) {
	s := newShape()
	for i, name := range []string{"a", "b", "c"} {
		slot, ok := addInternedTr(&s, name, attrDefault)
		if !ok {
			t.Fatalf("add %q failed", name)
		}
		if int(slot) != i {
			t.Errorf("key %q got slot %d want %d", name, slot, i)
		}
	}
	if s.count() != 3 {
		t.Fatalf("count=%d want 3", s.count())
	}
	for i, name := range []string{"a", "b", "c"} {
		if got := s.lookupInterned(name); got != int32(i) {
			t.Errorf("lookup %q=%d want %d", name, got, i)
		}
	}
	if s.lookupInterned("missing") != -1 {
		t.Error("missing key should return -1")
	}
}

func TestShapeReaddUpdatesAttrs(t *testing.T) {
	s := newShape()
	slot0, _ := addInternedTr(&s, "x", attrDefault)
	before := s.count()
	epoch := icEpoch
	// Re-adding same key with different attrs updates in place, no new slot.
	slot1, ok := addInternedTr(&s, "x", attrEnumerable)
	if !ok || slot1 != slot0 {
		t.Fatalf("re-add slot=%d want %d", slot1, slot0)
	}
	if s.count() != before {
		t.Errorf("count changed on re-add: %d", s.count())
	}
	if s.attrsAt(slot0) != attrEnumerable {
		t.Errorf("attrs not updated: %d", s.attrsAt(slot0))
	}
	if icEpoch == epoch {
		t.Error("attr change should bump IC epoch")
	}
}

func TestShapeTransitionCanonicalization(t *testing.T) {
	// The first object to create a transition gets a private working shape;
	// the 2nd and subsequent objects converge on the canonical tree shape.
	s1 := newShape()
	addInternedTr(&s1, "a", attrDefault)

	s2 := newShape()
	addInternedTr(&s2, "a", attrDefault)

	s3 := newShape()
	addInternedTr(&s3, "a", attrDefault)

	if s2 != s3 {
		t.Error("objects 2 and 3 should share the canonical shape")
	}
	if s2.lookupInterned("a") != 0 || s1.lookupInterned("a") != 0 {
		t.Error("all shapes should map 'a' to slot 0")
	}
	// Distinct property additions diverge into distinct shapes.
	sx := newShape()
	addInternedTr(&sx, "different", attrDefault)
	if sx == s2 {
		t.Error("distinct keys must not share a shape")
	}
}

func TestShapeSymbolKeys(t *testing.T) {
	s := newShape()
	slotA, _ := addSymbolTr(&s, 100, attrDefault)
	slotB, _ := addInternedTr(&s, "str", attrDefault)
	if slotA == slotB {
		t.Fatal("symbol and string keys collided")
	}
	if s.lookupSymbol(100) != int32(slotA) {
		t.Error("symbol lookup failed")
	}
	if s.lookupSymbol(999) != -1 {
		t.Error("absent symbol should be -1")
	}
}

func TestShapeRemoveSlot(t *testing.T) {
	// removeSlot uses swap-with-last; the returned index is the slot the last
	// element moved from.
	s := newShape()
	addInternedTr(&s, "a", attrDefault)
	addInternedTr(&s, "b", attrDefault)
	addInternedTr(&s, "c", attrDefault)
	// Clone to a private shape so we can mutate.
	priv := s.clone()
	swappedFrom, ok := priv.removeSlot(0) // remove "a"
	if !ok {
		t.Fatal("remove failed")
	}
	if swappedFrom != 2 {
		t.Errorf("swappedFrom=%d want 2 (c moved into slot 0)", swappedFrom)
	}
	if priv.count() != 2 {
		t.Errorf("count=%d want 2", priv.count())
	}
	if priv.lookupInterned("a") != -1 {
		t.Error("'a' should be gone")
	}
	if priv.lookupInterned("c") != 0 {
		t.Errorf("'c' should now be slot 0, got %d", priv.lookupInterned("c"))
	}
	if priv.lookupInterned("b") != 1 {
		t.Errorf("'b' should still be slot 1, got %d", priv.lookupInterned("b"))
	}
}

func TestShapeInobjLimit(t *testing.T) {
	s := newShapeWithLimit(2)
	if s.getInobjLimit() != 2 {
		t.Errorf("inobj limit=%d want 2", s.getInobjLimit())
	}
	// Over-limit clamps to inobjMaxSlots.
	s2 := newShapeWithLimit(99)
	if s2.getInobjLimit() != inobjMaxSlots {
		t.Errorf("clamp failed: %d", s2.getInobjLimit())
	}
}
