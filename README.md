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
| Out of memory     | **an error you can catch**¹    | takes the process down | takes the process down          |
| Per-run isolation | **a fresh global, 111 ns**     | a fresh Runtime    | a fresh context                     |

¹ With `SetMemoryLimit` set. It is judged on what survives a collection, so a
script that allocates a great deal and keeps little still passes. Some array
builtins can still exhaust memory outside it — see [Status](#status).

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

Of the 326 remaining failures, **211 are in `staging/sm`** — SpiderMonkey's own
suite, imported into test262's staging area. Of the other 115: 47 are proposals
that have not landed, about 50 sit behind one root cause (per-function
`[[Realm]]`), 12 are growable `SharedArrayBuffer`, and six are a tail of
singletons. [Status](#status) has the breakdown.

---

## Benchmarks

Measured on an idle Azure `Standard_D8s_v5` created for the purpose and
destroyed afterwards. Scripts are in the repository; see
[Reproducing](#reproducing).

The Octane table stands goant beside the engines in its own class — embeddable
interpreters with no optimising JIT. Cold start and binary size also list node,
deno and bun, which are whole runtimes rather than peers and are there for
scale.

### Octane 2.0

The eight workloads [ahaoboy/js-engine-benchmark](https://github.com/ahaoboy/js-engine-benchmark)
scores. Higher is better. These are the engines in goant's own class — no JIT,
embeddable, one binary — which is the comparison that says something.

| Benchmark    |    goant | goant+JIT |     goja |      ant |  quickjs | duktape |
| ------------ | -------: | --------: | -------: | -------: | -------: | ------: |
| Richards     |      227 |  **1374** |      218 |      445 |      836 |     221 |
| DeltaBlue    |      258 |   **929** |      271 |      914 |      818 |     269 |
| Crypto       |      257 |  **2320** |      127 |      274 |     1099 |     347 |
| RayTrace     |      431 |       557 |      304 |      691 |  **995** |     551 |
| EarleyBoyer  |      536 |       822 |      540 |     1111 | **1598** |     580 |
| RegExp       |      238 |   **276** |      212 |      152 |      268 |     123 |
| Splay        |     1874 |      2320 |     2138 |     2137 | **3426** |    2753 |
| NavierStokes |      467 |  **6012** |      202 |    error |     1971 |    1224 |

`goant` is the interpreter, which is what runs unless a host asks for
[the tier](docs/embedding.md#the-compiled-tier); `goant+JIT` is the same binary
with `WithJIT(true)`. Bold marks the fastest engine in each row.

Interpreter against interpreter, goant leads goja on five of eight — the
like-for-like pair, both pure Go — and loses to everything written in C. **ant,
the engine goant is a port of, is 1.1x to 3.5x faster on six of the seven it
scores**, which is the honest answer to "what did the port cost": a bytecode
loop in Go with NaN-boxed handles gives up a lot to the same loop in C. quickjs
is 1.1x to 4.3x ahead on all eight. RegExp is the single interpreter row goant
wins outright, which is the RE2 fast path rather than the loop.

The tier is what closes it. With `WithJIT(true)` goant is fastest on five of
the eight, including NavierStokes at 6012 against quickjs's 1971 and Crypto at
2320 against 1099. It stays behind quickjs on RayTrace, EarleyBoyer and Splay:
a baseline compiler makes arithmetic, property access and calls cheaper, and
those three are dominated by allocation and the collector instead.

Over all fifteen Octane workloads the tier is worth **3.1x** — 6.5x on the three
asm.js-shaped ones (zlib, mandreel, gbemu) and 2.6x on the other twelve. On the
workload this engine was built for, short flows on a pooled `Runtime`, **2.9x**.
`code-load` is the one workload the tier makes worse, which is what a benchmark
that measures compiling rather than running should do.

A JIT runtime is still one to two orders of magnitude ahead on this suite: node
scored 34,924 on Richards against goant+JIT's 1374 (from the earlier run — node
is not in the table above). If your scripts are compute-bound and you can afford
cgo, that gap is the reason to reach for V8.

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

One machine, one sitting, nothing else running on it. Every Octane column was
measured together with `-refresh`, so no engine's score was read from a cached
baseline. `ant` fails on NavierStokes rather than scoring it.

The all-fifteen tier figures (3.1x, 6.5x, 2.6x) are from an earlier run of the
same suite on an identically specified instance.

| Detail   | Value                                            |
| -------- | ------------------------------------------------ |
| Hardware | Azure `Standard_D8s_v5` — Xeon Platinum 8370C @ 2.80 GHz, 8 vCPU, 31 GB |
| OS       | Ubuntu 24.04.4 LTS (x86_64), kernel 6.17          |
| Go       | 1.26.3, `CGO_ENABLED=0`                           |
| Octane   | chromium/octane @ `570ad1cc`                      |
| test262  | tc39/test262 @ `b363f29d`                         |
| goja     | `v0.0.0-20260723142020-b4aef50fa347`              |
| node / deno / bun | 22.23.2 / 2.9.5 / 1.3.14                      |
| ant / quickjs / duktape | ant @ HEAD / 2021-03-27 / 2.7.0         |

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
rewritten in Go: lexer → AST → bytecode compiler → 218-opcode interpreter, its
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
  bytecode, inline caches, per-site type feedback for element access, compiled
  calls that reach compiled code directly, mid-body deopt, and no inlining. It
  is opt-in per Runtime with `WithJIT(true)`, off by default because "safe for
  a host that opts in and watches it" is a different claim from "safe for
  everyone on upgrade". It has had differential fuzzing against the interpreter
  on four platforms, Test262 and mjsunit with the tier on, a race-detector
  concurrency suite, and multi-million-invocation soaks — but no soak on
  `darwin/amd64`, the one supported platform with no hardware behind it.
- **`staging/sm`, 211 test262 failures.** SpiderMonkey's own suite, and the
  largest single bucket left by some way. Mostly real semantic gaps rather than
  harness noise, but a long tail rather than a lever.
- **Unlanded proposals**, 47 failures: decorators, import-defer, import-bytes,
  source-phase imports.
- **Per-function `[[Realm]]`**, about 50 failures behind one root cause: a value
  must report the error of the realm its function came from, and goant reaches
  for the realm on the stack.
- **Growable `SharedArrayBuffer`**, 12 failures, and threads — `SharedArrayBuffer`
  and `Atomics` are single-agent.
- **Some array builtins escape the memory limit.** The limit is charged on
  engine cells and out-of-line payload, so a builtin that grows a plain Go
  slice — `fill`, `copyWithin`, `includes`, `String.raw` against a
  `{length: 2**53-1}` array-like — grows without the budget being consulted,
  and `toReversed`/`with`/`toSorted`/`toSpliced` size their result from a
  claimed length before reading an element. A few long native loops also do not
  check the interrupt flag, so they cannot be cancelled. The claimed-length
  paths that used to crash the process outright are fixed; making these charge
  the limit is not.
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
