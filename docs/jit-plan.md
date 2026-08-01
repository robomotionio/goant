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
(`docs/PERFORMANCE-DEEP-DIVE.md`) explains it, and marks the first two causes
"Irreducible: Yes — root cause: Go GC architecture":

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

0.4% of it. 26 functions out of 6,976 across Octane.

`TestJITCoverage` reports that, and what stopped the rest:

| refused because | functions |
| --- | --- |
| a local it could not prove assigned | 1555 |
| `SHR` | 873 |
| `SPECIAL_OBJ` | 832 |
| `GET_FIELD` and `GET_FIELD2` | 979 |
| `GET_GLOBAL` | 786 |
| `GET_UPVAL` | 541 |
| `CLOSURE` | 235 |
| `RETURN_UNDEF` | 181 |
| `UNDEF` / `TRUE` / `NULL` / `FALSE` | 460 |

The number is small enough that wiring this tier into the interpreter would
change nothing measurable, which settles the order of the remaining work: the
tier has to cover far more before tiering is worth building.

It also corrects a prediction. Property access looked like the obvious next
lever — it is where Richards and DeltaBlue spend their time, and it is what
`lucasdss/v8go` named as the fix they never made. It is not the top of this list.
The straight-line prefix rule for definite assignment refuses more functions than
any opcode does, and it is the cheapest of these to lift, because nothing about
it needs new code generation.

## Still to do

In the order the table argues for:

1. **Definite assignment over the control-flow graph**, replacing the
   straight-line prefix rule. A local declared inside a loop or a branch stops
   being a refusal. No new instructions, and it is worth more than any of them.
2. **The rest of the numeric operators** — `SHR` and the other shifts and bitwise
   operations are ordinary integer instructions over the NaN box.
3. **Values that are not Numbers** — `undefined`, `null`, the Booleans, and the
   comparisons that produce them. Mostly a matter of letting tagged values live
   on the operand stack unexamined.
4. **Property access**, with the inline-cache guard chain emitted as machine
   code rather than delegated. This is the one that matters for Octane's score
   rather than its function count, and it is where the helper round trip
   measured in phase 2 starts to be load-bearing.
5. **Globals, upvalues and calls.** Calls are also the 70 ns of frame setup that
   the phase 3 measurement could not touch.
6. **Tiering** — counters, a compile threshold, and the lifecycle rules that keep
   a code block alive exactly as long as something can enter it. Worth building
   once the coverage justifies it, and not before.
7. **An arm64 emitter.** `jitmem` is already in place and tested for it; the
   emitter is mechanical once the amd64 shape has stopped moving.

Phase 1's type feedback is not on this list because this tier guards rather than
speculates. It moves back up when there is a second tier to feed.
