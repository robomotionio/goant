# Embedding goant

The full embedding API: runtimes, the JIT, values, converting Go
values in and JavaScript values out, errors, deadlines, promises, scopes and
pools, the bytes-in/bytes-out JSON path, and migrating from v8go.

The [README](../README.md) has a five-line quickstart; this has everything else.

---

## Runtime

```go
rt := goant.New(
    goant.WithMemoryLimit(512 << 20),  // a budget, not a crash
    goant.WithModuleDir("./js"),       // where import specifiers resolve
)
defer rt.Close()

v, err := rt.RunString(`2 + 2`)
v, err  = rt.RunScript("main.js", src)
v, err  = rt.RunFile("main.js")
v, err  = rt.RunModule("mod.js", src)   // ES module: strict, own scope, TLA
```

A `Runtime` runs one script at a time and is not safe for concurrent use — the
same constraint every engine places on an isolate. `Interrupt` is the exception,
and is safe from any goroutine.

Compile once, run many:

```go
prog, err := rt.Compile("transform.js", src)
v, err := rt.RunProgram(prog)
```

The remaining options: `WithJIT` (below); `WithGC(false)`, worth it only for a
Runtime whose whole heap is discarded at the end, where collecting costs
something and reclaims nothing; `WithFieldNamer`, which changes how Go struct
fields are named in JavaScript; and `WithHostAPI(true)`, which installs the
Test262 host object `$262` along with `evalScript` and `createRealm`. Leave that
last one off outside a conformance run — these are capabilities rather than
language features, and merely reading `$262.IsHTMLDDA` turns the JIT off for the
Runtime and leaves it off.

`SetMemoryLimit` changes the budget after construction, and `Collect` runs a
collection at a moment of the host's choosing rather than the collector's.

## The JIT

Off by default. A function entered often enough is compiled to machine code and
run natively from then on; everything the compiler declines keeps interpreting,
so this changes how fast a program runs and not what it computes.

```go
rt := goant.New(goant.WithJIT(true))

rt.SetJIT(false)   // stops compiling AND stops entering compiled code
rt.SetJIT(true)    // and back
rt.JITEnabled()
```

It is per `Runtime`, not per process. A host does not have one workload — the
JIT is worth having for a long numeric flow and worth nothing for a script that
runs once — and both usually run in the same binary. `GOANT_JIT=1` sets the
default for Runtimes created afterwards, which is a convenience for benchmarking
rather than the way a program should decide.

`SetJIT(false)` is a kill switch rather than a preference: it stops this Runtime
entering code it has **already** compiled, so a host that sees trouble can turn
the JIT off on a live Runtime and have the next call interpret. No restart, and
no effect on any other Runtime.

Executable memory is reported apart from the heap, because the memory limit does
not cover it:

```go
s := rt.Stats()
s.Bytes                    // the JavaScript heap — what WithMemoryLimit bounds
s.CodeBytes, s.CodeBlocks  // executable memory, process-wide, bounded by nothing
```

Compiled code is released when the function that owns it is collected, so this
tracks how much distinct code is live rather than how long the process has been
up. It is still worth watching: a limit set on the heap says nothing about it.

## Values

`Value` is a small struct — a handle plus its Runtime — so passing one around
costs nothing and reading one allocates nothing. The zero `Value` is `undefined`,
and every method on it is safe.

```go
v.Kind()          // KindString, KindNumber, KindObject, …
v.IsArray()       // and IsFunction, IsDate, IsPromise, IsError, IsTypedArray…
v.String()        // ToString; also makes Value an fmt.Stringer
v.Int()           // ToNumber, truncated
v.Float()
v.Bool()          // ToBoolean — JavaScript truthiness
v.Bytes()         // an ArrayBuffer or typed array, without copying
v.Time()          // a Date
v.BigInt()        // a BigInt, as *big.Int
v.TypeOf()        // the typeof string
v.Equals(o)       // ===
v.Export()        // -> any
v.ExportTo(&dst)  // -> the Go type you already have
```

