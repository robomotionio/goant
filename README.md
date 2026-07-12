# goant

A pure-Go (`CGO_ENABLED=0`) port of [`ant`](https://github.com/theMackabu/ant)'s
"Silver" JavaScript engine — lexer → AST → bytecode compiler → 213-opcode
interpreter + MIR-based JIT — targeting ant's own conformance bar of
**1511/1511** (ES1–ES5, ES6, ES2016+, ESNext).

See [PLAN.md](PLAN.md) for the architecture and [TODO.md](TODO.md) for the
phase-by-phase checklist.

## Layout

| path | purpose |
|------|---------|
| `goant.go` | public embedding API |
| `cmd/goant` | CLI (`goant file.js`, `-e`, `--parse`, `--disasm`) |
| `cmd/goant-conf` | conformance harness (spawn-per-test, `--profile interp\|jit\|all`) |
| `internal/engine` | the engine: value repr, pools, object model, compiler, interpreter |
| `internal/regexpjs` | JS→regexp2 translation layer |
| `internal/runtime` | event loop, timers, microtasks, console |
| `internal/gomir` | MIR ported to Go (JIT IR + codegen) |
| `internal/jitmem` | JIT executable memory, trampolines, ExecContext |
| `tools/genops` | generates `internal/engine/opcode.go` from ant `opcode.h` |
| `tools/compatgen` | regenerates the compat-table corpus |
| `conformance/` | the 1511-name spec + generated/hand-written test corpus |

## Build

```sh
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
```
