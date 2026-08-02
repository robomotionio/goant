# The JIT

goant runs a switch-dispatched bytecode interpreter. On Octane it scores between
23x and 430x behind the JIT engines. This document records what the profiles
actually say, why the plan in PLAN.md (port MIR, then port swarm.c onto it) is
not the first thing to build, and what replaces it.

## What the interpreter spends its time on

CPU profiles of five Octane benchmarks, 8 vCPU non-burstable, idle machine:

| cost centre | Richards / DeltaBlue | Crypto / NavierStokes |
| --- | --- | --- |
| dispatch (`runFrameBody` flat) | 29–31% | 30–34% |
| operand stack push/pop | 13–15% | 13% |
| handle to pointer (`objPtr`, pool) | 10–12% | 3–6% |
| tag checks (`isTagged`, `Type`) | ~7% | ~8% |
| inline-cache probe | 5–9% | — |
| number coercion helpers | small | 30–40% |

Half of Richards and DeltaBlue is dispatch, operand-stack traffic and tag
testing. A third of Crypto and NavierStokes is generic number coercion: `OpAdd`
has no inline double path, so `1.5 + 2.5` goes out of line through `toPrimitive`
twice, two string tests, two BigInt tests, `toNumberPrimitive` twice, and
`mknum`.

## The value representation is the thing that matters

`lucasdss/v8go` is a clean-room JavaScript engine in pure Go with a complete
two-tier JIT: a Sparkplug-style baseline and a TurboFan-style optimiser with an
SSA sea-of-nodes IR, escape analysis, GVN and nine passes. It is the only
comparable prior art, and its results are the most useful number in this
document:

| | interpreter | Sparkplug | TurboFan |
| --- | --- | --- | --- |
| loop, 100 iterations | 11.8 µs | 6.8 µs (1.7x) | 6.8 µs (1.7x) |
| object creation | 209 ns | 111 ns (2.0x) | — |

An entire optimising tier for no gain over the baseline. Their own analysis
explains it, and marks the first two causes "Irreducible: Yes — root cause: Go GC
architecture". The quotations in this section are from that project's README and
`docs/PERFORMANCE-DEEP-DIVE.md` at commit `c5bba68`
(github.com/lucasdss/v8go, BSD 3-Clause, © 2026 Lucas de Souza Santos). None of
its code is used here; what is borrowed is the conclusion, which is worth more:

- No tagged integers, because "Go's garbage collector scans every 8-byte aligned
  value for valid heap pointers. If we used tagged integers, Go's GC would
  interpret them as corrupted pointers and crash." Their value is a 48-byte
  struct, so `1 + 2` reads 48 bytes, computes, writes 48 bytes and allocates.
- No compressed references, because "Go's GC only traces full 64-bit pointers."

goant is not subject to either. `Value` is a NaN-boxed `uint64`; object
references are 32-bit handles into chunked non-moving pools; nothing the
interpreter manipulates is a Go pointer, so Go's collector never traces a value
and never needs to be told about one. What caps their JIT at 1.7x does not exist
here. It also means goant needs no shadow stack to keep values visible to the Go
collector, which they carry as `AMD64ShadowStack`.

The corollary is a constraint, not a licence. Their third cause is that
"most property operations delegate to Go helper functions", and they name the
fix they did not implement: "True inline property access would require emitting
the full guard chain in assembly." A JIT that removes dispatch and calls a Go
helper for everything else lands on their number regardless of value
representation. Generated code has to inline the guard chain, the double
arithmetic path and the tag tests, or there is no point emitting code at all.

## Why not MIR first

`ant/src/silver/swarm.c` emits 38 distinct `MIR_*` symbols, and the distribution
shows what MIR is being asked for:

```
285 MIR_MOV    103 MIR_JMP     59 MIR_BNE     53 MIR_BEQ
 44 MIR_URSH    20 MIR_OR      13 MIR_AND      ← NaN-box pack and unpack
  9 MIR_DADD     9 MIR_DSUB     6 MIR_DMUL     6 MIR_DDIV   ← all the JS arithmetic
```

About 25 real instructions. Porting mir.c, mir-gen.c and two backends — roughly
30k lines of dense C — to obtain an emitter for 25 instructions is a poor trade.
MIR does do work beyond emission: swarm creates around 1,445 virtual registers
and leans on `-O3` copy propagation, DCE and live-range allocation to clean up
deliberately naive IR. But that is SSA plus copy-prop plus DCE plus linear scan,
not the loop optimiser the other 25k lines pay for.

