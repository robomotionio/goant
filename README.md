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

|                   | goant                          | goja               | V8 via cgo                          |
| ----------------- | ------------------------------ | ------------------ | ----------------------------------- |
| cgo               | **none**                       | none               | required                            |
| Cross-compile     | **anywhere Go does**           | anywhere Go does   | a C toolchain, plus a prebuilt V8 archive per platform |
| Test262 (`-all`)  | **99.4%**, 53,247 / 53,573     | 64.2%              | current                             |
| Engine            | bytecode interpreter + **baseline JIT** (amd64, arm64, opt-in) | bytecode interpreter, no JIT | optimising JIT |
| Binary size       | **11.1 MB**                    | 13.3 MB            | ~90 MB linked                       |
| Cold start        | **2.5 ms**                     | 2.0 ms             | n/a                                 |
| Out of memory     | **an error you can catch**¹    | takes the process down | takes the process down          |
| Per-run isolation | **a fresh global, 111 ns**     | a fresh Runtime    | a fresh context                     |

¹ With `SetMemoryLimit` set. It is judged on what survives a collection, so a
script that allocates a great deal and keeps little still passes. Some array
builtins can still exhaust memory outside it; see [Status](#status).

goant is for a Go program that has to run JavaScript it did not write, on
machines it does not control, without the process dying when that JavaScript
misbehaves. If your scripts are compute-bound and you can afford cgo, V8 is
still faster at running them; see [Benchmarks](#benchmarks).

## The road here

[Robomotion](https://www.robomotion.io) is an Agentic RPA platform. Robot runtime evaluates customer
JavaScript in Function nodes: a script per message, millions of messages, on
Windows laptops, Raspberry Pis, Apple Silicon and Linux servers. It has run four
JavaScript engines in seven years.

<img width="1894" height="938" alt="image" src="https://github.com/user-attachments/assets/95844799-8169-43bf-8c37-f96e74a6a650" />
<img width="1614" height="862" alt="image" src="https://github.com/user-attachments/assets/1f43a034-e811-4ee9-9147-a37bbf2dc79f" />

**[otto](https://github.com/robertkrimen/otto)** (2019), pure Go. Dropped for
the dialect. It is ES5 only, and it uses Go's `regexp` package, so there is no
lookahead and there are no backreferences. It passes 20.9% of test262.

**[duktape](https://github.com/olebedev/go-duktape)** (2021), C through cgo.
Still ES5, and now a C toolchain in every build on every platform we ship. The
binding was archived four months after we moved off it.

**[rogchap.com/v8go](https://github.com/rogchap/v8go)** (2021 to 2026). Current
JavaScript at last, and five years of paying for it. v8go vendors prebuilt V8 as
static archives, one per platform, and the set changed under us.
[v0.6.0](https://github.com/rogchap/v8go/releases), in May 2021, was the first
release with a Windows binary. v0.7.0, that December, took it back out:
*"Removed Windows support until its build issues are addressed."* The same
release is the one that added Apple Silicon and linux/arm64. So the version with
Windows and the version with arm64 were never the same version.

Windows is our largest install base, so Windows stayed on v0.6.0 and the V8 it
carries from April 2021, while Apple Silicon, Intel Mac and Linux moved on to
later releases. One product, two V8 versions, split by operating system: which
JavaScript a customer's script met depended on which machine the robot was
running on.

**[Our own v8go fork](https://github.com/robomotionio/v8go)** (2026). Windows
restored on amd64 and arm64 with clang-cl, and V8 moved up to 14.7. It works. It
also means a C++ toolchain per platform that is now ours to keep building.

Two other things wore V8 out. It keeps per-isolate state that GC cannot reclaim,
so a pooled isolate eventually reaches its 1.4 GB ceiling and the process dies
instead of throwing. And a large message cost three copies to cross cgo: one Go
string, one C string, one V8 string. On Windows that was fatal by itself. We
routed around it with a `Proxy` over the message, which removed the copies and
then made a loop over 10,000 records take 19.7 s instead of 7.9 ms. That is not
a fix, it is a choice of which failure to have.

**goant** (2026). No cgo boundary, so there is nothing to marshal and no
`abort()` to catch. [The long version](docs/why-goant-exists.md).

## Why not goja?

goja is the obvious answer if you want off cgo. It is good, and it is the
yardstick in every benchmark below. The problem is which JavaScript it speaks.

Our users often do not write the JavaScript themselves. They ask an AI for it,
paste the result into an automation, and run it. What comes back is written
against today's language, not the one an engine stopped at. That code does not
fail in strange ways. It fails on ordinary things.

Regex is where it hurts most. Automations lean on regex heavily, and what an AI
writes is rarely a simple pattern. goja passes 53% of test262's RegExp suite.

```js
// "strip anything that isn't a letter or a digit"
"Ankara-2026!".replace(/[^\p{L}\p{N}]+/gu, "_")
goja   "_"
goant  "Ankara_2026_"

// "format this as euros"
new Intl.NumberFormat("de-DE", { style: "currency", currency: "EUR" }).format(1234.5)
goja   ReferenceError: Intl is not defined
goant  1.234,50 €

// "show it with thousands separators"
(1234567.891).toLocaleString("en-US", { maximumFractionDigits: 2 })
goja   RangeError: toString() radix argument must be between 2 and 36
goant  1,234,567.89

// "group these records by region"
Object.groupBy(orders, o => o.region)
goja   TypeError: Object has no member 'groupBy'
goant  { EU: [...], US: [...] }

// "take the first four multiples of three"
[...naturals().filter(n => n % 3 === 0).take(4)]
goja   TypeError: Object has no member 'filter'
goant  0,3,6,9
```

The first is the sharpest, and on its own it settles the question for us. goja
has no Unicode property escapes, and does not reject them either. `\p{L}`
matches nothing, so the negated class matches everything and the string is
erased; `"Grüße, Ankara".match(/\p{L}+/gu)` returns `null` for the same reason.
No error, no warning, just the wrong answer, in a flow that was working. And
`\p{L}` is what an AI reaches for the moment the text is not ASCII.

The `toLocaleString` case has the same shape: with no `Intl` at all, the options
object falls through to `Number.prototype.toString`, which reads it as a
*radix*. Neither is a missing feature declining politely.

Be fair about the size of the gap: goja **does** have `at`, `toSorted`, `with`,
`findLast`, `Object.hasOwn`, `replaceAll` and `??=`, and its regex engine does
lookbehind, named groups, named backreferences, `s` and `y`. Of fifteen regex
features tried it passed eleven, missing property escapes and the `d` and `v`
flags. Every example above was run against both engines before it was written
down. What is missing is the last few years: `Intl`, property escapes, iterator
helpers, `Object.groupBy`, `Promise.withResolvers`, `Array.fromAsync` and
modules. That is exactly the vintage an AI writes in.

---

## Conformance

**99.4% of test262, with nothing excluded.** Not a profile, not a subset.
`./goant-t262 -all` runs every file in the suite.

| suite | goant | goja | otto |
|---|---|---|---|
| **Test262** (`-all`, every file) | **53,247 / 53,573, 99.4%** | 34,377, 64.2% | 11,205, 20.9% |
| Test262 `built-ins/RegExp` | **1,877 / 1,879, 99.9%** | 997, 53.1% | 538, 28.6% |
| ant's compat-table corpus (ES1–ESNext) | **1514 / 1514** | n/a | n/a |
| V8's `mjsunit` | 2,711 / 3,149 | n/a | n/a |

All three were scored on the same test262 checkout through the same harness, so
the numbers mean the same thing, and they are the three pure-Go engines in the
order we ran them. `RegExp` is broken out because RPA flows lean on it heavily;
the regex *literal* grammar is a second measurement, at 238 / 238 against goja's
210 and otto's 40. Neither goja nor otto has an ES module goal, so the runner
declines module tests rather than emulating them.

At 100%, whole directories: `built-ins/Temporal` (4,603), `annexB` (1,086),
`built-ins/Promise` (729), `built-ins/Date` (594), `built-ins/Array/prototype`,
`built-ins/JSON`, `built-ins/Proxy`. `intl402` is 3,356 / 3,357 and
`built-ins/RegExp` 1,877 / 1,879. **Temporal and Intl are implemented**, both to
essentially the whole suite, including the sixteen non-ISO calendars and
CLDR-driven formatting. See
[docs/intl402-and-temporal.md](docs/intl402-and-temporal.md). Of the 326
failures left, two thirds are SpiderMonkey's imported suite; [Status](#status)
accounts for the rest.

---

## Benchmarks

Measured on an idle Azure `Standard_D8s_v5` created for the purpose. Scripts are
in the repository; see [Reproducing](#reproducing).

### Octane 2.0

The eight workloads [ahaoboy/js-engine-benchmark](https://github.com/ahaoboy/js-engine-benchmark)
scores. Higher is better. These are the engines in goant's own class: small,
embeddable, shipped as one binary. That is the comparison that says something.
None of them has an optimising JIT.

| Benchmark    |    goant | goant+JIT |     goja |     otto |      ant |  quickjs | duktape |
| ------------ | -------: | --------: | -------: | -------: | -------: | -------: | ------: |
| Richards     |      227 |  **1374** |      218 |       22 |      445 |      836 |     221 |
| DeltaBlue    |      258 |   **929** |      271 |       20 |      914 |      818 |     269 |
| Crypto       |      257 |  **2320** |      127 |       26 |      274 |     1099 |     347 |
| RayTrace     |      431 |       557 |      304 |       82 |      691 |  **995** |     551 |
| EarleyBoyer  |      536 |       822 |      540 |       84 |     1111 | **1598** |     580 |
| RegExp       |      238 |   **276** |      212 |    error |      152 |      268 |     123 |
| Splay        |     1874 |      2320 |     2138 |      527 |     2137 | **3426** |    2753 |
| NavierStokes |      467 |  **6012** |      202 |       41 |    error |     1971 |    1224 |

`goant` is the interpreter, which is what runs unless a host asks for
[the JIT](docs/embedding.md#the-jit); `goant+JIT` is the same binary with
`WithJIT(true)`. Bold marks the fastest engine in each row.

Interpreter against interpreter, goant leads goja on five of eight. That is the
like-for-like pair, both pure Go. It trails the C engines almost everywhere.
**ant, the engine goant is a port of, is 1.1x to 3.5x faster on six of the seven
it scores**, which is the honest answer to "what did the port cost": a bytecode
loop in Go with NaN-boxed handles gives up a lot to the same loop in C. quickjs
is 1.1x to 4.3x ahead on all eight, so **the goant interpreter wins no row
outright**. It comes closest on RegExp, which is the RE2 fast path rather than
the loop. otto is the other end of the range: 3.6x to 12.9x behind goant on the
seven it scores, and it cannot run the eighth at all. Octane's RegExp benchmark
uses lookahead, and Go's `regexp` has no syntax for it.

The JIT is what closes it: with `WithJIT(true)` goant is fastest on five of the
eight. It stays behind quickjs on RayTrace, EarleyBoyer and Splay, which are
dominated by allocation and the collector rather than by the arithmetic,
property access and calls a baseline compiler makes cheaper. Over all fifteen
Octane workloads the JIT is worth **3.1x**: 6.5x on the three asm.js-shaped
ones (zlib, mandreel, gbemu), 2.6x on the other twelve, and **2.9x** on the
workload this engine was built for, short flows on a pooled `Runtime`.
`code-load` is the one it makes worse, which is what a benchmark that measures
compiling rather than running should do.

An *optimising* JIT is still one to two orders of magnitude ahead: node, on the
same machine, scores 33,300 on Richards against goant+JIT's 1374. If your
scripts are compute-bound and you can afford cgo, that gap is the reason to
reach for V8.

### Cold start and size

Time to start, evaluate one line, and exit. hyperfine `--shell=none`, 20 warmup
and 200 timed runs, all nine in one sitting on the same idle machine. node, deno
and bun are whole runtimes rather than peers and are listed for scale.

| Runtime   |    Cold start | Binary      | Links against |
| --------- | ------------: | ----------: | ------------- |
| quickjs   |        0.8 ms |      1.0 MB | system libs   |
| duktape   |        1.1 ms |      0.3 MB | system libs   |
| otto      |        1.9 ms |      7.1 MB | nothing       |
| goja      |        2.0 ms |     13.3 MB | nothing       |
| **goant** |    **2.5 ms** | **11.1 MB** | **nothing**   |
| ant       |        3.6 ms |     11.8 MB | system libs   |
| bun       |       11.5 ms |     88.5 MB | system libs   |
| deno      |       13.3 ms |     91.2 MB | system libs   |
| node      |       24.0 ms |    119.1 MB | system libs   |

goant is neither the smallest nor the quickest to start. quickjs, duktape and
otto beat it on both. What its 11.1 MB contains is a whole engine: parser,
compiler, interpreter, garbage collector, every built-in, Temporal, Intl, the
Unicode 17 tables and a regex engine, statically linked, with nothing to install
beside it. It was 6.4 MB before Temporal and Intl landed. The dynamically linked
rows are one file plus whatever the system already has, which is not the same
measurement.

<details>
<summary>Environment</summary>

One machine, nothing else running on it, and it is still there. `goant-bench`
caches every comparison engine's score against the host and the hash of the
exact script, so a later run adds a column without re-measuring the others or
mixing in a second machine. The goant, goja, ant, quickjs and duktape columns
were measured together in one sitting with `-refresh`; the otto column was added
later on that same machine, best of two runs like the rest. `ant` fails on
NavierStokes rather than scoring it. The all-fifteen JIT figures (3.1x, 6.5x,
2.6x) are from an earlier run of the same suite on an identically specified
instance.

| Detail   | Value                                            |
| -------- | ------------------------------------------------ |
| Hardware | Azure `Standard_D8s_v5`, Xeon Platinum 8370C @ 2.80 GHz, 8 vCPU, 31 GB |
| OS       | Ubuntu 24.04.4 LTS (x86_64), kernel 6.17          |
| Go       | 1.26.3, `CGO_ENABLED=0`                           |
| Octane   | chromium/octane @ `570ad1cc`                      |
| test262  | tc39/test262 @ `b363f29d`                         |
| goja / otto | `v0.0.0-20260723142020-b4aef50fa347` / `v0.5.1` |
| node / deno / bun | 22.23.2 / 2.9.5 / 1.3.14                      |
| ant / quickjs / duktape | ant @ HEAD / 2021-03-27 / 2.7.0         |

Octane scores are the best of two runs. Splay carries the most run-to-run
variance of the eight, being the one dominated by the collector. The cold-start
and binary-size table is a separate single sitting, all nine measured together
against the same one-line script, on a machine the script refuses to measure on
until it is idle.

**Reproducing:**

```sh
sh bench/suites/fetch.sh                                  # Octane, pinned commit
go build -C bench/gojarun -o "$(go env GOPATH)/bin/goja-run" .
go build -C bench/ottorun -o "$(go env GOPATH)/bin/otto-run" .

CGO_ENABLED=0 go build -o goant ./cmd/goant
CGO_ENABLED=0 go build -o goant-bench ./cmd/goant-bench

./goant-bench -suite octane -only splay -n 2   # one Octane suite, every engine
./goant-bench                                  # the microbenchmarks
```

`goant-bench` scores whichever engines are on PATH and skips the rest.

</details>

---

## What it is

A from-scratch port of the **"Silver" engine** from
[`ant`](https://github.com/theMackabu/ant), a JavaScript runtime written in C,
rewritten in Go: lexer → AST → bytecode compiler → 218-opcode interpreter, its
own tracing garbage collector, WTF-8 strings, a JS→regex translation layer over
a vendored `regexp2`, and a baseline JIT for amd64 and arm64. The engine lives
in `internal/engine` deliberately: everything the interpreter does is free to
change shape as long as the observable behaviour holds. The stable surfaces are
the root package and `v8go/`.

## Using it

```go
rt := goant.New(goant.WithJIT(true))   // the baseline JIT, off by default
defer rt.Close()

rt.Set("fetchRow", func(id int) map[string]any { ... })   // Go in
v, err := rt.RunString(`JSON.stringify(fetchRow(7))`)     // JS out

rt.SetDeadline(50 * time.Millisecond)   // interruptible
rt.SetMemoryLimit(64 << 20)             // an error, not an abort
```

[**docs/embedding.md**](docs/embedding.md) is the full API: values, converting
Go in and JavaScript out, errors, deadlines, promises, scopes and pools, the
bytes-in/bytes-out JSON path, and migrating from v8go.

## Status

**Used in production**, embedded in a Go service that runs scripts it did not
write, under a deadline and a memory limit, on Linux, macOS and Windows across
amd64 and arm64. There is no tagged release yet, so `go get` pins a commit; the
root package and `v8go/` are the surfaces meant to stay put, and
`internal/engine` is explicitly not one.

What is missing:

- **An optimising JIT.** What exists is a *baseline* compiler: one template per
  bytecode, inline caches, per-site type feedback for element access, compiled
  calls that reach compiled code directly, mid-body deopt, and no inlining. It
  is opt-in per Runtime with `WithJIT(true)`, off by default because "safe for a
  host that opts in and watches it" is a different claim from "safe for everyone
  on upgrade". It has had differential fuzzing against the interpreter on four
  platforms, Test262 and mjsunit with it on, a race-detector concurrency suite,
  and multi-million-invocation soaks, but no soak on `darwin/amd64`, the one
  supported platform with no hardware behind it.
- **The 326 test262 failures**: 211 in `staging/sm` (SpiderMonkey's own suite,
  real semantic gaps, but a long tail rather than a lever), 47 unlanded
  proposals (decorators, import-defer, import-bytes, source-phase imports),
  about 50 behind per-function `[[Realm]]`, where a value must report the error
  of the realm its function came from and goant reaches for the realm on the
  stack, and 12 growable `SharedArrayBuffer`. `SharedArrayBuffer` and `Atomics` are
  single-agent; there are no threads.
- **Some array builtins escape the memory limit.** The limit is charged on
  engine cells and out-of-line payload, so a builtin that grows a plain Go
  slice (`fill`, `copyWithin`, `includes`, or `String.raw` against a
  `{length: 2**53-1}` array-like) grows without the budget being consulted, and
  `toReversed`/`with`/`toSorted`/`toSpliced` size their result from a claimed
  length before reading an element. A few long native loops also do not check
  the interrupt flag, so they cannot be cancelled. The paths that used to crash
  the process outright are fixed; making these charge the limit is not.
- **Host modules.** No `fs`, no `http`, no Node compatibility layer. The engine
  plus a minimal runtime (event loop, timers, microtasks, `console`) is the
  whole scope. Give a script what it needs with `Set`.

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
darwin/amd64, darwin/arm64, windows/amd64 and windows/arm64, from any of them,
with no toolchain beyond Go.

<details>
<summary>Conformance harnesses and layout</summary>

```sh
CGO_ENABLED=0 go build -o goant-conf ./cmd/goant-conf
CGO_ENABLED=0 go build -o goant-t262 ./cmd/goant-t262

./goant-conf --runner ./goant --profile interp         # ant's corpus: 1514/1514
./goant-t262 -all -t262 ../test262                     # every file, nothing skipped
./goant-t262 -all -t262 ../test262 -runner goja-run    # the same, for goja
```

| path | purpose |
|---|---|
| `goant.go`, `value.go`, `object.go`, `convert.go`, `scope.go`, `json.go`, `errors.go` | the embedding API |
| `v8go/` | drop-in v8go-compatible binding |
| `internal/engine` | the engine: values, object model, compiler, interpreter, GC, built-ins |
| `internal/regexpjs`, `internal/regexp2` | JS regex translation over a vendored regexp2 |
| `internal/jitasm`, `internal/jitmem` | the JIT: instruction encoders and executable memory |
| `cmd/goant` | CLI (`goant file.js`, `-e`, `--parse`, `--disasm`) |
| `cmd/goant-conf`, `cmd/goant-t262`, `cmd/goant-mjsunit` | conformance harnesses |
| `tools/` | generators (opcodes, Unicode tables, Intl data, corpus) |
| `bench/`, `conformance/` | benchmarks and the test corpus |

[PLAN.md](PLAN.md) has the architecture and [TODO.md](TODO.md) the
phase-by-phase checklist.

</details>

## Credits

goant is a port of the Silver engine from
[`theMackabu/ant`](https://github.com/theMackabu/ant); the architecture, the
opcode set and the conformance bar are all its. The regex engine is a fork of
[`dlclark/regexp2`](https://github.com/dlclark/regexp2). The `v8go` package
mirrors the API of [`rogchap/v8go`](https://github.com/rogchap/v8go).

MIT, like ant. `ATTRIB` says which parts came from where.
