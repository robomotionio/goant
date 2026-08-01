# bench

Workloads shared by two harnesses, so their numbers mean the same thing:

- `cmd/goant-bench` runs each file under goant and under whichever of
  node / deno / bun is installed, and reports wall time with process startup
  subtracted. This answers "how far from V8 are we".
- `internal/engine/bench_test.go` runs the same files through `go test -bench`,
  which is where `-cpuprofile` and `-benchmem` live. This answers "why".

Each file is a self-contained script that does a fixed amount of work and
leaves a checksum in `globalThis.RESULT` so a dead-code-eliminating engine
cannot skip it. Sizes are chosen so node takes tens of milliseconds — small
enough that goant, which is currently far slower, still finishes quickly.

`_empty.js` is the startup baseline and does nothing.

## The other engines

`goant-bench` scores whichever of `goja-run`, `node`, `deno` and `bun` is on
PATH, skipping the rest. node, deno and bun are the JIT reference; **goja is the
comparison that matters**, being the other pure-Go engine and so the only one
measured under the same constraints.

goja is reached through a `goja-run` binary rather than linked in, which keeps
it out of this module's dependency graph. `gojarun/` is its own module for the
same reason:

```sh
go build -C bench/gojarun -o "$(go env GOPATH)/bin/goja-run" .
```

It presents the same command-line contract as `./goant`, so `goant-t262` can
drive it too — which is what makes the conformance comparison in the README an
apples-to-apples one rather than two numbers from two harnesses:

```sh
./goant-t262 -core -runner goja-run
```

## Octane 2.0

```sh
sh bench/suites/fetch.sh                 # clones the suite at a pinned commit
./goant-bench -suite octane -only splay -n 5
```

Octane is retired as a V8 optimisation target but is still the most portable
cross-engine score there is: pure JavaScript, no host APIs beyond `Date.now`.
Scores are higher-is-better, the opposite of the microbenchmarks above.