Two further problems. MIR models neither deoptimisation nor GC safepoints, so
the hard parts of a JavaScript JIT remain to be designed either way. And in C a
`jit_helper_*` call is an ordinary `CALL`, while in Go generated code cannot call
a Go function directly — `morestack` would run with an unknown return PC — so
each of swarm's 72 helpers becomes an exit and re-entry through a trampoline.
swarm's code fires helpers on every inline-cache miss. Whether that is affordable
is unknown, and it decides the whole design. Phase 2 measures it before anything
depends on the answer.

## Shape of the work

| phase | what | gate |
| --- | --- | --- |
| 0 | Inline double fast paths in the interpreter | conformance unchanged, Octane up |
| 1 | Per-site type feedback recorded by the interpreter | feedback matches a reference interpretation |
| 2 | `jitmem`: executable memory, entry trampoline, helper round trip | runs on all five targets; round-trip cost measured |
| 3 | Baseline JIT for amd64 and arm64 | zero interpreter/JIT differentials |
| 4 | Optimising tier, backend chosen on phase 2's evidence | — |

Phase 1 is a prerequisite for every possible phase 3 and pays off in the
interpreter first. Phase 2 is where the project either de-risks or dies, and it
is small.

### Phase 0, done

Four inline guards. Octane, median of three, the two binaries interleaved on an
idle machine so drift cannot favour either:

| | before | after | |
| --- | --- | --- | --- |
| Crypto | 133 | 236 | +77% |
| NavierStokes | 311 | 435 | +40% |
| Richards | 197 | 215 | +9% |
| RayTrace | 374 | 391 | +5% |
| RegExp | 143 | 148 | +3% |
| DeltaBlue | 233 | 240 | +3% |
| EarleyBoyer | 599 | 612 | +2% |
| Splay | 1919 | 1944 | +1% |
| **geomean** | | | **+15.3%** |

test262 core unchanged at 42739/42740.

## Platforms

Five targets — windows/amd64, linux/amd64, darwin/amd64, darwin/arm64,
linux/arm64 — but two backends. Generated code never uses the platform C ABI, so
the calling convention is goant's own and identical across operating systems for
a given architecture. What differs is confined to obtaining executable memory:

| | reserve and commit | make executable |
| --- | --- | --- |
| linux, darwin | `mmap` PROT_READ\|PROT_WRITE | `mprotect` PROT_READ\|PROT_EXEC |
| windows | `VirtualAlloc` PAGE_READWRITE | `VirtualProtect` PAGE_EXECUTE_READ |

Write-then-flip rather than a permanently writable-and-executable mapping, which
is what makes this work on Apple Silicon without `MAP_JIT` and keeps the pages
acceptable to hardened runtimes. None of it needs cgo: on Windows, `kernel32` is
reached through `syscall.NewLazyDLL`, which is how Go's own standard library
calls Win32.

wazero is the existence proof for the approach — a pure-Go compiler backend in
production on linux/amd64, linux/arm64, windows/amd64 and darwin/arm64 with zero
cgo. `lucasdss/v8go` is the cautionary one: it has a working baseline JIT that
builds on linux/arm64 and no other target.

The interpreter remains the fallback tier on every platform, so a target without
a backend is slow, never broken.

### Hazards that are not optional

- **Reserve the goroutine register.** R14 on amd64, R28 on arm64. Go's runtime
  and signal handling find the current goroutine through it; generated code that
  clobbers it turns the next signal into a crash.
- **Give compiled loops a safepoint.** Generated code has no PC the Go runtime
  can attribute to a function, so it cannot be preempted and a loop that never
  returned would hold up the collector for as long as it ran. Two ways to do it:
  read `g.stackguard0` (R14+16 on amd64) and look for Go's `0xfffffffffffffade`,
  which integrates with the real scheduler but depends on a layout no compatibility
  promise covers; or count iterations and return. Phase 3 counts, because being
  wrong about the offset would mean a hang rather than a slowdown.
- **Flush the instruction cache on arm64.** D-cache and I-cache are not coherent,
  so code written through a read-write mapping is not necessarily visible to the
  fetcher. Needs a short assembly routine per platform.
- **Do not grow the goroutine stack.** Generated code runs on its own stack, and
  the trampoline that enters it is `NOSPLIT`.
- **macOS notarisation** requires `com.apple.security.cs.allow-jit` and
  `com.apple.security.cs.allow-unsigned-executable-memory` on the shipped app.
  This applies to any JIT, V8 included.