`Bytes`, `Time` and `BigInt` return a second `ok`, false when the value is not
that thing. The coercing readers do not: `String` yields "" for a `toString`
that throws, so use `ToString` when that difference matters.

Objects, arrays and functions come through views:

```go
obj := v.Object()
name, err := obj.Get("name")
err  = obj.Set("count", 3)
ok, err := obj.Has("count")                // the `in` operator
gone, err := obj.Delete("count")
keys, err := obj.Keys()
vals, err := obj.Values()
n, err := obj.Len()
item, err := obj.At(0)
err  = obj.SetAt(0, "first")
res, err := obj.Call("method", 1, "two")   // `this` is bound to obj
m, err := obj.Method("method")             // the same, as a callable

fn := v.Function()
res, err := fn.Call(1, 2)
res, err  = fn.CallOn(this, 1, 2)          // an explicit receiver
inst, err := fn.Construct("arg")           // new fn("arg")
```

Reads return `(Value, error)` rather than swallowing failures: a property read
can run a getter or a Proxy trap, and a host bridging one should not silently
receive `undefined` when it actually threw. A key that simply is not there is
`undefined` with no error, which is what JavaScript does.

## Go values in

`Set` and every `any` parameter accept ordinary Go values:

| Go | JavaScript |
|---|---|
| `nil` | `null` |
| `bool`, all `int`/`uint`/`float` | `boolean`, `number` |
| `string` | `string` |
| `[]byte` | `Uint8Array` — **no copy**, the script writes into your slice |
| `*big.Int` | `bigint` |
| `time.Time` | `Date` |
| `error` | `Error` |
| slice, array | `Array` |
| map | object |
| struct, `*struct` | object — exported fields by the Runtime's `FieldNamer`, exported methods as functions |
| `func(…)` | function |

The default `FieldNamer` is `JSONFieldNamer`, so a struct crosses under the names
its JSON form already uses and one set of tags describes both directions.
`GoFieldNamer` and `LowerCamelFieldNamer` are the alternatives, and any
`func(reflect.StructField) string` will do; returning "" leaves a field out.

Cycles are preserved: the same Go pointer converts to the same JavaScript
object. Types with no JavaScript form — channels, complex numbers,
`unsafe.Pointer` — are an error rather than a guess.

## Go functions in

Pass an ordinary Go function and it is bound by its signature: arguments
converted into its parameter types, its result converted back, a returned
`error` thrown into the script.

```go
rt.Set("upper", strings.ToUpper)
rt.Set("hypot", math.Hypot)
rt.Set("lookup", func(key string) (string, error) {
    v, ok := table[key]
    if !ok {
        return "", fmt.Errorf("no such key %q", key)
    }
    return v, nil
})
rt.Set("join", func(sep string, parts ...string) string { … })  // variadic
rt.Set("describe", func(o Order) string { … })                  // struct argument
```

A missing argument is the parameter's zero value and extra ones are ignored,
which is what a JavaScript function does with them.

When the argument list is variable, or you want the values unconverted, use the
raw form:

```go
rt.Set("log", goant.Func(func(c *goant.Call) (any, error) {
    parts := make([]string, c.Len())
    for i := range parts {
        parts[i] = c.String(i)
    }
    logger.Println(strings.Join(parts, " "))
    return nil, nil
}))
```

`c.This` is the receiver, so a raw function installed as a method works.

A returned error is thrown into the script. A **panic is not**: it unwinds
through the interpreter and out of the `Run` call that started it, which is what
you want for a bug, since turning it into a catchable JavaScript exception would
let a script swallow it. Return an error for the failures you mean, and discard a
Runtime a panic escaped from.

## JavaScript values out

```go
var cfg struct {
    Host string   `json:"host"`
    Port int      `json:"port"`
    Tags []string `json:"tags"`
}
err := v.ExportTo(&cfg)
```

Structs, maps, slices, arrays, pointers, `time.Time`, `[]byte`, `*big.Int` and
anything implementing `json.Unmarshaler` are all supported. A property the
script did not set leaves its field alone, so `ExportTo` fills in over defaults.

