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

7.5% of it — 521 functions out of 6,976 across Octane. It was 0.4% before the two
analyses below, and 4.3% when this section was written.

That number is here because it is easy to produce, and the rest of this document
is the argument for not trusting it: see "What the tier is refusing, weighted by
how often it runs", which counts frame entries instead and has disagreed with
this table every time the two have been compared.

`TestJITCoverage` measures it two ways, because the obvious way misleads. A
histogram of the *first* thing that stopped each function flatters whatever the
emitter checks earliest: a body blocked by five different opcodes is charged
entirely to one of them. The useful question is which functions are one feature
away, so the test also collects every unsupported opcode in a body and counts
only the bodies with exactly one:

| implementing this alone would compile | functions |
| --- | --- |
| `CALL` | 108 |
| `CLOSURE` | 104 |
| `GET_ELEM` | 102 |
| `GET_UPVAL` | 98 |
| `PUT_GLOBAL` | 95 |

Against 4,934 that need several and would move for none of them, plus 835 blocked
by something that is not an opcode at all.

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

`GOANT_JIT=1` turns the tier on, and `GOANT_JIT=0` turns it off — which it did
not always do, see the correction below. It is off by default while its coverage
of a mixed workload is still narrow; on Octane it is now measured at +16.9% on
DeltaBlue and +8.6% on Richards.

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

## Property access

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

### The cache, emitted

So it is emitted, and the runtime keeps only the miss. The probe is `icWay.hit`
restricted to the case machine code can serve — one way, an own slot, in the
object rather than its overflow, holding something other than an unparsed JSON
span — and everything else falls through to exactly the helper above.

```js
function spin(o, n) { var i = 0; while (i < n) { o.a; o.b; o.c; o.d; i = i + 1; } return i; }
```

| 80,000,000 property reads | |
| --- | --- |
| interpreted | 2690 ms |
| compiled | **184 ms** |

**14.6x**, and 15.6x on `BenchmarkJITProperty`, which measures the same loop
without the process around it. The counters say 79,992,000 of 80,000,000 reads
were served by the emitted probe — the missing 8,000 are the 2,000 iterations
that ran before the loop tiered up, which is what makes the accounting worth
printing rather than merely believing.

The values are discarded because this tier still cannot compute with a field's
value, which is the next section's problem and not the cache's.

### The thing that made it unreachable

Writing the probe was not the hard part. Finding out that nothing could ever
reach it was.

The prologue used to check every parameter, and the check is "untagged, or hand
the frame back to the interpreter". A checked parameter therefore cannot be an
object — so for as long as every parameter was checked, no object could enter
compiled code at all, and a compiled property read could only ever be handed a
primitive. The cache would have been correct, tested, and dead.

The fix is to check the parameters the body actually computes with, which needs a
small analysis (`jitNumberDemand`): a local is in demand when a read of it
reaches an operation defined on Numbers, and demand propagates backwards through
stores, so `var t = o; t * 2` demands a Number of `o`. Everything else — being a
receiver, being stored, being returned — demands nothing, because the templates
for those work on any value. An unchecked parameter is not thereby trusted: the
numeric analysis is seeded from the same set, so a template that needs a Number
refuses the function rather than guarding it.

That change also surfaced a latent miscompilation. `GET_FIELD` carries private
names as well as properties, and they are not properties — `other.#x` in a class
method resolves against the class environment the frame carries. Compiled code
called `getField` with the mangled name, which finds nothing:

| `static read(o) { return o.#x; }`, summed over 200 instances | |
| --- | --- |
| interpreted | 19900 |
| compiled, before the guard | **NaN** |

Unreachable while parameters were checked, because the object argument bailed
before the read. The first thing letting objects in did was to make it reachable.

## Operators without a type

The cache made a field's value fast to read and left it useless. A field holds
anything, so `sum += o.a` had no template — every operator in this tier required
both operands to be known Numbers before it would emit anything, and a function
containing one went back to the interpreter, cache and all.

The fix is not deoptimisation, and specifically does not need it. Deoptimisation
is for a guard that fails after the frame has been changed; here the operands are
still in their registers, untouched, and the runtime already has an
implementation of every one of these operators. So the guard tests both operands
for the tag bits, takes the SSE path when neither has them, and otherwise calls
what the interpreter would have called:

```
cmp  x, r15      ; r15 is the NaN-box threshold; an untagged Value IS a double
ja   slow
cmp  y, r15
ja   slow
movq xmm0, x
movq xmm1, y
addsd xmm0, xmm1
...
```

Two compares and a branch that is not taken. `+`, `-`, `*`, `/`, the four
relational operators and all four equality operators have this form, and the
runtime side of it is literally the call the interpreter makes for the same
opcode — `jsAdd`, `jsArith`, `jsRelational`, `abstractEquals`, `strictEquals` —
so a compiled `+` and an interpreted one cannot disagree about a Date, a Symbol,
or a String that looks like a number.

The operands are not passed to the helper. Calling out already spills the operand
stack into the context and records its depth, which is what roots those values
for the collector, so the helper reads its two operands from the top of `Spill`
and is handed only the opcode and the depth. That mattered more than it looks: a
spilled operand used to be a Number, which refers to nothing, and now it can be a
String while a `valueOf` on the other side runs a collection.

Two things fall out of the same change. A comparison no longer has to be consumed
by a branch — the Boolean can be materialised, which is what `jitRelationalValue`
is for — and a String constant can be loaded, because baking the handle into an
immediate is sound for the same reason reading it from the pool is: the pool does
not move, and the collector marks `fn.constants` for as long as `fn` is
reachable, which is longer than the code `fn` owns.

Three refusal reasons disappeared — `non-numeric-operand`, `non-numeric-constant`
and `compare-not-branched` — and static coverage went from 417 functions to 460.

**What it costs when it is not needed: nothing.** The guard is emitted only where
the type is unknown, so every function that compiled before this emits the same
bytes. `TestKnownNumbersStillSkipTheGuard` measures that rather than asserting
it: one more `+` costs 43 bytes between two known Numbers and 177 generically,
and the first number is the bare SSE sequence with no room for a guard in it.

`GOANT_JIT_STATS=1` now reports both sides of that guard, for the same reason the
cache needed a hit counter: a guard that always sends its operands to the runtime
and one that never does produce identical answers, and nothing else can tell them
apart.

### What that made visible: the probe is aimed at the wrong shape

`dist(p)` compiles now, so the benchmark from the previous section can be run
again — and it comes out at **1361 ms** against the interpreter's 1137, which is
worse than the 1244 ms it managed when it delegated every read to a helper.

The counters say exactly where it goes. Every frame entry is compiled, all
5,999,979 untyped operators take the machine instruction — the generic path is
working perfectly — and of 3,999,986 property reads the emitted cache serves
**39,998, or 1.0%.** Four million helper round trips is 200 ms, which is the
regression to the byte.

The reason is not polymorphism. It is that compiled code probes *one* way, and
ways fill in order, so way 0 holds the first shape the site ever saw:

```
100 objects from the same {x, y} literal occupy three shapes: 1, 1, and 98.
```

The first two objects each get a shape of their own while the transition memo
warms up, and from the third on they share. So way 0 is filled by an object whose
shape never recurs, ways 1 and 2 hold the one that does, and the interpreter —
which probes all `ic.n` ways — hits while compiled code misses 98% of the time
by construction.

That is the same mistake as the prologue guard, one level down: the probe was
verified against a site with one receiver, and one receiver is the case where way
0 is the answer.

### Scanning the ways

The probe now walks all eight. Three instructions per way that does not match,
none at all for a site that matches at way 0, and no extra register: the object's
shape sits in the scratch while the way pointer walks forward, and the tail
re-reads what it needs. At most one way can hold a given shape — a fill reuses
the way already holding it — so the first match is the only candidate.

The one thing that had to change underneath it is that a site which gives up
caching now clears its ways rather than only setting its count to zero. Compiled
code has no register to spare for that count, so it decides a way is empty by
looking at it, and the two readers have to mean the same thing by empty.

| `dist` over 100 points, 2M calls | |
| --- | --- |
| interpreted | 1138–1167 ms |
| compiled, delegating every read | 1244 ms |
| compiled, cache emitted, one way probed | 1361 ms, **1.0%** of reads served |
| compiled, eight ways probed | 1163 ms, **100.0%** served |
| compiled, and no allocation per entry | **944–998 ms** |
| node | 8 ms |

The 198 ms is exactly the four million helper round trips that are no longer
made. What is left is a 2% loss against the interpreter on a benchmark whose
caller is interpreted.

### Entering a frame allocated

Every entry into compiled code built the context it shares with the runtime on
the heap: 160 bytes and one allocation per call, which `-benchmem` reports and
nothing else would have shown. `dist` pays it two million times for a callee that
does three arithmetic operations, which is most of what compiling it saved.

The root stack it lives on is LIFO and a context is dead the moment it is popped,
so the slice past its length is a free list that needs no bookkeeping of its own.
The one thing that needs care is clearing a reused context *before* it is
published rather than after, because the collector reads `Ret` and the spill
slots and the previous call's are stale.

Entering compiled code now allocates nothing, and
`TestJITFrameEntryDoesNotAllocate` is what keeps it that way. `dist` runs in 944
to 998 ms against the interpreter's 1138 to 1167 — the first time on this
benchmark that compiling a function has been worth doing at all, and it took the
cache, the operators and the frame together to get there.

## The store, and the four-property ceiling underneath it

`PUT_FIELD` was the largest single thing the tier was missing by static count:
597 functions in the Octane corpus had it as their only unsupported opcode, seven
times the next one. A function that reads a field almost always writes one, so
refusing the write refused the read as well.

The probe is the read's, piece for piece — tag check, handle resolve, a scan over
every way, epoch, the three pointers that must be nil, the slot bounds — and then
a store instead of a load. Building it out of the same parts is the point: two
probes that disagreed about the same cache would be two ways to be wrong about
it. This cut serves an own writable slot and declines the store that *creates*
the property, which is what `toShape` marks and what the runtime keeps.

Two things the read did not have to do. The store maintains the
invocation-dirty flag itself, because the compiled path skips `[[Set]]` and that
is where a write to state older than the run would otherwise be noticed — four
instructions and a not-taken branch when no invocation is running. And it needs
the runtime's address, so the context carries one alongside the pool, for the
same reason: two Runtimes have two of them.