- **Profilers go blind** through generated frames. Symbolisation is a later
  concern but the frame layout should not make it impossible.

## Phase 2 in detail

The spike that settles the design:

1. `CodeBuf` — allocate, write, make executable, free. Five targets.
2. An entry trampoline in Go assembly per architecture: switch to the JIT stack,
   enter generated code, return a value.
3. A hand-assembled function proving the path end to end.
4. A helper protocol: generated code leaves, a Go function runs, execution
   resumes. Measured, because the number decides phase 3 and 4.

The answer to look for: an ordinary Go call is a few nanoseconds. If the round
trip is comparable, generated code can lean on helpers and swarm.c's structure
ports. If it is an order of magnitude worse, the baseline JIT must inline far
more aggressively and porting swarm's helper-heavy output is the wrong shape.

### What it measured

8 vCPU non-burstable, idle, three runs each, all within 1%:

| | ns/op | |
| --- | --- | --- |
| direct Go call | 1.44 | what a `jit_helper_*` costs in C ant |
| enter generated code and return | 3.19 | the trampoline alone |
| full helper round trip | 7.60 | exit, dispatch in Go, re-enter |

Leaving generated code and coming back costs about six nanoseconds more than an
ordinary call. That is affordable — an inline-cache miss already costs several
times this in shape lookup and hashing, so delegating slow paths to helpers is
fine, and the exit-and-re-enter protocol is not the obstacle it might have been.

It is also a bound on what may be delegated. Interpreted `a + b` now costs on the
order of two nanoseconds, so an `OpAdd` compiled into a helper call would be
*slower* than not compiling it at all. This is the same conclusion
`lucasdss/v8go` reached from the other direction, with a number attached: the
arithmetic fast paths and the inline-cache hit path have to be emitted as
machine code, and helpers are for the cases that were already expensive.

Two things the measurement does not include, both expected to be small: generated
code here runs on the goroutine stack rather than a dedicated one, which will add
a register save and restore rather than a stack switch, and there is no
back-edge preemption poll yet, which is a load and a compare.

## Phase 3, begun

`jitCompile` compiles numeric functions — locals, numeric constants, `+ - * /`,
comparisons, branches and counted loops — and refuses everything else.

What makes it tractable is an invariant rather than a restriction on shape.
Parameters are checked once on entry, before anything has been written; every
value the compiled code then produces is the result of double arithmetic and so
is a Number by construction; and every local it reads is either one of those
checked parameters or one it assigned itself. Nothing in the body can fail a
type check, so no guard is needed after entry and the only way out is the one at
the top — before a single store. Bailing therefore means the interpreter runs the
function from the beginning with no state to reconstruct.

Two consequences worth stating. Compiled code contains no safepoint the Go
runtime can recognise, so a loop counts iterations and returns to Go every
20,000; at a back edge the operand stack is empty and every live value is already
in the locals array, so resuming needs nothing but an address. And a local
assigned only inside a branch is refused rather than read, because the
straight-line prefix cannot prove it initialised on every path — lifting that
needs a definite-assignment pass over the whole control-flow graph.

Measured on the same machine as everything above:

| | compiled | interpreted | |
| --- | --- | --- | --- |
| `(a+b)*(a-b)/b+a*b` | 8.0 ns | 128.1 ns | |
| the call alone (`return a`) | | 70.2 ns | not this tier's problem |
| the expression, less the call | ~4.8 ns | ~57.9 ns | **12x** |
| `while (i<n) { s=s+i*m; i=i+1 }`, n=1000 | 4.3 µs | 76.8 µs | **18x** |

The loop is the honest number: at a thousand iterations the call overhead has
gone to nothing, so 18x is the arithmetic and the dispatch, which is what a
baseline JIT is for. The remaining 70 ns of frame setup per call is a later
tier's work.

Two findings worth keeping.

A NaN produced by generated code has to be canonicalised. x86 hands back
0xFFF8000000000000 for `0/0`, which is numerically above the tag threshold, so
storing it raw would give the rest of the engine a number that reads as a tagged
object. `tov` folds it for the same reason; compiled code has to do the same, at
the cost of a compare and a never-taken branch.

The correctness gate is bit equality with the interpreter, not numeric equality.
The two differ exactly where it matters — the first NaN test written against
`math.NaN()` failed, because Go's NaN is 0x7FF8000000000001 and neither the
interpreter nor the compiler produces that one.

## How much of a real corpus this compiles

4.3% of it — 303 functions out of 6,976 across Octane. It was 0.4% before the two
analyses below.

