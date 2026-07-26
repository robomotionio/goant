// Appended after Octane's base.js and one benchmark file. Octane ships a
// browser runner and a shell runner that needs a `load()` builtin; neither
// exists in all four engines, so goant-bench concatenates the files instead and
// finishes with this. BenchmarkSuite.RunSuites is fully synchronous when there
// is no `window`, so nothing here has to wait.
//
// The last line of stdout is the score, higher being faster — the opposite of
// the microbenchmarks, which report nanoseconds.
(function () {
  var failed = null;
  BenchmarkSuite.RunSuites({
    NotifyError: function (name, error) {
      failed = name + ": " + (error && error.message ? error.message : error);
      console.log("ERROR " + failed);
    },
    NotifyScore: function (score) {
      if (!failed) console.log(score);
    },
  });
  if (failed) {
    // Exit non-zero so the harness reports the engine as failing this benchmark
    // rather than silently scoring it.
    if (typeof process !== "undefined" && process.exit) process.exit(1);
    throw new Error(failed);
  }
})();
