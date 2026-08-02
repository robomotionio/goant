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

`GOANT_JIT=1` turns the tier on. It is off by default: it is measured at level
with the interpreter on Octane, so it cannot yet pay for itself on a mixed
workload, and an execution path that is not the default is one to justify rather
than assume.

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

## What the tier is worth on Octane: nothing yet

`GOANT_JIT_STATS=1` counts frame entries by where they ran. Scores are
higher-is-better; the two arms are interleaved per benchmark so drift cannot
favour either. Two runs of each arm, alternating, on the benchmark VM, with
everything in this document in:

| | on | off | on | off |
| --- | --- | --- | --- | --- |
| Richards | 227 | 227 | 227 | 214 |
| **DeltaBlue** | **263** | **263** | **263** | **263** |
| Crypto | 252 | 253 | 253 | 252 |
| RayTrace | 404 | 400 | 403 | 403 |
| NavierStokes | 453 | 454 | 452 | 453 |
| Splay | 1927 | 1907 | 2079 | 1933 |
| RegExp | 147 | 147 | 147 | 147 |
| EarleyBoyer | 511 | 512 | 511 | 511 |

**DeltaBlue is the whole result.** 263 in all four columns — and 22.4% of its
frame entries are executing as machine code. A quarter of the frames in that
benchmark went from the interpreter to compiled code and the score did not move
by one point in either direction.

That is a much sharper statement than "the tier does not help yet", and it says
something the coverage numbers cannot: **compiled code is running at almost
exactly interpreter speed on the work DeltaBlue does.** Not slower, which the
frame-entry allocation and the one-way probe used to make it. Not faster either.
The property probe is about thirty instructions and the interpreter's path to the
same answer is a Go call chain that costs about the same, so moving a frame
between them buys nothing.

Richards agrees at 5.0% of frame entries, RegExp is identical because none of it
compiles, and the rest are within the drift below.

**How much of a difference this table can see.** DeltaBlue's *interpreter* arm —
the one nothing in this document touches — read 249–250 in one session, 260–266
in the next and 263 in this one; EarleyBoyer's read 623 and then 511. That is 4%
to 18% of run-to-run drift, so only the two arms of a single interleaved run are
comparable to each other, and a 1–2% difference between them is at the noise
floor rather than above it. Every "unchanged" here means "not distinguishable
from unchanged". DeltaBlue's four identical columns are the exception and are
worth the most, because they are four measurements of the same thing that agree.

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

### What that changes about the plan

Coverage was the bottleneck and is no longer the only one. The list above still
matters — `stack-across-blocks` is 1.6M frame entries in two DeltaBlue functions
— but reaching 40% or 60% of frame entries by the same route would, on this
evidence, produce the same 263.

What has to come down is the cost *per operation*, and the measurement for that
already exists: a call between two compiled functions is **1.15 ns against 4.69
ns** for the exit-and-re-enter detour, and 70 ns of interpreted frame setup
disappears with it. Every compiled function today is an island — it is entered
from the interpreter, and every call it makes goes back out. DeltaBlue's 22.4%
is 1.55 million frames that each pay full entry and exit for work that is
otherwise interpreter-speed.

The hope early on was that a numeric compiler would miss many functions but catch
the ones that run in loops. It was the other way round, and the fix for that —
methods, stores, globals, fields past the fourth — is now in. The next thing is
not another opcode. It is making a compiled function cheap to *call*.

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

## Still to do

Rewritten 2 August, three times: after measuring the numeric operators, after
the inline cache landed, and after the generic operators made the cache's hit
rate measurable on real code.

**This list is now ordered by measurement rather than by corpus count.** The
static histogram is still worth reading — 521 of 6,976 functions compile, and
`GET_ELEM` 1321, `GET_FIELD2` 1268, `GET_UPVAL` 918, `SPECIAL_OBJ` 832 lead the
refusals — but the entry-weighted table above is what decides what to build. It
has disagreed with the corpus count every time they have been compared.

1. **Calls, compiled to compiled** — promoted to the top by DeltaBlue's four
   identical columns, and now with a constraint attached. Measured at 1.15 ns
   against 4.69 ns for the detour through the runtime, plus the ~100 ns of
   interpreted frame entry that disappears with it. Every compiled function
   today is an island: entered from the interpreter, and every call it makes
   goes back out. This is also what makes `GET_FIELD2`, `NEW` and `CLOSURE` worth
   anything, since all three are refused in functions that call.

   The constraint, from the attempt above: the saving has to be taken **without
   adding a branch to `runFrameBody` or `runFrame`**, because doing so costs the
   interpreter more than the saving is worth. Which points at the call site — a
   compiled caller reaching a compiled callee directly — rather than at a
   cheaper frame entry bolted onto the shared path.
2. **`stack-across-blocks`**, which is not an opcode: the two analyses that walk
   the emitter's stack discipline model every block as starting empty, so a
   block reached with an operand still live refuses the whole function. The
   largest coverage item left — 1.08M entries in richards in *one* function,
   1.60M in deltablue in two — and the shape that produces it is ordinary:
   `a && b`, `a || b` and `a ? b : c` all jump with a value on the stack. The
   work is a per-block entry depth in place of the "must be zero" rule, which the
   positional register assignment then follows for free provided every
   predecessor agrees.
3. **`non-numeric-operand`** (579k in richards, 4 functions) and **`GET_ELEM`**
   (940k in deltablue, 2 functions).
4. **An arm64 emitter.** `jitmem` is already in place and tested for it; the
   emitter is mechanical once the amd64 shape has stopped moving.

Coverage led this list for three sessions and has been demoted on evidence:
DeltaBlue reached 22.4% of frame entries in compiled code and scored 263 against
the interpreter's 263. More of the same buys more of the same.

`GET_FIELD2` has come *off* this list, having been item 2 on it. It is the
second-largest blocker in the static corpus and worth exactly zero entries on its
own, because `CALL` is always the next instruction.

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

Phase 1's type feedback is not on this list because this tier guards rather than
speculates. It moves back up when there is a second tier to feed.