Coverage went 460 → 497 functions and Octane did not move. The interesting part
is what came next.

### The probe served four properties

Emitting the global read (below) produced a **0%** hit rate, and the reason was
worth more than the feature. Both probes had a bound that reads like a corner
case and is not one:

```
slot < 4          // ANT_INOBJ_MAX_SLOTS
slot < shape.inobjLimit
```

Four is how many properties live in the object itself; everything past that is in
a slice. So a class instance with five fields had a fifth field no compiled read
could reach — and the global object, which carries every builtin before a
script's own names get near it, has *none* of its properties in the object at
all. The global read could never hit, and no test caught it because falling
through to the runtime is how every test agreed with the runtime.

`jitEmitSlotAddr` resolves a slot number to an address either way, for both
probes, in four instructions on the inline path and seven on the other. The bound
is the slice's length rather than its capacity, so a slot the shape declares but
the slice has not been grown to still goes to the runtime — growing one is its
job. `TestJITReadsAnOverflowSlot` and its store counterpart require a hit rather
than an answer, which is the difference that would have caught this.

## What the tier is refusing, weighted by how often it runs

The static histogram has now pointed at the wrong work twice. It said the numeric
operators were not worth building and they were; it said `PUT_FIELD` was the
largest blocker by a factor of seven, and clearing it moved 37 functions and no
score at all. Both times for the same reason: a program's time is not spread
evenly over its functions, so counting functions counts the wrong thing.

`GOANT_JIT_STATS=1` now charges every interpreted frame entry to the reason its
function was refused for, with a second column for what that reason *alone* would
unblock. The two are not the same question, and the difference is the whole
point:

| richards, 5.4M interpreted entries | entries | unblocks | functions |
| --- | --- | --- | --- |
| `stack-across-blocks` | 1.43M | 1.43M | 1 |
| `local-not-assigned` | 1.04M | 1.04M | 11 |
| `op:NEW` | 2.1k | 1.2k | 4 |
| `op:GET_FIELD2` | 2.51M | **0** | 14 |
| `op:GET_ELEM` | 0.45M | **0** | 2 |

| deltablue, 8.7M interpreted entries | entries | unblocks | functions |
| --- | --- | --- | --- |
| `stack-across-blocks` | 1.98M | 1.98M | 2 |
| `local-not-assigned` | 1.82M | 1.82M | 11 |
| `op:GET_ELEM` | 1.16M | 1.16M | 2 |
| `op:NEW` | 0.13M | 0.09M | 9 |
| `op:GET_FIELD2` | 3.56M | **0** | 35 |

`GET_FIELD2` is what `o.m()` compiles to and `CALL` is the instruction after it,
so a template for it alone moves the refusal one opcode along and unblocks
nothing — 3.56M entries and a zero. It was the second-largest blocker in the
static corpus at 1,071 functions. A reason with a large entry count and a small
unblock count is a queue, not a blocker, and the static histogram cannot tell the
two apart.

What is left at the top of the list is not missing opcodes. `stack-across-blocks`
is a block reached with operands still on the stack, which the two analyses model
as empty; `local-not-assigned` is a local the emitter cannot prove was written on
every path, which is a `var` read before its assignment or a lexical binding that
would have to throw. Both are limits in how the emitter models a frame, and
between them they are 45% of richards and 44% of deltablue.

The caveat the column carries: `unblocks` counts functions whose *one* missing
opcode is this one, so it is an upper bound rather than a prediction. `GET_GLOBAL`
was measured at 1.95M and 1.96M and delivered nothing, because the same functions
met `local-not-assigned` the moment the template existed. The way to read it is
"no more than this", and the way to check it is to build the thing and measure
again.

## The global read

Emitted, and it is the same cache once more: the probe over a receiver compiled
code fetches from the runtime rather than one it was handed. `rt.global` is
loaded on every probe rather than baked in, because `BeginInvocation` swaps a
fresh global in and `End` puts the old one back, and a compiled site outlives
several of them.

The guard that is not a shape: a Script-level `let` shadows a global property of
the same name and lives in a declarative record that no shape describes. It does
not need checking here — registering one bumps the cache epoch, which the probe
already tests.

## The receiver, and the first thing that moved

`this` was the last piece of a frame compiled code could not see. It was handed a
locals slice, and the prologue that binds `this` writes a slot from a value that
is not in it, so the emitter stepped over that prologue and refused every read of
the slot — which is to say it refused every method and every constructor.

The fix is one field in the context, beside the pool and the runtime, and `THIS`
becomes a template like any other. Unlike everything else in the context it holds
a Value, so the collector traces it: a getter is JavaScript and can run a
collection while the frame is suspended in a helper, with the receiver reachable
from nowhere the collector's walk descends into.

It is the first change in this tier to move the share of frame entries that land
in compiled code by more than a rounding error:

| | before | after |
| --- | --- | --- |
| DeltaBlue, compiled frame entries | 2.1% | **22.4%** |
| static coverage, Octane | 521 | **919** of 6,976 |

DeltaBlue's compiled code now executes 3.96M property reads at a 72% hit rate,
173k stores at 89%, and 258k global reads at 100% — against 283,096 reads and
nothing else two changes ago. Its score reads 265/266 with the tier on against
260/266 without, which is inside the drift described above rather than a result.

### The declines it uncovered

Richards did not follow, and the counter said why in one line: 195,342 *declines*
against 639 entries that ran. Compiled code was entered, the prologue's parameter
check turned the arguments away, and the frame went back to the interpreter — for
every call, for the life of the program. The bet that a caller passes Numbers is
worth making, and it was invisible when it lost, because the functions it loses
on are methods and methods did not compile at all.

After 32 declines the function is rebuilt with the demand set suppressed: the
prologue accepts every frame and the arithmetic emits its own guard, which is
what the generic operators are for. Richards goes to 5.0% of frame entries, 64
declines, 100% of its property reads and 98.4% of its stores served — and all
309,057 of its untyped operators take the runtime path, which is the trade.

### How it was found, which is the part worth keeping

Not from the histogram. `this` never appears in one, because it is not an opcode
the emitter lacks a template for — it was a rule the emitter relied on without
ever writing down. The prologue skip was documented as sound *because* an
unproven read refused the function, so relaxing `local-not-assigned` silently
removed a second rule that had been riding on the first. `this.x = 1` in a
compiled constructor started reading the undefined the frame was filled with, and
what caught it was Richards, not a test.

The weighted diagnostic then named it in one line — `this-local`, 1.10M entries
in richards and 1.99M in deltablue, in exactly the eleven functions per benchmark
that had been charged to definite assignment. Those functions were never blocked
by definite assignment at all.

## Calls, and the first thing that moved a score

Three opcodes, in the order the weighted diagnostic named them, and the third one
is where it turns.

**`CALL`** had refused every function containing it, which is why `GET_FIELD2`
was worth 2.83M frame entries and *zero* unblocks: `CALL_METHOD` was always the
instruction after it. The operands are already where the helper wants them — a
call site holds `[callee, arg0..argN-1]`, spilling roots all of it, and `SpillN`
says how much is live — so nothing is passed but the count. The arguments are
copied into a slice of their own, because a sloppy callee's mapped `arguments`
writes through to the array it is given and that array must not be the caller's
operand stack.

**`GET_FIELD2` and `CALL_METHOD`**, the pair `o.m()` compiles to. GET_FIELD2 is
GET_FIELD's probe with the receiver kept for the call that follows; the probe
writes over the register it is given, so the receiver is copied up first and the
copy is what gets probed — which also leaves the slow path holding the receiver
where the helper wants it, so GET_FIELD's helper is reused unchanged.

| frame entries in compiled code | before | after |
| --- | --- | --- |
| Richards | 5.0% | **32.7%** |
| DeltaBlue | 22.4% | **59.1%** |
| static coverage | 999 | **1485** of 6,976 |

And DeltaBlue got **9% slower**.

### The inherited slot

A class's methods live on its prototype, so `o.m` is an inherited read every
time — and the probe declined every one of them. Compiling the method call
therefore moved 55% of DeltaBlue's property reads *out* of the interpreter's
cache, which was serving them, and into a helper round trip. Richards, less
prototype-heavy, gained 3% from the same change; DeltaBlue lost 9%.

What the cache holds for an inherited property is the conclusion of a prototype
walk, so the guard is the receiver's `[[Prototype]]` still being the one the
entry was filled from — two objects can share a shape and not a prototype.
Everything else that could change the answer already bumps the epoch, because
every object the walk passed through is flagged `usedAsProto`.

| property reads served by the compiled cache | before | after |
| --- | --- | --- |
| Richards | 74.0% | **98.9%** |
| DeltaBlue | 44.6% | **82.0%** |

Same build, same machine, tier on in both arms, three interleaved pairs:

| | before these three commits | after |
| --- | --- | --- |
| DeltaBlue | 263, 261, 262 | **295, 295, 294** |
| Richards | 221, 219, 220 | **246, 247, 246** |

**+12.6% and +11.8%.** After four sessions in which every coverage gain produced
exactly nothing, this is the first change to move an Octane score past the noise
floor — and it is not the coverage that did it. The coverage came first and cost
9%; what turned it into a gain was serving the reads that coverage exposed.

## What the tier is worth on Octane

### First, a correction: every table below this line used to be wrong

`jitEnabled` was `os.Getenv("GOANT_JIT") != ""`, so `GOANT_JIT=0` — which is what
every control arm in every differential set — turned the tier **on**. For several
sessions this document reported "Octane is unchanged with the tier on" from two
identical arms. It was not measuring the tier against the interpreter; it was
measuring the tier against itself, and the differences it recorded were noise,
correctly reported as noise, from a comparison that could not have shown anything
else.

What survives that is every comparison of two *builds*, because those differ in
the binary rather than in the flag and both arms were on throughout. The
attribution runs are all of that kind, including the one that credits the call
work below.

`envOn` now reads the value. The lesson is the same one this document keeps
finding: **a control arm has to be checked, not assumed.** The check that would
have caught it is one line — run the control and confirm it compiled nothing —
and `GOANT_JIT_STATS=1` had been printing exactly that all along.

### What the tier is actually worth

