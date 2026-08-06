package engine

import (
	"sync"
	"testing"
)

// Isolation has two halves in a node-red-style system, and they are provided by
// different things.
//
// Concurrent runs are isolated by running on separate Runtimes: a Runtime
// executes one script at a time, and a host achieves parallelism by leasing one
// per in-flight call. Nothing here makes a single Runtime safe to share.
//
// Sequential runs on the same pooled Runtime are isolated by Invocation. That
// is the half these tests interrogate, because it is the half that changed.

// Separate Runtimes must share nothing observable, even running at once.
func TestSeparateRuntimesAreIndependentUnderConcurrency(t *testing.T) {
	const workers = 8
	const iterations = 50

	var wg sync.WaitGroup
	errs := make(chan string, workers*iterations)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// One Runtime per goroutine — the pooled-isolate model.
			rt := New()
			for i := 0; i < iterations; i++ {
				inv := rt.BeginInvocation()
				// Each worker writes a value only it should ever see, and mutates
				// a builtin prototype, which is the most invasive thing a script
				// can do to shared state.
				s, err := rt.CompileScript("w.js", `
					globalThis.mine = WORKER;
					Array.prototype.tag = WORKER;
					[globalThis.mine, [].tag].join(",")
				`)
				if err != nil {
					errs <- err.Error()
					inv.End()
					return
				}
				_ = s
				src := `globalThis.mine = ` + itoa(w) + `; Array.prototype.tag = ` + itoa(w) +
					`; [globalThis.mine, [].tag].join(",")`
				sc, err := rt.CompileScript("w.js", src)
				if err != nil {
					errs <- err.Error()
					inv.End()
					return
				}
				v, err := rt.RunScript(sc)
				if err != nil {
					errs <- err.Error()
					inv.End()
					return
				}
				got, _ := rt.ToString(v)
				want := itoa(w) + "," + itoa(w)
				if got != want {
					errs <- "worker " + itoa(w) + " saw " + got + " want " + want
				}
				inv.End()
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Invocation isolates what a run installs, not what it modifies: the builtins
// are shared. Rather than pay to prevent that, the engine detects it — a run
// that reaches below the invocation watermark marks the Runtime unfit to reuse,
// and a host pooling Runtimes discards it instead of handing it to the next
// message.
//
// These check the contract that makes that sound: the flag is set exactly when
// shared state was touched, and honouring it restores full isolation.
func TestInvocationReportsBuiltinMutation(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		dirty bool
	}{
		{"adds to a shared prototype", `Array.prototype.polluted = 1; "ok"`, true},
		{"replaces a builtin method", `Array.prototype.push = function () {}; "ok"`, true},
		{"writes a global builtin", `globalThis.JSON = null; "ok"`, false}, // own property of the fresh global
		{"defineProperty on a shared proto", `Object.defineProperty(Array.prototype, "z", {value: 1}); "ok"`, true},
		{"deletes from a shared proto", `delete Array.prototype.pop; "ok"`, true},
		{"mutates its own objects", `var o = {a: 1}; o.b = 2; o.a = 3; "ok"`, false},
		{"mutates an object it created a prototype for", `function C(){}; C.prototype.m = 1; "ok"`, false},
		{"pure computation", `[1,2,3].map(x => x * 2).join(",")`, false},
		{"writes its own globals", `globalThis.x = 1; var y = 2; "ok"`, false},
	}
	for _, tc := range cases {
		rt := New()
		inv := rt.BeginInvocation()
		sc, err := rt.CompileScript("d.js", tc.src)
		if err != nil {
			t.Fatalf("%s: compile: %v", tc.name, err)
		}
		if _, err := rt.RunScript(sc); err != nil {
			t.Fatalf("%s: run: %v", tc.name, err)
		}
		got := inv.Dirty()
		inv.End()
		if got != tc.dirty {
			t.Errorf("%s: Dirty() = %v, want %v (%s)", tc.name, got, tc.dirty, tc.src)
		}
	}
}

// A host that honours the flag gets full isolation back: the polluted Runtime
// is dropped and the next message runs on a clean one.
func TestDiscardingDirtyRuntimeRestoresIsolation(t *testing.T) {
	rt := New()

	inv := rt.BeginInvocation()
	sc, err := rt.CompileScript("p.js", `Array.prototype.polluted = 1; "ok"`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunScript(sc); err != nil {
		t.Fatal(err)
	}
	dirty := inv.Dirty()
	inv.End()

	if !dirty {
		t.Fatal("a run that polluted a shared prototype did not report Dirty")
	}
	// This is what a pool does with a dirty Runtime.
	rt = New()

	inv = rt.BeginInvocation()
	sc2, err := rt.CompileScript("q.js", `typeof [].polluted`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunScript(sc2)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := rt.ToString(v)
	inv.End()

	if got != "undefined" {
		t.Fatalf("pollution survived discarding the Runtime: %q", got)
	}
}

// Ordinary property writes to objects a script created must obviously not
// escape — this is the common case and should already hold.
func TestSequentialRunsDoNotShareScriptObjects(t *testing.T) {
	rt := New()

	inv := rt.BeginInvocation()
	s, err := rt.CompileScript("a.js", `globalThis.shared = {n: 1}; "ok"`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunScript(s); err != nil {
		t.Fatal(err)
	}
	inv.End()

	inv = rt.BeginInvocation()
	s2, err := rt.CompileScript("b.js", `typeof globalThis.shared`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunScript(s2)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := rt.ToString(v)
	inv.End()

	if got != "undefined" {
		t.Fatalf("an object stored on globalThis leaked to the next run: %q", got)
	}
}

// An INDEXED write into an array that predates the invocation is state the next
// run inherits, exactly as a named one is — and none of them were noticed.
//
// The isolation model rests on Dirty(): a host pooling Runtimes recycles one
// whose invocation reported false. So a write that mutates pre-existing state
// without setting the flag does not merely under-report, it hands the next
// message an array the last one edited.
//
// Two separate reasons it was missed, and the second is why the list below is
// exhaustive rather than a single case:
//
//   - The plain-array path never called noteSharedMutation at all. Named writes
//     go through setField, which notes; `a[0] = 1` goes through setElementR,
//     which did not.
//   - Every TypedArray write was unreachable by the check even when called.
//     noteSharedMutation takes a Value and returns early unless IsObjectType(),
//     and T_TYPEDARRAY is not in tObjectMask — so every view handed to it was
//     silently ignored. That is what noteSharedMutationOf exists for.
//
// The operations that already worked are kept in the list. They are what makes
// it a specification of the boundary rather than a regression test for two bugs:
// the rule is "any script-driven mutation of an object older than the
// invocation", and the way to check a rule is to include the cases that pass.
func TestIndexedWritesToPreexistingArraysAreNoticed(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		// The two that did not work, and the growth case beside them.
		{"element store", `shared[0] = 99; "ok"`},
		{"element store past the end", `shared[9] = 99; "ok"`},
		{"typed array element", `view[0] = 99; "ok"`},
		{"typed array via set()", `view.set([9, 9]); "ok"`},

		// Already noticed, and they have to stay that way.
		{"named property", `shared.tag = 99; "ok"`},
		{"length", `shared.length = 1; "ok"`},
		{"plain object index", `obj[0] = 99; "ok"`},
		{"push", `shared.push(99); "ok"`},
		{"pop", `shared.pop(); "ok"`},
		{"shift", `shared.shift(); "ok"`},
		{"splice", `shared.splice(0, 1); "ok"`},
		{"fill", `shared.fill(0); "ok"`},
		{"copyWithin", `shared.copyWithin(0, 1); "ok"`},
		{"sort", `shared.sort(function (a, b) { return b - a; }); "ok"`},
		{"reverse", `shared.reverse(); "ok"`},
		{"delete an element", `delete shared[0]; "ok"`},
		{"defineProperty an index", `Object.defineProperty(shared, "0", {value: 5}); "ok"`},
		{"typed array fill", `view.fill(7); "ok"`},
		{"typed array copyWithin", `view.copyWithin(0, 1); "ok"`},
		{"typed array sort", `view.sort(); "ok"`},
		{"typed array reverse", `view.reverse(); "ok"`},
	} {
		rt := New()
		// Built BEFORE the invocation, which is what makes it shared: this is the
		// host state a pooled Runtime carries between messages.
		if _, err := rt.RunString("pre.js", `
			globalThis.shared = [1, 2, 3];
			globalThis.view = new Int32Array(4);
			globalThis.obj = {};
			1;`); err != nil {
			t.Fatalf("%s: pre: %v", tc.name, err)
		}
		inv := rt.BeginInvocation()
		sc, err := rt.CompileScript("d.js", tc.src)
		if err != nil {
			t.Fatalf("%s: compile: %v", tc.name, err)
		}
		if _, err := rt.RunScript(sc); err != nil {
			t.Fatalf("%s: run: %v", tc.name, err)
		}
		got := inv.Dirty()
		inv.End()
		if !got {
			t.Errorf("%s: Dirty() = false, but %q mutated state older than the invocation",
				tc.name, tc.src)
		}
	}
}

// The other half of the boundary: a script mutating only what it made itself
// must NOT dirty the Runtime, or pooling degenerates to a fresh Runtime per
// message and the isolation model buys nothing.
func TestWritesToATotalledArrayOfItsOwnAreNotNoticed(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"its own array", `var a = [1, 2, 3]; a[0] = 9; a[7] = 9; a.push(1); "ok"`},
		{"its own view", `var v = new Int32Array(4); v[0] = 9; v.fill(1); "ok"`},
		{"a view over its own buffer", `
			var b = new ArrayBuffer(16), v = new Int32Array(b); v[1] = 5; "ok"`},
	} {
		rt := New()
		inv := rt.BeginInvocation()
		sc, err := rt.CompileScript("c.js", tc.src)
		if err != nil {
			t.Fatalf("%s: compile: %v", tc.name, err)
		}
		if _, err := rt.RunScript(sc); err != nil {
			t.Fatalf("%s: run: %v", tc.name, err)
		}
		got := inv.Dirty()
		inv.End()
		if got {
			t.Errorf("%s: Dirty() = true for %q, which touched nothing it did not create",
				tc.name, tc.src)
		}
	}
}
