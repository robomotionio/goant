package engine

import "testing"

func TestObjectDefineGetSet(t *testing.T) {
	rt := New()
	ov := rt.newObject(mknull())
	o := rt.objPtr(ov)
	if o == nil {
		t.Fatal("objPtr nil")
	}

	if !o.defineOwn("a", mknum(1), attrDefault) {
		t.Fatal("define a failed")
	}
	if !o.defineOwn("b", mknum(2), attrDefault) {
		t.Fatal("define b failed")
	}
	if v, ok := o.getOwn("a"); !ok || v.Number() != 1 {
		t.Fatalf("get a = %v,%v", v.Number(), ok)
	}
	if v, ok := o.getOwn("b"); !ok || v.Number() != 2 {
		t.Fatalf("get b = %v,%v", v.Number(), ok)
	}
	if _, ok := o.getOwn("missing"); ok {
		t.Fatal("missing should not be found")
	}

	// Overwrite via setOwn.
	if !o.setOwn("a", mknum(42)) {
		t.Fatal("set a failed")
	}
	if v, _ := o.getOwn("a"); v.Number() != 42 {
		t.Fatalf("a not updated: %v", v.Number())
	}
}

func TestObjectManyPropsOverflow(t *testing.T) {
	// More than inobjMaxSlots properties exercises the overflow slice.
	rt := New()
	o := rt.objPtr(rt.newObject(mknull()))
	names := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7"}
	for i, n := range names {
		o.defineOwn(n, mknum(float64(i*10)), attrDefault)
	}
	for i, n := range names {
		v, ok := o.getOwn(n)
		if !ok || v.Number() != float64(i*10) {
			t.Errorf("prop %s = %v,%v want %d", n, v.Number(), ok, i*10)
		}
	}
	if len(o.overflow) != len(names)-inobjMaxSlots {
		t.Errorf("overflow len=%d want %d", len(o.overflow), len(names)-inobjMaxSlots)
	}
}

func TestObjectNonWritable(t *testing.T) {
	rt := New()
	o := rt.objPtr(rt.newObject(mknull()))
	o.defineOwn("ro", mknum(1), attrEnumerable|attrConfigurable) // no writable
	if o.setOwn("ro", mknum(2)) {
		t.Fatal("set on non-writable should fail")
	}
	if v, _ := o.getOwn("ro"); v.Number() != 1 {
		t.Fatal("non-writable value changed")
	}
}

func TestObjectNonExtensible(t *testing.T) {
	rt := New()
	o := rt.objPtr(rt.newObject(mknull()))
	o.defineOwn("x", mknum(1), attrDefault)
	o.flags.extensible = false
	if o.setOwn("y", mknum(2)) {
		t.Fatal("adding to non-extensible should fail")
	}
	if o.hasOwn("y") {
		t.Fatal("y should not exist")
	}
	// But existing writable property still settable.
	if !o.setOwn("x", mknum(9)) {
		t.Fatal("set existing on non-extensible should succeed")
	}
}

func TestObjectDelete(t *testing.T) {
	rt := New()
	o := rt.objPtr(rt.newObject(mknull()))
	for _, n := range []string{"a", "b", "c"} {
		o.defineOwn(n, mknum(1), attrDefault)
	}
	if !o.deleteOwn("a") {
		t.Fatal("delete a failed")
	}
	if o.hasOwn("a") {
		t.Fatal("a still present")
	}
	// Remaining properties still accessible with correct values.
	o.defineOwn("b", mknum(20), attrDefault) // b existed; slot may have moved
	if v, ok := o.getOwn("b"); !ok || v.Number() != 20 {
		t.Fatalf("b after delete = %v,%v", v.Number(), ok)
	}
	if v, ok := o.getOwn("c"); !ok || v.Number() != 1 {
		t.Fatalf("c after delete = %v,%v", v.Number(), ok)
	}

	// Non-configurable delete rejected.
	o.defineOwn("locked", mknum(1), attrWritable)
	if o.deleteOwn("locked") {
		t.Fatal("delete of non-configurable should fail")
	}
	if !o.hasOwn("locked") {
		t.Fatal("locked should remain")
	}
}

func TestObjectOwnKeysOrder(t *testing.T) {
	rt := New()
	o := rt.objPtr(rt.newObject(mknull()))
	order := []string{"z", "a", "m", "b"}
	for _, n := range order {
		o.defineOwn(n, mknum(1), attrDefault)
	}
	keys := o.ownKeys()
	for i, k := range order {
		if keys[i] != k {
			t.Errorf("key order[%d]=%q want %q", i, keys[i], k)
		}
	}
	// Enumerable filtering.
	o.defineOwn("hidden", mknum(1), attrWritable) // not enumerable
	enum := o.ownKeysEnumerable()
	for _, k := range enum {
		if k == "hidden" {
			t.Fatal("non-enumerable key leaked into enumerable set")
		}
	}
}

func TestObjectInternalSlots(t *testing.T) {
	rt := New()
	o := rt.objPtr(rt.newObject(mknull()))
	if !o.getSlot(slotData).IsUndefined() {
		t.Fatal("absent slot should be undefined")
	}
	o.setSlot(slotData, mknum(7))
	o.setSlot(slotBrand, mknum(3))
	if o.getSlot(slotData).Number() != 7 {
		t.Fatal("slotData")
	}
	if o.brandID() != 3 {
		t.Fatalf("brandID=%d want 3", o.brandID())
	}
	// Internal slots do not appear as named properties.
	if o.hasOwn("data") {
		t.Fatal("internal slot leaked as property")
	}
}