`GOANT_JIT_STATS=1` counts frame entries by where they ran. Scores are
higher-is-better; the two arms are interleaved per benchmark so drift cannot
favour either. Two runs of each arm on the benchmark VM, with the control arm
verified to compile nothing:

| | off | on | off | on | | vs node |
| --- | --- | --- | --- | --- | --- | --- |
| **Crypto** | 235 | **1054** | 235 | **1054** | **+348.5%** | 163x → **36x** |
| **NavierStokes** | 436 | **1828** | 435 | **1830** | **+320.0%** | 76x → **18x** |
| **Richards** | 213 | **728** | 212 | **730** | **+243.1%** | 155x → **45x** |
| **DeltaBlue** | 241 | **544** | 241 | **552** | **+127.4%** | 374x → **165x** |
| **RayTrace** | 395 | **465** | 393 | **469** | **+18.5%** | 141x → 120x |
| RegExp | 144 | **157** | 146 | **156** | +7.9% | 55x → 51x |
| EarleyBoyer | 603 | **640** | 596 | **641** | +6.8% | 92x → 86x |
| Splay | 1983 | **2128** | 1988 | **2110** | +6.7% | |
| | | | | | **geomean +95.6%** | |

Both arms are steady to the point, so none of these are drift.

All eight positive, and compiled code holds **100.0%** of both Richards' and
DeltaBlue's frame entries. Crypto and NavierStokes were *negative* for most of
this document; they are array mathematics and what they were waiting for was
`a[i] = v`, which the entry-weighted table never ranked and the
instruction-weighted one put at the top the moment it existed.

The profile of compiled code has moved with it. It was 11% of DeltaBlue's CPU
when 72% of frame entries reached it; it is **25%** now, and what sits beside it
is the call path — `callValue`, `runCompiledFrame`, `jitRunAt`, `Enter` — at
about 22%, plus the helpers at 15% and allocation at 5%. That is the case for
compiled-to-compiled calls, and it is now a measured one rather than an
extrapolated one.

**Coverage measured by count kept not paying, and then it did.** Four changes in
a row moved the share of frame entries substantially and the score not at all —
carrying operands across a branch was the clearest, DeltaBlue 59.1% → 72.2% and
the geomean from +4.5% to +3.9%. What changed is not the tier but the
measurement: once refusals were weighted by *interpreted instructions* rather
than by entries, the next three changes it named were worth +5.1% → +14.2%
between them. The count was never the problem; counting the wrong thing was.

NavierStokes makes **2,826 frame entries in the whole benchmark** — its time is
inside a handful of enormous loops rather than spread over calls — so its share
of entries in compiled code says nothing, and −2.4% is close to what the tiering
check costs on a benchmark it cannot help. What it has left is element *writes*.
Crypto's remaining blocker is `stack-across-blocks` at 313,372 entries across
eleven functions, all of them fully unblocking.

Getting here took, in this order: `CALL`; `GET_FIELD2` and `CALL_METHOD`
together, because `o.m()` is that pair; inherited slots in the property probe,
without which the previous item was a 9% loss; the cached store reaching the
helper, without which EarleyBoyer was an 18% loss; and `GET_ELEM`, which took
DeltaBlue from +19% to +24% and Richards from +8% to +15%. Static coverage went
from 919 functions to 2,006 of 6,976 along the way, but the two changes that
mattered most — inherited slots and the cached store — added no coverage at all.

### The call path, and where the Go side stops giving

`GOANT_JIT_STATS=1` on DeltaBlue says what the tier is now made of:

```
jit: 20284071 compiled (100.0% of frame entries), 64 declined, 590 interpreted
jit: 59733670 property reads, 52397316 served by the compiled cache (87.7%)
jit:  3997361 property stores, 3230867 served by the compiled cache (80.8%)
jit:  6656556 global reads, 6655693 served by the compiled cache (100.0%)
jit: 11249847 untyped operators, 10304521 took the machine instruction (91.6%)
```

Twenty million calls against about nine million other helper exits, so **69% of
every round trip out of compiled code is a call**. That is what makes the call
path the only thing left worth attacking, and three changes to it paid:

| | | |
| --- | --- | --- |
| the callee's arguments read out of the caller's spill area, not copied | Richards +12.3%, DeltaBlue +8.0% | geomean +3.5% |
| `new` reaching a compiled constructor the way a call does | +0.5%, inside the drift | — |
| what a callee Value resolved to, remembered | DeltaBlue +4.5%, Richards +3.6% | |

The first is the one worth reading twice. A call site holds
`[callee, arg0 .. argN-1]` on the operand stack and spilling writes the whole
stack to the context before leaving generated code, so the arguments are already
contiguous, in order, and rooted by `SpillN` — and the helper copied them into a
slice of its own anyway, 20 million times. `makeslice` was 9.3% of the
benchmark.

Aliasing them is not sound in general, and the reason it is sound here is
structural rather than a check: a sloppy function's mapped `arguments` writes
*through* to the array it was given, a mapped `arguments` needs `SPECIAL_OBJ`,
that opcode has no template, so a function containing one never compiles.
`TestCompiledCalleeCannotSeeArguments` asserts the link rather than the
conclusion, and fails the day `SPECIAL_OBJ` gets a template.

The collector's half is the part that would have bitten. `markFrames` walks
every frame rather than the live prefix, so a returned frame still holding the
window is scanned after that context has been popped and handed to an unrelated
call — over whatever the next frame wrote, stale handles included. That is a
poison panic, not a wrong answer, and the fix is one store on the way out.

### Counting exits by helper, which named the largest win in the tier

The profile says where the time is; it does not say what compiled code is
*asking for*. Charging each exit to its helper does, and the answer was not the
one the call work had assumed:

| exits | richards | deltablue | earley-boyer |
| --- | --- | --- | --- |
| `CallMethod` | 17.9M | 20.9M | — |
| **`Equals`** | **14.7M** | 0.6M | **11.6M** |
| `PutField` | — | 0.8M | **19.9M** |
| `GetField` | 0.2M | 7.4M | 0.2M |
| `Call` / `New` | — | 0.4M | 4.0M / 2.4M |

Equality was leaving compiled code almost as often as calls were, and the reason
is that the generic operators guard on both operands being Numbers. Richards has
fourteen `== null` / `!= null` sites; EarleyBoyer has a hundred and thirty-eight
`=== null` / `!== false` / `=== true`. A comparison against a literal is what
this code is made of.

Against a literal the answer is exact and needs no guard at all:

  - `x === undefined|null|true|false` is bit equality with that singleton — each
    has a payload fixed by its constructor and a Number is untagged, so nothing
    can collide. One compare.
  - `x == undefined|null` is `x` being one of the two. TUndef is 7 and TNull is
    8, so a subtract and an unsigned compare; the subtract disposes of Numbers
    too, which wrap to a large value.

**Richards 840 → 991, +18.0%** — the largest single change in this tier since
the element write — with NavierStokes +3.4% and nothing negative.

Knowing the operand is a literal is done by reading the previous opcode rather
than by tracking a constant kind per stack slot. That is not laziness: a tracked
kind has to be maintained at every site that writes a slot, and a slot wrongly
believed constant is a miscompilation. The only extra fact the local answer needs
is that the comparison is not itself a branch target.

`x == true` is deliberately left to the helper. Abstract equality coerces the
Boolean through ToNumber and then runs ToPrimitive on an object operand, which is
user code and can throw.

**What EarleyBoyer says about scores.** Its untyped-operator exits fell from
11.76M to 3.02M — 8.7 million round trips removed — and its score did not move,
because only 50.9% of its frame entries are compiled and the interpreted half is
what sets the score. Removing work from the compiled half of a half-compiled
program is invisible.

### Seven attempts on the Go side of a call, and what they establish

The exit-and-re-enter protocol costs about 30 ns a call, split between
`jitHelper`'s dispatch, `jitCallCompiled`'s frame work, `jitRunAt`'s context
setup and the two trampoline transitions. Seven changes have now tried to shave
it, and three of them worked:

| | |
| --- | --- |
| the arguments read out of the spill area, not copied | **+3.5% geomean** |
| what a callee Value resolved to, remembered | **DeltaBlue +4.5%** |
| `new` reaching a compiled constructor | +0.5%, inside drift |
| remember the compiled code too | DeltaBlue +0.9%, Richards **−1.1%** |
| …retired only by a collection | EarleyBoyer **−4.3%** |
| each locals slot written exactly once | EarleyBoyer **−1.5%** |
| `runtime.KeepAlive` hoisted out of the loop | no change |
| **a dedicated `ExitCall`, so calls skip the helper switch** | **no change** |

The last is the one that settles it. Calls are 96% of what Richards asks the
runtime for, so a separate exit code that lets the re-entry loop serve them
without the prologue, the jump table and the generic operand decode should have
been worth several nanoseconds a call by the line profile. Across four
interleaved pairs on four benchmarks it moved nothing: Richards +0.3%, DeltaBlue
−0.3%, EarleyBoyer −0.2%, Splay +0.5%. Reverted.

**What seven attempts establish is that the cost is not in the layers.** It is
the round trip itself — leaving generated code, running any Go at all, and
coming back. Nothing that keeps that shape removes it, which is the whole
argument for the item at the top of the list below, and now a measured one
rather than an extrapolated one.

### Sixteen ways: one winner, six losers

`icWays` 8 -> 16 was tried before, measured at DeltaBlue +8% and EarleyBoyer
-5%, and left in the notes as "the fix that takes both is a scan bounded by
`propIC.n`". Re-taken now that the exit table exists, and both halves of that
note turn out to be wrong.

| | | | |
| --- | --- | --- | --- |
| DeltaBlue **+11.0%** | Richards +0.4% | NavierStokes -0.2% | Splay -0.5% |
| RegExp -1.9% | Crypto -2.5% | RayTrace -3.0% | EarleyBoyer -4.0% |

**geomean -0.2%.** One benchmark gains a great deal and six lose a little, which
is what a change that helps one access pattern and costs every program memory
looks like.

The bound-the-scan theory was checkable and false. `propIC.lookup` already scans
`ic.n` rather than the array, so the interpreter never walked empty ways; and
promoting a hit one place toward the front — so a hot shape reaches way 0 and
stays there — moved EarleyBoyer from -5.0% to only -4.0%. What is left is
footprint: an `icWay` is 40 bytes, so sixteen of them make a site 640 and a
function's cache array twice what it was. EarleyBoyer has far more sites than
DeltaBlue and gets nothing back for the extra cache pressure.

