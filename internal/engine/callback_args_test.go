package engine

import "testing"

// The array iteration methods share one argument buffer across a whole call
// rather than allocating one per element, and callback_args.go decides from the
// CALLEE whether that is safe.
//
// These do NOT prove the gate is load-bearing, and saying so is the point.
// Every one of them was written to fail with the gate deliberately widened to
// admit async functions and generators, and every one of them still passed:
// `arguments` is materialised at frame entry, before a shared buffer could be
// rewritten. The gate is kept anyway because newGenState retains the slice
// rather than copying it, and "the arguments object happens to be built early"
// is not a lifetime rule.
//
// What they do pin is the OBSERVABLE contract — each callback sees its own
// element, its own index, and an arguments object that keeps its own copy —
// across the four shapes most likely to break if buffer reuse is ever extended.
func TestCallbackArgsAreNotSharedWithCalleesThatKeepThem(t *testing.T) {
	cases := []struct{ name, src string }{
		// An async function suspends holding the arguments it was called with.
		// Its parameters were copied into locals at entry, so those are safe --
		// but `arguments` is built where the expression appears, which here is
		// AFTER the await, by which time a shared buffer holds the last
		// element. forEach does not await, so all three continuations resume
		// long after the loop has finished.
		{"async callback reading arguments after await", `
			var out = [];
			[10, 20, 30].forEach(async function () {
				await null;
				out.push(arguments[0]);
			});
			// Two drains: each continuation queues from a distinct microtask.
			Promise.resolve().then(() => {}).then(() => {});
			out;
		`},
		// Calling a generator function builds the generator and runs none of the
		// body, so every `arguments` here is read after the loop is over.
		{"generator callback reading arguments when advanced", `
			var gs = [10, 20, 30].map(function* () { yield arguments[0]; });
			var out = gs.map(g => g.next().value);
			out;
		`},
		// The ordinary case, which IS shared: an arguments object copies each
		// value into a property of its own at the point it is created, so three
		// of them hold three different values even though one buffer produced
		// them. This is the claim the sharing rests on.
		{"ordinary callback capturing arguments", `
			var seen = [];
			[10, 20, 30].forEach(function () { seen.push(arguments); });
			var out = seen.map(a => a[0]);
			out;
		`},
		// The same claim with the READ deferred as long as JavaScript allows:
		// an arrow closes over the enclosing arguments and is not called until
		// the loop is finished and the buffer has been rewritten twice.
		{"arrow closing over arguments, read after the loop", `
			var fs = [];
			[10, 20, 30].forEach(function () { fs.push(() => arguments[0]); });
			var out = fs.map(f => f());
			out;
		`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := New()
			v, err := rt.RunString("cb.js", c.src)
			if err != nil {
				t.Fatalf("%v", err)
			}
			rt.DrainJobs()
			out, e := rt.getField(rt.global, "out")
			if e != nil {
				t.Fatalf("out: %v", e)
			}
			if out.IsUndefined() {
				out = v
			}
			for i, want := range []float64{10, 20, 30} {
				got, e := rt.getElement(out, mknum(float64(i)))
				if e != nil {
					t.Fatalf("out[%d]: %v", i, e)
				}
				if !got.IsNumber() || got.Number() != want {
					t.Fatalf("out[%d] = %v, want %v — the argument buffer was shared with a callee that kept it",
						i, rt.inspect(got, false), want)
				}
			}
		})
	}
}

// The three-argument shape is what every iteration method passes, and sharing a
// buffer must not change what the callback sees while it runs.
func TestCallbackArgsSeeTheirOwnElement(t *testing.T) {
	rt := New()
	v, err := rt.RunString("cb.js", `
		var log = [];
		["a", "b"].forEach(function (el, i, arr) {
			log.push(el + ":" + i + ":" + arr.length);
		});
		[1, 2].reduce(function (acc, el, i, arr) {
			log.push(acc + "/" + el + "/" + i + "/" + arr.length);
			return acc + el;
		}, 0);
		log.join("|");
	`)
	if err != nil {
		t.Fatalf("%v", err)
	}
	want := "a:0:2|b:1:2|0/1/0/2|1/2/1/2"
	if got := rt.strGo(v); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A hole is not an element, and the fast presence test added alongside this
// must agree with the slow one about which is which — including for an index
// that exists only as a named property with non-default attributes, which is
// the case the fast test deliberately declines to answer.
func TestFastElemPresenceMatchesTheSlowPath(t *testing.T) {
	rt := New()
	v, err := rt.RunString("holes.js", `
		var log = [];
		var a = [1, , 3];          // a[1] is a hole
		a.forEach(function (el, i) { log.push(i + "=" + el); });

		var b = [1, 2, 3];
		Object.defineProperty(b, 1, { value: 99, enumerable: false, writable: false });
		b.forEach(function (el, i) { log.push("b" + i + "=" + el); });

		var c = [1, 2];
		Object.defineProperty(c, 1, { get: function () { return 7; }, configurable: true });
		c.forEach(function (el, i) { log.push("c" + i + "=" + el); });

		log.join("|");
	`)
	if err != nil {
		t.Fatalf("%v", err)
	}
	want := "0=1|2=3|b0=1|b1=99|b2=3|c0=1|c1=7"
	if got := rt.strGo(v); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
