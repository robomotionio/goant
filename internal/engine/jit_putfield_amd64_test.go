//go:build amd64

package engine

import (
	"strconv"
	"strings"
	"testing"
)

// The compiled store, checked against the interpreter running the same source.
//
// A store is harder to check than a read: a read produces a value and a wrong
// one is visible immediately, while a store produces nothing and a wrong one is
// a difference in the receiver that may not surface until much later. So every
// case below runs the same assignment twice on two identically built receivers —
// once through compiled code, once through the interpreter — and compares the
// whole object afterwards, not just the property that was assigned.

// jitStore compiles `function f(o,v){ o.NAME = v; return 0; }` and returns
// everything needed to run it both ways.
func jitStore(t testing.TB, name string) (*Runtime, *svFunc, *jitCode, string) {
	t.Helper()
	return jitStoreSrc(t, "function f(o,v){ o."+name+" = v; return 0; }")
}

func jitStoreSrc(t testing.TB, src string) (*Runtime, *svFunc, *jitCode, string) {
	t.Helper()
	rt, fn := jitFnRT(t, src)
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatalf("refused to compile %q", src)
	}
	t.Cleanup(c.free)
	return rt, fn, c, src
}

// jitPut runs the compiled store on one receiver and value.
func jitPut(t testing.TB, rt *Runtime, fn *svFunc, c *jitCode, recv, val Value) *ThrowError {
	t.Helper()
	locals := make([]Value, fn.maxLocals)
	locals[0], locals[1] = recv, val
	_, e, ok := c.jitRun(rt, fn, nil, 0, nil, locals, mkundef())
	if !ok {
		t.Fatalf("compiled code declined arguments it should have handled")
	}
	return e
}

// jitSnapshot describes an object completely enough that any difference a store
// could make shows up: its own keys in slot order, and what each holds.
//
// Read through getField rather than out of the slots, so an accessor is
// described by what it answers and a lazily parsed span by what it becomes —
// which is what a program would see.
//
// Structural rather than by identity, and it has to be: the two receivers being
// compared are two different objects, so every handle in them differs. What must
// match is the shape of what the store left behind.
func jitSnapshot(rt *Runtime, v Value) string {
	var b strings.Builder
	rt.describe(&b, v, 3)
	return b.String()
}

func (rt *Runtime) describe(b *strings.Builder, v Value, depth int) {
	if v.IsNumber() {
		b.WriteString(strconv.FormatFloat(v.Number(), 'g', -1, 64))
		return
	}
	if v.Type() == TStr {
		b.WriteString("'" + string(rt.strBytes(v)) + "'")
		return
	}
	o := rt.objPtr(v)
	if o == nil {
		b.WriteString(typeName(v.Type()))
		return
	}
	if depth == 0 {
		b.WriteString(typeName(v.Type()) + "{...}")
		return
	}
	b.WriteString(typeName(v.Type()) + "{")
	for _, k := range o.ownKeys() {
		b.WriteString(k)
		b.WriteByte('=')
		if got, e := rt.getField(v, k); e != nil {
			b.WriteString("<throw>")
		} else {
			rt.describe(b, got, depth-1)
		}
		b.WriteByte(';')
	}
	if !o.flags.extensible {
		b.WriteString("!extensible;")
	}
	b.WriteByte('}')
}