Reverted, including the promotion, which had no measurable benefit at eight ways
and is a write on the hottest read path in the engine.

### The measurement that inverted, again

Before the singleton templates, 59.5% of Richards' compiled frames left for a
helper at least once, which made a compiled-to-compiled call look worth less than
half of what it had seemed: a direct call only avoids the round trip if the
callee never exits anyway.

After them, Richards' exits are **96% `CallMethod`** and nothing else. The
argument now runs the other way and compounds: a frame whose only exit is a call
becomes helper-free the moment that call is direct, and so does its caller. This
is the third time in this document that a measurement has had to be re-taken
because the thing it measured had moved — and the second time the conclusion
reversed.

### Four things that did not work, and what they cost to find out

Everything after those three landed inside the noise floor or below it. They are
recorded because the next person to look at this path will think of them too:

| | |
| --- | --- |
| remember the compiled code alongside the function | DeltaBlue +0.9%, Richards **−1.1%** |
| …with only a collection retiring the entry | EarleyBoyer **−4.3%** |
| fill each locals slot once instead of once and a bit | EarleyBoyer **−1.5%** |
| hoist `runtime.KeepAlive` out of the re-entry loop | no change |

The second row is the interesting failure. `fn.jit.code` is the one part of a
callee's resolution that changes with time — a function compiles after eight
entries — so an entry recorded while the callee was cold keeps it interpreted for
the rest of the run. Retiring on every compile instead fixes that and costs
Richards more than it gives DeltaBlue. Reading the field is cheaper than keeping
a copy of it honest.

The third is the one that reads like an obvious win. `frameLocals` sets every
slot to undefined and the caller then overwrites the parameters, so an argument
is written twice; splitting the fill writes each slot exactly once and was
*slower*, because it replaced one inlined fill with two calls on a path whose
frames are small enough that the calls cost more than the writes.

The fourth was chosen off a profile line that cannot cost anything: `KeepAlive`
emits no instructions, so the 30ms attributed to it belonged to its neighbours.
Hoisting it only extends a slice's live range across the helper call.

**What that says about the path.** The Go side of a compiled call has been
squeezed to where per-call micro-optimisation no longer registers above a ~1%
run-to-run floor, and about 29% of DeltaBlue is still `jitCallCompiled`,
`jitRunAt`, `jitHelper` and the two trampoline transitions. That number does not
come down by making those functions cheaper. It comes down by not calling them:
a compiled caller entering a compiled callee directly, which is measured at 1.15
ns against 4.69 for the detour.

### EarleyBoyer: 18.75 million stores at a 0.8% hit rate

Before the last commit this table read −18.2% for EarleyBoyer and −3.4% for
Splay, and `GOANT_JIT_STATS=1` said why in a line:

```
jit: 18754393 property stores, 152621 served by the compiled cache (0.8%)
```

Eighteen million stores and the emitted probe served eight in a thousand;
Splay's figure was 0.0% of 349,364. `PUT_FIELD`'s first cut serves a store to a
slot that already exists and declines the one that *creates* the property —
which is what `toShape` marks, and what building an object is entirely made of.
Compiling those functions moved eighteen million stores out of the interpreter,
which handled each with a shape install and a slot write, and into a full
`OrdinarySet` behind an exit-and-re-enter.

It is the same shape as the inherited slot above, and the second time in one
session that compiling more code made a benchmark slower by taking work away
from a cache that was already serving it. **Coverage moves work out of the
interpreter; it does not make the work cheaper.** Whatever the new coverage
exposes has to be served before the coverage is a gain.

The fix here is the cheap half: the interpreter's cached-store path is now a
method both callers share, and the JIT's helper tries it before `setFieldR`. The
same for the read, because what compiled code emits is narrower than what the
cache can answer — a receiver that is not a plain object, a slot past the end of
the overflow slice, a site with no spare registers. EarleyBoyer and Splay go back
to level.

The expensive half is still open: emitting the transition in machine code rather
than reaching it through a helper. That means *installing a shape*, which would
be the first Go pointer this tier stores from generated code, and the first that
needs an argument about write barriers rather than the standing one that a Value
is an integer.

**How much of a difference this table can see.** DeltaBlue's score has read
249–250 in one session, 260–266 in the next and 255 in this one, all from builds
that should have measured the same; EarleyBoyer's read 623 and then 511. That is
4% to 18% of run-to-run drift, so only the two arms of a single interleaved run
are comparable to each other, and a 1–2% difference between them is at the noise
floor rather than above it. The +16.9% above is well clear of it, and its two
pairs agree to the point.

### Where the call work left it

Interleaved on the benchmark VM, tier on in both arms, median of three, against
the build the call work started from:

| | before | after | | vs node |
| --- | --- | --- | --- | --- |
| **Richards** | 716 | **990** | **+38.3%** | 45x -> **33x** |
| **DeltaBlue** | 548 | **626** | **+14.2%** | 165x -> **145x** |
| NavierStokes | 1827 | **1870** | +2.4% | 18x -> **17.6x** |
| Crypto | 1053 | **1077** | +2.3% | 36x -> 35x |
| EarleyBoyer | 639 | **650** | +1.7% | |
| RegExp | 156 | **158** | +1.3% | |
| RayTrace | 462 | **463** | +0.2% | |
| Splay | 2108 | 2108 | 0% | |
| | | | **geomean +6.9%** | |

NavierStokes is the one that does not move, and it cannot: 2,826 frame entries
in the whole benchmark, so nothing about the cost of a call reaches it.

test262 core 42739/42740 with `GOANT_JIT=1`, 23173/23173 under
`GOANT_GC_POISON=1`, `go vet` clean on all five targets.

**A gate that gated nothing.** The first core run of this session scored
`./goant`, which is what `-runner` defaults to and was still the previous build.
It reported 42739/42740 and meant nothing at all. The runner has to be rebuilt,
not merely present.

### Why: entering a frame costs more than a small method does

`BenchmarkCallIntoATinyFunction` and `BenchmarkCallIntoALoop` are the two numbers
that explain the table. Same call, same machine, the arms differing in nothing
but `jitEnabled`:

| | interpreted | compiled |
| --- | --- | --- |
| a body worth compiling (a 50-iteration loop) | 5,400 ns | 250 ns — **16x** |
| a body of one addition | ~100 ns | ~100 ns — **flat** |

Entering a frame costs about a hundred nanoseconds, and DeltaBlue is made of
methods smaller than that. A tier that makes bodies faster cannot help until the
entry stops dominating them, which is the whole of the 263/263.

### The fix that was not one

So: enter a compiled frame without building an interpreted one around it. A
compiled frame has no operand stack — its stack is nine registers and the spill
area — so `frameStack` builds and clears a buffer nobody reads; and
`runFrameBody` declares eight closures over its frame state, which capture, so
they escape, and every entry pays for them whether or not a bytecode runs. What
is actually needed is the receiver bound, the locals filled, and new.target
consumed. It took the tiny call from ~100 ns to **~48**.

On Octane it lost 2–4%, and the measurement that settles where is this one: with
`GOANT_JIT=0`, so the new path is never entered at all, the build carrying it
still scored 256–257 on DeltaBlue and 211–213 on Richards against 262 and 221
without it. **One branch at the top of `runFrameBody` costs the interpreter
several percent.** It was tried in `runFrame` first and measured −1.3% there. That
function is the hottest in the engine and its register allocation does not
survive being perturbed.

The gain applied to 22.4% of DeltaBlue's frame entries and was invisible in the
score; the cost applied to all of them and was not. `interp.go` went back byte
for byte, and what is left is the benchmark that says why the work is worth
doing — and the constraint on doing it: **whatever attacks the frame entry must
not perturb `runFrameBody`'s codegen**, which most likely means doing the work at
the call site rather than at the frame.

### The denominator was wrong three times

A profile of compiled code, taken once DeltaBlue was running 72% of its frame
entries there, said something the coverage numbers could not: that code is **11%
of the CPU time**. The rest is still the interpreter, running the functions the
tier refuses — and those have long bodies rather than many calls.

So the diagnostic got its third denominator. Functions were wrong because a
program's time is not spread evenly over them. Frame entries were wrong because
it is not spread evenly over calls either. **Interpreted bytecode instructions**
are what the interpreter actually does, and they name different work:

| richards | insns | entries | functions |
| --- | --- | --- | --- |
| `non-numeric-operand` | 66.6M | 3.4M | 8 |
| `op:JMP_FALSE` | 41.0M | **165** | 1 |
| `unreachable-target` | 12.7M | 0.6M | 2 |

The second row is the case entry-counting is blind to by construction: one
function, entered a hundred and sixty-five times, executing forty-one million
instructions.

The first row was the bitwise operators, which never got the guard-and-call the
arithmetic ones did. Giving it to them took Richards from 38.5% of its frame
entries in compiled code to 52.6% and its score from +14.5% to **+19.3%** — the
first change in this document chosen by work rather than by count, and the first
in four to move a score in proportion to the coverage it added.

### What that changes about the plan

The hope early on was that a numeric compiler would miss many functions but catch
the ones that run in loops. It was the other way round: the functions Octane
spends its time in allocate, dispatch over a class hierarchy and call each other,
and the tier refused all of them. The fix for that — methods, stores, globals,
fields past the fourth, calls, and the inherited slot a method lives in — is what
this session put in, and it is what the +16.9% is.

It also shows what "coverage" is worth on its own, which is nothing and sometimes
less. Compiling the method call took DeltaBlue from 22.4% of frame entries to
59.1% and made it **9% slower**, because the newly compiled sites paid a helper
round trip for reads the interpreter's cache had been serving. Coverage is only a
gain once the thing it exposes is served.

Still true, and still the next thing: a call between two compiled functions is
**1.15 ns against 4.69 ns** for the exit-and-re-enter detour, and ~100 ns of
interpreted frame entry disappears with it. Every compiled function is still an
island — entered from the interpreter, and every call it makes goes back out.

## Can this reach ant's speed in Go?

ant is the same engine in C with a complete MIR JIT, so it is the ceiling for
this architecture. The question is whether Go costs anything structural, and it
turns out to cost almost nothing — provided compiled code calls compiled code.

