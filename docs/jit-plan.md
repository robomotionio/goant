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

## What the tier is worth on Octane: nothing yet

`GOANT_JIT_STATS=1` counts frame entries by where they ran. Static coverage —
6.6% of functions — turns out to be a poor guide to anything. Scores are
higher-is-better; the tier is on for the second column, and the two arms are
interleaved per benchmark so drift cannot favour either.

| | off | on |
| --- | --- | --- |
| Richards | 226 | 221 |
| DeltaBlue | 252 | 246 |
| Crypto | 256 | 252 |
| RayTrace | 397 | 394 |
| EarleyBoyer | 624 | 613 |
| Splay | 1981 | 1978 |
| NavierStokes | 462 | 454 |
| RegExp | 147 | 147 |

Unchanged, and slightly down in every column — the tier costs a compile attempt
and a check per frame entry, and returns nothing here.

The generic operators did change one thing the scores cannot show. Compiled code
used to execute **zero** property reads across the whole suite, because the 6% of
functions that compiled were numeric leaves that never touched a field. Now
DeltaBlue executes 283,096 of them and EarleyBoyer 225,432. Against 8.4M and
29.4M frame entries that is still a rounding error, which is why the scores do
not move — but the cache is no longer sitting in code nothing runs.

The hope was that the split would favour the tier — that a numeric compiler would
miss many functions but catch the ones that run in loops. It is the other way
round. The functions Octane spends its time in are the ones that allocate,
dispatch over a class hierarchy, close over variables and call each other, which
is precisely the set this tier refuses.

So there is no shortcut here and no 80/20. Reaching a score that competes with
node on this suite needs calls, stores, closures, upvalues, globals, arrays and
exceptions — most of the language, not a numeric core with a long tail. The
measurement is worth having early: it is the difference between a plan and a
hope, and it is what keeps a 15.6x microbenchmark from being mistaken for
progress on the thing that was actually asked for.

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

**What the histogram says.** The list here used to argue that the numeric
operators were not worth building. They were: `SHR` alone blocked 892 functions
and the sole-blocker column was reading the marginal case, not the aggregate.
They are all in, they cost one runtime call in a range real code never reaches,
and arithmetic has disappeared from the refusal histogram completely. What is
left is **88% memory access** — GET_GLOBAL 1761, GET_FIELD2 962, PUT_FIELD 870,
SPECIAL_OBJ 832, GET_UPVAL 824, GET_ELEM 323, CLOSURE 277.

**What the histogram does not say.** None of it moved Octane, which is unchanged
with the tier on. Integer kernels went 1.6x to 5.6x in the same build and a
property read in isolation 14.6x. A tier is worth what its *hot* coverage is
worth, and static coverage is a poor proxy for it: 6.6% of functions compile and
0.0% to 0.4% of frame entries land in them. Nor is a microbenchmark a proxy for
a hit rate — the same cache that is 14.6x on one receiver serves 1.0% of the
reads on a hundred.

1. **`PUT_FIELD`**, the store side of the same cache. `icFillPutTransition`
   already records the case that matters most — a store that *creates* the
   property, which is what initialising a fresh object is made of — and it was
   measured at 8% of EarleyBoyer on the interpreter side alone. It is now the
   largest sole blocker in the corpus by a factor of seven: 597 functions have
   no other unsupported opcode in them, and it reuses the read's guard chain
   whole.
2. **`GET_FIELD2`**, which is the same probe with a different stack effect (962
   functions), and is what `o.m()` compiles to. Worth little before calls and
   nearly free after this one.
3. **Globals and upvalues** — the largest single blocker at 1761 and 824 — and
   the same problem with a simpler shape: the binding is found once and the
   guard is a version check rather than a prototype walk.
4. **Calls, compiled to compiled.** Measured at 1.15 ns against 4.69 ns for the
   detour through the runtime, so the convention has to be decided before the
   first one is emitted. This is also the 70 ns of frame setup that phase 3
   could not touch.
5. **An arm64 emitter.** `jitmem` is already in place and tested for it; the
   emitter is mechanical once the amd64 shape has stopped moving.

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

Tiering has come off this list too. Counters and a compile threshold are in, and
so is the case a threshold cannot see: a function called once whose loop is hot
never reaches a call count, and used to run to completion in the interpreter
however long it took. Entering compiled code at a loop header costs nothing to
support because a back edge already has an empty operand stack and every live
value in the locals array — the same state the fuel exit resumes from. That case
is 5.6x.

Phase 1's type feedback is not on this list because this tier guards rather than
speculates. It moves back up when there is a second tier to feed.