A `*func` target binds a JavaScript function to a Go signature:

```go
v, _ := rt.Get("format")

var format func(string, int) (string, error)
v.ExportTo(&format)

s, err := format("row", 3)
```

`Export()` is the untyped direction, and follows `encoding/json`'s conventions:
numbers are always `float64`, objects are `map[string]any`, arrays are `[]any`.
A cycle exports as the same Go map twice, not forever.

## Errors

```go
_, err := rt.RunString(src)

var jsErr *goant.Error
if errors.As(err, &jsErr) {
    jsErr.Name        // "TypeError"
    jsErr.Message
    jsErr.Stack
    jsErr.Value()     // the thrown value itself — a script may throw anything
}

var se *goant.SyntaxError   // would not parse; nothing ran
errors.As(err, &se)

errors.Is(err, goant.ErrInterrupted)   // stopped from outside
errors.Is(err, goant.ErrMemoryLimit)   // outgrew its budget
errors.Is(err, goant.ErrClosed)        // the Runtime was closed
```

Being stopped is not something the script did, so it is not reported as an
exception — and a memory limit is a different problem from a timeout, with a
different fix, so it is not reported as one either.

`ErrPending` comes from `Await`, `ErrPoolClosed` from `Pool.Do`, and `*ExitError`
carries the status from a script that called the host exit hook.

## Deadlines

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

stop := rt.WithContext(ctx)
defer stop()

v, err := rt.RunProgram(prog)   // errors.Is(err, goant.ErrInterrupted)
```

The interruption is left set afterwards, so an abandoned script cannot quietly
resume; call `ClearInterrupt` before reusing the Runtime, or let a `Pool` retire
it.

## Promises

Nothing settles until the job queue runs, and the checkpoint is explicit so a
host stays in control of when its callbacks fire.

```go
v, err := rt.RunString(`(async () => { … })()`)
res, err := rt.Await(v)   // drains the queue, unwraps; a rejection becomes *Error
```

`Await` passes a non-promise straight through, so it is safe to wrap around any
result. A promise still pending once the queue is empty reports `ErrPending`:
nothing left to run can settle it, which usually means it is waiting on host
asynchrony this Runtime does not have. `RunJobs` is the bare drain.

To read a promise without resolving it, `v.Promise()` gives `State()` and
`Result()` — a promise reports what it has settled to, and schedules nothing.

## Scopes and pools

A `Scope` is one unit of work with its own globals, reclaimed in one step when it
closes:

```go
prog, _ := rt.Compile("transform.js", src)

s, err := rt.NewScope()
defer s.Close()

s.Set("input", msg)
v, err := s.RunProgram(prog)
out, _, err := v.AppendJSON(buf[:0])   // read what you need BEFORE Close
```

Everything the run allocated is freed at `Close`, so **every Value from the scope
becomes invalid** — read out what you need first.

The one thing a Scope does not isolate is modification of the shared built-ins;
that is detected rather than prevented, and `s.Polluted()` reports it. Pollution
also cancels the reclamation: once a script has written to state that predates
the scope, something outside the freed region could point into it, so `Close`
frees nothing rather than risk it. A Runtime whose scope reports polluted must be
discarded rather than reused — the next run would inherit the change, and its
memory would not come back either. `Pool` does that for you.

`Scope` also has `Get`, `Await`, `RunJobs` and `Global`, which behave as the
Runtime's do but see the scope's own global object.

A `Pool` does the bookkeeping a server would otherwise write itself — leasing,
deadlines, and retiring a Runtime that was polluted, abandoned, or has grown too
large:

```go
pool := goant.NewPool(goant.PoolConfig{
    New: func() (*goant.Runtime, error) {
        rt := goant.New(goant.WithMemoryLimit(1 << 30))
        return rt, rt.Set("log", logger.Println)
    },
    MaxUses:   50_000,
    MaxMemory: 256 << 20,
    MaxIdle:   runtime.NumCPU(),
})
defer pool.Close()