// jitStoreCases are the shapes of receiver a store site meets, one per guard the
// probe emits plus the ones it declines by falling through.
var jitStoreCases = []struct {
	name  string
	field string
	build func(rt *Runtime) Value
}{
	{"own-inline-slot", "x", func(rt *Runtime) Value {
		o := rt.newObject(rt.objectProto)
		rt.objPtr(o).defineOwn("x", tov(1), attrDefault)
		return o
	}},
	{"second-inline-slot", "y", func(rt *Runtime) Value {
		o := rt.newObject(rt.objectProto)
		rt.objPtr(o).defineOwn("x", tov(1), attrDefault)
		rt.objPtr(o).defineOwn("y", tov(2), attrDefault)
		return o
	}},
	{"past-the-inline-slots", "e", func(rt *Runtime) Value {
		// Six properties against four inline slots, so the last ones live in the
		// overflow slice and the probe has to decline them.
		o := rt.newObject(rt.objectProto)
		for i, n := range []string{"a", "b", "c", "d", "e", "f"} {
			rt.objPtr(o).defineOwn(n, tov(float64(i)), attrDefault)
		}
		return o
	}},
	{"creates-the-property", "x", func(rt *Runtime) Value {
		// The transition case, which this cut declines: the store adds a slot
		// and installs a new shape, and only the runtime does that.
		o := rt.newObject(rt.objectProto)
		rt.objPtr(o).defineOwn("y", tov(1), attrDefault)
		return o
	}},
	{"non-writable", "x", func(rt *Runtime) Value {
		o := rt.newObject(rt.objectProto)
		rt.objPtr(o).defineOwn("x", tov(1), attrEnumerable|attrConfigurable)
		return o
	}},
	{"frozen", "x", func(rt *Runtime) Value {
		v, _ := rt.RunString("frozen.js", "Object.freeze({x: 1})")
		return v
	}},
	{"not-extensible", "x", func(rt *Runtime) Value {
		v, _ := rt.RunString("sealed.js", "Object.preventExtensions({y: 1})")
		return v
	}},
	{"through-a-setter", "x", func(rt *Runtime) Value {
		v, _ := rt.RunString("setter.js", "({ n: 0, set x(v) { this.n = v; } })")
		return v
	}},
	{"inherited-setter", "x", func(rt *Runtime) Value {
		v, _ := rt.RunString("proto-setter.js",
			"Object.create({ set x(v) { this.n = v; } }, { n: {value: 0, writable: true, enumerable: true} })")
		return v
	}},
	{"inherited-data-property", "x", func(rt *Runtime) Value {
		v, _ := rt.RunString("proto-data.js", "Object.create({x: 1}, {y: {value: 2, enumerable: true}})")
		return v
	}},
	{"through-a-proxy", "x", func(rt *Runtime) Value {
		v, _ := rt.RunString("proxy.js",
			"new Proxy({x: 1}, { set: function (t, k, v) { t[k] = 'trapped'; return true; } })")
		return v
	}},
	{"a-named-property-on-an-array", "tag", func(rt *Runtime) Value {
		// An index cannot reach PUT_FIELD — `o.0` does not parse and `o[0]` is
		// PUT_ELEM — but a name on an array can, and an array's [[Set]] is not a
		// slot write.
		v, _ := rt.RunString("arr.js", "[1, 2, 3]")
		return v
	}},
	{"an-array-length", "length", func(rt *Runtime) Value {
		v, _ := rt.RunString("arr.js", "[1, 2, 3]")
		return v
	}},
	{"a-string-object", "x", func(rt *Runtime) Value {
		v, _ := rt.RunString("str.js", "new String('hi')")
		return v
	}},
}

// TestJITStoreAgreesWithTheInterpreter is the gate for the whole probe.
func TestJITStoreAgreesWithTheInterpreter(t *testing.T) {
	values := []struct {
		name string
		of   func(rt *Runtime) Value
	}{
		{"a-number", func(rt *Runtime) Value { return tov(42) }},
		{"undefined", func(rt *Runtime) Value { return mkundef() }},
		{"an-object", func(rt *Runtime) Value { return rt.newObject(rt.objectProto) }},
		{"a-string", func(rt *Runtime) Value { return rt.newString("stored") }},
	}
	for _, tc := range jitStoreCases {
		for _, val := range values {
			t.Run(tc.name+"/"+val.name, func(t *testing.T) {
				rt, fn, c, src := jitStore(t, tc.field)

				// The interpreter's answer, from the same source in the same
				// Runtime on its own copy of the receiver.
				ref, err := rt.RunString("ref.js", src+"; f;")
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				want := tc.build(rt)
				_, wantErr := rt.callValue(ref, mkundef(), []Value{want, val.of(rt)})

				got := tc.build(rt)
				gotErr := jitPut(t, rt, fn, c, got, val.of(rt))

				if (wantErr == nil) != (gotErr == nil) {
					t.Fatalf("compiled threw %v, interpreter threw %v", gotErr != nil, wantErr != nil)
				}
				if a, b := jitSnapshot(rt, got), jitSnapshot(rt, want); a != b {
					t.Errorf("compiled left %s\ninterpreter left %s", a, b)
				}
			})
		}
	}
}

