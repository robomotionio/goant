//go:build amd64

package engine

import (
	"testing"
)

// The compiled inline cache, checked against the interpreter rather than
// against what it was meant to do.
//
// A property read is the one place this tier reproduces engine logic rather
// than emitting arithmetic, so the way to be confident in it is not to reason
// about the guards but to run both and compare. Every case below is a shape the
// probe must either serve identically or decline — and declining has to be
// invisible, because the runtime path answers the same question.

// jitField compiles `function f(o){ return o.NAME; }` and returns everything
// needed to run it both ways.
func jitField(t testing.TB, name string) (*Runtime, *svFunc, *jitCode) {
	t.Helper()
	src := "function f(o){ return o." + name + "; }"
	rt, fn := jitFnRT(t, src)
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatalf("refused to compile %q", src)
	}
	t.Cleanup(c.free)
	return rt, fn, c
}

// jitGet runs the compiled function on one receiver.
func jitGet(t testing.TB, rt *Runtime, fn *svFunc, c *jitCode, recv Value) (Value, *ThrowError) {
	t.Helper()
	locals := make([]Value, fn.maxLocals)
	locals[0] = recv
	v, e, ok := c.jitRun(rt, fn, locals)
	if !ok {
		t.Fatalf("compiled code declined a receiver it should have handled")
	}
	return v, e
}

// TestJITPropertyAgreesWithTheRuntime is the gate for the whole probe.
//
// Each case builds a receiver, reads one name from it through compiled code and
// through the engine, and requires the two to be the same Value bit for bit. The
// cases are chosen as the guards the probe emits, one per guard, plus the ones
// it does not emit a guard for because the tag check already excluded them.
func TestJITPropertyAgreesWithTheRuntime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		build func(rt *Runtime) Value
	}{
		{"own-inline-slot", "x", func(rt *Runtime) Value {
			o := rt.newObject(rt.objectProto)
			rt.objPtr(o).defineOwn("x", tov(42), attrDefault)
			return o
		}},
		{"second-inline-slot", "y", func(rt *Runtime) Value {
			o := rt.newObject(rt.objectProto)
			rt.objPtr(o).defineOwn("x", tov(1), attrDefault)
			rt.objPtr(o).defineOwn("y", tov(2), attrDefault)
			return o
		}},
		{"past-the-inline-slots", "e", func(rt *Runtime) Value {
			// Six properties against four inline slots, so the last ones live in
			// the overflow slice and the probe has to decline them.
			o := rt.newObject(rt.objectProto)
			for i, n := range []string{"a", "b", "c", "d", "e", "f"} {
				rt.objPtr(o).defineOwn(n, tov(float64(i)), attrDefault)
			}
			return o
		}},
		{"holds-an-object", "x", func(rt *Runtime) Value {
			o := rt.newObject(rt.objectProto)
			inner := rt.newObject(rt.objectProto)
			rt.objPtr(o).defineOwn("x", inner, attrDefault)
			return o
		}},
		{"holds-undefined", "x", func(rt *Runtime) Value {
			o := rt.newObject(rt.objectProto)
			rt.objPtr(o).defineOwn("x", mkundef(), attrDefault)
			return o
		}},
		{"absent", "nope", func(rt *Runtime) Value {
			o := rt.newObject(rt.objectProto)
			rt.objPtr(o).defineOwn("x", tov(1), attrDefault)
			return o
		}},
		{"on-the-prototype", "shared", func(rt *Runtime) Value {
			proto := rt.newObject(rt.objectProto)
			rt.objPtr(proto).defineOwn("shared", tov(99), attrDefault)
			o := rt.newObject(proto)
			rt.objPtr(o).defineOwn("own", tov(1), attrDefault)
			return o
		}},
		{"shadowing-the-prototype", "shared", func(rt *Runtime) Value {
			proto := rt.newObject(rt.objectProto)
			rt.objPtr(proto).defineOwn("shared", tov(99), attrDefault)
			o := rt.newObject(proto)
			rt.objPtr(o).defineOwn("shared", tov(1), attrDefault)
			return o
		}},
		{"an-array", "length", func(rt *Runtime) Value {
			a := rt.newArray()
			rt.setField(a, "0", tov(1))
			rt.setField(a, "1", tov(2))
			return a
		}},
		{"a-string", "length", func(rt *Runtime) Value { return rt.newString("hello") }},
		{"a-number", "toFixed", func(rt *Runtime) Value { return tov(1.5) }},
		{"a-boolean", "x", func(rt *Runtime) Value { return mkbool(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, fn, c := jitField(t, tc.field)
			recv := tc.build(rt)

			want, wantErr := rt.getField(recv, tc.field)
			got, gotErr := jitGet(t, rt, fn, c, recv)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("threw %v, runtime threw %v", gotErr != nil, wantErr != nil)
			}
			if wantErr == nil && uint64(got) != uint64(want) {
				t.Errorf("compiled %#x (%v), runtime %#x (%v)",
					uint64(got), got.Type(), uint64(want), want.Type())
			}
		})
	}
}

