//go:build amd64 || arm64

package engine

import (
	"reflect"
	"testing"
)

// A compiled call site caches what it last called: the callee's handle, its
// machine entry, and the address of its closure's upvalue array. None of that
// may be a garbage-collection root, and the reason it may not is worth stating
// twice, because the collector's reflective walk reached it by accident and
// nobody had asked whether it should.
//
// Found by goant-soak under GOANT_GC_POISON, and only after the string-cell
// trigger (stringsFull) started collecting at moments the object-count trigger
// never chose: a callee dropped while its caller was idle was swept, because
// the walk reaches a site only through Runtime.frames and the caller was not on
// the stack; the next collection with that caller running then marked a cell
// that no longer existed. Intermittent by construction — the same run is clean
// or not depending on which function happens to be executing.

// TestACompiledCallSiteIsNotACollectorRoot is the invariant the exclusion
// states. It is a one-line assertion because the bug was a one-line omission:
// jitCallSite holds a Value, so the walk's "can this type reach a Value" test
// said yes and traced it.
func TestACompiledCallSiteIsNotACollectorRoot(t *testing.T) {
	if canHoldValue(jitCallSiteType) {
		t.Fatal("the reflective walk traces a call site's cached callee; " +
			"that makes it an intermittent root and, under GC poison, a panic in the collector")
	}
	// The exclusion has to be the whole struct and not the Value field alone —
	// entry and upvals are raw addresses into the callee, and they are retired
	// by the same epoch.
	if !canHoldValue(reflect.TypeOf(Value(0))) {
		t.Fatal("canHoldValue no longer recognises a Value at all; the test above proves nothing")
	}
}

// TestACollectionRetiresEveryCallSite is what makes the exclusion SOUND rather
// than merely convenient. Not tracing the cache is safe only because no compiled
// call can use it after a collection: the emitted guard compares the site's
// epoch against the global counter, and collect() bumps that counter. If this
// test ever fails, the exclusion above becomes a use-after-free in machine code.
func TestACollectionRetiresEveryCallSite(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	rt := New()
	if _, err := rt.RunString("sites.js", `
		function callee(a) { return a + 1; }
		function caller(n) { let s = 0; for (let i = 0; i < n; i++) s = callee(s); return s; }
		// caller has to be ENTERED past the tier's threshold, not merely loop a
		// lot: the counter that compiles is on entry.
		for (let k = 0; k < 200; k++) caller(20);
	`); err != nil {
		t.Fatalf("run: %v", err)
	}
	v, err := rt.RunString("sites2.js", `caller`)
	if err != nil {
		t.Fatal(err)
	}
	o := rt.objPtr(v)
	if o == nil || o.clPtr == nil || o.clPtr.fn.jit.code == nil {
		t.Skip("caller did not compile on this build, so there is no site to retire")
	}
	sites := o.clPtr.fn.jit.code.sites
	if len(sites) == 0 {
		t.Skip("caller compiled with no call site")
	}
	var bound *jitCallSite
	for i := range sites {
		if sites[i].callee != 0 {
			bound = &sites[i]
			break
		}
	}
	if bound == nil {
		t.Skip("no site was filled, so there is nothing to retire")
	}
	if bound.epoch != icEpoch() {
		t.Fatalf("a freshly filled site is already retired (epoch %d, current %d)", bound.epoch, icEpoch())
	}

	rt.Collect()

	if bound.epoch == icEpoch() {
		t.Fatal("a collection left a call site live: its cached callee handle may name a recycled " +
			"cell, and machine code would call it")
	}
}
