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
v.Export()        // -> any
v.ExportTo(&dst)  // -> the Go type you already have
```

Objects, arrays and functions come through views:

```go
obj := v.Object()
name, err := obj.Get("name")
err  = obj.Set("count", 3)
keys, err := obj.Keys()
n, err := obj.Len()
item, err := obj.At(0)
res, err := obj.Call("method", 1, "two")   // `this` is bound to obj

fn := v.Function()
res, err := fn.Call(1, 2)
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
| struct, `*struct` | object — fields by `json` tags, methods as functions |
| `func(…)` | function |

Cycles are preserved: the same Go pointer converts to the same JavaScript
object. Types with no JavaScript form — channels, complex numbers — are an
error rather than a guess.

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
```

Being stopped is not something the script did, so it is not reported as an
exception — and a memory limit is a different problem from a timeout, with a
different fix, so it is not reported as one either.

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
result. `RunJobs` is the bare drain.

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
becomes invalid** — read out what you need first. The one thing a Scope does not
isolate is modification of the shared built-ins; that is detected rather than
prevented, and `s.Polluted()` reports it.

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
duration.

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

For a host with several outputs per run, `AppendJSONEach` serializes an array's
elements into one buffer and hands back the offsets, so the payloads are spans of
a single allocation and each value is serialized exactly once.

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
  there is one at a time per isolate. `ContextDirty` reports a script that
  modified shared state; such an isolate must be disposed rather than pooled.
- Anything not needed by a caller is absent rather than stubbed, so a missing
  feature is a compile error and not a wrong answer at runtime.

New code should use the `goant` package. The shim exists so an existing program
can move in one commit and modernise afterwards.

---
