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

// The limit is only worth anything if it fires for every shape of runaway
// memory, not just the one the accounting happened to notice. Each script here
// retains what it makes, and each of the last four used to take the process
// down instead: bytes hanging off a cell — string payloads, array element
// storage — were not counted at all, and the collection that tests the limit
// was triggered by cell COUNT, which none of these shapes reaches. A hundred
// 100 KB strings is 10 MB in a hundred cells.
func TestHeapLimitStopsEveryShapeOfGrowth(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"objectCells", `const k = []; for (;;) k.push({});`},
		{"arrayElements", `const k = []; for (;;) k.push(new Array(256).fill(7));`},
		{"stringBytes", `const k = []; for (;;) k.push("x".repeat(100000));`},
		{"arrayBuffers", `const k = []; for (;;) k.push(new ArrayBuffer(1 << 20));`},
		// Doubling outruns any check made after the fact: the allocation that
		// exhausts the host is the same one the post-sweep test was waiting for.
		{"doublingString", `let s = "x"; for (;;) s += s;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := New()
			rt.SetHeapLimit(8 << 20)
			sc, err := rt.CompileScript("t.js", tc.src)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if _, err := rt.RunScript(sc); err == nil {
				t.Fatal("a script that blew its budget ran to completion")
			}
			if !rt.HeapLimitExceeded() {
				t.Error("stopped, but not reported as a heap-limit failure — a host " +
					"cannot tell this from an interrupt or a timeout")
			}
		})
	}
}

// HeapUsage has to include what the cells point at, or the number a host reads
// is not the number its budget is judged against.
func TestHeapUsageCountsPayloadNotJustCells(t *testing.T) {
	rt := New()
	rt.SetHeapLimit(64 << 20) // set, so the sweep maintains the payload total
	_, before := rt.HeapUsage()
	sc, err := rt.CompileScript("t.js", `globalThis.keep = "x".repeat(4*1024*1024); 1;`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := rt.RunScript(sc); err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.Collect()
	if _, after := rt.HeapUsage(); after-before < 4<<20 {
		t.Errorf("retaining a 4 MB string grew reported usage by %d bytes; "+
			"string payloads are not being counted", after-before)
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