// TestJITStoreThrowsLikeTheInterpreter covers the receivers that have no
// properties to store into, and the strict-mode assignment that must report a
// failed store as a TypeError rather than swallowing it.
func TestJITStoreThrowsLikeTheInterpreter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		src   string
		build func(rt *Runtime) Value
		throw bool
	}{
		{"undefined-receiver", "function f(o,v){ o.x = v; return 0; }",
			func(rt *Runtime) Value { return mkundef() }, true},
		{"null-receiver", "function f(o,v){ o.x = v; return 0; }",
			func(rt *Runtime) Value { return mknull() }, true},
		{"sloppy-non-writable", "function f(o,v){ o.x = v; return 0; }", func(rt *Runtime) Value {
			v, _ := rt.RunString("f.js", "Object.freeze({x: 1})")
			return v
		}, false},
		{"strict-non-writable", "function f(o,v){ 'use strict'; o.x = v; return 0; }", func(rt *Runtime) Value {
			v, _ := rt.RunString("f.js", "Object.freeze({x: 1})")
			return v
		}, true},
		{"strict-not-extensible", "function f(o,v){ 'use strict'; o.x = v; return 0; }", func(rt *Runtime) Value {
			v, _ := rt.RunString("f.js", "Object.preventExtensions({y: 1})")
			return v
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, fn, c, _ := jitStoreSrc(t, tc.src)
			// Warm first where the receiver allows it, so the emitted path is
			// the one under test rather than the runtime's on every run.
			for i := 0; i < 4; i++ {
				if e := jitPut(t, rt, fn, c, tc.build(rt), tov(7)); (e != nil) != tc.throw {
					t.Fatalf("run %d: threw %v, want %v", i, e != nil, tc.throw)
				}
			}
		})
	}
}

// TestJITStoreSurvivesAShapeChange is the epoch guard from the other side.
//
// A store site caches a slot number, and a delete can move slots underneath a
// shape whose pointer has not changed — which for a read is a wrong answer and
// for a store is a wrong property overwritten.
func TestJITStoreSurvivesAShapeChange(t *testing.T) {
	rt, fn, c, _ := jitStore(t, "x")
	o := rt.newObject(rt.objectProto)
	rt.objPtr(o).defineOwn("x", tov(1), attrDefault)

	n := 0
	check := func(step string) {
		t.Helper()
		for i := 0; i < 8; i++ {
			n++
			if e := jitPut(t, rt, fn, c, o, tov(float64(n))); e != nil {
				t.Fatalf("%s: threw", step)
			}
			got, _ := rt.getField(o, "x")
			if got != tov(float64(n)) {
				t.Fatalf("%s: x is %v after storing %d", step, got, n)
			}
		}
	}

	check("as defined")
	rt.objPtr(o).defineOwn("y", tov(2), attrDefault)
	check("after growing a new shape")
	rt.objPtr(o).defineOwn("z", tov(3), attrDefault)
	rt.objPtr(o).deleteOwn("y")
	check("after a delete moved the slots")
	if got, _ := rt.getField(o, "z"); got != tov(3) {
		t.Errorf("z is %v; a store to x landed on the wrong slot", got)
	}
}

// TestJITStoreProbeActuallyRuns is what stops every test above from passing
// against a probe that never fires: falling through to the runtime is how they
// agree, so agreement alone proves nothing about the emitted path.
func TestJITStoreProbeActuallyRuns(t *testing.T) {
	was := jitStats.enabled
	jitStats.enabled = true
	defer func() { jitStats.enabled = was }()
	hit0, miss0 := jitStats.putHit, jitStats.putMiss

	// Compiled after the flag is set, because the increment is only emitted when
	// it is on.
	rt, fn, c, _ := jitStore(t, "x")
	o := rt.newObject(rt.objectProto)
	rt.objPtr(o).defineOwn("x", tov(0), attrDefault)

	const runs = 32
	for i := 0; i < runs; i++ {
		if e := jitPut(t, rt, fn, c, o, tov(float64(i))); e != nil {
			t.Fatalf("run %d threw", i)
		}
	}
	if got, _ := rt.getField(o, "x"); got != tov(runs-1) {
		t.Fatalf("x is %v after %d stores", got, runs)
	}

	hits, misses := jitStats.putHit-hit0, jitStats.putMiss-miss0
	if hits+misses != runs {
		t.Fatalf("%d hits + %d misses, want %d stores", hits, misses, runs)
	}
	if misses != 1 {
		t.Errorf("%d misses, want exactly the one that fills the cache", misses)
	}
	if hits != runs-1 {
		t.Errorf("%d hits out of %d stores", hits, runs)
	}
}

