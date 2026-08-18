package goant_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robomotionio/goant"
)

// A resolver serving modules that are not on disk at all, including a cycle and
// a bare specifier — the two things the default path-joining resolution cannot
// express.
func TestModuleResolverServesVirtualGraph(t *testing.T) {
	sources := map[string]string{
		"virt:/entry.mjs": `
			import { name } from '@pkg/greeter';
			import { depth } from 'virt:/cycle-a.mjs';
			globalThis.result = name + ':' + depth();
		`,
		"virt:/greeter.mjs": `
			import { suffix } from 'virt:/cycle-b.mjs';
			export const name = 'hello' + suffix;
		`,
		// A legal cycle: each half reaches the other through a function, which
		// is what makes the reference happen after both bodies have run.
		"virt:/cycle-a.mjs": `
			import { other } from 'virt:/cycle-b.mjs';
			export const one = 1;
			export function depth() { return one + other() }
		`,
		"virt:/cycle-b.mjs": `
			import { one } from 'virt:/cycle-a.mjs';
			export const suffix = '!';
			export function other() { return one }
		`,
	}
	var asked []string
	rt := goant.New(goant.WithModuleResolver(func(spec, referrer string) (string, string, error) {
		asked = append(asked, spec)
		path := spec
		if spec == "@pkg/greeter" {
			path = "virt:/greeter.mjs"
		}
		src, ok := sources[path]
		if !ok {
			return "", "", errors.New("no such module")
		}
		return src, path, nil
	}))
	defer rt.Close()

	if _, err := rt.RunModule("virt:/entry.mjs", sources["virt:/entry.mjs"]); err != nil {
		t.Fatalf("run module graph: %v", err)
	}
	got, err := rt.Get("result")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "hello!:2" {
		t.Fatalf("result = %q, want %q", got.String(), "hello!:2")
	}
	// The bare specifier reached the resolver as written, rather than being
	// mangled into a path first.
	if !contains(asked, "@pkg/greeter") {
		t.Fatalf("resolver was asked %v, expected the bare specifier among them", asked)
	}
}

// One module imported twice must be one instance: shared state is the whole
// point of a module registry, and a resolver that is asked twice must not
// produce two copies.
func TestModuleResolverSharesOneInstance(t *testing.T) {
	sources := map[string]string{
		"v:/entry.mjs": `
			import 'v:/a.mjs';
			import 'v:/b.mjs';
			import { count } from 'v:/shared.mjs';
			globalThis.count = count();
		`,
		"v:/a.mjs":      `import { bump } from 'v:/shared.mjs'; bump();`,
		"v:/b.mjs":      `import { bump } from 'v:/shared.mjs'; bump();`,
		"v:/shared.mjs": `let n = 0; export function bump() { n++ } export function count() { return n }`,
	}
	rt := goant.New(goant.WithModuleResolver(func(spec, _ string) (string, string, error) {
		src, ok := sources[spec]
		if !ok {
			return "", "", errors.New("no such module: " + spec)
		}
		return src, spec, nil
	}))
	defer rt.Close()

	if _, err := rt.RunModule("v:/entry.mjs", sources["v:/entry.mjs"]); err != nil {
		t.Fatal(err)
	}
	v, _ := rt.Get("count")
	if v.Int() != 2 {
		t.Fatalf("shared module bumped %d times, want 2 — it was instantiated twice", v.Int())
	}
}

// A resolver that fails must reject the import rather than fall back to the
// filesystem and produce something surprising.
func TestModuleResolverFailureIsReported(t *testing.T) {
	rt := goant.New(goant.WithModuleResolver(func(spec, _ string) (string, string, error) {
		return "", "", errors.New("not in the bundle")
	}))
	defer rt.Close()

	_, err := rt.RunModule("v:/entry.mjs", `import 'nope';`)
	if err == nil {
		t.Fatal("expected an unresolvable import to fail")
	}
	if !strings.Contains(err.Error(), "not in the bundle") {
		t.Fatalf("error %q does not carry the resolver's reason", err)
	}
}

// import.meta.url used to be an empty object, and `new URL('./x',
// import.meta.url)` failed somewhere else entirely as a result.
func TestImportMetaURL(t *testing.T) {
	rt := goant.New(goant.WithModuleResolver(func(spec, _ string) (string, string, error) {
		return `export const url = import.meta.url;`, spec, nil
	}))
	defer rt.Close()

	if _, err := rt.RunModule("bundle:/pkg/index.mjs", `
		import { url } from 'bundle:/dep.mjs';
		globalThis.depURL = url;
		globalThis.selfURL = import.meta.url;
	`); err != nil {
		t.Fatal(err)
	}
	dep, _ := rt.Get("depURL")
	if dep.String() != "bundle:/dep.mjs" {
		t.Fatalf("import.meta.url = %q, want the module's own key", dep.String())
	}
	self, _ := rt.Get("selfURL")
	if !strings.HasSuffix(self.String(), "index.mjs") {
		t.Fatalf("entry import.meta.url = %q", self.String())
	}
}