`TestJITCoverage` measures it two ways, because the obvious way misleads. A
histogram of the *first* thing that stopped each function flatters whatever the
emitter checks earliest: a body blocked by five different opcodes is charged
entirely to one of them. The useful question is which functions are one feature
away, so the test also collects every unsupported opcode in a body and counts
only the bodies with exactly one:

| implementing this alone would compile | functions |
| --- | --- |
| `GET_FIELD` | 239 |
| `CLOSURE` | 27 |
| `GET_GLOBAL` | 10 |
| `THROW` | 10 |
| `SPECIAL_OBJ` | 9 |

Against 6,314 that need several and would move for none of them.

That last number is the important one. A baseline JIT is close to all-or-nothing:
most functions are blocked by a handful of features at once, so coverage does not
climb smoothly with each opcode added. It jumped from 0.4% to 4.3% only because
the two changes below lifted whole categories rather than single instructions.

**Definite assignment over the control-flow graph.** The straight-line prefix
rule refused 1,555 functions — more than any missing opcode — because a variable
declared inside a loop or a branch is the ordinary way to write JavaScript. The
usual forward analysis with intersection at the meet replaces it, and needs no
new code generation.

**Values that are not Numbers.** `undefined`, `null` and the Booleans can now sit
on the operand stack and in locals. They need no guard because they never reach
an arithmetic instruction: a compile-time kind on each stack slot, and a
flow-insensitive pass marking any local that ever receives a non-Number, is
enough to refuse the arithmetic instead. This is the change everything else
depends on — no further opcode could have been added while the tier could only
represent Numbers.

### One thing designing this found

Emitting the bitwise operators means emitting ToInt32, which is why its
definition was read closely — and the interpreter's was wrong. It truncated
straight to `int64`, a conversion Go leaves undefined outside that type's range
and which amd64 answers with `INT64_MIN`, so every operand at or above 2**63
reported as zero. `1e20 | 0` is 1661992960; goant said 0.

Fixing it correctly and fixing it cheaply turned out to be the same problem. The
first three attempts all cost about 6% of Crypto — a benchmark that is almost
entirely bitwise — measured against a build differing in nothing else. The cause
was the inliner: `toUint32` is called from every bitwise operator, and any
version large enough to hold the out-of-range case, or containing a call to it,
exceeded the budget and stopped being inlined.

What made it free was making the fast path *wider* rather than the function
smaller. Guarding on `math.Abs(d) < 2**63` covers every operand a program
actually has, and the reduction that follows turns NaN and both infinities into
NaN — so one comparison replaces `IsNaN` and `IsInf` and the whole thing stays
inlinable. Crypto measures 255 against 256, which is to say no cost at all.

test262 does not cover the range that was wrong, which is worth remembering about
a 99.998% score: the suite is a floor, not a proof.

## Running it

`GOANT_JIT=1` turns the tier on. It is off by default: at 4.3% coverage it cannot
pay for itself on a mixed workload, and an execution path that is not the default
is one to justify rather than assume.

A function is compiled after eight entries. The threshold is low because
compiling is cheap here — two dataflow passes and straight-line templates, no
register allocation to speak of — and a refusal is remembered, so the cost of
trying and failing is paid once. Compiled code is never released: it has to
outlive every entry into it, and nothing at this level can prove those have
ended.

On the workload the tier exists for, a script through the CLI rather than a
microbenchmark:

```js
function work(n, m) { var s = 0, i = 0; while (i < n) { s = s + i*m; i = i + 1; } return s; }
for (var k = 0; k < 3000; k++) r += work(1000, 1.5);
```

| | |
| --- | --- |
| goant, interpreted | 231 ms |
| goant, compiled | **9 ms** |
| node | 4 ms |

All three agree on the checksum. Roughly 25x over the interpreter, and about 2x
off node — on the friendliest possible shape, with no property access or calls in
the hot loop, which is exactly the shape this tier is for. It is still the number
that matters: it says the ceiling here is set by how much of the language the
compiler covers, not by the value representation. `lucasdss/v8go` reached 1.7x
with a whole optimising tier because 48-byte values capped it. Nothing caps this
one in the same way.

## Property access, and why it is not yet a win

`GET_FIELD` compiles. It needed a mechanism rather than an instruction: compiled
code keeps the operand stack in registers, and calling into the runtime loses
every one of them, so the live slots go to the ExecContext and come back on the
way in. A template compiler knows its depth at each point, so that is a fixed
sequence rather than a scan.

