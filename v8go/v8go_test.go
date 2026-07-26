package v8go

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The cases below are shaped after the way the robomotion FunctionNode drives
// the binding, because that is the contract this package has to honour: one
// pooled isolate, one compiled UnboundScript, a fresh Context per call with
// host functions on the global, an async IIFE returning a promise, a microtask
// checkpoint, then reading the result.

func TestRunScriptAndReadValue(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso, nil)
	defer ctx.Close()

	v, err := ctx.RunScript(`1 + 2`, "t.js")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := v.Number(); got != 3 {
		t.Fatalf("got %v, want 3", got)
	}
	if !v.IsNumber() || v.IsString() || v.IsObject() {
		t.Fatal("type predicates disagree about a number")
	}
}

func TestGlobalSetIsVisibleToScript(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso, nil)
	defer ctx.Close()

	val, err := NewValue(iso, `{"a":1}`)
	if err != nil {
		t.Fatalf("NewValue: %v", err)
	}
	if err := ctx.Global().Set("__inMsg__", val); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := ctx.RunScript(`JSON.parse(globalThis.__inMsg__).a`, "t.js")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := v.Number(); got != 1 {
		t.Fatalf("got %v, want 1", got)
	}
}

// Host functions are the whole point of the ObjectTemplate: local/global/flow
// accessors and the msg bridge all arrive this way.
func TestHostFunctionRoundTrip(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()

	tmpl := NewObjectTemplate(iso)
	tmpl.Set("addOne", NewFunctionTemplate(iso, func(info *FunctionCallbackInfo) *Value {
		args := info.Args()
		out, _ := NewValue(iso, args[0].Number()+1)
		return out
	}))
	tmpl.Set("echo", NewFunctionTemplate(iso, func(info *FunctionCallbackInfo) *Value {
		out, _ := NewValue(iso, info.Args()[0].String()+"!")
		return out
	}))

	ctx := NewContext(iso, tmpl)
	defer ctx.Close()

	v, err := ctx.RunScript(`addOne(41) + "/" + echo("hi")`, "t.js")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := v.String(); got != "42/hi!" {
		t.Fatalf("got %q, want %q", got, "42/hi!")
	}
}

// A host function must be able to throw something the script can catch.
func TestHostFunctionThrow(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()

	tmpl := NewObjectTemplate(iso)
	tmpl.Set("boom", NewFunctionTemplate(iso, func(info *FunctionCallbackInfo) *Value {
		msg, _ := NewValue(iso, "host said no")
		return iso.ThrowException(msg)
	}))
	ctx := NewContext(iso, tmpl)
	defer ctx.Close()

	v, err := ctx.RunScript(`(function(){ try { boom(); return "not reached"; } catch (e) { return String(e); } })()`, "t.js")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := v.String(); got != "host said no" {
		t.Fatalf("got %q, want %q", got, "host said no")
	}
}

// A throw from a host function that the script does NOT catch has to surface as
// an error, not as a normal completion.
func TestUncaughtHostThrowSurfaces(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()

	tmpl := NewObjectTemplate(iso)
	tmpl.Set("boom", NewFunctionTemplate(iso, func(info *FunctionCallbackInfo) *Value {
		msg, _ := NewValue(iso, "unhandled")
		return iso.ThrowException(msg)
	}))
	ctx := NewContext(iso, tmpl)
	defer ctx.Close()

	if _, err := ctx.RunScript(`boom()`, "t.js"); err == nil {
		t.Fatal("expected an error from an uncaught host throw")
	}
}

