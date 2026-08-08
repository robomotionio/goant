# ECMA-402 and Temporal — plan and TODO

**Branch:** `feat/intl402-and-temporal`
**Baseline measured 2026-08-08 against test262 `b363f29d`:** `intl402` **216 / 3,357 (6.4%)**

| stage | `intl402` | `-core` |
|---|---:|---|
| baseline | 216 / 3,357 | 42,739 / 42,740 |
| A0 — `timeZone` | 226 / 3,357 | unchanged |
| A1 — locales, `Intl.Locale` | 389 / 3,357 | unchanged |
| A3 — Collator, PluralRules | (measuring) | unchanged |
| A2 — NumberFormat options | (measuring) | unchanged |

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

### A0 — `timeZone`, and stop lying about it  ▸ **done**, `cadc8d5`

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

- [x] `IsValidTimeZoneName` per ECMA-402 §6.4, over the IANA database — the
      identifier set is `zoneDisplayNames`' key set, which was read out of ICU
      zone by zone and agrees exactly, name for name, with the database Go
      bundles. A test checks that agreement over all 598, exhaustively.
- [x] Throw `RangeError` for an unknown zone, at construction (not at format)
- [x] `resolvedOptions().timeZone` reports what was requested, case-normalised
      but **not** resolved to the zone it links to: `Asia/Calcutta` and
      `Asia/Kolkata` are the same instant and two different answers
- [x] Offset identifiers (`+05`, `+0530`, `+05:30`), reported as `±HH:MM`
- [x] Format the instant in the requested zone in `DateTimeFormat.format` and
      `Date.prototype.toLocale*`. `formatToParts` does not exist yet — A4.
- [x] `import _ "time/tzdata"` — **+408 KB measured**, and verified: on Windows without
      it, `LoadLocation` fails with `unknown time zone America/New_York` unless
      Go is installed, because it falls back to `$GOROOT/lib/time/zoneinfo.zip`.
      A deployed robot has no GOROOT.
- [x] Where that import lives: `internal/engine`, for now. Revisit under
      "Packaging" below.

### A1 — locales and negotiation  ▸ **done**, `121e9fc` and `43ed27d`

`x/text/language` does **not** do this, which was the surprise. Measured against
test262's own canonicalisation table it gets 12 of 19: it is built for locale
*matching*, so it drops subtags it does not recognise (`de-gregory` loses its
variant), it has no complex language aliases (`sgn-GR` is not `gss`), it does
not sort variants, and its likely-subtags coverage is 753 of CLDR's 7,788.
ECMA-402 needs structural validity and nothing more, which is a different job.

So the tag grammar and canonicalisation are written here (`intl_langtag.go`)
over CLDR 48 tables generated by `tools/gencldr`, in the shape of
`tools/genunicode`. +184 KB, most of it likely subtags.

- [x] `Intl.getCanonicalLocales` (UTS 35 `unicode_locale_id`, not RFC 5646
      `langtag` — the old regular expression accepted extlang, grandfathered
      and privateuse-only tags, all of which must be rejected)
- [x] `Intl.Locale` — constructor, option overrides, every subtag and keyword
      getter, `maximize`/`minimize`
- [x] `supportedLocalesOf` and the `localeMatcher` negotiation shared by every
      constructor (it returned an empty array before)
- [x] `Intl.supportedValuesOf`
- [ ] `Intl.Locale.prototype.getCalendars`, `getCollations`, `getHourCycles`,
      `getNumberingSystems`, `getTimeZones`, `getTextInfo`, `getWeekInfo` — 52
      tests, each needing a CLDR table this engine has no other use for yet.
      Left with A4, which is where that data would arrive anyway.

### A2 — `Intl.NumberFormat`  ▸ **options done**, `33695f4`; formatting next

Highest customer value after A0: invoices, amounts, reports. The options bag was
not read at all; `formatNumber` in `intl_format.go` already did grouping and the
decimal separator per locale, so the first half of this was options and
validation rather than arithmetic.

- [x] The whole options bag read, validated and reported: `style`, `currency`
      with `currencyDisplay` and `currencySign`, `unit` with `unitDisplay`, the
      digit options, `useGrouping` in both its boolean and its string spelling,
      `signDisplay`, `notation`, `compactDisplay` — and `resolvedOptions` in the
      specified key order
- [x] `Number.prototype.toLocaleString` delegating to it, which a test checks
      value by value
- [x] Percent, `minimum/maximumFractionDigits`, `minimumIntegerDigits`,
      `minimum/maximumSignificantDigits` including rounding above the decimal
      point (123456 to 3 significant digits is 123000)
- [ ] Currency symbols and their placement — the code is written out today,
      which is `currencyDisplay: "code"` and truthful, but not what `"symbol"`
      promises. Needs CLDR currency data (`x/text/currency` carries some).
- [ ] `notation: "compact"` and `"scientific"`/`"engineering"` output
- [ ] `formatToParts`, `formatRange`, `formatRangeToParts` — 51 tests
- [ ] Rounding modes other than `halfExpand`, `roundingIncrement`,
      `trailingZeroDisplay`, `roundingPriority`

### A3 — `Intl.Collator` and `Intl.PluralRules`  ▸ **done**, `884cce7` and `56b4a9b`

Done before A2, which is out of the order this list sets. The reason is
practical rather than principled: A3 is small and self-contained, A2 is the
largest remaining piece of Track A, and A3 fit.

- [x] `Collator` over `x/text/collate`; `usage`, `sensitivity`, `numeric`,
      `collation`, `ignorePunctuation`, `caseFirst`, and `resolvedOptions`
      reporting all of them. The comparison had been a code-unit order with an
      NFC pass bolted on and no options read at all.
- [x] `String.prototype.localeCompare` delegating to it, rather than keeping a
      second ordering that would disagree
- [x] `PluralRules` over `x/text/feature/plural`; `select`, `selectRange`,
      `pluralCategories`. It did not exist: all 53 tests failed on a missing
      constructor.
- [ ] Per-locale collation availability — 5 Collator tests. "pinyin" is a
      supported collation for `zh` and not for `de`, and ResolveLocale needs to
      know which, which is CLDR data we do not carry.
- [ ] `caseFirst` is reported but does not reorder: `x/text/collate` has no
      option for it.
- [ ] Plural RANGE rules — `selectRange` answers with the shared form where both
      ends agree and "other" where they do not. CLDR's range table is not in
      `x/text`.

**What this turned up, and A2 inherits it:** the locale a service resolved to
had been `localeTable`'s, which is the set of locales this engine can *format*.
The plural rules and the collator cover every CLDR locale, so both now keep the
tag that was **asked for**. Arabic has six plural categories and no entry in
that table; it was answering with English's two.

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