It also needed the collector to be told. A spilled slot holds a Value that
nothing else refers to — the registers it came from are gone, and the frame's
locals do not contain it — so a compiled frame suspended in a helper is a root,
and `SpillN` is how it says which slots are live. A stale slot from an earlier
call holds a handle to a cell that may since have been freed, which is why the
count is written before the exit rather than inferred afterwards. `Args[0]` is a
pointer and `Args[3]` an immediate, so tracing either would be worse than missing
one. The getter tests in `TestTieringAgreesWithTheInterpreter` exist for this:
a getter is JavaScript re-entering the engine underneath a compiled frame that is
holding values only its context refers to, and one of them allocates hard enough
to collect while that is true.

And it is slower:

```js
function dist(p) { var dx = p.x, dy = p.y; return dx*dx + dy*dy; }
```

| | |
| --- | --- |
| goant, interpreted | 1133 ms |
| goant, compiled | **1244 ms** |
| node | 10 ms |

A helper round trip is 7.6ns and the interpreter's inline-cache hit is less than
that, so every field read compiled this way loses more than the surrounding
arithmetic wins. That is the design rule from the top of this document arriving
with a number attached: **the fast paths have to be emitted, not delegated.**
`lucasdss/v8go` stopped at 1.7x for the same reason, and named the same fix.

Kept rather than reverted because the tier is off by default, the machinery it
needed is correct and tested, and the next step is now precisely specified: emit
the inline-cache hit — resolve the handle, compare the shape, load the slot —
and call the helper only on a miss. Until then, turning the JIT on makes
property-heavy code slower, which is the honest state of it.

## What the tier is worth on Octane: nothing yet

`GOANT_JIT_STATS=1` counts frame entries by where they ran. The static coverage
figure above — 4.9% of functions — turns out to be a poor guide to anything:

| | entries served by compiled code |
| --- | --- |
| Richards | 0 of 5,870,643 |
| DeltaBlue | 37,918 of 9,292,451 (0.4%) |
| Crypto | 84 of 3,102,716 |
| NavierStokes | 12 of 2,743 (0.4%) |
| RayTrace | 0 of 8,382,213 |
| EarleyBoyer | 0 of 29,596,449 |
| Splay | 0 of 3,389,073 |
| RegExp | 0 of 1,646,915 |

Essentially zero, which is what the unchanged Octane scores were already saying.

The hope was that the split would favour the tier — that a numeric compiler would
miss many functions but catch the ones that run in loops. It is the other way
round. The functions Octane spends its time in are the ones that allocate,
dispatch over a class hierarchy, close over variables and call each other, which
is precisely the set this tier refuses. The 4.9% it does compile are leaves and
helpers that barely execute.

So there is no shortcut here and no 80/20. Reaching a score that competes with
node on this suite needs calls, property access through an emitted inline cache,
closures, upvalues, globals, arrays and exceptions — most of the language, not a
numeric core with a long tail. The measurement is worth having early: it is the
difference between a plan and a hope.

## Still to do

In the order the table argues for:

1. **The inline cache for property access, emitted as machine code.** The lookup
   works and loses to the interpreter; this is what turns it into a gain. It
   needs the object header, pool and cache-entry offsets exported as constants
   from one source of truth, because a wrong offset here is memory corruption
   rather than a wrong answer. No deoptimisation is required: an unknown-typed
   result cannot reach an arithmetic instruction, because the compiler refuses
   it — which is the one piece of luck in the whole design.
2. **The remaining numeric operators** — `MOD`, the shifts and the bitwise
   operations. Tempting because `SHR` is the second most common *first* refusal,
   but the sole-blocker column is the one that matters and it puts them at two or
   three functions each. They also need ToInt32 emitted inline, which is a
   twenty-instruction sequence needing three scratch registers once the range
   above 2**63 is handled correctly — so they are neither as cheap nor as
   valuable as they first look.
3. **Globals, upvalues, closures and calls.** Calls are also the 70 ns of frame
   setup the phase 3 measurement could not touch.
4. **Tiering** — counters, a compile threshold, and the lifecycle rules that keep
   a code block alive exactly as long as something can enter it. At 4.3% this
   would still change nothing measurable; it is worth building when the coverage
   earns it.
5. **An arm64 emitter.** `jitmem` is already in place and tested for it; the
   emitter is mechanical once the amd64 shape has stopped moving.

Phase 1's type feedback is not on this list because this tier guards rather than
speculates. It moves back up when there is a second tier to feed.