| | ns/op |
| --- | --- |
| a plain Go call, which is what `jit_helper_*` costs ant | 1.61 |
| entering generated code and returning | 2.89 |
| entering, then one compiled-to-compiled CALL | 4.04 |
| a full round trip through the runtime | 7.58 |

**A call between two compiled functions costs 1.15 ns** — less than a Go call and
level with a C one. Going through the runtime instead costs 4.69 ns. Both sides
are already machine code, so nothing forces the detour, and a JIT that takes it
anyway pays four times over on every call in the program.

That was the one place Go could have imposed a structural tax, and it does not.
What remains is a fuel check per loop iteration and one register given up to `g`
— measured at about 0.1% of `numeric.js`, where 3M iterations and 3,000 entries
cost roughly 11 µs of a 9 ms run.

Everything else is already the same as ant by construction: a NaN-boxed `uint64`
over non-moving pools, a collector of our own that Go never traces through, and —
if MIR-gen's allocator is ported — the same register allocation over the same IR
from the same bytecode.

Where the tier is complete it already performs like one. A numeric loop runs
2.25x behind node; ant's own published `fib(40)` is 2.85x behind. Different
workloads, so not a head-to-head, but it says the gap to ant is coverage — 24
opcodes against 135 — and not speed per opcode.

## The call, emitted

The prediction above was right, and it was the largest single thing in this
document: a compiled function now enters a compiled function with a `CALL`.

DeltaBlue's profile said what it would be worth before it was built. 29% of the
run was generated code and 50% was the round trip around it — `jitRunAt`'s
context setup, `jitHelper`'s dispatch, `jitCallCompiled`'s frame work, and the
two transitions through the trampoline, each an indirect branch to an address
that changes every time.

| | before | after | |
| --- | --- | --- | --- |
| richards | 841 | **1400** | +66% |
| deltablue | 578 | **876** | +51% |
| typescript | 4502 | 4712 | +5% |
| raytrace | 637 | 655 | +3% |
| earley-boyer | 939 | 956 | +2% |
| box2d | 1555 | 1591 | +2% |
| regexp | 297 | 301 | +1% |
| mandreel | 904 | 900 | — |
| code-load | 11845 | 11764 | −1% |
| crypto | 1734 | 1694 | −2% |
| gbemu | 3064 | 3002 | −2% |
| splay | 2652 | 2513 | −5% |
| pdfjs | 1809 | 1736 | −4% |
| navier-stokes | 3533 | 3265 | −8% |
| zlib | 893 | 820 | −8% |

The share of calls a call site makes itself: DeltaBlue 94%, Richards 91%,
EarleyBoyer 91%, RayTrace 48% — and RayTrace's other half is calls into natives,
which have no compiled form to enter and never will.

The right-hand end of that table is the cost, and it is worth being exact about
where it comes from. NavierStokes lost 8%, and with the sites present but never
armed — `GOANT_JIT_CALLMASK=0`, so every instruction is emitted and no call is
made in machine code — it still loses 5.5%. So most of it is not the compiled
call at all. Two thirds of that is the code a call site now carries whether it
arms or not, about fifty instructions of setup, call and unwind sitting inline
between the operands and the runtime path; and the rest is the driver, which now
asks which frame stopped before it can serve it. NavierStokes leaves compiled
code for nearly every array store, so it pays that per exit and gains nothing
back: its calls are `Math.sqrt`.

The answer was layout: the guard stays at the site and the body of the call is
emitted with the entry stubs, so a site that never arms costs six instructions
rather than fifty of dead weight in the instruction stream, and one that does
pays an unconditional jump each way. NavierStokes recovered half of what it had
lost, from 3249 to 3373, and nothing that had gained gave any of it back. What
made it fiddly rather than obvious is the fixup table: the emitter's loop assigns
each resume address its catch handler as the instruction that produced it is
emitted, and a deferred block appends its fixup long after that loop has moved
on — so the site now reads the handler in force and hands it to the block.

### What had to be built, and what it is not allowed to do

The constraint that shaped all of it: **Go must never run with a compiled frame
on the goroutine stack.** Generated code is entered from a NOSPLIT trampoline, so
a stack growth while one is live would send morestack walking frames the runtime
has no map for. That rules out the obvious design — a nested frame returning to
Go for a helper the way an outermost one does — and forces the one that works: a
compiled frame that needs the runtime returns to *its caller*, which saves its
own operands and its resume address and returns in turn, all the way out. The
chain unwinds, Go runs on a stack it understands, and the frames reconstitute as
new calls are made.

Three other things had to exist first.

**The contexts became a chain.** They were allocated per entry and stacked in a
slice; now there is one per depth, linked, built ahead of the deepest frame that
has run. A call site finds the callee's frame with one load, and `markRoots`
walks the live prefix exactly as it walked the slice. Building ahead is not an
optimisation: a call site finds the next context through the caller's, so a chain
that only grew where the runtime had already entered a frame would let the first
compiled call at a depth happen and never the second.

**The callee's locals live in its context.** That is the frame it publishes for
the collector — there is no vmFrame, no entry in the locals slab, no depth of its
own in the runtime's frame counter, because publishing any of those means writing
a Go pointer from machine code. It is only sound for a function that cannot let
its locals outlive it, which is what the predicate refuses: a closure over a
local, an `arguments` object, a direct eval, a `with`, a tail call, more locals
than a context holds, or an arity the site does not match exactly.

**The frame's identity is a record, not the site.** See below.

### Two things that cost a day between them

**The chain walk was 38% of EarleyBoyer.** A frame published its identity as an
index into the code that called it, so nothing but integers travelled through
machine code — and recovering it meant walking down from the frame the runtime
entered, sixteen levels of pointer-chasing through contexts nowhere near the
cache, on the path taken every time a compiled frame reaches the runtime for
anything at all. EarleyBoyer went from 937 to 715 and the profile named the
function outright. It is one load now: the context holds the address, and the
note on `ExecContext.Site` is the argument for why the one pointer generated code
writes needs no write barrier.

**A refill reassigned the identity of a live frame.** TypeScript compiled to a
helper indexing an empty constant pool, at a call depth of three and only after
two million calls had gone right. A frame publishes what it is running because a
helper serving it has to be handed that function's constant pool and inline
caches — but a site is a *cache*: refilled after a collection retires it, and
whenever it is reached with a callee it does not hold. And a site is reachable
from inside the frames it opens. `f(g)` where `g` calls `f` again is enough.

So a fill allocates a record and never writes to it again; the site points at the
current one and a frame at the one it was entered through. The fill also had to
become atomic — a site holding one function's entry address beside another's
guard sends a call into the wrong function altogether — so everything that can
decline now happens before the first store.

Neither bug was found by a test. The first was a profile; the second was Octane's
largest workload failing while `test262 -core` stayed at 42739/42740 with the
tier forced on at threshold 1. What found *it* was the same knob that has found
every miscompilation in this tier: halve the callees a site may enter,
`GOANT_JIT_CALLMASK`, and see which half fails.

### A third, found later: a rebuilt callee looked like a different one

A site tolerates a bounded number of different callees before it gives the
machine path up, which is right for a site that really is polymorphic. The record
it holds names the compiled *block*, though, so a callee that recompiled itself
looked exactly like a callee that had been replaced — and eight rebuilds retired
the site for good.

The tier already rebuilds a function whenever a bet compiled into it has been
lost. Today that is only the parameter check, which is rare; anything driven by
feedback would make it common, which is how this was found. Rebuilding functions
to widen their property probes took DeltaBlue from 92.2% of calls made in machine
code to 82.5% and cost 12% of the score — while the probes themselves served
87.6% of reads against 87.4%, so the thing being measured was not the thing doing
the damage. A rebuild is not a change of callee and no longer counts as one.

It is worth naming as a class: the compiled call binds to a block, and every
future optimisation that replaces a block has to know that.

### What it gives up

A frame entered this way is not in `rt.frames`, so it does not appear in
`Error.stack`. The trace walks the runtime's own frame array, and putting a
compiled frame there means writing a `*svFunc` and a slice header from machine
code. A trace taken under a compiled call chain is short by however many of these
frames lie between the throw and the last frame the runtime entered. Nothing in
the language depends on it and nothing in test262 tests it, but it is a real
difference and not a subtle one.

## The second backend

The tier emits machine code on arm64 as well, from one copy of the templates.

Almost all of the work was naming. The emitter reached for a dozen register
constants directly, so those became roles — `jitStackRegs`, `jitRegLocals`,
`jitRegGuard`, `jitRegScratch`, `jitRegReturn`, `jitRegExit`, `jitRegF0`,
`jitRegF1` — defined per architecture. After that the whole engine compiled for
arm64 with one error, which says more about how narrow a template compiler's
instruction vocabulary is than about the port.

Four things a shared template genuinely cannot say in one way:

- **A patched constant.** A resume address is written into code already emitted,
  and on amd64 that is eight bytes inside a `movabs` while on arm64 it is four
  instructions carrying sixteen bits each. The encoder patches it now, rather
  than the emitter reaching into the buffer.
- **The return address.** `BLR` puts it in a register and `RET` jumps to it, so a
  nested call destroys its caller's way back into Go. `SaveLink`/`RestoreLink`
  bracket the compiled call and are nothing at all on amd64.
- **Flags as a side effect.** amd64's `AND` and `OR` set them and arm64's do
  not, and two templates branched on flags they never asked for. Both property
  probes test a shape transition and a proxy pointer for nil *as one value*,
  which is where the first arm64 miscompilation was — Richards read
  `this.scheduler` as undefined, three property reads away from the cause.
- **An unconvertible double.** `CVTTSD2SI` reports failure by returning
  `INT64_MIN` for everything, including a NaN. `FCVTZS` saturates and does not
  report: too large gives `INT64_MAX`, too small gives `INT64_MIN`, and a NaN
  gives zero — which is already what `ToInt32` says a NaN is, so the arm64 guard
  is shorter than the amd64 one and needs no slow path for it.

### And the one it cost to say them

Separating the two condition families added `SetfccReg`, and it went through the
encoder's ordinary register-operand helper — which omits a REX prefix it does not
need. `SETcc` needs one it does not need: without it, the encodings 4 to 7 name
AH, CH, DH and BH rather than SPL, BPL, SIL and DIL. Two operand-stack slots are
RSI and RDI, so a comparison materialised into either wrote its answer into the
high byte of a different operand and left its own slot untouched.