var out []byte
err := pool.Do(ctx, func(s *goant.Scope) error {
    s.Set("msg", input)
    v, err := s.RunProgram(prog)
    if err != nil {
        return err
    }
    out, _, err = v.AppendJSON(nil)
    return err
})
```

A `Pool` is safe for concurrent use; each job gets a Runtime to itself for its
duration. `ctx` bounds the job: if it is done before or during the run, the
script is interrupted, the error is `ctx.Err()`, and the Runtime is retired
rather than reused, since it was abandoned mid-script. `Stats` reports how many
Runtimes are parked and how many exist.

## JSON, as bytes

```go
v, err := rt.ParseJSON(data)          // no intermediate JavaScript string
v, err  = rt.ParseJSONLazy(data)      // build each value on first read

out, ok, err := v.AppendJSON(dst)     // append into the host's buffer
```

`ParseJSONLazy` validates the whole document up front, then builds properties
and elements as something reads them. A value nobody reads is never built, and a
document the host serializes without touching goes back out as the bytes it came
in as — so a pass-through costs a scan, a full traversal costs what the eager
parse would have, and anything in between lands in between. Nothing to configure
and nothing to predict.

There is one contract difference, and it is the one to get right: `ParseJSON`
does not retain `data`, but **`ParseJSONLazy` does**. It must not be modified or
reused while the returned value, or anything reachable from it, is alive — so a
host recycling a read buffer between messages has to stop doing that on this
path, or copy.

`SetBlobResolver` installs the callback a lazy parse uses for envelopes, the
small stand-ins a host leaves in a message for data it keeps elsewhere:

```go
rt.SetBlobResolver(func(ref string) ([]byte, error) { … })
```

Resolving on first read rather than before the parse is what makes an envelope
pay: a field the script never mentions is never fetched, so a message referencing
a hundred megabytes costs a few hundred bytes. A fetch the resolver cannot
satisfy stops the script and is reported as the error from the run, rather than
surfacing as a type error in the middle of someone's JavaScript. It affects
`ParseJSONLazy` only.

For a host with several outputs per run, `AppendJSONEach` serializes an array's
elements into one buffer and hands back the offset each one ends at, so the
payloads are spans of a single allocation and each value is serialized exactly
once:

```go
buf, ends, err := arr.AppendJSONEach(nil, -1)   // -1: no limit
start := 0
for _, end := range ends {
    payload := buf[start:end:end]   // capped, so an append cannot reach the next
    start = end
}
```

An element with no JSON form — undefined, a function, a symbol — contributes no
bytes, so its span is empty. That is distinguishable from an element that
serialized to the empty string, which is not valid JSON and cannot occur.

---

## Migrating from v8go

The `v8go/` package is a drop-in replacement for the `rogchap` /
`robomotionio` v8go binding, implemented on goant. Changing the import path takes
a program off cgo and off V8 without touching call sites:

```go
import v8go "github.com/robomotionio/goant/v8go"
```

`NewIsolate`, `NewContext`, `RunScript`, `FunctionTemplate`, `ObjectTemplate`,
`JSError` and the rest keep their signatures. The differences are deliberate,
and each one says so in its own doc comment:

- `IsolateOptions.MaxOldSpaceBytes` becomes a real budget on the live heap,
  enforced after a collection and reported through `HeapLimitExceeded()` — which
  is what the option was always meant to do.
- The V8-tuning no-ops (`SetFlags`, `AddNearHeapLimitCallback`,
  `WarmupOldGenerationHeap`) are kept so call sites compile, and each one
  documents that it does nothing.
- `CreateCodeCache` returns nil: there is no serialised bytecode format. Callers
  already handle that, because V8 can refuse to produce one too.
- A `Context` is an invocation on a shared isolate rather than a fresh realm, so
  there is one at a time per isolate. `Context.Dirty()` reports a script that
  modified shared state; such an isolate must be disposed rather than pooled.
- Anything not needed by a caller is absent rather than stubbed, so a missing
  feature is a compile error and not a wrong answer at runtime.

New code should use the `goant` package. The shim exists so an existing program
can move in one commit and modernise afterwards.

---
