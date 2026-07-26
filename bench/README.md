# bench

Workloads shared by two harnesses, so their numbers mean the same thing:

- `cmd/goant-bench` runs each file under goant and under whichever of
  node / deno / bun is installed, and reports wall time with process startup
  subtracted. This answers "how far from V8 are we".
- `internal/engine/bench_test.go` runs the same files through `go test -bench`,
  which is where `-cpuprofile` and `-benchmem` live. This answers "why".

Each file is a self-contained script that does a fixed amount of work and
leaves a checksum in the global `RESULT` so a dead-code-eliminating engine
cannot skip it. Sizes are chosen so node takes tens of milliseconds — small
enough that goant, which is currently far slower, still finishes quickly.

`_empty.js` is the startup baseline and does nothing.
