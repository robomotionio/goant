# Open differential-fuzz findings

Inputs for `FuzzJITAgreesWithTheInterpreter` that still fail. They live here
rather than in `internal/engine/testdata/fuzz/` on purpose: a corpus entry is run
by an ordinary `go test`, so leaving them there would make the suite red for
everybody and the failures would stop meaning anything. The passing entries — 51
of them, every bug fixed so far — stay in the corpus as regressions.

To reproduce one, copy it back and name it:

    cp docs/jit-fuzz-findings/<name> \
       internal/engine/testdata/fuzz/FuzzJITAgreesWithTheInterpreter/
    go test ./internal/engine/ -run 'FuzzJITAgreesWithTheInterpreter/<name>'

## All thirteen are one bug: a method call gets the wrong receiver

**This is reachable in the shipped configuration.** An earlier draft of this file
said it needed a lowered threshold; that was an artifact of the larger program
the fuzzer happened to produce. Reduced, it fails at the default threshold of 8.

    var out = [];
    function f0(a, b) { return a; }
    function body() { out.push(String(f0(1, f0(1, f0(Object.create({a:9}), 1))))); }
    for (var r = 0; r < 40; r++) {
      try { body(); } catch (e) { console.log("FAIL@" + r + ": " + e.message); break; }
    }
    console.log("len:", out.length);

    goant repro.js              # len: 40
    GOANT_JIT=1 goant repro.js  # FAIL@7: Cannot assign to read only property 'length'

The symptom is a `TypeError` about a read-only `length` from `Array.prototype.push`,
on an array that is demonstrably healthy — `writable: true`, not frozen,
extensible. The array is not the problem. Instrumenting `setLengthOrThrow` shows
the receiver it was handed has **tag 3, T_FUNC**: `push` ran with a function as
its `this`, and `Function.prototype.length` is non-writable, so the assignment is
correctly refused for an object that should never have been there.

So this is not a wrong answer about arrays. It is `obj.method(...)` reaching the
runtime with something other than `obj`.

## What the reduction established

Each of these was checked by removing exactly one thing:

- **Operand depth is required.** The nesting sweep fails at five levels and
  passes at four. `jitStackWindow` is 9, and the receiver of `push` sits at slot
  0 — the bottom of the operand stack, which falls out of the register window
  once the argument expression grows past it. Below the window a slot lives in
  the frame's memory array, and `jitSpillArgs` reads the receiver from there.
- **A native call is required.** `Object.create({a:9})` and `Object.keys({a:9})`
  both trigger it at the deepest position. The plain object literal `{a:9}` in
  the same position does **not**, and neither does an array literal. So it is the
  CFunc call path, not the allocation.
- **The interpreter is correct** at every depth, so this is the tier.
- **It is pre-existing**, reproducing at `6e1a135`, before any of this session's
  work.

The reading that fits: a slot below the register window is not written back to
memory on some path through a native call, so the receiver `jitSpillArgs` reads
is stale — whatever the register held before, which at that depth is one of the
callees on the operand stack, hence a function.

That is a hypothesis about WHICH path, not about the shape, and no fix should be
written until the path is identified. The eviction is driven by the predicted
stack effect (`jitEvictSlots`, called once per instruction from the depth the
analysis expects); the same emitter loop has three arms that leave with
`continue`, and one of those skipping its bookkeeping is exactly the bug that was
fixed in the catch-stamping this session. That is where to look first.

## Why it matters more than a wrong answer

A method call silently receiving the wrong `this` does not usually raise. Here it
did, because `push` happened to write `length` and functions refuse that. The
same corruption on a method that only reads, or writes a normal property, is a
silently wrong result or a mutation of the wrong object.