// TestJITStoresToAnOverflowSlot is the store's half of TestJITReadsAnOverflowSlot:
// the same slice-header offset, and the same four-property ceiling if it is
// wrong. It checks every other property as well, because a store that computes
// the wrong address does not fail — it overwrites a neighbour.
func TestJITStoresToAnOverflowSlot(t *testing.T) {
	was := jitStats.enabled
	jitStats.enabled = true
	t.Cleanup(func() { jitStats.enabled = was })

	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	if len(names) <= jitInobjSlots {
		t.Fatal("this receiver no longer has any properties past the inline area")
	}
	for i, field := range names {
		t.Run(field, func(t *testing.T) {
			rt, fn, c, _ := jitStore(t, field)
			o := rt.newObject(rt.objectProto)
			for j, n := range names {
				rt.objPtr(o).defineOwn(n, tov(float64(j*10)), attrDefault)
			}
			hit0, miss0 := jitStats.putHit, jitStats.putMiss
			const runs = 8
			for k := 0; k < runs; k++ {
				if e := jitPut(t, rt, fn, c, o, tov(float64(1000+k))); e != nil {
					t.Fatalf("store %d threw", k)
				}
			}
			hits, misses := jitStats.putHit-hit0, jitStats.putMiss-miss0
			if misses != 1 || hits != runs-1 {
				t.Errorf("slot %d: %d hits and %d misses over %d stores; the probe declined a slot it should serve",
					i, hits, misses, runs)
			}
			for j, n := range names {
				want := tov(float64(j * 10))
				if j == i {
					want = tov(float64(1000 + runs - 1))
				}
				if got, _ := rt.getField(o, n); got != want {
					t.Errorf("after storing to %q, %q is %v (want %v)", field, n, got, want)
				}
			}
		})
	}
}

// TestJITStoreServesEveryShapeASiteHolds is the read probe's lesson applied
// before it can be learned twice: a site holds up to eight shapes and the scan
// has to consult all of them, because way 0 holds the first shape the site ever
// saw and objects from one literal do not all share it.
func TestJITStoreServesEveryShapeASiteHolds(t *testing.T) {
	was := jitStats.enabled
	jitStats.enabled = true
	t.Cleanup(func() { jitStats.enabled = was })

	rt, fn, c, _ := jitStore(t, "x")
	pts, err := rt.RunString("pts.js", "var pts=[]; for (var i=0;i<100;i++) pts.push({x:i,y:i*2}); pts;")
	if err != nil {
		t.Fatalf("building the receivers: %v", err)
	}
	var objs []Value
	shapes := map[*shape]int{}
	for i := 0; i < 100; i++ {
		o, e := rt.getField(pts, itoa(i))
		if e != nil {
			t.Fatalf("pts[%d]: threw", i)
		}
		objs = append(objs, o)
		shapes[rt.objPtr(o).shape]++
	}
	if len(shapes) < 2 {
		t.Skip("this corpus no longer produces more than one shape; the multi-way scan is untested by it")
	}

	hit0, miss0 := jitStats.putHit, jitStats.putMiss
	for k := 0; k < 50; k++ {
		for i, o := range objs {
			if e := jitPut(t, rt, fn, c, o, tov(float64(i*1000+k))); e != nil {
				t.Fatalf("pts[%d]: threw", i)
			}
		}
	}
	for i, o := range objs {
		got, _ := rt.getField(o, "x")
		if got != tov(float64(i*1000+49)) {
			t.Fatalf("pts[%d].x is %v", i, got)
		}
	}
	hit, miss := jitStats.putHit-hit0, jitStats.putMiss-miss0
	t.Logf("%d stores over %d receivers in %d shapes: %d hit, %d missed",
		hit+miss, len(objs), len(shapes), hit, miss)
	if hit < (hit+miss)*9/10 {
		t.Errorf("the probe served %d of %d stores; a site holding every shape these receivers have should serve nearly all of them",
			hit, hit+miss)
	}
}

