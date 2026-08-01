<p align="center">
  <img src="docs/goant-gopher.png" alt="goant" width="300">
</p>

# goant

A JavaScript engine in pure Go. No cgo, nothing to fetch at build time, one
static binary, and it cross-compiles wherever Go does.

```go
rt := goant.New()
defer rt.Close()

rt.Set("greet", func(name string) string { return "hello " + name })

v, err := rt.RunString(`greet("ada").toUpperCase()`)
fmt.Println(v.String()) // HELLO ADA
```

```sh
go get github.com/robomotionio/goant
```

## Why goant?

The three ways to run JavaScript from Go, and what each one costs:

|                        | goant                      | goja                    | V8 via cgo                          |
| ---------------------- | -------------------------- | ----------------------- | ----------------------------------- |
| cgo                    | **none**                   | none                    | required                            |
| Cross-compile          | **anywhere Go does**       | anywhere Go does        | a toolchain and a 100–210 MB prebuilt archive per platform |
| Language level         | **99.998% of Test262 core** ([what that excludes](#conformance)) | 77.9%                   | current                             |
| Out of memory          | **an error you can catch** | takes the process down  | takes the process down              |
| Per-run isolation      | **a fresh global, 111 ns** | a fresh Runtime         | a fresh context                     |
| JIT                    | in progress                | none                    | yes                                 |
| Binary cost            | **6.4 MB**                 | 13.3 MB                 | ~90 MB linked                       |

goant is for a Go program that has to run JavaScript it did not write, on
machines it does not control, without the process dying when that JavaScript
misbehaves. If your scripts are compute-bound and you can afford cgo, V8 is
still faster at running them — see [Benchmarks](#benchmarks).

---

## Why this exists

goant was written for [Robomotion](https://www.robomotion.io), whose robot
runtime (`robomotion-deskbot`) evaluates customer JavaScript in **Function
nodes** — a script per message, millions of messages, on machines we do not
control: Windows laptops under memory pressure, Raspberry Pis, Apple Silicon
Macs, Ubuntu servers.

That ran on V8 through cgo, first
[`rogchap.com/v8go`](https://github.com/rogchap/v8go) and then
[`robomotionio/v8go`](https://github.com/robomotionio/v8go) — a fork we had to
maintain because upstream is unmaintained, pins a V8 from April 2023, and
dropped Windows in 2021. Our fork pinned V8 14.7.173.21 and restored Windows.

It worked. It also cost more than the JavaScript was worth, in four separate
ways, and goant exists to remove all four.

### 1. The build was a cross-platform research project

V8 is a 100–210 MB static archive per platform, too big to vendor, so every
build downloads one; rebuilding it takes hours under the Chromium toolchain.
That was the easy part. It also has to be linked with V8's own clang and its own
libc++, and on three of our five platforms that went wrong in a different way:
Windows needed a shim to strip cgo's MinGW flags out of Go's link line, macOS
linked clean and then died at the first script with `Check failed: (platform)
!= nullptr`, and the published linux/arm64 archive was missing its libc++
runtime — which nobody noticed until a Raspberry Pi build months later.

Nobody did anything wrong. That is just what linking a 100 MB C++ artifact into
a Go program across five platforms costs. A pure-Go engine cannot have any of
it: `CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build` and you are done.

### 2. A JavaScript out-of-memory killed the whole process

V8's fatal OOM is `abort()`. Not an exception, not a Go panic — no `recover`, no
deferred anything, no error to route to a Catch node:

```
# Fatal JavaScript out of memory: CALL_AND_RETRY_LAST
```

A customer hit it with a **13.4 MB message** from an SAP table extraction. The
crash was not in their script. It was in `Global().Set("__inMsg__", …)`, before
any user JavaScript ran, because handing a message to V8 through cgo cost
several copies at once:

| live at the same moment | where |
|---|---|
| the message | Go heap |
| `string(inMsg)` | Go heap |
| `C.CString(…)` | C heap |
| `SeqOneByteString` / `SeqTwoByteString` | V8 old space |

That is ~50–67 MB resident for a 13 MB message, and ~200–270 MB for a 50 MB one.
Non-ASCII input — Turkish field values, in that customer's case — makes V8 choose
UTF-16 and doubles its share.

Around that sat four more sharp edges. V8 14.x sizes an isolate's heap ceiling
from the RAM available when the isolate is created, which on a pressured Windows
host can land at **~20 MB**. The binding did not expose
`CreateParams::constraints`, so no initial heap could be committed and every
isolate had to grow on demand at exactly the worst moment. It did not expose
`AddNearHeapLimitCallback`, so there was no embedder hook before the abort. And
V8's idle memory reducer hands pages back to the OS between calls, so every
message re-commits and gets a fresh chance to be denied.

Under goant there is one heap, Go's, and a **memory limit is an ordinary error**:
the script is stopped, the host is told which limit it hit, the flow's error
handler runs, and the process — with every other robot on it — carries on.

#### What the memory limit counts

The limit is off unless [`WithMemoryLimit`](#deadlines) sets one, and it bounds
what a script **retains** rather than what it allocates: it is tested after a
collection, so a loop that builds and discards a million objects passes and one
that builds and keeps them does not.

What counts toward it is the whole live heap — cell headers plus the bytes those
cells point at: string payloads, array element storage, ArrayBuffer stores. An
allocation large enough to exceed the budget on its own is tested before it is
taken, since a budget checked only afterwards cannot prevent the allocation that
exhausts the host. Each row below retains everything it allocates, run with a
64 MB limit inside a 768 MB cgroup:

| script | result |
|---|---|
| `k.push({})` | `ErrMemoryLimit`, process alive |
| `k.push(new Array(256).fill(7))` | `ErrMemoryLimit`, process alive |
| `k.push("x".repeat(100000))` | `ErrMemoryLimit`, process alive |
| `k.push(new ArrayBuffer(1<<20))` | `ErrMemoryLimit`, process alive |
| `s += s` | `ErrMemoryLimit`, process alive |
| 200k × 5 KB strings, none kept | no error — allocation is not retention |

One case is not covered: a single very large `ArrayBuffer` that is allocated and
never written to. Go serves it as untouched zero pages, so it occupies no
resident memory, and it is counted at the next collection as soon as anything
uses it.

Setting no limit costs nothing. The accounting is placed so that it is absent
from the collector and the interpreter's dispatch loop rather than merely
skipped by them, which is measurable: see `memlimit_bench_test.go`.

### 3. Everything had to cross a boundary that no longer exists

A cgo binding's API shape is dictated by its boundary: values are marshalled
across it, so what it can offer is one big string in each direction.

```
inbound   []byte -> Go string -> V8 string -> JSON.parse -> objects
outbound  objects -> JSON.stringify -> V8 string -> Go string -> []byte
```

Worse, a node with several outputs has to bring them back in one crossing, so
the wrapper stringified each result and then stringified the *array of those
strings* — escaping every quote in every payload a second time, and unescaping
it all again on the far side.

With no boundary, none of that is necessary: goant parses the host's bytes in
place and appends output into the host's buffer. On the production message path
that is where these came from:

| Function-node call (pooled) | V8 | goant |
|---|---:|---:|
| passthrough | 316 µs | **109 µs** |
| transform | 329 µs | **111 µs** |
| async | 327 µs | **106 µs** |

and on a 27.3 MB `return msg`, peak RSS went from **483 MB** under V8 to
**272 MB**.

### 4. Isolating a run cost 60× the run

V8 builds a context from a heap snapshot, so a fresh realm per run is cheap and
every embedder learns that habit. goant has no snapshot: building a realm means
constructing every prototype and every built-in, measured at **366 µs and 885
allocations** — against roughly 6 µs for what a short script actually does.

But almost none of a realm needs isolating. The built-ins are identical every
time; what differs per run is what the script installs. So goant gives each run
a fresh global object whose prototype is the shared one: built-ins resolve up
the chain, and everything the script assigns lands on the fresh object and is
dropped at the end. **111 ns** — about three thousand times cheaper — and the
whole run's memory is then reclaimed in one step by rewinding the allocator,
with nothing to trace.

That is [`Scope`](#scopes-and-pools), and it is the shape a message pump wants.

---

## Why not goja?

[goja](https://github.com/dop251/goja) is the obvious answer to "we want off
cgo", it is good, and we tried it. The problem is the language it speaks.

goja implements ECMAScript 5.1 plus a growing set of later features. Our users
do not write ES5.1. Increasingly they do not write the JavaScript at all — they
describe what they want to an AI, paste the result into a Function node, and run
it. What comes back is modern by default, and a lot of it does not parse:

```js
async function* pages() { yield 1; yield 2 }
for await (const p of pages()) console.log(p)
```

```
goja   SyntaxError: Line 2:20 Unexpected token await
goant  1
       2
```

On the same Test262 core profile, from the same checkout and through the same
harness, **goant passes 42,620 of 42,758 and goja passes 33,304** — a gap of
over nine thousand tests. It is not spread thin, either. It is `for await…of`
(1,142), async generators (1,608), classes containing them (2,016), `import()`
(475), top-level `await` (250), iterator helpers (495), `\p{…}` property
escapes (596), the `v` regex flag, `Array.fromAsync`, `Object.groupBy`, the new
`Set` methods, `using`. That is a list of the things an AI reaches for when you
ask it to page through an API or group some rows.

What made this untenable was not the size of the gap but its shape. A runtime
that plainly does not support something is a documented limit; one that supports
most of a language is a guessing game, and the person guessing is a customer who
wanted to filter some rows. The gap is invisible until it fires, it moves as
models change what they emit, and it cannot be explained in a tooltip.

So the requirement was never "pure Go" on its own. It was **pure Go and current**
— which in practice means chasing the specification itself rather than a feature
list. That is why the target is 100% of Test262's core profile.

None of this is a knock on goja: it is a good engine, it is faster than goant on
tight loops, and it starts faster. It is aimed at a different problem.

## Where it came from

The `ant` announcement went past on Hacker News: a JavaScript runtime written
from scratch in C, with its own engine and a MIR-based JIT. Around the same time
Bun was rewriting parts of itself from Zig to Rust, which made the general idea
of a wholesale language port feel less exotic than it sounds.

C is close enough to Go that a faithful port is mechanical more often than it is
clever — no ownership model to reconcile, no borrow checker to satisfy, and the
data structures transfer almost as they are. So the question was just whether
someone would do it. That gave us a starting point rather than a blank page:
ant's architecture, its opcode set, and its conformance corpus.

What happened after was not a port any more:

1. **Get the corpus green.** ant's own 1511-case compat-table suite, ES1 through
   ESNext, as the first bar. → 1511/1511.
2. **Then chase the specification.** Test262's ECMA-262 core profile, because
   that is the only bar that answers "will this run what my users write". →
   42,620 of 42,758, against goja's 33,304.
3. **Then find the bugs conformance does not.** V8's own `mjsunit` corpus — a
   decade and a half of real bug reports — run as a crash hunt rather than for
   a score. → 2,653/3,149, and nothing in it crashes the engine any more.
4. **Then make it fast.** Octane 2.0, scored against goja the way
   [ahaoboy/js-engine-benchmark](https://github.com/ahaoboy/js-engine-benchmark)
   does, plus the message-pump path that Robomotion actually runs.

Steps 2 to 4 are where nearly all of the work went, and they are why goant is no
longer describable as "ant in Go".

---

## What it is

goant is a from-scratch port of the **"Silver" engine** from
[`ant`](https://github.com/theMackabu/ant) — a JavaScript runtime written in C —
rewritten in Go: lexer → AST → bytecode compiler → 213-opcode interpreter, its
own tracing garbage collector, WTF-8 strings, and a JS→regex translation layer
over a vendored `regexp2`. A MIR-based JIT is ported but is not yet the default
execution tier (see `PLAN.md`).

The engine lives in `internal/engine`, deliberately: everything the interpreter
does is free to change shape as long as the observable behaviour holds. The
stable surfaces are this package and `v8go/`.

### Conformance

| suite | goant | goja |
|---|---|---|
| Test262, ECMA-262 core profile (`-core`) | **42,739 / 42,740** (99.998%) | 33,303 / 42,740 (77.9%) |
| Test262, every file, nothing excluded (`-all`) | 44,691 / 53,411 (83.7%) | — |
| ant's compat-table corpus (ES1–ESNext) | **1511 / 1511** | — |
| V8's `mjsunit` | 2,653 / 3,149 | — |

Both engines were scored on the same test262 checkout (`b363f29d`) through the
same harness — `./goant-t262 -core -runner goja-run` — so the two numbers mean
the same thing. goja has no ES module goal, so the runner declines module tests
rather than emulating them.

**What "core profile" means, and what it hides.** There is no official test262
profile; `-core` is goant's own name for its own exclusion list, so the only
honest way to quote it is next to the list. Of the 53,575 test files, `-core`
runs 42,740 and excludes 10,835:

| excluded | files | why |
|---|---|---|
| `intl402/` | 3,357 | ECMA-402 is a separate specification. goant's Intl is stubs. |
| `Temporal` | 4,611 | **In the specification. Simply not implemented.** |
| `staging/` | 1,483 | test262's own staging area — its CONTRIBUTING.md says these "do not count towards the test262 coverage requirement for a TC39 proposal to reach Stage 4". |
| Atomics, SharedArrayBuffer | 602 | Need the agent/worker host model. |
| `cross-realm`, `IsHTMLDDA` | 226 | Need `$262.createRealm` and web-legacy `document.all`. |
| unlanded proposals | 556 | decorators, import-defer, import-text/bytes, source-phase-imports, `Iterator.prototype.join`. |

Only the first two lines should give you pause, and only the Temporal one is a
real deduction: it is a shipped part of the language that goant does not have.
Folding Temporal back in would take the figure from 99.998% to 90.3% — so treat
the core number as "the language minus Temporal and Intl", never as "JavaScript".
That is what the `-all` row is for: `./goant-t262 -all` skips nothing.

The two numbers reconcile. Under `-all` there are 8,720 failures, and 8,718 of
them sit in the buckets above (4,611 Temporal — every single excluded Temporal
file fails, 3,141 `intl402/`, 582 host-model, 195 `staging/`, 189 unlanded
proposals). Of the two that do not, one is a harness artefact — a test that
times out only under full-suite load and passes on its own — and **one** is a
genuine engine gap: `built-ins/Proxy/revocable/tco-fn-realm.js`, which wants a
revoked Proxy to throw the TypeError of its own function's realm. That is also
the single failure under `-core`.

Whole directories are at 100%, including `built-ins/RegExp` (1867/1867),
`built-ins/Array`, `built-ins/Promise`, `built-ins/Proxy`, `built-ins/Date`,
`built-ins/JSON`, `built-ins/Iterator` and `language/module-code` (595/595).

Not implemented: **Temporal**, and **Intl** beyond stubs for
`Collator`/`NumberFormat`/`DateTimeFormat` that ignore locale and options. If
your scripts need real internationalisation, goant is not ready for you yet.

[Benchmarks](#benchmarks) has the same profile scored against goja, on the same
checkout and the same harness.

---

## Benchmarks

Everything below was measured on an idle machine created for the purpose and
destroyed afterwards, with nothing else running on it. The scripts are in the
repository; see [Reproducing](#reproducing).

Read the pure-Go engines against each other. node, deno and bun are here as the
JIT reference, not as peers — the distance to them is what a bytecode
interpreter costs, and closing it is what the JIT tier is for.

### Octane 2.0

The suite the cross-engine comparisons standardise on, and what
[ahaoboy/js-engine-benchmark](https://github.com/ahaoboy/js-engine-benchmark)
scores. Higher is better.

| Benchmark    |    goant |     goja |   node |   deno |    bun | goant vs fastest JIT |
| ------------ | -------: | -------: | -----: | -----: | -----: | -------------------: |
| Richards     |      199 |  **216** |  34924 |  34352 |  41889 |                 210× |
| DeltaBlue    |      232 |  **273** |  99554 |  99851 |  58488 |                 430× |
| Crypto       |  **134** |      129 |  39846 |  42729 |  46841 |                 350× |
| RayTrace     |  **382** |      298 |  73333 |  77477 | 115956 |                 304× |
| EarleyBoyer  |  **597** |      537 |  65363 |  62784 |  72582 |                 122× |
| RegExp       |      145 |  **215** |   9346 |   9748 |  10776 |                  74× |
| Splay        |     1925 | **2201** |  43982 |  43465 |  43845 |                  23× |
| NavierStokes |  **312** |      205 |  32467 |  32550 |  34178 |                 110× |

**goant and goja are level**: four each, and a geometric mean of 1.005 across
the eight. goant's lead is largest on NavierStokes (+52%) and RayTrace (+28%),
which are float and allocation heavy; goja's is largest on RegExp (+48%), where
goant's remaining cost is the per-match glue rather than the matcher itself.

Against the JIT engines the gap runs from 23× (Splay, which is dominated by
allocation and GC) to 430× (DeltaBlue, which is polymorphic dispatch a JIT can
inline and an interpreter cannot). That is the honest shape of an interpreter
against a tiered JIT, and it is why the compute-bound case is the one place
goant should not be chosen today.

On tight-loop microbenchmarks (`./goant-bench`, our own workloads rather than a
neutral suite) goja is ahead of goant on most, by roughly 1.3–1.8×. Octane is
the fairer of the two comparisons — it is neutral, it is what everyone else
publishes, and it exercises whole programs rather than single operations — but
both numbers are in the repository and neither is hidden.

### Cold start

Time to start, evaluate one line, and exit — the part every one of these can do.
The hono-style cold start other runtimes publish has no counterpart here: goant
is an engine, not a runtime with a module resolver and an npm ecosystem.

Measured with hyperfine, 20 warmup runs and 200 timed runs:

| Runtime   | Mean        | Relative       |
| --------- | ----------: | -------------- |
| **goja**  | **1.9 ms**  | 1.00           |
| **goant** | **2.4 ms**  | 1.24× slower   |
| bun       |    10.0 ms  | 5.15× slower   |
| deno      |    13.1 ms  | 6.75× slower   |
| node      |    20.7 ms  | 10.67× slower  |

### Binary size

| Binary                        |     Size |
| ----------------------------- | -------: |
| **goant** (`cmd/goant`, `-s -w`) | **6.4 MB** |
| goja (`bench/gojarun`, minimal)  |  13.3 MB |
| bun                              |  88.5 MB |
| deno                             | 101.4 MB |
| node                             | 123.0 MB |

goant's is a whole JavaScript engine — parser, compiler, interpreter, garbage
collector, every built-in, the Unicode tables and a regex engine — statically
linked, with no shared library to ship beside it. node, deno and bun are whole
runtimes and are not really being compared like for like; they are here for
scale.

<details>
<summary>Environment</summary>

| Detail   | Value                                            |
| -------- | ------------------------------------------------ |
| Hardware | Azure `Standard_D8s_v5` — Xeon Platinum 8370C @ 2.80 GHz, 8 vCPU, 31 GB |
| OS       | Ubuntu 24.04.4 LTS (x86_64), kernel 6.17          |
| Go       | 1.26.5, `CGO_ENABLED=0`                           |
| Octane   | chromium/octane @ `570ad1cc`                      |
| test262  | tc39/test262 @ `b363f29d`                         |
| goja     | `v0.0.0-20260723142020-b4aef50fa347`              |
| node     | 25.9.0                                            |
| deno     | 2.9.3                                             |
| bun      | 1.3.14                                            |

Octane scores are the best of two runs (the suite already repeats internally to
a stable score); the JIT engines carry more run-to-run variance than the two
interpreters do.

</details>

### Reproducing

```sh
sh bench/suites/fetch.sh                                  # Octane, pinned commit
go build -C bench/gojarun -o "$(go env GOPATH)/bin/goja-run" .

CGO_ENABLED=0 go build -o goant ./cmd/goant
CGO_ENABLED=0 go build -o goant-bench ./cmd/goant-bench

./goant-bench -suite octane -only splay -n 2   # one Octane suite, every engine
./goant-bench                                  # the microbenchmarks
```

`goant-bench` scores whichever engines are on PATH and skips the rest, so a
partial install still produces a table. See [bench/README.md](bench/README.md).

---

## Using it

### Runtime

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

### Values

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

### Go values in

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

### Go functions in

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

### JavaScript values out

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

### Errors

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

### Deadlines

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

### Promises

Nothing settles until the job queue runs, and the checkpoint is explicit so a
host stays in control of when its callbacks fire.

```go
v, err := rt.RunString(`(async () => { … })()`)
res, err := rt.Await(v)   // drains the queue, unwraps; a rejection becomes *Error
```

`Await` passes a non-promise straight through, so it is safe to wrap around any
result. `RunJobs` is the bare drain.

### Scopes and pools

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

### JSON, as bytes

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

## Status

Working, and in production for Function-node scripts. What is not there yet:

- **JIT.** Ported (`internal/gomir`, `internal/jitmem`) but not the default
  tier. Compute-bound JavaScript runs on the interpreter today, which is the
  23–430× in the [Octane table](#octane-20).
- **Per-function `[[Realm]]`.** The one remaining Test262 core failure: a
  revoked Proxy must report the TypeError of the realm its function came from,
  and goant has a single realm's worth of intrinsics to reach for.
- **Temporal.** Not implemented.
- **Intl.** Stubs that ignore locale and options.
- **Host modules.** No `fs`, no `http`, no Node compatibility layer — the engine
  plus a minimal runtime (event loop, timers, microtasks, `console`) is the
  whole scope. Give a script what it needs with `Set`.
- **Threads.** `SharedArrayBuffer` and `Atomics` are single-agent.

---

## Building

```sh
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...

CGO_ENABLED=0 go build -o goant ./cmd/goant        # the CLI
./goant script.js
./goant -e 'console.log(1 + 1)'
```

Verified to cross-compile with `CGO_ENABLED=0` for linux/amd64, linux/arm64,
darwin/amd64, darwin/arm64, windows/amd64 and windows/arm64 — from any of them,
with no toolchain beyond Go.

### Conformance harnesses

```sh
CGO_ENABLED=0 go build -o goant ./cmd/goant
CGO_ENABLED=0 go build -o goant-conf ./cmd/goant-conf
CGO_ENABLED=0 go build -o goant-t262 ./cmd/goant-t262

./goant-conf --runner ./goant --profile interp     # ant's corpus: 1511/1511
./goant-t262 -core -t262 ../test262                # ECMA-262 core profile
./goant-t262 -all  -t262 ../test262                # every file, nothing skipped
./goant-t262 -core -t262 ../test262 -runner goja-run   # the core profile, for goja
```

`-core` and `-all` are the two numbers in [Conformance](#conformance); `-core`
prints what it skipped and why with `-show-skip`, so its exclusion list is
checkable rather than something you have to take on trust.

---

## Layout

| path | purpose |
|---|---|
| `goant.go`, `value.go`, `object.go`, `convert.go`, `scope.go`, `json.go`, `errors.go` | the embedding API |
| `v8go/` | drop-in v8go-compatible binding |
| `internal/engine` | the engine: values, object model, compiler, interpreter, GC, built-ins |
| `internal/regexpjs`, `internal/regexp2` | JS regex translation over a vendored regexp2 |
| `internal/gomir`, `internal/jitmem` | MIR ported to Go; JIT executable memory |
| `cmd/goant` | CLI (`goant file.js`, `-e`, `--parse`, `--disasm`) |
| `cmd/goant-conf`, `cmd/goant-t262`, `cmd/goant-mjsunit` | conformance harnesses |
| `tools/` | generators (opcodes, Unicode tables, Intl data, corpus) |
| `bench/`, `conformance/` | benchmarks and the test corpus |

[PLAN.md](PLAN.md) has the architecture and [TODO.md](TODO.md) the
phase-by-phase checklist.

## Credits

goant is a port of the Silver engine from
[`theMackabu/ant`](https://github.com/theMackabu/ant); the architecture, the
opcode set and the conformance bar are all its. The regex engine is a fork of
[`dlclark/regexp2`](https://github.com/dlclark/regexp2). The `v8go` package
mirrors the API of [`rogchap/v8go`](https://github.com/rogchap/v8go).