`SetccReg` has emitted the prefix unconditionally since it was written and says
why in a comment directly above. The new function was a copy of the shape of the
code around it rather than of the one instruction it replaced.

Nothing crashes, and nothing shallow is wrong — the slots are the fourth and
fifth, and a small function never reaches them. test262 core was 42739/42740
with the tier forced on at threshold 1, and mjsunit's 3,149 tests agreed with
the interpreter exactly, both with this in. What found it was Octane's TypeScript
benchmark reporting "Parse errors", which is a compiler comparing token positions
at depth. The differential that pins it now walks every operand depth from 0 to
8 for all eight comparison operators, and fails at 3 and 4 and nowhere else.

### The emulator, and what it could not tell us

`GOARCH=arm64 go test -exec qemu-aarch64` runs everything without hardware, and
it earned its place twice: it caught a push whose pre-index displacement was four
bits short and would have assembled cleanly, and `!(a - b)` answering false for a
NaN — the amd64 template rests on UCOMISD setting the zero flag for unordered as
well as equal, and FCMP does not.

It is also 113 times slower. The engine's test suite is 5.7 seconds on an Apple
Silicon Mac and 650 under qemu; test262 core is three minutes against an hour and
a half. And three of the things that mattered it could not have told us at all:

- **`MRS CTR_EL0` is illegal on macOS.** The instruction-cache flush read the
  real line size, which is the careful thing to do and which Apple does not
  permit from userspace. It was the first instruction of the function and the
  process died on the first compiled function. The stride is the architecture's
  minimum now — sixteen bytes, which repeats a maintenance operation rather than
  skipping a line, and is always right.
- **`day * msPerDay + t` fuses on arm64.** Go permits contracting a multiply and
  an add where the architecture has the instruction, and MakeDate is specified
  with the product rounded first. One test262 test separates the two
  architectures on it, and an explicit conversion is how the language says round
  here.
- **A branch further than its instruction can reach.** arm64's conditional branch
  covers a megabyte and amd64's rel32 covers two gigabytes, so a function large
  enough to overflow the first exists and the second has never seen one. It is a
  refusal now rather than a panic.

darwin/arm64 matches amd64 exactly: test262 core 42739/42740, the same with the
tier forced on at threshold 1, and `language` under `GOANT_GC_POISON` at
23173/23173. On that machine the tier is worth 5.4x on Richards and 3.5x on
DeltaBlue over the interpreter; on a Raspberry Pi, 3.6x on Richards.

## What compiles it, and what it costs to compile

The tier had never been asked what it costs to *compile*, only what it saves. A
conformance suite does not time anything and Octane's functions are small, so
nothing in the loop measured this. V8's mjsunit does, by accident: it is fifteen
years of bug reports, and several of them are about pathological source.

Run twice over 3,149 runnable tests — once interpreted, once with the tier forced
on at threshold 1 — the two arms differed by three tests, all of them timeouts,
none of them a wrong answer. Three timeouts is the entire quality report on the
compiled call and the second backend from a corpus neither had seen. The bugs
behind them were not in what the tier emits. They were in what it costs to decide
what to emit.

**Two fixpoints, both iterating a Go map.** Definite assignment and the
operand-depth agreement are both forward dataflow problems, and both were solved
by looping over `map[int]*jitBlock` until nothing changed. A Go map hands its
keys back in an order deliberately unrelated to anything, so propagating a fact
along a chain of n blocks takes n passes over all n blocks. That is quadratic,
and the comment above one of them said the graph was small.

It is small in Octane. mjsunit has a function that switches on eighty thousand
cases: 267ms to interpret, over two hundred seconds to compile, which is not a
slow tier but a hung process. The depth analysis is a worklist now, which visits
each block once — a block's depth is decided the first time it is reached, since
a second predecessor either agrees or refuses the function. Definite assignment
keeps its fixpoint and visits in reverse post-order, which carries a fact the
length of a straight run in one pass and settles in two. 16,000 cases went from
9.0 seconds to 65 milliseconds; 80,000 from unbounded to 328.

**An allocation per (block, local, predecessor).** The entry-set intersection
built its `out` set inside the innermost loop, so a file with a few thousand
blocks spent 69% of its time allocating the same answer repeatedly. One buffer,
refilled. A 2,032-line file of fuzzer-generated evals went from 200 seconds to
0.04.

**And a budget, because the rest is inherent.** Both analyses keep dense sets of
`maxLocals` bits per block, so their cost is the product — and a function can
grow both at once. Eight thousand try/catch blocks declare eight thousand catch
bindings; the product is quadratic in the source and no ordering fixes that. The
analysis now declines above 2²¹ block-local pairs, checked after the block count
is known and before the first set is allocated. Octane's largest real function
comes to 8,978 pairs, so the budget is two hundred times anything real code has
asked for, and what sits above it is generated, cold, and cheaper to interpret.

With those in, the tier and the interpreter agree on all 3,149 tests: the diff
between the two arms is empty on amd64 and on arm64. Two tests fail interpreted
and pass compiled, both about `arguments` — those are interpreter bugs, and they
are on the list below rather than in this section.

## Handing a frame back partway

Everything above this line is a tier that can only be right. It has two ways to
end a frame — produce the answer, or decline the arguments before running a
single instruction — and both are total, so every assumption it makes has to be
one an entry guard can check. That is the constraint behind the shape of the
analyses: `jitNumericLocals` is a proof over the whole body rather than a guess
about the common case, because a local that is a Number nine hundred times and a
String once has nowhere to put the discovery. The String has to be compiled for.

A bail is the third ending. The frame stops where it is, says which bytecode
instruction it stopped before, and the interpreter carries it the rest of the
way. What it buys is the right to be wrong, which is the whole of what separates
a template tier from an optimising one: code can be emitted for what the program
has actually been doing, behind a check that costs a compare, and the case it was
not written for stops being a reason to refuse the function.

**It turned out to cost one number.** The reason is the value representation, and
this is the payment for the decision two thousand lines above: a Value is a
NaN-boxed integer whether the interpreter wrote it or compiled code did, and both
address the frame's locals as one flat array. So there is no state to translate
on the way out — no register map, no unboxing, no per-site descriptor. The whole
description of a bail point is its bytecode offset, and the work at run time is
spilling the two or three operands that were in registers, which is the first
half of the call-out sequence that was already there.

The Go side is four lines longer than that suggests, and the four lines are the
interesting part. A frame is not only its locals and its operand stack: it also
has a `with` chain, a variable object, a new.target and a set of open upvalues,
and a compiled frame keeps all of those in the *published* frame — which is where
the collector looks — while the interpreter keeps them in Go locals it computed
at entry. A resumed frame has to take the first and discard the second.

Getting that backwards is what the sweep below caught, and it is worth writing
down because it looked right. The resume read those fields out of the published
frame — after calling `syncFrame`, which writes them *into* the published frame
out of the very Go locals being replaced. So the resume faithfully restored the
values entry had computed: the `with` chain the body had since moved on from, and
a new.target the compiled entry had already consumed. Five test262 tests failed;
none of the twelve unit programs did, because none of them had a chain that
moves.

### Testing the thing that is never supposed to happen

A bail cannot be tested where it will be used. Every real one sits behind a guard
written not to fail, so the paths that matter are the ones no corpus reaches —
and it fails quietly, because a handover that loses an operand or resumes one
instruction late computes a plausible number rather than crashing.

So the guard is taken out of the question. `GOANT_JIT_BAILAT=<offset>` plants an
*unconditional* bail before the instruction at that offset in every function
compiled, which turns "does the handover work" into something that can be swept:

- **Per instruction.** Twelve programs, each run once per offset in its body,
  every answer required to be the interpreter's bit for bit. 190 offsets.
- **Per corpus.** The whole `-core` profile, once per offset from 0 to 15, with
  the tier forced on at threshold 1 — so every function in test262 that compiles
  at all hands itself back mid-body. All sixteen produce the *same failing set*
  as the tier arm: one test, `Proxy/revocable/tco-fn-realm`, which is the
  per-function `[[Realm]]` gap and fails without any of this. Small offsets
  because that is where the coverage is: every compiled body has an instruction
  at byte 3, and progressively fewer have one at byte 40.

The corpus arm has to be run **one suite at a time and compared by failing-path
set**, and that is not fastidiousness. Four concurrent `-core` runs on eight
vCPUs produced 34 failures at offset 0; the same offset alone produces one. Every
extra was a timeout on the heavy generated files. A total measured under load is
not a result, and the same trap had already cost a run on the Mac the same day.

The unit sweep found nothing and the corpus sweep found the ordering bug, which
is the right way round for a mechanism whose failures are all about state the
small cases do not have. Both are checks that can fail: dropping the operand
stack, resuming at `ip+1`, and re-fetching the locals instead of carrying them
each make the unit sweep fail, and the last of those is why the locals travel in
the resume rather than being looked up again.

### What it does not cover yet

**A bail inside a `try` is refused at compile time.** The interpreter's handler
stack is a Go local of `runFrameBody`, and a frame resumed without it would run
its catch clauses in the wrong place — silently, because a body that does not
throw behaves identically either way. The handlers in force at any point are
compile-time knowledge and could be handed over as a small table; that is real
work, and it belongs with the first speculation that wants to guard inside a try.

**A frame that has bailed does not re-enter compiled code.** The loop below would
enter at the same header, meet the same failing guard and hand back again, a Go
frame deeper each time. Refusing is also what a real tier does — the standing
answer is to recompile without the assumption, which is a thing to build once
there is an assumption to drop.

Nothing emits a bail yet outside the test knob. This is the mechanism, built and
proved — and it is what the "type feedback" line at the bottom of this document
has been waiting on since phase 1 was written down.

### The frame a call site opened

A frame the runtime entered has an interpreter underneath it already: runFrameBody
is on the Go stack below, holding the locals slab, the operand buffer and the
published `vmFrame`. A frame a compiled call site opened has none of that. It
lives in a context and nothing else, and that is not an oversight — building no
frame is most of what makes the compiled call worth what it is worth.

The first answer was to withhold the machine entry from any body that could bail,
which is correct and useless: it means speculating in a function costs that
function its compiled call, up to 66% on the workloads that call most. Trading a
measured 66% for a guard that is meant never to fail is a bad trade in every
case, so the mechanism was not finished until this was closed.

