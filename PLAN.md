# goant — Plan: Port of `ant` to Pure Go, target 1511/1511 conformance

Companion file: [TODO.md](TODO.md) — the complete phase-by-phase checkbox checklist executing this plan.

## Context

`/home/faik/projects/ant/` (github.com/theMackabu/ant) is a from-scratch JavaScript runtime in C (~180k lines): the "Ant Silver" engine (lexer → AST → bytecode compiler → 213-opcode interpreter + MIR-based JIT), generational GC, WTF-8 strings, PCRE2-backed regex, and a large Node-compat host layer. Goal: port it to **pure Go** (`CGO_ENABLED=0`) in this repo, hitting ant's own conformance bar: **1511/1511** on its kangax-compat-table-style suite (ES1–ES5, ES6, ES2016+, ESNext) as recorded in `ant/examples/results.txt`.

## Decisions (locked)

1. **JIT is mandatory.** Approach: **port MIR to Go** ("gomir"), then port ant's JIT (`swarm.c`) onto it. Pure-Go JIT precedent: wazero (pure-Go assembler → mmap'd executable pages → asm trampolines, zero cgo).
2. **Scope:** engine + minimal runtime (event loop, microtasks, timers, console, CLI). Host modules (fs/http/…) out of scope.
3. **Deps:** pure-Go third-party allowed (`golang.org/x/*`, `dlclark/regexp2`); `CGO_ENABLED=0` everywhere.
4. **Corpus:** regenerate 1091 compat-table cases from upstream kangax/compat-table (pinned commit) matched to `results.txt` names; hand-write the 420 custom es1/es3/es5 cases from their names.

## Key verified facts

- The 1511 test files are NOT in the ant repo — only `examples/results.txt` (the name list). Breakdown: es1/198, es3/148, es5/74, compat-table/1091 (es5:73, es6:700, es2016:14, es2017:64, es2018:22, es2019:24, es2020:19, es2021:15, es2022:40, es2023:10, es2024:13, es2025:29, intl:28, next:40).
- Corpus contains **no** Temporal, decorators, Array.fromAsync, ES modules/import, or worker/thread tests. intl/ needs only Collator, NumberFormat, DateTimeFormat (+ `toLocale*`). Atomics/SAB tests are single-agent.
- Values: 64-bit NaN-boxing (`include/internal.h`), 5-bit tag in bits 51–47, 47-bit payload = raw pointer. Errors are `T_ERR`-tagged values (`thrown_value` on the isolate) — the convention permeates everything.
- Interpreter: computed-goto over **213 opcodes** (`include/silver/opcode.h`, `OP_DEF(name, size, n_pop, n_push, fmt)`); opcode bodies in `src/silver/ops/*.h` (22 headers) are **shared between interpreter and JIT glue** (`glue.c` wraps them as `jit_helper_*`) — this pattern must survive the port.
- TCO is real, bytecode-level (`OP_TAIL_CALL`, frame reuse — `compiler.c:3778+`, `engine.c:1533+`). Generators suspend at bytecode level (own `sv_vm_t` each). Async = lazy VM materialization + minicoro fallback for native-stack awaits.
- GC: generational mark-sweep, nursery/old lists, write barriers, remember sets, pools/arenas (`src/gc/`).
- Ant's harness contract: spawn binary per test; pass = exit 0 within timeout (no error markers).
- Estimated Go surface: ~55–65k lines engine core + ~20–27k gomir/JIT + corpus/tooling.

## Architecture decisions

