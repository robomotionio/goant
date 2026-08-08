//go:build amd64 || arm64

package engine

import "testing"

// An async function suspends at `await`, and a compiled frame cannot suspend.
// The tier used to conclude from that that no async function could be compiled,
// which is a stronger claim than the premise supports: an async function with no
// `await` in it never suspends.
//
// It is not a rare shape. A host that hands a script a message and takes a value
// back wraps the script in an async function, because some scripts await and the
// wrapper has to serve those too. The ones that do not await were paying for the
// ones that do — with their whole body, since the wrapper is where the script
// lives.

// jitAsyncStats runs src with the tier on and reports how the frames divided.
func jitAsyncStats(t *testing.T, src string) (compiled, interpreted uint64, out Value) {
	t.Helper()
	was := jitStats.enabled
	jitStats.enabled = true
	c0, _, i0 := JITStats()
	t.Cleanup(func() { jitStats.enabled = was })

	defer withThreshold(2)()
	rt := New()
	rt.SetJITEnabled(true)
	v, err := rt.RunString("async_test.js", src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.DrainJobs()
	c1, _, i1 := JITStats()
	return c1 - c0, i1 - i0, v
}

func TestAnAsyncFunctionWithoutAwaitIsCompiled(t *testing.T) {
	const src = `
		var total = 0;
		var work = async function () {
			var t = 0;
			for (var i = 0; i < 50; i++) { t += i * 3 + 1; }
			return t;
		};
		for (var k = 0; k < 40; k++) { work().then(function (v) { total += v; }); }
		total;
	`
	compiled, _, _ := jitAsyncStats(t, src)
	// 40 calls, compiled from the third; the exact count depends on how many
	// other frames the harness runs, so what is pinned is that the body was
	// compiled at all rather than a number that moves when the runtime does.
	if compiled < 30 {
		t.Fatalf("async body was not compiled: only %d compiled entries", compiled)
	}
}

// The refusal that has to stay. A function containing AWAIT is refused by the
// opcode scan, which is where it belongs — one refusal, stated in terms of the
// body rather than the shape.
func TestAnAsyncFunctionThatAwaitsIsStillRefused(t *testing.T) {
	rt := New()
	rt.SetJITEnabled(true)
	prog, err := Parse("a.js", `async function f() { var t = 0; await 1; return t; }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	top, err := rt.Compile(prog, "a.js", "")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	fn := top.childFuncs[0]
	if !jitEligible(fn) {
		t.Fatal("an awaiting function should reach the opcode scan, not be refused before it")
	}
	// &why directly rather than jitWhy, which reports nothing unless the
	// diagnostic counters are on and this test is about the reason.
	var why string
	if code := jitCompile(fn, &why); code != nil {
		t.Fatal("compiled a function containing AWAIT")
	}
	if why != "op:AWAIT" {
		t.Fatalf("refused for %q, want op:AWAIT — the reason should name what is in the body", why)
	}
}

func TestAGeneratorIsStillRefusedOutright(t *testing.T) {
	rt := New()
	prog, _ := Parse("g.js", `function* g() { var t = 0; for (var i = 0; i < 3; i++) t += i; return t; }`)
	top, err := rt.Compile(prog, "g.js", "")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if jitEligible(top.childFuncs[0]) {
		t.Fatal("a generator must be refused before the opcode scan: its frame is entered by next()")
	}
}

// The answer is what must not move. Same script, both tiers, including the
// promise the async function settles and the order the jobs run in.
func TestCompilingAnAsyncFunctionDoesNotChangeTheAnswer(t *testing.T) {
	const src = `
		var log = [];
		var work = async function (n) {
			var t = 0;
			for (var i = 0; i < n; i++) { t += i % 7 === 0 ? -i : i * 2; }
			if (n > 20) { return t + "-big"; }
			return t + "-small";
		};
		var boom = async function () { throw new Error("expected"); };
		for (var k = 0; k < 30; k++) {
			work(k).then(function (v) { log.push(v); });
		}
		boom().catch(function (e) { log.push("caught:" + e.message); });
		log;
	`
	defer withThreshold(2)()
	run := func(jit bool) string {
		rt := New()
		rt.SetJITEnabled(jit)
		v, err := rt.RunString("a.js", src)
		if err != nil {
			t.Fatalf("jit=%v run: %v", jit, err)
		}
		rt.DrainJobs()
		s, e := rt.ToString(v)
		if e != nil {
			t.Fatalf("jit=%v toString: %v", jit, e)
		}
		return s
	}
	if a, b := run(false), run(true); a != b {
		t.Fatalf("the tier changed an async answer:\n interpreted %s\n compiled    %s", a, b)
	}
}

// A compiled frame can now be live inside a coroutine, and an ExecContext is
// addressed by depth — so this is the case that would corrupt one if genDrive
// did not swap the chain: a suspended coroutine holding frames at depths the
// driver goes on to reuse, with compiled code running on both sides.
func TestCompiledFramesInsideSuspendedCoroutines(t *testing.T) {
	const src = `
		var out = [];
		var deep = function (n) { var t = 0; for (var i = 0; i < n; i++) { t += i * 2; } return t; };
		var inner = async function (n) { return deep(n) + deep(n + 1); };
		var outer = async function (n) {
			var a = await inner(n);      // suspends, holding frames at this depth
			var b = deep(n) + a;         // compiled again after resuming
			var c = await inner(n + 2);
			return b + c;
		};
		for (var k = 0; k < 25; k++) { outer(k).then(function (v) { out.push(v); }); }
		out;
	`
	defer withThreshold(2)()
	run := func(jit bool) string {
		rt := New()
		rt.SetJITEnabled(jit)
		v, err := rt.RunString("c.js", src)
		if err != nil {
			t.Fatalf("jit=%v run: %v", jit, err)
		}
		rt.DrainJobs()
		s, e := rt.ToString(v)
		if e != nil {
			t.Fatalf("jit=%v toString: %v", jit, e)
		}
		return s
	}
	interpreted, compiled := run(false), run(true)
	if interpreted != compiled {
		t.Fatalf("compiled frames inside coroutines changed the answer:\n interpreted %s\n compiled    %s",
			interpreted, compiled)
	}
	if interpreted == "" {
		t.Fatal("the fixture produced nothing, so it pinned nothing")
	}
}