`jitBailSiteFrame` builds the frame instead — publish, copy the context's locals
into a slab, resume — and hands the answer back the way an ordinary return does:
into the caller's `Ret`, resuming at the address it saved on the way out. The
caller is machine code suspended mid-call and cannot tell the difference. What
makes the copy sound is the same predicate that allows the compiled call at all:
`jitMachineCallable` refuses a body whose locals can outlive it, so nothing
aliases them, and refuses `arguments`, so nil is an honest argument list.

### What its existence cost, twice, before it emitted anything

Both of these were found by benchmarking a feature that compiles to nothing —
`jitBailAt` is −1 in production, so no bail is emitted and no entry withheld —
and both were about what the *possibility* cost the common path. Together they
were 1.1% of the Octane geomean.

**A rare exit must not be given a case on the dispatch every common exit takes.**
`case ExitBail:` in the run loop's switch widened its range from 0–4 to 0–6,
which is enough to change how Go compiles it. The workloads that lost most were
the ones that leave compiled code most often — NavierStokes 3.2%, zlib 2.7%,
mandreel 2.3%, gbemu 2.1% — which is what pointed at the dispatch rather than at
anything a bail does. Testing it inside `default:`, with the body out of line,
recovered gbemu, zlib and mandreel outright.

**A field added ahead of `Locals` moves every frame's variables.** `BailIP` sat
between `Deep` and `Locals` and pushed the locals array from offset 416 to 424,
so a function with four of them began straddling the 448 cache-line boundary it
used to sit inside. That was the residual: NavierStokes 2.3%, box2d 1.2%.
Appending it past `Locals` leaves every offset generated code compiles against
exactly where it was, and NavierStokes came back to +0.3%. The layout test now
asserts `Locals` ends where `BailIP` begins, because "Locals is last" was only
ever a proxy for the thing that matters.

The whole mechanism is now −0.03% on the geomean, which is nothing.

## Still to do

Rewritten 4 August, after the compiled call and the second backend.

**This list is ordered by measurement rather than by corpus count.** The static
histogram is still worth reading — 4,971 of 6,976 functions now compile, 71.3% —
but it has pointed at the wrong work every time it has been consulted, and the
frame gate behind it is nearly empty: only 4 of those 6,976 are refused by
`jitEligible` rather than by a missing template. Octane is 2012-era JavaScript
and contains no generators, no async functions and no class constructors, which
is the corpus flattering the tier rather than the tier being complete.

No benchmark is now made materially worse by the tier, so the list is ordered by
what would gain rather than by what is bleeding.

**This list was reordered on 5 August by counting exits rather than opcodes**, and
the count moved the top of it. `JITHelperStats` reports why compiled code left,
by helper, and it says the exits are memory accesses — not arithmetic.

`arith` is 0.5% of DeltaBlue's exits, 0.2% of crypto's and 0.0% of mandreel's.
So type feedback for arithmetic, which is what an optimising tier is usually
built for and what phase 1 wrote down, would move very nearly nothing here: the
arithmetic is already emitted. What is not emitted is `a[i]`.

| workload | exits | first | second |
|---|---|---|---|
| zlib | 2,889M | getelem 73.7% | putelem 24.4% |
| mandreel | 312M | getelem 61.8% | putelem 37.0% |
| typescript | 136M | getfield 53.7% | callmethod 19.1% |
| earley-boyer | 89M | putfield 22.9% | instanceof 21.1% |
| crypto | 89M | putelem 81.5% | getfield 6.9% |
| navier-stokes | 50M | putelem 86.0% | topropkey 9.8% |
| gbemu | 40M | getelem 37.6% | putelem 33.6% |
| raytrace | 28M | putfield 31.5% | getfield 27.3% |
| box2d | 21M | getfield 80.2% | putfield 6.2% |
| deltablue | 16M | getfield 66.3% | callmethod 15.2% |
| richards | 3.8M | callmethod 71.2% | putelem 17.7% |

0. **`a[i]` on a typed array, which the emitted guard chain rejects at its first
   instruction.** The chain serves 100% of crypto's element reads and 98.6% of
   NavierStokes's, and 6,059 of zlib's 2,128,486,098 — 0.0%. The reason is
   `jitEmitTagCheck(a, recv, TArr, slow)`: a typed array is not a fast array, so
   every read of one is a helper round trip. Measured rather than reasoned:
   **100.0% of the element-read misses in zlib, mandreel and gbemu have a
   TTypedArray receiver** — 2.33 billion round trips across three workloads,
   which is the largest number anywhere in this document by two orders of
   magnitude.

   Two things make it tractable. Every view in the corpus is *fixed* — not one
   length-tracking view over a resizable buffer, and not one non-zero
   byteOffset — so the bound is `t.length` and the address is `i*size`. And the
   guard the read needs beyond a fast array's is small: the view is not
   detached, and the element kind is the one compiled for.

   That last clause looked like the design question, because no kind dominates a
   workload: zlib is 53% Int32, mandreel 50% Uint16, gbemu 67% Uint8, six kinds
   between them (Int8, Uint8, Int16, Uint16, Int32, Float32). Emitting one kind
   for every site would capture about half; emitting all six inline is forty
   instructions on a path that runs two billion times.

   **Counted per site, it is not a question at all.** Every element-read site in
   all three workloads sees exactly one kind:

   | | misses | sites | at single-kind sites |
   |---|---|---|---|
   | zlib | 2,128,480,034 | 791 | **100.0%** |
   | mandreel | 192,858,491 | 1,673 | **100.0%** |
   | gbemu | 18,610,223 | 116 | **100.0%** |

   2,580 sites and not one polymorphic. So a site needs a record of the kind it
   has been reading and one emitted load — not a dispatch — and that record is
   the first thing in this tier compiled from **type feedback** rather than from
   the bytecode or from a shape cache. Which is what phase 1 asked for, arrived
   at by counting rather than by assuming.

   The shape of it:

   - `svFunc` grows a lazily allocated `[]uint8` indexed by bytecode offset, one
     byte per code byte, holding kind+1 (zero for a site not yet seen, and a
     sentinel for one that has seen two). Indexed by ip rather than by a site
     number because OpGetElem has no operand to spare and adding one is a
     bytecode format change for a table that costs a byte per opcode byte.
   - The interpreter records it, which is where the property caches are filled
     too, and for the same reason: a function reaches the compiler only after
     the interpreter has run it. With the threshold at 8 a site has run seven
     times before it is compiled.
   - The emitter reads it and emits the tag check for `TTypedArray`, the resolve,
     `t.track == 0`, `t.kind == K`, the integer-key test the fast-array chain
     already has, `idx < t.length`, and one load — `MovzxRegMem8` for a byte
     kind, `Mov32RegMem` + `MovsxdRegReg` for Int32, and so on. No new encoder
     instructions are needed for the integer kinds; Float32 wants a cvtss2sd.
   - A site whose kind was never recorded keeps today's behaviour exactly.

   The detached check is the one that cannot be skipped: a detached buffer keeps
   its length but loses its bytes, so the bound has to come from the byte slice
   rather than from `t.length` alone.

1. **The site that gives up, and the half of it that is still on the table.**
   Half of this item has been built — `icMissLimit`, see below — and what remains
   is the width.

   `icWays` from 8 to 16 is one character and it is the largest single number on
   this list: **+23.4% on box2d** and **+14.0% on DeltaBlue**, +1.4% geomean. It
   is also the only thing measured this session that makes a benchmark
   materially worse, taking 6.5% off EarleyBoyer and 2.7% off TypeScript, and the
   reason is not the scan — a bounded scan leaves EarleyBoyer at exactly the same
   914 — but the memory: every site doubles from 320 bytes to 640, and the
   workloads that lose are the ones that touch the most sites.

   So it wants a width per site rather than a width per build, and the constraint
   that makes that hard is worth stating: the win needs the extra ways reachable
   **from machine code**, and the emitted probe reads its ways at a constant
   address. Anything behind a slice header is a helper round trip, which is the
   same thing the bounded-scan experiment measured from the other direction. Two
   pools of sites — narrow and wide, each with its own emitted probe — is the
   shape that could work.
2. **The 1.56 million reads the emitted probe declines that the cache answers
   anyway** — `JITNarrowStats()` counts them, and the guard chain in machine
   code is narrower than `icWay.hit` for a receiver that is not a plain object.
   Widening the tag check to the object family is not free: the tags are not
   contiguous, and a naive mask test costs about three instructions on all 59.7
   million reads against the round trips it saves. It has to keep the `TObj`
   path at its current cost, and it has to be measured rather than reasoned
   about.
3. **The store that creates a property, emitted rather than helped.** The helper
   reaches it, which recovered EarleyBoyer's 18%, but each one still costs an
   exit and a re-entry where the interpreter pays neither. Installing a shape
   would be the first *Go pointer* this tier stores from machine code, so it is
   the first that needs an argument about write barriers rather than the
   standing one that a Value is an integer.
4. **`op:SPECIAL_OBJ`** — 469 functions in the corpus have it as their only
   missing opcode and 832 meet it first, both the largest by some way. It is
   `arguments`, and the two halves are not the same problem: unmapped (strict)
   is a plain array-like, while a sloppy mapped one writes through to the
   frame's argument array — which is exactly the invariant that lets a compiled
   callee be handed the caller's spill area. Building one closes the door on the
   other.

   The interpreter has the same problem one level down, and mjsunit found it:
   the mapping holds while the frame is live and breaks in both directions once
   a closure outlives it. `function f(x) { var a = arguments; return function
   (v) { a[0] = v; return x } }` returns the original argument rather than `v`,
   and so does the converse. Two mjsunit tests fail interpreted and pass
   compiled for this reason — the compiled call hands the callee the caller's
   spill area, so it is accidentally right about the case the interpreter gets
   wrong. It is the same underlying work as item 5: a parameter that is both
   mapped and captured has to live in one place, and right now it lives in two.
5. **`op:CLOSURE`** — 307 alone, 378 first. The allocation is a helper call; the
   problem underneath is that a captured local has to outlive the frame, and
   compiled code addresses locals as a raw pointer into a slice. Boxed locals is
   a frame-model change, and `SET_UPVAL`/`PUT_UPVAL` are the same problem.

