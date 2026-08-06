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

## What they are

All 13 are the same shape, and it is a TIER bug rather than a wrong answer in
the engine: a `TypeError: Cannot assign to read only property 'length'` that the
interpreter never produces, thrown from `Array.prototype.push` on an array that
is demonstrably healthy — `writable: true`, not frozen, extensible, and with a
plausible length — at the moment the surrounding function tiers up.

What is established:

- It is **pre-existing**. It reproduces at `6e1a135`, before any of this
  session's work.
- It is **threshold-dependent**: fails at `GOANT_JIT_THRESHOLD=1` from the first
  round and at `2` from round 18, and **passes at the default of 8**. So the
  shipped configuration does not reach it, which is why nothing had noticed.
- It is in the **compiled call path**, not the decline path. Declines are
  identical at every threshold (32); calls the site made itself are not — 81 at
  threshold 1, 40 at 2, 28 at 8 — and the failure tracks that number.
- Instrumenting the rejection shows the failing `push` is not on the array the
  program is building: the state printed at the throw (`n=2`, `arrLen=0`) is
  inconsistent with the array the program can see (`length: 18`). Something is
  arriving at `setLengthOrThrow` for an object other than the intended receiver.

The reading that fits all four is an argument or receiver mismatch on a call a
compiled site made itself, which `setArrayLengthTo` then answers correctly for
the wrong object. It has not been proven, and no fix should be written until it
is: the numbers above are the evidence any candidate has to explain.

A reproducer that does not need the fuzzer:

```js
var out = [];
function f0(a, b) { return a; }
function f1(a, b) { try { return a[b]; } catch (e) { return "E"; } }
function body(v0, v1) {
  out.push(String(f0((f1(0, v1) * (v1 * Object.create({a:9}))),
                     f0((v1 * Object.create({a:9})), f0(Object.create({a:9}), v0)))));
}
for (var round = 0; round < 40; round++) {
  try { body(0, Object.create({a:9})); }
  catch (e) { out.push("T:" + e.message); }
}
console.log(out.slice(15, 21).join(" | "));
```

    GOANT_JIT=1 GOANT_JIT_THRESHOLD=2 goant repro.js   # TypeErrors from round 18
    GOANT_JIT=1 goant repro.js                         # clean at the default
