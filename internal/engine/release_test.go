package engine

import (
	"strings"
	"testing"
)

// Release is the memory answer for this workload: a run allocates a message
// graph, produces a result, and everything it made dies together. These check
// the two things that make it usable — that memory actually stays flat, and
// that a run which could still be reachable is refused rather than freed.

// The headline: memory must not grow with the number of messages processed.
func TestReleaseKeepsMemoryFlat(t *testing.T) {
	rt := New()
	s, err := rt.CompileScript("w.js", `(function (msg) {
		return { n: msg.items.length, tags: msg.items.map(function (x) { return x.id + "!"; }) };
	})`)
	if err != nil {
		t.Fatal(err)
	}
	fn, err := rt.RunScript(s)
	if err != nil {
		t.Fatal(err)
	}
	in := []byte(smallMsg)
	buf := make([]byte, 0, 1024)

	run := func() string {
		inv := rt.BeginInvocation()
		msg, err := rt.JSONParseBytes(in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := rt.Call(fn, rt.Undefined(), []Value{msg})
		if err != nil {
			t.Fatal(err)
		}
		// The result is extracted to bytes BEFORE releasing, which is the whole
		// contract: after Release every Value from this run is invalid.
		got, ok, err := rt.JSONStringifyToBytes(out, buf[:0])
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		res := string(got)
		if !inv.Release() {
			t.Fatal("a clean invocation refused to release")
		}
		return res
	}

	const want = `{"n":5,"tags":["a!","b!","c!","d!","e!"]}`
	if got := run(); got != want {
		t.Fatalf("got %s", got)
	}
	_, baseline := rt.HeapUsage()

	for i := 0; i < 20000; i++ {
		if got := run(); got != want {
			t.Fatalf("run %d produced %s", i, got)
		}
	}
	_, after := rt.HeapUsage()

	if after > baseline {
		t.Fatalf("memory grew over 20000 released invocations: %d -> %d bytes", baseline, after)
	}
	t.Logf("20000 invocations, occupancy %d -> %d bytes", baseline, after)
}

// A run that wrote to shared state may still be reachable from it, so releasing
// would free memory something else points at. It must refuse.
func TestReleaseRefusesAfterSharedMutation(t *testing.T) {
	rt := New()
	inv := rt.BeginInvocation()
	s, err := rt.CompileScript("p.js", `Array.prototype.leak = {kept: 1}; "ok"`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunScript(s); err != nil {
		t.Fatal(err)
	}
	if inv.Release() {
		t.Fatal("released an invocation that wrote to a shared prototype — the " +
			"object it stored there would have been freed underneath it")
	}
	// And the runtime must still work, having fallen back to a plain End.
	inv2 := rt.BeginInvocation()
	s2, err := rt.CompileScript("q.js", `[].leak.kept`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunScript(s2)
	if err != nil {
		t.Fatalf("the object stored on the shared prototype did not survive: %v", err)
	}
	if n, _ := rt.ToNumber(v); n != 1 {
		t.Fatalf("got %v, want 1", n)
	}
	inv2.End()
}

// Interning during a run adds to a table that outlives it. Those entries must
// be rolled back, or the table points at freed cells — and the next run that
// interns the same text would get a handle to a recycled object.
func TestReleaseRollsBackInterning(t *testing.T) {
	rt := New()
	before := rt.InternedCount()

	for i := 0; i < 200; i++ {
		inv := rt.BeginInvocation()
		s, err := rt.CompileScript("i.js", `
			var o = {};
			o["key" + Math.floor(Math.random() * 0 + 7)] = "v";
			Object.keys(o).join("")
		`)
		if err != nil {
			t.Fatal(err)
		}
		v, err := rt.RunScript(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := rt.ToString(v)
		if err != nil {
			t.Fatal(err)
		}
		if got != "key7" {
			t.Fatalf("run %d: got %q", i, got)
		}
		inv.Release()
	}

	if after := rt.InternedCount(); after != before {
		t.Fatalf("intern table grew across released invocations: %d -> %d", before, after)
	}
}

// Successive released runs must not see each other's data — the recycled cells
// must genuinely be reinitialised, not merely handed back.
func TestReleasedRunsSeeCleanState(t *testing.T) {
	rt := New()
	for i := 0; i < 500; i++ {
		inv := rt.BeginInvocation()
		s, err := rt.CompileScript("r.js", `
			if (typeof globalThis.carried !== "undefined") { "LEAK:" + globalThis.carried }
			else { globalThis.carried = "run"; var a = [1,2,3].map(x => x * 2); a.join(",") }
		`)
		if err != nil {
			t.Fatal(err)
		}
		v, err := rt.RunScript(s)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := rt.ToString(v)
		if strings.HasPrefix(got, "LEAK:") {
			t.Fatalf("run %d saw state from a previous run: %s", i, got)
		}
		if got != "2,4,6" {
			t.Fatalf("run %d: got %q", i, got)
		}
		inv.Release()
	}
}

// Release must be safe to call twice and on a nil invocation, since a host will
// have it on a defer.
func TestReleaseIsIdempotent(t *testing.T) {
	rt := New()
	inv := rt.BeginInvocation()
	if !inv.Release() {
		t.Fatal("first release failed")
	}
	if inv.Release() {
		t.Fatal("second release should report nothing to do")
	}
	var nilInv *Invocation
	if nilInv.Release() {
		t.Fatal("nil release should report false")
	}
	// The runtime must still be usable.
	inv2 := rt.BeginInvocation()
	s, _ := rt.CompileScript("x.js", `6*7`)
	v, err := rt.RunScript(s)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := rt.ToNumber(v); n != 42 {
		t.Fatalf("got %v", n)
	}
	inv2.End()
}
