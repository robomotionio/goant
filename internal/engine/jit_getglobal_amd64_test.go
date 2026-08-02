//go:build amd64

package engine

import "testing"

// The compiled global read, checked against the interpreter.
//
// A global read is the same cache as a field read over a receiver compiled code
// fetches for itself, so most of what it can get wrong is already covered by
// the field tests. What is not covered is everything that makes a global read
// *not* a property read: a Script-level `let` shadows a global property and is
// not part of any shape; an undeclared name is a ReferenceError rather than
// undefined; and BeginInvocation swaps the global object out from under a site
// that has already been warmed on the old one.

// jitGlobal compiles `function f(){ return NAME; }` in a Runtime that has
// already run setup.
func jitGlobal(t testing.TB, setup, name string) (*Runtime, *svFunc, *jitCode) {
	t.Helper()
	src := "function f(){ return " + name + "; }"
	rt, fn := jitFnRT(t, src)
	if setup != "" {
		if _, err := rt.RunString("setup.js", setup); err != nil {
			t.Fatalf("setup %q: %v", setup, err)
		}
	}
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatalf("refused to compile %q", src)
	}
	t.Cleanup(c.free)
	return rt, fn, c
}

func jitReadGlobal(t testing.TB, rt *Runtime, fn *svFunc, c *jitCode) (Value, *ThrowError) {
	t.Helper()
	locals := make([]Value, fn.maxLocals)
	v, e, ok := c.jitRun(rt, fn, nil, locals, mkundef())
	if !ok {
		t.Fatal("compiled code declined a frame it should have handled")
	}
	return v, e
}

// TestJITGlobalAgreesWithTheInterpreter runs each case eight times, because the
// first read fills the site and only the ones after it take the emitted probe —
// a test that ran once would check the runtime path and call it a pass.
func TestJITGlobalAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		read  string
		throw bool
	}{
		{"a-var", "var g = 42;", "g", false},
		{"a-function", "function g(){ return 1; }", "g", false},
		{"a-builtin", "", "Math", false},
		{"assigned-later", "var g = 1; g = 'two';", "g", false},
		{"holds-undefined", "var g;", "g", false},
		{"undeclared", "", "nope", true},
		{"deleted", "g = 1; delete globalThis.g;", "g", true},
		{"a-getter", "Object.defineProperty(globalThis, 'g', {get: function(){ return 7; }});", "g", false},
		{"shadowed-by-let", "var g = 1; ", "g", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, fn, c := jitGlobal(t, tc.setup, tc.read)
			for i := 0; i < 8; i++ {
				want, wantErr := rt.RunString("ref.js", tc.read+";")
				got, gotErr := jitReadGlobal(t, rt, fn, c)
				if (gotErr != nil) != tc.throw {
					t.Fatalf("run %d: threw %v, want %v", i, gotErr != nil, tc.throw)
				}
				if (wantErr != nil) != (gotErr != nil) {
					t.Fatalf("run %d: compiled threw %v, interpreter threw %v",
						i, gotErr != nil, wantErr != nil)
				}
				if gotErr == nil && uint64(got) != uint64(want) {
					t.Fatalf("run %d: compiled %#x (%v), interpreter %#x (%v)",
						i, uint64(got), got.Type(), uint64(want), want.Type())
				}
			}
		})
	}
}

// TestJITGlobalSeesALexicalBinding is the guard that is not a shape.
//
// A Script-level let or const shadows a global property of the same name, and
// the declarative record it lives in is nowhere in the global object's shape. A
// site warmed on the property must stop answering the moment the binding is
// registered — which is the cache epoch's job, not the shape check's.
func TestJITGlobalSeesALexicalBinding(t *testing.T) {
	rt, fn, c := jitGlobal(t, "globalThis.g = 'property';", "g")
	for i := 0; i < 8; i++ {
		got, e := jitReadGlobal(t, rt, fn, c)
		if e != nil || string(rt.strBytes(got)) != "property" {
			t.Fatalf("run %d read %v", i, got)
		}
	}
	if _, err := rt.RunString("let.js", "let g = 'binding';"); err != nil {
		t.Fatalf("declaring the binding: %v", err)
	}
	got, e := jitReadGlobal(t, rt, fn, c)
	if e != nil {
		t.Fatal("threw after the binding was declared")
	}
	if s := string(rt.strBytes(got)); s != "binding" {
		t.Errorf("read %q; a warmed site kept answering with the shadowed property", s)
	}
}