// The point of the whole exercise: a promise created in Go, settled from a
// different goroutine, resuming an await inside JavaScript.
func TestNewPromiseSettledFromAnotherGoroutine(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	rt.Set("slowAnswer", func() (goant.Value, error) {
		p, resolve, _ := rt.NewPromise()
		go func() {
			time.Sleep(20 * time.Millisecond)
			if err := resolve(map[string]any{"answer": 42}); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}()
		return p, nil
	})

	v, err := rt.RunString(`(async () => { const r = await slowAnswer(); return r.answer * 2 })()`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := rt.AwaitContext(context.Background(), v)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if out.Int() != 84 {
		t.Fatalf("got %v, want 84", out.Int())
	}
}

// A rejection carries a Go error into the script's catch.
func TestNewPromiseRejectsWithGoError(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	rt.Set("failing", func() (goant.Value, error) {
		p, _, reject := rt.NewPromise()
		go func() { reject(errors.New("disk on fire")) }()
		return p, nil
	})

	v, err := rt.RunString(`(async () => { try { await failing() } catch (e) { return 'caught: ' + e.message } })()`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := rt.AwaitContext(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "caught: disk on fire" {
		t.Fatalf("got %q", out.String())
	}
}

// Only the first settlement counts, and the second says so instead of silently
// doing nothing.
func TestNewPromiseSettlesOnce(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	p, resolve, reject := rt.NewPromise()
	if err := resolve("first"); err != nil {
		t.Fatal(err)
	}
	if err := reject(errors.New("second")); !errors.Is(err, goant.ErrSettled) {
		t.Fatalf("second settle reported %v, want ErrSettled", err)
	}
	out, err := rt.AwaitContext(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "first" {
		t.Fatalf("promise settled to %q", out.String())
	}
}

// RunLoop must not conclude it is idle while a host operation is outstanding:
// the queues are empty for the whole 30ms below, and the program is not done.
func TestRunLoopWaitsForHostWork(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	rt.Set("later", func() (goant.Value, error) {
		p, resolve, _ := rt.NewPromise()
		go func() {
			time.Sleep(30 * time.Millisecond)
			resolve("done")
		}()
		return p, nil
	})
	if _, err := rt.RunString(`globalThis.state = 'waiting'; later().then(v => { globalThis.state = v })`); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, _ := rt.Get("state")
	if v.String() != "done" {
		t.Fatalf("state = %q — the loop returned before the host answered", v.String())
	}
}

// And it must give up when told to, rather than waiting on an answer that is
// never coming.
func TestRunLoopHonoursContext(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	p, _, _ := rt.NewPromise()
	_ = p // handed to nobody and never settled: the loop has work forever

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := rt.RunLoop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunLoop returned %v, want the context deadline", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("RunLoop took %v to notice the deadline", d)
	}
}

// Post is the only other thing safe from another goroutine, and what it posts
// runs on the Runtime's own.
func TestPostRunsOnTheLoop(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	var wg sync.WaitGroup
	wg.Add(4)
	for i := 0; i < 4; i++ {
		go func(n int) {
			defer wg.Done()
			rt.HostRef()
			rt.Post(func() {
				defer rt.HostUnref()
				v, _ := rt.Get("total")
				rt.Set("total", int(v.Int())+n)
			})
		}(i)
	}
	rt.Set("total", 0)
	wg.Wait()
	if err := rt.RunLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, _ := rt.Get("total")
	if v.Int() != 6 {
		t.Fatalf("total = %d, want 6", v.Int())
	}
}

// A closed Runtime tells a late goroutine that its answer went nowhere.
func TestPostAfterCloseReportsClosed(t *testing.T) {
	rt := goant.New()
	rt.Close()
	if err := rt.Post(func() {}); !errors.Is(err, goant.ErrClosed) {
		t.Fatalf("Post after Close returned %v, want ErrClosed", err)
	}
}

// Under the virtual clock a delay only orders callbacks. Under real timers it
// is a delay.
func TestRealTimersActuallyElapse(t *testing.T) {
	virtual := goant.New()
	defer virtual.Close()
	start := time.Now()
	mustRun(t, virtual, `setTimeout(() => { globalThis.fired = true }, 250)`)
	if err := virtual.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("virtual clock waited %v", d)
	}
	if v, _ := virtual.Get("fired"); !v.Bool() {
		t.Fatal("virtual timer did not fire")
	}

	real := goant.New(goant.WithRealTimers(true))
	defer real.Close()
	start = time.Now()
	mustRun(t, real, `setTimeout(() => { globalThis.fired = true }, 60)`)
	if err := real.RunLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 50*time.Millisecond {
		t.Fatalf("real timer fired after %v, expected to wait ~60ms", elapsed)
	}
	if v, _ := real.Get("fired"); !v.Bool() {
		t.Fatal("real timer did not fire")
	}
}

// Ordering across two real timers is by deadline, not by scheduling order.
func TestRealTimersFireInDeadlineOrder(t *testing.T) {
	rt := goant.New(goant.WithRealTimers(true))
	defer rt.Close()
	mustRun(t, rt, `
		globalThis.order = [];
		setTimeout(() => order.push('slow'), 40);
		setTimeout(() => order.push('fast'), 5);
	`)
	if err := rt.RunLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := mustRun(t, rt, `order.join(',')`)
	if v.String() != "fast,slow" {
		t.Fatalf("order = %q", v.String())
	}
}

// A real-clock interval must stop when cleared, and not spin the loop in the
// meantime.
func TestRealTimersInterval(t *testing.T) {
	rt := goant.New(goant.WithRealTimers(true))
	defer rt.Close()
	mustRun(t, rt, `
		globalThis.ticks = 0;
		const id = setInterval(() => { if (++ticks === 3) clearInterval(id) }, 5);
	`)
	if err := rt.RunLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, _ := rt.Get("ticks")
	if v.Int() != 3 {
		t.Fatalf("ticks = %d, want 3", v.Int())
	}
}

func TestStructuredClone(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"plain", `const o = {a: 1, b: 'x'}; const c = structuredClone(o); c.a = 2; o.a + ':' + c.a`, "1:2"},
		{"nested", `const o = {inner: {n: 1}}; const c = structuredClone(o); c.inner.n = 9; o.inner.n + ':' + c.inner.n`, "1:9"},
		{"cycle", `const o = {}; o.self = o; const c = structuredClone(o); String(c.self === c)`, "true"},
		{"shared identity", `const inner = {}; const o = {a: inner, b: inner}; const c = structuredClone(o); String(c.a === c.b && c.a !== inner)`, "true"},
		{"array holes", `const a = [1, , 3]; const c = structuredClone(a); String(1 in c) + ':' + c.length`, "false:3"},
		{"map", `const m = new Map([['k', {v: 1}]]); const c = structuredClone(m); c.get('k').v = 2; m.get('k').v + ':' + c.get('k').v`, "1:2"},
		{"set", `const s = new Set([1, 2]); const c = structuredClone(s); c.add(3); s.size + ':' + c.size`, "2:3"},
		{"date", `const d = new Date(86400000); const c = structuredClone(d); String(c instanceof Date) + ':' + c.getTime()`, "true:86400000"},
		{"regexp", `const r = /ab+c/gi; const c = structuredClone(r); c.source + ':' + c.flags`, "ab+c:gi"},
		{"typed array", `const t = new Uint8Array([1,2,3]); const c = structuredClone(t); c[0] = 9; t[0] + ':' + c[0] + ':' + c.length`, "1:9:3"},
		{"views share one buffer", `
			const buf = new ArrayBuffer(8);
			const o = {a: new Uint8Array(buf), b: new Uint8Array(buf)};
			const c = structuredClone(o);
			c.a[0] = 7;
			String(c.b[0] === 7 && o.a[0] === 0)`, "true"},
		{"undefined survives", `const c = structuredClone({u: undefined}); String('u' in c)`, "true"},
		{"error", `const c = structuredClone(new TypeError('bad')); c.name + ':' + c.message`, "TypeError:bad"},
		{"bigint", `String(structuredClone({n: 10n}).n)`, "10"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// One Runtime each: a script's top-level const is a lexical binding
			// on the global environment, so re-running these in one world is a
			// redeclaration rather than a test.
			rt := goant.New()
			defer rt.Close()
			v := mustRun(t, rt, c.src)
			if v.String() != c.want {
				t.Fatalf("got %q, want %q", v.String(), c.want)
			}
		})
	}

	rt := goant.New()
	defer rt.Close()
	for _, bad := range []string{`structuredClone(() => 1)`, `structuredClone({f() {}})`, `structuredClone(Symbol('s'))`} {
		if _, err := rt.RunString(bad); err == nil {
			t.Fatalf("%s should have been refused", bad)
		}
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// A host function taking an `any` used to panic on null: Export renders it as a
// nil interface and reflect.ValueOf(nil) is not a settable value.
func TestNullIntoAnyParameter(t *testing.T) {
	rt := goant.New()
	defer rt.Close()
	rt.Set("kind", func(v any) string {
		if v == nil {
			return "nil"
		}
		return "value"
	})
	for src, want := range map[string]string{
		`kind(null)`:      "nil",
		`kind(undefined)`: "nil",
		`kind()`:          "nil",
		`kind(1)`:         "value",
	} {
		v := mustRun(t, rt, src)
		if v.String() != want {
			t.Fatalf("%s = %q, want %q", src, v.String(), want)
		}
	}
}
