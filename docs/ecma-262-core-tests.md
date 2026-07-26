# ECMA-262 core conformance

**42,605 / 42,607 — 99.995%**, measured by `./goant-t262 -core` against test262
`d1d583db` (2026-07-09) and verified on 2026-07-26.

```
CGO_ENABLED=0 go build -o goant ./cmd/goant
./goant-t262 -runner ./goant -core -j 4 -timeout 30s
```

`-j 4` produces occasional timeout flakes — usually the four
`language/literals/regexp/S7.8.5_A*_T2.js`. Re-run those with `-j 1 -timeout 60s`
before believing them.

## What `-core` measures

The ECMA-262 language and built-ins, and nothing else. It skips ECMA-402
(`intl402/`), the non-normative `staging/` directory, staged proposals, the
tests that need threads, and web-legacy `IsHTMLDDA`. 42,607 tests run;
10,799 skip.

| Skipped | Tests |
| --- | ---: |
| Temporal | 4,603 |
| intl402 (ECMA-402) | 3,341 |
| `staging/` (non-normative) | 1,482 |
| explicit-resource-management | 477 |
| Atomics + SharedArrayBuffer | 493 |
| source-phase-imports | 252 |
| import-defer | 251 |
| cross-realm | 203 |
| import-attributes | 101 |
| IsHTMLDDA | 42 |
| decorators | 27 |

## The two failures

Both are things no shipping JavaScript runtime supports either. Verified
directly against node 25.9 (V8), deno 2.9.3 (V8) and bun 1.3.14 (JSC) on
2026-07-26 rather than assumed.

### 1. `built-ins/Promise/allSettledKeyed/result-property-descriptors`

**A bug in the test. Not fixable by any engine.**

test262's `verifyProperty` checks configurability by deleting the property, and
without `{ restore: true }` it never puts it back:

```js
verifyProperty(result, "fulfilled", { /* … */ });   // result.fulfilled is now gone
verifyProperty(result.fulfilled, "status", { /* … */ });   // TypeError
```

Reproduced under node with test262's own harness — `o.fulfilled` reads back as
`undefined` after the first call. The test needs `{ restore: true }` on its
first two assertions.

Separately, the feature it covers (`await-dictionary`) is a proposal nobody has
shipped: `typeof Promise.allSettledKeyed` is `"undefined"` in node, deno **and**
bun. goant is the only one of the four that implements it at all.

### 2. `built-ins/Proxy/revocable/tco-fn-realm`

**Needs per-function `[[Realm]]`. Node and Deno fail every tail-call test.**

A function returned by `other.evalScript(…)` and then called from the main realm
must resolve intrinsics — here `%TypeError%` — in *its* realm, not the caller's.
goant models a realm as an entire `*Runtime`, so the agent-level state
(job queues, the call-depth counter, the new.target handoff) has to be separated
from the realm-level state before a frame can switch realms.

Sized, not started: ~95 references to the agent-level fields. The shape that
avoids a hot-path cost is to share the job queue and coroutine slot by pointer,
save and restore the per-call handoff fields only at the rare cross-realm
boundary, and put `realm` on `svFunc` so the dispatch is one predictable compare
on a value the call path has already loaded.

V8 has never implemented proper tail calls, so node and deno fail *all* of
test262's `tail-call-optimization` tests, not just this one:

```
$ node -e 'function f(n){return n===0?"ok":f(n-1)} console.log(f(500000))'
RangeError: Maximum call stack size exceeded
```

goant and bun both print `ok`. This single cross-realm variant is the only
tail-call test goant does not pass.

## How goant compares on the tests it recently fixed

Measured, one engine per column. `—` means the feature does not exist in that
engine; 💥 means it crashed.

| Test | goant | node | deno | bun |
| --- | :-: | :-: | :-: | :-: |
| `RegExp/nullable-quantifier` | ✅ | ✅ | ✅ | ✅ |
| `RegExp/lookahead-quantifier-match-groups` | ✅ | ✅ | ✅ | ✅ |
| `module-code/verify-dfs` | ✅ | ✅ | ✅ | ✅ |
| `top-level-await/pending-async-dep-from-cycle` | ✅ | ✅ | ✅ | ✅ |
| `top-level-await/fulfillment-order` | ✅ | ✅ | ✅ | ❌ |
| `top-level-await/rejection-order` | ✅ | ❌ | ✅ | ❌ |
| `dynamic-import/import-fulfilled-member-of-errored-cycle` | ✅ | 💥 | ❌ | ✅ |
| `Proxy/revocable/tco-fn-realm` | ❌ | ❌ | ❌ | ? |
| `Promise/allSettledKeyed/result-property-descriptors` | ❌ | — | — | — |

Two worth calling out:

- node **hard-crashes** on the errored-cycle test:
  `Fatal error … Check failed: module->status() == kLinked || kEvaluatingAsync || kEvaluated`.
- bun fails both top-level-await ordering tests with output byte-identical to
  goant's own failure before the module rewrite:
  `Actual [A, B] and expected [B, A]`.

## Outside the core profile

Not counted above, and the honest gap against a shipping runtime.

| Area | goant |
| --- | --- |
| **ECMA-402 (Intl)** | **202 / 1,231 = 16.4%** (Temporal-tagged tests excluded). `internal/engine/builtin_intl.go` is 140 lines: `Collator`, `NumberFormat` and `DateTimeFormat` exist as constructors that validate their locale argument and then **ignore both locale and options**. `Intl.Locale`, `PluralRules`, `ListFormat`, `RelativeTimeFormat`, `DisplayNames`, `Segmenter`, `DurationFormat`, `getCanonicalLocales` and `supportedValuesOf` are absent. |
| **Temporal** | Not implemented (4,603 tests). Shipped by bun and deno; behind a flag in node 25. |
| **Threads** | No `Atomics` / `SharedArrayBuffer` (493 tests). |
| **Staged proposals** | decorators, source-phase-imports, import-defer, import-bytes, import-text. |
