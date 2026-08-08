# ECMA-402 and Temporal — plan and TODO

**Branch:** `feat/intl402-and-temporal`
**Baseline measured 2026-08-08 against test262 `b363f29d`:** `intl402` **216 / 3,357 (6.4%)**

Companion to [TODO.md](../TODO.md) (the master port plan) and
[docs/ecma-262-core-tests.md](./ecma-262-core-tests.md) (what `-core` excludes and why).

---

## The number that decides the shape of this

`intl402` is 3,357 tests. **Temporal is 2,029 of them — 60%.** And Temporal is
another 4,603 in `built-ins/`, so the whole proposal is **6,632 tests**, five
times the size of everything else in ECMA-402 put together.

| Area | Tests | Notes |
|---|---:|---|
| **Temporal** (intl402 part) | 2,029 | a separate proposal, needs `built-ins/Temporal` first |
| NumberFormat | 249 | |
| DateTimeFormat | 244 | |
| Locale | 168 | |
| DurationFormat | 110 | |
| ListFormat | 81 | |
| RelativeTimeFormat | 80 | |
| Segmenter | 79 | |
| Intl statics | 66 | `getCanonicalLocales`, `supportedValuesOf` |
| Collator | 65 | |
| DisplayNames | 57 | |
| PluralRules | 53 | |
| String / Date / root | 76 | `localeCompare`, `toLocale*` delegation |
| | | |
| **classic Intl, without Temporal** | **1,328** | |

So this is two projects, and conflating them is how it becomes unschedulable.
**Track A is classic Intl (1,328 tests). Track B is Temporal (6,632).** Track A
is worth doing now. Track B is a decision, not a task.

---

## What we are starting from

`golang.org/x/text` is already a dependency, and it carries CLDR-derived data
*and* the hard algorithms. Measured cost of each, statically linked, stripped,
over a 1.5 MB baseline:

| import | binary | serves |
|---|---:|---|
| `x/text/collate` | +0.2 MB | `Intl.Collator`, `localeCompare` |
| `x/text/number` | +0.3 MB | `Intl.NumberFormat` numerics |
| `x/text/feature/plural` | +0.3 MB | `Intl.PluralRules` |
| `x/text/currency` | +0.2 MB | currency codes and symbols |
| `x/text/language` | (already in) | `Intl.Locale`, canonicalisation, negotiation |

**About +1 MB for the lot.** ICU, for comparison, ships tens of megabytes. This
is the fact that makes Track A worth doing: the expensive part — CLDR data,
UCA collation, plural rules — is already written, already compact, already a
dependency.

**The one real gap is `DateTimeFormat`.** `x/text/date` has a `tables.go`
generated from CLDR but no exported API: the package was started and never
finished. Its data is usable; the formatter is not written. Precedent for
building one exists in this repo — `tools/genunicode` is 540 lines and produced
the entire Unicode 17 property database.

---

## Track A — classic Intl

Ordered by value to a host running customer scripts, not by test count.

### A0 — `timeZone`, and stop lying about it  ▸ do this first

Today the option is accepted and ignored, which is the worst of the three ways
to be incomplete:

```js
d.toLocaleString("en-US", { timeZone: "America/New_York" })  // → local time
d.toLocaleString("en-US", { timeZone: "Not/AZone" })          // → local time, must throw RangeError
Intl.DateTimeFormat("en-US", { timeZone: "Asia/Tokyo" }).resolvedOptions().timeZone  // → the host zone
```

A flow formatting a timestamp for another country gets the robot's own time,
silently, with no error. It worked under V8, which bundles ICU. This is a
regression from the migration and it is invisible to `-core`.

No CLDR needed: Go's `time.LoadLocation` does the conversion. Keep today's
formatting and put the instant in the right zone first.

- [ ] `IsValidTimeZoneName` per ECMA-402 §6.4, over the IANA database
- [ ] Throw `RangeError` for an unknown zone, at construction (not at format)
- [ ] `resolvedOptions().timeZone` reports what was requested, canonicalised
- [ ] Format the instant in the requested zone in `DateTimeFormat.format`,
      `formatToParts`, `Date.prototype.toLocale*`
- [ ] `import _ "time/tzdata"` — **required**, and verified: on Windows without
      it, `LoadLocation` fails with `unknown time zone America/New_York` unless
      Go is installed, because it falls back to `$GOROOT/lib/time/zoneinfo.zip`.
      A deployed robot has no GOROOT. Costs ~450 KB.
- [ ] Decide where that import lives (see "Packaging" below)

### A1 — locales and negotiation

`x/text/language` does effectively all of this.