- **Value = `uint64` NaN-box, exact ant tag layout; heap payloads are 32-bit handles into chunked non-moving pools** (objects, closures, upvalues, strings, ropes, builders, symbols, bigints). Values are pointer-free from Go's perspective → JIT'd native code can hold/copy them freely, no Go write-barrier hazards, VM stack is `[]uint64`.
- **Port ant's GC** (don't lean on Go's): handles aren't traceable by Go anyway; WeakMap ephemerons + WeakRef/FinalizationRegistry timing need our own collector; GC runs only at allocation points in Go helper code where roots are fully enumerable (ant's model). Payload slices are ordinary Go allocations owned by pool cells; sweep nils them → Go GC reclaims bytes.
- **Generators/async:** port ant's bytecode-level suspension (primary); replace minicoro with goroutine handoff (unbuffered channels, single-runner discipline) for TLA/native-stack awaits only.
- **Strings:** keep WTF-8 flat + ropes + builders + cached UTF-16 scan cursors (port `utf8.c` verbatim) — avoids rewriting every string builtin.
- **Regex:** `internal/regexpjs` — port ant's JS→PCRE2 translation-layer architecture, retargeted to `dlclark/regexp2` (fork/vendor if gaps), with translate-time expansion of property escapes (UCD 17.0 tables) and v-flag set notation; anchored program variant for sticky; ported `RegExp.$1-$9`/lastMatch statics.
- **Single Go package** `internal/engine` (ant.c↔engine.c mutual recursion forbids splitting; goja precedent), files mirroring ant's layout 1:1.
- **Full ES2025 grammar in the parser from day one** — milestones gate semantics/builtins, never parser rewrites. Retain function source slices (Function.prototype.toString tests). Frame design accommodates TCO from day one.
- **JIT (verified against swarm.c/glue.c/MIR fork):** the MIR fork (`themackabu/mir` @ `cb71e1ee`, ~16 commits of codegen fixes over upstream, **no API additions**) is the porting reference. Port scope: mir core + mir-gen + amd64 + arm64 + mir-interp (~30k lines C → ~20–27k Go); c2mir/serialization/other targets excluded. swarm.c (9,179 lines) emits only ~35 MIR insn codes via ~70 `jit_helper_*` imports and 14 protos. Execution model: dedicated per-VM mmap'd JIT stack + guard page; one Go-asm entry trampoline per arch (NOSPLIT/NOFRAME, wazero pattern); **helper calls = exit/re-enter through the trampoline** (generated code never CALLs Go directly — morestack would die on unknown PC); import calls clobber all registers so the complete JIT state is always materialized in JIT stack + ExecContext when Go runs; back-edge **fuel checks** give Go's async preemption its safepoints. VM GC conservatively scans active JIT stack ranges + ExecContext (fixes ant's latent unrooted-JIT-registers hazard — `jit_active_depth` has no consumer in C ant). Non-moving storage + per-CompiledFunc keep-alive lists for pointers embedded as immediates; struct offsets used by JIT'd loads exported as Go constants from one source of truth.
- **JIT scheduling:** gomir starts **immediately in parallel** with the engine (zero dependency on value repr/opcodes; longest-lead item). goswarm starts once bytecode + JIT ABI are frozen and the interpreter is ≳90% of 1511. Tier ladder J0→J3; J0 can run on the gomir *interpreter* before native codegen lands, so goswarm is never blocked on G3.

## Execution order & effort

Three parallel tracks after Phase 0 (phases refer to TODO.md):

- **Track A (engine):** Phases 1→8 sequentially. Relative sizes: front end 10%, object model 12%, compiler+interp 20%, ES5 20%, ES6 20%, async+runtime 7%, GC 5%, long tail 13% of the ~55–65k-line engine core.
- **Track B (gomir):** Phase 9 starts immediately (zero engine dependencies, longest lead: G1 ~3–4 wk, G2 ~2–3 wk, G3 ~6–8 wk, G4 ~3–4 wk). Phase 10 after G2. Phase 11 (goswarm) gates on Track A ≳90% of 1511 + frozen JIT ABI; J0 can run on the G2 MIR interpreter before G3 native codegen lands.
- **Track C (conformance):** corpus tooling (TODO 0.3/0.4, ~1–2 wk incl. ~200–300 manual mapping curations) immediately; the 420 hand-written tests authored in batches aligned with Phases 4.2/4.3/4.4 (~2–3 wk total, doubling as TDD fixtures).

Primary milestone = interpreter 1511/1511 (end of Phase 8). Final milestone = JIT lane 1511/1511 with zero interp-vs-JIT differentials (end of Phase 11), then JIT defaults on.

Cumulative conformance targets per milestone: 198 (es1) → 346 (es3) → 493 (es5 + compat-table/es5) → ~707 (ES6 syntax) → ~971 (ES6 builtins) → 1193 (ES6 complete) → 1351 (ES2016–2021) → 1443 (ES2022–2025) → 1471 (intl) → **1511** (next/).

## Top risks (tracked)

1. `compiler.c` fidelity (6.6k dense lines) → bytecode-diff harness vs ant from day one
2. regexp2 semantic gaps (empty-match advance, v-flag, case folding under `u`) → translation layer owns validation; fork/vendor hedge; regex feature list is finite and enumerated per milestone
3. UTF-16-over-WTF-8 cursor off-by-ones → port `utf8.c` first, exhaustive matrix + fuzz
4. gomir G3 regalloc/codegen bugs → three-way differential (Go-interp/Go-native/C-goldens) + random-MIR fuzz + fork's fixes from day one
5. Go async preemption vs JIT code → fuel checks + watchdog tests (structural, testable)
6. Deopt state reconstruction (try-handler rebuild, upvalue rebase) → bailout-fuzz mode + `examples/jit/*.js` behavior parity
7. Microtask-drain-point drift → port each drain site explicitly; ordering suite in Phase 6
8. `numbers.cc` replacement edge cases (toFixed/toPrecision/radix) → ant-derived vectors
9. Handle ABA on weak refs → generation counters, no intra-cycle reuse
10. Single 60k-line package sprawl → strict file-per-ant-file mapping + internal conventions doc

## Final verification

```sh
CGO_ENABLED=0 go build -o goant ./cmd/goant        # also arm64 cross-build
go run ./cmd/goant-conf --runner ./goant --profile all --results out.txt
diff <(sort out.txt) <(sort conformance/ant-results.txt)   # must be empty, 1511 lines
```

- `--profile all` requires identical results from every execution tier (interpreter AND JIT) — zero cross-profile mismatches.
- Node-oracle corpus run green modulo the reviewed allowlist; CI ratchet at 1511; amd64+arm64 matrix green.
- Soak: full corpus ×N under `-race` with forced-GC stress flag.

## Reference files in the ant repo

- `ant/examples/results.txt` — the 1511-name spec (copied to `conformance/ant-results.txt`)
- `ant/include/internal.h` — NaN-box encoding, type tags, isolate state
- `ant/src/silver/engine.c` + `include/silver/engine.h` — dispatch loop, frames, unwind, TCO, suspension, JIT call site/tiering constants
- `ant/src/silver/compiler.c` — AST→bytecode (scoping/TDZ/IC/feedback tables)
- `ant/src/silver/ops/*.h` — the 22 shared opcode-body headers
- `ant/src/silver/swarm.c` / `glue.c` — JIT frontend + the ~70 `jit_helper_*` implementations
- `ant/src/ant.c` — object model, property protocol, Proxy, Promise, TypedArrays, core builtins
- `ant/src/gc/` — generational GC to port
- `ant/src/utf8.c` — UTF-16-over-WTF-8 cursor machinery
- `ant/tests/harness/harness.js` — pass-criterion semantics our runner mirrors
- `ant/vendor/mir.wrap` — pins `themackabu/mir` @ `cb71e1eefde4b8f01c05c94961c52c45dccbfc7e` (gomir porting reference)
- `ant/docs/exec-plans/active/silver-recursive-jit-perf.md` — JIT perf improvements to incorporate in J3
