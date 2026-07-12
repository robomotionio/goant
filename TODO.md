# goant — Master TODO

Port of the `ant` JavaScript runtime (C) to pure Go (`CGO_ENABLED=0`), including the JIT. Target: **1511/1511** conformance (ES1–ES5, ES6, ES2016+, ESNext), matching `ant/examples/results.txt`.

Design rationale, architecture decisions, risks, and verification story: see [PLAN.md](PLAN.md).

Tracks run in parallel after Phase 0: **A** = engine (Phases 1–8), **B** = JIT (Phases 9–11, gomir starts immediately), **C** = conformance (0.3/0.4 + hand-written test batches).

---

## Phase 0 — Scaffolding & tooling

### 0.1 Repo bring-up
- [ ] `git init`; commit `PLAN.md` + `TODO.md`
- [ ] `go mod init goant` (Go ≥ 1.24); deps: `dlclark/regexp2`, `golang.org/x/text`, `golang.org/x/sys`
- [ ] Layout: `goant.go` (embed API), `cmd/goant/` (CLI), `cmd/goant-conf/` (conformance runner), `internal/engine/`, `internal/regexpjs/`, `internal/runtime/`, `internal/gomir/` (+ `gen/`, `gen/amd64`, `gen/arm64`, `interp/`, `mirtext/`), `internal/jitmem/`, `tools/genops/`, `tools/compatgen/`, `conformance/`
- [ ] CI workflow: build (`CGO_ENABLED=0` enforced), `go vet`, `go test`, ubuntu amd64 + arm64 matrix
- [ ] `tools/genops`: parse ant `include/silver/opcode.h` `OP_DEF(name, size, n_pop, n_push, fmt)` → generate `internal/engine/opcode.go` (all 213 opcodes: constants, sizes, stack effects, operand formats); commit generated file

### 0.2 Value & pools foundation
- [ ] `value.go`: `type Value uint64`; ant's exact NaN-box layout (`0xFFF0…` prefix, tag bits 51–47, 47-bit payload); `mkval/Type()/Data()`, double passthrough + NaN canonicalization; all ~19 tags (`T_OBJ/T_STR/T_ARR/T_FUNC/T_CFUNC/T_PROMISE/T_GENERATOR/T_UNDEF/T_NULL/T_BOOL/T_NUM/T_BIGINT/T_SYMBOL/T_ERR/T_TYPEDARRAY/…`)
- [ ] Chunked non-moving handle pools (32-bit handle → `&chunks[h>>shift][h&mask]`), power-of-two chunks, free lists, generation counters (weak refs / ABA safety): pools for objects, closures, upvalues, flat strings, ropes, builders, symbols, bigints
- [ ] String-payload low-2-bit flat/rope/builder tag preserved inside Value payload
- [ ] Error-as-value convention: `T_ERR` values + `thrownValue/thrownExists` on Runtime
- [ ] Unit tests: NaN-box round-trips for every tag, doubles (±0, NaN, Inf, subnormals), pool alloc/free/deref/generation

### 0.3 Conformance corpus tooling (Track C — parallel with engine work)
- [ ] Copy `ant/examples/results.txt` → `conformance/ant-results.txt` verbatim (the 1511-name spec / diff target)
- [ ] `tools/compatgen/extract.mjs` (node-only regeneration step): load pinned compat-table `data-es5/es6/es2016plus/esnext/esintl.js`, emit raw subtests JSON `{dataFile, category, feature, subtest, execSource, isAsync}`; handle both exec styles (data-es5 `fn.toString()`, data-es6+ `function(){/* code */}` comment extraction); `isAsync` = references `asyncTestPassed`
- [ ] Pin compat-table commit (`tools/compatgen/COMPAT_TABLE_COMMIT`) — must be ≥ Oct 2025 (contains `unicode-17.0` subtests and ES2025 categorization); MIT attribution in generated headers
- [ ] `tools/compatgen/main.go`: name mapping in 3 layers (`feature-aliases.json` ~100 curated feature→prefix entries; mechanical subtest normalization; `mapping.json` per-test overrides + `"handwritten": true` escape hatch). **Hard-fail** on: unmapped name, missing subtest at pin, duplicate mapping. Search ALL data files regardless of ant's category dir (upstream recategorizes)
- [ ] Wrapper templates — sync: IIFE around verbatim exec body, `!== true` → `process.exit(1)`, print `PASS`; async: 5s `setTimeout` fail-timer + `asyncTestPassed` callback → `PASS`/exit 0. Never hoist body to top level (preserves inner `'use strict'`/`return`/scoping); wrapper top level stays sloppy
- [ ] Helpers prelude (inject only when referenced): `global` shim; `__createIterableObject` vendored verbatim from pinned checkout; hard-fail on any other unresolved `__*` free identifier or `window`/`document`/`Worker` references
- [ ] Generate + commit `conformance/compat-table/<cat>/*.js` (1091 files, hermetic — no network/node needed to run)
- [ ] Fidelity: name-set equality vs `ant-results.txt`; **node-25 oracle run** over full corpus with committed `oracle-node.allow` (~25 expected: tail-calls ×2, throw-expr ×4, AsyncIterator ×17, isTemplateObject, upsert ×2, possibly unicode-17.0 rows) — every allowlisted test hand-reviewed against proposal/spec text
- [ ] Optional strongest oracle: build ant once (`meson setup build && ninja -C build`) and run corpus via `--runner ant` expecting exactly 1511 OK (validates pass-criterion equivalence)

