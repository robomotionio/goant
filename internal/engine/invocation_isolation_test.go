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