// TestJITGlobalThrowsInTheDeadZone is the same shadowing one step earlier: the
// binding exists but has not been initialised, and reading it is a ReferenceError
// rather than the property it shadows.
func TestJITGlobalThrowsInTheDeadZone(t *testing.T) {
	rt, fn, c := jitGlobal(t, "globalThis.g = 1;", "g")
	for i := 0; i < 8; i++ {
		if _, e := jitReadGlobal(t, rt, fn, c); e != nil {
			t.Fatalf("run %d threw before the binding existed", i)
		}
	}
	// A binding registered but not yet initialised: `let` in a block that has
	// not run its initialiser is the same state a hoisted Script-level let is
	// in before its declaration is reached.
	if _, err := rt.RunString("tdz.js", "let g;"); err != nil {
		t.Fatalf("declaring: %v", err)
	}
	// `let g;` initialises to undefined, so this must now read undefined rather
	// than the property.
	got, e := jitReadGlobal(t, rt, fn, c)
	if e != nil {
		t.Fatal("threw")
	}
	if got != mkundef() {
		t.Errorf("read %v, want undefined from the lexical binding", got)
	}
}

// TestJITGlobalFollowsTheInvocation is the receiver changing underneath the
// site.
//
// BeginInvocation gives the run a fresh global object whose prototype is the
// shared one, so a site warmed before it is warmed on a different object
// entirely. Reading the runtime's global on every probe rather than baking one
// in is what makes that safe; the epoch bump is what makes it correct.
func TestJITGlobalFollowsTheInvocation(t *testing.T) {
	rt, fn, c := jitGlobal(t, "var g = 'outer';", "g")
	for i := 0; i < 8; i++ {
		got, e := jitReadGlobal(t, rt, fn, c)
		if e != nil || string(rt.strBytes(got)) != "outer" {
			t.Fatalf("run %d read %v", i, got)
		}
	}

	inv := rt.BeginInvocation()
	if _, err := rt.RunString("inv.js", "var g = 'inner';"); err != nil {
		t.Fatalf("invocation setup: %v", err)
	}
	for i := 0; i < 8; i++ {
		got, e := jitReadGlobal(t, rt, fn, c)
		if e != nil {
			t.Fatalf("run %d threw inside the invocation", i)
		}
		if s := string(rt.strBytes(got)); s != "inner" {
			t.Fatalf("run %d read %q inside the invocation", i, s)
		}
	}
	inv.End()

	got, e := jitReadGlobal(t, rt, fn, c)
	if e != nil {
		t.Fatal("threw after the invocation ended")
	}
	if s := string(rt.strBytes(got)); s != "outer" {
		t.Errorf("read %q after the invocation ended; the site kept the fresh global", s)
	}
}

// TestJITGlobalProbeActuallyRuns is what stops the agreement above from being
// agreement between the runtime and itself.
func TestJITGlobalProbeActuallyRuns(t *testing.T) {
	was := jitStats.enabled
	jitStats.enabled = true
	defer func() { jitStats.enabled = was }()
	hit0, miss0 := jitStats.glbHit, jitStats.glbMiss

	rt, fn, c := jitGlobal(t, "var g = 42;", "g")
	const runs = 32
	for i := 0; i < runs; i++ {
		got, e := jitReadGlobal(t, rt, fn, c)
		if e != nil || got != tov(42) {
			t.Fatalf("run %d returned %v", i, got)
		}
	}
	hits, misses := jitStats.glbHit-hit0, jitStats.glbMiss-miss0
	if hits+misses != runs {
		t.Fatalf("%d hits + %d misses, want %d reads", hits, misses, runs)
	}
	if misses != 1 {
		t.Errorf("%d misses, want exactly the one that fills the cache", misses)
	}
}

// TestJITGlobalReadsAGetterEveryTime pins the case a slot read would get
// silently wrong: the slot behind an accessor holds undefined, so a site that
// served it would replace every getter call with undefined.
func TestJITGlobalReadsAGetterEveryTime(t *testing.T) {
	rt, fn, c := jitGlobal(t,
		"var n = 0; Object.defineProperty(globalThis, 'g', {get: function(){ return ++n; }});", "g")
	for i := 1; i <= 8; i++ {
		got, e := jitReadGlobal(t, rt, fn, c)
		if e != nil {
			t.Fatal("threw")
		}
		if got != tov(float64(i)) {
			t.Fatalf("read %d returned %v, want %d — the getter was not run", i, got, i)
		}
	}
}
