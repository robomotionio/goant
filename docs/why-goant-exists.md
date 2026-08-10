# Why goant exists

goant was written for [Robomotion](https://www.robomotion.io), whose robot
runtime evaluates customer JavaScript in Function nodes. This is the long
version of that story: what running V8 through cgo cost, why goja was not the
answer either, and where the engine came from. The [README](../README.md) has
the short one.

---

## Four engines in seven years

One file — the Function node — has had its JavaScript engine replaced three
times before goant. The git log is the shortest honest account of why an engine
like this got written:

| date | change |
| --- | --- |
| 2019-11 | **otto**, vendored. Pure Go, ES5. The code comment: *"25.10.2019 otto.New takes 500 ms!! so we take it from onmessage to oncreate but now we have to use mutex"* — a per-node VM behind a lock, because construction was too expensive to do per message. |
| 2021-04 | **duktape**, via `gopkg.in/olebedev/go-duktape.v3`. Still ES5, and now cgo. |
| 2021-06 | *"Switch to v8"* — `rogchap.com/v8go` v0.6.0, bundling V8 9.0.257.18 from April 2021. |
| 2021-12 | v8go 0.7.0. Reverted two days later. |
| 2024-01 | *"Mac Signing Fixes - Arm Support"*, then *"arm fixes"*. |
| 2024-01 | v8go 0.7.0 again. Reverted the next day. |
| 2026-04 | Fork to `github.com/robomotionio/v8go` at V8 14.7, Windows restored — five years on a 2021 V8 ends. |

**Why five years on one release.** v8go vendors prebuilt V8 as static archives
in the module, and the platforms it ships are not the same set from one release
to the next:

| release | archives |
| --- | --- |
| v0.6.0 | `darwin_x86_64`, `linux_x86_64`, **`windows_x86_64`** |
| v0.9.0 | `darwin_arm64`, `darwin_x86_64`, `linux_arm64`, `linux_x86_64` |
| robomotionio fork | all four, plus `windows_arm64` and `windows_x86_64` |

v0.6.0 is the last release with a Windows archive, and Windows is our largest
install base — so v0.6.0 is where we stayed. It is also the release with no
arm64 archive at all: the versions that would have brought Apple Silicon and
Raspberry Pi are exactly the versions that drop Windows. That is what both
one-day reverts were, in 2021 and again in 2024. Upstream's own README says only
*"There used to be Windows binary support"*; the fork's says it exists *"to
restore Windows support that was dropped"*, which it does with clang-cl.
| 2026-04 | *"retire pooled V8 isolates to prevent heap exhaustion"*. |
| 2026-05 | *"survive 25-50 MB inMsg in Function under Windows commit pressure"*. |
| 2026-05 | *"lazy msg bridge to fix Windows V8 OOM on large inMsg"*. |
| 2026-07 | *"Run the FunctionNode on goant instead of V8"*. |
| 2026-07 | *"Withdraw LazyMessage and the message bridge"* — the workaround deleted, three months after it shipped. |

Two of those entries are worth reading together. The isolate retirement was
forced by V8 accumulating per-isolate state that GC cannot reclaim — hidden
classes, inline caches, JIT code, type-feedback maps — so a pooled isolate walks
into the ~1.4 GB per-isolate cap and dies there. The Windows crash log:

```
Mark-Compact 1358.6 → 1358.6 MB … last resort; GC freed nothing
```

It reproduced in about nine minutes. The fix was a use-count and heap-size cap
that disposes an isolate rather than returning it to the pool — that is, paying
for a fresh isolate often enough to stay under a ceiling that GC could not.

And the bridge. Handing a 13 MB message to V8 copied it three times (Go string,
C string, V8 string) and peaked at roughly four times its size in transient
commit; on Windows that was fatal. The replacement was a JS `Proxy` over the
message backed by Go callbacks, reading through `gjson` and writing through
`sjson`. It removed the copies and then cost a loop over 10,000 records **19.7 s
against 7.9 ms**, because every property read re-scanned the whole document. Per
robot, it was a flag — which meant it did not offer a way out, only a choice of
which failure to have.

Both problems are gone rather than solved. There is no boundary to marshal
across, so the message is parsed in place and serialized straight back out; a
27.3 MB pass-through peaks at 272 MB where V8 peaked at 698 MB. And isolation is
a fresh global rather than a fresh isolate, which is the subject of §4 below.

---

## What V8 through cgo cost

The robot runtime evaluates a script per message, millions of messages, on
machines we do not control: Windows laptops under memory pressure, Raspberry
Pis, Apple Silicon Macs, Ubuntu servers.

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

The limit is off unless [`WithMemoryLimit`](embedding.md#deadlines) sets one, and it bounds
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

That is [`Scope`](embedding.md#scopes-and-pools), and it is the shape a message pump wants.

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

On the same Test262 checkout, through the same harness, with nothing excluded,
**goant passes 53,247 of 53,573 and goja passes 34,377** — a gap of nearly
nineteen thousand tests. It is not spread thin, either. It is `for await…of`
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
list. That is why the target is 100% of Test262 with nothing excluded, which is
what `-all` runs.

There is a second reason, and for a robot runtime it matters as much as the
first: **goja has no heap limit.** Its `Runtime` exposes `SetMaxCallStackSize`,
which bounds recursion depth, and nothing that bounds memory. A script that
retains more than the host can give it allocates until the Go runtime cannot
satisfy an allocation, and Go's out-of-memory is a runtime throw rather than a
panic — no `recover`, no deferred anything, and every other goroutine in the
process goes with it. That is the same failure that took V8 down for us, arrived
at by a different road. goant's answer is
`WithMemoryLimit`: a budget, and an error the
host can route to a Catch node.

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
2. **Then chase the specification.** Test262 with nothing excluded, because that
   is the only bar that answers "will this run what my users write". → 53,247 of
   53,573, against goja's 34,377.
3. **Then find the bugs conformance does not.** V8's own `mjsunit` corpus — a
   decade and a half of real bug reports — run as a crash hunt rather than for
   a score. → 2,653/3,149, and nothing in it crashes the engine any more.
4. **Then make it fast.** Octane 2.0, scored against goja the way
   [ahaoboy/js-engine-benchmark](https://github.com/ahaoboy/js-engine-benchmark)
   does, plus the message-pump path that Robomotion actually runs.

Steps 2 to 4 are where nearly all of the work went, and they are why goant is no
longer describable as "ant in Go".

---