// TestJITPropertyThrowsLikeTheRuntime covers the receivers that have no
// properties at all. Compiled code must produce the TypeError rather than
// resolving a handle out of a Value that does not carry one.
func TestJITPropertyThrowsLikeTheRuntime(t *testing.T) {
	for _, recv := range []Value{mkundef(), mknull()} {
		rt, fn, c := jitField(t, "x")
		if _, e := jitGet(t, rt, fn, c, recv); e == nil {
			t.Errorf("reading a property of %v did not throw", recv.Type())
		}
	}
}

// TestJITPropertySurvivesAShapeChange is the guard the epoch is there for.
//
// Adding a property gives an object a new shape, which the cached one no longer
// matches; deleting one can move slots underneath a shape whose pointer has not
// changed, which identity alone would not catch. Both have to keep answering
// correctly, over enough repetitions that the site is warm before each change.
func TestJITPropertySurvivesAShapeChange(t *testing.T) {
	rt, fn, c := jitField(t, "x")
	o := rt.newObject(rt.objectProto)
	rt.objPtr(o).defineOwn("x", tov(1), attrDefault)

	check := func(step string) {
		t.Helper()
		want, _ := rt.getField(o, "x")
		for i := 0; i < 8; i++ {
			got, e := jitGet(t, rt, fn, c, o)
			if e != nil {
				t.Fatalf("%s: threw", step)
			}
			if uint64(got) != uint64(want) {
				t.Fatalf("%s: compiled %#x, runtime %#x", step, uint64(got), uint64(want))
			}
		}
	}

	check("as defined")
	rt.objPtr(o).defineOwn("y", tov(2), attrDefault)
	check("after growing a new shape")
	rt.setField(o, "x", tov(7))
	check("after a store")
	rt.objPtr(o).deleteOwn("y")
	check("after a delete moved the slots")
	rt.objPtr(o).deleteOwn("x")
	check("after the property itself went")
}

