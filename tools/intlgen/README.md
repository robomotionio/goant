# intlgen

Generates `internal/engine/intl_data.go`: the locale formatting table and the
IANA zone display names, extracted from the V8 build the robot shipped through
26.7.4 (`robomotionio/v8go`, V8 14.7 with ICU).

The tables are not transcribed from CLDR. They are read out of that V8 through
`Intl.DateTimeFormat.prototype.formatToParts`, which reports the structure of a
pattern rather than only its rendering, so they are exact by construction and
regenerating them is mechanical.

## Building the oracle

Needs clang and the prebuilt libv8 that the v8go fork vendors (~200 MB), which
is why the binary is not checked in and the oracle is a separate module:

    cd oracle && CC=clang CXX=clang++ go build -o ../probe-v8-2673 .

## Regenerating

`zonedump.tsv` holds one line per IANA zone per season. ICU caches the default
zone per process, so it cannot be gathered in a single run and is collected by
a shell loop:

    while read -r Z; do
      TZ="$Z" ./probe-v8-2673 zonename.js | sed "s|^|$Z\t|"
    done < <(python3 -c 'import zoneinfo;print("\n".join(sorted(zoneinfo.available_timezones())))') \
      > zonedump.tsv

Then:

    python3 gen.py > ../../internal/engine/intl_data.go
    gofmt -w ../../internal/engine/intl_data.go

## What is deliberately left out

Locales on a non-Gregorian calendar (th-TH, fa-IR) or with non-Latin digits
(ar-*, bn-IN, mr-IN) are excluded, so they fall back to en-US rather than render
subtly wrong. The oracle itself falls back for tags its ICU data does not carry
(is-IS, ka-GE, kk-KZ and others); those are listed at the bottom of the
generated file.
