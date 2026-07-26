// Shared harness, concatenated ahead of each workload by cmd/goant-bench.
//
// Timing happens INSIDE the process. Measuring from outside would work for
// goant, which starts in about two milliseconds, but node, deno and bun take
// tens of milliseconds to start — more than a V8-JITted workload takes to run,
// so the startup noise would be the measurement.
//
// The repeat count grows until a run lasts long enough for Date.now()'s
// millisecond resolution to mean something. That also gives a JIT time to warm
// up, so what gets reported is steady-state throughput rather than the first,
// interpreted pass. The result is nanoseconds per unit of work, which is
// comparable across engines and independent of how big the workload is.
globalThis.bench = function (work, unitsPerCall) {
  var reps = 1, elapsed = 0, out;
  for (;;) {
    var t0 = Date.now();
    for (var i = 0; i < reps; i++) out = work();
    elapsed = Date.now() - t0;
    if (elapsed >= 120 || reps >= (1 << 26)) break;
    reps *= 2;
  }
  // Kept in a global so nothing here can be eliminated as dead.
  globalThis.RESULT = out;
  console.log((elapsed * 1e6 / (reps * unitsPerCall)).toFixed(3));
  return out;
};