// TestJITPropertyCallsAGetter pins the case where reading a slot would be
// silently wrong rather than merely slow: an accessor's slot holds undefined,
// so a cache that served it would replace every getter call with undefined.
func TestJITPropertyCallsAGetter(t *testing.T) {
	rt, fn, c := jitField(t, "x")
	fnVal, err := rt.RunString("getter.js", `
		(function () {
			var n = 0;
			return { get x() { return ++n; } };
		})()`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for i := 1; i <= 8; i++ {
		got, e := jitGet(t, rt, fn, c, fnVal)
		if e != nil {
			t.Fatal("threw")
		}
		if !got.IsNumber() || got.Number() != float64(i) {
			t.Fatalf("call %d returned %v, want %d — the getter was not run", i, got, i)
		}
	}
}

// TestJITPropertyThroughAProxy checks the receiver whose [[Get]] is a trap. The
// probe tests for one explicitly even though a Proxy's own shape can never match
// a cached one, because that argument is about how ways are filled and this is
// not the place to depend on it.
func TestJITPropertyThroughAProxy(t *testing.T) {
	rt, fn, c := jitField(t, "x")
	p, err := rt.RunString("proxy.js", `
		new Proxy({x: 1}, { get: function (t, k) { return k === "x" ? 41 + 1 : undefined; } })`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, e := jitGet(t, rt, fn, c, p)
	if e != nil {
		t.Fatal("threw")
	}
	if !got.IsNumber() || got.Number() != 42 {
		t.Errorf("got %v, want the trap's 42", got)
	}
}

// TestJITPropertyForcesALazyDocument is the check that the probe honours the one
// thing slotGet does beyond reading a slot. A document parsed lazily leaves a
// sentinel in the slot until something asks for it; handing that sentinel back
// as a value would put an internal marker into a JavaScript program.
func TestJITPropertyForcesALazyDocument(t *testing.T) {
	rt, fn, c := jitField(t, "b")
	v, err := rt.RunString("lazy.js", `JSON.parse('{"a":1,"b":{"c":2},"d":3}')`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, e := jitGet(t, rt, fn, c, v)
	if e != nil {
		t.Fatal("threw")
	}
	if got.isLazy() {
		t.Fatal("an unparsed span was handed back as a value")
	}
	inner, e := rt.getField(got, "c")
	if e != nil || !inner.IsNumber() || inner.Number() != 2 {
		t.Errorf("b.c = %v, want 2", inner)
	}
}

// TestJITPropertyProbeActuallyRuns is what stops all of the above from passing
// against a probe that never fires. Every guard could be inverted and the tests
// would still agree with the runtime, because falling through to the runtime is
// how they agree.
func TestJITPropertyProbeActuallyRuns(t *testing.T) {
	was := jitStats.enabled
	jitStats.enabled = true
	defer func() { jitStats.enabled = was }()
	hit0, miss0 := jitStats.icHit, jitStats.icMiss

	// Compiled after the flag is set, because the counter's increment is only
	// emitted when it is on.
	rt, fn, c := jitField(t, "x")
	o := rt.newObject(rt.objectProto)
	rt.objPtr(o).defineOwn("x", tov(42), attrDefault)

	const runs = 32
	for i := 0; i < runs; i++ {
		got, e := jitGet(t, rt, fn, c, o)
		if e != nil || !got.IsNumber() || got.Number() != 42 {
			t.Fatalf("run %d returned %v", i, got)
		}
	}

	hits, misses := jitStats.icHit-hit0, jitStats.icMiss-miss0
	if hits+misses != runs {
		t.Fatalf("%d hits + %d misses, want %d reads", hits, misses, runs)
	}
	// The first read misses and fills the site; everything after it must hit.
	if misses != 1 {
		t.Errorf("%d misses, want exactly the one that fills the cache", misses)
	}
	if hits != runs-1 {
		t.Errorf("%d hits out of %d reads", hits, runs)
	}
}

// TestJITChecksOnlyTheParametersItComputesWith is the other half of letting an
// object into compiled code.
//
// The prologue's check is "untagged or leave", so a parameter it checks can
// never be an object. Skipping the check for a parameter the body only reads
// fields from is what makes the cache above reachable — and skipping it for one
// the body computes with would hand an object to an ADDSD.
//
// Demand is transitive through the locals, which is the case worth pinning: a
// parameter copied into a variable that is then multiplied is still a parameter
// that has to be checked.
func TestJITChecksOnlyTheParametersItComputesWith(t *testing.T) {
	for _, tc := range []struct {
		src      string
		accepted bool // whether an object argument reaches compiled code
	}{
		{"function f(o){ return o.x; }", true},
		{"function f(o){ var t = o; return t.x; }", true},
		{"function f(o){ return o.x.y; }", true},
		{"function f(o){ return o * 2; }", false},
		{"function f(o){ var t = o; return t * 2; }", false},
		{"function f(o){ var t = o; var u = t; return u - 1; }", false},
		{"function f(o){ if (o < 1) return 1; return o.x; }", false},
	} {
		rt, fn := jitFnRT(t, tc.src)
		c := jitCompile(fn, nil)
		if c == nil {
			t.Errorf("refused %q", tc.src)
			continue
		}
		locals := make([]Value, fn.maxLocals)
		o := rt.newObject(rt.objectProto)
		rt.objPtr(o).defineOwn("x", rt.newObject(rt.objectProto), attrDefault)
		locals[0] = o
		_, _, ok := c.jitRun(rt, fn, locals)
		if ok != tc.accepted {
			verb := "declined"
			if ok {
				verb = "accepted"
			}
			t.Errorf("%q %s an object parameter", tc.src, verb)
		}
		c.free()
	}
}

// BenchmarkJITProperty is the probe against the interpreter's cache, over a loop
// rather than a call, so what is measured is the read itself and not the cost of
// entering compiled code.
//
// The values are discarded because this tier cannot yet compute with a field's
// value. That makes this a measurement of the probe alone, which is what it is
// for — the arithmetic it would feed is a separate piece of work.
func BenchmarkJITProperty(b *testing.B) {
	const src = "function f(o,n){ var i=0; while (i<n) { o.a; o.b; o.c; o.d; i=i+1; } return i; }"
	rt, fn := jitFnRT(b, src)
	c := jitCompile(fn, nil)
	if c == nil {
		b.Fatal("refused to compile")
	}
	defer c.free()

	o := rt.newObject(rt.objectProto)
	for i, n := range []string{"a", "b", "c", "d"} {
		rt.objPtr(o).defineOwn(n, tov(float64(i)), attrDefault)
	}

	const iters = 1000
	b.Run("compiled", func(b *testing.B) {
		locals := make([]Value, fn.maxLocals)
		locals[0], locals[1] = o, tov(iters)
		for b.Loop() {
			if _, _, ok := c.jitRun(rt, fn, locals); !ok {
				b.Fatal("declined")
			}
		}
		b.ReportMetric(float64(4*iters), "reads/op")
	})
	b.Run("interpreted", func(b *testing.B) {
		// A second Runtime, and therefore a second receiver: a Value carries a
		// handle into the pool of the Runtime that made it, so the one above
		// would resolve to whatever happens to occupy that cell here.
		ref := New()
		fnVal, err := ref.RunString("bench.js", src+"; f;")
		if err != nil {
			b.Fatal(err)
		}
		recv, err := ref.RunString("recv.js", "({a:0,b:1,c:2,d:3})")
		if err != nil {
			b.Fatal(err)
		}
		args := []Value{recv, tov(iters)}
		for b.Loop() {
			if _, e := ref.callValue(fnVal, mkundef(), args); e != nil {
				b.Fatal("throw")
			}
		}
		b.ReportMetric(float64(4*iters), "reads/op")
	})
}

// TestJITPropertyRefusesAPrivateName keeps `other.#x` out of getField.
//
// GET_FIELD carries private names as well as properties, and they are not
// properties: they resolve against the class environment the frame carries,
// which compiled code does not have. Reading one as a property would find
// nothing and return undefined rather than throwing.
func TestJITPropertyRefusesAPrivateName(t *testing.T) {
	const src = `
		class P {
			#x = 1;
			static read(o) { return o.#x; }
		}`
	rt := New()
	prog, err := Parse("priv.js", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	top, err := rt.Compile(prog, "priv.js", src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	found := false
	var walk func(fns []*svFunc)
	walk = func(fns []*svFunc) {
		for _, fn := range fns {
			if fn.name == "read" {
				found = true
				if c := jitCompile(fn, nil); c != nil {
					c.free()
					t.Error("compiled a method reading a private name")
				}
			}
			walk(fn.childFuncs)
		}
	}
	walk(top.childFuncs)
	if !found {
		t.Fatal("did not find the method under test")
	}
}
