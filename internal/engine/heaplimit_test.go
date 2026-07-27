package engine

import "testing"

// A script that retains more than its budget must be stopped and reported as a
// memory failure, not left to reach Go's out-of-memory — which is a runtime
// throw the host cannot recover from or report.
func TestHeapLimitStopsARetainingScript(t *testing.T) {
	rt := New()
	rt.SetHeapLimit(8 << 20)
	sc, err := rt.CompileScript("t.js", `
		const held = [];
		for (let i = 0; i < 5000000; i++) held.push({ i: i, s: "x" + i });
		held.length;
	`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := rt.RunScript(sc); err == nil {
		t.Fatal("a script that blew its budget ran to completion")
	}
	if !rt.HeapLimitExceeded() {
		t.Error("stopped, but not reported as a heap-limit failure")
	}
	if _, b := rt.HeapUsage(); b == 0 {
		t.Error("no heap accounted at all; the fixture is not measuring anything")
	}
}

// Churn is not retention. A script that allocates far past the budget but keeps
// nothing must run to completion, or the limit would be unusable in practice.
func TestHeapLimitIgnoresGarbage(t *testing.T) {
	rt := New()
	rt.SetHeapLimit(8 << 20)
	sc, err := rt.CompileScript("t.js", `
		let n = 0;
		for (let i = 0; i < 300000; i++) { const o = { i: i, s: "x" + i }; n += o.i; }
		n;
	`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := rt.RunScript(sc); err != nil {
		t.Fatalf("a script that retained nothing was stopped: %v", err)
	}
	if rt.HeapLimitExceeded() {
		t.Error("reported a heap-limit failure for a script that held nothing")
	}
}

// No budget, no interference.
func TestHeapLimitOffByDefault(t *testing.T) {
	rt := New()
	if rt.HeapLimit() != 0 {
		t.Fatalf("default heap limit is %d, want 0", rt.HeapLimit())
	}
	sc, _ := rt.CompileScript("t.js", `const a = []; for (let i=0;i<200000;i++) a.push({i}); a.length;`)
	if _, err := rt.RunScript(sc); err != nil {
		t.Fatalf("unbudgeted script was stopped: %v", err)
	}
}