// The FunctionNode wrapper returns an async IIFE, so the completion value is a
// promise that only settles once microtasks run.
func TestAsyncIIFENeedsCheckpoint(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso, nil)
	defer ctx.Close()

	// The await is what makes this a real test: an async function with no await
	// runs to completion synchronously and resolves its promise before anyone
	// gets a chance to check, in V8 too. Suspending on an already-resolved
	// promise still costs a microtask turn, which is exactly the state the
	// FunctionNode wrapper is in when it reaches the checkpoint.
	v, err := ctx.RunScript(
		`(async () => { const r = await Promise.resolve("ok"); return JSON.stringify([r]); })()`, "t.js")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !v.IsPromise() {
		t.Fatal("expected a promise")
	}
	p, err := v.AsPromise()
	if err != nil {
		t.Fatalf("AsPromise: %v", err)
	}
	if p.State() != Pending {
		t.Fatal("promise settled before the checkpoint")
	}

	ctx.PerformMicrotaskCheckpoint()

	if p.State() != Fulfilled {
		t.Fatalf("state %v after checkpoint, want Fulfilled", p.State())
	}
	var out []string
	if err := json.Unmarshal([]byte(p.Result().String()), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 || out[0] != "ok" {
		t.Fatalf("got %v", out)
	}
}

func TestRejectedPromiseCarriesReason(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso, nil)
	defer ctx.Close()

	v, err := ctx.RunScript(`(async () => { throw new Error("nope"); })()`, "t.js")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	ctx.PerformMicrotaskCheckpoint()
	p, err := v.AsPromise()
	if err != nil {
		t.Fatalf("AsPromise: %v", err)
	}
	if p.State() != Rejected {
		t.Fatalf("state %v, want Rejected", p.State())
	}
	if got := p.Result().DetailString(); !strings.Contains(got, "nope") {
		t.Fatalf("detail %q does not mention the reason", got)
	}
}

// Compile once, run in many contexts — the pooled-isolate pattern. Each context
// must see its own globals, or state would leak between customer invocations.
func TestUnboundScriptRunsInSeveralContexts(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()

	script, err := iso.CompileUnboundScript(
		`(function(){ var seen = globalThis.n; globalThis.n = 99; return seen; })()`, "u.js", CompileOptions{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	for i, want := range []float64{1, 2, 3} {
		ctx := NewContext(iso, nil)
		n, _ := NewValue(iso, want)
		if err := ctx.Global().Set("n", n); err != nil {
			t.Fatalf("ctx %d Set: %v", i, err)
		}
		v, err := script.Run(ctx)
		if err != nil {
			t.Fatalf("ctx %d run: %v", i, err)
		}
		if got := v.Number(); got != want {
			t.Fatalf("ctx %d saw n=%v, want %v — contexts are sharing globals", i, got, want)
		}
		ctx.Close()
	}
}

// A supplied code cache must be reported rejected rather than silently
// accepted, so the caller's recompile-from-source fallback runs.
func TestCodeCacheIsRejectedNotIgnored(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()

	cache := &CompilerCachedData{Bytes: []byte("stale")}
	s, err := iso.CompileUnboundScript(`1`, "c.js", CompileOptions{CachedData: cache})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !cache.Rejected {
		t.Fatal("a cache we cannot honour was not marked Rejected")
	}
	if s.CreateCodeCache() != nil {
		t.Fatal("CreateCodeCache should report that there is no cache to produce")
	}
}

// The timeout path: a runaway script stopped from another goroutine.
func TestTerminateExecutionStopsRunawayScript(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso, nil)
	defer ctx.Close()

	go func() {
		time.Sleep(20 * time.Millisecond)
		iso.TerminateExecution()
	}()

	_, err := ctx.RunScript(`for(;;){}`, "spin.js")
	if err == nil {
		t.Fatal("runaway script completed normally")
	}
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v, want ErrTerminated", err)
	}
	// A termination is not something the script did, so it must not masquerade
	// as a JSError — the caller type-asserts to read a message.
	var jsErr *JSError
	if errors.As(err, &jsErr) {
		t.Fatal("termination reported as a *JSError")
	}
}

func TestResumeExecutionAllowsReuse(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()

	ctx := NewContext(iso, nil)
	go func() {
		time.Sleep(20 * time.Millisecond)
		iso.TerminateExecution()
	}()
	if _, err := ctx.RunScript(`for(;;){}`, "spin.js"); !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v, want ErrTerminated", err)
	}
	ctx.Close()

	iso.ResumeExecution()
	ctx2 := NewContext(iso, nil)
	defer ctx2.Close()
	v, err := ctx2.RunScript(`6*7`, "again.js")
	if err != nil {
		t.Fatalf("run after resume: %v", err)
	}
	if got := v.Number(); got != 42 {
		t.Fatalf("got %v, want 42", got)
	}
}

