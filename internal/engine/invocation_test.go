package engine

import (
	"strings"
	"testing"
)

// runIn runs src inside a fresh invocation and returns its completion value as
// a string, so each case reads as "what does the next run see".
func runIn(t *testing.T, rt *Runtime, src string) string {
	t.Helper()
	inv := rt.BeginInvocation()
	defer inv.End()
	s, err := rt.CompileScript("inv.js", src)
	if err != nil {
		t.Fatalf("compile %q: %v", src, err)
	}
	v, err := rt.RunScript(s)
	if err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	got, err := rt.ToString(v)
	if err != nil {
		t.Fatalf("tostring %q: %v", src, err)
	}
	return got
}

// The whole point: what one run installs must not be visible to the next.
// Each case writes in one invocation and reads in another.
func TestInvocationIsolatesGlobalState(t *testing.T) {
	cases := []struct{ write, read, want string }{
		{`globalThis.x = 1;`, `typeof globalThis.x`, "undefined"},
		{`implicitGlobal = 1;`, `typeof implicitGlobal`, "undefined"},
		{`var v = 1;`, `typeof v`, "undefined"},
		{`function f() {}`, `typeof f`, "undefined"},
		{`let l = 1;`, `typeof l`, "undefined"},
		{`const c = 1;`, `typeof c`, "undefined"},
		{`class K {}`, `typeof K`, "undefined"},
	}
	for _, tc := range cases {
		rt := New()
		runIn(t, rt, tc.write)
		if got := runIn(t, rt, tc.read); got != tc.want {
			t.Errorf("after %q, %q = %q, want %q — state leaked between invocations",
				tc.write, tc.read, got, tc.want)
		}
	}
}

// Re-declaring a top-level lexical binding in a later invocation must work.
// If the declarative environment were not cleared this would be a redeclaration
// error, which is a subtler failure than a stale read.
func TestInvocationAllowsRedeclaration(t *testing.T) {
	rt := New()
	for i := 0; i < 3; i++ {
		if got := runIn(t, rt, `let x = 7; x`); got != "7" {
			t.Fatalf("run %d: got %q", i, got)
		}
	}
}

// Builtins must remain fully reachable and usable through the inherited chain —
// this is what makes skipping the realm rebuild sound rather than merely fast.
func TestInvocationBuiltinsReachable(t *testing.T) {
	rt := New()
	checks := []struct{ src, want string }{
		{`typeof JSON`, "object"},
		{`typeof Array`, "function"},
		{`typeof Promise`, "function"},
		{`typeof Map`, "function"},
		{`[3,1,2].sort().join(",")`, "1,2,3"},
		{`JSON.stringify({a:[1,{b:2}]})`, `{"a":[1,{"b":2}]}`},
		{`JSON.parse('{"k":[1,2]}').k[1]`, "2"},
		{`[...new Set([1,1,2])].join(",")`, "1,2"},
		{`(() => "arrow")()`, "arrow"},
		{`"abc".toUpperCase()`, "ABC"},
		{`new Date(0).toISOString()`, "1970-01-01T00:00:00.000Z"},
		{`/a(b)c/.exec("abc")[1]`, "b"},
		{`String(Object.getPrototypeOf([]) === Array.prototype)`, "true"},
		{`String(({}) instanceof Object)`, "true"},
		{`typeof globalThis`, "object"},
		{`String(globalThis === globalThis.globalThis)`, "true"},
	}
	for _, c := range checks {
		if got := runIn(t, rt, c.src); got != c.want {
			t.Errorf("%s = %q, want %q", c.src, got, c.want)
		}
	}
}

// A run must still see its own writes while it is running — isolation is
// between invocations, not within one.
func TestInvocationSeesItsOwnState(t *testing.T) {
	rt := New()
	got := runIn(t, rt, `
		globalThis.a = 1;
		var b = 2;
		let c = 3;
		function d() { return 4; }
		[a, b, c, d(), typeof globalThis.a].join(",")
	`)
	if got != "1,2,3,4,number" {
		t.Fatalf("got %q", got)
	}
}

// Values created in one invocation stay valid afterwards: the host reads the
// result after End, and pooled handles are not recycled by ending a run.
func TestInvocationResultOutlivesIt(t *testing.T) {
	rt := New()
	inv := rt.BeginInvocation()
	s, err := rt.CompileScript("r.js", `({ok: [1,2,3]})`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunScript(s)
	if err != nil {
		t.Fatal(err)
	}
	inv.End()

	out, ok, err := rt.JSONStringifyToBytes(v, nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if string(out) != `{"ok":[1,2,3]}` {
		t.Fatalf("got %s", out)
	}
}

// End must be idempotent, and a nil Invocation must not panic — a host with a
// deferred End on an error path will do both.
func TestInvocationEndIsIdempotent(t *testing.T) {
	rt := New()
	before := rt.Global()
	inv := rt.BeginInvocation()
	inv.End()
	inv.End()
	if rt.Global() != before {
		t.Fatal("the shared global was not restored")
	}
	var nilInv *Invocation
	nilInv.End()
	if nilInv.Global() != mkundef() {
		t.Fatal("a nil invocation should report undefined")
	}
}

// A script that throws must still leave the runtime clean for the next run.
func TestInvocationSurvivesAThrow(t *testing.T) {
	rt := New()
	func() {
		inv := rt.BeginInvocation()
		defer inv.End()
		s, err := rt.CompileScript("t.js", `globalThis.partial = 1; throw new Error("boom")`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RunScript(s); err == nil {
			t.Fatal("expected a throw")
		} else if !strings.Contains(err.Error(), "boom") {
			t.Fatalf("unexpected error: %v", err)
		}
	}()
	if got := runIn(t, rt, `typeof globalThis.partial`); got != "undefined" {
		t.Fatalf("state from a throwing run leaked: %q", got)
	}
}

// Host values installed on the invocation's global are visible to the script
// and gone afterwards — this is how a message is handed in.
func TestInvocationHostGlobals(t *testing.T) {
	rt := New()
	inv := rt.BeginInvocation()
	if err := rt.SetProp(inv.Global(), "__inMsg__", rt.NewStringData(`{"n":5}`)); err != nil {
		t.Fatal(err)
	}
	s, err := rt.CompileScript("h.js", `JSON.parse(globalThis.__inMsg__).n`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunScript(s)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := rt.ToNumber(v); n != 5 {
		t.Fatalf("got %v", n)
	}
	inv.End()

	if got := runIn(t, rt, `typeof globalThis.__inMsg__`); got != "undefined" {
		t.Fatalf("host global leaked: %q", got)
	}
}