### 0.4 Conformance harness (`cmd/goant-conf`)
- [ ] Spawn-per-test runner (mirrors ant's `tests/harness/harness.js` contract): pass = exit 0 within timeout (default 10s, `// goant-timeout:` override); worker pool `-j`; deterministic ordering
- [ ] Flags: `--runner <path|node>`, `--profile interp|jit|all` (all = cross-profile mismatch = failure — the differential gate), `--only`, `--category`, `--timeout`, `--results out.txt` (ant section order), `--diff ant-results.txt` (order-insensitive), `--allow`
- [ ] Ratchet: `conformance/expected.txt` + `--check-ratchet` (CI fails on regression OR unrecorded new pass); per-category X/Y summary table every run (visible N/1511 in every CI log)
- [ ] CI job 2: regenerate corpus at pin + `git diff --exit-code` (hermeticity guard); node-oracle run with allowlist

## Phase 1 — Front end (full ES2025 grammar, day one)

- [ ] Port `src/silver/lexer.c` (~1.2k lines) 1:1: byte tokenizer, all tokens, template-literal modes, regex-literal disambiguation, ASI data, hashbang
- [ ] Port `src/silver/ast.c` (~2.3k lines): arena → per-parse slab (`[]Node`); all node kinds; **retain source-text slices per function** (Function.prototype.toString es2019 tests)
- [ ] Port `src/silver/directives.c`, `src/silver/limits.c`
- [ ] Full grammar coverage now (class fields/static blocks/private, optional chaining, using/await-using, throw expressions, numeric separators, etc.) — later phases never touch grammar
- [ ] `goant --parse` mode; gate: parses ant's `examples/spec/*.js` (97 files) + generated corpus without error

## Phase 2 — Object model core

- [ ] `object.go` from `include/object.h`: shape ptr (Go-GC-managed — never inside Values) + `inobj[N]Value` inline slots + overflow `[]Value`; unions → explicit fields (arr `[]Value` / closure handle / boxed Value / `*Sidecar` for proxy state, private tables, promise state, native tags); GC list link as handle; markEpoch
- [ ] Port `src/shapes.c` (~540 lines): transition trees, per-shape slot index (uthash → map), IC epoch counter
- [ ] Port `src/descriptors.c`, `src/errors.c`
- [ ] Flat strings (WTF-8, `is_ascii` fast path) + intern table (`map[string]uint32`) + pre-interned hot names
- [ ] Port `src/utf8.c` **completely, early**: `utf16_strlen`, `utf16_code_unit_at`, index↔byte-offset, cursor cache resume/store; exhaustive test matrix (ASCII/BMP/astral/lone-surrogate × every helper) + fuzz vs reference
- [ ] Coercions: ToPrimitive/ToNumber/ToString/ToPropertyKey/ToObject/SameValue(Zero)… (ant.c sections)
- [ ] Property protocol: get/set/delete/define/has, proto chains, accessors, array fast path
- [ ] Gate: Go unit tests mirroring ant for shapes/properties/coercions

## Phase 3 — Compiler + interpreter core

- [ ] Port `src/silver/compiler.c` (~6.6k lines, highest-fidelity-risk) section by section: scopes, TDZ, closures/upvalues, private names, IC/object-site/**type-feedback tables** (structures ported now, recording stubbed behind funcs — JIT consumes later)
- [ ] Port `src/silver/compile_ctx.c`
- [ ] Port disassembler → `goant --disasm`; **bytecode-diff harness**: goant vs ant bytecode over a corpus; every divergence reviewed, not assumed benign
- [ ] `vm.go`: `VM`/`Frame` (from `sv_vm_t`/`sv_frame_t`), handler stack, unwind (engine.c non-dispatch parts)
- [ ] `interp.go`: `for{switch}` dispatch (Go compiles dense switch → jump table)
- [ ] Port all 22 `ops/*.h` → `ops_*.go` 1:1, exported funcs, signatures preserved — **the interpreter/JIT-shared layer**: arithmetic, bitwise, calls, coercion, comparison, control, exceptions, globals, iteration, literals, locals, objects, optional, private, property, returns, stack, super, unary, upvalues, using, async
- [ ] TCO: `OP_TAIL_CALL`/`OP_TAIL_CALL_METHOD` + `tail_call_inline` frame reuse (close upvalues, memmove args, reset ip; guards: no async/generator/bound/super, empty handler stack); test 1e6-deep tail recursion, O(1) frames
- [ ] `maxFrames` (65536) RangeError; `vm_exec_depth` guard for Go-native re-entrancy (builtin→JS callback)
- [ ] Upvalue representation decision honoring JIT deopt: ant's bailout rebases raw open-upvalue location pointers into the new frame (`engine.c:846-858`) — either keep pointer+rebase (interior pointers into VM-owned frames) or switch to (frame, slot) handles; document the choice in the JIT ABI
- [ ] **Freeze + document the JIT ABI** (`docs/jit-abi.md`): ops calling convention (op bodies MUST stay standalone exported funcs — JIT helper table wraps them), VM stack = `[]uint64` layout, frame layout, pool deref, struct offsets consumed by JIT'd loads (object header/shape/IC entry/VM fields) exported as constants, root-visibility rules, where GC may run
- [ ] Gate: closures/recursion/exceptions/deep-tail-recursion correct; fib/ackermann; bytecode diffs clean

## Phase 4 — ES1/ES3/ES5 surface → **493/1511**

### 4.1 Hand-written test batches (Track C; author alongside; double as TDD fixtures)
- [ ] `conformance/es1/*.js` — 198 tests from names: operators/conversions/statements (~85), Math (26), Date (40, timezone-robust construction), String (17), Array (11 incl. `.generic` via `.call` on array-likes), Boolean/Number/Object/Function (~14), annex-b (7: getYear/setYear/toGMTString/escape/unescape/octal), literals (6); each: standalone, inline `assert`, `PASS` print, spec-clause header comment
- [ ] `conformance/es3/*.js` — 148: regex (41), Error hierarchy (16), String methods (20), Array mutators (15), Number formatting (7: toFixed/toExponential/toPrecision edges), literals (13), URI codecs (4), labels/switch/do-while/try (7), function expressions (3), instanceof/in/strict-equals (3), source/identifiers (4), misc
- [ ] `conformance/es5/*.js` — 74: strict.* (21), Object meta-ops (14), JSON (6), Array extras (12), bind/accessors/misc (21)

### 4.2 ES1-era semantics & builtins (gate: es1/ 198 → cumulative 198)
- [ ] `builtin_object.go`, `builtin_function.go`, `builtin_array.go` (+generics), `builtin_string.go`, `builtin_number.go`, `builtin_boolean.go`, `builtin_math.go`, `builtin_date.go` (full get/set/UTC matrix, parsing, local↔UTC), `builtin_globals.go` (parseInt/parseFloat/isNaN/isFinite/eval hook/escape/unescape)
- [ ] `with`, basic `eval` (direct/indirect distinction correct from start), `arguments`, ASI, for-in semantics, typeof/void/delete/comma/conditional
- [ ] `numbers.go`: number↔string (shortest repr via strconv; JS-exact `toString(2..36)`)

### 4.3 ES3 (gate: es3/ 148 → 346)
- [ ] Error hierarchy (all NativeErrors, message/name/toString, thrown types), try/catch/finally, switch, do-while, labelled statements
- [ ] **regexpjs v1**: JS regex parser/validator; translate to regexp2 (`ECMAScript|Unicode` opts); classes/quantifiers/lookahead/backrefs/flags g-i-m; lastIndex/sticky via anchored second program; compiled-program cache (pattern,flags); empty-match advancement semantics verified
- [ ] `builtin_regexp.go` + String↔RegExp methods (match/replace/split/search incl. `.generic`) + `update_regexp_statics` port (`RegExp.$1–$9`, lastMatch, contexts)
- [ ] `toFixed/toExponential/toPrecision` spec-exact with ant-derived vectors; URI codecs (encodeURI[Component]/decode…)
- [ ] instanceof/in, unicode identifiers, string literal edges
- [ ] Ropes + builders port (`gc/ropes.c` + builder cells) incl. materialization call sites

### 4.4 ES5 + compat-table/es5 (gate: es5/ 74 + compat-table/es5 73 → **493**)
- [ ] Strict mode end-to-end (compiler flags + runtime): the 21 `es5/strict.*` names enumerate exact requirements (no-octal, no-with, arguments/eval bindings, unmapped arguments, this-undefined, delete restrictions, dup params, caller/arguments poison, reserved words)
- [ ] Object meta: defineProperty/ies, getOwnPropertyDescriptor/Names, create, seal/freeze/preventExtensions + checks, getPrototypeOf
- [ ] JSON.parse/stringify (reviver/replacer/space/toJSON), Function.prototype.bind, accessors in literals, Array extras (forEach/map/filter/reduce/…), String.trim, Date.now/toISOString, immutable undefined/NaN/Infinity
- [ ] `Function` constructor + full eval semantics

## Phase 5 — ES6 → **1193/1511**

### 5.1 Syntax & bindings (~214 tests → ~707)
- [ ] let/const/TDZ (incl. loop-per-iteration bindings), destructuring ×3 (declarations/assignment/parameters, defaults, nested, iterator-protocol-driven), default/rest params, spread calls/literals
- [ ] Arrow functions (lexical this/arguments/new.target), classes v1 (methods/accessors/static/extends/super basics), `new.target`, shorthand/computed properties, template literals (tagged, raw, HTML-comment rules), Unicode code-point escapes, octal/binary literals
- [ ] for-of + iterator protocol core, minimal Symbol wired to internals
- [ ] Generators: bytecode suspension port (own VM per generator; `OP_YIELD`/`yield*` family; resume kinds NEXT/THROW/RETURN; return/throw semantics; generators-in-parameters edge cases)

### 5.2 Builtins (~264 → ~971)
- [ ] Symbol complete + all well-known symbols honored engine-wide (hasInstance, isConcatSpreadable, toPrimitive, toStringTag, unscopables…)
- [ ] Map/Set/WeakMap/WeakSet (port `modules/collections.c`; weak entries keyed handle+generation)
- [ ] Promise + microtask queue with ant's exact drain points; combinators (all/race), species
- [ ] Array statics (from/of) + iterator methods everywhere (keys/values/entries + String/Map/Set/arguments iterators)
- [ ] Object.assign/is/setPrototypeOf, String.raw/fromCodePoint/codePointAt/repeat/includes/starts/ends, Number.isInteger/isSafeInteger/EPSILON/…, Math.* (clz32/fround/imul/sign/trunc/cbrt/expm1/log1p/log2/log10/hypot/…)
- [ ] Function.name (all inference paths), RegExp y/u flags + `flags` + `Symbol.match/replace/search/split` protocol
- [ ] `new-function` subtest variants (every builtin constructible/callable per spec)

### 5.3 Meta & exotic (~222 → **1193**)
- [ ] Proxy: all 13 traps + invariants + `misc.Proxy.*` internal-call-site tests (~61 — every internal method call site routed correctly: Array.from, JSON, spread, etc.)
- [ ] Reflect complete (incl. Reflect.construct newTarget)
- [ ] Subclassing (31 tests): Array/RegExp/Promise/TypedArray/Map/Set/Error subclass semantics, `Symbol.species` everywhere
- [ ] TypedArrays (all) + ArrayBuffer + DataView: `[]byte` backing, ToIndex/offset/detach semantics, species, iteration; property order; own-property enumeration order spec-exact
- [ ] Annex-B group (28): `__proto__`, `__define/lookupGetter__`, HTML comments, RegExp compile, escape/unescape interplay, function-in-block semantics
- [ ] **TCO conformance green** (tail-calls.direct/mutual — no node oracle; verify vs spec/ant)
- [ ] Gate: **es6 700/700**

## Phase 6 — Async + minimal runtime

- [ ] async/await: lazy-start + lazy VM materialization port (`sv_async_prepare_materialization`, `SV_AWAIT_SUSPENDED` unwind); goroutine-handoff coroutine (unbuffered channels, strict single-runner) replacing minicoro for TLA/native-stack awaits; `promise_handler_t.await_coro` lifecycle
- [ ] Async generators + for-await-of; async arrow/method forms
- [ ] `internal/runtime`: single-threaded loop — drain microtasks → pop min-heap timer → advance/sleep → fire; exit when no timers + no pending coroutines; `setTimeout/setInterval/clear*`, `queueMicrotask`
- [ ] `console.*` (port needed subset of `modules/io.c` formatting), `process.exit`/argv/env subset for harness
- [ ] Unhandled-rejection tracking → nonzero exit (harness correctness)
- [ ] CLI final form: `goant file.js`, `-e`, `--parse/--disasm`, JIT flags reserved
- [ ] Gate: es2017 async rows pass; microtask-vs-timer ordering matches ant/node

## Phase 7 — GC completion & weak semantics

- [ ] Port `gc/gc.c` (adaptive thresholds, epochs), `gc/objects.c` (mark stack; promise-handler + coroutine + upvalue marking), `gc/roots.c` (root scopes for native code), `gc/refs.c`, `gc/strings.c`, `gc/ropes.c` (pool sweeps)
- [ ] Nursery/old handle-linked lists; write barriers + remember sets (objects/upvalues/func-consts) 1:1
- [ ] Sweep nils payload slices (hybrid ownership) → Go GC reclaims; heap-watermark assertions in CI soak
- [ ] Ephemeron WeakMap iteration in major mark; WeakRef deref via handle generation; FinalizationRegistry queue drained as jobs
- [ ] Soak/stress suites under `-race`; allocation-heavy corpus runs; zero leaks
- [ ] Gate: weak rows green; soak clean

## Phase 8 — ES2016+…ESNext long tail → **1511/1511 (interpreter)**

### 8.1 ES2016–2021 (158 → 1351)
- [ ] `**`, Array.prototype.includes
- [ ] Object.entries/values/getOwnPropertyDescriptors, padStart/End, trailing commas
- [ ] Atomics + SharedArrayBuffer single-agent semantics (12+5 tests; `[[CanBlock]]` behavior per extracted test bodies)
- [ ] Object rest/spread; async iteration protocol; **regexpjs v2**: lookbehind, named groups (+`groups`, `$<name>` replace), `s` flag, `\p{…}` property escapes via **UCD 17.0-generated tables** (`internal/regexpjs/tables_gen.go` — stdlib `unicode` lags)
- [ ] flat/flatMap, Object.fromEntries, trimStart/End, optional catch binding, Symbol.description, JSON superset + well-formed stringify, **Function.prototype.toString source-exact** (7 tests)
- [ ] BigInt via `math/big` (+ BigInt64/Uint64Array, DataView methods), globalThis, optional chaining, nullish, matchAll, Promise.allSettled, `import.meta` (syntax only if tested)
- [ ] Logical assignment, numeric separators, Promise.any + AggregateError, replaceAll, WeakRef/FinalizationRegistry rows

### 8.2 ES2022–2025 (92 → 1443)
- [ ] Class fields (public/private/static), private methods/accessors, `#x in obj` brand checks, static blocks
- [ ] Error `cause`, `at()`, Object.hasOwn, `d` regex flag (indices incl. named groups)
- [ ] findLast/findLastIndex, change-array-by-copy (toReversed/toSorted/toSpliced/with), hashbang, symbols-as-WeakMap-keys
- [ ] Resizable/growable ArrayBuffer, transfer/detached checks, Atomics.waitAsync (per test depth)
- [ ] **regexpjs v3**: `v` flag (set notation `[[a-z]--[aeiou]]`, `\q{…}`, properties-of-strings → translate-time desugar), pattern modifiers `(?i:…)`, duplicate named groups
- [ ] Object/Map.groupBy, Promise.withResolvers, Iterator helpers (map/filter/take/drop/flatMap/reduce/toArray/forEach/some/every/find + Iterator.from), Set methods (union/intersection/difference/symmetricDifference/isSubsetOf/isSupersetOf/isDisjointFrom), Promise.try, RegExp.escape, Float16Array + Math.f16round
- [ ] Property-enumeration-order and other misc rows in these categories

### 8.3 Intl-lite (28 → 1471)
- [ ] Port ant's `modules/intl.c` (~570 lines) surface exactly: `Intl` object, Collator, NumberFormat, DateTimeFormat (+resolvedOptions/supportedLocalesOf), `String.prototype.localeCompare`, `toLocaleString` family wiring
- [ ] Back with `x/text/language` (BCP47 parse — verify rejects-invalid-tags strictness; small own validator if x/text too lenient), `x/text/collate`, stdlib `time` + `time/tzdata` embed
- [ ] Gate: intl/ 28/28

### 8.4 next/ (40 → **1511**)
- [ ] Explicit resource management: `using`/`await using` (+for-of forms), DisposableStack, AsyncDisposableStack, SuppressedError, Symbol.dispose bound-check (port ant.c DisposableStack sections + `ops/using.h`)
- [ ] AsyncIterator helpers (17: extends/from ×3/instanceof/proto methods ×11/toStringTag) — large async machinery, budget accordingly; no node oracle
- [ ] Uint8Array fromBase64/toBase64/fromHex/toHex/setFromBase64/setFromHex (6)
- [ ] Map/WeakMap `upsert` — implement exactly what the pinned test bodies exercise (proposal renamed getOrInsert)
- [ ] Throw expressions (4) — parser already supports; verify vs stage-proposal text (shipped nowhere; no oracle)
- [ ] RegExp legacy statics rows (2), Array.isTemplateObject (1)
- [ ] 🎯 Gate: `goant-conf --runner ./goant --profile interp` → **1511/1511**, `--diff` vs ant-results.txt empty

## Phase 9 — gomir: MIR ported to Go *(Track B — starts alongside Phase 0; no engine dependencies)*

Porting reference: fetch `github.com/themackabu/mir` @ `cb71e1eefde4b8f01c05c94961c52c45dccbfc7e` (ant's pinned fork — carries upstream codegen fixes #410/#423/#424, aarch64 LR/long-jump fixes, vararg fixes; API-identical to upstream MIR 1.x) into a scratch checkout, never vendored. Build the C fork once to produce **golden testdata** (module dumps, execution results) committed to goant — CI then needs no C toolchain.

### 9.1 G1 — IR core + builder + text I/O (`internal/gomir`)
- [ ] Port `mir.c`/`mir.h` data structures: context, module, item (func/proto/import/export/data), func regs, insns, operands, labels; ADT headers (dlist/varr/bitmap/htab) → Go slices/maps/bitset
- [ ] Builder API 1:1 with C names preserved (`NewFunc`, `NewProto`, `NewImport`, `NewInsn`, `NewCallInsn`, `NewRetInsn`, `AppendInsn`, `NewReg/Label/Int/Uint/Double/Mem/RefOp`, `NewLabel`, `Reg`, `FinishFunc`, `FinishModule`…) so swarm.c ports mechanically
- [ ] `FinishFunc` verification; text writer (`MIR_output_module`) **and** text reader (unlocks MIR's own `mir-tests/*.mir` as test inputs)
- [ ] Gate: reconstructed mir-tests modules byte-comparable (modulo whitespace) with C MIR output; read→write round-trip is a fixed point

### 9.2 G2 — MIR interpreter (`internal/gomir/interp`)
- [ ] Port `mir-interp.c` minus libffi: external calls dispatch through a typed Go helper table (simpler than C)
- [ ] Gate: mir-tests suite (API-built + .mir-file tests, non-c2mir) passes; differential vs C-built golden results

### 9.3 G3 — mir-gen + amd64 backend (`internal/gomir/gen`, `gen/amd64`) — **the long pole (~10k dense lines)**
- [ ] Port `mir-gen.c`: simplify, CFG/SSA build, GVN/CSE, copy-prop, DCE, loop analysis + LICM, live-range priority register allocator, combine/peephole, target hook interface; optimize levels (swarm uses 3)
- [ ] Port `mir-gen-x86_64.c`: insn selection, lowering, code emission to byte buffer, label/relocation resolution
- [ ] Replace `mir-x86_64.c` FFI/closure thunks with our trampoline design (Phase 10); `MIR_load_external` binds import names → helper-table indices, not raw pointers
- [ ] Incremental bring-up allowed: hard error on unimplemented insn codes (swarm emits ~35: MOV/DMOV, ADD/SUB/AND/OR/LSH/URSH, JMP/BT/BF/BEQ/BNE/UBGT/UBGE/UBLE, DADD/DSUB/DMUL/DDIV/DNEG, DEQ/DNE/DLT/DLE/DGT/DGE, I2D/UI2D/D2I, ALLOCA, CALL, RET) — but faithful port eventually covers the full set (gen's lowering generates more internally)
- [ ] Gates: (a) mir-tests compiled-native green on linux/amd64; (b) **three-way differential fuzzing** — random valid MIR functions run under Go-interp vs Go-native vs C-MIR goldens; (c) race-detector clean at `-count=100`

### 9.4 G4 — arm64 backend (`gen/arm64`)
- [ ] Port `mir-gen-aarch64.c` incl. fork fixes (R30/LR fixed reg, indirect long jumps)
- [ ] Gates: same test matrix on linux/arm64 + darwin/arm64

## Phase 10 — JIT memory, trampolines, ExecContext (`internal/jitmem`)

- [ ] Exec pages: `unix.Mmap(ANON|PRIVATE, RW)` → copy code → `Mprotect(RX)`; **never write to an RX page** (re-flip RW for patching); RW→RX PTE transition handles icache coherency on linux/arm64 + darwin/arm64 (wazero precedent); reserve a Go-asm `IC IVAU/DSB ISH/ISB` routine behind a build tag as fallback
- [ ] Per-VM code arena, freed only at VM teardown (deopt clears `fn.jitCode`, never unmaps — frames may still execute; matches ant)
- [ ] Dedicated per-VM mmap'd JIT stack + guard page; prologue stack-limit check (replaces `jit_helper_stack_overflow` address arithmetic)
- [ ] Entry trampoline per arch (Go asm, `NOSPLIT|NOFRAME`, ABI0): save Go SP/BP into ExecContext → switch to JIT stack → jump; saved SP consumed only within the same invocation (goroutine stack moves can't invalidate it — not at a safepoint inside JIT code)
- [ ] **Exit/re-enter helper protocol**: import-call lowering spills (v1: clobber-all — nothing live in registers across helper calls), stores helper index + args + continuation PC in ExecContext, restores Go SP, returns to trampoline; Go dispatch loop calls the helper (plain Go — full GC/preemption/stack-growth freedom, may recursively re-enter interpreter/JIT with nested JIT-stack watermark); re-entry reloads JIT SP, places return value, JMPs to continuation
- [ ] **Back-edge fuel checks** (decrement counter in ExecContext; zero → exit `SAFEPOINT` + immediate re-enter) at the same back-edges swarm flags (`SV_OPF_JIT_OSR_BACKEDGE`) — solves Go async-preemption (SIGURG can't land in non-Go PC; STW would stall otherwise)
- [ ] VM-GC contract: for each active JIT activation, conservatively scan JIT stack `[currentSP, entrySP)` + ExecContext slots as roots (validate candidate words via pool-membership test; over-retention OK); precise gomir stack maps = v2 option
- [ ] Watchdog tests: JS `while(true){}` under `runtime.GC()` pressure must not wedge; stack-stress with deep Go recursion around JIT entries; `GOANT_GC_STRESS=1` (GC-every-allocation) across corpus with JIT forced

## Phase 11 — goswarm: port of swarm.c *(gate to start: bytecode + JIT ABI frozen, interpreter ≳90% of 1511)*

### 11.1 J0 — call-threaded JIT (minimal honest JIT)
- [ ] Compile driver: eligibility scan (`jit_is_eligible` — rejects async/generators/non-eligible ops), whole-function compile at `call_count > 100` (`SV_JIT_THRESHOLD`), one MIR module per function, ~70 imports + 14 protos declared upfront
- [ ] Virtual-stack machinery port (bytecode slots → MIR regs; branch-target label map with sp reconciliation) — dual-representation lattice fields present but unused in J0
- [ ] Every opcode lowered to full-semantics helper call wrapping the **shared ops funcs** (never bail); constants/local moves/jumps/dup/pop inline
- [ ] Runs on gomir **interpreter** (G2) first; switches to native codegen when G3 lands
- [ ] Gate: full corpus green with `GOANT_JIT=always` (threshold=1); **differential interp-vs-JIT corpus run** wired into CI (nightly from J0 onward)

### 11.2 J1 — type feedback + bailout/deopt
- [ ] Type-feedback recording live in interpreter (un-stub Phase 3 hooks): per-bc-offset tfb bytes (`SV_TFB_NUM/STR/BOOL/OTHER`), per-local numeric hints, feedback alloc at call 2
- [ ] Dual-register numeric unboxing (boxed I64 + unboxed D per slot, `slot_type`/`known_const` lattice); tfb-driven arithmetic/comparison specialization (`DADD` vs guarded-unbox vs helper+bailout, per swarm.c:3928+)
- [ ] Bailout: `SV_JIT_BAILOUT` NaN-sentinel returns from partial-semantics helpers → per-function bailout trampoline (spill vstack with lazy re-boxing, locals→lbuf, record bc_offset/sp) → `jit_helper_bailout_resume`
- [ ] Interpreter-side resume port (`engine.c:831-917`): restore params/locals/vstack, **rescan bytecode to rebuild try-handler stack**, rebase open upvalues
- [ ] Bailout bookkeeping: recompile-after-delay, disable after 5 bailouts per tfb version (`engine.h:949-994`)
- [ ] `GOANT_JIT_BAILOUT_FUZZ` mode: force every Nth fast path to bail; diff final output vs interpreter-only across corpus
- [ ] Gate: corpus green JIT-forced; `examples/jit/{bailout,type_feedback,exceptions}.js` behaviors match C ant

### 11.3 J2 — OSR
- [ ] Back-edge counters (`back_edge_count >= 500`, `JIT_OSR_BACK_EDGE`), `sv_jit_try_osr`, OSR prologue dispatch on `vm.jit_osr.bc_offset` → back-edge labels, locals import from interpreter frame (swarm.c:3209-3345); fuel checks share these back edges
- [ ] Gate: `examples/jit/osr.js` semantics; hot loops enter JIT without function re-entry

### 11.4 J3 — ICs, speculative calls, inlining
- [ ] Shape-guard get/put_field fast paths reading `sv_ic_entry_t` + global IC epoch invalidation (swarm.c:911+, 3082)
- [ ] Call-target feedback, speculative direct/self calls (`mir_emit_self_tail`), bytecode-level inlining of small monomorphic callees (`jit_emit_inline_body`)
- [ ] Incorporate ant's active perf plan improvements (`docs/exec-plans/active/silver-recursive-jit-perf.md`: tighter args_buf sizing, lightweight self-call path, cheaper stack-depth checks) instead of replicating known-slow choices
- [ ] Gate: fib(40)-class benchmarks within agreed factor of C ant JIT; `examples/jit/{stress,ops,closures,objects}.js` match; **JIT lane reaches 1511/1511 → JIT defaults ON**

### 11.5 Platform rollout (build tags `//go:build goant_jit && (amd64 || arm64)`; graceful interpreter-only degradation elsewhere)
- [ ] linux/amd64 (primary CI) → linux/arm64 → darwin/arm64 (plain mprotect flip; `MAP_JIT`/hardened runtime deferred) → darwin/amd64 → windows/amd64 (`x/sys/windows` VirtualAlloc/VirtualProtect; SysV conventions kept inside JIT code — it only ever calls our exit stub, so no win64 ABI work in gomir)

## Final verification

- [ ] `CGO_ENABLED=0 go build -o goant ./cmd/goant` (also arm64 cross-build)
- [ ] `go run ./cmd/goant-conf --runner ./goant --profile all --results out.txt` — **all profiles (interp AND jit) 1511/1511, zero cross-profile mismatches**
- [ ] `diff <(sort out.txt) <(sort conformance/ant-results.txt)` → empty
- [ ] Node-oracle corpus run green modulo reviewed allowlist; CI ratchet at 1511; amd64+arm64 matrix green
- [ ] Soak: full corpus ×N under `-race` with forced-GC stress flag