// TestJITStoreMarksASharedMutation is the one piece of runtime bookkeeping the
// compiled store has to do itself.
//
// A host that pools Runtimes reuses one only if the run did not modify anything
// that predates it, and the compiled store skips [[Set]] — which is where that
// would otherwise be noticed. Getting this wrong hands a host a Runtime carrying
// the last script's changes, which is not a crash and not a wrong answer until
// much later.
func TestJITStoreMarksASharedMutation(t *testing.T) {
	was := jitStats.enabled
	jitStats.enabled = true
	defer func() { jitStats.enabled = was }()

	rt, fn, c, _ := jitStore(t, "x")
	build := func() Value {
		o := rt.newObject(rt.objectProto)
		rt.objPtr(o).defineOwn("x", tov(0), attrDefault)
		return o
	}
	// Several, because the transition memo gives the first two objects to take a
	// transition a shape each and only shares from the third on. The site has to
	// be warmed on the shape the receiver under test actually has, or the store
	// misses and the runtime path is what gets measured.
	var old Value
	for i := 0; i < 4; i++ {
		old = build()
	}

	inv := rt.BeginInvocation()
	defer inv.End()
	newer := build()
	if rt.objPtr(old).shape != rt.objPtr(newer).shape {
		t.Skip("these receivers no longer share a shape; the site cannot be warmed on one and tested on the other")
	}

	store := func(recv Value, v float64, what string) {
		t.Helper()
		hit0 := jitStats.putHit
		if e := jitPut(t, rt, fn, c, recv, tov(v)); e != nil {
			t.Fatalf("%s: threw", what)
		}
		if jitStats.putHit == hit0 {
			t.Fatalf("%s: the store did not take the emitted path, so it proves nothing", what)
		}
	}

	// Beginning an invocation retires every cache way, so the first store misses
	// and refills. Only the ones after it are the emitted path.
	jitPut(t, rt, fn, c, newer, tov(1))
	store(newer, 2, "an object the invocation made")
	if inv.Dirty() {
		t.Fatal("writing to an object the invocation itself made marked it dirty")
	}

	store(old, 3, "an object older than the invocation")
	if !inv.Dirty() {
		t.Error("writing to an object older than the invocation left it clean")
	}
}

// TestJITStoredValueSurvivesACollection is the collector's side of the store.
//
// A setter is JavaScript, so a store that reaches one can run a collection while
// the compiled frame is suspended in the helper — with the receiver and the
// value sitting in the spill area and nothing else referring to either.
func TestJITStoredValueSurvivesACollection(t *testing.T) {
	rt, fn, c, _ := jitStore(t, "x")

	recv, err := rt.RunString("setter.js", `
		({ kept: null, set x(v) { collect(); this.kept = v; } })`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	collector := rt.newNativeFunc("collect", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		rt.collect()
		return mkundef(), nil
	})
	rt.setField(rt.global, "collect", collector)

	// Made here and referred to by nothing else: if the spill area does not root
	// it, the collection inside the setter frees it and what lands in `kept` is
	// a handle to a cell something else has since taken.
	val := rt.newObject(rt.objectProto)
	rt.objPtr(val).defineOwn("tag", tov(1234), attrDefault)

	if e := jitPut(t, rt, fn, c, recv, val); e != nil {
		t.Fatal("threw")
	}
	kept, e := rt.getField(recv, "kept")
	if e != nil {
		t.Fatal("reading back threw")
	}
	tag, e := rt.getField(kept, "tag")
	if e != nil || tag != tov(1234) {
		t.Errorf("the stored object came back as %v (tag %v)", kept, tag)
	}
}

// BenchmarkJITStore is the emitted store against the interpreter's, over a loop
// rather than a call, so what is measured is the store itself and not the cost
// of entering compiled code.
func BenchmarkJITStore(b *testing.B) {
	const src = "function f(o,n){ var i=0; while (i<n) { o.a=i; o.b=i; o.c=i; o.d=i; i=i+1; } return i; }"
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
			if _, _, ok := c.jitRun(rt, fn, nil, 0, nil, locals, mkundef()); !ok {
				b.Fatal("declined")
			}
		}
		b.ReportMetric(float64(4*iters), "stores/op")
	})
	b.Run("interpreted", func(b *testing.B) {
		// A second Runtime, and therefore a second receiver: a Value carries a
		// handle into the pool of the Runtime that made it.
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
		b.ReportMetric(float64(4*iters), "stores/op")
	})
}
