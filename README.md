<p align="center">
  <img src="docs/goant-gopher.png" alt="goant" width="300">
</p>

# goant

A JavaScript engine in pure Go, built to embed in Go applications. It passes
**99.4% of test262** with nothing excluded, and ships an opt-in baseline JIT for
amd64 and arm64. No cgo, nothing to fetch at build time, one static binary, and
it cross-compiles wherever Go does.

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

## The road here

[Robomotion](https://www.robomotion.io) is an RPA platform whose robot runtime is written in Go. 
Even in a low-code platform, users still need a scripting language for cases where visual building 
blocks are not enough, so we chose JavaScript from the beginning.

Robomotion is built around a Node-RED-inspired messaging architecture. This allows automation flows to 
process messages concurrently and execute multiple branches in parallel, a capability that is uncommon
in traditional RPA products.

The runtime executes customer JavaScript inside Function nodes, typically one script per message, 
across millions of messages. It runs on Linux servers, Windows servers, Intel and Apple Silicon Macs, 
and Raspberry Pi devices. Over the past seven years, we have used four different JavaScript engines.

Join our 5000+ [Robomotion Discord](https://community.robomotion.io) community.

<img width="1894" height="938" alt="image" src="https://github.com/user-attachments/assets/95844799-8169-43bf-8c37-f96e74a6a650" />
<img width="1614" height="862" alt="image" src="https://github.com/user-attachments/assets/1f43a034-e811-4ee9-9147-a37bbf2dc79f" />

**[otto](https://github.com/robertkrimen/otto)** (2019), pure Go. Dropped for
the dialect. It is ES5 only, and it uses Go's `regexp` package, so there is no
lookahead and there are no backreferences. It passes 20.9% of test262.

**[duktape](https://github.com/olebedev/go-duktape)** (2021), C through cgo.
Still ES5, and now a C toolchain in every build on every platform we ship. The
binding was archived four months after we moved off it.

**[rogchap.com/v8go](https://github.com/rogchap/v8go)** (2021 to 2026). Current
JavaScript at last. We used it for five years, and shipping on every platform we
support was the hard part. v8go vendors prebuilt V8 as static archives, one per
platform, and the set is not the same from one release to the next.
[v0.6.0](https://github.com/rogchap/v8go/releases), in May 2021, was the first
release with a Windows binary. v0.7.0, that December, took it back out:
*"Removed Windows support until its build issues are addressed."* The same
release is the one that added Apple Silicon and linux/arm64. So the version with
Windows and the version with arm64 were never the same version.

Windows is our largest install base, so Windows stayed on v0.6.0 and the V8 it
carries from April 2021, while Apple Silicon, Intel Mac and Linux moved on to
later releases. Worse, the interface changed underneath: code written against
v0.6.0 does not compile against v0.9.0, because `NewIsolate`, `NewContext`,
`NewObjectTemplate`, `NewFunctionTemplate` and `Context.Isolate` all return an
error in the first and a bare value in the second. Build tags do not get you out
of it either, because a module pins one version of a dependency for the whole
build: `//go:build windows` selects a different file, not a different v8go. What
we did was keep two branches of our own code, one for each v8go version.

**[Our own v8go fork](https://github.com/robomotionio/v8go)** landed in 2026. We
restored Windows support on both amd64 and arm64 with clang-cl, and upgraded V8
to 14.7.

It works, but it also means maintaining an LLVM toolchain for every platform.

V8 14 forced us to standardize on Clang across platforms. Linux uses Clang,
macOS needs a newer Homebrew Clang with a small libc++ patch, and Windows uses
clang-cl because Go's default MinGW toolchain is not ABI-compatible with V8.

Toolchains were only part of the problem. Two other issues made V8 difficult to
live with.

The first was memory.

V8 keeps some per-isolate state that garbage collection cannot reclaim. Over
time, a pooled isolate keeps growing until it reaches its roughly 1.4 GB limit.
At that point, the process dies instead of returning an error we can handle.

The second problem came from our own architecture.

Messages used to have a size limit, so very large payloads never reached V8.
Then we added Large Message Objects, which store large values outside the
message itself. Suddenly, flows could carry tens of megabytes.

Passing one of those messages into V8 caused several copies along the way: Go
bytes, Go string, C string, then V8 string. A 13 MB message could temporarily
use around 50 MB of memory.

On Windows, that was enough to kill the process even with several gigabytes of
RAM still free.

The reason is that Windows commits memory up front. If the system is close to
its commit limit, even a temporary 50 MB allocation can fail. Linux overcommits
memory, so we never saw the same problem there.

The practical fix was outside our code: increase the Windows page file, disable
memory compression, and reboot. In an enterprise environment that is hard to
explain and harder to get approved.

We also tried fixing it in software.

External strings let V8 point directly at the Go memory instead of copying it.
That worked well for ASCII, but non-ASCII text could put us back on the copying
path.

Then we built `LazyMessage`.

Instead of passing the whole message into V8, `LazyMessage` puts a Proxy in
front of it. Data only crosses into Go when the script actually reads a field.

That solved the memory problem, but created a performance problem.

A loop over 10,000 records went from 7.9 ms to 19.7 seconds.

Crossing into Go was not the expensive part. The real cost was that every
property read had to rescan the message to find the requested field.

So neither approach worked well for every flow.

We shipped `LazyMessage` behind a feature flag:

`--enable-features=LazyMessage`

Customers can choose based on the workload: lower memory usage for large
messages, or better performance for normal ones.

That became the pattern with V8. With a language and ABI boundary in the middle,
many fixes do not remove the cost. They just move it somewhere else.

**[goant](https://github.com/robomotionio/goant)** (2026). Ant came up on
[Hacker News](https://news.ycombinator.com/item?id=48875377) as *"Show HN: Ant,
a JavaScript runtime and ecosystem"*. Its engine, Silver, is written in C, which
is near enough to Go to port from rather than reimplement. Bun had just
[rewritten 535,000 lines of Zig into Rust](https://bun.com/blog/bun-in-rust) in
eleven days with AI agents, so C to Go looked like a smaller version of a bet
someone had already won. Once the port ran we started on test262, then on a
baseline JIT, and that is where we are.

## Why not goja?

[**goja**](https://github.com/dop251/goja) is the obvious choice if you want a
pure-Go JavaScript engine. It is fast, mature, and the baseline used in the
benchmarks below.

The problem for us is JavaScript compatibility.

Our users often do not write the JavaScript themselves. They ask an AI to
generate it, paste it into an automation, and run it. AI-generated code tends to
use current JavaScript features, so missing language and runtime features
quickly become production problems.

### Regular expressions

Regex is especially important for automation workloads. goja currently passes
53% of test262's RegExp suite.

```js
// "strip anything that isn't a letter or a digit"
"Ankara-2026!".replace(/[^\p{L}\p{N}]+/gu, "_")

goja  "_"
goant "Ankara_2026_"
```

goja does not support Unicode property escapes such as `\p{L}` and `\p{N}`. More
importantly, it does not reject them.

```js
"Grüße, Ankara".match(/\p{L}+/gu)
```

Instead of throwing an error, goja returns `null`.

That makes this more than a missing feature. The script runs successfully but
produces the wrong result.

Unicode property escapes are also common in AI-generated regex whenever text may
contain non-ASCII characters.

### Intl

goja does not implement `Intl`.

For automation code this affects much more than currency formatting.

```js
new Intl.NumberFormat("de-DE", {
  style: "currency",
  currency: "EUR"
}).format(1234.5)

goja  ReferenceError: Intl is not defined
goant "1.234,50 €"
```

`Intl` is also the standard JavaScript API for locale-aware numbers, dates,
sorting, relative time, lists, pluralization, and other common formatting tasks:

```js
new Intl.DateTimeFormat("tr-TR").format(date)

new Intl.Collator("de").compare(a, b)

new Intl.RelativeTimeFormat("en").format(-1, "day")

new Intl.ListFormat("en").format(["Alice", "Bob", "Carol"])

new Intl.PluralRules("en").select(2)
```

These APIs appear frequently in generated JavaScript because they are the normal
browser and Node.js solution to localization problems.

The absence of `Intl` also affects APIs that use locale formatting indirectly:

```js
(1234567.891).toLocaleString("en-US", {
  maximumFractionDigits: 2
})

goja  RangeError: toString() radix argument must be between 2 and 36
goant "1,234,567.89"
```

Here the options object ends up reaching `Number.prototype.toString`, where it
is interpreted as the radix argument. Code that should format a number therefore
fails for a reason unrelated to the code the user wrote.

### Newer JavaScript APIs

The same issue appears with newer built-ins:

```js
// group records by region
Object.groupBy(orders, o => o.region)

goja  TypeError: Object has no member 'groupBy'
goant { EU: [...], US: [...] }
```

```js
// take the first four multiples of three
[...naturals().filter(n => n % 3 === 0).take(4)]

goja  TypeError: Object has no member 'filter'
goant [0, 3, 6, 9]
```

goja supports many modern JavaScript features, including `at`, `toSorted`,
`with`, `findLast`, `Object.hasOwn`, `replaceAll`, and `??=`. Its RegExp
implementation also supports lookbehind, named groups, named backreferences, and
the `s` and `y` flags.

The gaps that matter to us are the ones that show up in ordinary automation
code: `Intl`, Unicode property escapes, newer RegExp flags, `Object.groupBy`,
iterator helpers, and other APIs that current browsers, Node.js, and
AI-generated JavaScript commonly assume are available.

For our workload, supporting the JavaScript users actually paste into Function
nodes matters more than broad compatibility with an older JavaScript surface.

There is also the installed base. Our customers have hundreds of thousands of
Function nodes written and working in production today. Moving from V8 to goja
would put all of that on a smaller language surface at once, and not every
failure would announce itself: the `\p{L}` case above returns the wrong answer
rather than an error.

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
CLDR-driven formatting. Of the 326 failures left, two thirds are SpiderMonkey's
imported suite; [Status](#status) accounts for the rest.

---

## Benchmarks

Measured on an idle Azure `Standard_D8s_v5` created for the purpose. Scripts are
in the repository; see [Reproducing](#reproducing).

### Octane 2.0

The eight workloads [ahaoboy/js-engine-benchmark](https://github.com/ahaoboy/js-engine-benchmark)
scores. Higher is better. These are the engines in goant's own class: small,
embeddable, shipped as one binary. That is the comparison that says something.
None of them has an optimizing JIT.

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

An *optimizing* JIT is still one to two orders of magnitude ahead: node, on the
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

## Using it

```go
rt := goant.New(goant.WithJIT(true))   // the baseline JIT, off by default
defer rt.Close()

rt.SetMemoryLimit(64 << 20)   // an error, not an abort

stop := rt.WithContext(ctx)   // ctx's deadline interrupts the script
defer stop()

rt.Set("fetchRow", func(id int) map[string]any { ... })   // Go in
v, err := rt.RunString(`JSON.stringify(fetchRow(7))`)     // JS out
```

[**docs/embedding.md**](docs/embedding.md) is the full API: values, converting
Go in and JavaScript out, errors, deadlines, promises, scopes and pools, the
bytes-in/bytes-out JSON path, and migrating from v8go.

## Status

**Used in production.** It is embedded in Robomotion RPA
[robots](https://robomotion.io/downloads) that run user-provided scripts with
time and memory limits, on Linux, macOS, and Windows across amd64 and arm64.

There is no tagged release yet, so `go get` must pin a specific commit. The root
package and `v8go/` are intended to remain stable. `internal/engine` is not a
public API and may change.

What is still missing:

- **An optimizing JIT.**

  The current JIT is a baseline compiler, not an optimizing one. It has one
  template per bytecode, inline caches, type feedback for element access, direct
  compiled-to-compiled calls, mid-function deoptimization, and no inlining.

  JIT is enabled per Runtime with `WithJIT(true)` and is off by default. The
  reason is simple: "safe when a host explicitly enables and monitors it" is not
  the same as "safe for everyone after an upgrade."

  It has been tested with differential fuzzing against the interpreter on four
  platforms, Test262, mjsunit, race-detector concurrency tests, and
  multi-million-call stress tests. The only supported platform without a stress
  test is `darwin/amd64`, because there is currently no hardware available for
  it.

- **326 Test262 failures.**

  These are mainly:

  - 211 failures in `staging/sm`, SpiderMonkey's test suite. These are real
    semantic gaps, but mostly many small issues rather than one major missing
    feature.
  - 47 failures for proposals not implemented yet, including decorators, import
    defer, import bytes, and source-phase imports.
  - About 50 failures related to per-function `[[Realm]]`. Some errors must come
    from the realm where the function was created, but goant currently uses the
    realm from the current stack.
  - 12 failures related to growable `SharedArrayBuffer`.

  `SharedArrayBuffer` and `Atomics` currently support only a single agent. There
  is no thread support.

- **Some array built-ins can bypass the memory limit.**

  The memory limit tracks engine objects and external payloads. However, some
  built-ins grow normal Go slices without checking the memory budget.

  This affects operations such as `fill`, `copyWithin`, `includes`, and
  `String.raw` when used with array-like objects that claim extremely large
  lengths, such as `{length: 2**53-1}`.

  Methods such as `toReversed`, `with`, `toSorted`, and `toSpliced` may also
  allocate their result based on the claimed length before reading any elements.

  Some long-running native loops also do not check the interrupt flag, so they
  cannot currently be cancelled.

  The cases that previously crashed the whole process have been fixed. Proper
  memory accounting for these remaining cases has not.

- **No host modules.**

  There is no `fs`, `http`, or Node.js compatibility layer.

  The scope is the JavaScript engine plus a small runtime with an event loop,
  timers, microtasks, and `console`.

  If a script needs access to host functionality, provide it explicitly with
  `Set`.

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
