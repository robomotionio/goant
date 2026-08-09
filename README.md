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

|                   | goant                          | goja               | V8 via cgo                          |
| ----------------- | ------------------------------ | ------------------ | ----------------------------------- |
| cgo               | **none**                       | none               | required                            |
| Cross-compile     | **anywhere Go does**           | anywhere Go does   | a toolchain and a 100–210 MB prebuilt archive per platform |
| Test262 (`-all`)  | **99.4%** — 53,247 / 53,573    | 64.2%              | current                             |
| Engine            | bytecode interpreter + baseline JIT | bytecode interpreter | optimising JIT                 |
| JIT               | amd64, arm64 (opt-in)          | none               | optimising                          |
| Binary size       | **11.1 MB**                    | 13.3 MB            | ~90 MB linked                       |
| Cold start        | **2.5 ms**                     | 1.9 ms             | —                                   |
| Out of memory     | **an error you can catch**     | takes the process down | takes the process down          |
| Per-run isolation | **a fresh global, 111 ns**     | a fresh Runtime    | a fresh context                     |

goant is for a Go program that has to run JavaScript it did not write, on
machines it does not control, without the process dying when that JavaScript
misbehaves. If your scripts are compute-bound and you can afford cgo, V8 is
still faster at running them — see [Benchmarks](#benchmarks).

Written for [Robomotion](https://www.robomotion.io), whose robot runtime
evaluates customer JavaScript in Function nodes — a script per message, millions
of messages, on Windows laptops, Raspberry Pis, Apple Silicon and Linux servers.
That ran on V8 through cgo. [Why we left, in
detail](docs/why-goant-exists.md).

---

## Conformance

**99.4% of test262, with nothing excluded.** Not a profile, not a subset —
`./goant-t262 -all` runs every file in the suite.

| suite | goant | goja |
|---|---|---|
| **Test262** (`-all`, every file) | **53,247 / 53,573 — 99.4%** | 34,377 / 53,573 — 64.2% |
| ant's compat-table corpus (ES1–ESNext) | **1514 / 1514** | — |
| V8's `mjsunit` | 2,711 / 3,149 | — |

Both engines were scored on the same test262 checkout through the same harness,
so the two numbers mean the same thing. goja has no ES module goal, so the
runner declines module tests rather than emulating them.

At 100%, whole directories: `built-ins/Temporal` (4,603), `annexB` (1,086),
`built-ins/Promise` (729), `built-ins/Date` (594), `built-ins/Array/prototype`,
`built-ins/JSON`, `built-ins/Proxy`. `intl402` is 3,356 / 3,357 and
`built-ins/RegExp` 1,877 / 1,879.

**Temporal and Intl are implemented**, both to essentially the whole suite,
including the sixteen non-ISO calendars and CLDR-driven formatting. See
[docs/intl402-and-temporal.md](docs/intl402-and-temporal.md).

The 326 remaining failures are roughly a third proposals that have not landed
(decorators, import-defer, import-bytes, source-phase imports), a third one root
cause (per-function `[[Realm]]`), and a third a genuine long tail, most of it in
`staging/`. [Status](#status) has the breakdown.

---

## Benchmarks

Measured on an idle Azure `Standard_D8s_v5` created for the purpose and
destroyed afterwards. Scripts are in the repository; see
[Reproducing](#reproducing).

Read the pure-Go engines against each other. node, deno and bun are the JIT
reference, not peers — the distance to them is what a bytecode interpreter
costs, and how much of it the compiled tier closes.

### Octane 2.0

The eight workloads [ahaoboy/js-engine-benchmark](https://github.com/ahaoboy/js-engine-benchmark)
scores, which is what makes this comparable with what other engines publish.
Higher is better.

| Benchmark    |    goant | goant+JIT |     goja |   node |   deno |    bun | +JIT vs fastest |
| ------------ | -------: | --------: | -------: | -----: | -----: | -----: | --------------: |
| Richards     |  **218** |      1393 |      216 |  34924 |  34352 |  41889 |             30× |
| DeltaBlue    |      251 |      1049 |  **273** |  99554 |  99851 |  58488 |             95× |
| Crypto       |  **241** |      2371 |      129 |  39846 |  42729 |  46841 |             20× |
| RayTrace     |  **467** |       647 |      298 |  73333 |  77477 | 115956 |            179× |
| EarleyBoyer  |  **617** |       967 |      537 |  65363 |  62784 |  72582 |             75× |
| RegExp       |  **255** |       302 |      215 |   9346 |   9748 |  10776 |             36× |
| Splay        |     1989 |      2414 | **2201** |  43982 |  43465 |  43845 |             18× |
| NavierStokes |  **427** |      6123 |      205 |  32467 |  32550 |  34178 |            5.6× |

`goant` is the interpreter, which is what runs unless a host asks for
[the tier](docs/embedding.md#the-compiled-tier); `goant+JIT` is the same binary
with `WithJIT(true)`. Bold marks the higher of the two pure-Go interpreters, the
only like-for-like pair here.

Against goja the interpreter leads on six of eight, geometric mean **1.27×**.
With the tier on, **4.1×**. Against the JIT engines the remaining gap runs 5.6×
to 179×, against 22× to 398× for the interpreter alone.

What the tier is worth varies by an order of magnitude inside one suite —
NavierStokes 14×, Crypto 9.8×, Richards 6.4×, then RegExp and Splay at 1.2×. A
baseline compiler makes arithmetic, property access and calls cheaper; it does
not make the regex matcher or the collector faster.

Over all fifteen Octane workloads the tier is **3.1×** — 6.5× on the three
asm.js-shaped ones (zlib, mandreel, gbemu) and 2.6× on the other twelve. On the
workload this engine was built for, short flows on a pooled `Runtime`, **2.9×**.
`code-load` is the one workload the tier makes worse, which is what a benchmark
that measures compiling rather than running should do.

### Cold start

Time to start, evaluate one line, and exit. hyperfine `--shell=none`, 20 warmup
and 200 timed runs, all five on the same idle machine — below 5 ms hyperfine
cannot calibrate shell startup, so timing through a shell would be measuring
bash as much as the engine.

| Runtime   | Mean        | Relative      |
| --------- | ----------: | ------------- |
| **goja**  | **1.9 ms**  | 1.00          |
| **goant** | **2.5 ms**  | 1.32× slower  |
| bun       |    10.9 ms  | 5.81× slower  |
| deno      |    12.7 ms  | 6.79× slower  |
| node      |    23.5 ms  | 12.56× slower |

### Binary size

| Binary                              |     Size |
| ----------------------------------- | -------: |
| **goant** (`cmd/goant`, `-s -w`)    | **11.1 MB** |
| goja (`bench/gojarun`, minimal)     |  13.3 MB |
| bun                                 |  88.5 MB |
| deno                                |  91.2 MB |
| node                                | 119.1 MB |

goant's is a whole engine — parser, compiler, interpreter, garbage collector,
every built-in, Temporal, Intl, the Unicode 17 tables and a regex engine —
statically linked, with no shared library beside it. It was 6.4 MB before
Temporal and Intl landed; those two and their CLDR data are most of the
difference. node, deno and bun are whole runtimes and are here for scale.

<details>
<summary>Environment</summary>

Two runs, and the tables do not mix within a row. Cold start and binary size are
today's, all five engines on one idle machine. The Octane node/deno/bun columns
are the earlier cross-engine run on an identically specified instance; the two
goant Octane columns are a within-machine A/B measured together, best of five
each, `-refresh` on both arms.

| Detail   | Value                                            |
| -------- | ------------------------------------------------ |
| Hardware | Azure `Standard_D8s_v5` — Xeon Platinum 8370C @ 2.80 GHz, 8 vCPU, 31 GB |
| OS       | Ubuntu 24.04.4 LTS (x86_64), kernel 6.17          |
| Go       | 1.26.3, `CGO_ENABLED=0`                           |
| Octane   | chromium/octane @ `570ad1cc`                      |
| test262  | tc39/test262 @ `b363f29d`                         |
| goja     | `v0.0.0-20260723142020-b4aef50fa347`              |
| node / deno / bun, cold start and size | 22.23.2 / 2.9.5 / 1.3.14 |
| node / deno / bun, Octane              | 25.9.0 / 2.9.3 / 1.3.14  |

Octane scores are the best of two runs; the JIT engines carry more run-to-run
variance than the two interpreters do.

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

`goant-bench` scores whichever engines are on PATH and skips the rest.

---

## What it is

A from-scratch port of the **"Silver" engine** from
[`ant`](https://github.com/theMackabu/ant) — a JavaScript runtime written in C —
rewritten in Go: lexer → AST → bytecode compiler → 213-opcode interpreter, its
own tracing garbage collector, WTF-8 strings, a JS→regex translation layer over
a vendored `regexp2`, and a baseline JIT for amd64 and arm64.

The engine lives in `internal/engine`, deliberately: everything the interpreter
does is free to change shape as long as the observable behaviour holds. The
stable surfaces are the root package and `v8go/`.

## Using it

```go
rt := goant.New(goant.WithJIT(true))   // the compiled tier, off by default
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

In production for Function-node scripts. What is not there yet:

- **An optimising tier.** The JIT is a baseline compiler — one template per
  bytecode, inline caches, per-site type feedback, compiled calls, no inlining
  — and is opt-in per Runtime with `WithJIT(true)`. It is off by default
  because "safe for a host that opts in and watches it" is a different claim
  from "safe for everyone on upgrade". It has had differential fuzzing against
  the interpreter on four platforms, Test262 and mjsunit with the tier on, a
  race-detector concurrency suite, and multi-million-invocation soaks; it has
  not had a soak on `darwin/amd64`, the one platform with no hardware behind it.
- **Unlanded proposals**: decorators, import-defer, import-bytes, source-phase
  imports. ~45 of the 326 test262 failures.
- **Per-function `[[Realm]]`.** One root cause behind ~55 failures: a value must
  report the error of the realm its function came from.
- **`SharedArrayBuffer.prototype.slice`** on growable buffers, and threads —
  `SharedArrayBuffer` and `Atomics` are single-agent.
- **Host modules.** No `fs`, no `http`, no Node compatibility layer — the engine
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
darwin/amd64, darwin/arm64, windows/amd64 and windows/arm64 — from any of them,
with no toolchain beyond Go.

### Conformance harnesses

```sh
CGO_ENABLED=0 go build -o goant ./cmd/goant
CGO_ENABLED=0 go build -o goant-conf ./cmd/goant-conf
CGO_ENABLED=0 go build -o goant-t262 ./cmd/goant-t262

./goant-conf --runner ./goant --profile interp         # ant's corpus: 1514/1514
./goant-t262 -all -t262 ../test262                     # every file, nothing skipped
./goant-t262 -all -t262 ../test262 -runner goja-run    # the same, for goja
```

## Layout

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

## Credits

goant is a port of the Silver engine from
[`theMackabu/ant`](https://github.com/theMackabu/ant); the architecture, the
opcode set and the conformance bar are all its. The regex engine is a fork of
[`dlclark/regexp2`](https://github.com/dlclark/regexp2). The `v8go` package
mirrors the API of [`rogchap/v8go`](https://github.com/rogchap/v8go).

MIT, like ant. `ATTRIB` says which parts came from where.
