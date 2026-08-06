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

## The silent half, which is the one that matters

`push` raised in the reproducer above only because the receiver it was handed
happened to be a FUNCTION, and `Function.prototype.length` is non-writable. When
the stolen receiver is a plain object, `push` SUCCEEDS — into the wrong object:

    var out = [];
    function f0(a, b) { return a; }
    var Plain = { make: function () { return 1; } };
    function body() { out.push(String(f0(1, f0(1, f0(Plain.make(), 1))))); }
    for (var r = 0; r < 12; r++) body();
    console.log(out.length, Object.keys(Plain));

    goant repro.js              # 12  [ "make" ]
    GOANT_JIT=1 goant repro.js  # 7   [ "0","1","2","3","4","make","length" ]

Five of the twelve pushes went into a bystander object. Nothing raised, nothing
was logged, and the array is simply short. That is silent cross-object data
corruption in the default configuration, and it is the reason this finding is
worth more than the four bugs fixed today put together.

## What the reduction established

Each of these was checked by removing exactly one thing:

- **Operand depth is required.** The nesting sweep fails at five levels and
  passes at four. `jitStackWindow` is 9, and the receiver of `push` sits at slot
  0 — the bottom of the operand stack, which falls out of the register window
  once the argument expression grows past it. Below the window a slot lives in
  the frame's memory array, and `jitSpillArgs` reads the receiver from there.
- **An inner METHOD call is required**, and the outer call ends up with the
  INNER call's receiver. That is why `Object.create(...)` and `Object.keys(...)`
  trigger it — `Object` is a function, which is what the corrupted receiver was
  measured to be — while `Math.max(...)` does not, because `Math` is not. A
  custom `Holder.make()` reproduces it exactly; the same depth built from plain
  calls does not, nor does an object literal in that position.
- **Depth decides it, periodically.** Nesting 3 is clean, 3-plus-one corrupts, 4
  raises, 5 is clean again. `jitSlot` is `regs[i % jitStackWindow]` with a window
  of 9, so slot 0 and slot 9 share a register — the outer receiver's slot and the
  inner call's.
- **The interpreter is correct** at every depth, so this is the tier.
- **It is pre-existing**, reproducing at `6e1a135`, before any of this session's
  work.

The reading that fits: the outer receiver's slot has left the register window and
lives in memory, its register has been reused by the inner call's receiver, and
on some path the eviction or the refill around the inner CALL_METHOD does not
keep the two in step — so the outer call reads the inner receiver.

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