- [ ] `Intl.getCanonicalLocales` (BCP 47 canonicalisation)
- [ ] `Intl.Locale` — constructor, accessors, `maximize`/`minimize`
- [ ] `supportedLocalesOf` and the `localeMatcher` negotiation shared by every
      constructor
- [ ] `Intl.supportedValuesOf`

### A2 — `Intl.NumberFormat`

Highest customer value after A0: invoices, amounts, reports.

- [ ] Decimal, percent, scientific, engineering via `x/text/number`
- [ ] Currency style and display via `x/text/currency`
- [ ] `minimum/maximumFractionDigits`, `minimumIntegerDigits`, rounding modes
- [ ] `notation: "compact"`, `signDisplay`, `useGrouping`
- [ ] `formatToParts`, `formatRange`

### A3 — `Intl.Collator` and `Intl.PluralRules`

Small, and both are a thin layer over a finished library.

- [ ] `Collator` over `x/text/collate`; `sensitivity`, `numeric`, `caseFirst`
- [ ] `String.prototype.localeCompare` delegating to it
- [ ] `PluralRules` over `x/text/feature/plural`; `select`, `selectRange`

### A4 — `Intl.DateTimeFormat`, properly

The large one. Everything above is an API over finished data; this is not.

- [ ] Decide: finish an API over `x/text/date`'s tables, or generate our own
      CLDR subset with a `tools/gencldr` in the shape of `tools/genunicode`
- [ ] Skeleton and pattern resolution (`dateStyle`/`timeStyle`, then components)
- [ ] Month, weekday, era, dayPeriod names per locale
- [ ] `formatToParts`, `formatRange`, `formatRangeToParts`
- [ ] Calendars beyond Gregorian — **probably decline**; see "What we will not do"

### A5 — the rest

~400 tests, near-zero value for a host running RPA scripts. Listed for
completeness, not scheduled.

- [ ] `Intl.DisplayNames` (57), `ListFormat` (81), `RelativeTimeFormat` (80),
      `Segmenter` (79), `DurationFormat` (110)

---

## Track B — Temporal

**6,632 tests.** This is not "the rest of Intl"; it is a second date/time API
alongside `Date`, with its own arithmetic, its own calendar and timezone model,
and its own string formats.

Before any of `intl402/Temporal` can pass, `built-ins/Temporal` has to exist:
`Instant`, `ZonedDateTime`, `PlainDate`, `PlainTime`, `PlainDateTime`,
`PlainYearMonth`, `PlainMonthDay`, `Duration`, plus `Temporal.Now`.

- [ ] **Decide whether to do this at all.** It is the single largest remaining
      item in the whole engine — larger than the JIT was — and no Robomotion
      flow has ever asked for it. The honest default is: not until a customer
      needs it, and then scoped to what they need.
- [ ] If yes: `Duration` and `Instant` first, since everything else is defined
      in terms of them
- [ ] Nail the ISO 8601 grammar early; a large fraction of the suite is parsing
- [ ] Go's `time` package gives the arithmetic and the zone database; the
      calendar model is the part with no Go equivalent

---

## Packaging

ECMA-402 *is* a separate specification, and it should be a separate module.

- [ ] `goant/intl` as its own module, registered by the embedder:
      `intl.Install(rt)`
- [ ] Base engine keeps its size — the README's 6.4 MB is a headline claim and
      +1 MB of CLDR should not be charged to a host that formats nothing
- [ ] `Intl` absent rather than stubbed when not installed, so
      `typeof Intl === "undefined"` is a truthful answer a script can test
- [ ] Decide whether `time/tzdata` belongs in the engine (correct by default,
      +450 KB for everyone) or in the embedder's main (Go's own convention,
      silent UTC for anyone who forgets). Leaning: engine, because a silently
      wrong timezone costs more than 450 KB.

---

## What we will not do

Stated up front so the plan is not quietly rewritten later:

- **Full CLDR locale coverage.** Ship the machinery and a subset of locales,
  extensible by regeneration. Parity with ICU across 200+ locales is an
  ICU-sized project and buying it in tests rather than capability.
- **Non-Gregorian calendars** in `DateTimeFormat`, unless asked for.
- **A0 through A3 without measuring.** Every stage lands with its `intl402`
  number in the commit message, the way the rest of this engine has been built.

---

## Definition of done, per stage

Each stage is finished when:

1. its slice of `intl402` is measured before and after, and the number is in the
   commit message;
2. `goant-t262 -core` is unchanged — 42,739 / 42,740 — because none of this may
   move ECMA-262;
3. the unit suite passes with the tier off, on, and on at threshold 1;
4. the differential fuzzer finds no disagreement.
