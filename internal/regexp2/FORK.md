# internal/regexp2 — a fork of dlclark/regexp2

Vendored from `github.com/dlclark/regexp2` **v1.12.0** (MIT; see `LICENSE` and
`ATTRIB`). Import paths were rewritten to `goant/internal/regexp2`; the upstream
`go.mod`, tests and `testoutput1` fixture were dropped.

## Why it is a fork and not a dependency

regexp2 is a port of .NET's regex engine, and .NET and ECMAScript disagree about
what an *optional iteration that matched the empty string* means:

- **.NET**: the loop ends, and the iteration is kept.
- **ECMAScript** (RepeatMatcher, 22.2.2.3.1 step 2.b): the iteration is
  DISCARDED and the engine backtracks looking for a longer one.

That is the difference between `/(a?b??)*/` matching `"a"` and matching `"ab"`
on the input `"ab"`, and it is the loop's control flow rather than its shape —
there is no way to express it by rewriting the pattern, which is how
`internal/regexpjs` handles every other divergence.

## The change

One new opcode, `Branchmarkjs`: `Branchmark` with the ECMAScript rule. The
writer emits it in place of `Branchmark` for a greedy unbounded loop with no
required iterations (`X*`), where every iteration is optional and so the rule
applies to all of them.

| file | change |
| --- | --- |
| `syntax/code.go` | declare `Branchmarkjs`, add it to the backtracking / size / name tables |
| `syntax/writer.go` | emit it for `lazy == 0 && node.m == 0` |
| `runner.go` | handle it and its `Back` / `Back2` forms; on an empty match, restore the mark and backtrack instead of leaving the loop |

Not covered, because no ECMAScript test exercises them and each needs the same
surgery on a different opcode: the lazy form (`X*?`, `Lazybranchmark`) and the
counted form (`X{n,m}`, `Branchcount`).

## Consequences

Upstream's PCRE conformance fixture drops from 769/769 to 721/769. All 48 are
cases where ECMAScript itself diverges from PCRE, and goant agrees with V8 on
every one of them — see `repeat_es_test.go`, which pins the behaviour.
