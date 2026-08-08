package engine

// Calendar identifiers.
//
// ECMA-402 fixes the set of calendars an implementation provides: it is the
// Calendar Type column of the Intl.Era-monthcode table, no more and no less,
// and Intl.supportedValuesOf("calendar") answers it in canonical form. A
// calendar outside that set is not an error -- "bangla" is a well-formed type
// and a real calendar, just not one of these -- so a request for it falls back
// rather than throwing.
//
// KNOWN GAP: the identifiers are honoured, the arithmetic is not. A formatter
// asked for "hebrew" reports "hebrew" from resolvedOptions and still computes
// its fields on the proleptic Gregorian calendar, so the year and month it
// prints are the Gregorian ones. The conversions belong with Temporal, which
// needs the same sixteen, and land there.

import "strings"

// supportedCalendars is the required set, in the lexicographic order
// AvailableCalendars is defined to return.
var supportedCalendars = []string{
	"buddhist", "chinese", "coptic", "dangi", "ethioaa", "ethiopic",
	"gregory", "hebrew", "indian", "islamic-civil", "islamic-tbla",
	"islamic-umalqura", "iso8601", "japanese", "persian", "roc",
}

// supportedCalendar resolves a calendar type through the -u-ca aliases and
// reports whether it names one of the sixteen. The alias table is CLDR's own,
// so "islamicc" arrives as "islamic-civil" and "ethiopic-amete-alem" as
// "ethioaa".
func supportedCalendar(s string) (string, bool) {
	c := strings.Join(aliasKeywordValue("ca", strings.Split(asciiLower(s), "-")), "-")
	return c, tagContains(supportedCalendars, c)
}

// calendarDisplayNames is what Intl.DisplayNames answers for type "calendar".
// The names are English, which is a limit of carrying no per-locale CLDR data
// rather than a choice; a calendar that is not in the supported set has no
// entry, because naming one would claim a calendar this engine does not offer.
var calendarDisplayNames = map[string]string{
	"buddhist":         "Buddhist Calendar",
	"chinese":          "Chinese Calendar",
	"coptic":           "Coptic Calendar",
	"dangi":            "Dangi Calendar",
	"ethioaa":          "Ethiopic Amete Alem Calendar",
	"ethiopic":         "Ethiopic Calendar",
	"gregory":          "Gregorian Calendar",
	"hebrew":           "Hebrew Calendar",
	"indian":           "Indian National Calendar",
	"islamic-civil":    "Islamic Calendar (tabular, civil epoch)",
	"islamic-tbla":     "Islamic Calendar (tabular, astronomical epoch)",
	"islamic-umalqura": "Islamic Calendar (Umm al-Qura)",
	"iso8601":          "ISO-8601 Calendar",
	"japanese":         "Japanese Calendar",
	"persian":          "Persian Calendar",
	"roc":              "Minguo Calendar",
}