**The site that stops caching while it is still hitting** has come off this list
by being fixed, and it is the one thing this session changed that made anything
faster. A site with eight ways and ten shapes misses on about one access in five
however well it does on the other four, so it reaches any fixed miss count
eventually and then gives up *at an 80% hit rate*. The count was 32, and the
comment above it already contained the argument against itself: a site seeing
nine or ten shapes "is not megamorphic, it is merely wider than the cache".

Two Octane workloads reach that state and they are the only two — a retired site
is consulted 16.9 million times in box2d and 1.68 million in DeltaBlue, and never
in EarleyBoyer or RayTrace. Raising the count to 250 (the counter is a byte) is
**+9.9% on box2d**, +1.7% on DeltaBlue, +0.9% geomean, and costs nothing at all:
no memory, nothing on the hit path, and not one workload out of fifteen worse
than noise.

| | 8 ways, limit 32 | 16 ways, limit 32 | 8 ways, limit 250 |
|---|---|---|---|
| box2d | 1626 | 2006 | **1787** |
| deltablue | 890 | 1015 | 905 |
| earley-boyer | 978 | 914 | 976 |
| typescript | 4788 | 4657 | 4794 |
| geomean | — | +1.42% | **+0.92%** |

The count should be a *rate*. Nothing here can tell a site that hits four times
in five from one that never hits, and that is the whole defect; 250 is a longer
leash on the wrong measurement, chosen because it is measured and free rather
than because it is right.

The bounded scan has come off this list by being built and measured, and it took
three findings with it — none of which was the one it was built for.

It is the first thing this tier ever compiled from *feedback* rather than from
the bytecode, which is what made it worth trying. A function reaches the compiler
only after the interpreter has run it, so its cache sites already record the
shapes the program really passes through them. Reading that count and emitting
exactly that many comparisons makes most sites emit one: EarleyBoyer's are 99%
single-shape and NavierStokes's 100%, box2d's 53%, DeltaBlue's 47%, Richards's
36%. The rest of an eight-way probe was five comparisons proving that five empty
ways are still empty.

| | 8, unbounded | 8, bounded | 16, bounded | 16, unbounded |
|---|---|---|---|---|
| richards | 1365 | 1398 | 1402 | 1380 |
| deltablue | 890 | 853 | 884 | **1015** |
| raytrace | 661 | 654 | 639 | 643 |
| navier-stokes | 3396 | 3307 | 3301 | 3428 |
| earley-boyer | 978 | 968 | 914 | 914 |
| box2d | 1626 | 1635 | 2017 | 2006 |

**At eight ways the width is not the constraint.** DeltaBlue's probes served
87.6% of reads narrow against 87.4% wide. The emitted scan already exits at the
first match, so narrowing shortens only the *miss* — and a miss is dominated by
the round trip after it, not by three comparisons. Fourth time something here has
been emitted, been correct, served everything that reached it and moved nothing.

**At sixteen it is actively harmful, and that is the finding.** Compare the last
two columns: DeltaBlue is 884 bounded and 1015 unbounded, so bounding the scan
cost it 13% of itself. The bound is read at *compile* time, and a site fills the
ways it needs afterwards — so the ways that arrive later are unreachable from
machine code and every access to one declines to a helper. Feedback compiled in
is feedback frozen, and the widening machinery built to unfreeze it recovers less
than it costs.

The EarleyBoyer column is the control: 914 either way, so its −6.5% is not scan
length at all. It is the memory — a site doubles from 320 bytes to 640 — which
this list's old entry attributed to scanning sixteen ways to miss. Wrong, and it
was hiding item 1.

**And the bet has to be widened, which is where the find was.** A site can grow
after its width is compiled in, so the miss helper notices and rebuilds the
function — and rebuilding cost DeltaBlue 12% by a route with nothing to do with
property caches. That is the rebind bug above. It was worth the whole experiment:
every optimisation that replaces a block would have met it.

The scan is reverted. What replaced it at the top of the list is what the last
column turned out to mean.

The compiled call has come off this list by being built, and it is the one item
that was worth what the profile said it was worth. Everything else on this list
has been smaller than it looked; that one was not, because what it removed was
not a layer but a boundary. It also produced the two hardest bugs in the tier's
history — see above — and neither was the kind a conformance suite finds.

Coverage led this list for three sessions and is no longer at the top of it.
Twice in one session, compiling more code made a benchmark *slower* — the method
call cost DeltaBlue 9% until inherited slots were served, and the store cost
EarleyBoyer 18% and still does. Coverage moves work out of the interpreter; it
does not make the work cheaper, and the interpreter's caches were already serving
some of it.

`GET_FIELD2` has come off this list by being built, and it proved the weighted
table right twice over: worth exactly zero entries on its own — `CALL_METHOD` is
always the instruction after it — and worth 37 points of DeltaBlue once that was
in too.

`local-not-assigned` has come off it too, and it is the one that taught the most:
it was worth 1.04M and 1.82M entries on the table, it was implemented, and the
entries went to `this-local` rather than to compiled code. Those eleven functions
per benchmark were never blocked by definite assignment. The table names the
first wall, and behind it there is sometimes another one.

The declines have come off it as the same lesson pointing the other way. They
were not on any list, because nothing was refusing: the functions compiled, ran,
and turned every caller away at the door. A refusal is counted and a decline was
not, so the one thing costing 195,342 frame entries in richards was the one thing
no histogram had a column for.

The inline cache has come off this list, and so has the thing that turned out to
be underneath it: a prologue that checked every parameter meant no object could
enter compiled code, so the cache would have been correct and unreachable. That
is the lesson worth carrying forward — a guard that rejects a type is also a
guard that prevents ever handling it, and the two are easy to confuse when the
guard is the one making everything else sound.

The one-way probe has come off it as the same lesson a level down: it was
verified against a site with one receiver, which is the only case where way 0 is
the answer. So has the allocation on every frame entry, which no conformance
suite and no score could have shown and `-benchmem` reported in one line.

The generic operators have come off it too, and they are the smaller half of
that lesson. They were listed first because the cache was waiting on them, and
they were: `dist(p)` above could not compile at all while `dx*dx` needed a known
Number. But by function count they were worth 43 of 6,976, because the wall in
front of this tier is not what it can do with a value — it is that it cannot
store one, read a global, or call anything.

The store and the global read have come off it as the third turn of the same
lesson, and the sharpest one: both were emitted, both were correct, both served
100% of what reached them on a microbenchmark, and neither moved a score. The
store's probe could reach four properties of an object and the global read's
could reach none, and every test passed throughout because a probe that declines
gives the same answer as one that hits. The fix — an address computation shared
by both — is smaller than either feature, and finding it took building the
weighted diagnostic rather than reading the corpus count again.

Tiering has come off this list too. Counters and a compile threshold are in, and
so is the case a threshold cannot see: a function called once whose loop is hot
never reaches a call count, and used to run to completion in the interpreter
however long it took. Entering compiled code at a loop header costs nothing to
support because a back edge already has an empty operand stack and every live
value in the locals array — the same state the fuel exit resumes from. That case
is 5.6x.

Phase 1's type feedback was not on this list because this tier guarded rather
than speculated — an assumption it could not check at entry was an assumption it
could not make, so feedback had nothing to be spent on. That is what the bail
above removes, and it is the only thing that was in the way: the tier has read
its own caches once already (the bounded probe, reverted for other reasons), so
there is feedback to hand and now somewhere to put it. It moves back up the list
here.

## Static coverage, and where it stops

Coverage of the Octane corpus went from 71.3% to 99.9% (6,966 of 6,976
functions) across eleven commits, and the shape of that work was the same every
time: a template is half of it, and three separate analyses are the other half.

`jitStackEffect`, `jitNumberDemand` and `jitNumericLocals` each end in a
`default: return false`, so an opcode with a template but no arm in all three
compiles nothing — the function is refused for the stack discipline instead,
which names the wrong thing. Eight templates in a row moved coverage by zero
before that was clear. `TestEveryTemplateIsKnownToTheAnalyses` is what keeps it
from happening again.

Three of the eleven were structural rather than another template:

  * **Tail calls** are an exit rather than a helper, because a helper runs with
    the compiled frame still on the Go stack and that is exactly what a proper
    tail call promises not to do. The trampoline in `jitRunAt` takes over the
    frame at the current depth, so a chain occupies one frame however long it
    runs — checked at 100,000 deep, which is test262's own bar.

  * **The operand window.** Nine registers was the whole stack a function could
    have. It is now the top nine of an array, with slot `i` in register
    `i mod 9`, so a push past nine evicts the slot exactly nine below into the
    register the new one wants. Both edges are emitted from `jitStackEffect` at
    the top and bottom of the dispatch loop rather than in each template — which
    also means the emitter now checks its own depth against the analyses after
    every instruction rather than only at labels.

  * **try/catch** is a compile-time answer. Which handler is in force propagates
    along the same block edges as everything else, and each call out records
    where a throw from it lands, keyed by the address it would have resumed at.
    Pairing TRY_PUSH with TRY_POP by counting would have been simpler and wrong:
    `break` out of a try body emits its own TRY_POP, so a scan stops early and
    leaves the rest of the body looking unprotected.

Ten functions are left and none of them is worth a template for its own sake —
every one is a top-level wrapper or a one-shot initialiser:

| reason | functions | what it would take |
|---|---|---|
| `op:TRY_PUSH_FINALLY` | 5 | completion records: a `return` or `break` crossing a finally has to become a recorded completion and a jump, and FINALLY_RET has to dispatch back to a bytecode ip |
| `op:WITH_GET_VAR` | 4 | a per-frame scope-object chain, plus three name-resolution opcodes whose flag byte selects between five fallbacks |
| `op:EVAL` | 1 | a dynamic variable store, which the engine does not have on any tier |

So 100% is not reachable: the last function is a direct eval, and sloppy `var`
inside eval writes into the enclosing function's variable environment, which a
flat locals slice cannot represent. The other nine are reachable and cost more
than they are worth — none of them runs more than once.

Two things nearly went in unnoticed along the way, and both are worth naming
because everything passed while they were true. Sizing a per-compile array from
the depth *bound* rather than from the function took the test suite from 38
seconds to seven minutes. And giving every frame a heap-allocated operand array
— to serve nineteen cold functions — cost between three and eleven percent
across Octane, in a pointer load per spill and a slice header per call out.
Neither shows up in a conformance suite or a coverage count.
