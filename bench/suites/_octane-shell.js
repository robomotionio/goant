// Prepended before Octane's base.js, ahead of every workload.
//
// Octane was written for a JavaScript shell — d8 — and expects the globals one
// provides. Two of them are missing here, and zlib does not merely tolerate the
// absence: its emscripten preamble decides it is running in a shell (no
// `window`, no `importScripts`, and no `require`, since that is a CommonJS
// module binding rather than a global and an indirect eval cannot see it) and
// then captures both by name, so the file fails to load without them.
//
//   print — node has console.log and nothing else. The other four engines have
//           print already and keep theirs.
//   read  — no engine here has it. Nothing in the suite calls it, because
//           goant-bench concatenates the data files instead of loading them, so
//           the reference only has to exist. It throws rather than returning
//           something empty, so a workload that really does need a file fails
//           loudly instead of quietly measuring the wrong thing.
//
// This is the harness matching Octane's environment, not the benchmarks being
// adjusted: every engine sees exactly the same shim.
(function (global) {
  if (typeof global.print !== "function") {
    global.print = function () {
      console.log(Array.prototype.join.call(arguments, " "));
    };
  }
  if (typeof global.read !== "function") {
    global.read = function (name) {
      throw new Error(
        "read(" + name + ") is not available: goant-bench concatenates " +
          "Octane's data files rather than loading them"
      );
    };
  }
})(typeof globalThis !== "undefined" ? globalThis : this);
