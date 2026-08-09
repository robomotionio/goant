package engine

import (
	"fmt"
	"testing"
)

// What it costs to call a JavaScript function once per element.
//
// A Function node in the field is mostly `msg.records.forEach(...)` and
// `.map(...)`, and a decomposition of one over a million records put 157ms of a
// 434ms call in the per-element callback alone — against 18.6ms for V8 doing
// the same work. The number that explains it is not time but allocation: two Go
// allocations per element, where V8 has none.
//
// allocs/op here is per WHOLE call over callbackElems elements, so divide by
// that for the per-element figure this is really about. Time is reported too,
// but allocation is the one that compounds: a robot runs for weeks, and every
// one of these is something the collector has to walk later.
const callbackElems = 1000

func benchArrayCallback(b *testing.B, body string) {
	rt := New()
	src := fmt.Sprintf(`
		globalThis.a = new Array(%d);
		for (let i = 0; i < a.length; i++) a[i] = i;
		globalThis.run = function () { %s };
	`, callbackElems, body)
	if _, err := rt.RunString("callback-alloc.js", src); err != nil {
		b.Fatalf("setup: %v", err)
	}
	run, e := rt.getField(rt.global, "run")
	if e != nil {
		b.Fatalf("run: %v", e)
	}
	// Warm up outside the timed window: the tier compiles on entry count, and
	// what is being measured is the steady state and not the compile.
	for i := 0; i < 64; i++ {
		if _, e := rt.callValue(run, mkundef(), nil); e != nil {
			b.Fatalf("warm-up: %v", e)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := rt.callValue(run, mkundef(), nil); e != nil {
			b.Fatalf("call: %v", e)
		}
	}
}

// The array iteration methods that take a callback, which is nearly all of what
// customer JavaScript does to a message.
func BenchmarkArrayCallback(b *testing.B) {
	cases := []struct{ name, body string }{
		{"forEach", `let s = 0; a.forEach(function (x) { s += x; }); return s;`},
		{"map", `return a.map(function (x) { return x + 1; });`},
		{"filter", `return a.filter(function (x) { return x > 500; });`},
		{"reduce", `return a.reduce(function (t, x) { return t + x; }, 0);`},
		{"some", `return a.some(function (x) { return x < 0; });`},
		// The control: the same work with no callback at all. Whatever separates
		// this from forEach is what calling a function per element costs, and
		// nothing else.
		{"for-loop", `let s = 0; for (let i = 0; i < a.length; i++) s += a[i]; return s;`},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) { benchArrayCallback(b, c.body) })
	}
}
