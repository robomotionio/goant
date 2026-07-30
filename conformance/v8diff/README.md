# V8 differential tests

These pin goant's behaviour to the V8 build the robot shipped through 26.7.4
(`robomotionio/v8go`, V8 14.7 with ICU), in the places where ECMA-262 does not
say what an engine must do:

- **local time.** `LocalTZA` is host-defined, so an engine may legally define
  local time as UTC. goant did, and on a UTC+03 host `getHours()` was three
  hours out and the date rolled over three hours late.
- **`toLocale*` output.** ECMA-262 says the contents are implementation-defined.
- **`Date.parse` of non-ISO strings.** Only the Date Time String Format is
  required; everything else is optional, so rejecting `Jul 30, 2026` is legal.

Neither the curated conformance suite nor test262 can catch a regression in any
of them, and not because they are run with `TZ=UTC`. test262's
`built-ins/Date/parse/without-utc-offset.js` is the only test that speaks to
local time at all, and it is self-referential: it asserts that `Date.parse` and
`getTimezoneOffset` agree with *each other*. An engine that reports its zone as
UTC and parses accordingly is internally consistent and passes, under any `TZ`.

So the oracle here is not the spec, it is the previous engine.

## Running

    go test ./conformance/v8diff

Each `*.js` builds an `out` array of labelled lines. The harness runs it under
every timezone named by an `expected/<script>.<zone>.txt` file and diffs.

## Regenerating expectations

Expectations come from the fork, which is a cgo build and is not available in
CI, so they are checked in and never regenerated automatically. To refresh them
you need the oracle binary (see `tools/intlgen/README`) and then:

    go run ./conformance/v8diff/cmd/regen -oracle path/to/probe-v8-2673

Regenerating is a deliberate act: a diff in these files means goant's observable
behaviour moved relative to the engine it replaced, which is exactly what the
test exists to make visible.
