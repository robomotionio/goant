package engine

import (
	"strings"
	"testing"
	"time"
)

// The premise of intl_timezone.go is that zoneDisplayNames' key set — read out
// of ICU zone by zone, see tools/intlgen — is also the set of identifiers
// ECMA-402 §6.4 says are valid, and that Go can load every one of them. That is
// a claim about two databases agreeing, made once at generation time on one
// machine, and it is checked here exhaustively rather than sampled: if a Go
// toolchain ever ships a zoneinfo.zip that has dropped a zone, a script asking
// for it would silently get the host's clock, which is the exact failure this
// file was written to end.
func TestEveryNamedZoneLoads(t *testing.T) {
	for id := range zoneDisplayNames {
		if _, err := time.LoadLocation(id); err != nil {
			t.Errorf("zoneDisplayNames has %q but Go cannot load it: %v", id, err)
		}
	}
}

// Matching is ASCII-case-insensitive and reports the canonical spelling, which
// is what resolvedOptions() echoes back. Over every identifier, in three
// spellings, the way test262's timezone-case-insensitive.js does it.
func TestZoneMatchingIsASCIICaseInsensitive(t *testing.T) {
	for id := range zoneDisplayNames {
		for _, spelling := range []string{id, strings.ToUpper(id), strings.ToLower(id)} {
			// Only ASCII case variants are being claimed; an identifier with a
			// non-ASCII byte is invalid whatever its case, and there are none.
			got, loc, ok := resolveTimeZone(spelling)
			if !ok {
				t.Errorf("resolveTimeZone(%q) rejected it", spelling)
				continue
			}
			// The three names for no offset at all are the one exception:
			// they are the same identifier, and it is spelled "UTC".
			want := id
			if id == "Etc/UTC" || id == "Etc/GMT" || id == "GMT" {
				want = "UTC"
			}
			if got != want {
				t.Errorf("resolveTimeZone(%q) = %q, want %q", spelling, got, want)
			}
			if loc == nil {
				t.Errorf("resolveTimeZone(%q) returned a nil location", spelling)
			}
		}
	}
}

// The identifier is stored as it was asked for, not as the zone it links to.
// Asia/Calcutta and Asia/Kolkata are the same instant and two different answers
// from resolvedOptions(); canonicalising here would be wrong in a way no test
// of the formatted output could see.
func TestLinkedZonesKeepTheIdentifierThatWasAskedFor(t *testing.T) {
	for _, pair := range [][2]string{
		{"Asia/Calcutta", "Asia/Kolkata"},
		{"Europe/Bratislava", "Europe/Prague"},
		{"Etc/UCT", "Etc/Universal"},
		{"Etc/GMT0", "Etc/Greenwich"},
	} {
		for _, id := range pair {
			got, _, ok := resolveTimeZone(id)
			if !ok || got != id {
				t.Errorf("resolveTimeZone(%q) = %q, %v; want it preserved", id, got, ok)
			}
		}
		a, la, _ := resolveTimeZone(pair[0])
		b, lb, _ := resolveTimeZone(pair[1])
		if a == b {
			t.Errorf("%q and %q collapsed to one identifier", pair[0], pair[1])
		}
		// Same instant, different names: the offsets must agree even though the
		// identifiers do not.
		when := time.Unix(0, 0)
		_, oa := when.In(la).Zone()
		_, ob := when.In(lb).Zone()
		if oa != ob {
			t.Errorf("%q and %q disagree on the offset: %d vs %d", pair[0], pair[1], oa, ob)
		}
	}
}

// Names that are not IANA identifiers must be rejected, not quietly matched.
// The three-letter list is test262's timezone-legacy-non-iana.js: Java's legacy
// abbreviations, which a script written against another engine may well pass.
func TestInvalidZoneNamesAreRejected(t *testing.T) {
	invalid := []string{
		"", "MEZ", "Pacific Time", "cnsha", "invalid", "Local",
		// Non-ASCII letters, which Unicode case folding would spell into
		// valid identifiers and ASCII case folding must not.
		"Europe/İstanbul", "asıa/baku", "europe/brußels",
		// Legacy non-IANA three-letter zones.
		"ACT", "AET", "AGT", "ART", "AST", "BET", "BST", "CAT", "CNT", "CST",
		"CTT", "EAT", "ECT", "IET", "IST", "JST", "MIT", "NET", "NST", "PLT",
		"PNT", "PRT", "PST", "SST", "VST",
	}
	for _, name := range invalid {
		if id, _, ok := resolveTimeZone(name); ok {
			t.Errorf("resolveTimeZone(%q) accepted it as %q", name, id)
		}
	}
}

// IsTimeZoneOffsetString: ASCIISign Hour (TimeSeparator MinuteSecond)?, with no
// sub-minute precision and no U+2212 MINUS SIGN. Both lists are test262's.
func TestOffsetTimeZoneGrammar(t *testing.T) {
	valid := map[string]string{
		"+05":    "+05:00",
		"-08":    "-08:00",
		"+00":    "+00:00",
		"-00":    "+00:00", // -0 minutes is +0 minutes
		"+0530":  "+05:30",
		"+05:30": "+05:30",
		"-03:30": "-03:30",
		"+23:59": "+23:59",
	}
	for in, want := range valid {
		got, loc, ok := resolveTimeZone(in)
		if !ok {
			t.Errorf("resolveTimeZone(%q) rejected a valid offset", in)
			continue
		}
		if got != want {
			t.Errorf("resolveTimeZone(%q) = %q, want %q", in, got, want)
		}
		if loc == nil {
			t.Errorf("resolveTimeZone(%q) returned a nil location", in)
		}
	}

	invalid := []string{
		"+3", "+24", "+23:0", "+130", "+13234", "+135678", "-7", "-10.50",
		"-10,50", "-24", "-014", "-210", "-2400", "-1:10", "-21:0", "+0:003",
		"+15:59:00", "+15:59.50", "+15:59,50", "+222700", "-02:3200",
		"-170100", "-22230",
		// U+2212 MINUS SIGN is not an ASCIISign.
		"−0900", "−10:00", "−05",
	}
	for _, in := range invalid {
		if id, _, ok := resolveTimeZone(in); ok {
			t.Errorf("resolveTimeZone(%q) accepted it as %q", in, id)
		}
	}
}

// An offset identifier denotes the offset it spells, whichever of the three
// accepted spellings it was written in.
func TestOffsetZoneShiftsTheInstant(t *testing.T) {
	// 2026-01-15T12:00:00Z.
	const ms = 1768478400000
	for _, tc := range []struct{ zone, want string }{
		{"+00:00", "12:00"},
		{"+0530", "17:30"},
		{"-08", "04:00"},
	} {
		_, loc, ok := resolveTimeZone(tc.zone)
		if !ok {
			t.Fatalf("resolveTimeZone(%q) rejected it", tc.zone)
		}
		if got := msInZone(ms, loc).Format("15:04"); got != tc.want {
			t.Errorf("%s at %q = %s, want %s", "12:00Z", tc.zone, got, tc.want)
		}
	}
}
