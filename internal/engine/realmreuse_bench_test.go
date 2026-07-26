package engine

import "testing"

// Realm construction is 91% of a short invocation's cost, so the question is
// whether it can be avoided without giving up the isolation it buys.
//
// A per-call realm exists so one invocation cannot see the next one's globals.
// But a realm is a whole universe — every prototype, every builtin, 885
// allocations — and almost none of that is what needs isolating. What needs
// isolating is the global object's own properties: `globalThis.x = 1`, and the
// var/function bindings a script installs.
//
// So: keep one realm, and give each invocation a fresh global object whose
// prototype is the shared one. Builtins resolve up the chain; assignments land
// on the fresh object and are dropped when it is. These measure whether that is
// as cheap as it sounds, and a correctness test alongside checks it isolates.

func BenchmarkRealmFreshGlobal(b *testing.B) {
	rt := New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt.BeginInvocation().End()
	}
}

// The full invocation with a fresh global per call: parse, call, serialise,
// with globals isolated.
func BenchmarkCostFullCallFreshGlobal(b *testing.B) {
	rt := New()
	s, err := rt.CompileScript("w.js", `(function (msg) { return msg; })`)
	if err != nil {
		b.Fatal(err)
	}
	fn, err := rt.RunScript(s)
	if err != nil {
		b.Fatal(err)
	}
	in := []byte(smallMsg)
	buf := make([]byte, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		inv := rt.BeginInvocation()
		msg, err := rt.JSONParseBytes(in)
		if err != nil {
			b.Fatal(err)
		}
		out, err := rt.Call(fn, rt.Undefined(), []Value{msg})
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := rt.JSONStringifyToBytes(out, buf[:0]); err != nil {
			b.Fatal(err)
		}
		inv.End()
	}
}
