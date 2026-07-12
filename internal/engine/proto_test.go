package engine

import "testing"

func TestProtoChainGet(t *testing.T) {
	rt := New()
	base := rt.newObject(mknull())
	rt.objPtr(base).defineOwn("inherited", mknum(1), attrDefault)

	child := rt.newObject(base)
	rt.objPtr(child).defineOwn("own", mknum(2), attrDefault)

	// own property
	if v, ok := rt.getProp(child, "own"); !ok || v.Number() != 2 {
		t.Errorf("own get = %v,%v", v.Number(), ok)
	}
	// inherited property
	if v, ok := rt.getProp(child, "inherited"); !ok || v.Number() != 1 {
		t.Errorf("inherited get = %v,%v", v.Number(), ok)
	}
	// absent property
	if _, ok := rt.getProp(child, "nope"); ok {
		t.Error("absent property should not be found")
	}
	if !rt.hasProp(child, "inherited") || rt.hasProp(child, "nope") {
		t.Error("hasProp wrong")
	}
}

func TestProtoChainShadowing(t *testing.T) {
	rt := New()
	base := rt.newObject(mknull())
	rt.objPtr(base).defineOwn("x", mknum(1), attrDefault)
	child := rt.newObject(base)

	// Setting x on child creates an own property shadowing the inherited one.
	if !rt.setProp(child, "x", mknum(99)) {
		t.Fatal("set failed")
	}
	if !rt.objPtr(child).hasOwn("x") {
		t.Error("set should create own property (shadowing)")
	}
	if v, _ := rt.getProp(child, "x"); v.Number() != 99 {
		t.Errorf("child.x = %v want 99", v.Number())
	}
	// Base is unchanged.
	if v, _ := rt.getProp(base, "x"); v.Number() != 1 {
		t.Errorf("base.x = %v want 1 (should not be mutated)", v.Number())
	}
}

func TestProtoChainInheritedNonWritable(t *testing.T) {
	rt := New()
	base := rt.newObject(mknull())
	// non-writable inherited property blocks the write
	rt.objPtr(base).defineOwn("ro", mknum(1), attrEnumerable|attrConfigurable)
	child := rt.newObject(base)

	if rt.setProp(child, "ro", mknum(2)) {
		t.Error("set through inherited non-writable should fail")
	}
	if rt.objPtr(child).hasOwn("ro") {
		t.Error("no own property should be created")
	}
}

func TestProtoChainDepthGuard(t *testing.T) {
	rt := New()
	// Build a deep chain and ensure resolveProp terminates.
	cur := rt.newObject(mknull())
	rt.objPtr(cur).defineOwn("deep", mknum(7), attrDefault)
	for i := 0; i < 10; i++ {
		cur = rt.newObject(cur)
	}
	if v, ok := rt.getProp(cur, "deep"); !ok || v.Number() != 7 {
		t.Errorf("deep chain get = %v,%v", v.Number(), ok)
	}
}

func TestDefineAccessorStorage(t *testing.T) {
	rt := New()
	o := rt.objPtr(rt.newObject(mknull()))
	getter := rt.newObject(mknull()) // stand-in callable (Phase 3 invokes it)
	if !o.defineAccessor("g", getter, mkundef(), true, false, attrEnumerable|attrConfigurable) {
		t.Fatal("defineAccessor failed")
	}
	slot := o.shape.lookupInterned("g")
	if slot < 0 || !o.isAccessorSlot(uint32(slot)) {
		t.Fatal("accessor slot not recorded")
	}
	p := o.shape.propAt(uint32(slot))
	if !p.hasGetter || p.getter != getter {
		t.Error("getter not stored")
	}
	// A sibling object with the same key must not inherit this getter.
	o2 := rt.objPtr(rt.newObject(mknull()))
	o2.defineOwn("g", mknum(5), attrDefault)
	s2 := o2.shape.lookupInterned("g")
	if o2.isAccessorSlot(uint32(s2)) {
		t.Error("sibling object contaminated by accessor")
	}
}
