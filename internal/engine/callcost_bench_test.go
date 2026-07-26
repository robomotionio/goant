package engine

import "testing"

// Where does a script invocation's time actually go?
//
// The robot's workload is not a long-running program; it is an enormous number
// of very short ones. A passthrough call — parse a small message, hand it to a
// function, serialise the result — measured 613 µs on goant and 304 µs on V8.
// For what is semantically "copy 200 bytes" that is essentially all fixed
// overhead, so the question worth answering is which fixed cost dominates, not
// how fast the interpreter runs.
//
// These break the per-call cost into its parts so the answer is measured rather
// than assumed.

const smallMsg = `{"items":[{"id":"a","v":1},{"id":"b","v":2},{"id":"c","v":3},` +
	`{"id":"d","v":4},{"id":"e","v":5}]}`

// A whole isolate: every prototype and builtin built from nothing.
func BenchmarkCostNewRuntime(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = New()
	}
}

// A fresh realm on an existing isolate — what a per-call Context costs. It
// shares the pools and the intern table, but still rebuilds every global.
func BenchmarkCostNewRealm(b *testing.B) {
	rt := New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = rt.NewRealm()
	}
}

// Compiling the wrapper. Cached in practice, measured for scale.
func BenchmarkCostCompile(b *testing.B) {
	rt := New()
	src := `(function (msg) { return msg; })`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.CompileScript("w.js", src); err != nil {
			b.Fatal(err)
		}
	}
}

// Running an already-compiled trivial script.
func BenchmarkCostRunTrivial(b *testing.B) {
	rt := New()
	s, err := rt.CompileScript("w.js", `1`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.RunScript(s); err != nil {
			b.Fatal(err)
		}
	}
}

// Parsing the message.
func BenchmarkCostJSONParse(b *testing.B) {
	rt := New()
	msg := []byte(smallMsg)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.JSONParseBytes(msg); err != nil {
			b.Fatal(err)
		}
	}
}

// Serialising the result.
func BenchmarkCostJSONStringify(b *testing.B) {
	rt := New()
	v, err := rt.JSONParseBytes([]byte(smallMsg))
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 0, 512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := rt.JSONStringifyToBytes(v, buf[:0]); err != nil {
			b.Fatal(err)
		}
	}
}

// Calling an already-compiled function value, which is what a wrapper that is
// compiled once and invoked per message would cost.
func BenchmarkCostCallFunction(b *testing.B) {
	rt := New()
	s, err := rt.CompileScript("w.js", `(function (msg) { return msg; })`)
	if err != nil {
		b.Fatal(err)
	}
	fn, err := rt.RunScript(s)
	if err != nil {
		b.Fatal(err)
	}
	msg, err := rt.JSONParseBytes([]byte(smallMsg))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.Call(fn, rt.Undefined(), []Value{msg}); err != nil {
			b.Fatal(err)
		}
	}
}

// The whole thing the way it is done today: fresh realm, compile-once script
// run in it, JSON in, JSON out.
func BenchmarkCostFullCallFreshRealm(b *testing.B) {
	root := New()
	src := `(function () { var msg = JSON.parse(globalThis.__inMsg__); return JSON.stringify([JSON.stringify(msg)]); })()`
	msg := smallMsg
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt := root.NewRealm()
		s, err := rt.CompileScript("w.js", src)
		if err != nil {
			b.Fatal(err)
		}
		if err := rt.SetProp(rt.Global(), "__inMsg__", rt.NewStringData(msg)); err != nil {
			b.Fatal(err)
		}
		if _, err := rt.RunScript(s); err != nil {
			b.Fatal(err)
		}
	}
}

// The same work with the realm reused and the script compiled once: what is
// left when the per-call rebuild is removed.
func BenchmarkCostFullCallReusedRealm(b *testing.B) {
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
	}
}
