package engine

import "testing"

// A let-headed loop hands every iteration its own binding. That is only
// observable through a closure, so the compiler now emits the opcode that
// implements it only when the loop contains something that could close over
// one — which makes the loop compilable, and makes these the tests that matter:
// every shape where the answer would change if the decision were wrong.

func TestPerIterationBindingsSurviveTheOptimisation(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"for(let) captured by a function expression",
			`var f = []; for (let i = 0; i < 3; i++) { f.push(function () { return i; }); }
			 f.map(function (g) { return g(); }).join(",")`,
			"0,1,2",
		},
		{
			"for(let) captured by an arrow",
			`var f = []; for (let i = 0; i < 3; i++) { f.push(() => i); }
			 f.map(g => g()).join(",")`,
			"0,1,2",
		},
		{
			"captured in the update clause",
			`var f = []; for (let i = 0; i < 3; i++, f.push(() => i)) {}
			 f.map(g => g()).join(",")`,
			"1,2,3",
		},
		{
			"captured in the condition",
			`var f = []; for (let i = 0; (f.push(() => i), i < 3); i++) {}
			 f.map(g => g()).join(",")`,
			"0,1,2,3",
		},
		{
			"captured by the initializer",
			`var f = []; for (let i = (f.push(() => i), 0); i < 2; i++) {}
			 String(f.length)`,
			"1",
		},
		{
			"for-of with a const head",
			`var f = []; for (const x of [1, 2, 3]) { f.push(() => x); }
			 f.map(g => g()).join(",")`,
			"1,2,3",
		},
		{
			"for-of with destructuring",
			`var f = []; for (const [a, b] of [[1,2],[3,4]]) { f.push(() => a + b); }
			 f.map(g => g()).join(",")`,
			"3,7",
		},
		{
			"for-in",
			`var f = []; for (let k in {x: 1, y: 2}) { f.push(() => k); }
			 f.map(g => g()).join(",")`,
			"x,y",
		},
		{
			"a closure nested two functions deep",
			`var f = []; for (let i = 0; i < 3; i++) { f.push(function () { return () => i; }); }
			 f.map(g => g()()).join(",")`,
			"0,1,2",
		},
		{
			"a closure inside a nested loop",
			`var f = []; for (let i = 0; i < 2; i++) { for (let j = 0; j < 2; j++) { f.push(() => i + "" + j); } }
			 f.map(g => g()).join(",")`,
			"00,01,10,11",
		},
		{
			"a closure inside a try in the loop",
			`var f = []; for (let i = 0; i < 3; i++) { try { f.push(() => i); } catch (e) {} }
			 f.map(g => g()).join(",")`,
			"0,1,2",
		},
		{
			"a loop with no closure at all",
			`var s = 0; for (let i = 0; i < 5; i++) { s += i; } String(s)`,
			"10",
		},
	}

	for _, c := range cases {
		for _, jit := range []bool{false, true} {
			t.Run(c.name, func(t *testing.T) {
				rt := New()
				rt.SetJITEnabled(jit)
				v, err := rt.RunString("periter.js", c.src)
				if err != nil {
					t.Fatalf("jit=%v: %v", jit, err)
				}
				got, e := rt.ToString(v)
				if e != nil {
					t.Fatalf("jit=%v toString: %v", jit, e)
				}
				if got != c.want {
					t.Fatalf("jit=%v: got %q, want %q", jit, got, c.want)
				}
			})
		}
	}
}

// The point of the change: a loop with nothing to capture is now compilable.
func TestALetLoopWithoutClosuresIsCompiled(t *testing.T) {
	defer withThreshold(2)()
	was := jitStats.enabled
	jitStats.enabled = true
	t.Cleanup(func() { jitStats.enabled = was })

	c0, _, _ := JITStats()
	rt := New()
	rt.SetJITEnabled(true)
	_, err := rt.RunString("hot.js", `
		function work(n) { let t = 0; for (let i = 0; i < n; i++) { t += i * 3 + 1; } return t; }
		let acc = 0;
		for (var k = 0; k < 40; k++) { acc += work(20); }
		acc;
	`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if c1, _, _ := JITStats(); c1-c0 < 30 {
		t.Fatalf("a let-headed loop was not compiled: %d compiled entries", c1-c0)
	}
}

// And a loop that could capture still gets the opcode, so it is still refused —
// correctness first. This is the case the crude test is crude in favour of.
func TestALoopThatCouldCaptureStillGetsTheOpcode(t *testing.T) {
	rt := New()
	prog, err := Parse("cap.js", `function f() { var a = []; for (let i = 0; i < 3; i++) { a.push(() => i); } return a; }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	top, err := rt.Compile(prog, "cap.js", "")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	fn := top.childFuncs[0]
	found := false
	for ip := fn.startIP; ip < len(fn.code); {
		op := Opcode(fn.code[ip])
		if op == OpCloseUpval {
			found = true
			break
		}
		ip += int(opTable[op].Size)
	}
	if !found {
		t.Fatal("a loop whose body creates a closure lost its per-iteration binding")
	}
}

// eval can create a closure the compiler never sees, so a mention of the name
// has to count.
func TestEvalInALoopKeepsThePerIterationBinding(t *testing.T) {
	rt := New()
	v, err := rt.RunString("ev.js", `
		var f = [];
		for (let i = 0; i < 3; i++) { eval("f.push(function () { return i; })"); }
		f.map(function (g) { return g(); }).join(",")
	`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, e := rt.ToString(v)
	if e != nil {
		t.Fatalf("toString: %v", e)
	}
	if got != "0,1,2" {
		t.Fatalf("got %q, want 0,1,2 — a closure made by eval shared one binding", got)
	}
}
