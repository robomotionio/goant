package engine

import (
	"strings"
	"testing"
)

// run compiles and runs src on rt, returning the completion value as a string.
func runCryptoScript(t *testing.T, rt *Runtime, src string) string {
	t.Helper()
	s, err := rt.CompileScript("c.js", src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	v, err := rt.RunScript(s)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	s2, err := rt.ToString(v)
	if err != nil {
		t.Fatalf("ToString: %v", err)
	}
	return s2
}

// The point of the interface: two calls never agree, and neither do two
// Runtimes. Math.random carries per-Runtime state and so has a seed to get
// wrong; this reads the OS on every call and carries none, which is why no
// arrangement of Runtimes can make it repeat.
func TestRandomUUIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		rt := New()
		for j := 0; j < 256; j++ {
			u := runCryptoScript(t, rt, `crypto.randomUUID()`)
			if len(u) != 36 {
				t.Fatalf("randomUUID returned %q, want 36 characters", u)
			}
			if u[14] != '4' {
				t.Fatalf("randomUUID returned %q, want version 4", u)
			}
			if !strings.ContainsRune("89ab", rune(u[19])) {
				t.Fatalf("randomUUID returned %q, want variant 10xx", u)
			}
			if seen[u] {
				t.Fatalf("randomUUID repeated %q", u)
			}
			seen[u] = true
		}
	}
}

// getRandomValues writes through the view's window and nowhere else. A
// byte-level write that ignored byteOffset would corrupt whatever else shares
// the buffer, which no test of the returned array alone would catch.
func TestGetRandomValuesRespectsTheViewWindow(t *testing.T) {
	rt := New()
	got := runCryptoScript(t, rt, `
		const buf = new ArrayBuffer(16);
		const whole = new Uint8Array(buf);
		crypto.getRandomValues(new Uint8Array(buf, 4, 8));
		const outside = [0,1,2,3,12,13,14,15].every(i => whole[i] === 0);
		const inside = [4,5,6,7,8,9,10,11].some(i => whole[i] !== 0);
		outside + "," + inside
	`)
	if got != "true,true" {
		t.Fatalf("outside-untouched,inside-written = %q, want \"true,true\"", got)
	}
}

// The spec's two refusals, by the message a script would match on.
func TestGetRandomValuesRefusals(t *testing.T) {
	rt := New()
	cases := []struct{ src, want string }{
		{`new Float64Array(4)`, "not an integer array type"},
		{`new Float32Array(4)`, "not an integer array type"},
		{`new DataView(new ArrayBuffer(8))`, "not an integer array type"},
		{`[1,2,3]`, "not of type 'ArrayBufferView'"},
		{`new Uint8Array(65537)`, "exceeds the number of bytes of entropy"},
		{`new Uint16Array(32769)`, "exceeds the number of bytes of entropy"},
	}
	for _, c := range cases {
		got := runCryptoScript(t, rt, `
			try { crypto.getRandomValues(`+c.src+`); "no throw" }
			catch (e) { e.name + ": " + e.message }
		`)
		if !strings.Contains(got, c.want) {
			t.Errorf("getRandomValues(%s) gave %q, want a message containing %q", c.src, got, c.want)
		}
	}
	// The ceiling itself is allowed.
	if got := runCryptoScript(t, rt, `crypto.getRandomValues(new Uint8Array(65536)).length`); got != "65536" {
		t.Errorf("65536 bytes refused: %q", got)
	}
}

// A detached view has zero length, so it is filled with nothing and handed
// back — the same answer every other TypedArray operation gives it, rather
// than a second failure mode for one condition.
func TestGetRandomValuesOnDetachedView(t *testing.T) {
	rt := New()
	got := runCryptoScript(t, rt, `
		const a = new Uint8Array(8);
		a.buffer.transfer();
		crypto.getRandomValues(a) === a && a.length === 0
	`)
	if got != "true" {
		t.Fatalf("detached view: %q, want \"true\"", got)
	}
}

// The embedder half. A host pools Runtimes and reuses one only when the run
// left shared state alone, so a call that fills the script's OWN array must not
// report dirty — a false positive costs a fresh Runtime on every message.
func TestGetRandomValuesDoesNotDirtyOwnArray(t *testing.T) {
	rt := New()
	inv := rt.BeginInvocation()
	runCryptoScript(t, rt, `
		crypto.randomUUID();
		crypto.getRandomValues(new Uint8Array(64));
	`)
	if inv.Dirty() {
		t.Fatal("filling the run's own array marked the Runtime dirty")
	}
	if !inv.Release() {
		t.Fatal("Release refused a run that only touched its own state")
	}
}

// The other half, and the reason the write cannot simply memcpy: a view that
// predates the invocation is shared state, and filling it has to be reported
// exactly as an element store would report it. Without this the host rewinds a
// region whose bytes the next run would still be reading.
func TestGetRandomValuesDirtiesAPreExistingArray(t *testing.T) {
	rt := New()
	shared, terr := rt.newTypedArray(taUint8, []Value{mknum(8)})
	if terr != nil {
		t.Fatalf("newTypedArray: %v", terr)
	}
	rt.objPtr(rt.global).defineOwn("shared", shared, attrWritable|attrConfigurable)

	inv := rt.BeginInvocation()
	runCryptoScript(t, rt, `crypto.getRandomValues(shared)`)
	if !inv.Dirty() {
		t.Fatal("filling a view older than the invocation went unreported")
	}
	if inv.Release() {
		t.Fatal("Release freed a run that had written to shared bytes")
	}
}