// A JS exception must arrive as a *JSError with the message split out, since
// callers build their error codes from e.Message and e.Location.
func TestJSErrorShape(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso, nil)
	defer ctx.Close()

	_, err := ctx.RunScript(`throw new TypeError("bad input")`, "t.js")
	if err == nil {
		t.Fatal("expected an error")
	}
	var jsErr *JSError
	if !errors.As(err, &jsErr) {
		t.Fatalf("got %T, want *JSError", err)
	}
	if !strings.Contains(jsErr.Message, "bad input") {
		t.Fatalf("message %q lost the text", jsErr.Message)
	}
	if !strings.Contains(jsErr.Message, "TypeError") {
		t.Fatalf("message %q lost the error name", jsErr.Message)
	}
}

func TestSyntaxErrorIsReported(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	if _, err := iso.CompileUnboundScript(`function (`, "bad.js", CompileOptions{}); err == nil {
		t.Fatal("expected a compile error")
	}
}

// MarshalJSON is how the node reads its result back, including the undefined
// case that must not become the string "undefined".
func TestMarshalJSON(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso, nil)
	defer ctx.Close()

	for _, tc := range []struct{ src, want string }{
		{`({a:1,b:"x"})`, `{"a":1,"b":"x"}`},
		{`[1,2,3]`, `[1,2,3]`},
		{`undefined`, `null`},
		{`"plain"`, `"plain"`},
	} {
		v, err := ctx.RunScript(tc.src, "m.js")
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		b, err := v.MarshalJSON()
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.src, err)
		}
		if string(b) != tc.want {
			t.Fatalf("%s: got %s, want %s", tc.src, b, tc.want)
		}
	}
}

// Number() must not truncate: the bridge-ID parsing depends on reading the full
// 2^53 range, and an earlier binding bug there truncated at 2^32.
func TestNumberDoesNotTruncate(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso, nil)
	defer ctx.Close()

	v, err := ctx.RunScript(`9007199254740991`, "n.js") // 2^53 - 1
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := v.Number(); got != 9007199254740991 {
		t.Fatalf("Number() = %v, want 2^53-1", got)
	}
	if got := v.Uint32(); float64(got) == 9007199254740991 {
		t.Fatal("Uint32 unexpectedly held the full value")
	}
}

func TestIsOneByteSafe(t *testing.T) {
	if !IsOneByteSafe([]byte("plain ascii")) {
		t.Fatal("ascii reported unsafe")
	}
	if IsOneByteSafe([]byte("café")) {
		t.Fatal("multi-byte utf-8 reported one-byte safe")
	}
}

// Dispose must not panic and must make further use fail cleanly.
func TestUseAfterDispose(t *testing.T) {
	iso := NewIsolate()
	iso.Dispose()
	iso.Dispose() // idempotent

	if _, err := iso.CompileUnboundScript(`1`, "d.js", CompileOptions{}); !errors.Is(err, ErrDisposed) {
		t.Fatalf("got %v, want ErrDisposed", err)
	}
	if ctx := NewContext(iso, nil); ctx != nil {
		t.Fatal("NewContext on a disposed isolate should fail")
	}
	iso.TerminateExecution() // must not panic
}

func TestHeapStatisticsReportsContexts(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()

	c1 := NewContext(iso, nil)
	c2 := NewContext(iso, nil)
	if n := iso.GetHeapStatistics().NumberOfNativeContexts; n != 2 {
		t.Fatalf("contexts = %d, want 2", n)
	}
	c1.Close()
	c1.Close() // double close must not double-decrement
	c2.Close()
	if n := iso.GetHeapStatistics().NumberOfNativeContexts; n != 0 {
		t.Fatalf("contexts = %d after close, want 0", n)
	}
	if iso.GetHeapStatistics().UsedHeapSize == 0 {
		t.Fatal("UsedHeapSize should report something")
	}
}
